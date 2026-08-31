-- Convert token price monitoring to a bounded, active-only daily probe. The
-- public price-page pipeline remains independent and is not a token-price
-- evidence source.

ALTER TABLE upstream_price_monitor_config
    ADD COLUMN IF NOT EXISTS active_only BOOLEAN NOT NULL DEFAULT TRUE,
    ADD COLUMN IF NOT EXISTS active_probe_max_requests_per_model INTEGER NOT NULL DEFAULT 7,
    ADD COLUMN IF NOT EXISTS active_probe_max_models_per_run INTEGER NOT NULL DEFAULT 19,
    ADD COLUMN IF NOT EXISTS active_probe_run_budget_usd NUMERIC(20, 10) NOT NULL DEFAULT 0.1500000000,
    ADD COLUMN IF NOT EXISTS active_probe_daily_budget_usd NUMERIC(20, 10) NOT NULL DEFAULT 0.2000000000;

ALTER TABLE upstream_price_monitor_config
    ALTER COLUMN mode SET DEFAULT 'auto_apply',
    ALTER COLUMN interval_minutes SET DEFAULT 1440,
    ALTER COLUMN markup SET DEFAULT 1.200000,
    ALTER COLUMN passive_sample_max_age_minutes SET DEFAULT 1440,
    ALTER COLUMN active_probe_enabled SET DEFAULT TRUE;

UPDATE upstream_price_monitor_config
SET mode = 'auto_apply',
    interval_minutes = 1440,
    markup = 1.200000,
    passive_sample_max_age_minutes = 1440,
    active_probe_enabled = TRUE,
    active_only = TRUE,
    active_probe_max_requests_per_model = 7,
    active_probe_max_models_per_run = 19,
    active_probe_run_budget_usd = 0.1500000000,
    active_probe_daily_budget_usd = 0.2000000000,
    domestic_models = ARRAY[
        'deepseek-v4-flash-0731', 'deepseek-v4-pro-0813', 'deepseek-v4-flash-vision-exp',
        'kimi-k2.6', 'kimi-k2.7-code', 'kimi-k3',
        'glm-5.1', 'glm-5.2', 'glm-5.3', 'glm-5.3-flash',
        'MiniMax-M2.7', 'MiniMax-M2.7-highspeed', 'MiniMax-M3',
        'qwen3.7-max', 'qwen3.8-flash', 'qwen3.8-max',
        'mimo-v2.5', 'mimo-v2.5-pro', 'hy3'
    ]::TEXT[],
    updated_at = NOW()
WHERE id = 1;

INSERT INTO upstream_price_monitor_models
    (model_key,model_name,status,domestic_candidate,first_seen_at,last_seen_at)
SELECT LOWER(model_name),model_name,'managed',TRUE,NOW(),NOW()
FROM UNNEST(ARRAY[
    'deepseek-v4-flash-0731', 'deepseek-v4-pro-0813', 'deepseek-v4-flash-vision-exp',
    'kimi-k2.6', 'kimi-k2.7-code', 'kimi-k3',
    'glm-5.1', 'glm-5.2', 'glm-5.3', 'glm-5.3-flash',
    'MiniMax-M2.7', 'MiniMax-M2.7-highspeed', 'MiniMax-M3',
    'qwen3.7-max', 'qwen3.8-flash', 'qwen3.8-max',
    'mimo-v2.5', 'mimo-v2.5-pro', 'hy3'
]) AS seed(model_name)
ON CONFLICT (model_key) DO UPDATE SET
    model_name = EXCLUDED.model_name,
    status = 'managed',
    domestic_candidate = TRUE,
    updated_at = NOW();

UPDATE upstream_price_monitor_models
SET status = 'discovered', updated_at = NOW()
WHERE status = 'managed'
  AND model_key <> ALL(ARRAY[
      'deepseek-v4-flash-0731', 'deepseek-v4-pro-0813', 'deepseek-v4-flash-vision-exp',
      'kimi-k2.6', 'kimi-k2.7-code', 'kimi-k3',
      'glm-5.1', 'glm-5.2', 'glm-5.3', 'glm-5.3-flash',
      'minimax-m2.7', 'minimax-m2.7-highspeed', 'minimax-m3',
      'qwen3.7-max', 'qwen3.8-flash', 'qwen3.8-max',
      'mimo-v2.5', 'mimo-v2.5-pro', 'hy3'
  ]::TEXT[]);

ALTER TABLE upstream_price_monitor_config
    DROP CONSTRAINT IF EXISTS upstream_price_monitor_config_active_only_check,
    DROP CONSTRAINT IF EXISTS upstream_price_monitor_config_probe_requests_check,
    DROP CONSTRAINT IF EXISTS upstream_price_monitor_config_probe_models_check,
    DROP CONSTRAINT IF EXISTS upstream_price_monitor_config_probe_budgets_check,
    ADD CONSTRAINT upstream_price_monitor_config_active_only_check CHECK (active_only),
    ADD CONSTRAINT upstream_price_monitor_config_probe_requests_check CHECK (
        active_probe_max_requests_per_model BETWEEN 1 AND 7
    ),
    ADD CONSTRAINT upstream_price_monitor_config_probe_models_check CHECK (
        active_probe_max_models_per_run BETWEEN 1 AND 19
    ),
    ADD CONSTRAINT upstream_price_monitor_config_probe_budgets_check CHECK (
        active_probe_run_budget_usd > 0
        AND active_probe_daily_budget_usd > 0
        AND active_probe_run_budget_usd <= 0.1500000000
        AND active_probe_daily_budget_usd <= 0.2000000000
        AND active_probe_run_budget_usd <= active_probe_daily_budget_usd
    );

ALTER TABLE upstream_price_monitor_evidence
    ADD COLUMN IF NOT EXISTS dimension_statuses JSONB NOT NULL DEFAULT '{}'::jsonb;

COMMENT ON COLUMN upstream_price_monitor_config.active_only IS
    'Token prices are inferred only from isolated synthetic probes; user requests are contamination signals only.';
COMMENT ON COLUMN upstream_price_monitor_config.active_probe_run_budget_usd IS
    'Stop threshold checked after every settled sample; one request already in flight is allowed to settle.';
COMMENT ON COLUMN upstream_price_monitor_config.active_probe_daily_budget_usd IS
    'Database-day stop threshold checked before probing and after every settled sample.';
