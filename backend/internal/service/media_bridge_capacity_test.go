package service

import (
	"bytes"
	"context"
	"errors"
	"io"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestMediaBridgeCapacityRejectsInvalidDecodedBytes(t *testing.T) {
	capacity := NewMediaBridgeCapacity()

	for _, decodedBytes := range []int64{0, -1} {
		lease, err := capacity.Acquire(context.Background(), MediaBridgeCapacitySettings{}, 1, decodedBytes)
		require.Nil(t, lease)
		require.ErrorIs(t, err, ErrMediaBridgeInvalidDecodedBytes)
	}
}

func TestMediaBridgeCapacityZeroLimitsAreUnlimitedAndNoFixedCeiling(t *testing.T) {
	capacity := NewMediaBridgeCapacity()
	settings := MediaBridgeCapacitySettings{}
	leases := make([]*MediaBridgeCapacityLease, 2048)

	for i := range leases {
		lease, err := capacity.Acquire(context.Background(), settings, 0, 1)
		require.NoError(t, err)
		leases[i] = lease
	}

	snapshot := capacity.Snapshot(0)
	require.EqualValues(t, len(leases), snapshot.Global.InflightRequests)
	require.EqualValues(t, len(leases), snapshot.Global.InflightDecodedBytes)
	require.Equal(t, snapshot.Global, snapshot.Tenant)

	for _, lease := range leases {
		lease.Release()
	}
	require.Equal(t, MediaBridgeCapacityUsage{}, capacity.Snapshot(0).Global)
}

func TestMediaBridgeCapacityRequestLimitAndIdempotentRelease(t *testing.T) {
	capacity := NewMediaBridgeCapacity()
	settings := MediaBridgeCapacitySettings{MaxInflightRequests: 1}

	first, err := capacity.Acquire(context.Background(), settings, 7, 64)
	require.NoError(t, err)

	second, err := capacity.Acquire(context.Background(), settings, 7, 64)
	require.Nil(t, second)
	var capacityErr *MediaBridgeCapacityError
	require.ErrorAs(t, err, &capacityErr)
	require.ErrorIs(t, err, ErrMediaBridgeCapacityUnavailable)
	require.Equal(t, MediaBridgeCapacityReasonInflightRequests, capacityErr.Reason)
	require.Equal(t, MediaBridgeCapacityScopeGlobal, capacityErr.Scope)
	require.Positive(t, capacityErr.RetryAfter)

	first.Release()
	first.Release()
	require.Equal(t, MediaBridgeCapacityUsage{}, capacity.Snapshot(7).Global)

	second, err = capacity.Acquire(context.Background(), settings, 7, 64)
	require.NoError(t, err)
	second.Release()
}

func TestMediaBridgeCapacityTenantOverrideIsAdditionalToGlobalLimit(t *testing.T) {
	capacity := NewMediaBridgeCapacity()
	settings := MediaBridgeCapacitySettings{
		MaxInflightDecodedBytes: 1000,
		TenantOverrides: []MediaBridgeTenantCapacity{
			{TenantID: 7, Weight: 10, MaxInflightRequests: 1, MaxInflightDecodedBytes: 100},
			{TenantID: 8, Weight: 10},
		},
	}

	first, err := capacity.Acquire(context.Background(), settings, 7, 80)
	require.NoError(t, err)

	lease, err := capacity.Acquire(context.Background(), settings, 7, 20)
	require.Nil(t, lease)
	var capacityErr *MediaBridgeCapacityError
	require.ErrorAs(t, err, &capacityErr)
	require.Equal(t, MediaBridgeCapacityScopeTenant, capacityErr.Scope)
	require.Equal(t, MediaBridgeCapacityReasonInflightRequests, capacityErr.Reason)

	otherTenant, err := capacity.Acquire(context.Background(), settings, 8, 500)
	require.NoError(t, err, "zero tenant limits must remain unlimited within the global limit")
	require.EqualValues(t, 580, capacity.Snapshot(8).Global.InflightDecodedBytes)
	require.EqualValues(t, 500, capacity.Snapshot(8).Tenant.InflightDecodedBytes)

	first.Release()
	otherTenant.Release()
}

func TestMediaBridgeCapacityRejectsRequestLargerThanByteLimitWithoutWaiting(t *testing.T) {
	capacity := NewMediaBridgeCapacity()
	settings := MediaBridgeCapacitySettings{
		MaxInflightDecodedBytes: 100,
		AdmissionWaitMS:         1000,
	}

	started := time.Now()
	lease, err := capacity.Acquire(context.Background(), settings, 1, 101)
	require.Nil(t, lease)
	require.Less(t, time.Since(started), 100*time.Millisecond)
	var capacityErr *MediaBridgeCapacityError
	require.ErrorAs(t, err, &capacityErr)
	require.Equal(t, MediaBridgeCapacityReasonInflightBytes, capacityErr.Reason)
	require.Zero(t, capacityErr.RetryAfter)
}

func TestMediaBridgeCapacityWaitsForRelease(t *testing.T) {
	capacity := NewMediaBridgeCapacity()
	settings := MediaBridgeCapacitySettings{
		MaxInflightRequests: 1,
		AdmissionWaitMS:     500,
	}
	first, err := capacity.Acquire(context.Background(), settings, 1, 10)
	require.NoError(t, err)

	result := make(chan error, 1)
	var second *MediaBridgeCapacityLease
	go func() {
		var acquireErr error
		second, acquireErr = capacity.Acquire(context.Background(), settings, 1, 10)
		result <- acquireErr
	}()

	require.Never(t, func() bool {
		select {
		case <-result:
			return true
		default:
			return false
		}
	}, 30*time.Millisecond, 5*time.Millisecond)
	first.Release()
	require.NoError(t, <-result)
	require.NotNil(t, second)
	second.Release()
}

func TestMediaBridgeCapacityContextCancellationDoesNotLeakUsage(t *testing.T) {
	capacity := NewMediaBridgeCapacity()
	settings := MediaBridgeCapacitySettings{
		MaxInflightRequests: 1,
		AdmissionWaitMS:     5000,
	}
	first, err := capacity.Acquire(context.Background(), settings, 9, 10)
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() {
		_, acquireErr := capacity.Acquire(ctx, settings, 9, 10)
		result <- acquireErr
	}()
	time.Sleep(20 * time.Millisecond)
	cancel()
	require.ErrorIs(t, <-result, context.Canceled)

	snapshot := capacity.Snapshot(9)
	require.EqualValues(t, 1, snapshot.Global.InflightRequests)
	require.EqualValues(t, 1, snapshot.Tenant.InflightRequests)
	first.Release()
	require.Equal(t, MediaBridgeCapacityUsage{}, capacity.Snapshot(9).Global)
}

func TestMediaBridgeCapacityGlobalBandwidthTokenBucket(t *testing.T) {
	capacity := NewMediaBridgeCapacity()
	settings := MediaBridgeCapacitySettings{
		MaxBandwidthBytesPerSecond: 1000,
		BurstBytes:                 100,
		AdmissionWaitMS:            300,
	}

	lease, err := capacity.Acquire(context.Background(), settings, 1, 200)
	require.NoError(t, err)
	defer lease.Release()

	started := time.Now()
	result, err := io.ReadAll(lease.WrapReader(context.Background(), bytes.NewReader(make([]byte, 200))))
	require.NoError(t, err)
	require.Len(t, result, 200)
	require.GreaterOrEqual(t, time.Since(started), 70*time.Millisecond)
}

func TestMediaBridgeCapacityBandwidthAdmissionRejectsQueueBeyondBudget(t *testing.T) {
	capacity := NewMediaBridgeCapacity()
	settings := MediaBridgeCapacitySettings{
		MaxBandwidthBytesPerSecond: 1000,
		BurstBytes:                 100,
		AdmissionWaitMS:            50,
	}

	first, err := capacity.Acquire(context.Background(), settings, 1, 300)
	require.NoError(t, err, "an idle lane must admit a large file regardless of its own service time")
	defer first.Release()

	started := time.Now()
	second, err := capacity.Acquire(context.Background(), settings, 1, 10)
	require.Nil(t, second)
	require.Less(t, time.Since(started), 40*time.Millisecond, "over-budget work must be shed instead of queued in memory")
	var capacityErr *MediaBridgeCapacityError
	require.ErrorAs(t, err, &capacityErr)
	require.ErrorIs(t, err, ErrMediaBridgeCapacityUnavailable)
	require.Equal(t, MediaBridgeCapacityReasonBandwidth, capacityErr.Reason)
	require.Equal(t, MediaBridgeCapacityScopeGlobal, capacityErr.Scope)
	require.EqualValues(t, 1000, capacityErr.Limit)
	require.EqualValues(t, 300, capacityErr.Current)
	require.EqualValues(t, 10, capacityErr.Requested)
	require.Equal(t, 200*time.Millisecond, capacityErr.RetryAfter)

	first.Release()
	second, err = capacity.Acquire(context.Background(), settings, 1, 10)
	require.NoError(t, err)
	second.Release()
}

func TestMediaBridgeCapacityBandwidthAdmissionBoundsConcurrentQueue(t *testing.T) {
	capacity := NewMediaBridgeCapacity()
	settings := MediaBridgeCapacitySettings{
		MaxBandwidthBytesPerSecond: 1000,
		BurstBytes:                 100,
		AdmissionWaitMS:            0,
	}

	const callers = 64
	start := make(chan struct{})
	results := make(chan struct {
		lease *MediaBridgeCapacityLease
		err   error
	}, callers)
	for range callers {
		go func() {
			<-start
			lease, err := capacity.Acquire(context.Background(), settings, 1, 300)
			results <- struct {
				lease *MediaBridgeCapacityLease
				err   error
			}{lease: lease, err: err}
		}()
	}
	close(start)

	admitted := make([]*MediaBridgeCapacityLease, 0, 1)
	for range callers {
		result := <-results
		if result.err == nil {
			admitted = append(admitted, result.lease)
			continue
		}
		var capacityErr *MediaBridgeCapacityError
		require.ErrorAs(t, result.err, &capacityErr)
		require.Equal(t, MediaBridgeCapacityReasonBandwidth, capacityErr.Reason)
	}
	require.Len(t, admitted, 1, "concurrent requests must not build an unbounded PUT queue")
	admitted[0].Release()
}

func TestMediaBridgeCapacityBandwidthAdmissionAllowsQueueWithinBudget(t *testing.T) {
	capacity := NewMediaBridgeCapacity()
	settings := MediaBridgeCapacitySettings{
		MaxBandwidthBytesPerSecond: 1000,
		BurstBytes:                 100,
		AdmissionWaitMS:            250,
	}

	first, err := capacity.Acquire(context.Background(), settings, 1, 300)
	require.NoError(t, err)
	defer first.Release()
	second, err := capacity.Acquire(context.Background(), settings, 1, 10)
	require.NoError(t, err)
	second.Release()
}

func TestMediaBridgeCapacityBandwidthAdmissionTracksTransferredProgress(t *testing.T) {
	capacity := NewMediaBridgeCapacity()
	settings := MediaBridgeCapacitySettings{
		MaxBandwidthBytesPerSecond: 1000,
		BurstBytes:                 100,
		AdmissionWaitMS:            175,
	}

	first, err := capacity.Acquire(context.Background(), settings, 1, 300)
	require.NoError(t, err)
	defer first.Release()
	require.NoError(t, first.Throttle(context.Background(), 150))

	second, err := capacity.Acquire(context.Background(), settings, 1, 10)
	require.NoError(t, err, "bytes already granted to the uploader must not remain in the admission backlog")
	second.Release()
}

func TestMediaBridgeCapacityTenantBandwidthAdmissionIsIsolated(t *testing.T) {
	capacity := NewMediaBridgeCapacity()
	settings := MediaBridgeCapacitySettings{
		BurstBytes:      100,
		AdmissionWaitMS: 50,
		TenantOverrides: []MediaBridgeTenantCapacity{
			{TenantID: 1, Weight: 1, MaxBandwidthBytesPerSecond: 1000},
		},
	}

	first, err := capacity.Acquire(context.Background(), settings, 1, 300)
	require.NoError(t, err)
	defer first.Release()

	second, err := capacity.Acquire(context.Background(), settings, 1, 10)
	require.Nil(t, second)
	var capacityErr *MediaBridgeCapacityError
	require.ErrorAs(t, err, &capacityErr)
	require.Equal(t, MediaBridgeCapacityReasonBandwidth, capacityErr.Reason)
	require.Equal(t, MediaBridgeCapacityScopeTenant, capacityErr.Scope)

	otherTenant, err := capacity.Acquire(context.Background(), settings, 2, 10)
	require.NoError(t, err, "one tenant's bandwidth queue must not reject an unlimited tenant")
	otherTenant.Release()
}

func TestMediaBridgeCapacityBandwidthHotConfigUsesNewAdmissionSnapshot(t *testing.T) {
	capacity := NewMediaBridgeCapacity()
	oldSettings := MediaBridgeCapacitySettings{
		MaxBandwidthBytesPerSecond: 1000,
		BurstBytes:                 100,
		AdmissionWaitMS:            50,
	}
	first, err := capacity.Acquire(context.Background(), oldSettings, 1, 300)
	require.NoError(t, err)
	defer first.Release()

	newSettings := oldSettings
	newSettings.MaxBandwidthBytesPerSecond = 10_000
	second, err := capacity.Acquire(context.Background(), newSettings, 1, 10)
	require.NoError(t, err, "new admissions must use the hot settings snapshot")
	second.Release()
}

func TestMediaBridgeCapacityBandwidthDoesNotImposePerFileHardLimit(t *testing.T) {
	capacity := NewMediaBridgeCapacity()
	settings := MediaBridgeCapacitySettings{
		MaxBandwidthBytesPerSecond: 1,
		BurstBytes:                 1,
		AdmissionWaitMS:            0,
	}

	lease, err := capacity.Acquire(context.Background(), settings, 1, 1<<40)
	require.NoError(t, err)
	lease.Release()
}

func TestMediaBridgeCapacityTenantBandwidthOverrideIsIsolated(t *testing.T) {
	capacity := NewMediaBridgeCapacity()
	settings := MediaBridgeCapacitySettings{
		BurstBytes: 100,
		TenantOverrides: []MediaBridgeTenantCapacity{
			{TenantID: 1, Weight: 10, MaxBandwidthBytesPerSecond: 1000},
		},
	}

	first, err := capacity.Acquire(context.Background(), settings, 1, 100)
	require.NoError(t, err)
	defer first.Release()
	require.NoError(t, first.Throttle(context.Background(), 100))

	waitCtx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	err = first.Throttle(waitCtx, 100)
	require.ErrorIs(t, err, context.DeadlineExceeded)

	otherTenant, err := capacity.Acquire(context.Background(), settings, 2, 100)
	require.NoError(t, err, "a tenant override must not throttle another tenant")
	require.NoError(t, otherTenant.Throttle(context.Background(), 100))
	otherTenant.Release()
}

func TestMediaBridgeCapacityStreamingThrottleSplitsLargerThanBurst(t *testing.T) {
	capacity := NewMediaBridgeCapacity()
	settings := MediaBridgeCapacitySettings{
		MaxBandwidthBytesPerSecond: 10_000,
		BurstBytes:                 100,
		AdmissionWaitMS:            1,
	}

	lease, err := capacity.Acquire(context.Background(), settings, 1, 1000)
	require.NoError(t, err)
	defer lease.Release()

	started := time.Now()
	require.NoError(t, lease.Throttle(context.Background(), 1000))
	require.GreaterOrEqual(t, time.Since(started), 70*time.Millisecond)
}

func TestMediaBridgeCapacityLargeReaderContinuesBeyondAdmissionWait(t *testing.T) {
	capacity := NewMediaBridgeCapacity()
	settings := MediaBridgeCapacitySettings{
		MaxBandwidthBytesPerSecond: 10_000,
		BurstBytes:                 1000,
		AdmissionWaitMS:            10,
	}
	lease, err := capacity.Acquire(context.Background(), settings, 1, 2500)
	require.NoError(t, err)
	defer lease.Release()

	started := time.Now()
	result, err := io.ReadAll(lease.WrapReader(context.Background(), bytes.NewReader(make([]byte, 2500))))
	require.NoError(t, err)
	require.Len(t, result, 2500)
	require.GreaterOrEqual(t, time.Since(started), 120*time.Millisecond)
}

func TestMediaBridgeCapacityTenantWeightControlsFairBandwidthShare(t *testing.T) {
	capacity := NewMediaBridgeCapacity()
	settings := MediaBridgeCapacitySettings{
		MaxBandwidthBytesPerSecond: 1000,
		DefaultTenantWeight:        1,
		TenantOverrides: []MediaBridgeTenantCapacity{
			{TenantID: 2, Weight: 3},
		},
	}

	first, err := capacity.Acquire(context.Background(), settings, 1, 100)
	require.NoError(t, err)
	defer first.Release()
	second, err := capacity.Acquire(context.Background(), settings, 2, 100)
	require.NoError(t, err)
	defer second.Release()

	capacity.mu.Lock()
	firstRate := capacity.tenantBandwidthRateLocked(first)
	secondRate := capacity.tenantBandwidthRateLocked(second)
	capacity.mu.Unlock()
	require.EqualValues(t, 250, firstRate)
	require.EqualValues(t, 750, secondRate)
}

func TestMediaBridgeCapacityBandwidthWaitHonorsContextCancellation(t *testing.T) {
	capacity := NewMediaBridgeCapacity()
	settings := MediaBridgeCapacitySettings{
		MaxBandwidthBytesPerSecond: 1000,
		BurstBytes:                 100,
		AdmissionWaitMS:            5000,
	}
	lease, err := capacity.Acquire(context.Background(), settings, 1, 200)
	require.NoError(t, err)
	defer lease.Release()
	require.NoError(t, lease.Throttle(context.Background(), 100))

	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() { result <- lease.Throttle(ctx, 100) }()
	time.Sleep(20 * time.Millisecond)
	cancel()
	require.ErrorIs(t, <-result, context.Canceled)
}

func TestMediaBridgeCapacityPolicyHookIsInjectable(t *testing.T) {
	policyErr := errors.New("memory pressure")
	var calls atomic.Int64
	capacity := NewMediaBridgeCapacity(MediaBridgeCapacityPolicyFunc(func(
		_ context.Context,
		input MediaBridgeCapacityPolicyInput,
	) error {
		calls.Add(1)
		require.EqualValues(t, 42, input.TenantID)
		require.EqualValues(t, 128, input.DecodedBytes)
		require.EqualValues(t, 1, input.Usage.Global.InflightRequests)
		require.EqualValues(t, 128, input.Usage.Global.InflightDecodedBytes)
		return policyErr
	}))

	lease, err := capacity.Acquire(context.Background(), MediaBridgeCapacitySettings{}, 42, 128)
	require.Nil(t, lease)
	require.ErrorIs(t, err, policyErr)
	require.EqualValues(t, 1, calls.Load())
	require.Equal(t, MediaBridgeCapacityUsage{}, capacity.Snapshot(42).Global)
}
