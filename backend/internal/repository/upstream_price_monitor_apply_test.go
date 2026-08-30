package repository

import (
	"context"
	"database/sql"
	"regexp"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/Wei-Shaw/sub2api/internal/domain"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestApplyUpstreamTokenBaseIntervalsPreservesMultiplierOnlyFields(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	mock.ExpectBegin()
	tx, err := db.BeginTx(context.Background(), nil)
	require.NoError(t, err)

	mock.ExpectQuery(`SELECT id,input_price::text,output_price::text`).
		WithArgs(int64(44)).
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "input_price", "output_price", "cache_write_price", "cache_read_price", "per_request_price",
			"has_input_multiplier", "has_output_multiplier", "has_cache_write_multiplier", "has_cache_read_multiplier",
		}).AddRow(int64(101), nil, "0.000002", nil, nil, nil, true, false, false, false))
	mock.ExpectExec(`UPDATE channel_pricing_intervals SET`).
		WithArgs(int64(101), nil, "0.000003", nil, nil).
		WillReturnResult(sqlmock.NewResult(0, 1))

	snapshot := &upstreamPriceRollbackSnapshot{}
	changed, err := applyUpstreamTokenBaseIntervals(
		context.Background(), tx, 44, "0.000001", "0.000003", nil, nil, snapshot,
	)
	require.NoError(t, err)
	require.True(t, changed)
	require.Len(t, snapshot.Intervals, 1)
	require.Nil(t, snapshot.Intervals[0].InputPrice)
	require.Equal(t, "0.000002", *snapshot.Intervals[0].OutputPrice)

	mock.ExpectRollback()
	require.NoError(t, tx.Rollback())
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestCalculateDisplayMultiplierUsesHalfUpRounding(t *testing.T) {
	input := 0.0135
	multiplier, ok, err := calculateDisplayMultiplier(upstreamPriceApplyEvidence{
		Model: "domestic-model", BillingMode: service.DisplayBillingModeToken,
		Suggested: domain.UpstreamPriceVector{InputPerMillion: &input},
	}, sql.NullString{String: "1", Valid: true}, sql.NullString{}, 3)
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, "0.014", multiplier.StringFixed(3))
}

func TestMergeMaximumCommonUpstreamPriceVectorProtectsEveryAccountMargin(t *testing.T) {
	aInput, aOutput, aCache := 0.2, 0.8, 0.03
	bInput, bOutput := 0.25, 0.7
	merged := mergeMaximumCommonUpstreamPriceVector([]domain.UpstreamPriceVector{
		{InputPerMillion: &aInput, OutputPerMillion: &aOutput, CacheReadPerMillion: &aCache},
		{InputPerMillion: &bInput, OutputPerMillion: &bOutput},
	})
	require.InDelta(t, 0.25, *merged.InputPerMillion, 1e-12)
	require.InDelta(t, 0.8, *merged.OutputPerMillion, 1e-12)
	require.Nil(t, merged.CacheReadPerMillion, "a dimension missing on any selected account must remain unchanged")
}

func TestMergeCompatibleUpstreamPriceVectorsCombinesPassiveAndActiveDimensions(t *testing.T) {
	input, output, cacheWrite, cacheRead := 0.2, 0.8, 0.4, 0.05
	merged, ok := mergeCompatibleUpstreamPriceVectors(
		domain.UpstreamPriceVector{InputPerMillion: &input, OutputPerMillion: &output, CacheReadPerMillion: &cacheRead},
		domain.UpstreamPriceVector{InputPerMillion: &input, OutputPerMillion: &output, CacheWritePerMillion: &cacheWrite},
	)
	require.True(t, ok)
	require.InDelta(t, 0.4, *merged.CacheWritePerMillion, 1e-12)
	require.InDelta(t, 0.05, *merged.CacheReadPerMillion, 1e-12)

	conflicting := 0.25
	_, ok = mergeCompatibleUpstreamPriceVectors(
		domain.UpstreamPriceVector{InputPerMillion: &input},
		domain.UpstreamPriceVector{InputPerMillion: &conflicting},
	)
	require.False(t, ok)
}

func TestValidateUpstreamPriceApplyEvidenceRejectsZero(t *testing.T) {
	zero := 0.0
	err := validateUpstreamPriceApplyEvidence([]upstreamPriceApplyEvidence{{
		Model: "deepseek-zero", BillingMode: service.DisplayBillingModeToken,
		Suggested: domain.UpstreamPriceVector{InputPerMillion: &zero},
	}}, false)
	require.ErrorIs(t, err, service.ErrUpstreamPriceRunNotApplicable)
}

func TestValidateUpstreamPriceApplyEvidenceCapsAutomaticChanges(t *testing.T) {
	current, extreme := 1.0, 1.2000001
	item := upstreamPriceApplyEvidence{
		Model: "deepseek-auto", BillingMode: service.DisplayBillingModeToken,
		Current:   domain.UpstreamPriceVector{InputPerMillion: &current},
		Suggested: domain.UpstreamPriceVector{InputPerMillion: &extreme},
	}
	err := validateUpstreamPriceApplyEvidence([]upstreamPriceApplyEvidence{item}, true)
	require.ErrorIs(t, err, service.ErrUpstreamPriceRunNotApplicable)

	extreme = 9
	item.Suggested.InputPerMillion = &extreme
	require.NoError(t, validateUpstreamPriceApplyEvidence([]upstreamPriceApplyEvidence{item}, false),
		"manual apply may accept a large positive change")
}

func TestAssertTokenChannelSnapshotRejectsCASConflict(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	mock.ExpectBegin()
	tx, err := db.BeginTx(context.Background(), nil)
	require.NoError(t, err)

	mock.ExpectQuery(`FROM channel_pricing_intervals WHERE pricing_id=\$1 AND min_tokens=0`).
		WithArgs(int64(77)).
		WillReturnRows(sqlmock.NewRows([]string{
			"input_price", "output_price", "cache_write_price", "cache_read_price",
			"input_multiplier", "output_multiplier", "cache_write_multiplier", "cache_read_multiplier",
		}))
	expected := 2.0
	err = assertTokenChannelSnapshot(context.Background(), tx, upstreamPriceChannelRollback{
		ID: int64(77), InputPrice: stringPointer("0.000001"),
	}, domain.UpstreamPriceVector{InputPerMillion: &expected})
	require.ErrorIs(t, err, service.ErrUpstreamPriceSnapshotMismatch)

	mock.ExpectRollback()
	require.NoError(t, tx.Rollback())
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestAssertDisplayMultiplierSnapshotPreservesNullSemantics(t *testing.T) {
	current, changed := 0.2, 0.3
	require.NoError(t, assertDisplayMultiplierSnapshot("deepseek-display", &current, &current))
	require.ErrorIs(t, assertDisplayMultiplierSnapshot("deepseek-display", &current, &changed),
		service.ErrUpstreamPriceSnapshotMismatch)
	require.ErrorIs(t, assertDisplayMultiplierSnapshot("deepseek-display", nil, &current),
		service.ErrUpstreamPriceSnapshotMismatch)
}

func TestAssertPerRequestDisplaySnapshotPreservesNullSemantics(t *testing.T) {
	low, middle, high := 0.01, 0.015, 0.02
	expected := domain.UpstreamPriceVector{
		PerRequestLTE256K: &low, PerRequest256K512K: &middle, PerRequestGT512K: &high,
	}
	require.NoError(t, assertPerRequestDisplaySnapshot("deepseek-request", expected, expected))
	changed := expected
	changed.PerRequestGT512K = nil
	require.ErrorIs(t, assertPerRequestDisplaySnapshot("deepseek-request", expected, changed),
		service.ErrUpstreamPriceSnapshotMismatch)
}

func TestLoadLockedUpstreamChannelPricingRowsFiltersOpenAIPlatform(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	mock.ExpectBegin()
	tx, err := db.BeginTx(context.Background(), nil)
	require.NoError(t, err)

	mock.ExpectQuery(regexp.QuoteMeta(`WHERE channel_id=$1 AND platform='openai' AND billing_mode=$2`)).
		WithArgs(int64(8), service.DisplayBillingModeToken, "deepseek-openai").
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "input_price", "output_price", "cache_write_price", "cache_read_price", "per_request_price",
		}).AddRow(int64(91), "0.000001", "0.000002", nil, nil, nil))

	rows, err := loadLockedUpstreamChannelPricingRows(
		context.Background(), tx, 8, service.DisplayBillingModeToken, "deepseek-openai",
	)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.Equal(t, int64(91), rows[0].ID)

	mock.ExpectRollback()
	require.NoError(t, tx.Rollback())
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestFreezeEvidenceApplySnapshotPersistsCurrentPricesAndMultiplier(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	mock.ExpectBegin()
	now := time.Now().UTC()
	mock.ExpectQuery(`SELECT id,run_id,account_id,model_name,billing_mode,status,source`).
		WithArgs(int64(12)).
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "run_id", "account_id", "model_name", "billing_mode", "status", "source",
			"reconciliation_status", "context_key", "observed_at", "sample_count", "local_delta",
			"remote_delta", "prices", "current_prices", "suggested_prices", "display_prices_current", "display_multiplier_current",
			"display_multiplier_suggested", "last_error", "created_at",
		}).AddRow(int64(32), int64(12), int64(0), "deepseek-request-freeze", service.DisplayBillingModePerRequest,
			string(domain.UpstreamPriceEvidenceStatusTrusted), string(domain.UpstreamPriceEvidenceSourcePricePage),
			string(domain.UpstreamPriceReconciliationMatched), "price-page", now, 1, []byte(`{}`), []byte(`{}`),
			[]byte(`{}`), []byte(`{}`), []byte(`{"per_request_lte_256k":0.012,"per_request_256k_512k":0.018,"per_request_gt_512k":0.024}`),
			[]byte(`{}`), nil, nil, "", now).
			AddRow(int64(31), int64(12), int64(3), "deepseek-freeze", service.DisplayBillingModeToken,
				string(domain.UpstreamPriceEvidenceStatusTrusted), string(domain.UpstreamPriceEvidenceSourceUserRequest),
				string(domain.UpstreamPriceReconciliationMatched), "ctx", now, 2, []byte(`{}`), []byte(`{}`),
				[]byte(`{}`), []byte(`{}`), []byte(`{"input_per_million":1.2}`), []byte(`{}`), nil, nil, "", now))
	mock.ExpectQuery(`(?s)SELECT LOWER\(cmp.models->>0\),cmp.billing_mode,.*cmp.platform='openai'`).
		WillReturnRows(sqlmock.NewRows([]string{
			"model", "billing_mode", "input", "output", "cache_write", "cache_read", "low", "middle", "high",
		}).AddRow("deepseek-request-freeze", service.DisplayBillingModePerRequest, nil, nil, nil, nil, 0.01, 0.015, 0.02).
			AddRow("deepseek-freeze", service.DisplayBillingModeToken, 1.0, 2.0, nil, nil, nil, nil, nil))
	mock.ExpectQuery(`(?s)SELECT LOWER\(d.model_name\).*d.platform='openai'`).
		WillReturnRows(sqlmock.NewRows([]string{
			"model", "billing_mode", "multiplier", "official_input", "official_output", "low", "middle", "high",
		}).AddRow("deepseek-request-freeze", service.DisplayBillingModePerRequest, 1, nil, nil, 0.01, 0.015, 0.02).
			AddRow("deepseek-freeze", service.DisplayBillingModeToken, 0.5, "2", "4", nil, nil, nil))
	mock.ExpectExec(`UPDATE upstream_price_monitor_evidence SET`).
		WithArgs(int64(32), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), int64(12)).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`UPDATE upstream_price_monitor_evidence SET`).
		WithArgs(int64(31), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), int64(12)).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	repo := &upstreamPriceMonitorRepository{db: db}
	evidence, err := repo.FreezeEvidenceApplySnapshot(context.Background(), 12, []int64{8}, 3)
	require.NoError(t, err)
	require.Len(t, evidence, 2)
	require.NotNil(t, evidence[0].DisplayPricesCurrent.PerRequestLTE256K)
	require.InDelta(t, 0.01, *evidence[0].DisplayPricesCurrent.PerRequestLTE256K, 1e-12)
	require.NotNil(t, evidence[1].CurrentPrices.InputPerMillion)
	require.InDelta(t, 1, *evidence[1].CurrentPrices.InputPerMillion, 1e-12)
	require.NotNil(t, evidence[1].DisplayMultiplierCurrent)
	require.InDelta(t, 0.5, *evidence[1].DisplayMultiplierCurrent, 1e-12)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestFreezeEvidenceApplySnapshotAllowsObserveOnlyRunWithoutChannels(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	mock.ExpectBegin()
	now := time.Now().UTC()
	mock.ExpectQuery(`SELECT id,run_id,account_id,model_name,billing_mode,status,source`).
		WithArgs(int64(14)).
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "run_id", "account_id", "model_name", "billing_mode", "status", "source",
			"reconciliation_status", "context_key", "observed_at", "sample_count", "local_delta",
			"remote_delta", "prices", "current_prices", "suggested_prices", "display_prices_current", "display_multiplier_current",
			"display_multiplier_suggested", "last_error", "created_at",
		}).AddRow(int64(41), int64(14), int64(3), "deepseek-observe", service.DisplayBillingModeToken,
			string(domain.UpstreamPriceEvidenceStatusTrusted), string(domain.UpstreamPriceEvidenceSourceUserRequest),
			string(domain.UpstreamPriceReconciliationMatched), "ctx", now, 1, []byte(`{}`), []byte(`{}`),
			[]byte(`{"input_per_million":1}`), []byte(`{}`), []byte(`{"input_per_million":1.2}`),
			[]byte(`{}`), nil, nil, "", now))
	mock.ExpectExec(`UPDATE upstream_price_monitor_evidence SET`).
		WithArgs(int64(41), []byte(`{}`), []byte(`{}`), nil, int64(14)).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	repo := &upstreamPriceMonitorRepository{db: db}
	evidence, err := repo.FreezeEvidenceApplySnapshot(context.Background(), 14, nil, 3)
	require.NoError(t, err)
	require.Len(t, evidence, 1)
	require.Empty(t, evidence[0].CurrentPrices)
	require.Empty(t, evidence[0].DisplayPricesCurrent)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestListEvidenceByRunKeepsCompletedSnapshotFrozen(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	now := time.Now().UTC()
	mock.ExpectQuery(`SELECT id,run_id,account_id,model_name,billing_mode,status,source`).
		WithArgs(int64(21)).
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "run_id", "account_id", "model_name", "billing_mode", "status", "source",
			"reconciliation_status", "context_key", "observed_at", "sample_count", "local_delta",
			"remote_delta", "prices", "current_prices", "suggested_prices", "display_prices_current", "display_multiplier_current",
			"display_multiplier_suggested", "last_error", "created_at",
		}).AddRow(int64(51), int64(21), int64(3), "deepseek-frozen", service.DisplayBillingModeToken,
			string(domain.UpstreamPriceEvidenceStatusTrusted), string(domain.UpstreamPriceEvidenceSourceUserRequest),
			string(domain.UpstreamPriceReconciliationMatched), "ctx", now, 1, []byte(`{}`), []byte(`{}`),
			[]byte(`{"input_per_million":1}`), []byte(`{"input_per_million":0.75}`),
			[]byte(`{"input_per_million":1.2}`), []byte(`{}`), 0.25, 0.4, "", now))
	summary := []byte(`{"account_ids":[3],"account_ledger_hashes":{"3":"ledger"},"account_identity_hashes":{"3":"identity"},"channel_ids":[8],"display_multiplier_decimals":3,"snapshot_max_age_minutes":60,"config_updated_at":"2026-08-30T00:00:00Z","model_catalog_revision":1}`)
	mock.ExpectQuery(`SELECT id,trigger,status,mode,dry_run,started_at,finished_at`).
		WithArgs(int64(21)).
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "trigger", "status", "mode", "dry_run", "started_at", "finished_at",
			"matched_models", "mismatched_models", "probed_models", "probe_cost", "snapshot_hash",
			"summary", "error", "applied_at", "rollback_available",
		}).AddRow(int64(21), string(domain.UpstreamPriceMonitorRunTriggerManual),
			string(domain.UpstreamPriceMonitorRunStatusCompleted), string(domain.UpstreamPriceMonitorModeObserve),
			true, now, now, 1, 0, 0, 0, "hash", summary, "", nil, false))

	repo := &upstreamPriceMonitorRepository{db: db}
	evidence, err := repo.ListEvidenceByRun(context.Background(), 21)
	require.NoError(t, err)
	require.Len(t, evidence, 1)
	require.NotNil(t, evidence[0].CurrentPrices.InputPerMillion)
	require.InDelta(t, 0.75, *evidence[0].CurrentPrices.InputPerMillion, 1e-12)
	require.NotNil(t, evidence[0].DisplayMultiplierCurrent)
	require.InDelta(t, 0.25, *evidence[0].DisplayMultiplierCurrent, 1e-12)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestListEvidenceByRunMayEnrichRunningRun(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	now := time.Now().UTC()
	mock.ExpectQuery(`SELECT id,run_id,account_id,model_name,billing_mode,status,source`).
		WithArgs(int64(22)).
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "run_id", "account_id", "model_name", "billing_mode", "status", "source",
			"reconciliation_status", "context_key", "observed_at", "sample_count", "local_delta",
			"remote_delta", "prices", "current_prices", "suggested_prices", "display_prices_current", "display_multiplier_current",
			"display_multiplier_suggested", "last_error", "created_at",
		}).AddRow(int64(52), int64(22), int64(3), "deepseek-running", service.DisplayBillingModeToken,
			string(domain.UpstreamPriceEvidenceStatusTrusted), string(domain.UpstreamPriceEvidenceSourceUserRequest),
			string(domain.UpstreamPriceReconciliationMatched), "ctx", now, 1, []byte(`{}`), []byte(`{}`),
			[]byte(`{"input_per_million":1}`), []byte(`{}`), []byte(`{"input_per_million":1.2}`),
			[]byte(`{}`), nil, 0.4, "", now))
	summary := []byte(`{"account_ids":[3],"account_ledger_hashes":{"3":"ledger"},"account_identity_hashes":{"3":"identity"},"channel_ids":[8],"display_multiplier_decimals":3,"snapshot_max_age_minutes":60,"config_updated_at":"2026-08-30T00:00:00Z","model_catalog_revision":1}`)
	mock.ExpectQuery(`SELECT id,trigger,status,mode,dry_run,started_at,finished_at`).
		WithArgs(int64(22)).
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "trigger", "status", "mode", "dry_run", "started_at", "finished_at",
			"matched_models", "mismatched_models", "probed_models", "probe_cost", "snapshot_hash",
			"summary", "error", "applied_at", "rollback_available",
		}).AddRow(int64(22), string(domain.UpstreamPriceMonitorRunTriggerManual),
			string(domain.UpstreamPriceMonitorRunStatusRunning), string(domain.UpstreamPriceMonitorModeObserve),
			true, now, nil, 0, 0, 0, 0, "", summary, "", nil, false))
	mock.ExpectQuery(`(?s)SELECT LOWER\(cmp.models->>0\),cmp.billing_mode,.*cmp.platform='openai'`).
		WithArgs(sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{
			"model", "billing_mode", "input", "output", "cache_write", "cache_read", "low", "middle", "high",
		}).AddRow("deepseek-running", service.DisplayBillingModeToken, 1.5, 3.0, nil, nil, nil, nil, nil))
	mock.ExpectQuery(`(?s)SELECT LOWER\(d.model_name\).*d.platform='openai'`).
		WithArgs(sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{
			"model", "billing_mode", "multiplier", "official_input", "official_output", "low", "middle", "high",
		}).AddRow("deepseek-running", service.DisplayBillingModeToken, 0.5, "3", "6", nil, nil, nil))

	repo := &upstreamPriceMonitorRepository{db: db}
	evidence, err := repo.ListEvidenceByRun(context.Background(), 22)
	require.NoError(t, err)
	require.Len(t, evidence, 1)
	require.NotNil(t, evidence[0].CurrentPrices.InputPerMillion)
	require.InDelta(t, 1.5, *evidence[0].CurrentPrices.InputPerMillion, 1e-12)
	require.NotNil(t, evidence[0].DisplayMultiplierCurrent)
	require.InDelta(t, 0.5, *evidence[0].DisplayMultiplierCurrent, 1e-12)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestResolveDisplayMultiplierFiltersOpenAIPlatform(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	mock.ExpectBegin()
	tx, err := db.BeginTx(context.Background(), nil)
	require.NoError(t, err)
	mock.ExpectQuery(`(?s)FROM display_model_prices WHERE platform='openai'.*billing_mode='token'`).
		WithArgs("deepseek-platform-safe").
		WillReturnRows(sqlmock.NewRows([]string{"official_input", "official_output"}).AddRow("2", "4"))
	input, output := 1.0, 2.0
	multiplier, ok, err := resolveDisplayMultiplier(context.Background(), tx, upstreamPriceApplyEvidence{
		Model: "deepseek-platform-safe",
		Suggested: domain.UpstreamPriceVector{
			InputPerMillion: &input, OutputPerMillion: &output,
		},
	}, 3)
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, "0.5", multiplier.String())
	mock.ExpectRollback()
	require.NoError(t, tx.Rollback())
	require.NoError(t, mock.ExpectationsWereMet())
}

func stringPointer(value string) *string { return &value }
