-- Paid probes are an audit sample, not a full-catalogue repricing pass. Cap a
-- run at three models; durable least-recently-attempted ordering rotates that
-- sample across the managed catalogue over subsequent runs.

ALTER TABLE upstream_price_monitor_config
    ALTER COLUMN active_probe_max_models_per_run SET DEFAULT 3;

UPDATE upstream_price_monitor_config
SET active_probe_max_models_per_run = LEAST(active_probe_max_models_per_run, 3),
    updated_at = NOW()
WHERE active_probe_max_models_per_run > 3;

ALTER TABLE upstream_price_monitor_config
    DROP CONSTRAINT IF EXISTS upstream_price_monitor_config_probe_models_check;

ALTER TABLE upstream_price_monitor_config
    ADD CONSTRAINT upstream_price_monitor_config_probe_models_check CHECK (
        active_probe_max_models_per_run BETWEEN 1 AND 3
    );

COMMENT ON COLUMN upstream_price_monitor_config.active_probe_max_models_per_run IS
    'Maximum synthetic audit sample per run; least-recently-attempted evidence rotates coverage across managed models.';
