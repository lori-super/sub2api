//go:build unit

package service

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func x5M5XTestContext(path string, body []byte, apiKeyID int64) *gin.Context {
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, path, bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")
	c.Set("api_key", &APIKey{ID: apiKeyID, Group: &Group{Platform: PlatformOpenAI}})
	return c
}

func TestX5M5XRawChatInjectsIsolatedSessionAffinity(t *testing.T) {
	gin.SetMode(gin.TestMode)
	body := []byte(`{"model":"glm-5.2","messages":[{"role":"user","content":"hello"}],"stream":false}`)
	upstream := &httpUpstreamRecorder{err: errors.New("stop after capture")}
	svc := &OpenAIGatewayService{cfg: rawChatCompletionsTestConfig(), httpUpstream: upstream}
	account := genericOpenAIAdaptiveTestAccount("https://us-api.x5m5x.com/v1")
	c := x5M5XTestContext("/v1/chat/completions", body, 901)
	c.Request.Header.Set(x5M5XSessionHeader, "client-session")

	_, err := svc.ForwardAsChatCompletions(context.Background(), c, account, body, "", "")

	require.Error(t, err)
	want := isolateOpenAIUpstreamSessionID(901, account, "client-session")
	require.NotEmpty(t, want)
	require.Equal(t, want, upstream.lastReq.Header.Get(x5M5XSessionHeader))
	require.NotEqual(t, "client-session", upstream.lastReq.Header.Get(x5M5XSessionHeader))
}

func TestX5M5XNativeResponsesPreservesCacheFieldsAndUsesSameAffinity(t *testing.T) {
	gin.SetMode(gin.TestMode)
	body := []byte(`{"model":"glm-5.2","input":"hello","prompt_cache_key":"cache-session","previous_response_id":"resp_123","store":true,"stream":false}`)
	upstream := &httpUpstreamRecorder{err: errors.New("stop after capture")}
	svc := &OpenAIGatewayService{cfg: rawChatCompletionsTestConfig(), httpUpstream: upstream}
	account := genericOpenAIAdaptiveTestAccount("https://api.x5m5x.com/v1")
	c := x5M5XTestContext("/v1/responses", body, 902)

	_, err := svc.Forward(context.Background(), c, account, body)

	require.Error(t, err)
	want := isolateOpenAIUpstreamSessionID(902, account, "cache-session")
	require.Equal(t, want, upstream.lastReq.Header.Get(x5M5XSessionHeader))
	require.Equal(t, "cache-session", gjson.GetBytes(upstream.lastBody, "prompt_cache_key").String())
	require.Equal(t, "resp_123", gjson.GetBytes(upstream.lastBody, "previous_response_id").String())
	require.True(t, gjson.GetBytes(upstream.lastBody, "store").Bool())
}

func TestX5M5XAffinityIsHostScoped(t *testing.T) {
	account := genericOpenAIAdaptiveTestAccount("https://relay.example.com/v1")
	require.False(t, isX5M5XOpenAIAPIKeyAccount(account))
	require.Empty(t, x5M5XCacheIdentity(nil, account, []byte(`{"prompt_cache_key":"cache"}`)))
}
