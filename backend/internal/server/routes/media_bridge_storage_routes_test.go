package routes

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/handler"
	adminhandler "github.com/Wei-Shaw/sub2api/internal/handler/admin"
	servermiddleware "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestMediaBridgeStorageRoutesRequireStepUp(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	handlers := &handler.Handlers{Admin: &handler.AdminHandlers{Setting: &adminhandler.SettingHandler{}}}
	stepUpCalls := 0
	stepUp := servermiddleware.StepUpAuthMiddleware(func(c *gin.Context) {
		stepUpCalls++
		c.AbortWithStatus(http.StatusTeapot)
	})
	registerSettingsRoutes(router.Group("/api/v1/admin"), handlers, stepUp)

	for _, route := range []struct {
		method string
		path   string
	}{
		{method: http.MethodPut, path: "/api/v1/admin/settings/media-bridge/storage"},
		{method: http.MethodPost, path: "/api/v1/admin/settings/media-bridge/storage/test"},
	} {
		recorder := httptest.NewRecorder()
		request := httptest.NewRequest(route.method, route.path, nil)
		router.ServeHTTP(recorder, request)
		require.Equal(t, http.StatusTeapot, recorder.Code)
	}
	require.Equal(t, 2, stepUpCalls)
}

func TestMediaBridgeStorageHandlersRejectAdminAPIKeyEvenWhenRouteGatePasses(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	handlers := &handler.Handlers{Admin: &handler.AdminHandlers{Setting: &adminhandler.SettingHandler{}}}
	stepUp := servermiddleware.StepUpAuthMiddleware(func(c *gin.Context) {
		// Models the production-wide optional gate when step_up_enabled=false.
		c.Set("auth_method", "admin_api_key")
		c.Next()
	})
	registerSettingsRoutes(router.Group("/api/v1/admin"), handlers, stepUp)

	for _, route := range []struct {
		method string
		path   string
	}{
		{method: http.MethodPut, path: "/api/v1/admin/settings/media-bridge/storage"},
		{method: http.MethodPost, path: "/api/v1/admin/settings/media-bridge/storage/test"},
	} {
		recorder := httptest.NewRecorder()
		request := httptest.NewRequest(route.method, route.path, nil)
		router.ServeHTTP(recorder, request)
		require.Equal(t, http.StatusForbidden, recorder.Code)
		require.Contains(t, recorder.Body.String(), "STEP_UP_ADMIN_API_KEY_FORBIDDEN")
	}
}
