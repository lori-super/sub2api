package apicompat

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

const deepSeekChatUsageJSON = `{
	"prompt_tokens":1200,
	"completion_tokens":30,
	"total_tokens":1230,
	"prompt_cache_hit_tokens":800,
	"prompt_cache_miss_tokens":400
}`

func TestDeepSeekChatUsage_NonStreamingBridgesUseExclusiveBuckets(t *testing.T) {
	var usage ChatUsage
	require.NoError(t, json.Unmarshal([]byte(deepSeekChatUsageJSON), &usage))
	require.NotNil(t, usage.PromptCacheHitTokens)
	require.NotNil(t, usage.PromptCacheMissTokens)
	require.Equal(t, 800, *usage.PromptCacheHitTokens)
	require.Equal(t, 400, *usage.PromptCacheMissTokens)

	responsesUsage := ChatUsageToResponsesUsage(&usage)
	require.NotNil(t, responsesUsage)
	require.Equal(t, 1200, responsesUsage.InputTokens, "Responses input_tokens is the total prompt count")
	require.Equal(t, 30, responsesUsage.OutputTokens)
	require.Equal(t, 1230, responsesUsage.TotalTokens)
	require.NotNil(t, responsesUsage.InputTokensDetails)
	require.Equal(t, 800, responsesUsage.InputTokensDetails.CachedTokens)
	require.Equal(t, 400, responsesUsage.InputTokens-responsesUsage.InputTokensDetails.CachedTokens)

	// Responses output uses the canonical cached_tokens detail; the
	// DeepSeek-only aliases stay on the decoded Chat usage structure.
	wire, err := json.Marshal(responsesUsage)
	require.NoError(t, err)
	require.Contains(t, string(wire), `"cached_tokens":800`)
	require.NotContains(t, string(wire), "prompt_cache_hit_tokens")
	require.NotContains(t, string(wire), "prompt_cache_miss_tokens")

	anthropicUsage := chatUsageToAnthropicUsage(&usage)
	require.Equal(t, 400, anthropicUsage.InputTokens, "Anthropic input_tokens is the uncached/miss bucket")
	require.Equal(t, 800, anthropicUsage.CacheReadInputTokens)
	require.Equal(t, 30, anthropicUsage.OutputTokens)
	require.Equal(t, 1200, anthropicUsage.InputTokens+anthropicUsage.CacheReadInputTokens)
}

func TestDeepSeekChatUsage_MissingPromptTotalIsReconstructed(t *testing.T) {
	var usage ChatUsage
	require.NoError(t, json.Unmarshal([]byte(`{
		"completion_tokens":30,
		"prompt_cache_hit_tokens":800,
		"prompt_cache_miss_tokens":400
	}`), &usage))

	responsesUsage := ChatUsageToResponsesUsage(&usage)
	require.Equal(t, 1200, responsesUsage.InputTokens)
	require.Equal(t, 1230, responsesUsage.TotalTokens)
	require.Equal(t, 800, responsesUsage.InputTokensDetails.CachedTokens)

	anthropicUsage := chatUsageToAnthropicUsage(&usage)
	require.Equal(t, 400, anthropicUsage.InputTokens)
	require.Equal(t, 800, anthropicUsage.CacheReadInputTokens)
}

func TestDeepSeekChatUsage_StreamingBridgesPreserveCacheHit(t *testing.T) {
	payload := `{"id":"chatcmpl-ds","model":"deepseek-chat","choices":[],"usage":` + deepSeekChatUsageJSON + `}`
	var chunk ChatCompletionsChunk
	require.NoError(t, json.Unmarshal([]byte(payload), &chunk))

	responsesState := NewChatCompletionsToResponsesStreamState("deepseek-chat")
	ChatCompletionsChunkToResponsesEvents(&chunk, responsesState)
	require.NotNil(t, responsesState.Usage)
	require.Equal(t, 1200, responsesState.Usage.InputTokens)
	require.NotNil(t, responsesState.Usage.InputTokensDetails)
	require.Equal(t, 800, responsesState.Usage.InputTokensDetails.CachedTokens)

	anthropicState := NewChatCompletionsToAnthropicStreamState("deepseek-chat")
	ChatCompletionsChunkToAnthropicEvents(&chunk, anthropicState)
	require.Equal(t, 400, anthropicState.InputTokens)
	require.Equal(t, 800, anthropicState.CacheReadInputTokens)
	require.Equal(t, 30, anthropicState.OutputTokens)
}
