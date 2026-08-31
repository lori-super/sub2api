-- Lock the customer-facing catalogue to one unambiguous pricing model:
--   official provider price * final downstream provider/model multiplier.
-- The stored multiplier already includes the 20% downstream markup, so the
-- value shown in the admin editor is exactly the value used on the public page.
--
-- This migration touches presentation-only tables; runtime billing data is
-- intentionally outside its scope.

UPDATE display_pricing_settings
SET global_multiplier = 1.000000,
    updated_at = NOW()
WHERE id = 1
  AND global_multiplier IS DISTINCT FROM 1.000000;

ALTER TABLE display_pricing_settings
    DROP CONSTRAINT IF EXISTS display_pricing_settings_multiplier_positive;

ALTER TABLE display_pricing_settings
    DROP CONSTRAINT IF EXISTS display_pricing_settings_global_locked;

ALTER TABLE display_pricing_settings
    ADD CONSTRAINT display_pricing_settings_global_locked
    CHECK (global_multiplier = 1.000000);

-- Normalize known legacy/upstream values to the final downstream multiplier.
-- Custom values outside these exact legacy values are deliberately preserved.
UPDATE display_pricing_providers
SET multiplier = CASE
        WHEN multiplier IN (0.100000, 0.125000) THEN 0.120000
        WHEN multiplier IN (0.035000, 0.043750) THEN 0.042000
        ELSE multiplier
    END,
    updated_at = NOW()
WHERE multiplier IN (0.100000, 0.125000, 0.035000, 0.043750);

UPDATE display_model_prices
SET model_multiplier = 0.450000,
    updated_at = NOW()
WHERE model_name = 'deepseek-v4-flash-vision-exp'
  AND billing_mode = 'token'
  AND (model_multiplier IS NULL OR model_multiplier IN (0.375000, 0.468750));

UPDATE display_model_prices
SET model_multiplier = 0.039120,
    updated_at = NOW()
WHERE model_name = 'gpt-5.6-luna'
  AND billing_mode = 'token'
  AND (model_multiplier IS NULL OR model_multiplier = 0.032600);

-- Token display/configuration is domestic-only. Foreign models remain
-- available through their independent per-request rows.
UPDATE display_model_prices
SET enabled = FALSE,
    updated_at = NOW()
WHERE billing_mode = 'token'
  AND provider NOT IN ('deepseek', 'zhipu', 'moonshot', 'minimax', 'qwen', 'mimo', 'hunyuan');

ALTER TABLE display_model_prices
    DROP CONSTRAINT IF EXISTS display_model_prices_domestic_token_only;

ALTER TABLE display_model_prices
    ADD CONSTRAINT display_model_prices_domestic_token_only
    CHECK (
        billing_mode <> 'token'
        OR provider IN ('deepseek', 'zhipu', 'moonshot', 'minimax', 'qwen', 'mimo', 'hunyuan')
        OR enabled = FALSE
    );

-- Per-request prices have exactly one stored base tier. The public API derives
-- the remaining tiers as base * 1.5 and base * 2.
UPDATE display_model_prices
SET per_request_256k_512k_override = NULL,
    per_request_gt_512k_override = NULL,
    updated_at = NOW()
WHERE billing_mode = 'per_request'
  AND (per_request_256k_512k_override IS NOT NULL
       OR per_request_gt_512k_override IS NOT NULL);

ALTER TABLE display_model_prices
    DROP CONSTRAINT IF EXISTS display_model_prices_per_request_derived_tiers;

ALTER TABLE display_model_prices
    ADD CONSTRAINT display_model_prices_per_request_derived_tiers
    CHECK (
        billing_mode <> 'per_request'
        OR (per_request_256k_512k_override IS NULL
            AND per_request_gt_512k_override IS NULL)
    );

COMMENT ON COLUMN display_pricing_settings.global_multiplier IS
    'Compatibility field locked to 1; no hidden global markup is applied';
COMMENT ON COLUMN display_pricing_providers.multiplier IS
    'Final customer-facing provider multiplier, including downstream markup';
COMMENT ON COLUMN display_model_prices.model_multiplier IS
    'Optional final customer-facing model multiplier replacing provider multiplier';
COMMENT ON COLUMN display_model_prices.per_request_lte_256k IS
    'Customer-facing first per-request tier; higher tiers are derived as 1.5x and 2x';
