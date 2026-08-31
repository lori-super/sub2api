package repository

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"sort"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/lib/pq"
	"github.com/shopspring/decimal"
)

type lockedPerRequestPriceRow struct {
	id        int64
	channelID int64
}

type lockedPerRequestPriceInterval struct {
	id        int64
	minTokens int64
	maxTokens sql.NullInt64
	sortOrder int
}

// SyncPerRequestPrices atomically updates only numeric per-request price
// columns on pre-existing channel rows, their three native intervals, and the
// matching presentation rows. It never creates, deletes, enables, disables, or
// otherwise reshapes catalogue data.
func (r *upstreamPriceMonitorRepository) SyncPerRequestPrices(
	ctx context.Context,
	channelIDs []int64,
	updates []service.UpstreamPerRequestPriceUpdate,
) (*service.UpstreamPerRequestPriceSyncResult, error) {
	if r == nil || r.db == nil || len(channelIDs) == 0 || len(updates) == 0 {
		return nil, service.ErrUpstreamPriceRunNotApplicable
	}
	channelIDs = normalizedPerRequestChannelIDs(channelIDs)
	updates, err := normalizedPerRequestUpdates(updates)
	if err != nil || len(channelIDs) == 0 {
		return nil, service.ErrUpstreamPriceRunNotApplicable
	}

	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	// Serialize with token auto-apply and rollback so the pricing cache always
	// observes one complete committed generation.
	if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(742193847561)`); err != nil {
		return nil, err
	}
	if err := lockPerRequestTargetChannels(ctx, tx, channelIDs); err != nil {
		return nil, err
	}

	result := &service.UpstreamPerRequestPriceSyncResult{}
	for _, update := range updates {
		if err := rejectSharedPerRequestPriceRows(ctx, tx, channelIDs, update.ModelName); err != nil {
			return nil, err
		}
		rows, err := loadPerRequestPriceRows(ctx, tx, channelIDs, update.ModelName)
		if err != nil {
			return nil, err
		}
		if len(rows) != 1 {
			return nil, fmt.Errorf("%w: model %s matched %d per-request channel rows; exactly one is required",
				service.ErrUpstreamPriceRunNotApplicable, update.ModelName, len(rows))
		}
		intervals, err := loadPerRequestPriceIntervals(ctx, tx, rows[0].id)
		if err != nil {
			return nil, err
		}
		if err := validatePerRequestPriceIntervals(update.ModelName, intervals); err != nil {
			return nil, err
		}
		displayID, err := loadPerRequestDisplayPriceRow(ctx, tx, update.ModelName)
		if err != nil {
			return nil, err
		}

		base := perRequestDecimal(update.BasePrice)
		middle := perRequestDecimal(update.MiddlePrice)
		high := perRequestDecimal(update.HighPrice)
		channelChanged := false
		modelChanged := false
		changed, err := updatePerRequestNumericColumn(ctx, tx, `UPDATE channel_model_pricing
			SET per_request_price=$2::numeric,updated_at=NOW()
			WHERE id=$1 AND platform='openai' AND billing_mode='per_request'
			  AND per_request_price IS DISTINCT FROM $2::numeric`, rows[0].id, base)
		if err != nil {
			return nil, err
		}
		channelChanged = channelChanged || changed
		modelChanged = modelChanged || changed

		prices := []string{base, middle, high}
		for i := range intervals {
			changed, err = updatePerRequestNumericColumn(ctx, tx, `UPDATE channel_pricing_intervals
				SET per_request_price=$2::numeric,updated_at=NOW()
				WHERE id=$1 AND pricing_id=$3
				  AND per_request_price IS DISTINCT FROM $2::numeric`, intervals[i].id, prices[i], rows[0].id)
			if err != nil {
				return nil, err
			}
			channelChanged = channelChanged || changed
			modelChanged = modelChanged || changed
		}

		changed, err = updatePerRequestNumericColumn(ctx, tx, `UPDATE display_model_prices
			SET per_request_lte_256k=$2::numeric,updated_at=NOW()
			WHERE id=$1 AND billing_mode='per_request'
			  AND per_request_lte_256k IS DISTINCT FROM $2::numeric`, displayID, base)
		if err != nil {
			return nil, err
		}
		modelChanged = modelChanged || changed
		result.Models++
		if channelChanged {
			result.ChangedChannelRows++
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

func normalizedPerRequestChannelIDs(values []int64) []int64 {
	seen := make(map[int64]struct{}, len(values))
	out := make([]int64, 0, len(values))
	for _, value := range values {
		if value <= 0 {
			continue
		}
		if _, exists := seen[value]; exists {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

func normalizedPerRequestUpdates(values []service.UpstreamPerRequestPriceUpdate) ([]service.UpstreamPerRequestPriceUpdate, error) {
	seen := make(map[string]struct{}, len(values))
	out := append([]service.UpstreamPerRequestPriceUpdate(nil), values...)
	for i := range out {
		out[i].ModelName = strings.TrimSpace(out[i].ModelName)
		key := strings.ToLower(out[i].ModelName)
		if key == "" || !validRepositoryPerRequestPrice(out[i].BasePrice) ||
			!validRepositoryPerRequestPrice(out[i].MiddlePrice) || !validRepositoryPerRequestPrice(out[i].HighPrice) {
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

func validRepositoryPerRequestPrice(value float64) bool {
	return value > 0 && !math.IsNaN(value) && !math.IsInf(value, 0) && decimal.NewFromFloat(value).IsPositive()
}

func lockPerRequestTargetChannels(ctx context.Context, tx *sql.Tx, channelIDs []int64) error {
	rows, err := tx.QueryContext(ctx, `SELECT id FROM channels WHERE id=ANY($1) ORDER BY id FOR UPDATE`, pq.Array(channelIDs))
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	count := 0
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return err
		}
		count++
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if count != len(channelIDs) {
		return fmt.Errorf("%w: one or more per-request target channels do not exist", service.ErrUpstreamPriceRunNotApplicable)
	}
	return nil
}

func rejectSharedPerRequestPriceRows(ctx context.Context, tx *sql.Tx, channelIDs []int64, model string) error {
	var exists bool
	err := tx.QueryRowContext(ctx, `SELECT EXISTS(
		SELECT 1 FROM channel_model_pricing cmp
		WHERE cmp.channel_id=ANY($1) AND cmp.platform='openai' AND cmp.billing_mode='per_request'
		  AND jsonb_array_length(cmp.models)<>1
		  AND EXISTS (SELECT 1 FROM jsonb_array_elements_text(cmp.models) AS model_name(value)
		              WHERE LOWER(value)=LOWER($2)))`, pq.Array(channelIDs), model).Scan(&exists)
	if err != nil {
		return err
	}
	if exists {
		return fmt.Errorf("%w: model %s is inside a shared per-request channel row",
			service.ErrUpstreamPriceRunNotApplicable, model)
	}
	return nil
}

func loadPerRequestPriceRows(ctx context.Context, tx *sql.Tx, channelIDs []int64, model string) ([]lockedPerRequestPriceRow, error) {
	rows, err := tx.QueryContext(ctx, `SELECT id,channel_id FROM channel_model_pricing
		WHERE channel_id=ANY($1) AND platform='openai' AND billing_mode='per_request'
		  AND jsonb_array_length(models)=1 AND LOWER(models->>0)=LOWER($2)
		ORDER BY id FOR UPDATE`, pq.Array(channelIDs), model)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := make([]lockedPerRequestPriceRow, 0, 1)
	for rows.Next() {
		var row lockedPerRequestPriceRow
		if err := rows.Scan(&row.id, &row.channelID); err != nil {
			return nil, err
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

func loadPerRequestPriceIntervals(ctx context.Context, tx *sql.Tx, pricingID int64) ([]lockedPerRequestPriceInterval, error) {
	rows, err := tx.QueryContext(ctx, `SELECT id,min_tokens,max_tokens,sort_order
		FROM channel_pricing_intervals WHERE pricing_id=$1 ORDER BY sort_order,id FOR UPDATE`, pricingID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := make([]lockedPerRequestPriceInterval, 0, 3)
	for rows.Next() {
		var row lockedPerRequestPriceInterval
		if err := rows.Scan(&row.id, &row.minTokens, &row.maxTokens, &row.sortOrder); err != nil {
			return nil, err
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

func validatePerRequestPriceIntervals(model string, intervals []lockedPerRequestPriceInterval) error {
	valid := len(intervals) == 3
	if valid {
		valid = intervals[0].sortOrder == 0 && intervals[0].minTokens == 0 &&
			intervals[0].maxTokens.Valid && intervals[0].maxTokens.Int64 == 256000 &&
			intervals[1].sortOrder == 1 && intervals[1].minTokens == 256000 &&
			intervals[1].maxTokens.Valid && intervals[1].maxTokens.Int64 == 512000 &&
			intervals[2].sortOrder == 2 && intervals[2].minTokens == 512000 && !intervals[2].maxTokens.Valid
	}
	if !valid {
		return fmt.Errorf("%w: model %s does not have the native 0/256K/512K three-tier structure",
			service.ErrUpstreamPriceRunNotApplicable, model)
	}
	return nil
}

func loadPerRequestDisplayPriceRow(ctx context.Context, tx *sql.Tx, model string) (int64, error) {
	rows, err := tx.QueryContext(ctx, `SELECT id,enabled,per_request_256k_512k_override::text,
		per_request_gt_512k_override::text FROM display_model_prices
		WHERE billing_mode='per_request' AND LOWER(model_name)=LOWER($1)
		ORDER BY id FOR UPDATE`, model)
	if err != nil {
		return 0, err
	}
	defer func() { _ = rows.Close() }()
	type displayRow struct {
		id      int64
		enabled bool
		middle  sql.NullString
		high    sql.NullString
	}
	var matched []displayRow
	for rows.Next() {
		var row displayRow
		if err := rows.Scan(&row.id, &row.enabled, &row.middle, &row.high); err != nil {
			return 0, err
		}
		matched = append(matched, row)
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}
	if len(matched) != 1 || !matched[0].enabled || matched[0].middle.Valid || matched[0].high.Valid {
		return 0, fmt.Errorf("%w: model %s has no unique enabled derived-tier display row",
			service.ErrUpstreamPriceRunNotApplicable, model)
	}
	return matched[0].id, nil
}

func updatePerRequestNumericColumn(ctx context.Context, tx *sql.Tx, query string, args ...any) (bool, error) {
	result, err := tx.ExecContext(ctx, query, args...)
	if err != nil {
		return false, err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return false, err
	}
	return affected > 0, nil
}

func perRequestDecimal(value float64) string {
	return decimal.NewFromFloat(value).Round(12).String()
}
