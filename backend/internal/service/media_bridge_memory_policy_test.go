package service

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/shirou/gopsutil/v4/mem"
	"github.com/stretchr/testify/require"
)

type mediaBridgeSettingsSnapshotStub struct {
	mu       sync.RWMutex
	settings MediaBridgeSettings
}

func (s *mediaBridgeSettingsSnapshotStub) GetMediaBridgeSettingsCached(context.Context) MediaBridgeSettings {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return cloneMediaBridgeSettings(s.settings)
}

func (s *mediaBridgeSettingsSnapshotStub) setProtection(protection MediaBridgeProtectionSettings) {
	s.mu.Lock()
	s.settings.Protection = protection
	s.mu.Unlock()
}

func TestMediaBridgeMemoryPolicyZeroThresholdsSkipProbe(t *testing.T) {
	settings := &mediaBridgeSettingsSnapshotStub{}
	probeCalled := false
	policy := NewMediaBridgeMemoryPolicy(settings, MediaBridgeMemoryProbeFunc(func(context.Context) (MediaBridgeMemorySample, error) {
		probeCalled = true
		return MediaBridgeMemorySample{}, errors.New("must not be called")
	}))

	err := policy.CheckMediaBridgeCapacity(context.Background(), MediaBridgeCapacityPolicyInput{DecodedBytes: 1})
	require.NoError(t, err)
	require.False(t, probeCalled)
}

func TestMediaBridgeMemoryPolicyHardLimitHasPriority(t *testing.T) {
	settings := &mediaBridgeSettingsSnapshotStub{}
	settings.setProtection(MediaBridgeProtectionSettings{
		MemorySoftLimitPercent: 70,
		MemoryHardLimitPercent: 80,
		MinFreeMemoryBytes:     300,
	})
	policy := NewMediaBridgeMemoryPolicy(settings, fixedMediaBridgeMemoryProbe(MediaBridgeMemorySample{
		UsedBytes:      850,
		TotalBytes:     1000,
		AvailableBytes: 150,
	}))

	err := policy.CheckMediaBridgeCapacity(context.Background(), MediaBridgeCapacityPolicyInput{
		TenantID:     7,
		DecodedBytes: 128,
	})
	capacityErr := requireMediaBridgeMemoryCapacityError(t, err, MediaBridgeCapacityReasonMemoryHard)
	require.EqualValues(t, 80, capacityErr.Limit)
	require.EqualValues(t, 85, capacityErr.Current)
	require.EqualValues(t, 7, capacityErr.TenantID)
	require.EqualValues(t, 128, capacityErr.Requested)
}

func TestMediaBridgeMemoryPolicyMinFreeAndSoftLimits(t *testing.T) {
	tests := []struct {
		name       string
		protection MediaBridgeProtectionSettings
		sample     MediaBridgeMemorySample
		reason     MediaBridgeCapacityReason
	}{
		{
			name:       "minimum free",
			protection: MediaBridgeProtectionSettings{MinFreeMemoryBytes: 200},
			sample:     MediaBridgeMemorySample{UsedBytes: 700, TotalBytes: 1000, AvailableBytes: 199},
			reason:     MediaBridgeCapacityReasonMemoryMinFree,
		},
		{
			name:       "soft percent",
			protection: MediaBridgeProtectionSettings{MemorySoftLimitPercent: 70},
			sample:     MediaBridgeMemorySample{UsedBytes: 700, TotalBytes: 1000, AvailableBytes: 300},
			reason:     MediaBridgeCapacityReasonMemorySoft,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			settings := &mediaBridgeSettingsSnapshotStub{}
			settings.setProtection(tt.protection)
			policy := NewMediaBridgeMemoryPolicy(settings, fixedMediaBridgeMemoryProbe(tt.sample))
			err := policy.CheckMediaBridgeCapacity(context.Background(), MediaBridgeCapacityPolicyInput{DecodedBytes: 1})
			requireMediaBridgeMemoryCapacityError(t, err, tt.reason)
		})
	}
}

func TestMediaBridgeMemoryPolicyAllowsBelowEveryThreshold(t *testing.T) {
	settings := &mediaBridgeSettingsSnapshotStub{}
	settings.setProtection(MediaBridgeProtectionSettings{
		MemorySoftLimitPercent: 70,
		MemoryHardLimitPercent: 80,
		MinFreeMemoryBytes:     200,
	})
	policy := NewMediaBridgeMemoryPolicy(settings, fixedMediaBridgeMemoryProbe(MediaBridgeMemorySample{
		UsedBytes:      600,
		TotalBytes:     1000,
		AvailableBytes: 400,
	}))
	require.NoError(t, policy.CheckMediaBridgeCapacity(context.Background(), MediaBridgeCapacityPolicyInput{DecodedBytes: 1}))
}

func TestMediaBridgeMemoryPolicyIncludesAtomicallyReservedBytes(t *testing.T) {
	settings := &mediaBridgeSettingsSnapshotStub{}
	settings.setProtection(MediaBridgeProtectionSettings{MemorySoftLimitPercent: 70})
	policy := NewMediaBridgeMemoryPolicy(settings, fixedMediaBridgeMemoryProbe(MediaBridgeMemorySample{
		UsedBytes:      600,
		TotalBytes:     1000,
		AvailableBytes: 400,
	}))
	err := policy.CheckMediaBridgeCapacity(context.Background(), MediaBridgeCapacityPolicyInput{
		DecodedBytes: 150,
		Usage: MediaBridgeCapacitySnapshot{
			Global: MediaBridgeCapacityUsage{InflightRequests: 1, InflightDecodedBytes: 150},
		},
	})
	requireMediaBridgeMemoryCapacityError(t, err, MediaBridgeCapacityReasonMemorySoft)
}

func TestMediaBridgeMemoryPolicyUsesHotProtectionSnapshot(t *testing.T) {
	settings := &mediaBridgeSettingsSnapshotStub{}
	policy := NewMediaBridgeMemoryPolicy(settings, fixedMediaBridgeMemoryProbe(MediaBridgeMemorySample{
		UsedBytes:      900,
		TotalBytes:     1000,
		AvailableBytes: 100,
	}))
	input := MediaBridgeCapacityPolicyInput{DecodedBytes: 1}

	require.NoError(t, policy.CheckMediaBridgeCapacity(context.Background(), input))
	settings.setProtection(MediaBridgeProtectionSettings{MemoryHardLimitPercent: 80})
	requireMediaBridgeMemoryCapacityError(
		t,
		policy.CheckMediaBridgeCapacity(context.Background(), input),
		MediaBridgeCapacityReasonMemoryHard,
	)
}

func TestMediaBridgeMemoryPolicyProbeFailureFailsClosed(t *testing.T) {
	settings := &mediaBridgeSettingsSnapshotStub{}
	settings.setProtection(MediaBridgeProtectionSettings{MemoryHardLimitPercent: 80})
	policy := NewMediaBridgeMemoryPolicy(settings, MediaBridgeMemoryProbeFunc(func(context.Context) (MediaBridgeMemorySample, error) {
		return MediaBridgeMemorySample{}, errors.New("probe failed")
	}))

	err := policy.CheckMediaBridgeCapacity(context.Background(), MediaBridgeCapacityPolicyInput{DecodedBytes: 1})
	requireMediaBridgeMemoryCapacityError(t, err, MediaBridgeCapacityReasonMemoryUnavailable)
}

func TestResolveMediaBridgeMemorySampleUsesOneScope(t *testing.T) {
	host := &mem.VirtualMemoryStat{
		Used:      800,
		Total:     1000,
		Available: 200,
	}

	cgroup, err := resolveMediaBridgeMemorySample(50, 100, true, host)
	require.NoError(t, err)
	require.Equal(t, MediaBridgeMemorySample{UsedBytes: 50, TotalBytes: 100, AvailableBytes: 50, Source: "cgroup"}, cgroup)

	hostFallback, err := resolveMediaBridgeMemorySample(50, 0, true, host)
	require.NoError(t, err)
	require.Equal(t, MediaBridgeMemorySample{UsedBytes: 800, TotalBytes: 1000, AvailableBytes: 200, Source: "host"}, hostFallback)
}

func fixedMediaBridgeMemoryProbe(sample MediaBridgeMemorySample) MediaBridgeMemoryProbe {
	return MediaBridgeMemoryProbeFunc(func(context.Context) (MediaBridgeMemorySample, error) {
		return sample, nil
	})
}

func requireMediaBridgeMemoryCapacityError(
	t *testing.T,
	err error,
	reason MediaBridgeCapacityReason,
) *MediaBridgeCapacityError {
	t.Helper()
	var capacityErr *MediaBridgeCapacityError
	require.ErrorAs(t, err, &capacityErr)
	require.ErrorIs(t, err, ErrMediaBridgeCapacityUnavailable)
	require.Equal(t, reason, capacityErr.Reason)
	require.Equal(t, MediaBridgeCapacityScopeGlobal, capacityErr.Scope)
	require.Positive(t, capacityErr.RetryAfter)
	return capacityErr
}
