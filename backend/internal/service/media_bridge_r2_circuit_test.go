package service

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type mediaBridgeR2FakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *mediaBridgeR2FakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *mediaBridgeR2FakeClock) advance(duration time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(duration)
	c.mu.Unlock()
}

func TestMediaBridgeR2CircuitOpensAndRecoversThroughHalfOpen(t *testing.T) {
	settings := newMediaBridgeR2SettingsStub()
	clock := &mediaBridgeR2FakeClock{now: time.Unix(100, 0)}
	circuit := NewMediaBridgeR2Circuit(settings, clock)

	permit, err := circuit.Admission(context.Background())
	require.NoError(t, err)
	circuit.Observe(permit, 10*time.Millisecond, errors.New("r2 failed"))
	require.Equal(t, MediaBridgeR2CircuitOpen, circuit.Snapshot().State)

	blocked, err := circuit.Admission(context.Background())
	require.Nil(t, blocked)
	var capacityErr *MediaBridgeCapacityError
	require.ErrorAs(t, err, &capacityErr)
	require.ErrorIs(t, err, ErrMediaBridgeCapacityUnavailable)
	require.Equal(t, MediaBridgeCapacityReasonR2CircuitOpen, capacityErr.Reason)
	require.Equal(t, 5*time.Second, capacityErr.RetryAfter)

	clock.advance(5 * time.Second)
	firstProbe, err := circuit.Admission(context.Background())
	require.NoError(t, err)
	secondProbe, err := circuit.Admission(context.Background())
	require.NoError(t, err)
	require.Equal(t, MediaBridgeR2CircuitHalfOpen, circuit.Snapshot().State)
	thirdProbe, err := circuit.Admission(context.Background())
	require.Nil(t, thirdProbe)
	require.ErrorAs(t, err, &capacityErr)
	require.Equal(t, MediaBridgeCapacityReasonR2HalfOpenLimited, capacityErr.Reason)

	circuit.Observe(firstProbe, 10*time.Millisecond, nil)
	require.Equal(t, MediaBridgeR2CircuitHalfOpen, circuit.Snapshot().State)
	circuit.Observe(secondProbe, 10*time.Millisecond, nil)
	require.Equal(t, MediaBridgeR2CircuitClosed, circuit.Snapshot().State)
}

func TestMediaBridgeR2CircuitHalfOpenFailureReopensAndIgnoresStaleSuccess(t *testing.T) {
	settings := newMediaBridgeR2SettingsStub()
	clock := &mediaBridgeR2FakeClock{now: time.Unix(100, 0)}
	circuit := NewMediaBridgeR2Circuit(settings, clock)

	trip, err := circuit.Admission(context.Background())
	require.NoError(t, err)
	circuit.Observe(trip, 0, errors.New("trip"))
	clock.advance(5 * time.Second)
	failedProbe, err := circuit.Admission(context.Background())
	require.NoError(t, err)
	staleProbe, err := circuit.Admission(context.Background())
	require.NoError(t, err)

	circuit.Observe(failedProbe, 0, errors.New("still unhealthy"))
	require.Equal(t, MediaBridgeR2CircuitOpen, circuit.Snapshot().State)
	circuit.Observe(staleProbe, 0, nil)
	require.Equal(t, MediaBridgeR2CircuitOpen, circuit.Snapshot().State)
}

func TestMediaBridgeR2CircuitLatencyUsesWindowP95(t *testing.T) {
	settings := newMediaBridgeR2SettingsStub()
	settings.setProtection(MediaBridgeProtectionSettings{
		R2LatencyThresholdMS: 100,
		R2WindowSeconds:      60,
		R2OpenSeconds:        5,
		R2HalfOpenProbes:     1,
		R2MinimumSamples:     1,
	})
	clock := &mediaBridgeR2FakeClock{now: time.Unix(100, 0)}
	circuit := NewMediaBridgeR2Circuit(settings, clock)

	for i := 0; i < 18; i++ {
		permit, err := circuit.Admission(context.Background())
		require.NoError(t, err)
		circuit.Observe(permit, 10*time.Millisecond, nil)
	}
	require.Equal(t, MediaBridgeR2CircuitClosed, circuit.Snapshot().State)
	permit, err := circuit.Admission(context.Background())
	require.NoError(t, err)
	circuit.Observe(permit, 100*time.Millisecond, nil)
	require.Equal(t, MediaBridgeR2CircuitOpen, circuit.Snapshot().State)
}

func TestMediaBridgeR2CircuitAggregatesHighVolumeOutcomesBySecond(t *testing.T) {
	settings := newMediaBridgeR2SettingsStub()
	settings.setProtection(MediaBridgeProtectionSettings{
		R2ErrorRateThresholdPercent: 100,
		R2WindowSeconds:             60,
		R2OpenSeconds:               5,
		R2HalfOpenProbes:            1,
		R2MinimumSamples:            1,
	})
	clock := &mediaBridgeR2FakeClock{now: time.Unix(100, 0)}
	circuit := NewMediaBridgeR2Circuit(settings, clock)

	const samples = 10_000
	for index := 0; index < samples; index++ {
		permit, err := circuit.Admission(context.Background())
		require.NoError(t, err)
		circuit.Observe(permit, 10*time.Millisecond, nil)
	}

	require.Equal(t, MediaBridgeR2CircuitClosed, circuit.Snapshot().State)
	require.Equal(t, samples, circuit.Snapshot().WindowSamples)
	require.Equal(t, 1, circuit.bucketCount)
	require.Len(t, circuit.outcomeBuckets, 61)
}

func TestMediaBridgeR2CircuitTwentyFourHourWindowKeepsBoundedBuckets(t *testing.T) {
	const windowSeconds = int64(24 * 60 * 60)
	settings := newMediaBridgeR2SettingsStub()
	settings.setProtection(MediaBridgeProtectionSettings{
		R2ErrorRateThresholdPercent: 100,
		R2WindowSeconds:             windowSeconds,
		R2OpenSeconds:               5,
		R2HalfOpenProbes:            1,
		R2MinimumSamples:            1,
	})
	clock := &mediaBridgeR2FakeClock{now: time.Unix(100, 0)}
	circuit := NewMediaBridgeR2Circuit(settings, clock)

	config := mediaBridgeR2Config(settings.GetMediaBridgeSettingsCached(context.Background()).Protection)
	startedAt := clock.Now()
	circuit.mu.Lock()
	circuit.applyConfigLocked(config)
	for second := int64(0); second < windowSeconds+2; second++ {
		now := startedAt.Add(time.Duration(second) * time.Second)
		circuit.pruneOutcomesLocked(now)
		circuit.recordOutcomeLocked(now, false, false)
	}
	circuit.mu.Unlock()

	const maxBuckets = int(windowSeconds) + 1
	require.Equal(t, MediaBridgeR2CircuitClosed, circuit.Snapshot().State)
	require.Equal(t, maxBuckets, circuit.Snapshot().WindowSamples)
	require.Equal(t, maxBuckets, circuit.bucketCount)
	require.Len(t, circuit.outcomeBuckets, maxBuckets)
}

func TestMediaBridgeR2CircuitPrunesExpiredBucketCounts(t *testing.T) {
	settings := newMediaBridgeR2SettingsStub()
	settings.setProtection(MediaBridgeProtectionSettings{
		R2ErrorRateThresholdPercent: 100,
		R2WindowSeconds:             2,
		R2OpenSeconds:               5,
		R2HalfOpenProbes:            1,
		R2MinimumSamples:            1,
	})
	clock := &mediaBridgeR2FakeClock{now: time.Unix(100, 0)}
	circuit := NewMediaBridgeR2Circuit(settings, clock)

	first, err := circuit.Admission(context.Background())
	require.NoError(t, err)
	circuit.Observe(first, 10*time.Millisecond, nil)
	clock.advance(3 * time.Second)
	second, err := circuit.Admission(context.Background())
	require.NoError(t, err)
	circuit.Observe(second, 10*time.Millisecond, nil)

	require.Equal(t, 1, circuit.Snapshot().WindowSamples)
	require.Equal(t, 1, circuit.bucketCount)
}

func TestMediaBridgeR2CircuitHotDisableResetsOpenState(t *testing.T) {
	settings := newMediaBridgeR2SettingsStub()
	clock := &mediaBridgeR2FakeClock{now: time.Unix(100, 0)}
	circuit := NewMediaBridgeR2Circuit(settings, clock)

	permit, err := circuit.Admission(context.Background())
	require.NoError(t, err)
	circuit.Observe(permit, 0, errors.New("trip"))
	require.Equal(t, MediaBridgeR2CircuitOpen, circuit.Snapshot().State)

	settings.setProtection(MediaBridgeProtectionSettings{})
	permit, err = circuit.Admission(context.Background())
	require.NoError(t, err)
	require.NotNil(t, permit)
	require.Equal(t, MediaBridgeR2CircuitClosed, circuit.Snapshot().State)
}

func TestMediaBridgeR2CircuitHalfOpenAdmissionIsConcurrencySafe(t *testing.T) {
	settings := newMediaBridgeR2SettingsStub()
	settings.setProtection(MediaBridgeProtectionSettings{
		R2ErrorRateThresholdPercent: 1,
		R2WindowSeconds:             60,
		R2OpenSeconds:               1,
		R2HalfOpenProbes:            3,
		R2MinimumSamples:            1,
	})
	clock := &mediaBridgeR2FakeClock{now: time.Unix(100, 0)}
	circuit := NewMediaBridgeR2Circuit(settings, clock)
	trip, err := circuit.Admission(context.Background())
	require.NoError(t, err)
	circuit.Observe(trip, 0, errors.New("trip"))
	clock.advance(time.Second)

	const goroutines = 100
	start := make(chan struct{})
	permits := make(chan *MediaBridgeR2Permit, goroutines)
	var admitted atomic.Int64
	var wg sync.WaitGroup
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			permit, admissionErr := circuit.Admission(context.Background())
			if admissionErr == nil {
				admitted.Add(1)
				permits <- permit
			}
		}()
	}
	close(start)
	wg.Wait()
	close(permits)
	require.EqualValues(t, 3, admitted.Load())
	for permit := range permits {
		circuit.Observe(permit, 0, nil)
	}
	require.Equal(t, MediaBridgeR2CircuitClosed, circuit.Snapshot().State)
}

func TestMediaBridgeR2CircuitObserveIsIdempotent(t *testing.T) {
	settings := newMediaBridgeR2SettingsStub()
	settings.setProtection(MediaBridgeProtectionSettings{
		R2ErrorRateThresholdPercent: 100,
		R2WindowSeconds:             60,
		R2OpenSeconds:               5,
		R2HalfOpenProbes:            1,
		R2MinimumSamples:            1,
	})
	circuit := NewMediaBridgeR2Circuit(settings)
	permit, err := circuit.Admission(context.Background())
	require.NoError(t, err)
	circuit.Observe(permit, 0, nil)
	circuit.Observe(permit, 0, errors.New("must be ignored"))
	require.Equal(t, 1, circuit.Snapshot().WindowSamples)
}

func TestMediaBridgeR2CircuitWaitsForMinimumSamples(t *testing.T) {
	settings := newMediaBridgeR2SettingsStub()
	settings.setProtection(MediaBridgeProtectionSettings{
		R2ErrorRateThresholdPercent: 1,
		R2WindowSeconds:             60,
		R2OpenSeconds:               5,
		R2HalfOpenProbes:            1,
		R2MinimumSamples:            3,
	})
	circuit := NewMediaBridgeR2Circuit(settings)
	for index := 0; index < 2; index++ {
		permit, err := circuit.Admission(context.Background())
		require.NoError(t, err)
		circuit.Observe(permit, 0, errors.New("r2 failed"))
		require.Equal(t, MediaBridgeR2CircuitClosed, circuit.Snapshot().State)
	}
	permit, err := circuit.Admission(context.Background())
	require.NoError(t, err)
	circuit.Observe(permit, 0, errors.New("r2 failed"))
	require.Equal(t, MediaBridgeR2CircuitOpen, circuit.Snapshot().State)
}

func TestMediaBridgeR2CircuitIgnoresRequestCancellation(t *testing.T) {
	circuit := NewMediaBridgeR2Circuit(newMediaBridgeR2SettingsStub())
	permit, err := circuit.Admission(context.Background())
	require.NoError(t, err)
	circuit.Observe(permit, time.Hour, context.Canceled)
	require.Equal(t, MediaBridgeR2CircuitClosed, circuit.Snapshot().State)
	require.Zero(t, circuit.Snapshot().WindowSamples)
}

func newMediaBridgeR2SettingsStub() *mediaBridgeSettingsSnapshotStub {
	settings := &mediaBridgeSettingsSnapshotStub{}
	settings.setProtection(MediaBridgeProtectionSettings{
		R2ErrorRateThresholdPercent: 1,
		R2LatencyThresholdMS:        100,
		R2WindowSeconds:             60,
		R2OpenSeconds:               5,
		R2HalfOpenProbes:            2,
		R2MinimumSamples:            1,
	})
	return settings
}
