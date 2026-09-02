package service

import (
	"context"
	"fmt"
	"math"
	"time"

	"github.com/shirou/gopsutil/v4/mem"
)

const (
	MediaBridgeCapacityReasonMemorySoft        MediaBridgeCapacityReason = "memory_soft_limit"
	MediaBridgeCapacityReasonMemoryHard        MediaBridgeCapacityReason = "memory_hard_limit"
	MediaBridgeCapacityReasonMemoryMinFree     MediaBridgeCapacityReason = "memory_min_free"
	MediaBridgeCapacityReasonMemoryUnavailable MediaBridgeCapacityReason = "memory_sample_unavailable"

	mediaBridgeMemoryRetryAfter = time.Second
)

// MediaBridgeSettingsSnapshotSource is satisfied by SettingService. Keeping
// this narrow boundary makes the hot settings read and memory probe independently
// testable without reading a production host.
type MediaBridgeSettingsSnapshotSource interface {
	GetMediaBridgeSettingsCached(context.Context) MediaBridgeSettings
}

type MediaBridgeMemorySample struct {
	UsedBytes      uint64
	TotalBytes     uint64
	AvailableBytes uint64
	Source         string
}

type MediaBridgeMemoryProbe interface {
	SampleMediaBridgeMemory(context.Context) (MediaBridgeMemorySample, error)
}

type MediaBridgeMemoryProbeFunc func(context.Context) (MediaBridgeMemorySample, error)

func (f MediaBridgeMemoryProbeFunc) SampleMediaBridgeMemory(ctx context.Context) (MediaBridgeMemorySample, error) {
	return f(ctx)
}

type systemMediaBridgeMemoryProbe struct{}

func (systemMediaBridgeMemoryProbe) SampleMediaBridgeMemory(ctx context.Context) (MediaBridgeMemorySample, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	cgroupUsed, cgroupTotal, cgroupOK := readCgroupMemoryBytes()
	var host *mem.VirtualMemoryStat
	if !cgroupOK || cgroupTotal == 0 {
		vm, err := mem.VirtualMemoryWithContext(ctx)
		if err != nil {
			return MediaBridgeMemorySample{}, fmt.Errorf("read host memory: %w", err)
		}
		host = vm
	}
	return resolveMediaBridgeMemorySample(cgroupUsed, cgroupTotal, cgroupOK, host)
}

// resolveMediaBridgeMemorySample follows the same cgroup/host rule as the ops
// collector: use cgroup values only with a concrete cgroup limit, otherwise
// fall back entirely to gopsutil host values and never mix the two scopes.
func resolveMediaBridgeMemorySample(
	cgroupUsed uint64,
	cgroupTotal uint64,
	cgroupOK bool,
	host *mem.VirtualMemoryStat,
) (MediaBridgeMemorySample, error) {
	if cgroupOK && cgroupTotal > 0 {
		available := uint64(0)
		if cgroupUsed < cgroupTotal {
			available = cgroupTotal - cgroupUsed
		}
		return MediaBridgeMemorySample{
			UsedBytes:      cgroupUsed,
			TotalBytes:     cgroupTotal,
			AvailableBytes: available,
			Source:         "cgroup",
		}, nil
	}
	if host == nil || host.Total == 0 {
		return MediaBridgeMemorySample{}, fmt.Errorf("memory sample has no usable total")
	}
	return MediaBridgeMemorySample{
		UsedBytes:      host.Used,
		TotalBytes:     host.Total,
		AvailableBytes: host.Available,
		Source:         "host",
	}, nil
}

type MediaBridgeMemoryPolicy struct {
	settings MediaBridgeSettingsSnapshotSource
	probe    MediaBridgeMemoryProbe
}

func NewMediaBridgeMemoryPolicy(
	settings MediaBridgeSettingsSnapshotSource,
	probes ...MediaBridgeMemoryProbe,
) *MediaBridgeMemoryPolicy {
	policy := &MediaBridgeMemoryPolicy{
		settings: settings,
		probe:    systemMediaBridgeMemoryProbe{},
	}
	for _, probe := range probes {
		if probe != nil {
			policy.probe = probe
			break
		}
	}
	return policy
}

var _ MediaBridgeCapacityPolicy = (*MediaBridgeMemoryPolicy)(nil)

func (p *MediaBridgeMemoryPolicy) CheckMediaBridgeCapacity(
	ctx context.Context,
	input MediaBridgeCapacityPolicyInput,
) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if p == nil || p.settings == nil {
		return mediaBridgeMemoryCapacityError(
			MediaBridgeCapacityReasonMemoryUnavailable,
			input,
			0,
			0,
			"media bridge memory settings are unavailable",
		)
	}

	protection := p.settings.GetMediaBridgeSettingsCached(ctx).Protection
	if !mediaBridgeMemoryProtectionEnabled(protection) {
		return nil
	}
	if p.probe == nil {
		return mediaBridgeMemoryCapacityError(
			MediaBridgeCapacityReasonMemoryUnavailable,
			input,
			0,
			0,
			"media bridge memory probe is unavailable",
		)
	}

	sample, err := p.probe.SampleMediaBridgeMemory(ctx)
	if err != nil || sample.TotalBytes == 0 {
		description := "media bridge memory sample is unavailable"
		if err != nil {
			description = fmt.Sprintf("%s: %v", description, err)
		}
		return mediaBridgeMemoryCapacityError(
			MediaBridgeCapacityReasonMemoryUnavailable,
			input,
			0,
			0,
			description,
		)
	}

	reservedBytes := mediaBridgeInt64ToUint64(input.Usage.Global.InflightDecodedBytes)
	projectedUsed := mediaBridgeSaturatingAddUint64(sample.UsedBytes, reservedBytes)
	projectedAvailable := uint64(0)
	if sample.AvailableBytes > reservedBytes {
		projectedAvailable = sample.AvailableBytes - reservedBytes
	}
	usedPercent := float64(projectedUsed) / float64(sample.TotalBytes) * 100
	if protection.MemoryHardLimitPercent > 0 && usedPercent >= float64(protection.MemoryHardLimitPercent) {
		return mediaBridgeMemoryCapacityError(
			MediaBridgeCapacityReasonMemoryHard,
			input,
			protection.MemoryHardLimitPercent,
			mediaBridgeRoundedPercent(usedPercent),
			fmt.Sprintf("media bridge memory hard limit reached: %.1f%% used", usedPercent),
		)
	}
	if protection.MinFreeMemoryBytes > 0 && projectedAvailable < uint64(protection.MinFreeMemoryBytes) {
		return mediaBridgeMemoryCapacityError(
			MediaBridgeCapacityReasonMemoryMinFree,
			input,
			protection.MinFreeMemoryBytes,
			mediaBridgeUint64ToInt64(projectedAvailable),
			fmt.Sprintf("media bridge minimum free memory not met: %d projected bytes available", projectedAvailable),
		)
	}
	if protection.MemorySoftLimitPercent > 0 && usedPercent >= float64(protection.MemorySoftLimitPercent) {
		return mediaBridgeMemoryCapacityError(
			MediaBridgeCapacityReasonMemorySoft,
			input,
			protection.MemorySoftLimitPercent,
			mediaBridgeRoundedPercent(usedPercent),
			fmt.Sprintf("media bridge memory soft limit reached: %.1f%% used", usedPercent),
		)
	}
	return nil
}

func mediaBridgeMemoryProtectionEnabled(settings MediaBridgeProtectionSettings) bool {
	return settings.MemorySoftLimitPercent > 0 ||
		settings.MemoryHardLimitPercent > 0 ||
		settings.MinFreeMemoryBytes > 0
}

func mediaBridgeMemoryCapacityError(
	reason MediaBridgeCapacityReason,
	input MediaBridgeCapacityPolicyInput,
	limit int64,
	current int64,
	description string,
) *MediaBridgeCapacityError {
	return &MediaBridgeCapacityError{
		Reason:      reason,
		Scope:       MediaBridgeCapacityScopeGlobal,
		TenantID:    input.TenantID,
		Limit:       limit,
		Current:     current,
		Requested:   input.DecodedBytes,
		RetryAfter:  mediaBridgeMemoryRetryAfter,
		Description: description,
	}
}

func mediaBridgeRoundedPercent(value float64) int64 {
	if value >= float64(math.MaxInt64) {
		return math.MaxInt64
	}
	if value <= 0 {
		return 0
	}
	return int64(math.Round(value))
}

func mediaBridgeUint64ToInt64(value uint64) int64 {
	if value > math.MaxInt64 {
		return math.MaxInt64
	}
	return int64(value)
}

func mediaBridgeInt64ToUint64(value int64) uint64 {
	if value <= 0 {
		return 0
	}
	return uint64(value)
}

func mediaBridgeSaturatingAddUint64(left, right uint64) uint64 {
	if ^uint64(0)-left < right {
		return ^uint64(0)
	}
	return left + right
}
