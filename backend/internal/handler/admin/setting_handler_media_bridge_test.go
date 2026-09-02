package admin

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type mediaBridgeHandlerRepo struct {
	service.SettingRepository
	values map[string]string
}

func (r *mediaBridgeHandlerRepo) GetValue(_ context.Context, key string) (string, error) {
	value, ok := r.values[key]
	if !ok {
		return "", service.ErrSettingNotFound
	}
	return value, nil
}

func (r *mediaBridgeHandlerRepo) Set(_ context.Context, key, value string) error {
	if r.values == nil {
		r.values = make(map[string]string)
	}
	r.values[key] = value
	return nil
}

func newMediaBridgeSettingHandler(repo service.SettingRepository) *SettingHandler {
	svc := service.NewSettingService(repo, &config.Config{})
	return NewSettingHandler(svc, nil, nil, nil, nil, nil, nil)
}

func TestMediaBridgeSettingHandlerGetDefaults(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodGet, "/api/v1/admin/settings/media-bridge", nil)
	h := newMediaBridgeSettingHandler(&mediaBridgeHandlerRepo{})

	h.GetMediaBridgeSettings(c)

	require.Equal(t, http.StatusOK, recorder.Code)
	var payload struct {
		Data service.MediaBridgeSettings `json:"data"`
	}
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &payload))
	require.Equal(t, service.MediaBridgeModeOff, payload.Data.Mode)
	require.Equal(t, []string{"kimi-k3"}, payload.Data.Scope.Models)
	require.EqualValues(t, 10, payload.Data.Capacity.DefaultTenantWeight)
	require.Equal(t, "r2", payload.Data.Storage.Provider)
}

func TestMediaBridgeSettingHandlerPartialUpdatePreservesPolicy(t *testing.T) {
	gin.SetMode(gin.TestMode)
	repo := &mediaBridgeHandlerRepo{}
	h := newMediaBridgeSettingHandler(repo)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(
		http.MethodPut,
		"/api/v1/admin/settings/media-bridge",
		bytes.NewBufferString(`{"mode":"observe","capacity":{"max_inflight_requests":1000000}}`),
	)
	c.Request.Header.Set("Content-Type", "application/json")

	h.UpdateMediaBridgeSettings(c)

	require.Equal(t, http.StatusOK, recorder.Code)
	var stored service.MediaBridgeSettings
	require.NoError(t, json.Unmarshal([]byte(repo.values[service.SettingKeyMediaBridgeSettings]), &stored))
	require.Equal(t, service.MediaBridgeModeObserve, stored.Mode)
	require.EqualValues(t, 1_000_000, stored.Capacity.MaxInflightRequests)
	require.EqualValues(t, 10, stored.Capacity.DefaultTenantWeight)
	require.EqualValues(t, 128*1024*1024, stored.FilePolicy.MaxSingleDecodedBytes)
}

func TestMediaBridgeSettingHandlerRejectsWriteOnlySecretOnPolicyEndpoint(t *testing.T) {
	gin.SetMode(gin.TestMode)
	repo := &mediaBridgeHandlerRepo{}
	h := newMediaBridgeSettingHandler(repo)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(
		http.MethodPut,
		"/api/v1/admin/settings/media-bridge",
		bytes.NewBufferString(`{"storage":{"provider":"r2","secret_access_key":"secret-value"}}`),
	)
	c.Request.Header.Set("Content-Type", "application/json")

	h.UpdateMediaBridgeSettings(c)

	require.Equal(t, http.StatusBadRequest, recorder.Code)
	_, persisted := repo.values[service.SettingKeyMediaBridgeSettings]
	require.False(t, persisted)
	require.Contains(t, recorder.Body.String(), "unknown field")
}

func TestMediaBridgeSettingHandlerRejectsUnsafeEnable(t *testing.T) {
	gin.SetMode(gin.TestMode)
	repo := &mediaBridgeHandlerRepo{}
	h := newMediaBridgeSettingHandler(repo)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(
		http.MethodPut,
		"/api/v1/admin/settings/media-bridge",
		bytes.NewBufferString(`{"mode":"on"}`),
	)
	c.Request.Header.Set("Content-Type", "application/json")

	h.UpdateMediaBridgeSettings(c)

	require.Equal(t, http.StatusBadRequest, recorder.Code)
	_, persisted := repo.values[service.SettingKeyMediaBridgeSettings]
	require.False(t, persisted)
	require.Contains(t, recorder.Body.String(), "storage is not configured")
}

func TestMediaBridgeSettingHandlerIgnoresReadOnlyStorageSnapshot(t *testing.T) {
	gin.SetMode(gin.TestMode)
	repo := &mediaBridgeHandlerRepo{}
	h := newMediaBridgeSettingHandler(repo)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(
		http.MethodPut,
		"/api/v1/admin/settings/media-bridge",
		bytes.NewBufferString(`{"mode":"observe","storage":{"provider":"r2","endpoint":"https://account.r2.cloudflarestorage.com","region":"auto","bucket":"media-private","object_prefix":"bridge/","access_key_id":"public-id","force_path_style":true,"secret_configured":true,"ready":true}}`),
	)
	c.Request.Header.Set("Content-Type", "application/json")

	h.UpdateMediaBridgeSettings(c)

	require.Equal(t, http.StatusOK, recorder.Code)
	var stored service.MediaBridgeSettings
	require.NoError(t, json.Unmarshal([]byte(repo.values[service.SettingKeyMediaBridgeSettings]), &stored))
	require.Equal(t, service.MediaBridgeModeObserve, stored.Mode)
	require.Empty(t, stored.Storage.Endpoint)
	require.Empty(t, stored.Storage.Bucket)
	require.Empty(t, stored.Storage.AccessKeyID)
	require.False(t, stored.Storage.SecretConfigured)
	require.False(t, stored.Storage.Ready)
}
