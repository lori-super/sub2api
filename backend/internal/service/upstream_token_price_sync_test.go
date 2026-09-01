package service

import (
	"context"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/domain"
	"github.com/stretchr/testify/require"
)

func TestSyncTokenPricesUsesConfiguredPageModelsAndFixedMarkup(t *testing.T) {
	officialInput, input, output, cacheRead, cacheWrite := 1.6, 0.1075, 0.3159, 0.0107, 0.0007
	foreignInput, foreignOutput := 1.0, 2.0
	cfg := domain.DefaultUpstreamPriceMonitorConfig()
	cfg.ChannelIDs = []int64{9, 8, 9}
	cfg.DomesticModels = []string{"qwen3.8-flash", "deepseek-v4-flash-0731"}
	repo := &perRequestPriceSyncRepoStub{
		activeProbeTestRepository: &activeProbeTestRepository{}, cfg: cfg,
		tokenResult: &UpstreamTokenPriceSyncResult{
			Models: 2, ChangedModels: 2, ChangedChannelRows: 1, ChangedIntervalRows: 1, ChangedDisplayRows: 2,
		},
	}
	fetcher := &perRequestPricePageStub{tokenPrices: map[string]UpstreamTokenDisplayPrice{
		"deepseek-v4-flash-0731": {
			ModelName: "deepseek-v4-flash-0731", OfficialInput: &officialInput,
			SellingInput: &input, SellingOutput: &output, SellingCacheRead: &cacheRead,
		},
		"qwen3.8-flash": {
			ModelName: "qwen3.8-flash", SellingInput: &input, SellingOutput: &output,
			SellingCacheWrite: &cacheWrite, SellingCacheRead: &cacheRead,
		},
		"gpt-5.6-sol": {
			ModelName: "gpt-5.6-sol", SellingInput: &foreignInput, SellingOutput: &foreignOutput,
		},
	}}
	invalidator := &perRequestCacheInvalidatorStub{}
	svc := NewUpstreamPriceMonitorService(repo, nil, nil)
	svc.SetPricePageFetcher(fetcher)
	svc.SetPricingCacheInvalidator(invalidator)

	result, err := svc.SyncTokenPrices(context.Background())
	require.NoError(t, err)
	require.Equal(t, 3, result.SourceModels)
	require.Equal(t, 2, result.Models)
	require.Equal(t, 2, result.MatchedModels)
	require.Equal(t, 2, result.UpdatedModels)
	require.Equal(t, []int64{8, 9}, repo.channels)
	require.Len(t, repo.tokenUpdates, 2, "foreign page rows are not part of the configured managed channel scope")
	require.Equal(t, "deepseek-v4-flash-0731", repo.tokenUpdates[0].ModelName)
	require.Equal(t, "qwen3.8-flash", repo.tokenUpdates[1].ModelName)
	require.Equal(t, "deepseek", repo.tokenUpdates[0].Provider)
	require.Equal(t, "qwen", repo.tokenUpdates[1].Provider)
	require.InDelta(t, input*1.2, *repo.tokenUpdates[0].InputPerMillion, 1e-12)
	require.InDelta(t, output*1.2, *repo.tokenUpdates[0].OutputPerMillion, 1e-12)
	require.InDelta(t, cacheWrite*1.2, *repo.tokenUpdates[1].CacheWritePerMillion, 1e-12)
	require.InDelta(t, officialInput, *repo.tokenUpdates[0].OfficialInput, 1e-12)
	require.Equal(t, 1, invalidator.calls)
	require.Zero(t, repo.applyRuns)
}

func TestSyncTokenPricesFailsClosedWhenConfiguredPageModelIsMissing(t *testing.T) {
	input, output := 0.1, 0.2
	cfg := domain.DefaultUpstreamPriceMonitorConfig()
	cfg.ChannelIDs = []int64{8}
	cfg.DomesticModels = []string{"deepseek-v4-flash-0731", "qwen3.8-flash"}
	repo := &perRequestPriceSyncRepoStub{
		activeProbeTestRepository: &activeProbeTestRepository{}, cfg: cfg,
		tokenResult: &UpstreamTokenPriceSyncResult{Models: 2},
	}
	fetcher := &perRequestPricePageStub{tokenPrices: map[string]UpstreamTokenDisplayPrice{
		"deepseek-v4-flash-0731": {
			ModelName: "deepseek-v4-flash-0731", SellingInput: &input, SellingOutput: &output,
		},
	}}
	svc := NewUpstreamPriceMonitorService(repo, nil, nil)
	svc.SetPricePageFetcher(fetcher)

	_, err := svc.SyncTokenPrices(context.Background())
	require.ErrorIs(t, err, ErrUpstreamPriceMonitorInvalidConfig)
	require.Zero(t, repo.tokenCalls, "an incomplete public page must fail before any mutation")
}
