package service

import (
	"context"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/domain"
	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/stretchr/testify/require"
)

type upstreamPriceRolloutGuardRepository struct {
	*activeProbeTestRepository
	config           *domain.UpstreamPriceMonitorConfig
	updated          *domain.UpstreamPriceMonitorConfig
	catalog          []domain.UpstreamPriceModelCatalogEntry
	created          *domain.UpstreamPriceMonitorRun
	finished         *domain.UpstreamPriceMonitorRun
	freezeChannelIDs []int64
}

func (r *upstreamPriceRolloutGuardRepository) GetConfig(context.Context) (*domain.UpstreamPriceMonitorConfig, error) {
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

func (r *upstreamPriceRolloutGuardRepository) CreateRun(_ context.Context, run *domain.UpstreamPriceMonitorRun) error {
	run.ID = 77
	copy := *run
	r.created = &copy
	return nil
}

func (r *upstreamPriceRolloutGuardRepository) FinishRun(_ context.Context, run *domain.UpstreamPriceMonitorRun) error {
	copy := *run
	r.finished = &copy
	return nil
}

func (r *upstreamPriceRolloutGuardRepository) FreezeEvidenceApplySnapshot(
	_ context.Context,
	_ int64,
	channelIDs []int64,
	_ int,
) ([]domain.UpstreamPriceEvidence, error) {
	r.freezeChannelIDs = append([]int64(nil), channelIDs...)
	return nil, nil
}

type upstreamPriceRolloutAccountReader struct{}

func (upstreamPriceRolloutAccountReader) GetByID(context.Context, int64) (*Account, error) {
	return nil, nil
}

func (r *upstreamPriceRolloutGuardRepository) UpdateConfig(_ context.Context, cfg *domain.UpstreamPriceMonitorConfig) error {
	copy := *cfg
	copy.AccountIDs = append([]int64(nil), cfg.AccountIDs...)
	copy.ChannelIDs = append([]int64(nil), cfg.ChannelIDs...)
	copy.DomesticModels = append([]string(nil), cfg.DomesticModels...)
	r.updated = &copy
	return nil
}

func (r *upstreamPriceRolloutGuardRepository) ListModelCatalog(context.Context) ([]domain.UpstreamPriceModelCatalogEntry, error) {
	return append([]domain.UpstreamPriceModelCatalogEntry(nil), r.catalog...), nil
}

func TestUpstreamPriceMonitorUpdateConfigRejectsActiveProbeDuringInitialRollout(t *testing.T) {
	repo := &upstreamPriceRolloutGuardRepository{activeProbeTestRepository: &activeProbeTestRepository{}}
	svc := NewUpstreamPriceMonitorService(repo, nil, nil)
	cfg := domain.DefaultUpstreamPriceMonitorConfig()
	cfg.ActiveProbeEnabled = true

	err := svc.UpdateConfig(context.Background(), &cfg)

	require.ErrorIs(t, err, ErrUpstreamPriceActiveProbeRolloutLocked)
	require.Equal(t, "UPSTREAM_PRICE_ACTIVE_PROBE_ROLLOUT_LOCKED", infraerrors.Reason(err))
	require.Nil(t, repo.updated)
}

func TestUpstreamPriceMonitorUpdateConfigRejectsAutoApplyDuringInitialRollout(t *testing.T) {
	repo := &upstreamPriceRolloutGuardRepository{activeProbeTestRepository: &activeProbeTestRepository{}}
	svc := NewUpstreamPriceMonitorService(repo, nil, nil)
	cfg := domain.DefaultUpstreamPriceMonitorConfig()
	cfg.Mode = domain.UpstreamPriceMonitorModeAutoApply

	err := svc.UpdateConfig(context.Background(), &cfg)

	require.ErrorIs(t, err, ErrUpstreamPriceAutoApplyRolloutLocked)
	require.Equal(t, "UPSTREAM_PRICE_AUTO_APPLY_ROLLOUT_LOCKED", infraerrors.Reason(err))
	require.Nil(t, repo.updated)
}

func TestUpstreamPriceMonitorUpdateConfigPersistsObserveOnlyConfig(t *testing.T) {
	repo := &upstreamPriceRolloutGuardRepository{
		activeProbeTestRepository: &activeProbeTestRepository{},
		catalog: []domain.UpstreamPriceModelCatalogEntry{{
			ModelName: "MiniMax-M3",
			Status:    domain.UpstreamPriceModelStatusManaged,
		}},
	}
	svc := NewUpstreamPriceMonitorService(repo, nil, nil)
	cfg := domain.DefaultUpstreamPriceMonitorConfig()
	cfg.Enabled = true
	cfg.DomesticModels = []string{"client-supplied-model-is-replaced"}

	err := svc.UpdateConfig(context.Background(), &cfg)

	require.NoError(t, err)
	require.NotNil(t, repo.updated)
	require.Equal(t, domain.UpstreamPriceMonitorModeObserve, repo.updated.Mode)
	require.False(t, repo.updated.ActiveProbeEnabled)
	require.Equal(t, []string{"MiniMax-M3"}, repo.updated.DomesticModels)
}

func TestUpstreamPriceMonitorRunOnceForcesObserveOnlyDespitePersistedUnsafeConfig(t *testing.T) {
	cfg := domain.DefaultUpstreamPriceMonitorConfig()
	cfg.Enabled = true
	cfg.Mode = domain.UpstreamPriceMonitorModeAutoApply
	cfg.ActiveProbeEnabled = true
	cfg.ChannelIDs = nil
	repo := &upstreamPriceRolloutGuardRepository{
		activeProbeTestRepository: &activeProbeTestRepository{},
		config:                    &cfg,
	}
	remote := &activeProbeScript{now: time.Date(2026, 8, 30, 2, 0, 0, 0, time.UTC)}
	svc := NewUpstreamPriceMonitorService(repo, upstreamPriceRolloutAccountReader{}, remote)
	svc.SetActiveProber(remote)

	run, err := svc.RunOnce(context.Background(), UpstreamPriceRunOptions{
		Trigger: domain.UpstreamPriceMonitorRunTriggerManual,
		DryRun:  false,
	})

	require.Error(t, err) // Empty account scope has no model-catalog revision, but the run is still safely finalized.
	require.NotNil(t, run)
	require.NotNil(t, repo.created)
	require.Equal(t, domain.UpstreamPriceMonitorModeObserve, repo.created.Mode)
	require.True(t, repo.created.DryRun)
	require.Equal(t, 0, remote.step, "runtime rollout guard must prevent every active probe")
	require.Empty(t, repo.freezeChannelIDs, "observe-only runs may freeze an empty apply snapshot")
	require.NotNil(t, repo.finished)
	require.True(t, repo.finished.DryRun)
	require.Equal(t, true, repo.finished.Summary["observe_only"])
}

func TestUpstreamPriceMonitorApplyAndRollbackAreIndependentlyLocked(t *testing.T) {
	repo := &upstreamPriceRolloutGuardRepository{activeProbeTestRepository: &activeProbeTestRepository{}}
	svc := NewUpstreamPriceMonitorService(repo, nil, nil)

	_, applyErr := svc.ApplyRun(context.Background(), 7, "snapshot")
	require.ErrorIs(t, applyErr, ErrUpstreamPriceApplyRolloutLocked)
	require.Equal(t, "UPSTREAM_PRICE_APPLY_ROLLOUT_LOCKED", infraerrors.Reason(applyErr))

	_, rollbackErr := svc.RollbackRun(context.Background(), 7, "snapshot")
	require.ErrorIs(t, rollbackErr, ErrUpstreamPriceRollbackRolloutLocked)
	require.Equal(t, "UPSTREAM_PRICE_ROLLBACK_ROLLOUT_LOCKED", infraerrors.Reason(rollbackErr))
}
