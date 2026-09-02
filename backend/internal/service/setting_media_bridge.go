package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"sort"
	"strings"
	"time"
)

const SettingKeyMediaBridgeSettings = "media_bridge_settings"

const (
	MediaBridgeModeOff     = "off"
	MediaBridgeModeObserve = "observe"
	MediaBridgeModeCanary  = "canary"
	MediaBridgeModeOn      = "on"
	MediaBridgeModeDrain   = "drain"

	MediaBridgeProtocolOpenAIChatCompletions = "openai_chat_completions"

	MediaBridgeIngressOpenAIChatCompletions = "openai_chat_completions"
	MediaBridgeIngressOpenAIResponses       = "openai_responses"
)

const (
	mediaBridgeSettingsCacheTTL  = 5 * time.Second
	mediaBridgeSettingsErrorTTL  = time.Second
	mediaBridgeSettingsDBTimeout = 3 * time.Second

	mediaBridgeMaxAdmissionWaitMS      = int64(60 * 1000)
	mediaBridgeMaxR2LatencyThresholdMS = int64(5 * 60 * 1000)
	mediaBridgeMaxProtectionWindowSecs = int64(24 * 60 * 60)
	mediaBridgeMaxUploadTimeoutSeconds = int64(60 * 60)
	mediaBridgeMaxRetentionSeconds     = int64(7 * 24 * 60 * 60)
)

// MediaBridgeSettings is the administrator-owned runtime policy for converting
// inline media into temporary object URLs. Storage is retained only as a
// redacted administrator-response snapshot for compatibility. The dedicated
// encrypted media_bridge_storage_settings row is the sole operational source.
type MediaBridgeSettings struct {
	Version       int                           `json:"version"`
	Mode          string                        `json:"mode"`
	CanaryPercent int                           `json:"canary_percent"`
	Scope         MediaBridgeScopeSettings      `json:"scope"`
	Capacity      MediaBridgeCapacitySettings   `json:"capacity"`
	Protection    MediaBridgeProtectionSettings `json:"protection"`
	FilePolicy    MediaBridgeFilePolicySettings `json:"file_policy"`
	Retention     MediaBridgeRetentionSettings  `json:"retention"`
	Storage       MediaBridgeStorageSettings    `json:"storage"`
}

type MediaBridgeScopeSettings struct {
	// An empty IngressProtocols list means no additional restriction.
	// UpstreamProtocols is canonicalized to the fixed K3 Chat egress.
	IngressProtocols  []string `json:"ingress_protocols"`
	UpstreamProtocols []string `json:"upstream_protocols"`
	Models            []string `json:"models"`
	AccountIDs        []int64  `json:"account_ids"`
}

type MediaBridgeCapacitySettings struct {
	// Zero means no explicit concurrency/byte ceiling or no bandwidth throttle.
	// Memory and R2 protection remain active without a business hard cap.
	MaxInflightRequests        int64                       `json:"max_inflight_requests"`
	MaxInflightDecodedBytes    int64                       `json:"max_inflight_decoded_bytes"`
	MaxBandwidthBytesPerSecond int64                       `json:"max_bandwidth_bytes_per_second"`
	BurstBytes                 int64                       `json:"burst_bytes"`
	AdmissionWaitMS            int64                       `json:"admission_wait_ms"`
	DefaultTenantWeight        int64                       `json:"default_tenant_weight"`
	TenantOverrides            []MediaBridgeTenantCapacity `json:"tenant_overrides"`
}

type MediaBridgeTenantCapacity struct {
	TenantID                   int64 `json:"tenant_id"`
	Weight                     int64 `json:"weight"`
	MaxInflightRequests        int64 `json:"max_inflight_requests"`
	MaxInflightDecodedBytes    int64 `json:"max_inflight_decoded_bytes"`
	MaxBandwidthBytesPerSecond int64 `json:"max_bandwidth_bytes_per_second"`
}

type MediaBridgeProtectionSettings struct {
	// Zero disables the corresponding threshold.
	MemorySoftLimitPercent      int64 `json:"memory_soft_limit_percent"`
	MemoryHardLimitPercent      int64 `json:"memory_hard_limit_percent"`
	MinFreeMemoryBytes          int64 `json:"min_free_memory_bytes"`
	R2ErrorRateThresholdPercent int64 `json:"r2_error_rate_threshold_percent"`
	R2LatencyThresholdMS        int64 `json:"r2_latency_threshold_ms"`
	R2WindowSeconds             int64 `json:"r2_window_seconds"`
	R2OpenSeconds               int64 `json:"r2_open_seconds"`
	R2HalfOpenProbes            int64 `json:"r2_half_open_probes"`
	R2MinimumSamples            int64 `json:"r2_minimum_samples"`
	R2UploadTimeoutSeconds      int64 `json:"r2_upload_timeout_seconds"`
}

type MediaBridgeFilePolicySettings struct {
	AllowedMIMETypes []string `json:"allowed_mime_types"`
	// Zero means unlimited at this policy layer; the HTTP body cap still applies.
	MaxFilesPerRequest       int64 `json:"max_files_per_request"`
	MaxSingleDecodedBytes    int64 `json:"max_single_decoded_bytes"`
	MaxRequestDecodedBytes   int64 `json:"max_request_decoded_bytes"`
	DeduplicateWithinRequest bool  `json:"deduplicate_within_request"`
}

type MediaBridgeRetentionSettings struct {
	SignedURLTTLSeconds          int64 `json:"signed_url_ttl_seconds"`
	RequestEndDeleteDelaySeconds int64 `json:"request_end_delete_delay_seconds"`
}

type MediaBridgeStorageSettings struct {
	Provider         string `json:"provider"`
	Endpoint         string `json:"endpoint"`
	Region           string `json:"region"`
	Bucket           string `json:"bucket"`
	ObjectPrefix     string `json:"object_prefix"`
	AccessKeyID      string `json:"access_key_id"`
	ForcePathStyle   bool   `json:"force_path_style"`
	SecretConfigured bool   `json:"secret_configured"`
	Ready            bool   `json:"ready"`
}

type cachedMediaBridgeSettings struct {
	settings  MediaBridgeSettings
	expiresAt int64
}

func DefaultMediaBridgeSettings() *MediaBridgeSettings {
	return &MediaBridgeSettings{
		Version:       1,
		Mode:          MediaBridgeModeOff,
		CanaryPercent: 0,
		Scope: MediaBridgeScopeSettings{
			IngressProtocols: []string{
				MediaBridgeIngressOpenAIChatCompletions,
				MediaBridgeIngressOpenAIResponses,
			},
			UpstreamProtocols: []string{MediaBridgeProtocolOpenAIChatCompletions},
			Models:            []string{"kimi-k3"},
			AccountIDs:        []int64{},
		},
		Capacity: MediaBridgeCapacitySettings{
			MaxInflightRequests:        0,
			MaxInflightDecodedBytes:    0,
			MaxBandwidthBytesPerSecond: 0,
			BurstBytes:                 0,
			AdmissionWaitMS:            200,
			DefaultTenantWeight:        10,
			TenantOverrides:            []MediaBridgeTenantCapacity{},
		},
		Protection: MediaBridgeProtectionSettings{
			MemorySoftLimitPercent:      72,
			MemoryHardLimitPercent:      82,
			MinFreeMemoryBytes:          0,
			R2ErrorRateThresholdPercent: 5,
			R2LatencyThresholdMS:        0,
			R2WindowSeconds:             60,
			R2OpenSeconds:               30,
			R2HalfOpenProbes:            2,
			R2MinimumSamples:            20,
			R2UploadTimeoutSeconds:      10 * 60,
		},
		FilePolicy: MediaBridgeFilePolicySettings{
			AllowedMIMETypes:         []string{"video/mp4"},
			MaxFilesPerRequest:       4,
			MaxSingleDecodedBytes:    128 * 1024 * 1024,
			MaxRequestDecodedBytes:   0,
			DeduplicateWithinRequest: true,
		},
		Retention: MediaBridgeRetentionSettings{
			SignedURLTTLSeconds:          60 * 60,
			RequestEndDeleteDelaySeconds: 15 * 60,
		},
		Storage: MediaBridgeStorageSettings{
			Provider:       "r2",
			Endpoint:       "",
			Region:         "auto",
			Bucket:         "",
			ObjectPrefix:   "media-bridge/",
			ForcePathStyle: true,
		},
	}
}

func normalizeMediaBridgeSettings(settings *MediaBridgeSettings) error {
	if settings == nil {
		return fmt.Errorf("settings cannot be nil")
	}
	if settings.Version != 1 {
		return fmt.Errorf("version must be 1")
	}

	settings.Mode = strings.ToLower(strings.TrimSpace(settings.Mode))
	switch settings.Mode {
	case MediaBridgeModeOff, MediaBridgeModeObserve, MediaBridgeModeCanary, MediaBridgeModeOn, MediaBridgeModeDrain:
	default:
		return fmt.Errorf("mode must be one of off, observe, canary, on, drain")
	}
	if settings.CanaryPercent < 0 || settings.CanaryPercent > 100 {
		return fmt.Errorf("canary_percent must be between 0 and 100")
	}
	if settings.Mode == MediaBridgeModeCanary && settings.CanaryPercent == 0 {
		return fmt.Errorf("canary_percent must be greater than 0 in canary mode")
	}

	ingressProtocols, err := normalizeMediaBridgeIngressProtocols(settings.Scope.IngressProtocols)
	if err != nil {
		return err
	}
	settings.Scope.IngressProtocols = ingressProtocols
	protocols, err := normalizeMediaBridgeProtocols(settings.Scope.UpstreamProtocols)
	if err != nil {
		return err
	}
	settings.Scope.UpstreamProtocols = protocols
	settings.Scope.Models = normalizeUniqueStrings(settings.Scope.Models, true)
	settings.Scope.AccountIDs, err = normalizePositiveIDs(settings.Scope.AccountIDs, "account_ids")
	if err != nil {
		return err
	}

	if err := validateMediaBridgeCapacity(&settings.Capacity); err != nil {
		return err
	}
	if err := validateMediaBridgeProtection(settings.Protection); err != nil {
		return err
	}
	if err := normalizeMediaBridgeFilePolicy(&settings.FilePolicy); err != nil {
		return err
	}
	if err := validateMediaBridgeRetention(settings.Retention); err != nil {
		return err
	}
	if err := normalizeMediaBridgeStorage(&settings.Storage); err != nil {
		return err
	}
	return nil
}

func normalizeMediaBridgeIngressProtocols(values []string) ([]string, error) {
	values = normalizeUniqueStrings(values, true)
	for _, value := range values {
		switch value {
		case MediaBridgeIngressOpenAIChatCompletions,
			MediaBridgeIngressOpenAIResponses:
		default:
			return nil, fmt.Errorf("unsupported ingress protocol %q", value)
		}
	}
	return values, nil
}

// ValidateMediaBridgeSettings validates a copy without mutating the caller.
// The administrator handler uses this before checking the independent storage
// runtime so a rejected policy cannot partially apply a storage-side change.
func ValidateMediaBridgeSettings(settings *MediaBridgeSettings) error {
	if settings == nil {
		return fmt.Errorf("settings cannot be nil")
	}
	validated := cloneMediaBridgeSettings(*settings)
	return normalizeMediaBridgeSettings(&validated)
}

func normalizeMediaBridgeProtocols(values []string) ([]string, error) {
	values = normalizeUniqueStrings(values, true)
	if len(values) == 0 {
		return []string{MediaBridgeProtocolOpenAIChatCompletions}, nil
	}
	for _, value := range values {
		switch value {
		case MediaBridgeProtocolOpenAIChatCompletions:
		default:
			return nil, fmt.Errorf("unsupported upstream protocol %q", value)
		}
	}
	return values, nil
}

func normalizeUniqueStrings(values []string, lower bool) []string {
	result := make([]string, 0, len(values))
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if lower {
			value = strings.ToLower(value)
		}
		if value == "" {
			continue
		}
		key := strings.ToLower(value)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		result = append(result, value)
	}
	if result == nil {
		return []string{}
	}
	return result
}

func normalizePositiveIDs(values []int64, field string) ([]int64, error) {
	result := make([]int64, 0, len(values))
	seen := make(map[int64]struct{}, len(values))
	for _, value := range values {
		if value <= 0 {
			return nil, fmt.Errorf("%s values must be positive", field)
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		result = append(result, value)
	}
	sort.Slice(result, func(i, j int) bool { return result[i] < result[j] })
	if result == nil {
		return []int64{}, nil
	}
	return result, nil
}

func validateMediaBridgeCapacity(settings *MediaBridgeCapacitySettings) error {
	if settings.MaxInflightRequests < 0 || settings.MaxInflightDecodedBytes < 0 ||
		settings.MaxBandwidthBytesPerSecond < 0 || settings.BurstBytes < 0 || settings.AdmissionWaitMS < 0 {
		return fmt.Errorf("capacity limits cannot be negative")
	}
	if settings.AdmissionWaitMS > mediaBridgeMaxAdmissionWaitMS {
		return fmt.Errorf("capacity.admission_wait_ms cannot exceed %d", mediaBridgeMaxAdmissionWaitMS)
	}
	if settings.DefaultTenantWeight <= 0 {
		return fmt.Errorf("capacity.default_tenant_weight must be positive")
	}

	seen := make(map[int64]struct{}, len(settings.TenantOverrides))
	for i := range settings.TenantOverrides {
		override := &settings.TenantOverrides[i]
		if override.TenantID <= 0 {
			return fmt.Errorf("capacity.tenant_overrides[%d].tenant_id must be positive", i)
		}
		if _, ok := seen[override.TenantID]; ok {
			return fmt.Errorf("capacity.tenant_overrides contains duplicate tenant_id %d", override.TenantID)
		}
		seen[override.TenantID] = struct{}{}
		if override.Weight <= 0 {
			return fmt.Errorf("capacity.tenant_overrides[%d].weight must be positive", i)
		}
		if override.MaxInflightRequests < 0 || override.MaxInflightDecodedBytes < 0 || override.MaxBandwidthBytesPerSecond < 0 {
			return fmt.Errorf("capacity.tenant_overrides[%d] limits cannot be negative", i)
		}
	}
	sort.Slice(settings.TenantOverrides, func(i, j int) bool {
		return settings.TenantOverrides[i].TenantID < settings.TenantOverrides[j].TenantID
	})
	if settings.TenantOverrides == nil {
		settings.TenantOverrides = []MediaBridgeTenantCapacity{}
	}
	return nil
}

func validateMediaBridgeProtection(settings MediaBridgeProtectionSettings) error {
	values := []int64{
		settings.MemorySoftLimitPercent,
		settings.MemoryHardLimitPercent,
		settings.MinFreeMemoryBytes,
		settings.R2ErrorRateThresholdPercent,
		settings.R2LatencyThresholdMS,
		settings.R2WindowSeconds,
		settings.R2OpenSeconds,
		settings.R2HalfOpenProbes,
		settings.R2MinimumSamples,
		settings.R2UploadTimeoutSeconds,
	}
	for _, value := range values {
		if value < 0 {
			return fmt.Errorf("protection thresholds cannot be negative")
		}
	}
	if settings.MemorySoftLimitPercent > 100 || settings.MemoryHardLimitPercent > 100 {
		return fmt.Errorf("memory limit percentages must be between 0 and 100")
	}
	if settings.MemorySoftLimitPercent > 0 && settings.MemoryHardLimitPercent > 0 &&
		settings.MemorySoftLimitPercent >= settings.MemoryHardLimitPercent {
		return fmt.Errorf("memory_soft_limit_percent must be lower than memory_hard_limit_percent")
	}
	if settings.R2ErrorRateThresholdPercent > 100 {
		return fmt.Errorf("r2_error_rate_threshold_percent must be between 0 and 100")
	}
	if settings.R2LatencyThresholdMS > mediaBridgeMaxR2LatencyThresholdMS {
		return fmt.Errorf("r2_latency_threshold_ms cannot exceed %d", mediaBridgeMaxR2LatencyThresholdMS)
	}
	if settings.R2UploadTimeoutSeconds <= 0 || settings.R2UploadTimeoutSeconds > mediaBridgeMaxUploadTimeoutSeconds {
		return fmt.Errorf("r2_upload_timeout_seconds must be between 1 and %d", mediaBridgeMaxUploadTimeoutSeconds)
	}
	if settings.R2WindowSeconds > mediaBridgeMaxProtectionWindowSecs || settings.R2OpenSeconds > mediaBridgeMaxProtectionWindowSecs {
		return fmt.Errorf("r2 window/open seconds cannot exceed %d", mediaBridgeMaxProtectionWindowSecs)
	}
	return nil
}

func normalizeMediaBridgeFilePolicy(settings *MediaBridgeFilePolicySettings) error {
	if settings.MaxFilesPerRequest < 0 || settings.MaxSingleDecodedBytes < 0 || settings.MaxRequestDecodedBytes < 0 {
		return fmt.Errorf("file policy limits cannot be negative")
	}
	mimeTypes := normalizeUniqueStrings(settings.AllowedMIMETypes, true)
	for _, value := range mimeTypes {
		if value != "video/mp4" {
			return fmt.Errorf("allowed_mime_types currently supports only video/mp4")
		}
	}
	settings.AllowedMIMETypes = mimeTypes
	return nil
}

func validateMediaBridgeRetention(settings MediaBridgeRetentionSettings) error {
	if settings.SignedURLTTLSeconds <= 0 {
		return fmt.Errorf("retention.signed_url_ttl_seconds must be positive")
	}
	if settings.RequestEndDeleteDelaySeconds < 0 {
		return fmt.Errorf("retention.request_end_delete_delay_seconds cannot be negative")
	}
	if settings.SignedURLTTLSeconds > mediaBridgeMaxRetentionSeconds ||
		settings.RequestEndDeleteDelaySeconds > mediaBridgeMaxRetentionSeconds {
		return fmt.Errorf("retention seconds cannot exceed %d", mediaBridgeMaxRetentionSeconds)
	}
	return nil
}

func normalizeMediaBridgeStorage(settings *MediaBridgeStorageSettings) error {
	settings.Provider = strings.ToLower(strings.TrimSpace(settings.Provider))
	if settings.Provider == "" {
		settings.Provider = "r2"
	}
	if settings.Provider != "r2" {
		return fmt.Errorf("storage.provider must be r2")
	}
	settings.Endpoint = strings.TrimSpace(settings.Endpoint)
	if settings.Endpoint != "" {
		parsed, err := url.Parse(settings.Endpoint)
		if err != nil || validateTemporaryMediaEndpoint(settings.Endpoint) != nil ||
			(parsed.Path != "" && parsed.Path != "/") {
			return fmt.Errorf("storage.endpoint must be an HTTPS origin without credentials, query, or fragment")
		}
		settings.Endpoint = strings.TrimRight(settings.Endpoint, "/")
	}
	settings.Region = strings.TrimSpace(settings.Region)
	if settings.Region == "" {
		settings.Region = "auto"
	}
	settings.Bucket = strings.TrimSpace(settings.Bucket)
	if settings.Bucket != "" && !validTemporaryMediaBucket(settings.Bucket) {
		return fmt.Errorf("storage.bucket contains invalid characters")
	}
	settings.AccessKeyID = strings.TrimSpace(settings.AccessKeyID)
	if len(settings.AccessKeyID) > 1024 || strings.ContainsAny(settings.AccessKeyID, "\r\n\x00") {
		return fmt.Errorf("storage.access_key_id is invalid")
	}
	settings.ObjectPrefix = strings.Trim(strings.TrimSpace(settings.ObjectPrefix), "/")
	if settings.ObjectPrefix != "" {
		if !validTemporaryMediaPrefix(settings.ObjectPrefix) {
			return fmt.Errorf("storage.object_prefix contains an invalid path segment")
		}
		settings.ObjectPrefix += "/"
	}
	return nil
}

// GetMediaBridgeSettings reads the persisted policy for administrator APIs.
// Missing or malformed values fail closed to the disabled defaults.
func (s *SettingService) GetMediaBridgeSettings(ctx context.Context) (*MediaBridgeSettings, error) {
	defaults := DefaultMediaBridgeSettings()
	if s == nil || s.settingRepo == nil {
		return defaults, nil
	}
	raw, err := s.settingRepo.GetValue(ctx, SettingKeyMediaBridgeSettings)
	if err != nil {
		if errors.Is(err, ErrSettingNotFound) {
			return defaults, nil
		}
		return nil, fmt.Errorf("get media bridge settings: %w", err)
	}
	if strings.TrimSpace(raw) == "" {
		return defaults, nil
	}

	settings := cloneMediaBridgeSettings(*defaults)
	if err := json.Unmarshal([]byte(raw), &settings); err != nil {
		slog.Warn("invalid media bridge settings, falling back to disabled defaults", "error", err)
		return defaults, nil
	}
	if err := normalizeMediaBridgeSettings(&settings); err != nil {
		slog.Warn("unsafe media bridge settings, falling back to disabled defaults", "error", err)
		return defaults, nil
	}
	return &settings, nil
}

// SetMediaBridgeSettings validates and atomically publishes the runtime policy.
func (s *SettingService) SetMediaBridgeSettings(ctx context.Context, settings *MediaBridgeSettings) error {
	if s == nil || s.settingRepo == nil {
		return fmt.Errorf("setting service is unavailable")
	}
	if settings == nil {
		return fmt.Errorf("settings cannot be nil")
	}
	normalized := cloneMediaBridgeSettings(*settings)
	if err := normalizeMediaBridgeSettings(&normalized); err != nil {
		return err
	}
	raw, err := json.Marshal(&normalized)
	if err != nil {
		return fmt.Errorf("marshal media bridge settings: %w", err)
	}
	if err := s.settingRepo.Set(ctx, SettingKeyMediaBridgeSettings, string(raw)); err != nil {
		return fmt.Errorf("save media bridge settings: %w", err)
	}
	*settings = cloneMediaBridgeSettings(normalized)
	s.storeMediaBridgeSettings(normalized, mediaBridgeSettingsCacheTTL)
	return nil
}

// GetMediaBridgeSettingsCached returns a hot-path snapshot. Writes are visible
// immediately in the current process and within five seconds on peer nodes.
func (s *SettingService) GetMediaBridgeSettingsCached(ctx context.Context) MediaBridgeSettings {
	if s == nil || s.settingRepo == nil {
		return cloneMediaBridgeSettings(*DefaultMediaBridgeSettings())
	}
	if cached, ok := s.mediaBridgeSettingsCache.Load().(*cachedMediaBridgeSettings); ok && cached != nil && time.Now().UnixNano() < cached.expiresAt {
		return cloneMediaBridgeSettings(cached.settings)
	}

	result, _, _ := s.mediaBridgeSettingsSF.Do(SettingKeyMediaBridgeSettings, func() (any, error) {
		if cached, ok := s.mediaBridgeSettingsCache.Load().(*cachedMediaBridgeSettings); ok && cached != nil && time.Now().UnixNano() < cached.expiresAt {
			return cloneMediaBridgeSettings(cached.settings), nil
		}
		if ctx == nil {
			ctx = context.Background()
		}
		dbCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), mediaBridgeSettingsDBTimeout)
		defer cancel()

		settings, err := s.GetMediaBridgeSettings(dbCtx)
		if err != nil {
			fallback := cloneMediaBridgeSettings(*DefaultMediaBridgeSettings())
			if prior, ok := s.mediaBridgeSettingsCache.Load().(*cachedMediaBridgeSettings); ok && prior != nil {
				fallback = cloneMediaBridgeSettings(prior.settings)
			}
			s.storeMediaBridgeSettings(fallback, mediaBridgeSettingsErrorTTL)
			slog.Warn("failed to refresh media bridge settings", "error", err)
			return fallback, nil
		}
		s.storeMediaBridgeSettings(*settings, mediaBridgeSettingsCacheTTL)
		return cloneMediaBridgeSettings(*settings), nil
	})
	if settings, ok := result.(MediaBridgeSettings); ok {
		return cloneMediaBridgeSettings(settings)
	}
	return cloneMediaBridgeSettings(*DefaultMediaBridgeSettings())
}

func (s *SettingService) storeMediaBridgeSettings(settings MediaBridgeSettings, ttl time.Duration) {
	if s == nil {
		return
	}
	s.mediaBridgeSettingsCache.Store(&cachedMediaBridgeSettings{
		settings:  cloneMediaBridgeSettings(settings),
		expiresAt: time.Now().Add(ttl).UnixNano(),
	})
}

func cloneMediaBridgeSettings(settings MediaBridgeSettings) MediaBridgeSettings {
	settings.Scope.IngressProtocols = append([]string(nil), settings.Scope.IngressProtocols...)
	settings.Scope.UpstreamProtocols = append([]string(nil), settings.Scope.UpstreamProtocols...)
	settings.Scope.Models = append([]string(nil), settings.Scope.Models...)
	settings.Scope.AccountIDs = append([]int64(nil), settings.Scope.AccountIDs...)
	settings.Capacity.TenantOverrides = append([]MediaBridgeTenantCapacity(nil), settings.Capacity.TenantOverrides...)
	settings.FilePolicy.AllowedMIMETypes = append([]string(nil), settings.FilePolicy.AllowedMIMETypes...)
	return settings
}
