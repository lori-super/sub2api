package routes

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	basehandler "github.com/Wei-Shaw/sub2api/internal/handler"
	adminhandler "github.com/Wei-Shaw/sub2api/internal/handler/admin"
	servermiddleware "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestUpstreamPriceMonitorSensitiveRoutesRequireStepUp(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	handlers := &basehandler.Handlers{Admin: &basehandler.AdminHandlers{
		UpstreamPriceMonitor: adminhandler.NewUpstreamPriceMonitorHandler(nil),
	}}
	stepUpCalls := 0
	stepUp := servermiddleware.StepUpAuthMiddleware(func(c *gin.Context) {
		stepUpCalls++
		c.AbortWithStatus(http.StatusPreconditionRequired)
	})
	registerUpstreamPriceMonitorRoutes(router.Group("/api/v1/admin"), handlers, stepUp)

	for _, tc := range []struct {
		method string
		path   string
		body   string
	}{
		{method: http.MethodPut, path: "/api/v1/admin/upstream-price-monitor/config", body: `{}`},
		{method: http.MethodPost, path: "/api/v1/admin/upstream-price-monitor/runs/7/apply", body: `{"snapshot_hash":"hash"}`},
		{method: http.MethodPost, path: "/api/v1/admin/upstream-price-monitor/runs/7/rollback", body: `{"snapshot_hash":"hash"}`},
	} {
		t.Run(tc.method+" "+tc.path, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			request := httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body))
			request.Header.Set("Content-Type", "application/json")
			router.ServeHTTP(recorder, request)
			require.Equal(t, http.StatusPreconditionRequired, recorder.Code)
		})
	}

	require.Equal(t, 3, stepUpCalls)
}
