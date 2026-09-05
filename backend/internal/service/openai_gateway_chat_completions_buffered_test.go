//go:build unit

package service

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func bufferedChatFixture(t *testing.T, events string, enabled bool) (*OpenAIForwardResult, error, *httptest.ResponseRecorder, *httpUpstreamRecorder) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	body := []byte(`{"model":"deepseek-v4-flash-0731","messages":[{"role":"user","content":"hello"}],"stream":false,"max_tokens":1234}`)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))
	upstream := &httpUpstreamRecorder{resp: &http.Response{StatusCode: 200,
		Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(events))}}
	svc := &OpenAIGatewayService{cfg: rawChatCompletionsTestConfig(), httpUpstream: upstream}
	account := rawChatCompletionsTestAccount()
	if enabled {
		account.Extra = map[string]any{"openai_chat_nonstream_via_stream_models": []any{"deepseek-v4-flash-0731"}}
	}
	result, err := svc.forwardAsRawChatCompletions(context.Background(), c, account, body, "")
	return result, err, rec, upstream
}

const bufferedChatEvents = `data: {"id":"chatcmpl-buffered","object":"chat.completion.chunk","created":123,"model":"deepseek-v4-flash-0731","choices":[{"index":0,"delta":{"role":"assistant","reasoning_content":"think "},"finish_reason":null}]}

data: {"id":"chatcmpl-buffered","model":"deepseek-v4-flash-0731","choices":[{"index":0,"delta":{"reasoning_content":"carefully","content":"hello "},"finish_reason":null}]}

data: {"id":"chatcmpl-buffered","model":"deepseek-v4-flash-0731","choices":[{"index":0,"delta":{"content":"world"},"finish_reason":"stop"}]}

data: {"id":"chatcmpl-buffered","model":"deepseek-v4-flash-0731","choices":[],"usage":{"prompt_tokens":66241,"completion_tokens":13,"total_tokens":66254,"prompt_tokens_details":{"cached_tokens":32000},"completion_tokens_details":{"reasoning_tokens":8}}}

data: [DONE]

`

func TestRawChatBufferedPreservesJSONAndUsage(t *testing.T) {
	result, err, rec, upstream := bufferedChatFixture(t, bufferedChatEvents, true)
	require.NoError(t, err)
	require.True(t, gjson.GetBytes(upstream.lastBody, "stream").Bool(), "upstream must stream")
	require.True(t, gjson.GetBytes(upstream.lastBody, "stream_options.include_usage").Bool())
	require.Equal(t, int64(1234), gjson.GetBytes(upstream.lastBody, "max_tokens").Int())
	require.Equal(t, "text/event-stream", upstream.lastReq.Header.Get("Accept"))
	require.Equal(t, 200, rec.Code)
	require.Contains(t, rec.Header().Get("Content-Type"), "application/json")
	require.True(t, gjson.Valid(rec.Body.String()))
	require.Equal(t, "chat.completion", gjson.Get(rec.Body.String(), "object").String())
	require.Equal(t, "hello world", gjson.Get(rec.Body.String(), "choices.0.message.content").String())
	require.Equal(t, "think carefully", gjson.Get(rec.Body.String(), "choices.0.message.reasoning_content").String())
	require.Equal(t, "stop", gjson.Get(rec.Body.String(), "choices.0.finish_reason").String())
	require.Equal(t, int64(8), gjson.Get(rec.Body.String(), "usage.completion_tokens_details.reasoning_tokens").Int())
	require.False(t, result.Stream)
	require.Equal(t, 66241, result.Usage.InputTokens)
	require.Equal(t, 32000, result.Usage.CacheReadInputTokens)
	require.Equal(t, 13, result.Usage.OutputTokens)
}

func TestRawChatBufferedRejectsIncompleteOrInvalidStream(t *testing.T) {
	for name, events := range map[string]string{
		"no done":          strings.ReplaceAll(bufferedChatEvents, "data: [DONE]", ""),
		"no finish":        strings.ReplaceAll(bufferedChatEvents, `"finish_reason":"stop"`, `"finish_reason":null`),
		"no usage":         strings.ReplaceAll(bufferedChatEvents, `"usage":`, `"ignored_usage":`),
		"malformed":        "data: {broken}\n\ndata: [DONE]\n\n",
		"invalid identity": strings.ReplaceAll(bufferedChatEvents, `"id":"chatcmpl-buffered"`, `"id":{}`),
		"invalid delta":    strings.Replace(bufferedChatEvents, `"delta":{"role":"assistant","reasoning_content":"think "}`, `"delta":"bad"`, 1),
		"invalid content":  strings.Replace(bufferedChatEvents, `"content":"hello "`, `"content":42`, 1),
		"upstream error":   "data: {\"error\":{\"message\":\"context window exceeded\",\"code\":\"context_length_exceeded\"}}\n\ndata: [DONE]\n\n",
	} {
		t.Run(name, func(t *testing.T) {
			result, err, rec, _ := bufferedChatFixture(t, events, true)
			require.Error(t, err)
			require.Nil(t, result)
			require.GreaterOrEqual(t, rec.Code, 400)
			require.NotContains(t, rec.Body.String(), "data:")
		})
	}
}

func TestRawChatBufferedPreservesContextErrorCode(t *testing.T) {
	events := "data: {\"error\":{\"code\":\"context_length_exceeded\",\"message\":\"请求超出模型限制\"}}\n\ndata: [DONE]\n\n"
	result, err, rec, _ := bufferedChatFixture(t, events, true)
	require.Error(t, err)
	require.Nil(t, result)
	require.Equal(t, 400, rec.Code)
	require.Equal(t, "invalid_request_error", gjson.Get(rec.Body.String(), "error.type").String())
}

func TestRawChatBufferedScope(t *testing.T) {
	account := rawChatCompletionsTestAccount()
	account.Extra = map[string]any{rawChatBufferedModelsKey: []any{"deepseek-v4-flash-0731"}}
	require.True(t, shouldBufferRawChatStream(account, "deepseek-v4-flash-0731", false))
	require.False(t, shouldBufferRawChatStream(account, "deepseek-v4-flash-0731", true))
	require.False(t, shouldBufferRawChatStream(account, "kimi-k3", false))
	require.False(t, shouldBufferRawChatStream(nil, "deepseek-v4-flash-0731", false))
}

func TestRawChatBufferedToolFragments(t *testing.T) {
	events := `data: {"id":"tools","model":"deepseek-v4-flash-0731","choices":[{"index":0,"delta":{"tool_calls":[{"index":1,"id":"call_b","type":"function","function":{"name":"second","arguments":"{"}},{"index":0,"id":"call_a","type":"function","function":{"name":"first","arguments":"{"}}]},"finish_reason":null}]}

data: {"id":"tools","model":"deepseek-v4-flash-0731","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"x\":1}"}},{"index":1,"function":{"arguments":"}"}}]},"finish_reason":"tool_calls"}]}

data: {"id":"tools","model":"deepseek-v4-flash-0731","choices":[],"usage":{"prompt_tokens":10,"completion_tokens":4,"total_tokens":14}}

data: [DONE]

`
	_, err, rec, _ := bufferedChatFixture(t, events, true)
	require.NoError(t, err)
	require.Equal(t, "call_a", gjson.Get(rec.Body.String(), "choices.0.message.tool_calls.0.id").String())
	require.Equal(t, `{"x":1}`, gjson.Get(rec.Body.String(), "choices.0.message.tool_calls.0.function.arguments").String())
	require.Equal(t, "call_b", gjson.Get(rec.Body.String(), "choices.0.message.tool_calls.1.id").String())
	require.False(t, gjson.Get(rec.Body.String(), "choices.0.message.tool_calls.0.index").Exists())
}

func TestRawChatBufferedBodyLimit(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	cfg := rawChatCompletionsTestConfig()
	cfg.Gateway.UpstreamResponseReadMaxBytes = 100
	svc := &OpenAIGatewayService{cfg: cfg}
	_, err := svc.bufferRawChatStreamResponse(c, &http.Response{Header: http.Header{}, Body: io.NopCloser(strings.NewReader(bufferedChatEvents))}, 1)
	require.Error(t, err)
	require.Equal(t, 502, rec.Code)
	require.Contains(t, rec.Body.String(), "too large")
}

func TestRawChatBufferedPreservesLogprobs(t *testing.T) {
	events := strings.Replace(bufferedChatEvents, `"delta":{"content":"world"}`, `"logprobs":{"content":[{"token":"world","logprob":-0.1}]},"delta":{"content":"world"}`, 1)
	_, err, rec, _ := bufferedChatFixture(t, events, true)
	require.NoError(t, err)
	require.Equal(t, "world", gjson.Get(rec.Body.String(), "choices.0.logprobs.content.0.token").String())
}

func TestRawChatBufferedDoesNotChangeUnconfiguredAccount(t *testing.T) {
	jsonBody := `{"id":"ok","model":"deepseek-v4-flash-0731","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":1,"total_tokens":4}}`
	result, err, rec, upstream := bufferedChatFixture(t, jsonBody, false)
	require.NoError(t, err)
	require.False(t, gjson.GetBytes(upstream.lastBody, "stream").Bool())
	require.Equal(t, jsonBody, rec.Body.String())
	require.False(t, result.Stream)
}
