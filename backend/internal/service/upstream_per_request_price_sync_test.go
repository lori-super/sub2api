package service

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/domain"
	"github.com/stretchr/testify/require"
)

type perRequestPricePageStub struct {
	mu     sync.Mutex
	prices map[string]domain.UpstreamPriceVector
	err    error
	calls  int
}

func (f *perRequestPricePageStub) FetchPerRequestPrices(context.Context) (map[string]domain.UpstreamPriceVector, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	return f.prices, f.err
}

func (f *perRequestPricePageStub) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

type perRequestPriceSyncRepoStub struct {
	*activeProbeTestRepository
	cfg       domain.UpstreamPriceMonitorConfig
	result    *UpstreamPerRequestPriceSyncResult
	err       error
	calls     int
	channels  []int64
	updates   []UpstreamPerRequestPriceUpdate
	applyRuns int
}

func (r *perRequestPriceSyncRepoStub) GetConfig(context.Context) (*domain.UpstreamPriceMonitorConfig, error) {
	cfg := r.cfg
	cfg.ChannelIDs = append([]int64(nil), r.cfg.ChannelIDs...)
	cfg.PerRequestModels = append([]string(nil), r.cfg.PerRequestModels...)
	return &cfg, nil
}

func (r *perRequestPriceSyncRepoStub) SyncPerRequestPrices(
	_ context.Context,
	channelIDs []int64,
	updates []UpstreamPerRequestPriceUpdate,
) (*UpstreamPerRequestPriceSyncResult, error) {
	r.calls++
	r.channels = append([]int64(nil), channelIDs...)
	r.updates = append([]UpstreamPerRequestPriceUpdate(nil), updates...)
	if r.result == nil {
		return nil, r.err
	}
	copy := *r.result
	return &copy, r.err
}

func (r *perRequestPriceSyncRepoStub) ApplyRun(
	context.Context, int64, string, []int64, []int64, int, int, time.Time, int64,
) error {
	r.applyRuns++
	return nil
}

type perRequestCacheInvalidatorStub struct{ calls int }

func (i *perRequestCacheInvalidatorStub) InvalidatePricingCache() { i.calls++ }

func TestSyncPerRequestPricesUsesOnlyFirstPageTierAndInvalidatesChannelCache(t *testing.T) {
	first, ignoredMiddle, ignoredHigh := 0.01, 9.0, 11.0
	cfg := domain.DefaultUpstreamPriceMonitorConfig()
	cfg.Enabled = false
	cfg.Mode = domain.UpstreamPriceMonitorModeObserve
	cfg.ChannelIDs = []int64{2, 1, 2}
	cfg.PerRequestModels = []string{"Model-B", "model-a"}
	repo := &perRequestPriceSyncRepoStub{
		activeProbeTestRepository: &activeProbeTestRepository{},
		cfg:                       cfg,
		result: &UpstreamPerRequestPriceSyncResult{
			Models: 2, ChangedModels: 2, ChangedChannelRows: 2,
		},
	}
	fetcher := &perRequestPricePageStub{prices: map[string]domain.UpstreamPriceVector{
		"MODEL-A": {PerRequestLTE256K: &first, PerRequest256K512K: &ignoredMiddle, PerRequestGT512K: &ignoredHigh},
		"model-b": {PerRequestLTE256K: &first},
	}}
	invalidator := &perRequestCacheInvalidatorStub{}
	svc := NewUpstreamPriceMonitorService(repo, nil, nil)
	svc.SetPricePageFetcher(fetcher)
	svc.SetPricingCacheInvalidator(invalidator)

	result, err := svc.SyncPerRequestPrices(context.Background())
	require.NoError(t, err)
	require.Equal(t, 2, result.Models)
	require.Equal(t, []int64{1, 2}, repo.channels)
	require.Len(t, repo.updates, 2)
	require.Equal(t, "model-a", repo.updates[0].ModelName)
	for _, update := range repo.updates {
		require.InDelta(t, 0.012, update.BasePrice, 1e-12)
		require.InDelta(t, 0.018, update.MiddlePrice, 1e-12)
		require.InDelta(t, 0.024, update.HighPrice, 1e-12)
	}
	require.Equal(t, 1, invalidator.calls)
	require.Zero(t, repo.applyRuns, "public-page sync must never enter token ApplyRun")
}

func TestSyncPerRequestPricesFailsClosedBeforeMutationWhenPageModelIsMissing(t *testing.T) {
	cfg := domain.DefaultUpstreamPriceMonitorConfig()
	cfg.ChannelIDs = []int64{2}
	cfg.PerRequestModels = []string{"expected-model"}
	repo := &perRequestPriceSyncRepoStub{
		activeProbeTestRepository: &activeProbeTestRepository{}, cfg: cfg,
		result: &UpstreamPerRequestPriceSyncResult{Models: 1},
	}
	fetcher := &perRequestPricePageStub{prices: map[string]domain.UpstreamPriceVector{}}
	svc := NewUpstreamPriceMonitorService(repo, nil, nil)
	svc.SetPricePageFetcher(fetcher)

	_, err := svc.SyncPerRequestPrices(context.Background())
	require.ErrorIs(t, err, ErrUpstreamPriceMonitorInvalidConfig)
	require.Zero(t, repo.calls)
}

func TestUpstreamPriceMonitorRunnerSchedulesPerRequestSyncIndependently(t *testing.T) {
	first := 0.01
	cfg := domain.DefaultUpstreamPriceMonitorConfig()
	cfg.Enabled = false // token scheduler is off; public-page sync remains on.
	cfg.ChannelIDs = []int64{2}
	cfg.PerRequestModels = []string{"model-a"}
	repo := &perRequestPriceSyncRepoStub{
		activeProbeTestRepository: &activeProbeTestRepository{}, cfg: cfg,
		result: &UpstreamPerRequestPriceSyncResult{Models: 1},
	}
	fetcher := &perRequestPricePageStub{prices: map[string]domain.UpstreamPriceVector{
		"model-a": {PerRequestLTE256K: &first},
	}}
	svc := NewUpstreamPriceMonitorService(repo, nil, nil)
	svc.SetPricePageFetcher(fetcher)
	runner := NewUpstreamPriceMonitorRunner(svc)
	defer runner.cancel()
	clock := time.Date(2026, 8, 31, 2, 0, 0, 0, time.UTC)
	runner.now = func() time.Time { return clock }

	runner.runIfDue()
	require.Equal(t, 1, fetcher.callCount())
	require.Zero(t, repo.applyRuns)
	clock = clock.Add(23*time.Hour + 59*time.Minute)
	runner.runIfDue()
	require.Equal(t, 1, fetcher.callCount())
	clock = clock.Add(time.Minute)
	runner.runIfDue()
	require.Equal(t, 2, fetcher.callCount())
}

func TestUpstreamPriceMonitorRunnerRetriesPerRequestSyncAfterOneHour(t *testing.T) {
	cfg := domain.DefaultUpstreamPriceMonitorConfig()
	cfg.ChannelIDs = []int64{2}
	cfg.PerRequestModels = []string{"model-a"}
	repo := &perRequestPriceSyncRepoStub{
		activeProbeTestRepository: &activeProbeTestRepository{}, cfg: cfg,
		result: &UpstreamPerRequestPriceSyncResult{Models: 1},
	}
	fetcher := &perRequestPricePageStub{err: errors.New("temporary page failure")}
	svc := NewUpstreamPriceMonitorService(repo, nil, nil)
	svc.SetPricePageFetcher(fetcher)
	runner := NewUpstreamPriceMonitorRunner(svc)
	defer runner.cancel()
	clock := time.Date(2026, 8, 31, 2, 0, 0, 0, time.UTC)
	runner.now = func() time.Time { return clock }

	runner.runPerRequestIfDue()
	require.Equal(t, 1, fetcher.callCount())
	clock = clock.Add(59 * time.Minute)
	runner.runPerRequestIfDue()
	require.Equal(t, 1, fetcher.callCount())
	clock = clock.Add(time.Minute)
	runner.runPerRequestIfDue()
	require.Equal(t, 2, fetcher.callCount())
}
