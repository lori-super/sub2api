package handler

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestPrepareMediaBridgeRequestBodyReusesCompositeAdmissionSessionAndLease(t *testing.T) {
	gin.SetMode(gin.TestMode)
	policySnapshots := 0
	bridge, err := service.NewOpenAIChatVideoBridge(
		service.NewUnavailableInlineMediaStore(),
		service.OpenAIChatVideoBridgePolicyProviderFunc(func(context.Context, service.OpenAIChatVideoBridgeRequest) (service.OpenAIChatVideoBridgePolicy, error) {
			policySnapshots++
			return service.OpenAIChatVideoBridgePolicy{
				Mode:                  service.MediaBridgeModeOn,
				IngressProtocols:      []string{service.MediaBridgeIngressOpenAIChatCompletions},
				UpstreamProtocols:     []string{service.MediaBridgeProtocolOpenAIChatCompletions},
				AllowedMIMETypes:      []string{"video/mp4"},
				SignedURLTTL:          time.Hour,
				RequestEndDeleteDelay: time.Minute,
				ObjectPrefix:          "media-bridge",
				UploadTimeout:         time.Minute,
			}, nil
		}),
	)
	require.NoError(t, err)
	bodyCapacity := service.NewMediaBridgeCapacity()
	bridge.SetBodyCapacity(bodyCapacity)
	gatewayService := &service.OpenAIGatewayService{}
	gatewayService.SetOpenAIChatVideoBridge(bridge)
	h := &OpenAIGatewayHandler{gatewayService: gatewayService}

	body := []byte(`{"model":"kimi-k3","messages":[{"role":"user","content":"hello"}]}`)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))

	// Composite middleware performs the admission and first read.
	require.True(t, h.PrepareMediaBridgeRequestBody(c))
	firstSnapshot := bodyCapacity.Snapshot(0)
	require.EqualValues(t, 1, firstSnapshot.Global.InflightRequests)
	readBody, err := io.ReadAll(c.Request.Body)
	require.NoError(t, err)
	require.Equal(t, body, readBody)
	routedSnapshot := bodyCapacity.Snapshot(0)
	c.Request.Body = io.NopCloser(bytes.NewReader(readBody))
	c.Request.ContentLength = int64(len(readBody))

	// The final handler sees the same Gin request and must not reserve again.
	require.True(t, h.prepareMediaBridgeRequestBody(c))
	require.Equal(t, routedSnapshot, bodyCapacity.Snapshot(0))
	require.Equal(t, 1, policySnapshots)

	h.CleanupMediaBridgeRequestBody(c)
	h.CleanupMediaBridgeRequestBody(c)
	require.Equal(t, service.MediaBridgeCapacityUsage{}, bodyCapacity.Snapshot(0).Global)
}
