package admin

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type rolloutLockedMonitorRepository struct {
	service.UpstreamPriceMonitorRepository
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

func TestUpstreamPriceMonitorRunActionsReturnIndependentRolloutLocks(t *testing.T) {
	gin.SetMode(gin.TestMode)
	monitor := service.NewUpstreamPriceMonitorService(&rolloutLockedMonitorRepository{}, nil, nil)
	handler := NewUpstreamPriceMonitorHandler(monitor)
	router := gin.New()
	router.POST("/runs/:id/apply", handler.ApplyRun)
	router.POST("/runs/:id/rollback", handler.RollbackRun)

	for _, tc := range []struct {
		path   string
		reason string
	}{
		{path: "/runs/7/apply", reason: "UPSTREAM_PRICE_APPLY_ROLLOUT_LOCKED"},
		{path: "/runs/7/rollback", reason: "UPSTREAM_PRICE_ROLLBACK_ROLLOUT_LOCKED"},
	} {
		recorder := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodPost, tc.path, strings.NewReader(`{"snapshot_hash":"snapshot"}`))
		request.Header.Set("Content-Type", "application/json")
		router.ServeHTTP(recorder, request)

		require.Equal(t, http.StatusBadRequest, recorder.Code)
		require.Contains(t, recorder.Body.String(), tc.reason)
	}
}
