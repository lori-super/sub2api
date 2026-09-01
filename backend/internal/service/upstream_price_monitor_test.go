package service

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/domain"
	"github.com/stretchr/testify/require"
)

func TestDefaultUpstreamPriceMonitorConfigIsDisabledObserveOnly(t *testing.T) {
	cfg := domain.DefaultUpstreamPriceMonitorConfig()
	require.False(t, cfg.Enabled)
	require.Equal(t, domain.UpstreamPriceMonitorModeObserve, cfg.Mode)
	require.Equal(t, 360, cfg.IntervalMinutes)
	require.InDelta(t, 1.20, cfg.Markup, 1e-12)
	require.True(t, cfg.ActiveOnly)
	require.Equal(t, 7, cfg.ActiveProbeMaxRequests)
	require.Equal(t, 19, cfg.ActiveProbeMaxModels)
	require.InDelta(t, 0.15, cfg.ActiveProbeRunBudgetUSD, 1e-12)
	require.InDelta(t, 0.40, cfg.ActiveProbeDailyBudgetUSD, 1e-12)
	require.Len(t, cfg.DomesticModels, 19)
	require.Contains(t, cfg.DomesticModels, "qwen3.8-flash")
	require.Len(t, cfg.PerRequestModels, 14)
	require.Contains(t, cfg.PerRequestModels, "Auto-Model")
	require.NoError(t, normalizeAndValidateUpstreamPriceMonitorConfig(&cfg))
}

func TestNormalizeUpstreamPriceMonitorConfigRejectsModelsOutsideClosedDomesticScope(t *testing.T) {
	cfg := domain.DefaultUpstreamPriceMonitorConfig()
	cfg.DomesticModels = []string{"new-domestic/model-v1"}
	require.ErrorIs(t, normalizeAndValidateUpstreamPriceMonitorConfig(&cfg), ErrUpstreamPriceMonitorInvalidConfig)

	cfg.DomesticModels = []string{"invalid model"}
	require.ErrorIs(t, normalizeAndValidateUpstreamPriceMonitorConfig(&cfg), ErrUpstreamPriceMonitorInvalidConfig)
}

func TestNormalizeUpstreamPriceMonitorConfigEnforcesActiveSafetyCaps(t *testing.T) {
	base := domain.DefaultUpstreamPriceMonitorConfig()
	for name, mutate := range map[string]func(*domain.UpstreamPriceMonitorConfig){
		"interval too short": func(cfg *domain.UpstreamPriceMonitorConfig) { cfg.IntervalMinutes = 59 },
		"interval too long":  func(cfg *domain.UpstreamPriceMonitorConfig) { cfg.IntervalMinutes = 1441 },
		"markup":             func(cfg *domain.UpstreamPriceMonitorConfig) { cfg.Markup = 1.21 },
		"active only":        func(cfg *domain.UpstreamPriceMonitorConfig) { cfg.ActiveOnly = false },
		"requests":           func(cfg *domain.UpstreamPriceMonitorConfig) { cfg.ActiveProbeMaxRequests = 8 },
		"models":             func(cfg *domain.UpstreamPriceMonitorConfig) { cfg.ActiveProbeMaxModels = 20 },
		"run budget":         func(cfg *domain.UpstreamPriceMonitorConfig) { cfg.ActiveProbeRunBudgetUSD = 0.151 },
		"daily budget":       func(cfg *domain.UpstreamPriceMonitorConfig) { cfg.ActiveProbeDailyBudgetUSD = 0.401 },
		"budget ordering": func(cfg *domain.UpstreamPriceMonitorConfig) {
			cfg.ActiveProbeRunBudgetUSD = 0.15
			cfg.ActiveProbeDailyBudgetUSD = 0.14
		},
	} {
		t.Run(name, func(t *testing.T) {
			cfg := base
			mutate(&cfg)
			require.ErrorIs(t, normalizeAndValidateUpstreamPriceMonitorConfig(&cfg), ErrUpstreamPriceMonitorInvalidConfig)
		})
	}
}

func TestNormalizeActiveProbeScopeRequiresExactlyOneAccount(t *testing.T) {
	cfg := domain.DefaultUpstreamPriceMonitorConfig()
	cfg.ActiveProbeEnabled = true
	cfg.AccountIDs = []int64{7, 8}
	cfg.ChannelIDs = []int64{9}
	require.ErrorIs(t, normalizeAndValidateUpstreamPriceMonitorConfig(&cfg), ErrUpstreamPriceMonitorInvalidConfig)

	cfg.DomesticModels = []string{}
	require.NoError(t, normalizeAndValidateUpstreamPriceMonitorConfig(&cfg))
}

func TestNormalizeUpstreamPriceMonitorConfigKeepsIndependentPerRequestScope(t *testing.T) {
	cfg := domain.DefaultUpstreamPriceMonitorConfig()
	cfg.DomesticModels = []string{"MiniMax-M3"}
	cfg.PerRequestModels = []string{"gpt-5.6", "Auto-Model"}
	require.NoError(t, normalizeAndValidateUpstreamPriceMonitorConfig(&cfg))
	require.Equal(t, []string{"Auto-Model", "gpt-5.6"}, cfg.PerRequestModels)
}

func TestSyncPerRequestDisplayCatalogKeepsForeignModelsAndAppliesMarkupOnce(t *testing.T) {
	upstreamBase, ignoredMiddle, ignoredHigh := 0.01, 9.0, 11.0
	legacyBase, legacyMiddle, legacyHigh := 99.0, 100.0, 101.0
	displayRepo := &stubDisplayPricingRepo{
		providers: []DisplayPricingProvider{
			{Provider: "anthropic", DisplayName: "Anthropic", Currency: DisplayCurrencyUSD},
			{Provider: "gemini", DisplayName: "Gemini", Currency: DisplayCurrencyUSD},
			{Provider: "grok", DisplayName: "Grok", Currency: DisplayCurrencyUSD},
		},
		models: []DisplayModelPrice{
			{ID: 41, Platform: "openai", ModelName: "claude-4", Provider: "anthropic", BillingMode: DisplayBillingModePerRequest, Currency: DisplayCurrencyUSD, Enabled: true, PerRequestLTE256K: &legacyBase, PerRequest256K512KOverride: &legacyMiddle, PerRequestGT512KOverride: &legacyHigh},
			{ID: 42, Platform: "openai", ModelName: "claude-removed", Provider: "anthropic", BillingMode: DisplayBillingModePerRequest, Currency: DisplayCurrencyUSD, Enabled: true, PerRequestLTE256K: &legacyBase},
		},
	}
	monitorRepo := &upstreamPriceAutoApplyRepository{activeProbeTestRepository: &activeProbeTestRepository{}}
	svc := &UpstreamPriceMonitorService{
		repo: monitorRepo, displayPricing: NewDisplayPricingService(displayRepo),
	}
	cfg := domain.DefaultUpstreamPriceMonitorConfig()
	cfg.Markup = 7 // channel monitor markup must not leak into display pricing
	cfg.PerRequestModels = []string{"claude-4", "claude-removed"}
	prices := map[string]domain.UpstreamPriceVector{
		"claude-4": {PerRequestLTE256K: &upstreamBase, PerRequest256K512K: &ignoredMiddle, PerRequestGT512K: &ignoredHigh},
		"gemini-3": {PerRequestLTE256K: &upstreamBase},
		"grok-5":   {PerRequestLTE256K: &upstreamBase},
	}

	managed, err := svc.syncPerRequestDisplayCatalog(context.Background(), prices, &cfg)
	require.NoError(t, err)
	require.Equal(t, []string{"claude-4", "gemini-3", "grok-5"}, managed)
	require.Equal(t, managed, cfg.PerRequestModels)
	require.NotNil(t, monitorRepo.updated)
	require.Equal(t, managed, monitorRepo.updated.PerRequestModels)

	modelsByName := make(map[string]DisplayModelPrice, len(displayRepo.models))
	for _, model := range displayRepo.models {
		modelsByName[model.ModelName] = model
	}
	for _, name := range managed {
		model := modelsByName[name]
		require.True(t, model.Enabled, name)
		require.Equal(t, DisplayBillingModePerRequest, model.BillingMode, name)
		require.InDelta(t, 0.012, *model.PerRequestLTE256K, 1e-12, name)
		require.Nil(t, model.PerRequest256K512KOverride, name)
		require.Nil(t, model.PerRequestGT512KOverride, name)
	}
	require.False(t, modelsByName["claude-removed"].Enabled)
}

func TestNormalizeUpstreamPriceMonitorConfigPreservesExplicitEmptyScope(t *testing.T) {
	cfg := domain.DefaultUpstreamPriceMonitorConfig()
	cfg.DomesticModels = []string{}
	require.NoError(t, normalizeAndValidateUpstreamPriceMonitorConfig(&cfg))
	require.Empty(t, cfg.DomesticModels)
}

func TestUpstreamPriceModelDiscoveryClassifiesAndIntersectsPerAccount(t *testing.T) {
	require.True(t, isLikelyDomesticUpstreamModel("deepseek-new-v1"))
	require.False(t, isLikelyDomesticUpstreamModel("gpt-foreign-v1"))
	managed := []string{"deepseek-new-v1", "MiniMax-M3"}
	require.Equal(t, []string{"deepseek-new-v1"}, intersectUpstreamPriceModels(managed, map[string]struct{}{"deepseek-new-v1": {}}))
	require.Equal(t, managed, intersectUpstreamPriceModels(managed, nil))
}

func TestUpstreamPriceAccountingMatchIgnoresLocalCost(t *testing.T) {
	remote := domain.UpstreamPriceUsageCounters{Requests: 2, InputTokens: 100, OutputTokens: 20, CacheReadTokens: 40, ActualCost: 0.2}
	local := remote
	local.ActualCost = 99
	require.True(t, equalUpstreamPriceAccountingCounters(remote, local))
	local.CacheReadTokens++
	require.False(t, equalUpstreamPriceAccountingCounters(remote, local))
}

func TestSolveUpstreamTokenPricesRequiresFullRankAndFindsCacheRead(t *testing.T) {
	rows := []domain.UpstreamPriceObservation{
		{InputTokens: 1_000_000, OutputTokens: 100_000, ActualCost: 0.2 + 0.03},
		{InputTokens: 100_000, OutputTokens: 1_000_000, ActualCost: 0.02 + 0.3},
		{InputTokens: 200_000, OutputTokens: 100_000, CacheReadTokens: 1_000_000, ActualCost: 0.04 + 0.03 + 0.05},
	}
	prices, count, err := SolveUpstreamTokenPrices(rows)
	require.NoError(t, err)
	require.Equal(t, 3, count)
	require.InDelta(t, 0.2, *prices.InputPerMillion, 1e-9)
	require.InDelta(t, 0.3, *prices.OutputPerMillion, 1e-9)
	require.InDelta(t, 0.05, *prices.CacheReadPerMillion, 1e-9)
	require.Nil(t, prices.CacheWritePerMillion)

	_, _, err = SolveUpstreamTokenPrices(rows[:1])
	require.ErrorIs(t, err, ErrUpstreamPriceInsufficientRank)
}

func TestSolveUpstreamTokenPricesFitsRequestInterceptAcrossSevenSamples(t *testing.T) {
	const fixedFee = 0.000013
	rows := []domain.UpstreamPriceObservation{
		{Requests: 1, InputTokens: 100, OutputTokens: 1},
		{Requests: 1, InputTokens: 1000, OutputTokens: 1},
		{Requests: 1, InputTokens: 100, OutputTokens: 50},
		{Requests: 1, InputTokens: 3000, OutputTokens: 1, CacheCreationTokens: 2000},
		{Requests: 1, InputTokens: 3000, OutputTokens: 1, CacheReadTokens: 2000},
		{Requests: 1, InputTokens: 500, OutputTokens: 20, CacheCreationTokens: 500},
		{Requests: 1, InputTokens: 700, OutputTokens: 30, CacheReadTokens: 300},
	}
	for i := range rows {
		rows[i].ActualCost = fixedFee + float64(rows[i].InputTokens)/1_000_000*0.2 +
			float64(rows[i].OutputTokens)/1_000_000*0.3 +
			float64(rows[i].CacheCreationTokens)/1_000_000*0.4 +
			float64(rows[i].CacheReadTokens)/1_000_000*0.05
	}
	prices, count, err := SolveUpstreamTokenPrices(rows)
	require.NoError(t, err)
	require.Equal(t, 7, count)
	require.InDelta(t, fixedFee, *prices.FixedPerRequest, 1e-10)
	require.InDelta(t, 0.2, *prices.InputPerMillion, 1e-8)
	require.InDelta(t, 0.3, *prices.OutputPerMillion, 1e-8)
	require.InDelta(t, 0.4, *prices.CacheWritePerMillion, 1e-8)
	require.InDelta(t, 0.05, *prices.CacheReadPerMillion, 1e-8)
}

func TestPeakNormalizationProducesBasePricesBeforeFixedMarkup(t *testing.T) {
	rows := []domain.UpstreamPriceObservation{
		{InputTokens: 1_000_000, ActualCost: 0.3},
		{OutputTokens: 1_000_000, ActualCost: 0.45},
		{CacheCreationTokens: 1_000_000, ActualCost: 0.6},
		{CacheReadTokens: 1_000_000, ActualCost: 0.075},
	}
	normalized, err := normalizeUpstreamPriceObservationsForPeak(rows, 1.5)
	require.NoError(t, err)
	prices, _, err := SolveUpstreamTokenPrices(normalized)
	require.NoError(t, err)
	suggested := multiplyUpstreamPriceVector(prices, upstreamPriceRequiredMarkup)
	require.InDelta(t, 0.2, *prices.InputPerMillion, 1e-12)
	require.InDelta(t, 0.24, *suggested.InputPerMillion, 1e-12)
	require.InDelta(t, 0.06, *suggested.CacheReadPerMillion, 1e-12)
}

func TestActiveProbeBudgetStopsBeforeStartingAnotherModel(t *testing.T) {
	cfg := domain.DefaultUpstreamPriceMonitorConfig()
	budget := newUpstreamPriceProbeBudget(&cfg, 0.05)
	require.Empty(t, budget.stopReason())
	budget.modelsStarted++
	budget.runSpent = 0.151
	require.Contains(t, budget.stopReason(), "run budget reached")

	daily := newUpstreamPriceProbeBudget(&cfg, 0.399)
	daily.modelsStarted++
	daily.runSpent = 0.001
	require.Contains(t, daily.stopReason(), "daily budget reached")
}

func TestActiveProbeAssignmentIsUniqueAndCappedAtNineteen(t *testing.T) {
	availability := map[int64]map[string]struct{}{1: {}, 2: {}}
	for index, model := range domain.DefaultX5M5XDomesticModels {
		accountID := int64(1 + index%2)
		availability[accountID][strings.ToLower(model)] = struct{}{}
	}
	assignments, unavailable := assignUpstreamPriceProbeModels(
		[]int64{1, 2}, domain.DefaultX5M5XDomesticModels, availability, 19,
	)
	require.Empty(t, unavailable)
	require.Len(t, assignments[1], 10)
	require.Len(t, assignments[2], 9)
}

func TestScheduledAutoApplyAcceptsTrustedSubsetButSkipsZeroCoverage(t *testing.T) {
	cfg := domain.DefaultUpstreamPriceMonitorConfig()
	cfg.Mode = domain.UpstreamPriceMonitorModeAutoApply
	run := &domain.UpstreamPriceMonitorRun{
		Status: domain.UpstreamPriceMonitorRunStatusCompleted, MatchedModels: 16, MismatchedModels: 3,
	}
	require.True(t, shouldAutoApplyUpstreamPriceRun(run, &cfg))
	run.MatchedModels = 0
	require.False(t, shouldAutoApplyUpstreamPriceRun(run, &cfg))
	run.MatchedModels = 16
	run.Status = domain.UpstreamPriceMonitorRunStatusPartial
	require.False(t, shouldAutoApplyUpstreamPriceRun(run, &cfg))
	run.Status = domain.UpstreamPriceMonitorRunStatusCompleted
	cfg.Mode = domain.UpstreamPriceMonitorModeReview
	require.False(t, shouldAutoApplyUpstreamPriceRun(run, &cfg), "review mode must wait for an administrator Apply action")
}

func TestSolveUpstreamTokenPricesRejectsIllConditionedTrafficWindows(t *testing.T) {
	rows := []domain.UpstreamPriceObservation{
		{InputTokens: 1_000_000, OutputTokens: 1_000_000, ActualCost: 0.5},
		{InputTokens: 1_000_000, OutputTokens: 1_000_001, ActualCost: 0.5000003},
	}
	_, _, err := SolveUpstreamTokenPrices(rows)
	require.ErrorIs(t, err, ErrUpstreamPriceInsufficientRank)
}

func TestParseX5M5XUsageModelStats(t *testing.T) {
	body := []byte(`{"model_stats":[{"model":"MiniMax-M3","requests":2,"input_tokens":100,"output_tokens":20,"cache_creation_tokens":0,"cache_read_tokens":80,"actual_cost":0.000042}]}`)
	models, err := parseX5M5XUsageModelStats(body)
	require.NoError(t, err)
	require.Equal(t, domain.UpstreamPriceUsageCounters{
		Requests: 2, InputTokens: 100, OutputTokens: 20, CacheReadTokens: 80, ActualCost: 0.000042,
	}, models["minimax-m3"])
}

func TestParseX5M5XModelListRejectsPartialOrDuplicateSnapshots(t *testing.T) {
	models, err := parseX5M5XModelList([]byte(`{"data":[{"id":"MiniMax-M3"},{"id":"new-domestic-v1"}]}`))
	require.NoError(t, err)
	require.Equal(t, []string{"MiniMax-M3", "new-domestic-v1"}, models)

	_, err = parseX5M5XModelList([]byte(`{"data":[{"id":"MiniMax-M3"},{"id":"minimax-m3"}]}`))
	require.Error(t, err)
	_, err = parseX5M5XModelList([]byte(`{"data":[{}]}`))
	require.Error(t, err)
}

func TestUsageCheckpointJSONNeverContainsCredential(t *testing.T) {
	checkpoint := domain.UpstreamPriceUsageCheckpoint{AccountIdentityHash: "hash-only", LedgerDate: "2026-08-30"}
	raw, err := json.Marshal(checkpoint)
	require.NoError(t, err)
	require.NotContains(t, string(raw), "api_key")
	require.NotContains(t, string(raw), "authorization")
}

func TestUpstreamPriceBillingContextIgnoresObservationTimestamp(t *testing.T) {
	first := &domain.UpstreamPriceBillingSnapshot{
		ResolvedRateMultiplier: 0.5, EffectiveRateMultiplier: 0.6, AppliedPeakMultiplier: 1.2,
		ObservedAt: time.Date(2026, 8, 30, 1, 0, 0, 0, time.UTC),
	}
	second := *first
	second.ObservedAt = first.ObservedAt.Add(15 * time.Minute)

	firstHash, firstKey := upstreamPriceBillingContext(first)
	secondHash, secondKey := upstreamPriceBillingContext(&second)
	require.Equal(t, firstHash, secondHash)
	require.Equal(t, firstKey, secondKey)

	second.EffectiveRateMultiplier = 0.7
	changedHash, changedKey := upstreamPriceBillingContext(&second)
	require.NotEqual(t, firstHash, changedHash)
	require.NotEqual(t, firstKey, changedKey)
}

func TestUpstreamPriceRunApplyScopeReadsPersistedJSONSummary(t *testing.T) {
	run := &domain.UpstreamPriceMonitorRun{Summary: map[string]any{
		"account_ids":                 []any{float64(7), float64(8)},
		"account_ledger_hashes":       map[string]any{"7": "hash-7", "8": "hash-8"},
		"account_identity_hashes":     map[string]any{"7": "identity-7", "8": "identity-8"},
		"channel_ids":                 []any{float64(9), float64(3), float64(9)},
		"display_multiplier_decimals": float64(3),
		"snapshot_max_age_minutes":    float64(60),
		"config_updated_at":           "2026-08-30T01:02:03Z",
		"model_catalog_revision":      float64(12),
	}}
	scope, ok := ReadUpstreamPriceRunApplyScope(run)
	require.True(t, ok)
	require.Equal(t, []int64{7, 8}, scope.AccountIDs)
	require.Equal(t, map[int64]string{7: "hash-7", 8: "hash-8"}, scope.AccountLedgerHashes)
	require.Equal(t, map[int64]string{7: "identity-7", 8: "identity-8"}, scope.AccountIdentityHashes)
	require.Equal(t, []int64{3, 9}, scope.ChannelIDs)
	require.Equal(t, 3, scope.DisplayMultiplierDecimals)
	require.Equal(t, 60, scope.MaxAgeMinutes)
	require.Equal(t, time.Date(2026, 8, 30, 1, 2, 3, 0, time.UTC), scope.ConfigUpdatedAt)
	require.Equal(t, int64(12), scope.ModelCatalogRevision)
}

func TestUpstreamPriceCredentialLedgerHashExcludesLocalAccountID(t *testing.T) {
	first := &Account{ID: 1, Type: AccountTypeAPIKey, Credentials: map[string]any{
		"api_key": "test-key-not-real", "base_url": "https://us-api.example.invalid",
	}}
	second := &Account{ID: 2, Type: AccountTypeAPIKey, Credentials: map[string]any{
		"api_key": "test-key-not-real", "base_url": "https://us-api.example.invalid",
	}}
	require.Equal(t, UpstreamPriceCredentialLedgerHash(first), UpstreamPriceCredentialLedgerHash(second))
	require.NotEqual(t, UpstreamPriceAccountIdentityHash(first), UpstreamPriceAccountIdentityHash(second))
}

func TestUpstreamPriceEvidenceHashIncludesDisplayPriceSnapshot(t *testing.T) {
	low, changed := 0.01, 0.02
	base := []domain.UpstreamPriceEvidence{{
		ID: 1, RunID: 2, ModelName: "deepseek-request", BillingMode: DisplayBillingModePerRequest,
		DisplayPricesCurrent: domain.UpstreamPriceVector{PerRequestLTE256K: &low},
	}}
	modified := append([]domain.UpstreamPriceEvidence(nil), base...)
	modified[0].DisplayPricesCurrent.PerRequestLTE256K = &changed
	require.NotEqual(t, upstreamPriceEvidenceHash(base), upstreamPriceEvidenceHash(modified))
}

func TestMissingUpstreamPriceProbeModelsPrefersTrustedNaturalEvidence(t *testing.T) {
	models := []string{"MiniMax-M3", "glm-5.3"}
	input, output, cacheWrite, cacheRead := 0.2, 0.8, 0.4, 0.05
	evidence := []domain.UpstreamPriceEvidence{
		{AccountID: 7, ModelName: "minimax-m3", BillingMode: DisplayBillingModeToken, Status: domain.UpstreamPriceEvidenceStatusTrusted,
			Prices: domain.UpstreamPriceVector{InputPerMillion: &input, OutputPerMillion: &output, CacheWritePerMillion: &cacheWrite, CacheReadPerMillion: &cacheRead}},
		{AccountID: 7, ModelName: "glm-5.3", BillingMode: DisplayBillingModeToken, Status: domain.UpstreamPriceEvidenceStatusPending},
	}
	require.Equal(t, []string{"glm-5.3"}, missingUpstreamPriceProbeModels(evidence, 7, models))
}

func TestUpstreamPriceActiveProbeSpecsReuseOnlyTheCachePrefix(t *testing.T) {
	specs := upstreamPriceActiveProbeSpecs("MiniMax-M3")
	require.Len(t, specs, 7)
	require.NotEqual(t, specs[0].SessionID, specs[1].SessionID)
	require.Equal(t, specs[3].SessionID, specs[4].SessionID)
	require.Equal(t, specs[3].SystemPrompt, specs[4].SystemPrompt)
	require.NotEmpty(t, specs[3].SystemPrompt)
	require.True(t, specs[5].ExplicitCache)
	require.Equal(t, specs[5].SessionID, specs[6].SessionID)
}

func TestActiveProbeRequiresAnExclusiveConcurrencyLeaseBackend(t *testing.T) {
	svc := &UpstreamPriceMonitorService{}
	release, acquired, err := svc.acquireUpstreamPriceProbeSlot(context.Background(), &Account{ID: 7, Concurrency: 1})
	require.NoError(t, err)
	require.False(t, acquired)
	require.NotPanics(t, release)
}
