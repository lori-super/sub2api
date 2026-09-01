package admin

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/domain"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type runActionMonitorRepository struct {
	service.UpstreamPriceMonitorRepository
}

func (r *runActionMonitorRepository) GetConfig(context.Context) (*domain.UpstreamPriceMonitorConfig, error) {
	cfg := domain.DefaultUpstreamPriceMonitorConfig()
	cfg.Mode = domain.UpstreamPriceMonitorModeReview
	return &cfg, nil
}

func (r *runActionMonitorRepository) GetRun(context.Context, int64) (*domain.UpstreamPriceMonitorRun, error) {
	return nil, service.ErrUpstreamPriceRunNotApplicable
}

func (r *runActionMonitorRepository) RollbackRun(context.Context, int64, string) error {
	return service.ErrUpstreamPriceRunNotApplicable
}

func TestUpstreamPriceMonitorManualRunRejectsNonDryRun(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	handler := NewUpstreamPriceMonitorHandler(nil)
	router.POST("/runs", handler.CreateRun)

	for _, tc := range []struct {
		body        string
		wantMessage string
	}{
		{body: `{"dry_run":false}`, wantMessage: "dry_run"},
		{body: `{}`},
	} {
		recorder := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodPost, "/runs", strings.NewReader(tc.body))
		request.Header.Set("Content-Type", "application/json")
		router.ServeHTTP(recorder, request)

		require.Equal(t, http.StatusBadRequest, recorder.Code)
		if tc.wantMessage != "" {
			require.Contains(t, recorder.Body.String(), tc.wantMessage)
		}
	}
}

func TestUpstreamPriceMonitorRunActionsReachTheService(t *testing.T) {
	gin.SetMode(gin.TestMode)
	monitor := service.NewUpstreamPriceMonitorService(&runActionMonitorRepository{}, nil, nil)
	handler := NewUpstreamPriceMonitorHandler(monitor)
	router := gin.New()
	router.POST("/runs/:id/apply", handler.ApplyRun)
	router.POST("/runs/:id/rollback", handler.RollbackRun)

	for _, tc := range []struct {
		path string
	}{
		{path: "/runs/7/apply"},
		{path: "/runs/7/rollback"},
	} {
		recorder := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodPost, tc.path, strings.NewReader(`{"snapshot_hash":"snapshot"}`))
		request.Header.Set("Content-Type", "application/json")
		router.ServeHTTP(recorder, request)

		require.Equal(t, http.StatusConflict, recorder.Code)
		require.Contains(t, recorder.Body.String(), "UPSTREAM_PRICE_RUN_NOT_APPLICABLE")
		require.NotContains(t, recorder.Body.String(), "ROLLOUT_LOCKED")
	}
}
