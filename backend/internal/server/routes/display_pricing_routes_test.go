package routes

import (
	"net/http"
	"net/http/httptest"
	"testing"

	basehandler "github.com/Wei-Shaw/sub2api/internal/handler"
	adminhandler "github.com/Wei-Shaw/sub2api/internal/handler/admin"
	servermiddleware "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestRegisterDisplayPricingRoutesIncludesProviderCRUD(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	admin := router.Group("/api/v1/admin")
	handlers := &basehandler.Handlers{Admin: &basehandler.AdminHandlers{
		DisplayPricing:       adminhandler.NewDisplayPricingHandler(nil, nil),
		UpstreamPriceMonitor: adminhandler.NewUpstreamPriceMonitorHandler(nil),
	}}
	registerDisplayPricingRoutes(admin, handlers, servermiddleware.StepUpAuthMiddleware(func(c *gin.Context) { c.Next() }))

	want := map[string]bool{
		"POST /api/v1/admin/display-pricing/providers":             false,
		"PUT /api/v1/admin/display-pricing/providers/:provider":    false,
		"DELETE /api/v1/admin/display-pricing/providers/:provider": false,
		"POST /api/v1/admin/display-pricing/official-sync/preview": false,
		"POST /api/v1/admin/display-pricing/official-sync/apply":   false,
		"POST /api/v1/admin/display-pricing/upstream-token-sync":   false,
	}
	for _, route := range router.Routes() {
		key := route.Method + " " + route.Path
		if _, ok := want[key]; ok {
			want[key] = true
		}
	}
	for route, registered := range want {
		require.True(t, registered, route)
	}
}

func TestAuthoritativeUpstreamTokenSyncRequiresStepUp(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	handlers := &basehandler.Handlers{Admin: &basehandler.AdminHandlers{
		DisplayPricing:       adminhandler.NewDisplayPricingHandler(nil, nil),
		UpstreamPriceMonitor: adminhandler.NewUpstreamPriceMonitorHandler(nil),
	}}
	stepUpCalls := 0
	stepUp := servermiddleware.StepUpAuthMiddleware(func(c *gin.Context) {
		stepUpCalls++
		c.AbortWithStatus(http.StatusPreconditionRequired)
	})
	registerDisplayPricingRoutes(router.Group("/api/v1/admin"), handlers, stepUp)

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/api/v1/admin/display-pricing/upstream-token-sync", nil)
	router.ServeHTTP(recorder, request)
	require.Equal(t, http.StatusPreconditionRequired, recorder.Code)
	require.Equal(t, 1, stepUpCalls)
}
