package service

import (
	"context"
	"errors"
	"math"
	"sync"
	"time"
)

const (
	MediaBridgeCapacityReasonR2CircuitOpen     MediaBridgeCapacityReason = "r2_circuit_open"
	MediaBridgeCapacityReasonR2HalfOpenLimited MediaBridgeCapacityReason = "r2_half_open_limited"
)

type MediaBridgeR2CircuitState string

const (
	MediaBridgeR2CircuitClosed   MediaBridgeR2CircuitState = "closed"
	MediaBridgeR2CircuitOpen     MediaBridgeR2CircuitState = "open"
	MediaBridgeR2CircuitHalfOpen MediaBridgeR2CircuitState = "half_open"
)

type MediaBridgeR2Clock interface {
	Now() time.Time
}

type mediaBridgeR2SystemClock struct{}

func (mediaBridgeR2SystemClock) Now() time.Time { return time.Now() }

type mediaBridgeR2CircuitConfig struct {
	enabled          bool
	window           time.Duration
	errorRatePercent int64
	latencyThreshold time.Duration
	openDuration     time.Duration
	halfOpenProbes   int64
	minimumSamples   int64
}

type mediaBridgeR2OutcomeBucket struct {
	second   int64
	samples  int
	failures int
	slow     int
}

type MediaBridgeR2Circuit struct {
	mu sync.Mutex

	settings MediaBridgeSettingsSnapshotSource
	clock    MediaBridgeR2Clock

	config         mediaBridgeR2CircuitConfig
	configured     bool
	state          MediaBridgeR2CircuitState
	generation     uint64
	openUntil      time.Time
	outcomeBuckets []mediaBridgeR2OutcomeBucket
	bucketHead     int
	bucketCount    int
	samples        int
	failures       int
	slow           int

	halfOpenInflight int64
	halfOpenSuccess  int64
}

func NewMediaBridgeR2Circuit(
	settings MediaBridgeSettingsSnapshotSource,
	clocks ...MediaBridgeR2Clock,
) *MediaBridgeR2Circuit {
	circuit := &MediaBridgeR2Circuit{
		settings: settings,
		clock:    mediaBridgeR2SystemClock{},
		state:    MediaBridgeR2CircuitClosed,
	}
	for _, clock := range clocks {
		if clock != nil {
			circuit.clock = clock
			break
		}
	}
	return circuit
}

// MediaBridgeR2Permit ties one R2 operation to the circuit generation that
// admitted it. Observe is idempotent, and stale observations from a previous
// open/half-open generation cannot close a newer circuit.
type MediaBridgeR2Permit struct {
	once       sync.Once
	circuit    *MediaBridgeR2Circuit
	generation uint64
	state      MediaBridgeR2CircuitState
	tracked    bool
}

type MediaBridgeR2CircuitSnapshot struct {
	State            MediaBridgeR2CircuitState
	OpenUntil        time.Time
	WindowSamples    int
	HalfOpenInflight int64
	HalfOpenSuccess  int64
}

// Admission reads the hot Protection snapshot on every call. A disabled
// breaker returns an untracked permit, allowing callers to use one code path.
func (c *MediaBridgeR2Circuit) Admission(ctx context.Context) (*MediaBridgeR2Permit, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if c == nil || c.settings == nil {
		return &MediaBridgeR2Permit{}, nil
	}

	config := mediaBridgeR2Config(c.settings.GetMediaBridgeSettingsCached(ctx).Protection)
	now := c.clock.Now()
	c.mu.Lock()
	defer c.mu.Unlock()
	c.applyConfigLocked(config)
	if !config.enabled {
		return &MediaBridgeR2Permit{}, nil
	}

	switch c.state {
	case MediaBridgeR2CircuitOpen:
		if now.Before(c.openUntil) {
			return nil, mediaBridgeR2AdmissionError(
				MediaBridgeCapacityReasonR2CircuitOpen,
				c.openUntil.Sub(now),
			)
		}
		c.state = MediaBridgeR2CircuitHalfOpen
		c.generation++
		c.halfOpenInflight = 0
		c.halfOpenSuccess = 0
	case MediaBridgeR2CircuitHalfOpen:
		// Continue with the existing half-open generation.
	default:
		c.state = MediaBridgeR2CircuitClosed
	}

	if c.state == MediaBridgeR2CircuitHalfOpen {
		if c.halfOpenInflight >= config.halfOpenProbes {
			return nil, mediaBridgeR2AdmissionError(
				MediaBridgeCapacityReasonR2HalfOpenLimited,
				mediaBridgeCapacityDefaultRetryAfter,
			)
		}
		c.halfOpenInflight++
	}
	return &MediaBridgeR2Permit{
		circuit:    c,
		generation: c.generation,
		state:      c.state,
		tracked:    true,
	}, nil
}

// Observe records the R2 result for a permit. Every tracked permit, especially
// a half-open probe, must be observed exactly once; repeated calls are ignored.
func (c *MediaBridgeR2Circuit) Observe(
	permit *MediaBridgeR2Permit,
	latency time.Duration,
	operationErr error,
) {
	if permit == nil {
		return
	}
	permit.once.Do(func() {
		if !permit.tracked || permit.circuit == nil {
			return
		}
		permit.circuit.observe(permit, latency, operationErr)
	})
}

func (c *MediaBridgeR2Circuit) Snapshot() MediaBridgeR2CircuitSnapshot {
	if c == nil {
		return MediaBridgeR2CircuitSnapshot{State: MediaBridgeR2CircuitClosed}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return MediaBridgeR2CircuitSnapshot{
		State:            c.state,
		OpenUntil:        c.openUntil,
		WindowSamples:    c.samples,
		HalfOpenInflight: c.halfOpenInflight,
		HalfOpenSuccess:  c.halfOpenSuccess,
	}
}

func (c *MediaBridgeR2Circuit) observe(
	permit *MediaBridgeR2Permit,
	latency time.Duration,
	operationErr error,
) {
	now := c.clock.Now()
	c.mu.Lock()
	defer c.mu.Unlock()
	if permit.generation != c.generation || !c.config.enabled {
		return
	}
	// Client cancellation is not an R2 health signal. DeadlineExceeded is: the
	// bridge applies its own administrator-configured upload timeout, so a stuck
	// R2 PUT must contribute to opening the circuit.
	ignored := errors.Is(operationErr, context.Canceled)

	if permit.state == MediaBridgeR2CircuitHalfOpen {
		if c.state != MediaBridgeR2CircuitHalfOpen {
			return
		}
		if c.halfOpenInflight > 0 {
			c.halfOpenInflight--
		}
		if ignored {
			return
		}
		if operationErr != nil || mediaBridgeR2LatencyFailed(c.config, latency) {
			c.openLocked(now)
			return
		}
		c.halfOpenSuccess++
		if c.halfOpenSuccess >= c.config.halfOpenProbes {
			c.closeLocked()
		}
		return
	}

	if permit.state != MediaBridgeR2CircuitClosed || c.state != MediaBridgeR2CircuitClosed {
		return
	}
	if ignored {
		return
	}
	c.pruneOutcomesLocked(now)
	c.recordOutcomeLocked(now, operationErr != nil, mediaBridgeR2LatencyFailed(c.config, latency))
	if c.shouldOpenLocked() {
		c.openLocked(now)
	}
}

func (c *MediaBridgeR2Circuit) applyConfigLocked(config mediaBridgeR2CircuitConfig) {
	if c.configured && c.config == config {
		return
	}
	c.configured = true
	c.config = config
	c.state = MediaBridgeR2CircuitClosed
	c.generation++
	c.openUntil = time.Time{}
	c.resetOutcomeBucketsLocked(config)
	c.halfOpenInflight = 0
	c.halfOpenSuccess = 0
}

func (c *MediaBridgeR2Circuit) shouldOpenLocked() bool {
	if int64(c.samples) < c.config.minimumSamples {
		return false
	}
	if c.config.errorRatePercent > 0 {
		if int64(c.failures)*100 >= c.config.errorRatePercent*int64(c.samples) {
			return true
		}
	}
	if c.config.latencyThreshold > 0 {
		// nearest-rank P95 is at or above the threshold exactly when fewer
		// than ceil(95% * n) samples are below the threshold.
		rank := int(math.Ceil(float64(c.samples) * 0.95))
		if c.samples-c.slow < rank {
			return true
		}
	}
	return false
}

func (c *MediaBridgeR2Circuit) pruneOutcomesLocked(now time.Time) {
	if len(c.outcomeBuckets) == 0 {
		return
	}
	// Buckets use whole-second resolution. Retaining the bucket containing the
	// cutoff mirrors the old inclusive boundary while bounding approximation to
	// less than one second.
	cutoffSecond := now.Add(-c.config.window).Unix()
	for c.bucketCount > 0 {
		bucket := c.outcomeBuckets[c.bucketHead]
		if bucket.second >= cutoffSecond {
			return
		}
		c.removeOldestOutcomeBucketLocked()
	}
}

func (c *MediaBridgeR2Circuit) recordOutcomeLocked(now time.Time, failed, slow bool) {
	if len(c.outcomeBuckets) == 0 {
		return
	}
	second := now.Unix()
	if c.bucketCount > 0 {
		lastIndex := (c.bucketHead + c.bucketCount - 1) % len(c.outcomeBuckets)
		last := &c.outcomeBuckets[lastIndex]
		// Keep buckets chronological if the wall clock moves backwards. The old
		// per-request queue also assumed monotonic observations; folding into the
		// latest bucket preserves bounded pruning without a linear lookup.
		if second <= last.second {
			second = last.second
		}
		if second == last.second {
			last.samples++
			if failed {
				last.failures++
			}
			if slow {
				last.slow++
			}
			c.samples++
			if failed {
				c.failures++
			}
			if slow {
				c.slow++
			}
			return
		}
	}

	if c.bucketCount == len(c.outcomeBuckets) {
		c.removeOldestOutcomeBucketLocked()
	}
	index := (c.bucketHead + c.bucketCount) % len(c.outcomeBuckets)
	bucket := mediaBridgeR2OutcomeBucket{second: second, samples: 1}
	if failed {
		bucket.failures = 1
	}
	if slow {
		bucket.slow = 1
	}
	c.outcomeBuckets[index] = bucket
	c.bucketCount++
	c.samples++
	if failed {
		c.failures++
	}
	if slow {
		c.slow++
	}
}

func (c *MediaBridgeR2Circuit) removeOldestOutcomeBucketLocked() {
	if c.bucketCount == 0 || len(c.outcomeBuckets) == 0 {
		return
	}
	bucket := c.outcomeBuckets[c.bucketHead]
	c.samples -= bucket.samples
	c.failures -= bucket.failures
	c.slow -= bucket.slow
	c.outcomeBuckets[c.bucketHead] = mediaBridgeR2OutcomeBucket{}
	c.bucketHead = (c.bucketHead + 1) % len(c.outcomeBuckets)
	c.bucketCount--
}

func (c *MediaBridgeR2Circuit) resetOutcomeBucketsLocked(config mediaBridgeR2CircuitConfig) {
	capacity := 0
	if config.enabled {
		capacity = mediaBridgeR2OutcomeBucketCapacity(config.window)
	}
	if capacity == 0 {
		c.outcomeBuckets = nil
	} else if len(c.outcomeBuckets) != capacity {
		c.outcomeBuckets = make([]mediaBridgeR2OutcomeBucket, capacity)
	}
	c.bucketHead = 0
	c.bucketCount = 0
	c.samples = 0
	c.failures = 0
	c.slow = 0
}

func mediaBridgeR2OutcomeBucketCapacity(window time.Duration) int {
	if window <= 0 {
		return 0
	}
	seconds := int64(window / time.Second)
	if window%time.Second != 0 {
		seconds++
	}
	// Settings validation caps the window at 24 hours. Keep this defensive cap
	// here too so a malformed snapshot cannot cause an unbounded allocation.
	if seconds > mediaBridgeMaxProtectionWindowSecs {
		seconds = mediaBridgeMaxProtectionWindowSecs
	}
	return int(seconds) + 1
}

func (c *MediaBridgeR2Circuit) openLocked(now time.Time) {
	c.state = MediaBridgeR2CircuitOpen
	c.generation++
	c.openUntil = now.Add(c.config.openDuration)
	c.halfOpenInflight = 0
	c.halfOpenSuccess = 0
}

func (c *MediaBridgeR2Circuit) closeLocked() {
	c.state = MediaBridgeR2CircuitClosed
	c.generation++
	c.openUntil = time.Time{}
	c.resetOutcomeBucketsLocked(c.config)
	c.halfOpenInflight = 0
	c.halfOpenSuccess = 0
}

func mediaBridgeR2Config(settings MediaBridgeProtectionSettings) mediaBridgeR2CircuitConfig {
	config := mediaBridgeR2CircuitConfig{
		window:           mediaBridgeSeconds(settings.R2WindowSeconds),
		errorRatePercent: settings.R2ErrorRateThresholdPercent,
		latencyThreshold: mediaBridgeMilliseconds(settings.R2LatencyThresholdMS),
		openDuration:     mediaBridgeSeconds(settings.R2OpenSeconds),
		halfOpenProbes:   settings.R2HalfOpenProbes,
		minimumSamples:   settings.R2MinimumSamples,
	}
	config.enabled = config.window > 0 &&
		config.openDuration > 0 &&
		config.halfOpenProbes > 0 &&
		config.minimumSamples > 0 &&
		(config.errorRatePercent > 0 || config.latencyThreshold > 0)
	return config
}

func mediaBridgeR2LatencyFailed(config mediaBridgeR2CircuitConfig, latency time.Duration) bool {
	return config.latencyThreshold > 0 && latency >= config.latencyThreshold
}

func mediaBridgeR2AdmissionError(reason MediaBridgeCapacityReason, retryAfter time.Duration) *MediaBridgeCapacityError {
	if retryAfter <= 0 {
		retryAfter = mediaBridgeCapacityDefaultRetryAfter
	}
	return &MediaBridgeCapacityError{
		Reason:      reason,
		Scope:       MediaBridgeCapacityScopeGlobal,
		RetryAfter:  retryAfter,
		Description: "media bridge R2 circuit is temporarily unavailable",
	}
}

func mediaBridgeSeconds(seconds int64) time.Duration {
	if seconds <= 0 {
		return 0
	}
	if seconds > math.MaxInt64/int64(time.Second) {
		return time.Duration(math.MaxInt64)
	}
	return time.Duration(seconds) * time.Second
}
