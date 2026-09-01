package repository

import (
	"context"
	"database/sql"
	"encoding/json"
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

func TestLoadTrustedApplyEvidenceSelectsOnlyFinalActiveTokenEvidence(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	mock.ExpectBegin()
	tx, err := db.BeginTx(context.Background(), nil)
	require.NoError(t, err)

	mock.ExpectQuery(`WHERE run_id=\$1 AND source='active_probe' AND billing_mode='token' AND status='trusted'`).
		WithArgs(int64(71)).
		WillReturnRows(sqlmock.NewRows([]string{
			"account_id", "model_name", "billing_mode", "status", "source", "context_key",
			"prices", "current_prices", "suggested_prices", "display_prices_current", "display_multiplier_current",
		}).AddRow(int64(3), "mimo-v2.5-pro", service.DisplayBillingModeToken,
			string(domain.UpstreamPriceEvidenceStatusTrusted), string(domain.UpstreamPriceEvidenceSourceActiveProbe),
			"active-final", []byte(`{"cache_read_per_million":0.05}`),
			[]byte(`{"input_per_million":0.12,"cache_read_per_million":0.04}`),
			[]byte(`{"cache_read_per_million":0.06}`), []byte(`{}`), nil))

	items, skipped, err := loadTrustedApplyEvidence(context.Background(), tx, 71, []int64{3})
	require.NoError(t, err)
	require.Len(t, items, 1)
	require.Empty(t, skipped)
	require.Equal(t, "mimo-v2.5-pro", items[0].Model)
	require.Nil(t, items[0].Suggested.InputPerMillion, "an unobserved dimension must preserve the channel value")
	require.NotNil(t, items[0].Suggested.CacheReadPerMillion)
	require.InDelta(t, 0.06, *items[0].Suggested.CacheReadPerMillion, 1e-12)

	mock.ExpectRollback()
	require.NoError(t, tx.Rollback())
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestLoadTrustedApplyEvidenceSkipsFixedFeeModelAndKeepsOtherModels(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	mock.ExpectBegin()
	tx, err := db.BeginTx(context.Background(), nil)
	require.NoError(t, err)

	mock.ExpectQuery(`WHERE run_id=\$1 AND source='active_probe' AND billing_mode='token' AND status='trusted'`).
		WithArgs(int64(72)).
		WillReturnRows(sqlmock.NewRows([]string{
			"account_id", "model_name", "billing_mode", "status", "source", "context_key",
			"prices", "current_prices", "suggested_prices", "display_prices_current", "display_multiplier_current",
		}).AddRow(int64(3), "fixed-fee-model", service.DisplayBillingModeToken,
			string(domain.UpstreamPriceEvidenceStatusTrusted), string(domain.UpstreamPriceEvidenceSourceActiveProbe),
			"active-final", []byte(`{"fixed_per_request":0.001,"input_per_million":0.2}`),
			[]byte(`{"input_per_million":0.1}`),
			[]byte(`{"fixed_per_request":0.0012,"input_per_million":0.24}`), []byte(`{}`), nil).
			AddRow(int64(3), "representable-model", service.DisplayBillingModeToken,
				string(domain.UpstreamPriceEvidenceStatusTrusted), string(domain.UpstreamPriceEvidenceSourceActiveProbe),
				"active-final", []byte(`{"input_per_million":0.2}`),
				[]byte(`{"input_per_million":0.1}`), []byte(`{"input_per_million":0.24}`), []byte(`{}`), nil))

	items, skipped, err := loadTrustedApplyEvidence(context.Background(), tx, 72, []int64{3})
	require.NoError(t, err)
	require.Len(t, items, 1)
	require.Equal(t, "representable-model", items[0].Model)
	require.Equal(t, []upstreamPriceApplySkippedModel{{
		Model: "fixed-fee-model", Reason: "fixed_request_fee_not_representable", FixedPerRequest: 0.001,
	}}, skipped)

	mock.ExpectRollback()
	require.NoError(t, tx.Rollback())
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestApplyRunAllFixedFeeModelsRecordsSkipSummaryThenReturnsNotApplicable(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	now := time.Now().UTC()
	configUpdatedAt := now.Add(-time.Hour)

	mock.ExpectBegin()
	mock.ExpectExec(`SELECT pg_advisory_xact_lock`).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery(`SELECT trigger,status,snapshot_hash,applied_at,finished_at`).
		WithArgs(int64(75)).
		WillReturnRows(sqlmock.NewRows([]string{"trigger", "status", "snapshot_hash", "applied_at", "finished_at"}).
			AddRow(string(domain.UpstreamPriceMonitorRunTriggerScheduled), string(domain.UpstreamPriceMonitorRunStatusCompleted),
				"snapshot", nil, now))
	mock.ExpectQuery(`SELECT EXISTS\(SELECT 1 FROM upstream_price_monitor_runs`).
		WithArgs(now, int64(75)).
		WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(false))
	mock.ExpectQuery(`SELECT enabled,mode,updated_at FROM upstream_price_monitor_config`).
		WillReturnRows(sqlmock.NewRows([]string{"enabled", "mode", "updated_at"}).
			AddRow(true, string(domain.UpstreamPriceMonitorModeAutoApply), configUpdatedAt))
	mock.ExpectQuery(`SELECT revision FROM upstream_price_monitor_model_scan_state`).
		WillReturnRows(sqlmock.NewRows([]string{"revision"}).AddRow(int64(1)))
	mock.ExpectQuery(`WHERE run_id=\$1 AND source='active_probe' AND billing_mode='token' AND status='trusted'`).
		WithArgs(int64(75)).
		WillReturnRows(sqlmock.NewRows([]string{
			"account_id", "model_name", "billing_mode", "status", "source", "context_key",
			"prices", "current_prices", "suggested_prices", "display_prices_current", "display_multiplier_current",
		}).AddRow(int64(3), "fixed-only", service.DisplayBillingModeToken,
			string(domain.UpstreamPriceEvidenceStatusTrusted), string(domain.UpstreamPriceEvidenceSourceActiveProbe),
			"active-final", []byte(`{"fixed_per_request":0.001,"input_per_million":0.2}`),
			[]byte(`{"input_per_million":0.1}`),
			[]byte(`{"fixed_per_request":0.0012,"input_per_million":0.24}`), []byte(`{}`), nil))
	mock.ExpectExec(`summary=jsonb_set\(summary,'\{skipped_models\}',\$2::jsonb,true\)`).
		WithArgs(int64(75), sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	repo := &upstreamPriceMonitorRepository{db: db}
	err = repo.ApplyRun(context.Background(), 75, "snapshot", []int64{8}, []int64{3}, 3, 60, configUpdatedAt, 1)
	require.ErrorIs(t, err, service.ErrUpstreamPriceRunNotApplicable)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestApplyRunObserveModeNeverWritesPrices(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	now := time.Now().UTC()
	configUpdatedAt := now.Add(-time.Hour)

	mock.ExpectBegin()
	mock.ExpectExec(`SELECT pg_advisory_xact_lock`).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery(`SELECT trigger,status,snapshot_hash,applied_at,finished_at`).
		WithArgs(int64(76)).
		WillReturnRows(sqlmock.NewRows([]string{"trigger", "status", "snapshot_hash", "applied_at", "finished_at"}).
			AddRow(string(domain.UpstreamPriceMonitorRunTriggerManual), string(domain.UpstreamPriceMonitorRunStatusCompleted),
				"snapshot", nil, now))
	mock.ExpectQuery(`SELECT EXISTS\(SELECT 1 FROM upstream_price_monitor_runs`).
		WithArgs(now, int64(76)).
		WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(false))
	mock.ExpectQuery(`SELECT enabled,mode,updated_at FROM upstream_price_monitor_config`).
		WillReturnRows(sqlmock.NewRows([]string{"enabled", "mode", "updated_at"}).
			AddRow(true, string(domain.UpstreamPriceMonitorModeObserve), configUpdatedAt))
	mock.ExpectRollback()

	repo := &upstreamPriceMonitorRepository{db: db}
	err = repo.ApplyRun(context.Background(), 76, "snapshot", []int64{8}, []int64{3}, 3, 60, configUpdatedAt, 1)
	require.ErrorIs(t, err, service.ErrUpstreamPriceRunNotApplicable)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestLoadTrustedApplyEvidenceRejectsSuggestionNotEqualToMeasuredTimesMarkup(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	mock.ExpectBegin()
	tx, err := db.BeginTx(context.Background(), nil)
	require.NoError(t, err)

	mock.ExpectQuery(`WHERE run_id=\$1 AND source='active_probe' AND billing_mode='token' AND status='trusted'`).
		WithArgs(int64(73)).
		WillReturnRows(sqlmock.NewRows([]string{
			"account_id", "model_name", "billing_mode", "status", "source", "context_key",
			"prices", "current_prices", "suggested_prices", "display_prices_current", "display_multiplier_current",
		}).AddRow(int64(3), "bad-markup-model", service.DisplayBillingModeToken,
			string(domain.UpstreamPriceEvidenceStatusTrusted), string(domain.UpstreamPriceEvidenceSourceActiveProbe),
			"active-final", []byte(`{"input_per_million":0.2}`), []byte(`{"input_per_million":0.1}`),
			[]byte(`{"input_per_million":0.3}`), []byte(`{}`), nil))

	_, _, err = loadTrustedApplyEvidence(context.Background(), tx, 73, []int64{3})
	require.ErrorIs(t, err, service.ErrUpstreamPriceSnapshotMismatch)

	mock.ExpectRollback()
	require.NoError(t, tx.Rollback())
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestValidateUpstreamPriceApplyEvidenceRejectsZero(t *testing.T) {
	zero := 0.0
	err := validateUpstreamPriceApplyEvidence([]upstreamPriceApplyEvidence{{
		Model: "deepseek-zero", BillingMode: service.DisplayBillingModeToken,
		Suggested: domain.UpstreamPriceVector{InputPerMillion: &zero},
	}}, false)
	require.ErrorIs(t, err, service.ErrUpstreamPriceRunNotApplicable)
}

func TestValidateUpstreamPriceApplyEvidenceUsesAuthoritativeUpstreamPrice(t *testing.T) {
	current, extreme := 1.0, 1.2000001
	item := upstreamPriceApplyEvidence{
		Model: "deepseek-auto", BillingMode: service.DisplayBillingModeToken,
		Current:   domain.UpstreamPriceVector{InputPerMillion: &current},
		Suggested: domain.UpstreamPriceVector{InputPerMillion: &extreme},
	}
	err := validateUpstreamPriceApplyEvidence([]upstreamPriceApplyEvidence{item}, true)
	require.NoError(t, err)

	extreme = 9
	item.Suggested.InputPerMillion = &extreme
	require.NoError(t, validateUpstreamPriceApplyEvidence([]upstreamPriceApplyEvidence{item}, true),
		"a large positive upstream change must be applied, not blocked")

	current = 0
	extreme = 0.24
	item.Current.InputPerMillion = &current
	item.Suggested.InputPerMillion = &extreme
	require.NoError(t, validateUpstreamPriceApplyEvidence([]upstreamPriceApplyEvidence{item}, true),
		"a zero baseline must not block a positive authoritative upstream price")
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

func TestRollbackRunRejectsLegacyDisplaySnapshotWithoutMutatingDisplayPrices(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	now := time.Now().UTC()
	raw, err := json.Marshal(upstreamPriceRollbackSnapshot{
		Displays: []upstreamPriceDisplayRollback{{ID: 81, ModelMultiplier: stringPointer("0.1"), AfterModelMultiplier: stringPointer("0.12")}},
	})
	require.NoError(t, err)

	mock.ExpectBegin()
	mock.ExpectExec(`SELECT pg_advisory_xact_lock`).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery(`SELECT mode FROM upstream_price_monitor_config`).
		WillReturnRows(sqlmock.NewRows([]string{"mode"}).AddRow(string(domain.UpstreamPriceMonitorModeReview)))
	mock.ExpectQuery(`SELECT snapshot_hash,applied_at,rollback_available,rollback_snapshot`).
		WithArgs(int64(74)).
		WillReturnRows(sqlmock.NewRows([]string{"snapshot_hash", "applied_at", "rollback_available", "rollback_snapshot"}).
			AddRow("snapshot", now, true, raw))
	mock.ExpectQuery(`SELECT EXISTS\(SELECT 1 FROM upstream_price_monitor_runs`).
		WithArgs(now).
		WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(false))
	mock.ExpectRollback()

	repo := &upstreamPriceMonitorRepository{db: db}
	err = repo.RollbackRun(context.Background(), 74, "snapshot")
	require.ErrorIs(t, err, service.ErrUpstreamPriceRunNotApplicable)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestRollbackRunRestoresExactTokenDisplayOverrides(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	now := time.Now().UTC()
	raw, err := json.Marshal(upstreamPriceRollbackSnapshot{
		Displays: []upstreamPriceDisplayRollback{{
			ID: 82, ModelMultiplier: stringPointer("0.12"), AfterModelMultiplier: stringPointer("0.12"),
			DisplayInputPrice: stringPointer("0.10"), AfterDisplayInputPrice: stringPointer("0.20"),
		}},
	})
	require.NoError(t, err)

	mock.ExpectBegin()
	mock.ExpectExec(`SELECT pg_advisory_xact_lock`).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery(`SELECT mode FROM upstream_price_monitor_config`).
		WillReturnRows(sqlmock.NewRows([]string{"mode"}).AddRow(string(domain.UpstreamPriceMonitorModeReview)))
	mock.ExpectQuery(`SELECT snapshot_hash,applied_at,rollback_available,rollback_snapshot`).
		WithArgs(int64(78)).
		WillReturnRows(sqlmock.NewRows([]string{"snapshot_hash", "applied_at", "rollback_available", "rollback_snapshot"}).
			AddRow("snapshot", now, true, raw))
	mock.ExpectQuery(`SELECT EXISTS\(SELECT 1 FROM upstream_price_monitor_runs`).
		WithArgs(now).
		WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(false))
	mock.ExpectExec(`UPDATE display_model_prices SET`).
		WithArgs(int64(82), "0.10", nil, nil, nil, "0.12", "0.20", nil, nil, nil).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`UPDATE upstream_price_monitor_runs SET rollback_available=FALSE`).
		WithArgs(int64(78), sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	repo := &upstreamPriceMonitorRepository{db: db}
	err = repo.RollbackRun(context.Background(), 78, "snapshot")
	require.NoError(t, err)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestRollbackRunObserveModeNeverWritesPrices(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	mock.ExpectBegin()
	mock.ExpectExec(`SELECT pg_advisory_xact_lock`).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery(`SELECT mode FROM upstream_price_monitor_config`).
		WillReturnRows(sqlmock.NewRows([]string{"mode"}).AddRow(string(domain.UpstreamPriceMonitorModeObserve)))
	mock.ExpectRollback()

	repo := &upstreamPriceMonitorRepository{db: db}
	err = repo.RollbackRun(context.Background(), 79, "snapshot")
	require.ErrorIs(t, err, service.ErrUpstreamPriceRunNotApplicable)
	require.NoError(t, mock.ExpectationsWereMet())
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

func TestApplyUpstreamTokenDisplayOverridesUpdatesExactPricesOnly(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	mock.ExpectBegin()
	tx, err := db.BeginTx(context.Background(), nil)
	require.NoError(t, err)

	mock.ExpectQuery(`SELECT d.id,d.model_multiplier::text`).
		WithArgs("deepseek-display").
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "model_multiplier", "input", "output", "cache_write", "cache_read", "effective_multiplier",
			"actual_input", "actual_output", "actual_cache_write", "actual_cache_read",
		}).AddRow(int64(92), "0.12", "0.10000000", nil, nil, "0.03000000", 0.12, 0.1, nil, nil, 0.03))
	mock.ExpectExec(`UPDATE display_model_prices SET`).
		WithArgs(int64(92), "0.20000000", "0.50000000", nil, "0.04000000").
		WillReturnResult(sqlmock.NewResult(0, 1))

	currentInput, currentCacheRead := 0.1, 0.03
	targetInput, targetOutput, targetCacheRead := 0.2, 0.5, 0.04
	multiplier := 0.12
	snapshot := &upstreamPriceRollbackSnapshot{}
	changed, err := applyUpstreamTokenDisplayOverrides(context.Background(), tx, upstreamPriceApplyEvidence{
		Model: "deepseek-display", BillingMode: service.DisplayBillingModeToken,
		Suggested: domain.UpstreamPriceVector{
			InputPerMillion: &targetInput, OutputPerMillion: &targetOutput, CacheReadPerMillion: &targetCacheRead,
		},
		DisplayPricesCurrent: domain.UpstreamPriceVector{
			InputPerMillion: &currentInput, CacheReadPerMillion: &currentCacheRead,
		},
		DisplayMultiplierCurrent: &multiplier,
	}, snapshot)
	require.NoError(t, err)
	require.True(t, changed)
	require.Len(t, snapshot.Displays, 1)
	require.Equal(t, "0.10000000", *snapshot.Displays[0].DisplayInputPrice)
	require.Equal(t, "0.20000000", *snapshot.Displays[0].AfterDisplayInputPrice)
	require.Nil(t, snapshot.Displays[0].DisplayCacheWritePrice)
	require.Equal(t, snapshot.Displays[0].ModelMultiplier, snapshot.Displays[0].AfterModelMultiplier)

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
			"display_multiplier_suggested", "dimension_statuses", "last_error", "created_at",
		}).AddRow(int64(32), int64(12), int64(0), "deepseek-request-freeze", service.DisplayBillingModePerRequest,
			string(domain.UpstreamPriceEvidenceStatusTrusted), string(domain.UpstreamPriceEvidenceSourcePricePage),
			string(domain.UpstreamPriceReconciliationMatched), "price-page", now, 1, []byte(`{}`), []byte(`{}`),
			[]byte(`{}`), []byte(`{}`), []byte(`{"per_request_lte_256k":0.012,"per_request_256k_512k":0.018,"per_request_gt_512k":0.024}`),
			[]byte(`{}`), nil, nil, []byte(`{}`), "", now).
			AddRow(int64(31), int64(12), int64(3), "deepseek-freeze", service.DisplayBillingModeToken,
				string(domain.UpstreamPriceEvidenceStatusTrusted), string(domain.UpstreamPriceEvidenceSourceUserRequest),
				string(domain.UpstreamPriceReconciliationMatched), "ctx", now, 2, []byte(`{}`), []byte(`{}`),
				[]byte(`{}`), []byte(`{}`), []byte(`{"input_per_million":1.2}`), []byte(`{}`), nil, nil, []byte(`{}`), "", now))
	mock.ExpectQuery(`(?s)SELECT LOWER\(cmp.models->>0\),cmp.billing_mode,.*cmp.platform='openai'`).
		WillReturnRows(sqlmock.NewRows([]string{
			"model", "billing_mode", "input", "output", "cache_write", "cache_read", "low", "middle", "high",
		}).AddRow("deepseek-request-freeze", service.DisplayBillingModePerRequest, nil, nil, nil, nil, 0.01, 0.015, 0.02).
			AddRow("deepseek-freeze", service.DisplayBillingModeToken, 1.0, 2.0, nil, nil, nil, nil, nil))
	mock.ExpectQuery(`(?s)SELECT LOWER\(d.model_name\).*d.platform='openai'`).
		WillReturnRows(sqlmock.NewRows([]string{
			"model", "billing_mode", "multiplier", "official_input", "official_output",
			"display_input", "display_output", "display_cache_write", "display_cache_read", "low", "middle", "high",
		}).AddRow("deepseek-request-freeze", service.DisplayBillingModePerRequest, 1, nil, nil, nil, nil, nil, nil, 0.01, 0.015, 0.02).
			AddRow("deepseek-freeze", service.DisplayBillingModeToken, 0.5, "2", "4", 0.8, nil, nil, nil, nil, nil, nil))
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
	require.InDelta(t, 0.8, *evidence[1].DisplayPricesCurrent.InputPerMillion, 1e-12)
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
			"display_multiplier_suggested", "dimension_statuses", "last_error", "created_at",
		}).AddRow(int64(41), int64(14), int64(3), "deepseek-observe", service.DisplayBillingModeToken,
			string(domain.UpstreamPriceEvidenceStatusTrusted), string(domain.UpstreamPriceEvidenceSourceUserRequest),
			string(domain.UpstreamPriceReconciliationMatched), "ctx", now, 1, []byte(`{}`), []byte(`{}`),
			[]byte(`{"input_per_million":1}`), []byte(`{}`), []byte(`{"input_per_million":1.2}`),
			[]byte(`{}`), nil, nil, []byte(`{}`), "", now))
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
			"display_multiplier_suggested", "dimension_statuses", "last_error", "created_at",
		}).AddRow(int64(51), int64(21), int64(3), "deepseek-frozen", service.DisplayBillingModeToken,
			string(domain.UpstreamPriceEvidenceStatusTrusted), string(domain.UpstreamPriceEvidenceSourceUserRequest),
			string(domain.UpstreamPriceReconciliationMatched), "ctx", now, 1, []byte(`{}`), []byte(`{}`),
			[]byte(`{"input_per_million":1}`), []byte(`{"input_per_million":0.75}`),
			[]byte(`{"input_per_million":1.2}`), []byte(`{}`), 0.25, 0.4, []byte(`{}`), "", now))
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
			"display_multiplier_suggested", "dimension_statuses", "last_error", "created_at",
		}).AddRow(int64(52), int64(22), int64(3), "deepseek-running", service.DisplayBillingModeToken,
			string(domain.UpstreamPriceEvidenceStatusTrusted), string(domain.UpstreamPriceEvidenceSourceUserRequest),
			string(domain.UpstreamPriceReconciliationMatched), "ctx", now, 1, []byte(`{}`), []byte(`{}`),
			[]byte(`{"input_per_million":1}`), []byte(`{}`), []byte(`{"input_per_million":1.2}`),
			[]byte(`{}`), nil, 0.4, []byte(`{}`), "", now))
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
			"model", "billing_mode", "multiplier", "official_input", "official_output",
			"display_input", "display_output", "display_cache_write", "display_cache_read", "low", "middle", "high",
		}).AddRow("deepseek-running", service.DisplayBillingModeToken, 0.5, "3", "6", 1.2, nil, nil, nil, nil, nil, nil))

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

func TestDecimalStringRoundsFloatingPointNoiseToDatabasePrecision(t *testing.T) {
	value := 0.009 * 1.2
	require.Equal(t, "0.0108", decimalString(&value))
}
