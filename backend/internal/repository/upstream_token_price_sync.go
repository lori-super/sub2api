package repository

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/lib/pq"
	"github.com/shopspring/decimal"
)

type lockedTokenPriceRow struct {
	id        int64
	channelID int64
	model     string
}

// SyncTokenPrices atomically applies one public-page generation to all token
// rows on the configured managed channels, their normal-context intervals,
// and the matching exact display prices. Every requested update must already
// have at least one single-model channel row; the synchronizer never invents
// channel support merely because a model appeared on a public web page.
func (r *upstreamPriceMonitorRepository) SyncTokenPrices(
	ctx context.Context,
	channelIDs []int64,
	updates []service.UpstreamTokenPriceUpdate,
) (*service.UpstreamTokenPriceSyncResult, error) {
	if r == nil || r.db == nil || len(channelIDs) == 0 || len(updates) == 0 {
		return nil, service.ErrUpstreamPriceRunNotApplicable
	}
	channelIDs = normalizedPerRequestChannelIDs(channelIDs)
	normalized, err := normalizedTokenPriceUpdates(updates)
	if err != nil || len(channelIDs) == 0 || len(normalized) == 0 {
		return nil, service.ErrUpstreamPriceRunNotApplicable
	}
	byName := make(map[string]service.UpstreamTokenPriceUpdate, len(normalized))
	lowerNames := make([]string, 0, len(normalized))
	for i := range normalized {
		key := strings.ToLower(normalized[i].ModelName)
		byName[key] = normalized[i]
		lowerNames = append(lowerNames, key)
	}

	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	// Serialize with page-driven per-request sync and legacy monitor rollback.
	if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(742193847561)`); err != nil {
		return nil, err
	}
	if err := lockPerRequestTargetChannels(ctx, tx, channelIDs); err != nil {
		return nil, err
	}
	var sharedRows bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(
		SELECT 1 FROM channel_model_pricing cmp
		WHERE channel_id=ANY($1) AND platform='openai' AND billing_mode='token'
		  AND jsonb_array_length(models)<>1
		  AND EXISTS (SELECT 1 FROM jsonb_array_elements_text(cmp.models) model_name(value)
		              WHERE LOWER(value)=ANY($2)))`, pq.Array(channelIDs), pq.Array(lowerNames)).Scan(&sharedRows); err != nil {
		return nil, err
	}
	if sharedRows {
		return nil, fmt.Errorf("%w: managed token channel contains a shared model pricing row",
			service.ErrUpstreamPriceRunNotApplicable)
	}
	rows, err := tx.QueryContext(ctx, `SELECT id,channel_id,models->>0
		FROM channel_model_pricing
		WHERE channel_id=ANY($1) AND platform='openai' AND billing_mode='token'
		  AND jsonb_array_length(models)=1 AND LOWER(models->>0)=ANY($2)
		ORDER BY LOWER(models->>0),channel_id,id FOR UPDATE`, pq.Array(channelIDs), pq.Array(lowerNames))
	if err != nil {
		return nil, err
	}
	var locked []lockedTokenPriceRow
	for rows.Next() {
		var row lockedTokenPriceRow
		if err := rows.Scan(&row.id, &row.channelID, &row.model); err != nil {
			_ = rows.Close()
			return nil, err
		}
		row.model = strings.TrimSpace(row.model)
		locked = append(locked, row)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	seenChannelModel := make(map[string]struct{}, len(locked))
	for _, row := range locked {
		identity := fmt.Sprintf("%d\x00%s", row.channelID, strings.ToLower(row.model))
		if _, duplicate := seenChannelModel[identity]; duplicate {
			return nil, fmt.Errorf("%w: model %s has duplicate token pricing rows on channel %d",
				service.ErrUpstreamPriceRunNotApplicable, row.model, row.channelID)
		}
		seenChannelModel[identity] = struct{}{}
	}
	rowsByModel := make(map[string][]lockedTokenPriceRow)
	canonicalByModel := make(map[string]string)
	for _, row := range locked {
		key := strings.ToLower(row.model)
		rowsByModel[key] = append(rowsByModel[key], row)
		canonicalByModel[key] = row.model
	}
	for _, update := range normalized {
		key := strings.ToLower(update.ModelName)
		if len(rowsByModel[key]) == 0 {
			return nil, fmt.Errorf("%w: configured token model %s has no single-model row on the managed channels",
				service.ErrUpstreamPriceRunNotApplicable, update.ModelName)
		}
	}
	modelKeys := make([]string, 0, len(rowsByModel))
	for key := range rowsByModel {
		modelKeys = append(modelKeys, key)
	}
	sort.Strings(modelKeys)

	result := &service.UpstreamTokenPriceSyncResult{Models: len(modelKeys)}
	for _, key := range modelKeys {
		update := byName[key]
		modelChanged := false
		input := tokenPerTokenDecimal(update.InputPerMillion)
		output := tokenPerTokenDecimal(update.OutputPerMillion)
		cacheWrite := tokenPerTokenDecimal(update.CacheWritePerMillion)
		cacheRead := tokenPerTokenDecimal(update.CacheReadPerMillion)
		for _, row := range rowsByModel[key] {
			changed, updateErr := execTokenPriceUpdate(ctx, tx, `UPDATE channel_model_pricing SET
				input_price=$2::numeric,output_price=$3::numeric,
				cache_write_price=$4::numeric,cache_read_price=$5::numeric,updated_at=NOW()
				WHERE id=$1 AND platform='openai' AND billing_mode='token' AND (
					input_price IS DISTINCT FROM $2::numeric OR output_price IS DISTINCT FROM $3::numeric OR
					cache_write_price IS DISTINCT FROM $4::numeric OR cache_read_price IS DISTINCT FROM $5::numeric)`,
				row.id, input, output, cacheWrite, cacheRead)
			if updateErr != nil {
				return nil, updateErr
			}
			if changed {
				result.ChangedChannelRows++
				modelChanged = true
			}
			changedIntervals, updateErr := execTokenPriceUpdateCount(ctx, tx, `UPDATE channel_pricing_intervals SET
				input_price=$2::numeric,output_price=$3::numeric,
				cache_write_price=$4::numeric,cache_read_price=$5::numeric,updated_at=NOW()
				WHERE pricing_id=$1 AND min_tokens=0 AND (
					input_price IS DISTINCT FROM $2::numeric OR output_price IS DISTINCT FROM $3::numeric OR
					cache_write_price IS DISTINCT FROM $4::numeric OR cache_read_price IS DISTINCT FROM $5::numeric)`,
				row.id, input, output, cacheWrite, cacheRead)
			if updateErr != nil {
				return nil, updateErr
			}
			if changedIntervals > 0 {
				result.ChangedIntervalRows += changedIntervals
				modelChanged = true
			}
		}

		displayChanged, displayCreated, displayErr := syncTokenDisplayPrice(
			ctx, tx, canonicalByModel[key], update,
		)
		if displayErr != nil {
			return nil, displayErr
		}
		if displayChanged {
			result.ChangedDisplayRows++
			modelChanged = true
		}
		if displayCreated {
			result.CreatedDisplayRows++
		}
		if modelChanged {
			result.ChangedModels++
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return result, nil
}

func normalizedTokenPriceUpdates(values []service.UpstreamTokenPriceUpdate) ([]service.UpstreamTokenPriceUpdate, error) {
	seen := make(map[string]struct{}, len(values))
	out := append([]service.UpstreamTokenPriceUpdate(nil), values...)
	for i := range out {
		out[i].ModelName = strings.TrimSpace(out[i].ModelName)
		out[i].Provider = strings.ToLower(strings.TrimSpace(out[i].Provider))
		key := strings.ToLower(out[i].ModelName)
		if key == "" || out[i].Provider == "" ||
			!validRepositoryTokenPrice(out[i].InputPerMillion, true) ||
			!validRepositoryTokenPrice(out[i].OutputPerMillion, true) ||
			!validRepositoryTokenPrice(out[i].CacheWritePerMillion, false) ||
			!validRepositoryTokenPrice(out[i].CacheReadPerMillion, false) ||
			!validRepositoryTokenPrice(out[i].OfficialInput, false) ||
			!validRepositoryTokenPrice(out[i].OfficialOutput, false) ||
			!validRepositoryTokenPrice(out[i].OfficialCacheWrite, false) ||
			!validRepositoryTokenPrice(out[i].OfficialCacheRead, false) {
			return nil, service.ErrUpstreamPriceRunNotApplicable
		}
		if _, duplicate := seen[key]; duplicate {
			return nil, service.ErrUpstreamPriceRunNotApplicable
		}
		seen[key] = struct{}{}
	}
	sort.SliceStable(out, func(i, j int) bool {
		return strings.ToLower(out[i].ModelName) < strings.ToLower(out[j].ModelName)
	})
	return out, nil
}

func validRepositoryTokenPrice(value *float64, required bool) bool {
	if value == nil {
		return !required
	}
	return !math.IsNaN(*value) && !math.IsInf(*value, 0) && ((!required && *value >= 0) || *value > 0)
}

func tokenPerTokenDecimal(value *float64) any {
	if value == nil {
		return nil
	}
	return decimal.NewFromFloat(*value).Div(decimal.NewFromInt(1_000_000)).Round(12).String()
}

func execTokenPriceUpdate(ctx context.Context, tx *sql.Tx, query string, args ...any) (bool, error) {
	count, err := execTokenPriceUpdateCount(ctx, tx, query, args...)
	return count > 0, err
}

func execTokenPriceUpdateCount(ctx context.Context, tx *sql.Tx, query string, args ...any) (int, error) {
	result, err := tx.ExecContext(ctx, query, args...)
	if err != nil {
		return 0, err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return 0, err
	}
	return int(affected), nil
}

func syncTokenDisplayPrice(
	ctx context.Context,
	tx *sql.Tx,
	model string,
	update service.UpstreamTokenPriceUpdate,
) (changed bool, created bool, err error) {
	var displayID int64
	err = tx.QueryRowContext(ctx, `SELECT id FROM display_model_prices
		WHERE platform='openai' AND billing_mode='token' AND LOWER(model_name)=LOWER($1)
		FOR UPDATE`, model).Scan(&displayID)
	if err != nil && err != sql.ErrNoRows {
		return false, false, err
	}
	now := time.Now().UTC()
	if err == sql.ErrNoRows {
		err = tx.QueryRowContext(ctx, `INSERT INTO display_model_prices
			(platform,model_name,provider,billing_mode,currency,enabled,
			 official_input_per_million,official_output_per_million,
			 official_cache_write_per_million,official_cache_read_per_million,
			 display_input_per_million_override,display_output_per_million_override,
			 display_cache_write_per_million_override,display_cache_read_per_million_override,
			 official_price_source,official_price_source_url,official_price_synced_at)
			SELECT 'openai',$1,p.provider,'token',p.currency,TRUE,
			 ROUND($3::numeric,8),ROUND($4::numeric,8),ROUND($5::numeric,8),ROUND($6::numeric,8),
			 ROUND($7::numeric,8),ROUND($8::numeric,8),ROUND($9::numeric,8),ROUND($10::numeric,8),
			 $11,$12,$13
			FROM display_pricing_providers p WHERE p.provider=$2
			RETURNING id`, model, update.Provider,
			update.OfficialInput, update.OfficialOutput, update.OfficialCacheWrite, update.OfficialCacheRead,
			update.InputPerMillion, update.OutputPerMillion, update.CacheWritePerMillion, update.CacheReadPerMillion,
			service.DisplayOfficialPriceX5M5X, service.DisplayUpstreamPriceSourceURL, now).Scan(&displayID)
		if err == sql.ErrNoRows {
			return false, false, fmt.Errorf("%w: display provider %s is unavailable for model %s",
				service.ErrUpstreamPriceRunNotApplicable, update.Provider, model)
		}
		return err == nil, err == nil, err
	}

	changed, err = execTokenPriceUpdate(ctx, tx, `UPDATE display_model_prices SET
		official_input_per_million=ROUND($2::numeric,8),official_output_per_million=ROUND($3::numeric,8),
		official_cache_write_per_million=ROUND($4::numeric,8),official_cache_read_per_million=ROUND($5::numeric,8),
		display_input_per_million_override=ROUND($6::numeric,8),display_output_per_million_override=ROUND($7::numeric,8),
		display_cache_write_per_million_override=ROUND($8::numeric,8),display_cache_read_per_million_override=ROUND($9::numeric,8),
		official_price_source=$10,official_price_source_url=$11,official_price_synced_at=$12,updated_at=NOW()
		WHERE id=$1 AND platform='openai' AND billing_mode='token' AND (
			official_input_per_million IS DISTINCT FROM ROUND($2::numeric,8) OR
			official_output_per_million IS DISTINCT FROM ROUND($3::numeric,8) OR
			official_cache_write_per_million IS DISTINCT FROM ROUND($4::numeric,8) OR
			official_cache_read_per_million IS DISTINCT FROM ROUND($5::numeric,8) OR
			display_input_per_million_override IS DISTINCT FROM ROUND($6::numeric,8) OR
			display_output_per_million_override IS DISTINCT FROM ROUND($7::numeric,8) OR
			display_cache_write_per_million_override IS DISTINCT FROM ROUND($8::numeric,8) OR
			display_cache_read_per_million_override IS DISTINCT FROM ROUND($9::numeric,8) OR
			official_price_source IS DISTINCT FROM $10 OR official_price_source_url IS DISTINCT FROM $11)`,
		displayID, update.OfficialInput, update.OfficialOutput, update.OfficialCacheWrite, update.OfficialCacheRead,
		update.InputPerMillion, update.OutputPerMillion, update.CacheWritePerMillion, update.CacheReadPerMillion,
		service.DisplayOfficialPriceX5M5X, service.DisplayUpstreamPriceSourceURL, now)
	return changed, false, err
}
