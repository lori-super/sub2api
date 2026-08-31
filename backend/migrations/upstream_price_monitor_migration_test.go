package migrations

import (
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestUpstreamPriceMonitorMigrationDefaultsToObserveOnlyAndPersistsLedgerDate(t *testing.T) {
	raw, err := os.ReadFile("235_upstream_price_monitor.sql")
	require.NoError(t, err)
	sql := strings.ToLower(string(raw))
	require.Contains(t, sql, "enabled boolean not null default false")
	require.Contains(t, sql, "mode varchar(24) not null default 'observe'")
	require.Contains(t, sql, "ledger_date date not null")
	require.Contains(t, sql, "primary key (account_id, model_name)")
	require.Contains(t, sql, "remote_snapshot jsonb")
	require.Contains(t, sql, "active_probe_pending boolean not null default false")
	require.Contains(t, sql, "idx_upstream_price_monitor_observations")
	require.Contains(t, sql, "idx_upstream_price_monitor_coverage")
	require.Contains(t, sql, "create table if not exists upstream_price_monitor_models")
	require.Contains(t, sql, "suspected_retired")
	require.NotContains(t, sql, "api_key")
	require.NotContains(t, sql, "refresh_token")
}

func TestUpstreamPriceMonitorUsageIndexIsConcurrent(t *testing.T) {
	raw, err := os.ReadFile("236_upstream_price_monitor_usage_window_index_notx.sql")
	require.NoError(t, err)
	sql := strings.ToLower(string(raw))
	require.Contains(t, sql, "create index concurrently if not exists")
	require.Contains(t, sql, "on usage_logs (account_id, id)")
}

func TestUpstreamPriceMonitorDisplayPriceSnapshotUsesForwardMigration(t *testing.T) {
	baseRaw, err := os.ReadFile("235_upstream_price_monitor.sql")
	require.NoError(t, err)
	require.NotContains(t, strings.ToLower(string(baseRaw)), "display_prices_current")

	raw, err := os.ReadFile("239_upstream_price_monitor_display_price_snapshot.sql")
	require.NoError(t, err)
	sql := strings.ToLower(string(raw))
	require.Contains(t, sql, "alter table upstream_price_monitor_evidence")
	require.Contains(t, sql, "add column if not exists display_prices_current jsonb")
	require.Contains(t, sql, "not null default '{}'::jsonb")
}

func TestUpstreamPriceMonitorPerRequestScopeUsesForwardMigration(t *testing.T) {
	raw, err := os.ReadFile("240_upstream_price_monitor_per_request_scope.sql")
	require.NoError(t, err)
	sql := strings.ToLower(string(raw))
	require.Contains(t, sql, "add column if not exists per_request_models text[]")
	require.Contains(t, sql, "'auto-model'")
	require.Contains(t, sql, "'gpt-5.6'")
}

func TestUpstreamPriceMonitorActiveOnlyMigrationLocksDailySafetyDefaults(t *testing.T) {
	raw, err := os.ReadFile("242_upstream_price_monitor_active_only.sql")
	require.NoError(t, err)
	sql := strings.ToLower(string(raw))
	require.Contains(t, sql, "active_only boolean not null default true")
	require.Contains(t, sql, "active_probe_max_requests_per_model integer not null default 7")
	require.Contains(t, sql, "active_probe_max_models_per_run integer not null default 19")
	require.Contains(t, sql, "active_probe_run_budget_usd numeric(20, 10) not null default 0.1500000000")
	require.Contains(t, sql, "active_probe_daily_budget_usd numeric(20, 10) not null default 0.2000000000")
	require.Contains(t, sql, "active_probe_run_budget_usd <= 0.1500000000")
	require.Contains(t, sql, "active_probe_daily_budget_usd <= 0.2000000000")
	require.Contains(t, sql, "interval_minutes set default 1440")
	require.Contains(t, sql, "mode set default 'auto_apply'")
	require.Contains(t, sql, "qwen3.8-flash")
	require.Contains(t, sql, "dimension_statuses jsonb")
}
