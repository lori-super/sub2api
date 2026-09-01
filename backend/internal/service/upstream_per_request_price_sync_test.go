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
	mu          sync.Mutex
	prices      map[string]domain.UpstreamPriceVector
	err         error
	calls       int
	tokenPrices map[string]UpstreamTokenDisplayPrice
	tokenErr    error
	tokenCalls  int
}

func (f *perRequestPricePageStub) FetchTokenPrices(context.Context) (map[string]UpstreamTokenDisplayPrice, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.tokenCalls++
	return f.tokenPrices, f.tokenErr
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

func (f *perRequestPricePageStub) tokenCallCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.tokenCalls
}

type perRequestPriceSyncRepoStub struct {
	*activeProbeTestRepository
	cfg          domain.UpstreamPriceMonitorConfig
	result       *UpstreamPerRequestPriceSyncResult
	err          error
	calls        int
	channels     []int64
	updates      []UpstreamPerRequestPriceUpdate
	tokenResult  *UpstreamTokenPriceSyncResult
	tokenErr     error
	tokenCalls   int
	tokenUpdates []UpstreamTokenPriceUpdate
	applyRuns    int
}

func (r *perRequestPriceSyncRepoStub) SyncTokenPrices(
	_ context.Context,
	channelIDs []int64,
	updates []UpstreamTokenPriceUpdate,
) (*UpstreamTokenPriceSyncResult, error) {
	r.tokenCalls++
	r.channels = append([]int64(nil), channelIDs...)
	r.tokenUpdates = append([]UpstreamTokenPriceUpdate(nil), updates...)
	if r.tokenResult == nil {
		return nil, r.tokenErr
	}
	copy := *r.tokenResult
	return &copy, r.tokenErr
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

func TestUpstreamPriceMonitorRunnerSchedulesPerRequestSyncEveryFifteenMinutes(t *testing.T) {
	first := 0.01
	cfg := domain.DefaultUpstreamPriceMonitorConfig()
	cfg.Enabled = false // Direct due-clock test; the auto-apply gate is covered below.
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

	runner.runPerRequestIfDue()
	require.Equal(t, 1, fetcher.callCount())
	require.Zero(t, repo.applyRuns)
	clock = clock.Add(14 * time.Minute)
	runner.runPerRequestIfDue()
	require.Equal(t, 1, fetcher.callCount())
	clock = clock.Add(time.Minute)
	runner.runPerRequestIfDue()
	require.Equal(t, 2, fetcher.callCount())
}

func TestUpstreamPriceMonitorRunnerRetriesPerRequestSyncAfterFiveMinutes(t *testing.T) {
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
	clock = clock.Add(4 * time.Minute)
	runner.runPerRequestIfDue()
	require.Equal(t, 1, fetcher.callCount())
	clock = clock.Add(time.Minute)
	runner.runPerRequestIfDue()
	require.Equal(t, 2, fetcher.callCount())
}

func TestUpstreamPriceMonitorRunnerSchedulesAuthoritativeTokenSyncEveryFifteenMinutes(t *testing.T) {
	official, selling, output := 1.6, 0.16, 0.32
	cfg := domain.DefaultUpstreamPriceMonitorConfig()
	cfg.Enabled = false
	cfg.ChannelIDs = []int64{2}
	cfg.DomesticModels = []string{"deepseek-v4-flash-0731"}
	monitorRepo := &perRequestPriceSyncRepoStub{
		activeProbeTestRepository: &activeProbeTestRepository{}, cfg: cfg,
		tokenResult: &UpstreamTokenPriceSyncResult{Models: 1, ChangedModels: 1, ChangedChannelRows: 1},
	}
	fetcher := &perRequestPricePageStub{
		tokenPrices: map[string]UpstreamTokenDisplayPrice{
			"deepseek-v4-flash-0731": {ModelName: "deepseek-v4-flash-0731", OfficialInput: &official, SellingInput: &selling, SellingOutput: &output},
		},
	}
	svc := NewUpstreamPriceMonitorService(monitorRepo, nil, nil)
	svc.SetPricePageFetcher(fetcher)
	runner := NewUpstreamPriceMonitorRunner(svc)
	defer runner.cancel()
	clock := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	runner.now = func() time.Time { return clock }

	runner.runTokenIfDue()
	require.Equal(t, 1, fetcher.tokenCallCount())
	require.Equal(t, 1, monitorRepo.tokenCalls)
	require.Len(t, monitorRepo.tokenUpdates, 1)
	require.InDelta(t, 0.192, *monitorRepo.tokenUpdates[0].InputPerMillion, 1e-12)
	clock = clock.Add(14 * time.Minute)
	runner.runTokenIfDue()
	require.Equal(t, 1, fetcher.tokenCallCount())
	clock = clock.Add(time.Minute)
	runner.runTokenIfDue()
	require.Equal(t, 2, fetcher.tokenCallCount())
	require.Equal(t, 2, monitorRepo.tokenCalls)
}

func TestUpstreamPriceMonitorRunnerDoesNotAutomaticallyWritePagePricesOutsideAutoApply(t *testing.T) {
	for _, tc := range []struct {
		name    string
		enabled bool
		mode    domain.UpstreamPriceMonitorMode
	}{
		{name: "observe", enabled: true, mode: domain.UpstreamPriceMonitorModeObserve},
		{name: "review", enabled: true, mode: domain.UpstreamPriceMonitorModeReview},
		{name: "disabled auto apply", enabled: false, mode: domain.UpstreamPriceMonitorModeAutoApply},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := domain.DefaultUpstreamPriceMonitorConfig()
			cfg.Enabled = tc.enabled
			cfg.Mode = tc.mode
			cfg.ChannelIDs = []int64{2}
			cfg.DomesticModels = []string{"deepseek-v4-flash-0731"}
			cfg.PerRequestModels = []string{"deepseek-v4-flash-0731"}
			repo := &perRequestPriceSyncRepoStub{
				activeProbeTestRepository: &activeProbeTestRepository{}, cfg: cfg,
				result:      &UpstreamPerRequestPriceSyncResult{Models: 1},
				tokenResult: &UpstreamTokenPriceSyncResult{Models: 1},
			}
			fetcher := &perRequestPricePageStub{}
			svc := NewUpstreamPriceMonitorService(repo, nil, nil)
			svc.SetPricePageFetcher(fetcher)
			runner := NewUpstreamPriceMonitorRunner(svc)
			defer runner.cancel()

			runner.runPageSyncsIfDue()
			require.Zero(t, fetcher.callCount())
			require.Zero(t, fetcher.tokenCallCount())
			require.Zero(t, repo.calls)
			require.Zero(t, repo.tokenCalls)
		})
	}
}

func TestUpstreamPriceMonitorRunnerAutoApplyRunsBothPageSyncModesImmediately(t *testing.T) {
	first, input, output := 0.01, 0.16, 0.32
	cfg := domain.DefaultUpstreamPriceMonitorConfig()
	cfg.Enabled = true
	cfg.Mode = domain.UpstreamPriceMonitorModeAutoApply
	cfg.ChannelIDs = []int64{2}
	cfg.DomesticModels = []string{"deepseek-v4-flash-0731"}
	cfg.PerRequestModels = []string{"deepseek-v4-flash-0731"}
	repo := &perRequestPriceSyncRepoStub{
		activeProbeTestRepository: &activeProbeTestRepository{}, cfg: cfg,
		result:      &UpstreamPerRequestPriceSyncResult{Models: 1},
		tokenResult: &UpstreamTokenPriceSyncResult{Models: 1},
	}
	fetcher := &perRequestPricePageStub{
		prices: map[string]domain.UpstreamPriceVector{
			"deepseek-v4-flash-0731": {PerRequestLTE256K: &first},
		},
		tokenPrices: map[string]UpstreamTokenDisplayPrice{
			"deepseek-v4-flash-0731": {
				ModelName: "deepseek-v4-flash-0731", SellingInput: &input, SellingOutput: &output,
			},
		},
	}
	svc := NewUpstreamPriceMonitorService(repo, nil, nil)
	svc.SetPricePageFetcher(fetcher)
	runner := NewUpstreamPriceMonitorRunner(svc)
	defer runner.cancel()

	runner.runPageSyncsIfDue()
	require.Equal(t, 1, fetcher.tokenCallCount())
	require.Equal(t, 1, fetcher.callCount())
	require.Equal(t, 1, repo.tokenCalls)
	require.Equal(t, 1, repo.calls)
}
