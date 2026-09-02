package service

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

type memoryInlineMediaStore struct {
	mu       sync.Mutex
	puts     int
	getTTLs  []time.Duration
	deletes  []string
	content  []byte
	putKey   string
	putType  string
	putSize  int64
	assetURL string
	putErr   error
	signErr  error
	keySeq   int
}

type mutableInlineMediaStoreResolver struct {
	mu    sync.Mutex
	store InlineMediaStore
	err   error
}

type blockingInlineMediaStore struct {
	*memoryInlineMediaStore
}

func (s *blockingInlineMediaStore) Put(ctx context.Context, key, contentType string, sizeBytes int64, _ io.Reader) error {
	s.mu.Lock()
	s.puts++
	s.putKey = key
	s.putType = contentType
	s.putSize = sizeBytes
	s.mu.Unlock()
	<-ctx.Done()
	return ctx.Err()
}

func (r *mutableInlineMediaStoreResolver) SnapshotStore(context.Context) (InlineMediaStore, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.store, r.err
}

func (r *mutableInlineMediaStoreResolver) set(store InlineMediaStore, err error) {
	r.mu.Lock()
	r.store = store
	r.err = err
	r.mu.Unlock()
}

func (s *memoryInlineMediaStore) NewObjectKey(relativePrefix, namespace, extension string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.keySeq++
	prefix := strings.Trim(relativePrefix, "/")
	if prefix != "" {
		prefix += "/"
	}
	return fmt.Sprintf("%s%s/test-%d%s", prefix, namespace, s.keySeq, extension), nil
}

func (s *memoryInlineMediaStore) Put(_ context.Context, key, contentType string, sizeBytes int64, body io.Reader) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.puts++
	s.putKey = key
	s.putType = contentType
	s.putSize = sizeBytes
	if s.putErr != nil {
		return s.putErr
	}
	content, err := io.ReadAll(body)
	if err != nil {
		return err
	}
	s.content = content
	return nil
}

func (s *memoryInlineMediaStore) PresignGet(_ context.Context, key string, ttl time.Duration) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.getTTLs = append(s.getTTLs, ttl)
	if s.signErr != nil {
		return "", s.signErr
	}
	if s.assetURL != "" {
		return s.assetURL, nil
	}
	return "https://private.example.test/" + key + "?sig=short-lived", nil
}

func (s *memoryInlineMediaStore) Delete(_ context.Context, key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.deletes = append(s.deletes, key)
	return nil
}

func openAIChatVideoTestPolicy() OpenAIChatVideoBridgePolicy {
	return OpenAIChatVideoBridgePolicy{
		Mode:                  MediaBridgeModeOn,
		UpstreamProtocols:     []string{MediaBridgeProtocolOpenAIChatCompletions},
		Models:                []string{"kimi-k3"},
		AllowedMIMETypes:      []string{"video/mp4"},
		SignedURLTTL:          time.Hour,
		RequestEndDeleteDelay: 15 * time.Minute,
		Deduplicate:           true,
		ObjectPrefix:          "media-bridge",
		UploadTimeout:         10 * time.Minute,
	}
}

func openAIChatVideoTestAccount() *Account {
	return &Account{
		ID:       7,
		Platform: PlatformKimi,
		Type:     AccountTypeAPIKey,
		Credentials: map[string]any{
			"api_key":  "sk-test",
			"base_url": "https://upstream.example",
		},
	}
}

func newOpenAIChatVideoTestBridge(t *testing.T, store InlineMediaStore, policy OpenAIChatVideoBridgePolicy) *OpenAIChatVideoBridge {
	t.Helper()
	bridge, err := NewOpenAIChatVideoBridge(store, OpenAIChatVideoBridgePolicyProviderFunc(
		func(context.Context, OpenAIChatVideoBridgeRequest) (OpenAIChatVideoBridgePolicy, error) {
			return policy, nil
		},
	))
	require.NoError(t, err)
	return bridge
}

func TestPrepareOpenAIChatVideoBridgeRequestBodyReservesUntilHandlerCleanup(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, endpoint := range []string{"/v1/chat/completions", "/v1/responses"} {
		t.Run(endpoint, func(t *testing.T) {
			bridge := newOpenAIChatVideoTestBridge(t, &memoryInlineMediaStore{}, openAIChatVideoTestPolicy())
			bodyCapacity := NewMediaBridgeCapacity()
			bridge.SetBodyCapacity(bodyCapacity)
			svc := &OpenAIGatewayService{}
			svc.SetOpenAIChatVideoBridge(bridge)

			body := []byte(`{"model":"kimi-k3","messages":[{"role":"user","content":"hello"}]}`)
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			c.Request = httptest.NewRequest(http.MethodPost, endpoint, bytes.NewReader(body))
			c.Request = c.Request.WithContext(context.WithValue(c.Request.Context(), ctxkey.UserID, int64(42)))

			require.NoError(t, svc.PrepareOpenAIChatVideoBridgeRequestBody(c.Request.Context(), c, int64(len(body))))
			require.EqualValues(t, 1, bodyCapacity.Snapshot(42).Global.InflightRequests)
			require.EqualValues(t, openAIChatVideoBridgeJSONBodyFactor*int64(len(body)), bodyCapacity.Snapshot(42).Global.InflightDecodedBytes)
			read, err := io.ReadAll(c.Request.Body)
			require.NoError(t, err)
			require.Equal(t, body, read)

			svc.CleanupOpenAIChatVideoBridgeSession(c)
			require.Equal(t, MediaBridgeCapacityUsage{}, bodyCapacity.Snapshot(42).Global)
		})
	}
}

func TestPrepareOpenAIChatVideoBridgeRequestBodyRejectsBeforeReadOnMemoryPressure(t *testing.T) {
	gin.SetMode(gin.TestMode)
	bridge := newOpenAIChatVideoTestBridge(t, &memoryInlineMediaStore{}, openAIChatVideoTestPolicy())
	bodyCapacity := NewMediaBridgeCapacity(MediaBridgeCapacityPolicyFunc(func(
		context.Context,
		MediaBridgeCapacityPolicyInput,
	) error {
		return &MediaBridgeCapacityError{
			Reason:     MediaBridgeCapacityReasonMemoryHard,
			Scope:      MediaBridgeCapacityScopeGlobal,
			RetryAfter: time.Second,
		}
	}))
	bridge.SetBodyCapacity(bodyCapacity)
	svc := &OpenAIGatewayService{}
	svc.SetOpenAIChatVideoBridge(bridge)

	body := []byte(`{"model":"kimi-k3"}`)
	source := &countingReadCloser{Reader: bytes.NewReader(body)}
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", source)
	c.Request.Body = source
	c.Request.ContentLength = int64(len(body))

	err := svc.PrepareOpenAIChatVideoBridgeRequestBody(c.Request.Context(), c, int64(len(body)))
	bridgeErr, ok := AsOpenAIChatVideoBridgeError(err)
	require.True(t, ok)
	require.Equal(t, http.StatusTooManyRequests, bridgeErr.StatusCode)
	require.Positive(t, bridgeErr.RetryAfter)
	require.Zero(t, source.reads)
	require.Equal(t, MediaBridgeCapacityUsage{}, bodyCapacity.Snapshot(0).Global)
}

type countingReadCloser struct {
	io.Reader
	reads int
}

func (r *countingReadCloser) Read(buffer []byte) (int, error) {
	r.reads++
	return r.Reader.Read(buffer)
}

func (*countingReadCloser) Close() error { return nil }

func testMP4Bytes() []byte {
	result := make([]byte, 24)
	binary.BigEndian.PutUint32(result[:4], uint32(len(result)))
	copy(result[4:8], "ftyp")
	copy(result[8:12], "isom")
	copy(result[16:20], "isom")
	copy(result[20:24], "mp42")
	return result
}

func testMP4DataURL(content []byte) string {
	return "data:video/mp4;base64," + base64.StdEncoding.EncodeToString(content)
}

func testOpenAIChatVideoBody(dataURL string) []byte {
	return []byte(fmt.Sprintf(`{
  "model":"client-model",
  "messages":[{"role":"user","metadata":{"keep":true},"content":[
    {"type":"text","text":"keep me"},
    {"type":"video_url","video_url":{"url":%q,"detail":"keep-detail"}},
    {"type":"image_url","image_url":{"url":"data:image/png;base64,AAAA"}}
  ]}],
  "unknown":{"keep":"unchanged"}
}`, dataURL))
}

func TestOpenAIChatVideoBridgeRewritesCanonicalURLAndReusesUploadAcrossRetry(t *testing.T) {
	store := &memoryInlineMediaStore{}
	bridge := newOpenAIChatVideoTestBridge(t, store, openAIChatVideoTestPolicy())
	var scheduledDelay time.Duration
	var scheduledCleanup func()
	bridge.schedule = func(delay time.Duration, cleanup func()) {
		scheduledDelay = delay
		scheduledCleanup = cleanup
	}
	session, err := bridge.NewSession(context.Background(), OpenAIChatVideoBridgeRequest{
		TenantID:         9,
		StableKey:        "request-1",
		UpstreamProtocol: MediaBridgeProtocolOpenAIChatCompletions,
	})
	require.NoError(t, err)
	require.NotNil(t, session)
	ctx := WithOpenAIChatVideoBridgeSession(context.Background(), session)
	body := testOpenAIChatVideoBody(testMP4DataURL(testMP4Bytes()))

	first, changed, err := MaterializeOpenAIChatVideoDataURLs(ctx, &Account{ID: 7}, "kimi-k3", body)
	require.NoError(t, err)
	require.True(t, changed)
	second, changed, err := MaterializeOpenAIChatVideoDataURLs(ctx, &Account{ID: 7}, "KIMI-K3", body)
	require.NoError(t, err)
	require.True(t, changed)
	require.JSONEq(t, string(first), string(second))
	require.Equal(t, 1, store.puts)
	require.Equal(t, testMP4Bytes(), store.content)
	require.Equal(t, int64(len(testMP4Bytes())), store.putSize)
	require.Equal(t, "video/mp4", store.putType)
	require.Equal(t, "media-bridge/chat-video/test-1.mp4", store.putKey)
	require.Equal(t, time.Hour, store.getTTLs[0])
	require.Equal(t, "keep me", gjson.GetBytes(first, "messages.0.content.0.text").String())
	require.Equal(t, "keep-detail", gjson.GetBytes(first, "messages.0.content.1.video_url.detail").String())
	require.Equal(t, "unchanged", gjson.GetBytes(first, "unknown.keep").String())
	require.True(t, strings.HasPrefix(gjson.GetBytes(first, "messages.0.content.1.video_url.url").String(), "https://"))

	require.NoError(t, session.Cleanup(context.Background()))
	require.Equal(t, 15*time.Minute, scheduledDelay)
	require.Empty(t, store.deletes)
	require.NotNil(t, scheduledCleanup)
	scheduledCleanup()
	require.Equal(t, []string{store.putKey}, store.deletes)
}

func TestOpenAIChatVideoBridgeHonorsFinalModelAndZeroLimits(t *testing.T) {
	store := &memoryInlineMediaStore{}
	policy := openAIChatVideoTestPolicy()
	policy.MaxVideoBytes = 0
	policy.MaxRequestBytes = 0
	policy.MaxVideos = 0
	policy.RequestEndDeleteDelay = 0
	bridge := newOpenAIChatVideoTestBridge(t, store, policy)
	session, err := bridge.NewSession(context.Background(), OpenAIChatVideoBridgeRequest{UpstreamProtocol: MediaBridgeProtocolOpenAIChatCompletions})
	require.NoError(t, err)
	ctx := WithOpenAIChatVideoBridgeSession(context.Background(), session)
	body := testOpenAIChatVideoBody(testMP4DataURL(testMP4Bytes()))

	untouched, changed, err := MaterializeOpenAIChatVideoDataURLs(ctx, &Account{ID: 7}, "other-model", body)
	require.NoError(t, err)
	require.False(t, changed)
	require.Equal(t, body, untouched)
	require.Zero(t, store.puts)

	_, changed, err = MaterializeOpenAIChatVideoDataURLs(ctx, &Account{ID: 7}, "kimi-k3", body)
	require.NoError(t, err)
	require.True(t, changed)
	require.Equal(t, 1, store.puts)
	require.NoError(t, session.Cleanup(context.Background()))
	require.Len(t, store.deletes, 1)
}

func TestOpenAIChatVideoBridgeHonorsIngressProtocolScope(t *testing.T) {
	policy := openAIChatVideoTestPolicy()
	policy.IngressProtocols = []string{MediaBridgeIngressOpenAIResponses}
	bridge := newOpenAIChatVideoTestBridge(t, &memoryInlineMediaStore{}, policy)

	disabled, err := bridge.NewSession(context.Background(), OpenAIChatVideoBridgeRequest{
		IngressProtocol:  MediaBridgeIngressOpenAIChatCompletions,
		UpstreamProtocol: MediaBridgeProtocolOpenAIChatCompletions,
	})
	require.NoError(t, err)
	require.Nil(t, disabled)

	enabled, err := bridge.NewSession(context.Background(), OpenAIChatVideoBridgeRequest{
		IngressProtocol:  MediaBridgeIngressOpenAIResponses,
		UpstreamProtocol: MediaBridgeProtocolOpenAIChatCompletions,
	})
	require.NoError(t, err)
	require.NotNil(t, enabled)
}

func TestOpenAIChatVideoBridgeClassifiesIngressProtocol(t *testing.T) {
	tests := []struct {
		path string
		want string
	}{
		{path: "/v1/chat/completions", want: MediaBridgeIngressOpenAIChatCompletions},
		{path: "/openai/v1/responses", want: MediaBridgeIngressOpenAIResponses},
		{path: "/v1/responses/compact", want: ""},
		{path: "/anthropic/v1/messages", want: ""},
		{path: "/v1/embeddings", want: ""},
	}
	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			c.Request = httptest.NewRequest(http.MethodPost, tt.path, nil)
			require.Equal(t, tt.want, openAIChatVideoBridgeIngressProtocol(c))
		})
	}
}

func TestOpenAIChatVideoBridgeClassifiesResponsesInputFilesPrecisely(t *testing.T) {
	largeMP4 := append(testMP4Bytes(), make([]byte, 8192)...)
	largeDataURL := testMP4DataURL(largeMP4)
	tests := []struct {
		name string
		part string
		want bool
	}{
		{name: "mp4 data URI", part: `{"type":"input_file","file_data":"data:video/mp4;base64,AAAA"}`, want: true},
		{name: "bare base64 with MIME", part: `{"type":"input_file","file_data":"AAAA","mime_type":"video/mp4"}`, want: true},
		{name: "bare base64 with filename", part: `{"type":"input_file","file_data":"AAAA","filename":"clip.mp4"}`, want: true},
		{name: "nested MP4 URL", part: `{"type":"input_file","file_url":{"url":"https://media.example/clip.mp4?sig=1"}}`, want: true},
		{name: "large nested MP4 data URI", part: fmt.Sprintf(`{"type":"input_file","file_url":{"url":%q}}`, largeDataURL), want: true},
		{name: "file id alone is not bridgeable", part: `{"type":"input_file","file_id":"file-1","filename":"clip.mp4"}`, want: false},
		{name: "ordinary PDF", part: `{"type":"input_file","file_data":"data:application/pdf;base64,AAAA","filename":"doc.pdf"}`, want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, openAIResponsesPartIsVideo(gjson.Parse(tt.part)))
		})
	}
}

func TestOpenAIChatVideoBridgeForcesChatEgressOnlyForEligibleVideo(t *testing.T) {
	gin.SetMode(gin.TestMode)
	dataURL := testMP4DataURL(testMP4Bytes())
	tests := []struct {
		name          string
		path          string
		body          string
		upstreamModel string
		want          bool
	}{
		{
			name:          "chat video",
			path:          "/v1/chat/completions",
			body:          fmt.Sprintf(`{"model":"kimi-k3","messages":[{"role":"user","content":[{"type":"video_url","video_url":{"url":%q}}]}]}`, dataURL),
			upstreamModel: "kimi-k3",
			want:          true,
		},
		{
			name:          "responses video",
			path:          "/v1/responses",
			body:          fmt.Sprintf(`{"model":"kimi-k3","input":[{"role":"user","content":[{"type":"input_video","video_url":%q}]}]}`, dataURL),
			upstreamModel: "kimi-k3",
			want:          true,
		},
		{
			name:          "text request keeps normal route",
			path:          "/v1/responses",
			body:          `{"model":"kimi-k3","input":"hello"}`,
			upstreamModel: "kimi-k3",
			want:          false,
		},
		{
			name:          "chat does not claim messages video dialect",
			path:          "/v1/chat/completions",
			body:          `{"model":"kimi-k3","messages":[{"role":"user","content":[{"type":"video","source":{"type":"url","url":"https://media.example/clip.mp4"}}]}]}`,
			upstreamModel: "kimi-k3",
			want:          false,
		},
		{
			name:          "other model keeps normal route",
			path:          "/v1/responses",
			body:          fmt.Sprintf(`{"model":"other-model","input":[{"role":"user","content":[{"type":"input_video","video_url":%q}]}]}`, dataURL),
			upstreamModel: "other-model",
			want:          false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			policy := openAIChatVideoTestPolicy()
			policy.IngressProtocols = []string{
				MediaBridgeIngressOpenAIChatCompletions,
				MediaBridgeIngressOpenAIResponses,
			}
			bridge := newOpenAIChatVideoTestBridge(t, &memoryInlineMediaStore{}, policy)
			svc := &OpenAIGatewayService{}
			svc.SetOpenAIChatVideoBridge(bridge)
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			c.Request = httptest.NewRequest(http.MethodPost, tt.path, strings.NewReader(tt.body))

			got, err := svc.shouldForceChatVideoEgress(
				context.Background(),
				c,
				openAIChatVideoTestAccount(),
				tt.upstreamModel,
				[]byte(tt.body),
			)
			require.NoError(t, err)
			require.Equal(t, tt.want, got)
		})
	}
}

func TestOpenAIChatVideoBridgeDoesNotOverrideAnthropicProtocolAccounts(t *testing.T) {
	gin.SetMode(gin.TestMode)
	dataURL := testMP4DataURL(testMP4Bytes())
	body := []byte(fmt.Sprintf(`{"model":"kimi-k3","messages":[{"role":"user","content":[{"type":"video_url","video_url":{"url":%q}}]}]}`, dataURL))
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))

	bridge := newOpenAIChatVideoTestBridge(t, &memoryInlineMediaStore{}, openAIChatVideoTestPolicy())
	svc := &OpenAIGatewayService{chatVideoBridge: bridge}
	account := &Account{
		ID:       7,
		Platform: PlatformKimi,
		Type:     AccountTypeAPIKey,
		Credentials: map[string]any{
			"api_protocol": APIProtocolAnthropic,
		},
	}

	forced, err := svc.shouldForceChatVideoEgress(context.Background(), c, account, "kimi-k3", body)
	require.NoError(t, err)
	require.False(t, forced)
}

func TestOpenAIChatVideoBridgeOnlyOverridesAPIKeyChatAccounts(t *testing.T) {
	gin.SetMode(gin.TestMode)
	dataURL := testMP4DataURL(testMP4Bytes())
	body := []byte(fmt.Sprintf(`{"model":"kimi-k3","input":[{"type":"input_video","video_url":%q}]}`, dataURL))

	tests := []struct {
		name    string
		account *Account
	}{
		{name: "openai oauth", account: &Account{ID: 7, Platform: PlatformOpenAI, Type: AccountTypeOAuth}},
		{name: "openai setup token", account: &Account{ID: 7, Platform: PlatformOpenAI, Type: AccountTypeSetupToken}},
		{name: "grok api key", account: &Account{ID: 7, Platform: PlatformGrok, Type: AccountTypeAPIKey}},
		{name: "generic upstream", account: &Account{ID: 7, Platform: PlatformOpenAI, Type: AccountTypeUpstream}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			bridge := newOpenAIChatVideoTestBridge(t, &memoryInlineMediaStore{}, openAIChatVideoTestPolicy())
			svc := &OpenAIGatewayService{chatVideoBridge: bridge}
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(body))

			forced, err := svc.shouldForceChatVideoEgress(context.Background(), c, tt.account, "kimi-k3", body)
			require.False(t, forced)
			var failoverErr *UpstreamFailoverError
			require.ErrorAs(t, err, &failoverErr)
			require.Equal(t, OpenAIMediaBridgeChatAccountIncompatibleReason, failoverErr.Reason)
			require.Equal(t, GatewayFailureScopeRequest, failoverErr.Scope)
			require.True(t, failoverErr.ShouldRetryNextAccount())
			require.False(t, failoverErr.ShouldReportAccountScheduleFailure())
		})
	}
}

func TestOpenAIChatVideoBridgeMissingAPIKeyFailsOverAndReportsAccount(t *testing.T) {
	gin.SetMode(gin.TestMode)
	dataURL := testMP4DataURL(testMP4Bytes())
	body := []byte(fmt.Sprintf(`{"model":"kimi-k3","input":[{"type":"input_video","video_url":%q}]}`, dataURL))
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(body))
	bridge := newOpenAIChatVideoTestBridge(t, &memoryInlineMediaStore{}, openAIChatVideoTestPolicy())
	svc := &OpenAIGatewayService{chatVideoBridge: bridge}
	account := &Account{ID: 7, Platform: PlatformKimi, Type: AccountTypeAPIKey}

	forced, err := svc.shouldForceChatVideoEgress(context.Background(), c, account, "kimi-k3", body)
	require.False(t, forced)
	var failoverErr *UpstreamFailoverError
	require.ErrorAs(t, err, &failoverErr)
	require.Equal(t, OpenAIMediaBridgeChatCredentialMissingReason, failoverErr.Reason)
	require.Equal(t, GatewayFailureScopeAccount, failoverErr.Scope)
	require.True(t, failoverErr.ShouldReportAccountScheduleFailure())
}

func TestOpenAIChatVideoBridgeRejectsLossyForcedResponsesConversion(t *testing.T) {
	gin.SetMode(gin.TestMode)
	dataURL := testMP4DataURL(testMP4Bytes())
	tests := []struct {
		name         string
		path         string
		body         string
		markCompact  bool
		wantContains string
	}{
		{
			name:         "previous response continuation",
			path:         "/v1/responses",
			body:         fmt.Sprintf(`{"model":"kimi-k3","previous_response_id":"resp_123","input":[{"type":"input_video","video_url":%q}]}`, dataURL),
			wantContains: "previous_response_id",
		},
		{
			name:         "mixed PDF",
			path:         "/v1/responses",
			body:         fmt.Sprintf(`{"model":"kimi-k3","input":[{"type":"message","role":"user","content":[{"type":"input_video","video_url":%q},{"type":"input_file","filename":"doc.pdf","file_data":"data:application/pdf;base64,AAAA"}]}]}`, dataURL),
			wantContains: "non-video input_file",
		},
		{
			name:         "hosted tool",
			path:         "/v1/responses",
			body:         fmt.Sprintf(`{"model":"kimi-k3","tools":[{"type":"web_search"}],"input":[{"type":"input_video","video_url":%q}]}`, dataURL),
			wantContains: "web_search",
		},
		{
			name:         "native compaction",
			path:         "/v1/responses",
			body:         fmt.Sprintf(`{"model":"kimi-k3","input":[{"type":"input_video","video_url":%q}]}`, dataURL),
			markCompact:  true,
			wantContains: "compaction",
		},
		{
			name:         "non-canonical Responses video type",
			path:         "/v1/responses",
			body:         fmt.Sprintf(`{"model":"kimi-k3","input":[{"type":"INPUT_VIDEO","video_url":%q}]}`, dataURL),
			wantContains: "exactly input_video",
		},
		{
			name:         "non-canonical Chat video type",
			path:         "/v1/chat/completions",
			body:         fmt.Sprintf(`{"model":"kimi-k3","messages":[{"role":"user","content":[{"type":"VIDEO_URL","video_url":{"url":%q}}]}]}`, dataURL),
			wantContains: "exactly video_url",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			policy := openAIChatVideoTestPolicy()
			policy.IngressProtocols = []string{
				MediaBridgeIngressOpenAIChatCompletions,
				MediaBridgeIngressOpenAIResponses,
			}
			bridge := newOpenAIChatVideoTestBridge(t, &memoryInlineMediaStore{}, policy)
			svc := &OpenAIGatewayService{}
			svc.SetOpenAIChatVideoBridge(bridge)
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			c.Request = httptest.NewRequest(http.MethodPost, tt.path, strings.NewReader(tt.body))
			if tt.markCompact {
				MarkOpenAINativeCompactionV2(c)
			}

			forced, err := svc.shouldForceChatVideoEgress(context.Background(), c, openAIChatVideoTestAccount(), "kimi-k3", []byte(tt.body))
			require.False(t, forced)
			bridgeErr, ok := AsOpenAIChatVideoBridgeError(err)
			require.True(t, ok)
			require.Equal(t, http.StatusBadRequest, bridgeErr.StatusCode)
			require.Contains(t, bridgeErr.Message, tt.wantContains)
		})
	}
}

func TestOpenAIChatVideoBridgeMaterializesConvertedProtocolInputs(t *testing.T) {
	data := base64.StdEncoding.EncodeToString(testMP4Bytes())
	dataURL := "data:video/mp4;base64," + data
	tests := []struct {
		name    string
		ingress string
		convert func(*testing.T) []byte
	}{
		{
			name:    "responses",
			ingress: MediaBridgeIngressOpenAIResponses,
			convert: func(t *testing.T) []byte {
				var req apicompat.ResponsesRequest
				require.NoError(t, json.Unmarshal([]byte(fmt.Sprintf(`{"model":"kimi-k3","input":[{"type":"message","role":"user","content":[{"type":"input_video","video_url":%q}]}]}`, dataURL)), &req))
				chatReq, err := apicompat.ResponsesToChatCompletionsRequest(&req)
				require.NoError(t, err)
				body, err := json.Marshal(chatReq)
				require.NoError(t, err)
				return body
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := &memoryInlineMediaStore{}
			policy := openAIChatVideoTestPolicy()
			policy.IngressProtocols = []string{tt.ingress}
			bridge := newOpenAIChatVideoTestBridge(t, store, policy)
			session, err := bridge.NewSession(context.Background(), OpenAIChatVideoBridgeRequest{
				IngressProtocol:  tt.ingress,
				UpstreamProtocol: MediaBridgeProtocolOpenAIChatCompletions,
			})
			require.NoError(t, err)
			require.NotNil(t, session)

			body := tt.convert(t)
			rewritten, changed, err := MaterializeOpenAIChatVideoDataURLs(
				WithOpenAIChatVideoBridgeSession(context.Background(), session),
				&Account{ID: 7},
				"kimi-k3",
				body,
			)
			require.NoError(t, err)
			require.True(t, changed)
			require.Equal(t, 1, store.puts)
			require.True(t, strings.HasPrefix(gjson.GetBytes(rewritten, "messages.0.content.0.video_url.url").String(), "https://"))
		})
	}
}

func TestOpenAIChatVideoBridgePreMaterializesResponsesBeforeProtocolConversion(t *testing.T) {
	dataURL := testMP4DataURL(testMP4Bytes())
	bareBase64 := base64.StdEncoding.EncodeToString(testMP4Bytes())
	body := []byte(fmt.Sprintf(`{
  "model":"kimi-k3",
  "input":[{"type":"message","role":"user","content":[
    {"type":"input_video","video_url":%q,"metadata":{"keep":true}},
    {"type":"video_url","video_url":{"url":%q,"detail":"keep"}},
    {"type":"input_video","video_url":"https://media.example/original.mp4"},
    {"type":"input_file","file_url":{"url":"https://media.example/replaced.mp4","detail":"keep-file-url"},"file_data":%q,"filename":"inline.mp4","custom":"keep"},
    {"type":"input_file","file_data":%q,"mime_type":"video/mp4"},
    {"type":"input_file","file_url":{"url":%q},"filename":"nested.mp4"},
    {"type":"input_file","file_data":%q},
    {"type":"input_file","file_data":"data:application/pdf;base64,JVBERg==","filename":"doc.pdf"}
  ]}],
  "unknown":{"keep":"unchanged"}
}`, dataURL, dataURL, dataURL, bareBase64, dataURL, dataURL))

	store := &memoryInlineMediaStore{assetURL: "https://private.example.test/object?id=opaque"}
	policy := openAIChatVideoTestPolicy()
	policy.IngressProtocols = []string{MediaBridgeIngressOpenAIResponses}
	svc := &OpenAIGatewayService{chatVideoBridge: newOpenAIChatVideoTestBridge(t, store, policy)}
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(body))

	first, changed, err := svc.materializeResponsesVideoDataURLs(context.Background(), c, &Account{ID: 7}, "kimi-k3", body)
	require.NoError(t, err)
	require.True(t, changed)
	require.True(t, gjson.ValidBytes(first))
	require.NotContains(t, string(first), "data:video/mp4;base64,")
	require.NotContains(t, string(first), bareBase64)
	require.Equal(t, 1, store.puts, "identical Responses parts should share the request-session object")
	require.True(t, gjson.GetBytes(first, "input.0.content.0.metadata.keep").Bool())
	require.Equal(t, "keep", gjson.GetBytes(first, "input.0.content.1.video_url.detail").String())
	require.Equal(t, "https://media.example/original.mp4", gjson.GetBytes(first, "input.0.content.2.video_url").String())
	require.Equal(t, "", gjson.GetBytes(first, "input.0.content.3.file_data").String())
	require.True(t, strings.HasPrefix(gjson.GetBytes(first, "input.0.content.3.file_url.url").String(), "https://"))
	require.Equal(t, "keep-file-url", gjson.GetBytes(first, "input.0.content.3.file_url.detail").String())
	require.Equal(t, "keep", gjson.GetBytes(first, "input.0.content.3.custom").String())
	require.Equal(t, "", gjson.GetBytes(first, "input.0.content.4.file_data").String())
	require.True(t, strings.HasPrefix(gjson.GetBytes(first, "input.0.content.4.file_url").String(), "https://"))
	require.True(t, strings.HasPrefix(gjson.GetBytes(first, "input.0.content.5.file_url.url").String(), "https://"))
	require.Equal(t, "", gjson.GetBytes(first, "input.0.content.6.file_data").String())
	require.Equal(t, "video/mp4", gjson.GetBytes(first, "input.0.content.6.mime_type").String())
	require.Equal(t, "data:application/pdf;base64,JVBERg==", gjson.GetBytes(first, "input.0.content.7.file_data").String())
	require.Equal(t, "unchanged", gjson.GetBytes(first, "unknown.keep").String())

	var responsesReq apicompat.ResponsesRequest
	require.NoError(t, json.Unmarshal(first, &responsesReq))
	chatReq, err := apicompat.ResponsesToChatCompletionsRequest(&responsesReq)
	require.NoError(t, err)
	chatBody, err := json.Marshal(chatReq)
	require.NoError(t, err)
	for _, path := range []string{
		"messages.0.content.0.video_url.url",
		"messages.0.content.1.video_url.url",
		"messages.0.content.2.video_url.url",
		"messages.0.content.3.video_url.url",
		"messages.0.content.4.video_url.url",
		"messages.0.content.5.video_url.url",
		"messages.0.content.6.video_url.url",
	} {
		require.True(t, strings.HasPrefix(gjson.GetBytes(chatBody, path).String(), "https://"), path)
	}

	second, changed, err := svc.materializeResponsesVideoDataURLs(context.Background(), c, &Account{ID: 8}, "kimi-k3", body)
	require.NoError(t, err)
	require.True(t, changed)
	require.JSONEq(t, string(first), string(second))
	require.Equal(t, 1, store.puts, "retry/failover must reuse the pre-materialized object")
}

func TestOpenAIChatVideoBridgeLeavesResponsesURLsUntouchedDuringPreMaterialization(t *testing.T) {
	body := []byte(`{"model":"kimi-k3","input":[{"type":"input_video","video_url":{"url":"https://media.example/video.mp4","detail":"keep"}}]}`)
	store := &memoryInlineMediaStore{}
	policy := openAIChatVideoTestPolicy()
	policy.IngressProtocols = []string{MediaBridgeIngressOpenAIResponses}
	svc := &OpenAIGatewayService{chatVideoBridge: newOpenAIChatVideoTestBridge(t, store, policy)}
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(body))

	got, changed, err := svc.materializeResponsesVideoDataURLs(context.Background(), c, &Account{ID: 7}, "kimi-k3", body)
	require.NoError(t, err)
	require.False(t, changed)
	require.Equal(t, body, got)
	require.Zero(t, store.puts)
}

func TestOpenAIChatVideoBridgeDeduplicationPolicyStillCachesRetries(t *testing.T) {
	dataURL := testMP4DataURL(testMP4Bytes())
	body := []byte(fmt.Sprintf(`{"model":"kimi-k3","messages":[{"role":"user","content":[{"type":"video_url","video_url":{"url":%q}},{"type":"video_url","video_url":{"url":%q}}]}]}`, dataURL, dataURL))
	for _, tt := range []struct {
		name         string
		deduplicate  bool
		wantPutCount int
	}{
		{name: "deduplicate identical parts", deduplicate: true, wantPutCount: 1},
		{name: "keep distinct parts", deduplicate: false, wantPutCount: 2},
	} {
		t.Run(tt.name, func(t *testing.T) {
			store := &memoryInlineMediaStore{}
			policy := openAIChatVideoTestPolicy()
			policy.Deduplicate = tt.deduplicate
			bridge := newOpenAIChatVideoTestBridge(t, store, policy)
			session, err := bridge.NewSession(context.Background(), OpenAIChatVideoBridgeRequest{UpstreamProtocol: MediaBridgeProtocolOpenAIChatCompletions})
			require.NoError(t, err)
			ctx := WithOpenAIChatVideoBridgeSession(context.Background(), session)

			_, changed, err := MaterializeOpenAIChatVideoDataURLs(ctx, &Account{ID: 7}, "kimi-k3", body)
			require.NoError(t, err)
			require.True(t, changed)
			_, changed, err = MaterializeOpenAIChatVideoDataURLs(ctx, &Account{ID: 8}, "kimi-k3", body)
			require.NoError(t, err)
			require.True(t, changed)
			require.Equal(t, tt.wantPutCount, store.puts, "retry must reuse every occurrence cache entry")
		})
	}
}

func TestOpenAIChatVideoBridgeRefreshesNearExpiryURLWithoutReupload(t *testing.T) {
	store := &memoryInlineMediaStore{}
	policy := openAIChatVideoTestPolicy()
	policy.SignedURLTTL = 500 * time.Millisecond
	bridge := newOpenAIChatVideoTestBridge(t, store, policy)
	session, err := bridge.NewSession(context.Background(), OpenAIChatVideoBridgeRequest{
		UpstreamProtocol: MediaBridgeProtocolOpenAIChatCompletions,
	})
	require.NoError(t, err)
	ctx := WithOpenAIChatVideoBridgeSession(context.Background(), session)
	body := testOpenAIChatVideoBody(testMP4DataURL(testMP4Bytes()))

	_, _, err = MaterializeOpenAIChatVideoDataURLs(ctx, &Account{ID: 7}, "kimi-k3", body)
	require.NoError(t, err)
	_, _, err = MaterializeOpenAIChatVideoDataURLs(ctx, &Account{ID: 7}, "kimi-k3", body)
	require.NoError(t, err)
	require.Equal(t, 1, store.puts)
	require.Len(t, store.getTTLs, 2)
}

func TestOpenAIChatVideoBridgeStrictValidationAndLimits(t *testing.T) {
	tests := []struct {
		name       string
		dataURL    string
		configure  func(*OpenAIChatVideoBridgePolicy)
		wantStatus int
	}{
		{name: "unsupported MIME", dataURL: "data:video/webm;base64," + base64.StdEncoding.EncodeToString(testMP4Bytes()), wantStatus: http.StatusUnsupportedMediaType},
		{name: "MIME parameters rejected", dataURL: "data:video/mp4;codecs=avc1;base64," + base64.StdEncoding.EncodeToString(testMP4Bytes()), wantStatus: http.StatusUnsupportedMediaType},
		{name: "invalid standard base64", dataURL: "data:video/mp4;base64,AAAA____", wantStatus: http.StatusBadRequest},
		{name: "non canonical padding bits", dataURL: "data:video/mp4;base64,AB==", wantStatus: http.StatusBadRequest},
		{name: "invalid MP4 header", dataURL: testMP4DataURL([]byte("not-an-mp4-file!")), wantStatus: http.StatusBadRequest},
		{name: "single file limit", dataURL: testMP4DataURL(testMP4Bytes()), configure: func(policy *OpenAIChatVideoBridgePolicy) { policy.MaxVideoBytes = 23 }, wantStatus: http.StatusRequestEntityTooLarge},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := &memoryInlineMediaStore{}
			policy := openAIChatVideoTestPolicy()
			if tt.configure != nil {
				tt.configure(&policy)
			}
			bridge := newOpenAIChatVideoTestBridge(t, store, policy)
			session, err := bridge.NewSession(context.Background(), OpenAIChatVideoBridgeRequest{UpstreamProtocol: MediaBridgeProtocolOpenAIChatCompletions})
			require.NoError(t, err)
			ctx := WithOpenAIChatVideoBridgeSession(context.Background(), session)
			_, _, err = MaterializeOpenAIChatVideoDataURLs(ctx, &Account{ID: 7}, "kimi-k3", testOpenAIChatVideoBody(tt.dataURL))
			bridgeErr, ok := AsOpenAIChatVideoBridgeError(err)
			require.True(t, ok)
			require.Equal(t, tt.wantStatus, bridgeErr.StatusCode)
			require.Zero(t, store.puts)
		})
	}
}

func TestOpenAIChatVideoDataURLUsesOriginalBodySlice(t *testing.T) {
	body := testOpenAIChatVideoBody(testMP4DataURL(testMP4Bytes()))
	result := gjson.GetBytes(body, "messages.0.content.1.video_url.url")
	dataURL, ok, err := openAIChatVideoDataURLBytes(body, result)
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, testMP4DataURL(testMP4Bytes()), string(dataURL))
	original := dataURL[0]
	dataURL[0] = 'D'
	require.Equal(t, byte('D'), body[result.Index+1])
	dataURL[0] = original
}

func TestOpenAIChatVideoDataURLRejectsEscapedSemanticPrefix(t *testing.T) {
	payload := base64.StdEncoding.EncodeToString(testMP4Bytes())
	for _, jsonValue := range []string{
		`"data\u003avideo/mp4;base64,` + payload + `"`,
		`"\u0064ata:video/mp4;base64,` + payload + `"`,
		`"DATA\u003AVIDEO/MP4;BASE64,` + payload + `"`,
	} {
		body := []byte(`{"url":` + jsonValue + `}`)
		require.True(t, json.Valid(body), string(body))
		dataURL, isDataURL, err := openAIChatVideoDataURLBytes(body, gjson.GetBytes(body, "url"))
		require.Error(t, err)
		require.True(t, isDataURL)
		require.Nil(t, dataURL)
	}

	body := []byte(`{"url":"https:\/\/media.example\/video.mp4"}`)
	dataURL, isDataURL, err := openAIChatVideoDataURLBytes(body, gjson.GetBytes(body, "url"))
	require.NoError(t, err)
	require.False(t, isDataURL)
	require.Nil(t, dataURL)
}

func TestSendCCUpstreamRequestMaterializesAtSharedFinalEgress(t *testing.T) {
	gin.SetMode(gin.TestMode)
	store := &memoryInlineMediaStore{}
	bridge := newOpenAIChatVideoTestBridge(t, store, openAIChatVideoTestPolicy())
	scheduledCleanup := make(chan func(), 1)
	bridge.schedule = func(_ time.Duration, cleanup func()) { scheduledCleanup <- cleanup }
	upstream := &httpUpstreamRecorder{resp: &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(`{"choices":[]}`)),
	}}
	svc := &OpenAIGatewayService{httpUpstream: upstream}
	svc.SetOpenAIChatVideoBridge(bridge)

	requestCtx, cancel := context.WithCancel(context.Background())
	requestCtx = context.WithValue(requestCtx, ctxkey.UserID, int64(99))
	requestCtx = context.WithValue(requestCtx, ctxkey.ClientRequestID, "client-request-1")
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	body := bytes.Replace(testOpenAIChatVideoBody(testMP4DataURL(testMP4Bytes())), []byte("client-model"), []byte("kimi-k3"), 1)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(body)).WithContext(requestCtx)
	account := &Account{ID: 101, Name: "raw-openai-apikey", Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Concurrency: 1}

	resp, err := svc.sendCCUpstreamRequest(requestCtx, c, account, "http://upstream.example/v1/chat/completions", body, false, "sk-test", "", "")
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	require.True(t, strings.HasPrefix(gjson.GetBytes(upstream.lastBody, "messages.0.content.1.video_url.url").String(), "https://"))
	require.Equal(t, 1, store.puts)

	resp, err = svc.sendCCUpstreamRequest(requestCtx, c, account, "http://upstream.example/v1/chat/completions", body, false, "sk-test", "", "")
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	require.Equal(t, 1, store.puts, "the Gin request session must be reused by retry/failover")

	cancel()
	select {
	case <-scheduledCleanup:
		t.Fatal("request cancellation must not clean up before the handler finishes upstream draining")
	case <-time.After(20 * time.Millisecond):
	}
	svc.CleanupOpenAIChatVideoBridgeSession(c)
	select {
	case cleanup := <-scheduledCleanup:
		cleanup()
	case <-time.After(time.Second):
		t.Fatal("request cleanup was not scheduled")
	}
	require.Len(t, store.deletes, 1)
}

func TestSendCCUpstreamRequestRejectsEscapedDataURIBeforeDispatch(t *testing.T) {
	gin.SetMode(gin.TestMode)
	store := &memoryInlineMediaStore{}
	upstream := &httpUpstreamRecorder{}
	svc := &OpenAIGatewayService{httpUpstream: upstream}
	svc.SetOpenAIChatVideoBridge(newOpenAIChatVideoTestBridge(t, store, openAIChatVideoTestPolicy()))

	payload := base64.StdEncoding.EncodeToString(testMP4Bytes())
	body := []byte(fmt.Sprintf(`{"model":"kimi-k3","messages":[{"role":"user","content":[{"type":"video_url","video_url":{"url":"data\u003avideo/mp4;base64,%s"}}]}]}`, payload))
	require.True(t, json.Valid(body), string(body))
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))
	account := &Account{ID: 101, Name: "raw-openai-apikey", Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Concurrency: 1}

	resp, err := svc.sendCCUpstreamRequest(context.Background(), c, account, "http://upstream.example/v1/chat/completions", body, false, "sk-test", "", "")
	require.Nil(t, resp)
	bridgeErr, ok := AsOpenAIChatVideoBridgeError(err)
	require.True(t, ok)
	require.Equal(t, http.StatusBadRequest, bridgeErr.StatusCode)
	require.Empty(t, upstream.requests, "escaped Data URI must never reach the upstream")
	require.Zero(t, store.puts)
}

func TestOpenAIChatVideoBridgeCapacityRejectsLocallyWithRetryAfter(t *testing.T) {
	store := &memoryInlineMediaStore{}
	policy := openAIChatVideoTestPolicy()
	policy.Capacity = MediaBridgeCapacitySettings{
		MaxInflightRequests: 1,
		AdmissionWaitMS:     0,
		DefaultTenantWeight: 1,
	}
	bridge := newOpenAIChatVideoTestBridge(t, store, policy)
	occupied, err := bridge.capacity.Acquire(context.Background(), policy.Capacity, 77, 1)
	require.NoError(t, err)
	defer occupied.Release()

	session, err := bridge.NewSession(context.Background(), OpenAIChatVideoBridgeRequest{
		TenantID:         77,
		UpstreamProtocol: MediaBridgeProtocolOpenAIChatCompletions,
	})
	require.NoError(t, err)
	ctx := WithOpenAIChatVideoBridgeSession(context.Background(), session)
	_, _, err = MaterializeOpenAIChatVideoDataURLs(
		ctx,
		&Account{ID: 7},
		"kimi-k3",
		testOpenAIChatVideoBody(testMP4DataURL(testMP4Bytes())),
	)
	bridgeErr, ok := AsOpenAIChatVideoBridgeError(err)
	require.True(t, ok)
	require.Equal(t, http.StatusTooManyRequests, bridgeErr.StatusCode)
	require.Equal(t, "rate_limit_error", bridgeErr.Type)
	require.Positive(t, bridgeErr.RetryAfter)
	require.Zero(t, store.puts)
}

func TestOpenAIChatVideoBridgeImpossibleInflightByteLimitIs413(t *testing.T) {
	store := &memoryInlineMediaStore{}
	policy := openAIChatVideoTestPolicy()
	policy.Capacity = MediaBridgeCapacitySettings{
		MaxInflightDecodedBytes: 10,
		DefaultTenantWeight:     1,
	}
	bridge := newOpenAIChatVideoTestBridge(t, store, policy)
	session, err := bridge.NewSession(context.Background(), OpenAIChatVideoBridgeRequest{
		TenantID:         77,
		UpstreamProtocol: MediaBridgeProtocolOpenAIChatCompletions,
	})
	require.NoError(t, err)
	_, _, err = MaterializeOpenAIChatVideoDataURLs(
		WithOpenAIChatVideoBridgeSession(context.Background(), session),
		&Account{ID: 7},
		"kimi-k3",
		testOpenAIChatVideoBody(testMP4DataURL(testMP4Bytes())),
	)
	bridgeErr, ok := AsOpenAIChatVideoBridgeError(err)
	require.True(t, ok)
	require.Equal(t, http.StatusRequestEntityTooLarge, bridgeErr.StatusCode)
	require.Zero(t, store.puts)
}

func TestOpenAIChatVideoBridgeReleasesCapacityAfterStreamingPut(t *testing.T) {
	store := &memoryInlineMediaStore{}
	policy := openAIChatVideoTestPolicy()
	policy.Capacity = MediaBridgeCapacitySettings{
		MaxInflightRequests: 1,
		DefaultTenantWeight: 1,
	}
	bridge := newOpenAIChatVideoTestBridge(t, store, policy)
	session, err := bridge.NewSession(context.Background(), OpenAIChatVideoBridgeRequest{
		TenantID:         88,
		UpstreamProtocol: MediaBridgeProtocolOpenAIChatCompletions,
	})
	require.NoError(t, err)
	ctx := WithOpenAIChatVideoBridgeSession(context.Background(), session)
	_, changed, err := MaterializeOpenAIChatVideoDataURLs(
		ctx,
		&Account{ID: 7},
		"kimi-k3",
		testOpenAIChatVideoBody(testMP4DataURL(testMP4Bytes())),
	)
	require.NoError(t, err)
	require.True(t, changed)
	require.Equal(t, MediaBridgeCapacityUsage{}, bridge.capacity.Snapshot(88).Global)
}

func TestOpenAIChatVideoBridgeR2CircuitRejectsBeforeSecondUpload(t *testing.T) {
	store := &memoryInlineMediaStore{putErr: errors.New("r2 unavailable")}
	bridge := newOpenAIChatVideoTestBridge(t, store, openAIChatVideoTestPolicy())
	bridge.SetR2Circuit(NewMediaBridgeR2Circuit(newMediaBridgeR2SettingsStub()))
	body := testOpenAIChatVideoBody(testMP4DataURL(testMP4Bytes()))

	firstSession, err := bridge.NewSession(context.Background(), OpenAIChatVideoBridgeRequest{
		TenantID:         91,
		UpstreamProtocol: MediaBridgeProtocolOpenAIChatCompletions,
	})
	require.NoError(t, err)
	_, _, err = MaterializeOpenAIChatVideoDataURLs(
		WithOpenAIChatVideoBridgeSession(context.Background(), firstSession),
		&Account{ID: 7},
		"kimi-k3",
		body,
	)
	firstErr, ok := AsOpenAIChatVideoBridgeError(err)
	require.True(t, ok)
	require.Equal(t, http.StatusServiceUnavailable, firstErr.StatusCode)
	require.Equal(t, 1, store.puts)

	secondSession, err := bridge.NewSession(context.Background(), OpenAIChatVideoBridgeRequest{
		TenantID:         91,
		UpstreamProtocol: MediaBridgeProtocolOpenAIChatCompletions,
	})
	require.NoError(t, err)
	_, _, err = MaterializeOpenAIChatVideoDataURLs(
		WithOpenAIChatVideoBridgeSession(context.Background(), secondSession),
		&Account{ID: 7},
		"kimi-k3",
		body,
	)
	secondErr, ok := AsOpenAIChatVideoBridgeError(err)
	require.True(t, ok)
	require.Equal(t, http.StatusServiceUnavailable, secondErr.StatusCode)
	require.Equal(t, "temporary_media_error", secondErr.Type)
	require.Positive(t, secondErr.RetryAfter)
	require.Equal(t, 1, store.puts, "open circuit must reject before another R2 upload")
}

func TestOpenAIChatVideoBridgeUploadTimeoutReleasesCapacityAndTripsCircuit(t *testing.T) {
	store := &blockingInlineMediaStore{memoryInlineMediaStore: &memoryInlineMediaStore{}}
	policy := openAIChatVideoTestPolicy()
	policy.UploadTimeout = 10 * time.Millisecond
	bridge := newOpenAIChatVideoTestBridge(t, store, policy)
	circuit := NewMediaBridgeR2Circuit(newMediaBridgeR2SettingsStub())
	bridge.SetR2Circuit(circuit)
	session, err := bridge.NewSession(context.Background(), OpenAIChatVideoBridgeRequest{
		TenantID:         92,
		UpstreamProtocol: MediaBridgeProtocolOpenAIChatCompletions,
	})
	require.NoError(t, err)
	_, _, err = MaterializeOpenAIChatVideoDataURLs(
		WithOpenAIChatVideoBridgeSession(context.Background(), session),
		&Account{ID: 7},
		"kimi-k3",
		testOpenAIChatVideoBody(testMP4DataURL(testMP4Bytes())),
	)
	bridgeErr, ok := AsOpenAIChatVideoBridgeError(err)
	require.True(t, ok)
	require.Equal(t, http.StatusServiceUnavailable, bridgeErr.StatusCode)
	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.Equal(t, MediaBridgeCapacityUsage{}, bridge.capacity.Snapshot(92).Global)
	require.Equal(t, MediaBridgeR2CircuitOpen, circuit.Snapshot().State)
}

func TestOpenAIChatVideoBridgeSnapshotsHotSwappedStorePerSession(t *testing.T) {
	firstStore := &memoryInlineMediaStore{}
	secondStore := &memoryInlineMediaStore{}
	resolver := &mutableInlineMediaStoreResolver{store: firstStore}
	bridge := newOpenAIChatVideoTestBridge(t, NewUnavailableInlineMediaStore(), openAIChatVideoTestPolicy())
	bridge.SetStoreResolver(resolver)

	firstSession, err := bridge.NewSession(context.Background(), OpenAIChatVideoBridgeRequest{
		TenantID:         1,
		UpstreamProtocol: MediaBridgeProtocolOpenAIChatCompletions,
	})
	require.NoError(t, err)
	body := testOpenAIChatVideoBody(testMP4DataURL(testMP4Bytes()))
	_, changed, err := MaterializeOpenAIChatVideoDataURLs(
		WithOpenAIChatVideoBridgeSession(context.Background(), firstSession),
		&Account{ID: 7},
		"kimi-k3",
		body,
	)
	require.NoError(t, err)
	require.True(t, changed)

	resolver.set(secondStore, nil)
	secondSession, err := bridge.NewSession(context.Background(), OpenAIChatVideoBridgeRequest{
		TenantID:         2,
		UpstreamProtocol: MediaBridgeProtocolOpenAIChatCompletions,
	})
	require.NoError(t, err)
	_, changed, err = MaterializeOpenAIChatVideoDataURLs(
		WithOpenAIChatVideoBridgeSession(context.Background(), secondSession),
		&Account{ID: 7},
		"kimi-k3",
		body,
	)
	require.NoError(t, err)
	require.True(t, changed)
	require.Equal(t, 1, firstStore.puts)
	require.Equal(t, 1, secondStore.puts)
	require.NotContains(t, firstStore.putKey, "media-bridge/media-bridge")
}

func TestOpenAIChatVideoBridgeFailsClosedWhenRuntimeStoreUnavailable(t *testing.T) {
	resolver := &mutableInlineMediaStoreResolver{err: ErrMediaBridgeStorageNotConfigured}
	bridge := newOpenAIChatVideoTestBridge(t, NewUnavailableInlineMediaStore(), openAIChatVideoTestPolicy())
	bridge.SetStoreResolver(resolver)

	session, err := bridge.NewSession(context.Background(), OpenAIChatVideoBridgeRequest{
		TenantID:         1,
		UpstreamProtocol: MediaBridgeProtocolOpenAIChatCompletions,
	})
	require.NoError(t, err)
	require.NotNil(t, session)
	_, _, err = MaterializeOpenAIChatVideoDataURLs(
		WithOpenAIChatVideoBridgeSession(context.Background(), session),
		&Account{ID: 7},
		"kimi-k3",
		testOpenAIChatVideoBody(testMP4DataURL(testMP4Bytes())),
	)
	bridgeErr, ok := AsOpenAIChatVideoBridgeError(err)
	require.True(t, ok)
	require.Equal(t, http.StatusServiceUnavailable, bridgeErr.StatusCode)
}

func TestOpenAIChatVideoBridgeDoesNotResolveStorageWithoutEligibleDataURI(t *testing.T) {
	resolver := &mutableInlineMediaStoreResolver{err: ErrMediaBridgeStorageNotConfigured}
	bridge := newOpenAIChatVideoTestBridge(t, NewUnavailableInlineMediaStore(), openAIChatVideoTestPolicy())
	bridge.SetStoreResolver(resolver)
	session, err := bridge.NewSession(context.Background(), OpenAIChatVideoBridgeRequest{
		TenantID:         1,
		UpstreamProtocol: MediaBridgeProtocolOpenAIChatCompletions,
	})
	require.NoError(t, err)

	urlBody := testOpenAIChatVideoBody("https://assets.example.test/video.mp4")
	untouched, changed, err := MaterializeOpenAIChatVideoDataURLs(
		WithOpenAIChatVideoBridgeSession(context.Background(), session),
		&Account{ID: 7},
		"kimi-k3",
		urlBody,
	)
	require.NoError(t, err)
	require.False(t, changed)
	require.Equal(t, urlBody, untouched)
}
