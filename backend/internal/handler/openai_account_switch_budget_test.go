package handler

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

type mediaBridgeSwitchBudgetStore struct {
	puts int
}

func (*mediaBridgeSwitchBudgetStore) NewObjectKey(string, string, string) (string, error) {
	return "media-bridge/test/video.mp4", nil
}

func (s *mediaBridgeSwitchBudgetStore) Put(_ context.Context, _ string, _ string, _ int64, body io.Reader) error {
	s.puts++
	_, err := io.Copy(io.Discard, body)
	return err
}

func (*mediaBridgeSwitchBudgetStore) PresignGet(context.Context, string, time.Duration) (string, error) {
	return "https://media.example.test/video.mp4?sig=test", nil
}

func (*mediaBridgeSwitchBudgetStore) Delete(context.Context, string) error { return nil }

type mediaBridgeSwitchBudgetUpstream struct {
	service.HTTPUpstream
	accountIDs []int64
}

func (u *mediaBridgeSwitchBudgetUpstream) Do(_ *http.Request, _ string, accountID int64, _ int) (*http.Response, error) {
	u.accountIDs = append(u.accountIDs, accountID)
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body: io.NopCloser(strings.NewReader(
			`{"id":"chatcmpl-media","object":"chat.completion","created":1,"model":"gpt-5.2","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`,
		)),
	}, nil
}

func TestMediaBridgeRequestIncompatibilityDoesNotConsumeOpenAIAccountSwitchBudget(t *testing.T) {
	mediaBridgeIncompatible := &service.UpstreamFailoverError{
		Stage:  service.GatewayFailureStageAccountAuth,
		Scope:  service.GatewayFailureScopeRequest,
		Reason: service.OpenAIMediaBridgeChatAccountIncompatibleReason,
	}

	require.False(t, shouldConsumeOpenAIAccountSwitchBudget(mediaBridgeIncompatible))

	for _, failoverErr := range []*service.UpstreamFailoverError{
		nil,
		{
			Stage:  service.GatewayFailureStageAccountAuth,
			Scope:  service.GatewayFailureScopeAccount,
			Reason: service.OpenAIMediaBridgeChatAccountIncompatibleReason,
		},
		{
			Stage:  service.GatewayFailureStageAccountAuth,
			Scope:  service.GatewayFailureScopeRequest,
			Reason: service.OpenAIMediaBridgeChatCredentialMissingReason,
		},
		{
			StatusCode: 503,
		},
	} {
		require.True(t, shouldConsumeOpenAIAccountSwitchBudget(failoverErr))
	}
}

func TestMediaBridgeAccountCompatibilityFailoverDoesNotExhaustOrdinarySwitchBudget(t *testing.T) {
	gin.SetMode(gin.TestMode)
	const videoDataURL = "data:video/mp4;base64,AAAAGGZ0eXBpc29tAAAAAGlzb21tcDQy"
	tests := []struct {
		name     string
		path     string
		body     string
		forward  func(*OpenAIGatewayHandler, *gin.Context)
		response func(*testing.T, *httptest.ResponseRecorder)
	}{
		{
			name: "Responses",
			path: "/openai/v1/responses",
			body: `{"model":"gpt-5.2","input":[{"type":"message","role":"user","content":[{"type":"input_video","video_url":"` + videoDataURL + `"}]}]}`,
			forward: func(handler *OpenAIGatewayHandler, c *gin.Context) {
				handler.Responses(c)
			},
			response: func(t *testing.T, recorder *httptest.ResponseRecorder) {
				t.Helper()
				require.Equal(t, "response", gjson.GetBytes(recorder.Body.Bytes(), "object").String())
			},
		},
		{
			name: "Chat Completions",
			path: "/openai/v1/chat/completions",
			body: `{"model":"gpt-5.2","messages":[{"role":"user","content":[{"type":"video_url","video_url":{"url":"` + videoDataURL + `"}}]}]}`,
			forward: func(handler *OpenAIGatewayHandler, c *gin.Context) {
				handler.ChatCompletions(c)
			},
			response: func(t *testing.T, recorder *httptest.ResponseRecorder) {
				t.Helper()
				require.Equal(t, "chat.completion", gjson.GetBytes(recorder.Body.Bytes(), "object").String())
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			groupID := int64(4205)
			accounts := []service.Account{
				{
					ID: 9920, Name: "oauth-incompatible-1", Platform: service.PlatformOpenAI,
					Type: service.AccountTypeOAuth, Status: service.StatusActive, Schedulable: true, Priority: 1,
					Credentials: map[string]any{"access_token": "oauth-1"},
				},
				{
					ID: 9921, Name: "oauth-incompatible-2", Platform: service.PlatformOpenAI,
					Type: service.AccountTypeOAuth, Status: service.StatusActive, Schedulable: true, Priority: 2,
					Credentials: map[string]any{"access_token": "oauth-2"},
				},
				{
					ID: 9922, Name: "api-key-compatible", Platform: service.PlatformOpenAI,
					Type: service.AccountTypeAPIKey, Status: service.StatusActive, Schedulable: true, Priority: 3,
					Credentials: map[string]any{"api_key": "sk-compatible", "base_url": "https://api.example.test"},
				},
			}
			cfg := &config.Config{RunMode: config.RunModeSimple}
			cfg.Default.RateMultiplier = 1
			cfg.Security.URLAllowlist.Enabled = false
			cfg.Gateway.MaxAccountSwitches = 1
			accountRepo := &openAIWSFailoverHandlerAccountRepoStub{accounts: accounts}
			upstream := &mediaBridgeSwitchBudgetUpstream{}
			billingCache := service.NewBillingCacheService(nil, nil, nil, nil, nil, nil, cfg, nil)
			t.Cleanup(billingCache.Stop)
			gateway := service.NewOpenAIGatewayService(
				accountRepo, nil, nil, nil, nil, nil, nil, cfg, nil, nil,
				service.NewBillingService(cfg, nil), nil, billingCache, upstream,
				&service.DeferredService{}, nil, nil, nil, nil, nil, nil, nil,
			)
			store := &mediaBridgeSwitchBudgetStore{}
			bridge, err := service.NewOpenAIChatVideoBridge(store, service.OpenAIChatVideoBridgePolicyProviderFunc(
				func(context.Context, service.OpenAIChatVideoBridgeRequest) (service.OpenAIChatVideoBridgePolicy, error) {
					return service.OpenAIChatVideoBridgePolicy{
						Mode:                  service.MediaBridgeModeOn,
						IngressProtocols:      []string{service.MediaBridgeIngressOpenAIChatCompletions, service.MediaBridgeIngressOpenAIResponses},
						UpstreamProtocols:     []string{service.MediaBridgeProtocolOpenAIChatCompletions},
						Models:                []string{"gpt-5.2"},
						AllowedMIMETypes:      []string{"video/mp4"},
						SignedURLTTL:          time.Hour,
						RequestEndDeleteDelay: time.Hour,
						Deduplicate:           true,
						ObjectPrefix:          "media-bridge",
						UploadTimeout:         time.Minute,
					}, nil
				},
			))
			require.NoError(t, err)
			gateway.SetOpenAIChatVideoBridge(bridge)
			handler := NewOpenAIGatewayHandler(
				gateway,
				service.NewConcurrencyService(nil),
				billingCache,
				service.NewAPIKeyService(nil, nil, nil, nil, nil, nil, cfg),
				nil, nil, nil, nil, cfg,
			)

			recorder := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(recorder)
			c.Request = httptest.NewRequest(http.MethodPost, tt.path, strings.NewReader(tt.body))
			c.Request.Header.Set("Content-Type", "application/json")
			c.Set(string(middleware.ContextKeyAPIKey), &service.APIKey{
				ID: 1805, GroupID: &groupID,
				User:  &service.User{ID: 1705, Status: service.StatusActive},
				Group: &service.Group{ID: groupID, Platform: service.PlatformOpenAI, Status: service.StatusActive},
			})
			c.Set(string(middleware.ContextKeyUser), middleware.AuthSubject{UserID: 1705})

			tt.forward(handler, c)

			require.Equal(t, http.StatusOK, recorder.Code)
			tt.response(t, recorder)
			require.Equal(t, []int64{9922}, upstream.accountIDs)
			require.Equal(t, 1, store.puts)
		})
	}
}
