package service

import (
	"context"
	"encoding/json"
	"sync"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

type mediaBridgeSettingRepo struct {
	SettingRepository
	mu     sync.Mutex
	values map[string]string
}

func (r *mediaBridgeSettingRepo) GetValue(_ context.Context, key string) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	value, ok := r.values[key]
	if !ok {
		return "", ErrSettingNotFound
	}
	return value, nil
}

func (r *mediaBridgeSettingRepo) Set(_ context.Context, key, value string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.values == nil {
		r.values = make(map[string]string)
	}
	r.values[key] = value
	return nil
}

func TestMediaBridgeSettingsDefaultsFailClosed(t *testing.T) {
	svc := NewSettingService(&mediaBridgeSettingRepo{}, &config.Config{})

	settings, err := svc.GetMediaBridgeSettings(context.Background())

	require.NoError(t, err)
	require.Equal(t, MediaBridgeModeOff, settings.Mode)
	require.Equal(t, []string{
		MediaBridgeIngressOpenAIChatCompletions,
		MediaBridgeIngressOpenAIResponses,
	}, settings.Scope.IngressProtocols)
	require.Equal(t, []string{MediaBridgeProtocolOpenAIChatCompletions}, settings.Scope.UpstreamProtocols)
	require.Equal(t, []string{"kimi-k3"}, settings.Scope.Models)
	require.Zero(t, settings.Capacity.MaxInflightRequests)
	require.Zero(t, settings.Protection.MinFreeMemoryBytes)
	require.Zero(t, settings.Protection.R2LatencyThresholdMS)
	require.EqualValues(t, 20, settings.Protection.R2MinimumSamples)
	require.EqualValues(t, 600, settings.Protection.R2UploadTimeoutSeconds)
	require.EqualValues(t, 128*1024*1024, settings.FilePolicy.MaxSingleDecodedBytes)
	require.EqualValues(t, 3600, settings.Retention.SignedURLTTLSeconds)
	require.EqualValues(t, 900, settings.Retention.RequestEndDeleteDelaySeconds)
	require.Equal(t, "r2", settings.Storage.Provider)
}

func TestMediaBridgeSettingsPartialPersistedDocumentKeepsSafeDefaults(t *testing.T) {
	repo := &mediaBridgeSettingRepo{values: map[string]string{
		SettingKeyMediaBridgeSettings: `{"mode":"observe","scope":{"models":[" KIMI-K3 ","kimi-k3"]}}`,
	}}
	svc := NewSettingService(repo, &config.Config{})

	settings, err := svc.GetMediaBridgeSettings(context.Background())

	require.NoError(t, err)
	require.Equal(t, MediaBridgeModeObserve, settings.Mode)
	require.Equal(t, 1, settings.Version)
	require.Equal(t, []string{"kimi-k3"}, settings.Scope.Models)
	require.EqualValues(t, 72, settings.Protection.MemorySoftLimitPercent)
}

func TestMediaBridgeSettingsCanonicalizesEmptyUpstreamScopeToChat(t *testing.T) {
	settings := DefaultMediaBridgeSettings()
	settings.Scope.UpstreamProtocols = nil

	require.NoError(t, normalizeMediaBridgeSettings(settings))
	require.Equal(t, []string{MediaBridgeProtocolOpenAIChatCompletions}, settings.Scope.UpstreamProtocols)
}

func TestMediaBridgeSettingsAcceptsUncappedCapacityAndPublishesImmediately(t *testing.T) {
	repo := &mediaBridgeSettingRepo{}
	svc := NewSettingService(repo, &config.Config{})
	settings := DefaultMediaBridgeSettings()
	settings.Mode = MediaBridgeModeOn
	settings.Scope.UpstreamProtocols = []string{
		MediaBridgeProtocolOpenAIChatCompletions,
		MediaBridgeProtocolOpenAIChatCompletions,
	}
	settings.Scope.AccountIDs = []int64{99, 7, 99}
	settings.Capacity.MaxInflightRequests = 1_000_000
	settings.Capacity.MaxInflightDecodedBytes = 1 << 50
	settings.Capacity.MaxBandwidthBytesPerSecond = 1 << 45
	settings.Capacity.TenantOverrides = []MediaBridgeTenantCapacity{
		{TenantID: 20, Weight: 1000, MaxInflightRequests: 500_000},
		{TenantID: 10, Weight: 2, MaxInflightDecodedBytes: 1 << 48},
	}
	settings.FilePolicy.MaxFilesPerRequest = 1_000_000
	settings.Storage.Endpoint = "https://account.r2.cloudflarestorage.com/"
	settings.Storage.Bucket = "media-private"
	settings.Storage.ObjectPrefix = "/bridge/jobs/"

	require.NoError(t, svc.SetMediaBridgeSettings(context.Background(), settings))
	require.Equal(t, "https://account.r2.cloudflarestorage.com", settings.Storage.Endpoint)
	require.Equal(t, "bridge/jobs/", settings.Storage.ObjectPrefix)
	require.Equal(t, []int64{7, 99}, settings.Scope.AccountIDs)
	require.EqualValues(t, 10, settings.Capacity.TenantOverrides[0].TenantID)

	cached := svc.GetMediaBridgeSettingsCached(context.Background())
	require.Equal(t, MediaBridgeModeOn, cached.Mode)
	require.EqualValues(t, 1_000_000, cached.Capacity.MaxInflightRequests)
	require.EqualValues(t, 1<<50, cached.Capacity.MaxInflightDecodedBytes)
	require.Equal(t, []string{
		MediaBridgeProtocolOpenAIChatCompletions,
	}, cached.Scope.UpstreamProtocols)

	repo.mu.Lock()
	stored := repo.values[SettingKeyMediaBridgeSettings]
	repo.mu.Unlock()
	var persisted MediaBridgeSettings
	require.NoError(t, json.Unmarshal([]byte(stored), &persisted))
	require.EqualValues(t, 1_000_000, persisted.Capacity.MaxInflightRequests)
	require.NotContains(t, stored, "secret_access_key")
}

func TestMediaBridgeSettingsCacheReturnsIsolatedSnapshot(t *testing.T) {
	repo := &mediaBridgeSettingRepo{}
	svc := NewSettingService(repo, &config.Config{})
	settings := DefaultMediaBridgeSettings()
	require.NoError(t, svc.SetMediaBridgeSettings(context.Background(), settings))

	first := svc.GetMediaBridgeSettingsCached(context.Background())
	first.Scope.Models[0] = "mutated"
	first.Scope.IngressProtocols[0] = "mutated"
	first.FilePolicy.AllowedMIMETypes[0] = "video/webm"

	second := svc.GetMediaBridgeSettingsCached(context.Background())
	require.Equal(t, "kimi-k3", second.Scope.Models[0])
	require.Equal(t, MediaBridgeIngressOpenAIChatCompletions, second.Scope.IngressProtocols[0])
	require.Equal(t, "video/mp4", second.FilePolicy.AllowedMIMETypes[0])
}

func TestMediaBridgeSettingsMalformedOrUnsafePersistenceFailsClosed(t *testing.T) {
	tests := []string{
		`{not-json`,
		`{"version":1,"mode":"on","storage":{"provider":"r2","endpoint":"http://unsafe.example","bucket":"media"}}`,
	}
	for _, raw := range tests {
		t.Run(raw, func(t *testing.T) {
			repo := &mediaBridgeSettingRepo{values: map[string]string{SettingKeyMediaBridgeSettings: raw}}
			svc := NewSettingService(repo, &config.Config{})

			settings, err := svc.GetMediaBridgeSettings(context.Background())

			require.NoError(t, err)
			require.Equal(t, MediaBridgeModeOff, settings.Mode)
		})
	}
}

func TestMediaBridgeSettingsRejectsInvalidPolicyWithoutPersisting(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*MediaBridgeSettings)
	}{
		{name: "negative capacity", mutate: func(s *MediaBridgeSettings) { s.Capacity.MaxInflightRequests = -1 }},
		{name: "zero tenant weight", mutate: func(s *MediaBridgeSettings) { s.Capacity.DefaultTenantWeight = 0 }},
		{name: "invalid memory order", mutate: func(s *MediaBridgeSettings) {
			s.Protection.MemorySoftLimitPercent = 90
			s.Protection.MemoryHardLimitPercent = 80
		}},
		{name: "non mp4 mime", mutate: func(s *MediaBridgeSettings) { s.FilePolicy.AllowedMIMETypes = []string{"video/webm"} }},
		{name: "canary without percentage", mutate: func(s *MediaBridgeSettings) { s.Mode = MediaBridgeModeCanary }},
		{name: "retention over seven days", mutate: func(s *MediaBridgeSettings) {
			s.Retention.SignedURLTTLSeconds = 7*24*60*60 + 1
		}},
		{name: "unknown protocol", mutate: func(s *MediaBridgeSettings) { s.Scope.UpstreamProtocols = []string{"unknown"} }},
		{name: "unknown ingress protocol", mutate: func(s *MediaBridgeSettings) { s.Scope.IngressProtocols = []string{"unknown"} }},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			repo := &mediaBridgeSettingRepo{}
			svc := NewSettingService(repo, &config.Config{})
			settings := DefaultMediaBridgeSettings()
			tt.mutate(settings)

			require.Error(t, svc.SetMediaBridgeSettings(context.Background(), settings))
			repo.mu.Lock()
			_, persisted := repo.values[SettingKeyMediaBridgeSettings]
			repo.mu.Unlock()
			require.False(t, persisted)
		})
	}
}
