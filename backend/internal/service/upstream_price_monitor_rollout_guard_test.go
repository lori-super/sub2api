package service

import (
	"context"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/domain"
	"github.com/stretchr/testify/require"
)

type upstreamPriceAutoApplyRepository struct {
	*activeProbeTestRepository
	config           *domain.UpstreamPriceMonitorConfig
	updated          *domain.UpstreamPriceMonitorConfig
	catalog          []domain.UpstreamPriceModelCatalogEntry
	created          *domain.UpstreamPriceMonitorRun
	finished         *domain.UpstreamPriceMonitorRun
	freezeChannelIDs []int64
	applyCalls       int
	rollbackCalls    int
}

func (r *upstreamPriceAutoApplyRepository) GetConfig(context.Context) (*domain.UpstreamPriceMonitorConfig, error) {
	if r.config == nil {
		cfg := domain.DefaultUpstreamPriceMonitorConfig()
		return &cfg, nil
	}
	copy := *r.config
	copy.AccountIDs = append([]int64(nil), r.config.AccountIDs...)
	copy.ChannelIDs = append([]int64(nil), r.config.ChannelIDs...)
	copy.DomesticModels = append([]string(nil), r.config.DomesticModels...)
	return &copy, nil
}

func (r *upstreamPriceAutoApplyRepository) UpdateConfig(_ context.Context, cfg *domain.UpstreamPriceMonitorConfig) error {
	copy := *cfg
	copy.AccountIDs = append([]int64(nil), cfg.AccountIDs...)
	copy.ChannelIDs = append([]int64(nil), cfg.ChannelIDs...)
	copy.DomesticModels = append([]string(nil), cfg.DomesticModels...)
	r.updated = &copy
	return nil
}

func (r *upstreamPriceAutoApplyRepository) ListModelCatalog(context.Context) ([]domain.UpstreamPriceModelCatalogEntry, error) {
	return append([]domain.UpstreamPriceModelCatalogEntry(nil), r.catalog...), nil
}

func (r *upstreamPriceAutoApplyRepository) CreateRun(_ context.Context, run *domain.UpstreamPriceMonitorRun) error {
	run.ID = 77
	copy := *run
	r.created = &copy
	return nil
}

func (r *upstreamPriceAutoApplyRepository) FinishRun(_ context.Context, run *domain.UpstreamPriceMonitorRun) error {
	copy := *run
	copy.Summary = cloneUpstreamPriceSummary(run.Summary)
	r.finished = &copy
	return nil
}

func (r *upstreamPriceAutoApplyRepository) FreezeEvidenceApplySnapshot(
	_ context.Context,
	_ int64,
	channelIDs []int64,
	_ int,
) ([]domain.UpstreamPriceEvidence, error) {
	r.freezeChannelIDs = append([]int64(nil), channelIDs...)
	return []domain.UpstreamPriceEvidence{{
		ID: 1, RunID: 77, AccountID: 7, ModelName: "MiniMax-M3",
		BillingMode: DisplayBillingModeToken, Status: domain.UpstreamPriceEvidenceStatusTrusted,
		Source:               domain.UpstreamPriceEvidenceSourceActiveProbe,
		ReconciliationStatus: domain.UpstreamPriceReconciliationMatched,
		ContextKey:           "active-final",
	}}, nil
}

func (r *upstreamPriceAutoApplyRepository) GetRun(context.Context, int64) (*domain.UpstreamPriceMonitorRun, error) {
	if r.finished == nil {
		return nil, ErrUpstreamPriceRunNotApplicable
	}
	copy := *r.finished
	copy.Summary = cloneUpstreamPriceSummary(r.finished.Summary)
	return &copy, nil
}

func (r *upstreamPriceAutoApplyRepository) ApplyRun(
	_ context.Context,
	_ int64,
	_ string,
	_ []int64,
	_ []int64,
	_ int,
	_ int,
	_ time.Time,
	_ int64,
) error {
	r.applyCalls++
	if r.finished.Summary == nil {
		r.finished.Summary = map[string]any{}
	}
	r.finished.Summary["applied_models"] = 1
	now := time.Date(2026, 8, 31, 1, 30, 0, 0, time.UTC)
	r.finished.AppliedAt = &now
	r.finished.RollbackAvailable = true
	return nil
}

func (r *upstreamPriceAutoApplyRepository) RollbackRun(context.Context, int64, string) error {
	r.rollbackCalls++
	r.finished.AppliedAt = nil
	r.finished.RollbackAvailable = false
	return nil
}

func cloneUpstreamPriceSummary(value map[string]any) map[string]any {
	if value == nil {
		return nil
	}
	out := make(map[string]any, len(value))
	for key, item := range value {
		out[key] = item
	}
	return out
}

type upstreamPriceAutoApplyAccountReader struct {
	account *Account
}

func (r upstreamPriceAutoApplyAccountReader) GetByID(_ context.Context, id int64) (*Account, error) {
	if r.account == nil || r.account.ID != id {
		return nil, ErrUpstreamPriceMonitorInvalidConfig
	}
	copy := *r.account
	return &copy, nil
}

type upstreamPriceCacheInvalidator struct{ calls int }

func (i *upstreamPriceCacheInvalidator) InvalidatePricingCache() { i.calls++ }

func testUpstreamPriceAccount() *Account {
	return &Account{ID: 7, Type: AccountTypeAPIKey, Credentials: map[string]any{
		"api_key": "test-key-not-real", "base_url": "https://us-api.example.invalid",
	}}
}

func testAutoApplyConfig() domain.UpstreamPriceMonitorConfig {
	cfg := domain.DefaultUpstreamPriceMonitorConfig()
	cfg.Enabled = true
	cfg.Mode = domain.UpstreamPriceMonitorModeAutoApply
	cfg.IntervalMinutes = 1440
	cfg.ActiveProbeEnabled = true
	cfg.AccountIDs = []int64{7}
	cfg.ChannelIDs = []int64{3}
	cfg.DomesticModels = []string{"MiniMax-M3"}
	cfg.UpdatedAt = time.Date(2026, 8, 31, 1, 0, 0, 0, time.UTC)
	return cfg
}

func TestUpstreamPriceMonitorUpdateConfigAcceptsAutoApplyAndActiveProbe(t *testing.T) {
	repo := &upstreamPriceAutoApplyRepository{
		activeProbeTestRepository: &activeProbeTestRepository{},
		catalog: []domain.UpstreamPriceModelCatalogEntry{{
			ModelName: "MiniMax-M3", Status: domain.UpstreamPriceModelStatusManaged,
		}},
	}
	svc := NewUpstreamPriceMonitorService(repo, upstreamPriceAutoApplyAccountReader{account: testUpstreamPriceAccount()}, nil)
	svc.SetActiveProber(&activeProbeScript{})
	cfg := testAutoApplyConfig()
	cfg.ActiveProbeEnabled = true

	err := svc.UpdateConfig(context.Background(), &cfg)

	require.NoError(t, err)
	require.NotNil(t, repo.updated)
	require.Equal(t, domain.UpstreamPriceMonitorModeAutoApply, repo.updated.Mode)
	require.True(t, repo.updated.ActiveProbeEnabled)
	require.Equal(t, []string{"MiniMax-M3"}, repo.updated.DomesticModels)
}

func TestUpstreamPriceMonitorUpdateConfigRejectsActiveProbeWithoutProber(t *testing.T) {
	repo := &upstreamPriceAutoApplyRepository{activeProbeTestRepository: &activeProbeTestRepository{}}
	svc := NewUpstreamPriceMonitorService(repo, upstreamPriceAutoApplyAccountReader{account: testUpstreamPriceAccount()}, nil)
	cfg := testAutoApplyConfig()
	cfg.ActiveProbeEnabled = true

	err := svc.UpdateConfig(context.Background(), &cfg)

	require.ErrorIs(t, err, ErrUpstreamPriceMonitorInvalidConfig)
	require.Nil(t, repo.updated)
}

func TestUpstreamPriceMonitorUpdateConfigRejectsActiveProbeWithoutAccountAndChannelScope(t *testing.T) {
	repo := &upstreamPriceAutoApplyRepository{activeProbeTestRepository: &activeProbeTestRepository{}}
	svc := NewUpstreamPriceMonitorService(repo, nil, nil)
	svc.SetActiveProber(&activeProbeScript{})
	cfg := domain.DefaultUpstreamPriceMonitorConfig()
	cfg.ActiveProbeEnabled = true

	err := svc.UpdateConfig(context.Background(), &cfg)

	require.ErrorIs(t, err, ErrUpstreamPriceMonitorInvalidConfig)
	require.Nil(t, repo.updated)
}

func TestUpstreamPriceMonitorManualRunAlwaysStaysDryRun(t *testing.T) {
	cfg := testAutoApplyConfig()
	repo := &upstreamPriceAutoApplyRepository{
		activeProbeTestRepository: &activeProbeTestRepository{},
		config:                    &cfg,
	}
	remote := &activeProbeScript{now: time.Date(2026, 8, 31, 1, 15, 0, 0, time.UTC)}
	svc := NewUpstreamPriceMonitorService(repo, upstreamPriceAutoApplyAccountReader{account: testUpstreamPriceAccount()}, remote)
	svc.SetActiveProber(remote)

	run, err := svc.RunOnce(context.Background(), UpstreamPriceRunOptions{
		Trigger: domain.UpstreamPriceMonitorRunTriggerManual,
		DryRun:  false,
	})

	require.NoError(t, err)
	require.NotNil(t, run)
	require.True(t, run.DryRun)
	require.Equal(t, domain.UpstreamPriceMonitorModeAutoApply, run.Mode)
	require.Zero(t, repo.applyCalls)
}

func TestUpstreamPriceMonitorScheduledAutoApplyInvalidatesPricingCache(t *testing.T) {
	cfg := testAutoApplyConfig()
	repo := &upstreamPriceAutoApplyRepository{
		activeProbeTestRepository: &activeProbeTestRepository{},
		config:                    &cfg,
	}
	remote := &activeProbeScript{now: time.Date(2026, 8, 31, 1, 15, 0, 0, time.UTC)}
	svc := NewUpstreamPriceMonitorService(repo, upstreamPriceAutoApplyAccountReader{account: testUpstreamPriceAccount()}, remote)
	svc.SetActiveProber(remote)
	invalidator := &upstreamPriceCacheInvalidator{}
	svc.SetPricingCacheInvalidator(invalidator)

	run, err := svc.RunOnce(context.Background(), UpstreamPriceRunOptions{
		Trigger: domain.UpstreamPriceMonitorRunTriggerScheduled,
		DryRun:  false,
	})

	require.NoError(t, err)
	require.NotNil(t, run)
	require.False(t, repo.created.DryRun)
	require.Equal(t, []int64{3}, repo.freezeChannelIDs)
	require.Equal(t, 1, repo.applyCalls)
	require.NotNil(t, run.AppliedAt)
	require.Equal(t, 1, invalidator.calls)
}

func TestUpstreamPriceMonitorApplyChecksAccountIdentityAndRollbackInvalidatesCache(t *testing.T) {
	account := testUpstreamPriceAccount()
	cfg := testAutoApplyConfig()
	repo := &upstreamPriceAutoApplyRepository{
		activeProbeTestRepository: &activeProbeTestRepository{},
		config:                    &cfg,
		finished: &domain.UpstreamPriceMonitorRun{
			ID: 77, Status: domain.UpstreamPriceMonitorRunStatusCompleted, SnapshotHash: "snapshot",
			Summary: map[string]any{
				"account_ids": []int64{7}, "channel_ids": []int64{3},
				"display_multiplier_decimals": 3, "snapshot_max_age_minutes": 60,
				"config_updated_at": cfg.UpdatedAt.Format(time.RFC3339Nano), "model_catalog_revision": int64(1),
				"account_ledger_hashes":   map[string]string{"7": UpstreamPriceCredentialLedgerHash(account)},
				"account_identity_hashes": map[string]string{"7": UpstreamPriceAccountIdentityHash(account)},
			},
		},
	}
	svc := NewUpstreamPriceMonitorService(repo, upstreamPriceAutoApplyAccountReader{account: account}, nil)
	invalidator := &upstreamPriceCacheInvalidator{}
	svc.SetPricingCacheInvalidator(invalidator)

	applied, err := svc.ApplyRun(context.Background(), 77, "snapshot")
	require.NoError(t, err)
	require.NotNil(t, applied.AppliedAt)
	require.Equal(t, 1, repo.applyCalls)
	require.Equal(t, 1, invalidator.calls)

	rolledBack, err := svc.RollbackRun(context.Background(), 77, "snapshot")
	require.NoError(t, err)
	require.Nil(t, rolledBack.AppliedAt)
	require.Equal(t, 1, repo.rollbackCalls)
	require.Equal(t, 2, invalidator.calls)

	rotated := testUpstreamPriceAccount()
	rotated.Credentials["api_key"] = "rotated-key-not-real"
	svc = NewUpstreamPriceMonitorService(repo, upstreamPriceAutoApplyAccountReader{account: rotated}, nil)
	_, err = svc.ApplyRun(context.Background(), 77, "snapshot")
	require.ErrorIs(t, err, ErrUpstreamPriceSnapshotMismatch)
	require.Equal(t, 1, repo.applyCalls)
}
