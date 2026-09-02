package repository

import (
	"context"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/Wei-Shaw/sub2api/internal/domain"
	"github.com/stretchr/testify/require"
)

func TestUpstreamPriceMonitorRuntimeManualRunDoesNotPostponeSchedule(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	updatedAt := time.Date(2026, 9, 1, 1, 0, 0, 0, time.UTC)
	manualAt := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	scheduledAt := time.Date(2026, 9, 1, 10, 0, 0, 0, time.UTC)

	mock.ExpectQuery(`SELECT enabled, mode, interval_minutes, markup`).
		WillReturnRows(sqlmock.NewRows([]string{
			"enabled", "mode", "interval_minutes", "markup", "display_multiplier_decimals",
			"account_ids", "channel_ids", "domestic_models", "per_request_models",
			"passive_sample_max_age_minutes", "active_probe_enabled", "active_only",
			"active_probe_max_requests_per_model", "active_probe_max_models_per_run",
			"active_probe_run_budget_usd", "active_probe_daily_budget_usd", "updated_at",
		}).AddRow(true, string(domain.UpstreamPriceMonitorModeReview), 360, 1.2, 3,
			"{7}", "{3}", "{MiniMax-M3}", "{}", 1440, true, true, 7, 19, 0.15, 0.40, updatedAt))
	mock.ExpectQuery(`SELECT status,started_at,error FROM upstream_price_monitor_runs`).
		WillReturnRows(sqlmock.NewRows([]string{"status", "started_at", "error"}).
			AddRow(string(domain.UpstreamPriceMonitorRunStatusCompleted), manualAt, ""))
	mock.ExpectQuery(`SELECT started_at FROM upstream_price_monitor_runs\s+WHERE trigger='scheduled'`).
		WillReturnRows(sqlmock.NewRows([]string{"started_at"}).AddRow(scheduledAt))
	mock.ExpectQuery(`SELECT status FROM upstream_price_monitor_runs`).
		WillReturnRows(sqlmock.NewRows([]string{"status"}).AddRow(string(domain.UpstreamPriceMonitorRunStatusCompleted)))
	mock.ExpectQuery(`SELECT COALESCE\(SUM.*WHERE source='active_probe' AND reconciliation_status<>'baseline' AND created_at >= CURRENT_DATE`).
		WillReturnRows(sqlmock.NewRows([]string{"cost"}).AddRow(0.01))
	mock.ExpectQuery(`SELECT COALESCE\(MAX`).
		WillReturnRows(sqlmock.NewRows([]string{"cost"}).AddRow(0.0))
	mock.ExpectQuery(`SELECT COUNT\(DISTINCT LOWER\(model_name\)\)`).
		WithArgs(1440, sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(1))

	runtime, err := NewUpstreamPriceMonitorRepository(db).GetRuntime(context.Background())
	require.NoError(t, err)
	require.Equal(t, manualAt, *runtime.LastRunAt)
	require.Equal(t, scheduledAt.Add(360*time.Minute), *runtime.NextRunAt)
	require.InDelta(t, 0.01, runtime.TodayProbeCost, 1e-12)
	require.InDelta(t, 0.39, runtime.RemainingDailyProbeBudgetUSD, 1e-12)
	require.NoError(t, mock.ExpectationsWereMet())
}
