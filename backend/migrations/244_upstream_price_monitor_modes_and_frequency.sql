-- Make active token-price monitoring frequent enough for practical repricing
-- while keeping three explicit operator control modes. This migration does
-- not toggle enabled: an installation that was running remains running, and
-- a paused installation remains paused.

ALTER TABLE upstream_price_monitor_config
    ALTER COLUMN interval_minutes SET DEFAULT 360,
    ALTER COLUMN active_probe_run_budget_usd SET DEFAULT 0.1500000000,
    ALTER COLUMN active_probe_daily_budget_usd SET DEFAULT 0.4000000000;

-- Only migrate the previous product default. Preserve a deliberately chosen
-- custom interval and every row's enabled/mode state.
UPDATE upstream_price_monitor_config
SET interval_minutes = 360,
    active_probe_daily_budget_usd = CASE
        WHEN active_probe_daily_budget_usd = 0.2000000000 THEN 0.4000000000
        ELSE active_probe_daily_budget_usd
    END,
    updated_at = NOW()
WHERE id = 1 AND interval_minutes = 1440;

-- Older installations could configure the former 5-minute lower bound.
-- Clamp those rows before replacing the constraint so the forward migration
-- remains valid without changing whether monitoring is enabled.
UPDATE upstream_price_monitor_config
SET interval_minutes = 60,
    updated_at = NOW()
WHERE interval_minutes < 60;

ALTER TABLE upstream_price_monitor_config
    DROP CONSTRAINT IF EXISTS upstream_price_monitor_config_mode_check,
    DROP CONSTRAINT IF EXISTS upstream_price_monitor_config_interval_check,
    DROP CONSTRAINT IF EXISTS upstream_price_monitor_config_probe_budgets_check,
    ADD CONSTRAINT upstream_price_monitor_config_mode_check
        CHECK (mode IN ('observe', 'review', 'auto_apply')),
    ADD CONSTRAINT upstream_price_monitor_config_interval_check
        CHECK (interval_minutes BETWEEN 60 AND 1440),
    ADD CONSTRAINT upstream_price_monitor_config_probe_budgets_check CHECK (
        active_probe_run_budget_usd > 0
        AND active_probe_daily_budget_usd > 0
        AND active_probe_run_budget_usd <= 0.1500000000
        AND active_probe_daily_budget_usd <= 0.4000000000
        AND active_probe_run_budget_usd <= active_probe_daily_budget_usd
    );

ALTER TABLE upstream_price_monitor_runs
    DROP CONSTRAINT IF EXISTS upstream_price_monitor_runs_mode_check,
    ADD CONSTRAINT upstream_price_monitor_runs_mode_check
        CHECK (mode IN ('observe', 'review', 'auto_apply'));

COMMENT ON COLUMN upstream_price_monitor_config.mode IS
    'observe never writes prices; review creates an administrator-applicable plan; auto_apply applies completed trusted plans automatically.';
COMMENT ON COLUMN upstream_price_monitor_config.interval_minutes IS
    'Synthetic token-price probe interval in minutes (60-1440).';
