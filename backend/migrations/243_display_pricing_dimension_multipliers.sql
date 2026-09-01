-- Presentation-only token multiplier overrides.
--
-- These columns belong exclusively to display pricing. Gateway routing,
-- channel_model_pricing, groups and user billing do not read these tables.

ALTER TABLE display_pricing_providers
    ADD COLUMN IF NOT EXISTS input_multiplier_override NUMERIC(12, 6),
    ADD COLUMN IF NOT EXISTS output_multiplier_override NUMERIC(12, 6),
    ADD COLUMN IF NOT EXISTS cache_write_multiplier_override NUMERIC(12, 6),
    ADD COLUMN IF NOT EXISTS cache_read_multiplier_override NUMERIC(12, 6);

ALTER TABLE display_pricing_providers
    DROP CONSTRAINT IF EXISTS display_pricing_providers_dimension_multipliers_positive;

ALTER TABLE display_pricing_providers
    ADD CONSTRAINT display_pricing_providers_dimension_multipliers_positive CHECK (
        (input_multiplier_override IS NULL OR input_multiplier_override > 0) AND
        (output_multiplier_override IS NULL OR output_multiplier_override > 0) AND
        (cache_write_multiplier_override IS NULL OR cache_write_multiplier_override > 0) AND
        (cache_read_multiplier_override IS NULL OR cache_read_multiplier_override > 0)
    );

ALTER TABLE display_model_prices
    ADD COLUMN IF NOT EXISTS input_multiplier_override NUMERIC(12, 6),
    ADD COLUMN IF NOT EXISTS output_multiplier_override NUMERIC(12, 6),
    ADD COLUMN IF NOT EXISTS cache_write_multiplier_override NUMERIC(12, 6),
    ADD COLUMN IF NOT EXISTS cache_read_multiplier_override NUMERIC(12, 6);

ALTER TABLE display_model_prices
    ADD COLUMN IF NOT EXISTS display_input_per_million_override NUMERIC(20, 8),
    ADD COLUMN IF NOT EXISTS display_output_per_million_override NUMERIC(20, 8),
    ADD COLUMN IF NOT EXISTS display_cache_write_per_million_override NUMERIC(20, 8),
    ADD COLUMN IF NOT EXISTS display_cache_read_per_million_override NUMERIC(20, 8);

ALTER TABLE display_model_prices
    DROP CONSTRAINT IF EXISTS display_model_prices_dimension_multipliers_positive;

ALTER TABLE display_model_prices
    ADD CONSTRAINT display_model_prices_dimension_multipliers_positive CHECK (
        (input_multiplier_override IS NULL OR input_multiplier_override > 0) AND
        (output_multiplier_override IS NULL OR output_multiplier_override > 0) AND
        (cache_write_multiplier_override IS NULL OR cache_write_multiplier_override > 0) AND
        (cache_read_multiplier_override IS NULL OR cache_read_multiplier_override > 0)
    );

ALTER TABLE display_model_prices
    DROP CONSTRAINT IF EXISTS display_model_prices_exact_prices_nonnegative;

ALTER TABLE display_model_prices
    ADD CONSTRAINT display_model_prices_exact_prices_nonnegative CHECK (
        (display_input_per_million_override IS NULL OR display_input_per_million_override >= 0) AND
        (display_output_per_million_override IS NULL OR display_output_per_million_override >= 0) AND
        (display_cache_write_per_million_override IS NULL OR display_cache_write_per_million_override >= 0) AND
        (display_cache_read_per_million_override IS NULL OR display_cache_read_per_million_override >= 0)
    );

ALTER TABLE display_model_prices
    DROP CONSTRAINT IF EXISTS display_model_prices_dimension_multipliers_token_only;

ALTER TABLE display_model_prices
    ADD CONSTRAINT display_model_prices_dimension_multipliers_token_only CHECK (
        billing_mode = 'token' OR (
            input_multiplier_override IS NULL AND
            output_multiplier_override IS NULL AND
            cache_write_multiplier_override IS NULL AND
            cache_read_multiplier_override IS NULL
			AND display_input_per_million_override IS NULL
			AND display_output_per_million_override IS NULL
			AND display_cache_write_per_million_override IS NULL
			AND display_cache_read_per_million_override IS NULL
        )
    );

COMMENT ON COLUMN display_pricing_providers.input_multiplier_override IS
    'Optional final customer-facing token input multiplier replacing provider multiplier; display only';
COMMENT ON COLUMN display_pricing_providers.output_multiplier_override IS
    'Optional final customer-facing token output multiplier replacing provider multiplier; display only';
COMMENT ON COLUMN display_pricing_providers.cache_write_multiplier_override IS
    'Optional final customer-facing cache-write multiplier replacing provider multiplier; display only';
COMMENT ON COLUMN display_pricing_providers.cache_read_multiplier_override IS
    'Optional final customer-facing cache-read multiplier replacing provider multiplier; display only';
COMMENT ON COLUMN display_model_prices.input_multiplier_override IS
    'Optional final customer-facing model input multiplier; display only';
COMMENT ON COLUMN display_model_prices.output_multiplier_override IS
    'Optional final customer-facing model output multiplier; display only';
COMMENT ON COLUMN display_model_prices.cache_write_multiplier_override IS
    'Optional final customer-facing model cache-write multiplier; display only';
COMMENT ON COLUMN display_model_prices.cache_read_multiplier_override IS
    'Optional final customer-facing model cache-read multiplier; display only';
COMMENT ON COLUMN display_model_prices.display_input_per_million_override IS
    'Optional exact customer-facing input price per million, overriding official price times multiplier; display only';
COMMENT ON COLUMN display_model_prices.display_output_per_million_override IS
    'Optional exact customer-facing output price per million, overriding official price times multiplier; display only';
COMMENT ON COLUMN display_model_prices.display_cache_write_per_million_override IS
    'Optional exact customer-facing cache-write price per million, overriding official price times multiplier; display only';
COMMENT ON COLUMN display_model_prices.display_cache_read_per_million_override IS
    'Optional exact customer-facing cache-read price per million, overriding official price times multiplier; display only';
