package service

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestExtractOpenAIUsageFromJSONBytes_DeepSeekCacheBuckets(t *testing.T) {
	tests := []struct {
		name          string
		body          string
		wantTotal     int
		wantCacheRead int
		wantOrdinary  int
	}{
		{
			name:          "prompt total remains total and hit is not added twice",
			body:          `{"usage":{"prompt_tokens":1200,"completion_tokens":30,"total_tokens":1230,"prompt_cache_hit_tokens":800,"prompt_cache_miss_tokens":400}}`,
			wantTotal:     1200,
			wantCacheRead: 800,
			wantOrdinary:  400,
		},
		{
			name:          "missing prompt total is reconstructed from hit and miss",
			body:          `{"usage":{"completion_tokens":30,"prompt_cache_hit_tokens":800,"prompt_cache_miss_tokens":400}}`,
			wantTotal:     1200,
			wantCacheRead: 800,
			wantOrdinary:  400,
		},
		{
			name:          "explicit zero hit overrides loose cache read alias",
			body:          `{"usage":{"prompt_tokens":400,"completion_tokens":30,"prompt_cache_hit_tokens":0,"prompt_cache_miss_tokens":400,"cache_read_input_tokens":99}}`,
			wantTotal:     400,
			wantCacheRead: 0,
			wantOrdinary:  400,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			usage, ok := extractOpenAIUsageFromJSONBytes([]byte(tt.body))
			require.True(t, ok)
			require.Equal(t, tt.wantTotal, usage.InputTokens, "InputTokens must be the total prompt count")
			require.Equal(t, 30, usage.OutputTokens)
			require.Equal(t, tt.wantCacheRead, usage.CacheReadInputTokens)
			require.Equal(t, tt.wantOrdinary,
				max(usage.InputTokens-usage.CacheReadInputTokens-usage.CacheCreationInputTokens, 0),
				"the total must split into mutually-exclusive ordinary/read/write billing buckets",
			)
		})
	}
}

func TestDeepSeekCacheUsage_StreamingPaths(t *testing.T) {
	t.Run("Responses SSE terminal event", func(t *testing.T) {
		usage := &OpenAIUsage{}
		(&OpenAIGatewayService{}).parseSSEUsage(
			`{"type":"response.completed","response":{"usage":{"prompt_tokens":1200,"completion_tokens":30,"prompt_cache_hit_tokens":800,"prompt_cache_miss_tokens":400}}}`,
			usage,
		)

		require.Equal(t, 1200, usage.InputTokens)
		require.Equal(t, 30, usage.OutputTokens)
		require.Equal(t, 800, usage.CacheReadInputTokens)
		require.Equal(t, 400, usage.InputTokens-usage.CacheReadInputTokens)
	})

	t.Run("Chat Completions include usage chunk", func(t *testing.T) {
		usage := extractCCStreamUsage(
			`{"choices":[],"usage":{"prompt_tokens":1200,"completion_tokens":30,"prompt_cache_hit_tokens":800,"prompt_cache_miss_tokens":400}}`,
		)

		require.NotNil(t, usage)
		require.Equal(t, 1200, usage.InputTokens)
		require.Equal(t, 30, usage.OutputTokens)
		require.Equal(t, 800, usage.CacheReadInputTokens)
		require.Equal(t, 400, usage.InputTokens-usage.CacheReadInputTokens)
	})
}
