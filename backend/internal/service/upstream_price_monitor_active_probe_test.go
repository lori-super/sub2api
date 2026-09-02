package service

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/domain"
	"github.com/stretchr/testify/require"
)

type activeProbeTestRepository struct {
	checkpoint     domain.UpstreamPriceUsageCheckpoint
	saved          *domain.UpstreamPriceEvidence
	savedCP        *domain.UpstreamPriceUsageCheckpoint
	aggregateCalls int
	contaminate    bool
	progressCosts  []float64
	runtimeCost    float64
}

func (r *activeProbeTestRepository) GetConfig(context.Context) (*domain.UpstreamPriceMonitorConfig, error) {
	cfg := domain.DefaultUpstreamPriceMonitorConfig()
	return &cfg, nil
}
func (r *activeProbeTestRepository) UpdateConfig(context.Context, *domain.UpstreamPriceMonitorConfig) error {
	return nil
}
func (r *activeProbeTestRepository) GetRuntime(context.Context) (*domain.UpstreamPriceMonitorRuntime, error) {
	return &domain.UpstreamPriceMonitorRuntime{TodayProbeCost: r.runtimeCost}, nil
}
func (r *activeProbeTestRepository) CreateRun(context.Context, *domain.UpstreamPriceMonitorRun) error {
	return nil
}
func (r *activeProbeTestRepository) UpdateRunProbeProgress(_ context.Context, _ int64, _ int, cost float64) error {
	r.progressCosts = append(r.progressCosts, cost)
	return nil
}
func (r *activeProbeTestRepository) FinishRun(context.Context, *domain.UpstreamPriceMonitorRun) error {
	return nil
}
func (r *activeProbeTestRepository) GetRun(context.Context, int64) (*domain.UpstreamPriceMonitorRun, error) {
	return &domain.UpstreamPriceMonitorRun{}, nil
}
func (r *activeProbeTestRepository) ListRuns(context.Context, int, int, ...domain.UpstreamPriceMonitorRunStatus) (*domain.UpstreamPriceMonitorRunPage, error) {
	return &domain.UpstreamPriceMonitorRunPage{}, nil
}
func (r *activeProbeTestRepository) ListEvidenceByRun(context.Context, int64) ([]domain.UpstreamPriceEvidence, error) {
	return nil, nil
}
func (r *activeProbeTestRepository) FreezeEvidenceApplySnapshot(context.Context, int64, []int64, int) ([]domain.UpstreamPriceEvidence, error) {
	return nil, nil
}
func (r *activeProbeTestRepository) GetCheckpoints(context.Context, int64, []string) (map[string]domain.UpstreamPriceUsageCheckpoint, error) {
	return map[string]domain.UpstreamPriceUsageCheckpoint{"minimax-m3": r.checkpoint}, nil
}
func (r *activeProbeTestRepository) CurrentLocalUsageLogID(context.Context, []int64) (int64, error) {
	return 0, nil
}
func (r *activeProbeTestRepository) AggregateLocalUsage(context.Context, []int64, map[string]int64, int64) (map[string]domain.UpstreamPriceLocalAggregate, error) {
	r.aggregateCalls++
	value := domain.UpstreamPriceLocalAggregate{ModelName: "MiniMax-M3"}
	if r.contaminate && r.aggregateCalls == 3 {
		value.Counters = domain.UpstreamPriceUsageCounters{Requests: 1, InputTokens: 20}
	}
	return map[string]domain.UpstreamPriceLocalAggregate{"minimax-m3": value}, nil
}
func (r *activeProbeTestRepository) ListMatchedObservations(context.Context, int64, string, string, time.Time, int) ([]domain.UpstreamPriceObservation, error) {
	return nil, nil
}
func (r *activeProbeTestRepository) SaveReconciliation(_ context.Context, checkpoint *domain.UpstreamPriceUsageCheckpoint, expected *int64, evidence *domain.UpstreamPriceEvidence) error {
	if checkpoint != nil {
		if expected == nil {
			checkpoint.Revision = 1
		} else {
			checkpoint.Revision = *expected + 1
		}
		copy := *checkpoint
		r.savedCP = &copy
		r.checkpoint = copy
	}
	if evidence != nil {
		copy := *evidence
		r.saved = &copy
	}
	return nil
}
func (r *activeProbeTestRepository) ApplyRun(context.Context, int64, string, []int64, []int64, int, int, time.Time, int64) error {
	return nil
}
func (r *activeProbeTestRepository) RollbackRun(context.Context, int64, string) error { return nil }
func (r *activeProbeTestRepository) MarkApplyFailure(context.Context, int64, string) error {
	return nil
}
func (r *activeProbeTestRepository) ReconcileModelCatalog(context.Context, []domain.UpstreamPriceDiscoveredModel, int, bool) (int64, error) {
	return 1, nil
}
func (r *activeProbeTestRepository) ListModelCatalog(context.Context) ([]domain.UpstreamPriceModelCatalogEntry, error) {
	return nil, nil
}
func (r *activeProbeTestRepository) SetModelCatalogStatus(context.Context, string, domain.UpstreamPriceModelStatus) (*domain.UpstreamPriceModelCatalogEntry, error) {
	return nil, nil
}

type activeProbeScript struct {
	mu       sync.Mutex
	counters domain.UpstreamPriceUsageCounters
	step     int
	now      time.Time
}

var activeProbeRows = []domain.UpstreamPriceUsageCounters{
	{Requests: 1, InputTokens: 100, OutputTokens: 1},
	{Requests: 1, InputTokens: 1000, OutputTokens: 1},
	{Requests: 1, InputTokens: 100, OutputTokens: 50},
	{Requests: 1, InputTokens: 3000, OutputTokens: 1, CacheCreationTokens: 2000},
	{Requests: 1, InputTokens: 3000, OutputTokens: 1, CacheReadTokens: 2000},
}

func activeProbeRowCost(row domain.UpstreamPriceUsageCounters) float64 {
	return float64(row.InputTokens)/1_000_000*0.2 + float64(row.OutputTokens)/1_000_000*0.3 +
		float64(row.CacheCreationTokens)/1_000_000*0.4 + float64(row.CacheReadTokens)/1_000_000*0.05
}

func (s *activeProbeScript) FetchUsage(_ context.Context, account *Account) (*domain.UpstreamPriceRemoteUsageSnapshot, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return &domain.UpstreamPriceRemoteUsageSnapshot{
		AccountID: account.ID, LedgerDate: "2026-08-30", CapturedAt: s.now,
		Models: map[string]domain.UpstreamPriceUsageCounters{"MiniMax-M3": s.counters},
	}, nil
}
func (s *activeProbeScript) FetchBilling(context.Context, *Account) (*domain.UpstreamPriceBillingSnapshot, error) {
	return &domain.UpstreamPriceBillingSnapshot{
		ResolvedRateMultiplier: 1, EffectiveRateMultiplier: 1, AppliedPeakMultiplier: 1,
	}, nil
}
func (s *activeProbeScript) FetchModels(context.Context, *Account) ([]string, error) {
	return []string{"MiniMax-M3"}, nil
}
func (s *activeProbeScript) Probe(_ context.Context, _ *Account, request UpstreamPriceActiveProbeRequest) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	row := activeProbeRows[s.step]
	row.ActualCost = activeProbeRowCost(row)
	s.counters = addUpstreamPriceCounters(s.counters, row)
	s.step++
	_ = request
	return nil
}

func TestActiveProbeSolvesAndCheckpointsFourTokenDimensions(t *testing.T) {
	originalPollInterval := upstreamPriceProbeLedgerPollInterval
	upstreamPriceProbeLedgerPollInterval = time.Millisecond
	t.Cleanup(func() { upstreamPriceProbeLedgerPollInterval = originalPollInterval })
	account := &Account{ID: 7, Type: AccountTypeAPIKey, Credentials: map[string]any{
		"api_key": "test-key-not-real", "base_url": "https://us-api.example.invalid",
	}}
	billing := &domain.UpstreamPriceBillingSnapshot{
		ResolvedRateMultiplier: 1, EffectiveRateMultiplier: 1, AppliedPeakMultiplier: 1,
	}
	billingHash, _ := upstreamPriceBillingContext(billing)
	repo := &activeProbeTestRepository{checkpoint: domain.UpstreamPriceUsageCheckpoint{
		AccountID: 7, ModelName: "MiniMax-M3", AccountIdentityHash: UpstreamPriceAccountIdentityHash(account),
		LedgerDate: "2026-08-30", BillingContextHash: billingHash, Revision: 1,
	}}
	script := &activeProbeScript{now: time.Date(2026, 8, 30, 1, 0, 0, 0, time.UTC)}
	svc := NewUpstreamPriceMonitorService(repo, nil, script)
	svc.SetActiveProber(script)
	svc.probeSlotAcquirer = func(context.Context, *Account) (func(), bool, error) {
		return func() {}, true, nil
	}
	svc.now = func() time.Time { return script.now }
	cfg := domain.DefaultUpstreamPriceMonitorConfig()

	probed, cost, err := svc.probeOneUpstreamPriceModel(context.Background(), 11, &cfg, account, billing, "MiniMax-M3")
	require.NoError(t, err)
	require.True(t, probed)
	require.Positive(t, cost)
	require.NotNil(t, repo.saved)
	require.Equal(t, domain.UpstreamPriceEvidenceStatusTrusted, repo.saved.Status)
	require.InDelta(t, 0.2, *repo.saved.Prices.InputPerMillion, 1e-9)
	require.InDelta(t, 0.3, *repo.saved.Prices.OutputPerMillion, 1e-9)
	require.InDelta(t, 0.4, *repo.saved.Prices.CacheWritePerMillion, 1e-9)
	require.InDelta(t, 0.05, *repo.saved.Prices.CacheReadPerMillion, 1e-9)
	require.InDelta(t, 0.24, *repo.saved.SuggestedPrices.InputPerMillion, 1e-9)
	require.NotNil(t, repo.savedCP)
	require.Equal(t, int64(5), repo.savedCP.Remote.Requests)
	require.False(t, repo.savedCP.ActiveProbePending)
}

func TestActiveProbeKeepsDurablePendingWhenAnyLocalTrafficAppears(t *testing.T) {
	originalPollInterval := upstreamPriceProbeLedgerPollInterval
	upstreamPriceProbeLedgerPollInterval = time.Millisecond
	t.Cleanup(func() { upstreamPriceProbeLedgerPollInterval = originalPollInterval })
	account := &Account{ID: 7, Type: AccountTypeAPIKey, Credentials: map[string]any{
		"api_key": "test-key-not-real", "base_url": "https://us-api.example.invalid",
	}}
	billing := &domain.UpstreamPriceBillingSnapshot{
		ResolvedRateMultiplier: 1, EffectiveRateMultiplier: 1, AppliedPeakMultiplier: 1,
	}
	billingHash, _ := upstreamPriceBillingContext(billing)
	repo := &activeProbeTestRepository{contaminate: true, checkpoint: domain.UpstreamPriceUsageCheckpoint{
		AccountID: 7, ModelName: "MiniMax-M3", AccountIdentityHash: UpstreamPriceAccountIdentityHash(account),
		LedgerDate: "2026-08-30", BillingContextHash: billingHash, Revision: 1,
	}}
	script := &activeProbeScript{now: time.Date(2026, 8, 30, 1, 0, 0, 0, time.UTC)}
	svc := NewUpstreamPriceMonitorService(repo, nil, script)
	svc.SetActiveProber(script)
	svc.probeSlotAcquirer = func(context.Context, *Account) (func(), bool, error) {
		return func() {}, true, nil
	}
	svc.now = func() time.Time { return script.now }
	cfg := domain.DefaultUpstreamPriceMonitorConfig()

	probed, _, err := svc.probeOneUpstreamPriceModel(context.Background(), 12, &cfg, account, billing, "MiniMax-M3")
	require.NoError(t, err)
	require.False(t, probed)
	require.NotNil(t, repo.savedCP)
	require.True(t, repo.savedCP.ActiveProbePending)
	require.NotNil(t, repo.saved)
	require.Equal(t, domain.UpstreamPriceEvidenceStatusUnobservable, repo.saved.Status)
}

func TestActiveProbeStopsCurrentModelImmediatelyAfterSettledSampleExhaustsBudget(t *testing.T) {
	originalPollInterval := upstreamPriceProbeLedgerPollInterval
	upstreamPriceProbeLedgerPollInterval = time.Millisecond
	t.Cleanup(func() { upstreamPriceProbeLedgerPollInterval = originalPollInterval })
	account := &Account{ID: 7, Type: AccountTypeAPIKey, Credentials: map[string]any{
		"api_key": "test-key-not-real", "base_url": "https://us-api.example.invalid",
	}}
	billing := &domain.UpstreamPriceBillingSnapshot{
		ResolvedRateMultiplier: 1, EffectiveRateMultiplier: 1, AppliedPeakMultiplier: 1,
	}
	billingHash, _ := upstreamPriceBillingContext(billing)
	repo := &activeProbeTestRepository{checkpoint: domain.UpstreamPriceUsageCheckpoint{
		AccountID: 7, ModelName: "MiniMax-M3", AccountIdentityHash: UpstreamPriceAccountIdentityHash(account),
		LedgerDate: "2026-08-30", BillingContextHash: billingHash, Revision: 1,
	}}
	script := &activeProbeScript{now: time.Date(2026, 8, 30, 1, 0, 0, 0, time.UTC)}
	svc := NewUpstreamPriceMonitorService(repo, nil, script)
	svc.SetActiveProber(script)
	svc.probeSlotAcquirer = func(context.Context, *Account) (func(), bool, error) {
		return func() {}, true, nil
	}
	svc.now = func() time.Time { return script.now }
	cfg := domain.DefaultUpstreamPriceMonitorConfig()
	cfg.ActiveProbeRunBudgetUSD = activeProbeRowCost(activeProbeRows[0]) / 2
	budget := newUpstreamPriceProbeBudget(&cfg, 0)

	err := svc.probeUpstreamPriceModels(
		context.Background(), 13, &cfg, account, billing, []string{"MiniMax-M3"}, budget,
	)
	require.NoError(t, err)
	require.Equal(t, 1, script.step, "no second request may start after the first settled sample exhausts the budget")
	require.InDelta(t, activeProbeRowCost(activeProbeRows[0]), budget.runSpent, 1e-12)
	require.Contains(t, budget.spendStopReason(), "run budget reached")
	require.NotEmpty(t, repo.progressCosts)
	require.InDelta(t, budget.runSpent, repo.progressCosts[0], 1e-12)
}

func TestRefreshProbeBudgetUsesPersistedActualProbeSpendBeforeNewProbe(t *testing.T) {
	cfg := domain.DefaultUpstreamPriceMonitorConfig()
	cfg.ActiveProbeDailyBudgetUSD = 0.20
	budget := newUpstreamPriceProbeBudget(&cfg, 0.05)
	repo := &activeProbeTestRepository{runtimeCost: 0.20}
	svc := NewUpstreamPriceMonitorService(repo, nil, nil)

	cost, err := svc.refreshUpstreamPriceProbeDailyBudget(context.Background(), budget)
	require.NoError(t, err)
	require.InDelta(t, 0.20, cost, 1e-12)
	require.InDelta(t, 0.20, budget.dailyBeforeRun, 1e-12)
	require.Contains(t, budget.spendStopReason(), "daily budget reached")
}
