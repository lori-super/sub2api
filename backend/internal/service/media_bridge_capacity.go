package service

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"sync"
	"time"
)

var (
	ErrMediaBridgeCapacityUnavailable = errors.New("media bridge capacity unavailable")
	ErrMediaBridgeInvalidDecodedBytes = errors.New("media bridge decoded bytes must be positive")
)

type MediaBridgeCapacityReason string

const (
	MediaBridgeCapacityReasonInflightRequests MediaBridgeCapacityReason = "inflight_requests"
	MediaBridgeCapacityReasonInflightBytes    MediaBridgeCapacityReason = "inflight_decoded_bytes"
	MediaBridgeCapacityReasonBandwidth        MediaBridgeCapacityReason = "bandwidth"
	MediaBridgeCapacityReasonPolicy           MediaBridgeCapacityReason = "policy"
)

type MediaBridgeCapacityScope string

const (
	MediaBridgeCapacityScopeGlobal MediaBridgeCapacityScope = "global"
	MediaBridgeCapacityScopeTenant MediaBridgeCapacityScope = "tenant"
)

const (
	mediaBridgeCapacityDefaultRetryAfter = time.Second
	mediaBridgeBandwidthSweepInterval    = time.Minute
)

// MediaBridgeCapacityError is returned when a request cannot be admitted.
// RetryAfter is suitable for a 429 response. A zero RetryAfter means that the
// request itself exceeds a configured in-flight byte limit and cannot become
// admissible merely by waiting for another request to finish.
type MediaBridgeCapacityError struct {
	Reason      MediaBridgeCapacityReason
	Scope       MediaBridgeCapacityScope
	TenantID    int64
	Limit       int64
	Current     int64
	Requested   int64
	RetryAfter  time.Duration
	Description string
}

func (e *MediaBridgeCapacityError) Error() string {
	if e == nil {
		return ErrMediaBridgeCapacityUnavailable.Error()
	}
	if e.Description != "" {
		return e.Description
	}
	return fmt.Sprintf("media bridge %s capacity unavailable in %s scope", e.Reason, e.Scope)
}

func (e *MediaBridgeCapacityError) Unwrap() error {
	return ErrMediaBridgeCapacityUnavailable
}

// MediaBridgeCapacityUsage intentionally contains only live resources. Token
// balances are implementation details and are not stable administrator APIs.
type MediaBridgeCapacityUsage struct {
	InflightRequests     int64
	InflightDecodedBytes int64
}

// MediaBridgeCapacitySnapshot separates process-wide usage from one tenant's
// usage so callers can expose both without walking all tracked tenants.
type MediaBridgeCapacitySnapshot struct {
	Global   MediaBridgeCapacityUsage
	TenantID int64
	Tenant   MediaBridgeCapacityUsage
}

type MediaBridgeCapacityPolicyInput struct {
	Settings     MediaBridgeCapacitySettings
	TenantID     int64
	DecodedBytes int64
	Usage        MediaBridgeCapacitySnapshot
}

// MediaBridgeCapacityPolicy is an injectable resource-policy hook. A later
// memory/CPU probe can be implemented behind this interface; this component
// deliberately performs no host or production resource reads itself.
type MediaBridgeCapacityPolicy interface {
	CheckMediaBridgeCapacity(context.Context, MediaBridgeCapacityPolicyInput) error
}

type MediaBridgeCapacityPolicyFunc func(context.Context, MediaBridgeCapacityPolicyInput) error

func (f MediaBridgeCapacityPolicyFunc) CheckMediaBridgeCapacity(
	ctx context.Context,
	input MediaBridgeCapacityPolicyInput,
) error {
	return f(ctx, input)
}

type mediaBridgeBandwidthBucket struct {
	initialized bool
	rate        int64
	burst       int64
	tokens      float64
	last        time.Time
}

type MediaBridgeCapacity struct {
	mu sync.Mutex

	globalUsage  MediaBridgeCapacityUsage
	tenantUsage  map[int64]MediaBridgeCapacityUsage
	tenantWeight map[int64]int64
	totalWeight  int64
	notify       chan struct{}

	// Bandwidth pending bytes are reduced as token-bucket chunks are granted.
	// Unlike InflightDecodedBytes, they represent only upload work that has not
	// yet entered the object-store client, which keeps admission estimates from
	// counting already transferred portions of a large file.
	globalBandwidthPending int64
	tenantBandwidthPending map[int64]int64

	globalBandwidth mediaBridgeBandwidthBucket
	tenantBandwidth map[int64]*mediaBridgeBandwidthBucket
	nextBucketSweep time.Time

	policy MediaBridgeCapacityPolicy
}

// NewMediaBridgeCapacity accepts at most one policy in normal use. The
// variadic form lets callers omit a resource policy until one is wired.
func NewMediaBridgeCapacity(policies ...MediaBridgeCapacityPolicy) *MediaBridgeCapacity {
	capacity := &MediaBridgeCapacity{}
	for _, policy := range policies {
		if policy != nil {
			capacity.policy = policy
			break
		}
	}
	return capacity
}

// MediaBridgeCapacityLease owns one admitted request. Release is idempotent and
// intentionally returns no error so it can always be deferred.
type MediaBridgeCapacityLease struct {
	once         sync.Once
	capacity     *MediaBridgeCapacity
	tenantID     int64
	decodedBytes int64
	settings     MediaBridgeCapacitySettings
	override     MediaBridgeTenantCapacity
	hasOverride  bool
	// bandwidthRemaining is protected by capacity.mu.
	bandwidthRemaining int64
}

func (l *MediaBridgeCapacityLease) Release() {
	if l == nil {
		return
	}
	l.once.Do(func() {
		if l.capacity != nil {
			l.capacity.releaseLease(l)
		}
	})
}

// Acquire waits for in-flight capacity and rejects bandwidth work whose
// estimated queue delay exceeds settings.AdmissionWaitMS. Each call uses its
// immutable settings snapshot; changing settings therefore affects new
// admissions without invalidating existing leases.
func (c *MediaBridgeCapacity) Acquire(
	ctx context.Context,
	settings MediaBridgeCapacitySettings,
	tenantID int64,
	decodedBytes int64,
) (*MediaBridgeCapacityLease, error) {
	if decodedBytes <= 0 {
		return nil, ErrMediaBridgeInvalidDecodedBytes
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if tenantID < 0 {
		tenantID = 0
	}

	if c == nil {
		return &MediaBridgeCapacityLease{}, nil
	}

	waitBudget := mediaBridgeMilliseconds(settings.AdmissionWaitMS)
	startedAt := time.Now()
	deadline := startedAt.Add(waitBudget)
	override, hasOverride := mediaBridgeTenantOverride(settings, tenantID)

	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}

		now := time.Now()
		c.mu.Lock()
		c.initLocked()
		c.sweepIdleBandwidthBucketsLocked(now)

		capacityErr := c.checkLocked(settings, override, hasOverride, tenantID, decodedBytes)
		if capacityErr == nil {
			capacityErr = c.checkBandwidthAdmissionLocked(
				settings,
				override,
				hasOverride,
				tenantID,
				decodedBytes,
				waitBudget,
				now,
			)
		}
		if capacityErr == nil {
			if err := ctx.Err(); err != nil {
				c.mu.Unlock()
				return nil, err
			}
			weight := mediaBridgeTenantWeight(settings, override, hasOverride)
			if !c.addUsageLocked(tenantID, decodedBytes, weight) {
				c.mu.Unlock()
				return nil, &MediaBridgeCapacityError{
					Reason:      MediaBridgeCapacityReasonInflightBytes,
					Scope:       MediaBridgeCapacityScopeGlobal,
					TenantID:    tenantID,
					Current:     math.MaxInt64,
					Requested:   decodedBytes,
					RetryAfter:  0,
					Description: "media bridge in-flight byte accounting overflow",
				}
			}
			c.signalLocked()
			usage := MediaBridgeCapacitySnapshot{
				Global:   c.globalUsage,
				TenantID: tenantID,
				Tenant:   c.tenantUsage[tenantID],
			}
			c.mu.Unlock()
			lease := &MediaBridgeCapacityLease{
				capacity:           c,
				tenantID:           tenantID,
				decodedBytes:       decodedBytes,
				settings:           settings,
				override:           override,
				hasOverride:        hasOverride,
				bandwidthRemaining: decodedBytes,
			}
			if c.policy != nil {
				input := MediaBridgeCapacityPolicyInput{
					Settings:     settings,
					TenantID:     tenantID,
					DecodedBytes: decodedBytes,
					Usage:        usage,
				}
				if err := c.policy.CheckMediaBridgeCapacity(ctx, input); err != nil {
					lease.Release()
					return nil, err
				}
			}
			return lease, nil
		}

		// A request larger than its byte limit can never fit after a release.
		if capacityErr.Reason == MediaBridgeCapacityReasonInflightBytes && capacityErr.Requested > capacityErr.Limit && capacityErr.Limit > 0 {
			capacityErr.RetryAfter = 0
			c.mu.Unlock()
			return nil, capacityErr
		}
		// Bandwidth admission is a queue-horizon decision: the current
		// unfinished upload work already exceeds the configured maximum wait.
		// Holding a fully parsed request until the horizon expires would only
		// move the same queue into memory, so shed it immediately with 429.
		if capacityErr.Reason == MediaBridgeCapacityReasonBandwidth {
			c.mu.Unlock()
			return nil, capacityErr
		}

		if waitBudget <= 0 {
			c.mu.Unlock()
			return nil, capacityErr
		}

		remaining := deadline.Sub(now)
		if remaining <= 0 {
			c.mu.Unlock()
			return nil, capacityErr
		}
		wakeAfter := remaining
		notify := c.notify
		c.mu.Unlock()

		timer := time.NewTimer(wakeAfter)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return nil, ctx.Err()
		case <-notify:
			if !timer.Stop() {
				<-timer.C
			}
		case <-timer.C:
		}
	}
}

func (c *MediaBridgeCapacity) Snapshot(tenantID int64) MediaBridgeCapacitySnapshot {
	if tenantID < 0 {
		tenantID = 0
	}
	if c == nil {
		return MediaBridgeCapacitySnapshot{TenantID: tenantID}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return MediaBridgeCapacitySnapshot{
		Global:   c.globalUsage,
		TenantID: tenantID,
		Tenant:   c.tenantUsage[tenantID],
	}
}

func (c *MediaBridgeCapacity) initLocked() {
	if c.tenantUsage == nil {
		c.tenantUsage = make(map[int64]MediaBridgeCapacityUsage)
	}
	if c.tenantWeight == nil {
		c.tenantWeight = make(map[int64]int64)
	}
	if c.tenantBandwidth == nil {
		c.tenantBandwidth = make(map[int64]*mediaBridgeBandwidthBucket)
	}
	if c.tenantBandwidthPending == nil {
		c.tenantBandwidthPending = make(map[int64]int64)
	}
	if c.notify == nil {
		c.notify = make(chan struct{})
	}
}

func (c *MediaBridgeCapacity) checkLocked(
	settings MediaBridgeCapacitySettings,
	override MediaBridgeTenantCapacity,
	hasOverride bool,
	tenantID int64,
	decodedBytes int64,
) *MediaBridgeCapacityError {
	if mediaBridgeLimitExceeded(c.globalUsage.InflightRequests, 1, settings.MaxInflightRequests) {
		return mediaBridgeLimitError(
			MediaBridgeCapacityReasonInflightRequests,
			MediaBridgeCapacityScopeGlobal,
			tenantID,
			settings.MaxInflightRequests,
			c.globalUsage.InflightRequests,
			1,
		)
	}
	if mediaBridgeLimitExceeded(c.globalUsage.InflightDecodedBytes, decodedBytes, settings.MaxInflightDecodedBytes) {
		return mediaBridgeLimitError(
			MediaBridgeCapacityReasonInflightBytes,
			MediaBridgeCapacityScopeGlobal,
			tenantID,
			settings.MaxInflightDecodedBytes,
			c.globalUsage.InflightDecodedBytes,
			decodedBytes,
		)
	}

	tenantUsage := c.tenantUsage[tenantID]
	if hasOverride && mediaBridgeLimitExceeded(tenantUsage.InflightRequests, 1, override.MaxInflightRequests) {
		return mediaBridgeLimitError(
			MediaBridgeCapacityReasonInflightRequests,
			MediaBridgeCapacityScopeTenant,
			tenantID,
			override.MaxInflightRequests,
			tenantUsage.InflightRequests,
			1,
		)
	}
	if hasOverride && mediaBridgeLimitExceeded(tenantUsage.InflightDecodedBytes, decodedBytes, override.MaxInflightDecodedBytes) {
		return mediaBridgeLimitError(
			MediaBridgeCapacityReasonInflightBytes,
			MediaBridgeCapacityScopeTenant,
			tenantID,
			override.MaxInflightDecodedBytes,
			tenantUsage.InflightDecodedBytes,
			decodedBytes,
		)
	}

	return nil
}

// checkBandwidthAdmissionLocked bounds only queueing ahead of this file. The
// file's own service time is intentionally not a hard size/concurrency limit:
// the first large upload may always enter an idle lane, while later uploads are
// rejected once unfinished work would make them wait beyond AdmissionWaitMS.
func (c *MediaBridgeCapacity) checkBandwidthAdmissionLocked(
	settings MediaBridgeCapacitySettings,
	override MediaBridgeTenantCapacity,
	hasOverride bool,
	tenantID int64,
	decodedBytes int64,
	waitBudget time.Duration,
	now time.Time,
) *MediaBridgeCapacityError {
	if rate := settings.MaxBandwidthBytesPerSecond; rate > 0 {
		queueDelay := mediaBridgeBandwidthQueueDelay(
			c.globalBandwidthPending,
			rate,
			mediaBridgeBandwidthAvailableBytes(&c.globalBandwidth, now, rate, settings.BurstBytes),
		)
		if queueDelay > waitBudget {
			return mediaBridgeBandwidthError(
				MediaBridgeCapacityScopeGlobal,
				tenantID,
				rate,
				c.globalBandwidthPending,
				decodedBytes,
				queueDelay,
			)
		}
	}

	tenantPending := c.tenantBandwidthPending[tenantID]
	if tenantPending <= 0 {
		return nil
	}
	lease := &MediaBridgeCapacityLease{
		capacity:    c,
		tenantID:    tenantID,
		settings:    settings,
		override:    override,
		hasOverride: hasOverride,
	}
	if rate := c.tenantBandwidthRateLocked(lease); rate > 0 {
		queueDelay := mediaBridgeBandwidthQueueDelay(
			tenantPending,
			rate,
			mediaBridgeBandwidthAvailableBytes(c.tenantBandwidth[tenantID], now, rate, settings.BurstBytes),
		)
		if queueDelay > waitBudget {
			return mediaBridgeBandwidthError(
				MediaBridgeCapacityScopeTenant,
				tenantID,
				rate,
				tenantPending,
				decodedBytes,
				queueDelay,
			)
		}
	}
	return nil
}

func (c *MediaBridgeCapacity) addUsageLocked(tenantID, decodedBytes, weight int64) bool {
	if c.globalUsage.InflightRequests == math.MaxInt64 ||
		c.globalUsage.InflightDecodedBytes > math.MaxInt64-decodedBytes ||
		c.globalBandwidthPending > math.MaxInt64-decodedBytes {
		return false
	}
	tenantUsage := c.tenantUsage[tenantID]
	if tenantUsage.InflightRequests == math.MaxInt64 ||
		tenantUsage.InflightDecodedBytes > math.MaxInt64-decodedBytes ||
		c.tenantBandwidthPending[tenantID] > math.MaxInt64-decodedBytes {
		return false
	}

	c.globalUsage.InflightRequests++
	c.globalUsage.InflightDecodedBytes += decodedBytes
	tenantUsage.InflightRequests++
	tenantUsage.InflightDecodedBytes += decodedBytes
	c.tenantUsage[tenantID] = tenantUsage
	c.globalBandwidthPending += decodedBytes
	c.tenantBandwidthPending[tenantID] += decodedBytes
	if tenantUsage.InflightRequests == 1 {
		c.tenantWeight[tenantID] = weight
		if c.totalWeight > math.MaxInt64-weight {
			c.totalWeight = math.MaxInt64
		} else {
			c.totalWeight += weight
		}
	}
	return true
}

func (c *MediaBridgeCapacity) releaseLease(lease *MediaBridgeCapacityLease) {
	if lease == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.initLocked()
	tenantID := lease.tenantID
	decodedBytes := lease.decodedBytes
	c.consumeBandwidthPendingLocked(lease, lease.bandwidthRemaining)

	if c.globalUsage.InflightRequests > 0 {
		c.globalUsage.InflightRequests--
	}
	if c.globalUsage.InflightDecodedBytes >= decodedBytes {
		c.globalUsage.InflightDecodedBytes -= decodedBytes
	} else {
		c.globalUsage.InflightDecodedBytes = 0
	}

	tenantUsage := c.tenantUsage[tenantID]
	if tenantUsage.InflightRequests > 0 {
		tenantUsage.InflightRequests--
	}
	if tenantUsage.InflightDecodedBytes >= decodedBytes {
		tenantUsage.InflightDecodedBytes -= decodedBytes
	} else {
		tenantUsage.InflightDecodedBytes = 0
	}
	if tenantUsage.InflightRequests == 0 && tenantUsage.InflightDecodedBytes == 0 {
		if weight := c.tenantWeight[tenantID]; weight > 0 {
			if c.totalWeight >= weight {
				c.totalWeight -= weight
			} else {
				c.totalWeight = 0
			}
		}
		delete(c.tenantUsage, tenantID)
		delete(c.tenantWeight, tenantID)
	} else {
		c.tenantUsage[tenantID] = tenantUsage
	}

	c.signalLocked()
}

func (c *MediaBridgeCapacity) consumeBandwidthPendingLocked(lease *MediaBridgeCapacityLease, requested int64) {
	if lease == nil || requested <= 0 || lease.bandwidthRemaining <= 0 {
		return
	}
	consumed := requested
	if consumed > lease.bandwidthRemaining {
		consumed = lease.bandwidthRemaining
	}
	lease.bandwidthRemaining -= consumed
	if c.globalBandwidthPending >= consumed {
		c.globalBandwidthPending -= consumed
	} else {
		c.globalBandwidthPending = 0
	}
	tenantPending := c.tenantBandwidthPending[lease.tenantID]
	if tenantPending > consumed {
		c.tenantBandwidthPending[lease.tenantID] = tenantPending - consumed
	} else {
		delete(c.tenantBandwidthPending, lease.tenantID)
	}
}

func (c *MediaBridgeCapacity) signalLocked() {
	close(c.notify)
	c.notify = make(chan struct{})
}

func (c *MediaBridgeCapacity) sweepIdleBandwidthBucketsLocked(now time.Time) {
	if !c.nextBucketSweep.IsZero() && now.Before(c.nextBucketSweep) {
		return
	}
	c.nextBucketSweep = now.Add(mediaBridgeBandwidthSweepInterval)
	for tenantID, bucket := range c.tenantBandwidth {
		if bucket == nil || c.tenantUsage[tenantID].InflightRequests > 0 {
			continue
		}
		bucket.refill(now)
		if !bucket.initialized || bucket.tokens >= float64(bucket.burst) {
			delete(c.tenantBandwidth, tenantID)
		}
	}
}

// WrapReader throttles bytes as the object store reads them. Acquire records
// pending bandwidth work for bounded admission; actual bandwidth is charged
// here so a single large PUT cannot bypass the configured sustained rate.
func (l *MediaBridgeCapacityLease) WrapReader(ctx context.Context, reader io.Reader) io.Reader {
	if reader == nil || l == nil || l.capacity == nil {
		return reader
	}
	if l.settings.MaxBandwidthBytesPerSecond <= 0 &&
		(!l.hasOverride || l.override.MaxBandwidthBytesPerSecond <= 0) {
		return reader
	}
	if ctx == nil {
		ctx = context.Background()
	}
	return &mediaBridgeCapacityReader{
		ctx:      ctx,
		reader:   reader,
		lease:    l,
		maxChunk: l.capacity.maxBandwidthChunk(l),
	}
}

// Throttle waits until decodedBytes may be transferred under the global and
// weighted tenant token buckets. AdmissionWaitMS applies only to Acquire:
// aborting an accepted PUT merely because a later token takes longer would
// leave an ambiguous partial object. Transfer throttling is bounded by ctx,
// which should carry the object-upload timeout.
func (l *MediaBridgeCapacityLease) Throttle(ctx context.Context, decodedBytes int64) error {
	if decodedBytes <= 0 || l == nil || l.capacity == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}

	remainingBytes := decodedBytes
	for remainingBytes > 0 {
		consumed, err := l.capacity.waitBandwidthChunk(ctx, l, remainingBytes)
		if err != nil {
			return err
		}
		if consumed <= 0 {
			return nil
		}
		remainingBytes -= consumed
	}
	return nil
}

type mediaBridgeCapacityReader struct {
	ctx      context.Context
	reader   io.Reader
	lease    *MediaBridgeCapacityLease
	maxChunk int64
}

func (r *mediaBridgeCapacityReader) Read(buffer []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	if r.maxChunk > 0 && int64(len(buffer)) > r.maxChunk {
		buffer = buffer[:int(r.maxChunk)]
	}
	read, err := r.reader.Read(buffer)
	if read <= 0 {
		return read, err
	}
	if throttleErr := r.lease.Throttle(r.ctx, int64(read)); throttleErr != nil {
		return 0, throttleErr
	}
	return read, err
}

func (c *MediaBridgeCapacity) maxBandwidthChunk(lease *MediaBridgeCapacityLease) int64 {
	if c == nil || lease == nil {
		return 0
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.initLocked()

	maxChunk := int64(0)
	if rate := lease.settings.MaxBandwidthBytesPerSecond; rate > 0 {
		maxChunk = mediaBridgeBandwidthBurst(rate, lease.settings.BurstBytes)
	}
	if rate := c.tenantBandwidthRateLocked(lease); rate > 0 {
		burst := mediaBridgeBandwidthBurst(rate, lease.settings.BurstBytes)
		if maxChunk == 0 || burst < maxChunk {
			maxChunk = burst
		}
	}
	return maxChunk
}

func (c *MediaBridgeCapacity) waitBandwidthChunk(
	ctx context.Context,
	lease *MediaBridgeCapacityLease,
	requested int64,
) (int64, error) {
	for {
		if err := ctx.Err(); err != nil {
			return 0, err
		}

		now := time.Now()
		c.mu.Lock()
		c.initLocked()

		globalRate := lease.settings.MaxBandwidthBytesPerSecond
		tenantRate := c.tenantBandwidthRateLocked(lease)
		chunk := requested
		if globalRate > 0 {
			burst := mediaBridgeBandwidthBurst(globalRate, lease.settings.BurstBytes)
			if chunk > burst {
				chunk = burst
			}
			c.globalBandwidth.configure(now, globalRate, lease.settings.BurstBytes)
		}
		var tenantBucket *mediaBridgeBandwidthBucket
		if tenantRate > 0 {
			burst := mediaBridgeBandwidthBurst(tenantRate, lease.settings.BurstBytes)
			if chunk > burst {
				chunk = burst
			}
			tenantBucket = c.tenantBandwidth[lease.tenantID]
			if tenantBucket == nil {
				tenantBucket = &mediaBridgeBandwidthBucket{}
				c.tenantBandwidth[lease.tenantID] = tenantBucket
			}
			tenantBucket.configure(now, tenantRate, lease.settings.BurstBytes)
		}

		if globalRate <= 0 && tenantRate <= 0 {
			c.consumeBandwidthPendingLocked(lease, requested)
			c.mu.Unlock()
			return requested, nil
		}

		globalWait := time.Duration(0)
		if globalRate > 0 {
			globalWait = c.globalBandwidth.waitDuration(now, chunk)
		}
		tenantWait := time.Duration(0)
		if tenantBucket != nil {
			tenantWait = tenantBucket.waitDuration(now, chunk)
		}
		if globalWait <= 0 && tenantWait <= 0 {
			if err := ctx.Err(); err != nil {
				c.mu.Unlock()
				return 0, err
			}
			if globalRate > 0 {
				c.globalBandwidth.consume(now, chunk)
			}
			if tenantBucket != nil {
				tenantBucket.consume(now, chunk)
			}
			c.consumeBandwidthPendingLocked(lease, chunk)
			c.mu.Unlock()
			return chunk, nil
		}

		wait := globalWait
		if tenantWait > wait {
			wait = tenantWait
		}
		notify := c.notify
		c.mu.Unlock()

		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return 0, ctx.Err()
		case <-notify:
			if !timer.Stop() {
				<-timer.C
			}
		case <-timer.C:
		}
	}
}

func (c *MediaBridgeCapacity) tenantBandwidthRateLocked(lease *MediaBridgeCapacityLease) int64 {
	if lease == nil {
		return 0
	}
	hardLimit := int64(0)
	if lease.hasOverride {
		hardLimit = lease.override.MaxBandwidthBytesPerSecond
	}
	globalRate := lease.settings.MaxBandwidthBytesPerSecond
	if globalRate <= 0 {
		return hardLimit
	}

	weight := c.tenantWeight[lease.tenantID]
	if weight <= 0 {
		weight = mediaBridgeTenantWeight(lease.settings, lease.override, lease.hasOverride)
	}
	totalWeight := c.totalWeight
	if totalWeight <= 0 {
		totalWeight = weight
	}
	share := mediaBridgeMulDivPositive(globalRate, weight, totalWeight)
	if share <= 0 {
		share = 1
	}
	if hardLimit > 0 && hardLimit < share {
		return hardLimit
	}
	return share
}

func mediaBridgeTenantWeight(
	settings MediaBridgeCapacitySettings,
	override MediaBridgeTenantCapacity,
	hasOverride bool,
) int64 {
	weight := settings.DefaultTenantWeight
	if weight <= 0 {
		weight = 1
	}
	if hasOverride && override.Weight > 0 {
		weight = override.Weight
	}
	return weight
}

func mediaBridgeMulDivPositive(value, multiplier, divisor int64) int64 {
	if value <= 0 || multiplier <= 0 || divisor <= 0 {
		return 0
	}
	if value <= math.MaxInt64/multiplier {
		return value * multiplier / divisor
	}
	result := float64(value) * float64(multiplier) / float64(divisor)
	if result >= float64(math.MaxInt64) {
		return math.MaxInt64
	}
	return int64(result)
}

func mediaBridgeBandwidthBurst(rate, configuredBurst int64) int64 {
	if configuredBurst > 0 {
		return configuredBurst
	}
	if rate > 0 {
		return rate
	}
	return 0
}

func mediaBridgeTenantOverride(
	settings MediaBridgeCapacitySettings,
	tenantID int64,
) (MediaBridgeTenantCapacity, bool) {
	if tenantID <= 0 {
		return MediaBridgeTenantCapacity{}, false
	}
	for _, override := range settings.TenantOverrides {
		if override.TenantID == tenantID {
			return override, true
		}
	}
	return MediaBridgeTenantCapacity{}, false
}

func mediaBridgeLimitExceeded(current, requested, limit int64) bool {
	if limit <= 0 {
		return false
	}
	return requested > limit || current > limit-requested
}

func mediaBridgeLimitError(
	reason MediaBridgeCapacityReason,
	scope MediaBridgeCapacityScope,
	tenantID int64,
	limit int64,
	current int64,
	requested int64,
) *MediaBridgeCapacityError {
	return &MediaBridgeCapacityError{
		Reason:     reason,
		Scope:      scope,
		TenantID:   tenantID,
		Limit:      limit,
		Current:    current,
		Requested:  requested,
		RetryAfter: mediaBridgeCapacityDefaultRetryAfter,
	}
}

func mediaBridgeBandwidthError(
	scope MediaBridgeCapacityScope,
	tenantID int64,
	rate int64,
	current int64,
	requested int64,
	retryAfter time.Duration,
) *MediaBridgeCapacityError {
	if retryAfter <= 0 {
		retryAfter = mediaBridgeCapacityDefaultRetryAfter
	}
	return &MediaBridgeCapacityError{
		Reason:     MediaBridgeCapacityReasonBandwidth,
		Scope:      scope,
		TenantID:   tenantID,
		Limit:      rate,
		Current:    current,
		Requested:  requested,
		RetryAfter: retryAfter,
	}
}

func mediaBridgeBandwidthQueueDelay(pendingBytes, rate, availableBytes int64) time.Duration {
	if pendingBytes <= 0 || rate <= 0 {
		return 0
	}
	if pendingBytes <= availableBytes {
		return 0
	}
	queuedBytes := pendingBytes - availableBytes
	seconds := float64(queuedBytes) / float64(rate)
	maxSeconds := float64(math.MaxInt64) / float64(time.Second)
	if seconds >= maxSeconds {
		return time.Duration(math.MaxInt64)
	}
	nanoseconds := math.Ceil(seconds * float64(time.Second))
	if nanoseconds < 1 {
		return time.Nanosecond
	}
	return time.Duration(nanoseconds)
}

func mediaBridgeBandwidthAvailableBytes(
	bucket *mediaBridgeBandwidthBucket,
	now time.Time,
	rate int64,
	configuredBurst int64,
) int64 {
	if rate <= 0 {
		return 0
	}
	burst := mediaBridgeBandwidthBurst(rate, configuredBurst)
	if bucket == nil || !bucket.initialized {
		return burst
	}
	// A hot-settings generation must not inherit an unrelated generation's
	// token balance. Its eventual transfer will reconfigure the bucket; using
	// zero headroom here is the safe admission estimate in the meantime.
	if bucket.rate != rate || bucket.burst != burst {
		return 0
	}
	bucket.refill(now)
	if bucket.tokens <= 0 {
		return 0
	}
	if bucket.tokens >= float64(burst) {
		return burst
	}
	return int64(bucket.tokens)
}

func mediaBridgeMilliseconds(milliseconds int64) time.Duration {
	if milliseconds <= 0 {
		return 0
	}
	if milliseconds > math.MaxInt64/int64(time.Millisecond) {
		return time.Duration(math.MaxInt64)
	}
	return time.Duration(milliseconds) * time.Millisecond
}

func (b *mediaBridgeBandwidthBucket) configure(now time.Time, rate, configuredBurst int64) {
	if rate <= 0 {
		*b = mediaBridgeBandwidthBucket{}
		return
	}
	burst := mediaBridgeBandwidthBurst(rate, configuredBurst)
	if !b.initialized {
		b.initialized = true
		b.rate = rate
		b.burst = burst
		b.tokens = float64(burst)
		b.last = now
		return
	}

	b.refill(now)
	b.rate = rate
	b.burst = burst
	if b.tokens > float64(burst) {
		b.tokens = float64(burst)
	}
	b.last = now
}

func (b *mediaBridgeBandwidthBucket) refill(now time.Time) {
	if !b.initialized || b.rate <= 0 || !now.After(b.last) {
		return
	}
	refilled := now.Sub(b.last).Seconds() * float64(b.rate)
	b.tokens = math.Min(float64(b.burst), b.tokens+refilled)
	b.last = now
}

func (b *mediaBridgeBandwidthBucket) waitDuration(now time.Time, requested int64) time.Duration {
	if !b.initialized || b.rate <= 0 || requested <= 0 {
		return 0
	}
	b.refill(now)
	requiredTokens := float64(requested)
	if requested > b.burst {
		return time.Duration(math.MaxInt64)
	}
	if b.tokens >= requiredTokens {
		return 0
	}
	seconds := (requiredTokens - b.tokens) / float64(b.rate)
	if seconds >= float64(math.MaxInt64)/float64(time.Second) {
		return time.Duration(math.MaxInt64)
	}
	nanoseconds := math.Ceil(seconds * float64(time.Second))
	if nanoseconds < 1 {
		return time.Nanosecond
	}
	return time.Duration(nanoseconds)
}

func (b *mediaBridgeBandwidthBucket) consume(now time.Time, requested int64) {
	if !b.initialized || b.rate <= 0 || requested <= 0 {
		return
	}
	b.refill(now)
	b.tokens -= float64(requested)
}
