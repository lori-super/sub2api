-- Preserve the display-price state that was visible when a monitor run
-- completed. This is a forward migration because migration 235 may already
-- have been applied in production and must remain checksum-stable.
ALTER TABLE upstream_price_monitor_evidence
    ADD COLUMN IF NOT EXISTS display_prices_current JSONB NOT NULL DEFAULT '{}'::jsonb;
