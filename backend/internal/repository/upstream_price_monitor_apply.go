package repository

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/domain"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/shopspring/decimal"
)

type upstreamPriceChannelRollback struct {
	ID                   int64   `json:"id"`
	InputPrice           *string `json:"input_price,omitempty"`
	OutputPrice          *string `json:"output_price,omitempty"`
	CacheWritePrice      *string `json:"cache_write_price,omitempty"`
	CacheReadPrice       *string `json:"cache_read_price,omitempty"`
	PerRequestPrice      *string `json:"per_request_price,omitempty"`
	AfterInputPrice      *string `json:"after_input_price,omitempty"`
	AfterOutputPrice     *string `json:"after_output_price,omitempty"`
	AfterCacheWritePrice *string `json:"after_cache_write_price,omitempty"`
	AfterCacheReadPrice  *string `json:"after_cache_read_price,omitempty"`
	AfterPerRequestPrice *string `json:"after_per_request_price,omitempty"`
}

type upstreamPriceIntervalRollback struct {
	ID                   int64   `json:"id"`
	InputPrice           *string `json:"input_price,omitempty"`
	OutputPrice          *string `json:"output_price,omitempty"`
	CacheWritePrice      *string `json:"cache_write_price,omitempty"`
	CacheReadPrice       *string `json:"cache_read_price,omitempty"`
	PerRequestPrice      *string `json:"per_request_price,omitempty"`
	AfterInputPrice      *string `json:"after_input_price,omitempty"`
	AfterOutputPrice     *string `json:"after_output_price,omitempty"`
	AfterCacheWritePrice *string `json:"after_cache_write_price,omitempty"`
	AfterCacheReadPrice  *string `json:"after_cache_read_price,omitempty"`
	AfterPerRequestPrice *string `json:"after_per_request_price,omitempty"`
}

type upstreamPriceDisplayRollback struct {
	ID                      int64   `json:"id"`
	ModelMultiplier         *string `json:"model_multiplier,omitempty"`
	PerRequestLTE256K       *string `json:"per_request_lte_256k,omitempty"`
	PerRequest256K512K      *string `json:"per_request_256k_512k,omitempty"`
	PerRequestGT512K        *string `json:"per_request_gt_512k,omitempty"`
	AfterModelMultiplier    *string `json:"after_model_multiplier,omitempty"`
	AfterPerRequestLTE256K  *string `json:"after_per_request_lte_256k,omitempty"`
	AfterPerRequest256K512K *string `json:"after_per_request_256k_512k,omitempty"`
	AfterPerRequestGT512K   *string `json:"after_per_request_gt_512k,omitempty"`
}

type upstreamPriceRollbackSnapshot struct {
	Channels  []upstreamPriceChannelRollback  `json:"channels"`
	Displays  []upstreamPriceDisplayRollback  `json:"displays"`
	Intervals []upstreamPriceIntervalRollback `json:"intervals"`
}

type lockedPerRequestInterval struct {
	rollback  upstreamPriceIntervalRollback
	minTokens int64
	maxTokens sql.NullInt64
}

type upstreamPriceApplyEvidence struct {
	Model                    string
	Current                  domain.UpstreamPriceVector
	Suggested                domain.UpstreamPriceVector
	DisplayPricesCurrent     domain.UpstreamPriceVector
	DisplayMultiplierCurrent *float64
	BillingMode              string
}

type upstreamPriceApplySkippedModel struct {
	Model           string  `json:"model"`
	Reason          string  `json:"reason"`
	FixedPerRequest float64 `json:"fixed_per_request"`
}

const upstreamPriceFixedRequestFeeTolerance = 1e-9

func (r *upstreamPriceMonitorRepository) ApplyRun(
	ctx context.Context,
	runID int64,
	snapshotHash string,
	channelIDs []int64,
	accountIDs []int64,
	decimals int,
	maxAgeMinutes int,
	expectedConfigUpdatedAt time.Time,
	expectedModelCatalogRevision int64,
) error {
	if r == nil || r.db == nil || runID <= 0 || strings.TrimSpace(snapshotHash) == "" || len(channelIDs) == 0 || len(accountIDs) == 0 ||
		maxAgeMinutes <= 0 || expectedConfigUpdatedAt.IsZero() || expectedModelCatalogRevision <= 0 {
		return service.ErrUpstreamPriceRunNotApplicable
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(742193847561)`); err != nil {
		return err
	}

	var trigger, status, storedHash string
	var appliedAt, finishedAt sql.NullTime
	if err := tx.QueryRowContext(ctx, `SELECT trigger,status,snapshot_hash,applied_at,finished_at
		FROM upstream_price_monitor_runs WHERE id=$1 FOR UPDATE`, runID).
		Scan(&trigger, &status, &storedHash, &appliedAt, &finishedAt); err != nil {
		return service.ErrUpstreamPriceRunNotApplicable
	}
	if status != string(domain.UpstreamPriceMonitorRunStatusCompleted) || appliedAt.Valid || !finishedAt.Valid ||
		finishedAt.Time.Before(time.Now().Add(-time.Duration(maxAgeMinutes)*time.Minute)) {
		return service.ErrUpstreamPriceRunNotApplicable
	}
	if storedHash != strings.TrimSpace(snapshotHash) {
		return service.ErrUpstreamPriceSnapshotMismatch
	}
	var newerAppliedSnapshot bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM upstream_price_monitor_runs
		WHERE applied_at IS NOT NULL AND (finished_at > $1 OR (finished_at=$1 AND id>$2)))`,
		finishedAt.Time, runID).Scan(&newerAppliedSnapshot); err != nil {
		return err
	}
	if newerAppliedSnapshot {
		return service.ErrUpstreamPriceRunNotApplicable
	}
	var enabled bool
	var mode string
	var currentConfigUpdatedAt time.Time
	if err := tx.QueryRowContext(ctx, `SELECT enabled,mode,updated_at FROM upstream_price_monitor_config
		WHERE id=1 FOR UPDATE`).Scan(&enabled, &mode, &currentConfigUpdatedAt); err != nil {
		return err
	}
	if !currentConfigUpdatedAt.Equal(expectedConfigUpdatedAt) {
		return service.ErrUpstreamPriceRunNotApplicable
	}
	if trigger == string(domain.UpstreamPriceMonitorRunTriggerScheduled) &&
		(!enabled || mode != string(domain.UpstreamPriceMonitorModeAutoApply)) {
		return service.ErrUpstreamPriceRunNotApplicable
	}
	var currentCatalogRevision int64
	if err := tx.QueryRowContext(ctx, `SELECT revision FROM upstream_price_monitor_model_scan_state
		WHERE id=1 FOR UPDATE`).Scan(&currentCatalogRevision); err != nil {
		return err
	}
	if currentCatalogRevision != expectedModelCatalogRevision {
		return service.ErrUpstreamPriceRunNotApplicable
	}

	evidence, skippedModels, err := loadTrustedApplyEvidence(ctx, tx, runID, accountIDs)
	if err != nil {
		return err
	}
	if len(skippedModels) > 0 {
		skippedJSON, marshalErr := json.Marshal(skippedModels)
		if marshalErr != nil {
			return marshalErr
		}
		if _, updateErr := tx.ExecContext(ctx, `UPDATE upstream_price_monitor_runs SET
			summary=jsonb_set(summary,'{skipped_models}',$2::jsonb,true) WHERE id=$1`, runID, string(skippedJSON)); updateErr != nil {
			return updateErr
		}
	}
	if len(evidence) == 0 {
		if len(skippedModels) > 0 {
			if commitErr := tx.Commit(); commitErr != nil {
				return commitErr
			}
			return fmt.Errorf("%w: all trusted models have fixed request fees that channel token pricing cannot represent",
				service.ErrUpstreamPriceRunNotApplicable)
		}
		return service.ErrUpstreamPriceRunNotApplicable
	}
	automaticApply := trigger == string(domain.UpstreamPriceMonitorRunTriggerScheduled) &&
		mode == string(domain.UpstreamPriceMonitorModeAutoApply)
	if err := validateUpstreamPriceApplyEvidence(evidence, automaticApply); err != nil {
		return err
	}

	snapshot := upstreamPriceRollbackSnapshot{}
	matchedChannels := 0
	appliedModels := 0
	changedAny := false
	for _, item := range evidence {
		// Application is deliberately narrower than evidence collection. Public
		// price-page/per-request evidence belongs to the display catalogue and
		// must never cross into real channel billing.
		if item.BillingMode != service.DisplayBillingModeToken {
			return fmt.Errorf("%w: only active token evidence can update channel pricing",
				service.ErrUpstreamPriceRunNotApplicable)
		}
		input := perMillionToPerToken(item.Suggested.InputPerMillion)
		output := perMillionToPerToken(item.Suggested.OutputPerMillion)
		cacheWrite := perMillionToPerToken(item.Suggested.CacheWritePerMillion)
		cacheRead := perMillionToPerToken(item.Suggested.CacheReadPerMillion)
		itemMatchedChannels := 0
		itemChanged := false
		for _, channelID := range channelIDs {
			if err := rejectSharedUpstreamPricingRow(ctx, tx, channelID, service.DisplayBillingModeToken, item.Model); err != nil {
				return err
			}
			lockedRows, queryErr := loadLockedUpstreamChannelPricingRows(
				ctx, tx, channelID, service.DisplayBillingModeToken, item.Model,
			)
			if queryErr != nil {
				return queryErr
			}
			for _, before := range lockedRows {
				if err := assertTokenChannelSnapshot(ctx, tx, before, item.Current); err != nil {
					return err
				}
				result, execErr := tx.ExecContext(ctx, `UPDATE channel_model_pricing SET
					input_price=COALESCE($2::numeric,input_price),output_price=COALESCE($3::numeric,output_price),
					cache_write_price=COALESCE($4::numeric,cache_write_price),cache_read_price=COALESCE($5::numeric,cache_read_price),
					updated_at=NOW() WHERE id=$1 AND (
						($2::numeric IS NOT NULL AND input_price IS DISTINCT FROM $2::numeric) OR
						($3::numeric IS NOT NULL AND output_price IS DISTINCT FROM $3::numeric) OR
						($4::numeric IS NOT NULL AND cache_write_price IS DISTINCT FROM $4::numeric) OR
						($5::numeric IS NOT NULL AND cache_read_price IS DISTINCT FROM $5::numeric))`,
					before.ID, input, output, cacheWrite, cacheRead)
				if execErr != nil {
					return execErr
				}
				if affected, _ := result.RowsAffected(); affected > 0 {
					before = channelRollbackWithAfter(before, input, output, cacheWrite, cacheRead, nil)
					snapshot.Channels = append(snapshot.Channels, before)
					itemChanged = true
				}
				intervalChanged, intervalErr := applyUpstreamTokenBaseIntervals(ctx, tx, before.ID, input, output, cacheWrite, cacheRead, &snapshot)
				if intervalErr != nil {
					return intervalErr
				}
				itemChanged = itemChanged || intervalChanged
				itemMatchedChannels++
			}
		}
		if itemMatchedChannels == 0 {
			continue
		}
		matchedChannels += itemMatchedChannels
		if itemChanged {
			appliedModels++
			changedAny = true
		}
	}
	if matchedChannels == 0 {
		return fmt.Errorf("%w: no configured channel price rows matched trusted evidence", service.ErrUpstreamPriceRunNotApplicable)
	}

	rawSnapshot, err := json.Marshal(snapshot)
	if err != nil {
		return err
	}
	// Only the most recent monitor apply may be rolled back. This prevents an
	// older snapshot from overwriting a newer successful monitor run.
	if changedAny {
		if _, err := tx.ExecContext(ctx, `UPDATE upstream_price_monitor_runs SET rollback_available=FALSE
			WHERE id<>$1 AND rollback_available=TRUE`, runID); err != nil {
			return err
		}
	}
	result, err := tx.ExecContext(ctx, `UPDATE upstream_price_monitor_runs SET applied_at=NOW(),rollback_available=$4,
		rollback_snapshot=CASE WHEN $4 THEN $2::jsonb ELSE NULL END,
		summary=jsonb_set(summary,'{applied_models}',to_jsonb($3::int),true)
		WHERE id=$1 AND applied_at IS NULL`, runID, string(rawSnapshot), appliedModels, changedAny)
	if err != nil {
		return err
	}
	rowsAffected, _ := result.RowsAffected()
	if rowsAffected != 1 {
		return service.ErrUpstreamPriceRunNotApplicable
	}
	return tx.Commit()
}

// applyUpstreamTokenBaseIntervals keeps the normal-context interval aligned
// with the channel row. The resolver gives an explicit interval precedence
// over the flat row, so updating only channel_model_pricing would leave live
// billing on the old value. Higher-context intervals are deliberately left
// untouched because passive default-tier evidence cannot infer their pricing.
func applyUpstreamTokenBaseIntervals(
	ctx context.Context,
	tx *sql.Tx,
	pricingID int64,
	input, output, cacheWrite, cacheRead any,
	snapshot *upstreamPriceRollbackSnapshot,
) (bool, error) {
	rows, err := tx.QueryContext(ctx, `SELECT id,input_price::text,output_price::text,
		cache_write_price::text,cache_read_price::text,per_request_price::text,
		input_multiplier IS NOT NULL,output_multiplier IS NOT NULL,
		cache_write_multiplier IS NOT NULL,cache_read_multiplier IS NOT NULL
		FROM channel_pricing_intervals WHERE pricing_id=$1 AND min_tokens=0
		ORDER BY sort_order,id FOR UPDATE`, pricingID)
	if err != nil {
		return false, err
	}
	type lockedTokenInterval struct {
		rollback                                        upstreamPriceIntervalRollback
		hasInputMultiplier, hasOutputMultiplier         bool
		hasCacheWriteMultiplier, hasCacheReadMultiplier bool
	}
	var locked []lockedTokenInterval
	for rows.Next() {
		var item lockedTokenInterval
		var in, out, cw, cr, request sql.NullString
		if err := rows.Scan(&item.rollback.ID, &in, &out, &cw, &cr, &request,
			&item.hasInputMultiplier, &item.hasOutputMultiplier,
			&item.hasCacheWriteMultiplier, &item.hasCacheReadMultiplier); err != nil {
			_ = rows.Close()
			return false, err
		}
		item.rollback.InputPrice = nullStringPtr(in)
		item.rollback.OutputPrice = nullStringPtr(out)
		item.rollback.CacheWritePrice = nullStringPtr(cw)
		item.rollback.CacheReadPrice = nullStringPtr(cr)
		item.rollback.PerRequestPrice = nullStringPtr(request)
		locked = append(locked, item)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return false, err
	}
	if err := rows.Close(); err != nil {
		return false, err
	}
	changed := false
	for _, item := range locked {
		before := item.rollback
		intervalInput, intervalOutput := input, output
		intervalCacheWrite, intervalCacheRead := cacheWrite, cacheRead
		if before.InputPrice == nil && item.hasInputMultiplier {
			intervalInput = nil
		}
		if before.OutputPrice == nil && item.hasOutputMultiplier {
			intervalOutput = nil
		}
		if before.CacheWritePrice == nil && item.hasCacheWriteMultiplier {
			intervalCacheWrite = nil
		}
		if before.CacheReadPrice == nil && item.hasCacheReadMultiplier {
			intervalCacheRead = nil
		}
		result, err := tx.ExecContext(ctx, `UPDATE channel_pricing_intervals SET
			input_price=COALESCE($2::numeric,input_price),output_price=COALESCE($3::numeric,output_price),
			cache_write_price=COALESCE($4::numeric,cache_write_price),cache_read_price=COALESCE($5::numeric,cache_read_price),
			updated_at=NOW() WHERE id=$1 AND (
				($2::numeric IS NOT NULL AND input_price IS DISTINCT FROM $2::numeric) OR
				($3::numeric IS NOT NULL AND output_price IS DISTINCT FROM $3::numeric) OR
				($4::numeric IS NOT NULL AND cache_write_price IS DISTINCT FROM $4::numeric) OR
				($5::numeric IS NOT NULL AND cache_read_price IS DISTINCT FROM $5::numeric))`,
			before.ID, intervalInput, intervalOutput, intervalCacheWrite, intervalCacheRead)
		if err != nil {
			return false, err
		}
		if affected, _ := result.RowsAffected(); affected > 0 {
			before = intervalRollbackWithAfter(before, intervalInput, intervalOutput, intervalCacheWrite, intervalCacheRead, nil)
			snapshot.Intervals = append(snapshot.Intervals, before)
			changed = true
		}
	}
	return changed, nil
}

func loadLockedUpstreamChannelPricingRows(
	ctx context.Context,
	tx *sql.Tx,
	channelID int64,
	billingMode string,
	model string,
) ([]upstreamPriceChannelRollback, error) {
	rows, err := tx.QueryContext(ctx, `SELECT id,input_price::text,output_price::text,
		cache_write_price::text,cache_read_price::text,per_request_price::text
		FROM channel_model_pricing
		WHERE channel_id=$1 AND platform='openai' AND billing_mode=$2
		  AND jsonb_array_length(models)=1 AND LOWER(models->>0)=LOWER($3)
		ORDER BY id FOR UPDATE`, channelID, billingMode, model)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var locked []upstreamPriceChannelRollback
	for rows.Next() {
		var item upstreamPriceChannelRollback
		var input, output, cacheWrite, cacheRead, perRequest sql.NullString
		if err := rows.Scan(&item.ID, &input, &output, &cacheWrite, &cacheRead, &perRequest); err != nil {
			return nil, err
		}
		item.InputPrice = nullStringPtr(input)
		item.OutputPrice = nullStringPtr(output)
		item.CacheWritePrice = nullStringPtr(cacheWrite)
		item.CacheReadPrice = nullStringPtr(cacheRead)
		item.PerRequestPrice = nullStringPtr(perRequest)
		locked = append(locked, item)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return locked, nil
}

func assertTokenChannelSnapshot(
	ctx context.Context,
	tx *sql.Tx,
	channel upstreamPriceChannelRollback,
	expected domain.UpstreamPriceVector,
) error {
	rows, err := tx.QueryContext(ctx, `SELECT input_price::text,output_price::text,
		cache_write_price::text,cache_read_price::text,input_multiplier::text,
		output_multiplier::text,cache_write_multiplier::text,cache_read_multiplier::text
		FROM channel_pricing_intervals WHERE pricing_id=$1 AND min_tokens=0
		ORDER BY sort_order,id FOR UPDATE`, channel.ID)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	type interval struct {
		input, output, cacheWrite, cacheRead                    sql.NullString
		inputMultiplier, outputMultiplier, cacheWriteMultiplier sql.NullString
		cacheReadMultiplier                                     sql.NullString
	}
	var intervals []interval
	for rows.Next() {
		var item interval
		if err := rows.Scan(&item.input, &item.output, &item.cacheWrite, &item.cacheRead,
			&item.inputMultiplier, &item.outputMultiplier, &item.cacheWriteMultiplier,
			&item.cacheReadMultiplier); err != nil {
			return err
		}
		intervals = append(intervals, item)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if len(intervals) == 0 {
		intervals = append(intervals, interval{})
	}
	for _, item := range intervals {
		actual := domain.UpstreamPriceVector{}
		actual.InputPerMillion, err = effectivePerMillion(channel.InputPrice, item.input, item.inputMultiplier)
		if err != nil {
			return err
		}
		actual.OutputPerMillion, err = effectivePerMillion(channel.OutputPrice, item.output, item.outputMultiplier)
		if err != nil {
			return err
		}
		actual.CacheWritePerMillion, err = effectivePerMillion(channel.CacheWritePrice, item.cacheWrite, item.cacheWriteMultiplier)
		if err != nil {
			return err
		}
		actual.CacheReadPerMillion, err = effectivePerMillion(channel.CacheReadPrice, item.cacheRead, item.cacheReadMultiplier)
		if err != nil {
			return err
		}
		if !sameTokenPriceSnapshot(expected, actual) {
			return fmt.Errorf("%w: channel token prices changed for pricing row %d",
				service.ErrUpstreamPriceSnapshotMismatch, channel.ID)
		}
	}
	return nil
}

func assertPerRequestChannelSnapshot(
	channel upstreamPriceChannelRollback,
	intervals []lockedPerRequestInterval,
	expected domain.UpstreamPriceVector,
) error {
	if len(intervals) != 3 {
		return service.ErrUpstreamPriceSnapshotMismatch
	}
	low := intervals[0].rollback.PerRequestPrice
	if low == nil {
		low = channel.PerRequestPrice
	}
	actualLow, err := numericStringFloatPtr(low)
	if err != nil {
		return err
	}
	actualMiddle, err := numericStringFloatPtr(intervals[1].rollback.PerRequestPrice)
	if err != nil {
		return err
	}
	actualHigh, err := numericStringFloatPtr(intervals[2].rollback.PerRequestPrice)
	if err != nil {
		return err
	}
	actual := domain.UpstreamPriceVector{
		PerRequestLTE256K: actualLow, PerRequest256K512K: actualMiddle, PerRequestGT512K: actualHigh,
	}
	if !samePerRequestPriceSnapshot(expected, actual) {
		return fmt.Errorf("%w: channel per-request prices changed for pricing row %d",
			service.ErrUpstreamPriceSnapshotMismatch, channel.ID)
	}
	return nil
}

func effectivePerMillion(base *string, override, multiplier sql.NullString) (*float64, error) {
	var value decimal.Decimal
	var err error
	if override.Valid {
		value, err = decimal.NewFromString(override.String)
	} else {
		if base == nil {
			return nil, nil
		}
		value, err = decimal.NewFromString(*base)
		if err == nil && multiplier.Valid {
			var factor decimal.Decimal
			factor, err = decimal.NewFromString(multiplier.String)
			if err == nil {
				value = value.Mul(factor)
			}
		}
	}
	if err != nil {
		return nil, err
	}
	result := value.Mul(decimal.NewFromInt(1_000_000)).InexactFloat64()
	return &result, nil
}

func numericStringFloatPtr(value *string) (*float64, error) {
	if value == nil {
		return nil, nil
	}
	parsed, err := decimal.NewFromString(*value)
	if err != nil {
		return nil, err
	}
	result := parsed.InexactFloat64()
	return &result, nil
}

func nullNumericFloatPtr(value sql.NullString) (*float64, error) {
	return numericStringFloatPtr(nullStringPtr(value))
}

func sameTokenPriceSnapshot(expected, actual domain.UpstreamPriceVector) bool {
	return sameFloatPtr(expected.InputPerMillion, actual.InputPerMillion) &&
		sameFloatPtr(expected.OutputPerMillion, actual.OutputPerMillion) &&
		sameFloatPtr(expected.CacheWritePerMillion, actual.CacheWritePerMillion) &&
		sameFloatPtr(expected.CacheReadPerMillion, actual.CacheReadPerMillion)
}

func samePerRequestPriceSnapshot(expected, actual domain.UpstreamPriceVector) bool {
	return sameFloatPtr(expected.PerRequestLTE256K, actual.PerRequestLTE256K) &&
		sameFloatPtr(expected.PerRequest256K512K, actual.PerRequest256K512K) &&
		sameFloatPtr(expected.PerRequestGT512K, actual.PerRequestGT512K)
}

func assertDisplayMultiplierSnapshot(model string, expected, actual *float64) error {
	if !sameFloatPtr(expected, actual) {
		return fmt.Errorf("%w: display multiplier changed for model %s",
			service.ErrUpstreamPriceSnapshotMismatch, model)
	}
	return nil
}

func assertPerRequestDisplaySnapshot(model string, expected, actual domain.UpstreamPriceVector) error {
	if !samePerRequestPriceSnapshot(expected, actual) {
		return fmt.Errorf("%w: per-request display prices changed for model %s",
			service.ErrUpstreamPriceSnapshotMismatch, model)
	}
	return nil
}

func rejectSharedUpstreamPricingRow(ctx context.Context, tx *sql.Tx, channelID int64, billingMode, model string) error {
	var exists bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(
		SELECT 1 FROM channel_model_pricing cmp
		WHERE cmp.channel_id=$1 AND cmp.platform='openai' AND cmp.billing_mode=$2
		  AND jsonb_array_length(cmp.models)<>1
		  AND EXISTS (SELECT 1 FROM jsonb_array_elements_text(cmp.models) AS model_name(value)
		              WHERE LOWER(value)=LOWER($3)))`, channelID, billingMode, model).Scan(&exists); err != nil {
		return err
	}
	if exists {
		return fmt.Errorf("%w: model %s is inside a shared price row on channel %d; split it into a single-model row before auto-apply",
			service.ErrUpstreamPriceRunNotApplicable, model, channelID)
	}
	return nil
}

func (r *upstreamPriceMonitorRepository) RollbackRun(ctx context.Context, runID int64, snapshotHash string) error {
	if r == nil || r.db == nil || runID <= 0 || strings.TrimSpace(snapshotHash) == "" {
		return service.ErrUpstreamPriceRunNotApplicable
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(742193847561)`); err != nil {
		return err
	}
	var storedHash string
	var appliedAt sql.NullTime
	var rollbackAvailable bool
	var raw []byte
	if err := tx.QueryRowContext(ctx, `SELECT snapshot_hash,applied_at,rollback_available,rollback_snapshot
		FROM upstream_price_monitor_runs WHERE id=$1 FOR UPDATE`, runID).
		Scan(&storedHash, &appliedAt, &rollbackAvailable, &raw); err != nil {
		return service.ErrUpstreamPriceRunNotApplicable
	}
	if storedHash != strings.TrimSpace(snapshotHash) {
		return service.ErrUpstreamPriceSnapshotMismatch
	}
	if !appliedAt.Valid || !rollbackAvailable || len(raw) == 0 {
		return service.ErrUpstreamPriceRunNotApplicable
	}
	var newerApplyExists bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM upstream_price_monitor_runs
		WHERE rollback_snapshot IS NOT NULL AND applied_at > $1)`, appliedAt.Time).Scan(&newerApplyExists); err != nil {
		return err
	}
	if newerApplyExists {
		return service.ErrUpstreamPriceRunNotApplicable
	}
	var snapshot upstreamPriceRollbackSnapshot
	if err := json.Unmarshal(raw, &snapshot); err != nil {
		return err
	}
	// Display pricing and per-request billing are a separate public-price-page
	// subsystem. Refuse legacy mixed snapshots instead of letting a token-price
	// rollback mutate either subsystem.
	if len(snapshot.Displays) > 0 || rollbackSnapshotChangesPerRequest(snapshot) {
		return fmt.Errorf("%w: rollback snapshot contains display or per-request pricing",
			service.ErrUpstreamPriceRunNotApplicable)
	}
	for _, row := range snapshot.Channels {
		result, err := tx.ExecContext(ctx, `UPDATE channel_model_pricing SET input_price=$2::numeric,output_price=$3::numeric,
			cache_write_price=$4::numeric,cache_read_price=$5::numeric,per_request_price=$6::numeric,updated_at=NOW()
			WHERE id=$1 AND platform='openai'
			  AND input_price IS NOT DISTINCT FROM $7::numeric AND output_price IS NOT DISTINCT FROM $8::numeric
			  AND cache_write_price IS NOT DISTINCT FROM $9::numeric AND cache_read_price IS NOT DISTINCT FROM $10::numeric
			  AND per_request_price IS NOT DISTINCT FROM $11::numeric`, row.ID, row.InputPrice, row.OutputPrice,
			row.CacheWritePrice, row.CacheReadPrice, row.PerRequestPrice, row.AfterInputPrice, row.AfterOutputPrice,
			row.AfterCacheWritePrice, row.AfterCacheReadPrice, row.AfterPerRequestPrice)
		if err != nil {
			return err
		}
		if affected, _ := result.RowsAffected(); affected != 1 {
			return service.ErrUpstreamPriceRunNotApplicable
		}
	}
	for _, row := range snapshot.Intervals {
		result, err := tx.ExecContext(ctx, `UPDATE channel_pricing_intervals SET input_price=$2::numeric,
			output_price=$3::numeric,cache_write_price=$4::numeric,cache_read_price=$5::numeric,
			per_request_price=$6::numeric,updated_at=NOW()
			WHERE id=$1
			  AND EXISTS (SELECT 1 FROM channel_model_pricing cmp
			              WHERE cmp.id=channel_pricing_intervals.pricing_id AND cmp.platform='openai')
			  AND input_price IS NOT DISTINCT FROM $7::numeric AND output_price IS NOT DISTINCT FROM $8::numeric
			  AND cache_write_price IS NOT DISTINCT FROM $9::numeric AND cache_read_price IS NOT DISTINCT FROM $10::numeric
			  AND per_request_price IS NOT DISTINCT FROM $11::numeric`, row.ID, row.InputPrice, row.OutputPrice,
			row.CacheWritePrice, row.CacheReadPrice, row.PerRequestPrice, row.AfterInputPrice, row.AfterOutputPrice,
			row.AfterCacheWritePrice, row.AfterCacheReadPrice, row.AfterPerRequestPrice)
		if err != nil {
			return err
		}
		if affected, _ := result.RowsAffected(); affected != 1 {
			return service.ErrUpstreamPriceRunNotApplicable
		}
	}
	rolledBackAt := time.Now().UTC().Format(time.RFC3339Nano)
	result, err := tx.ExecContext(ctx, `UPDATE upstream_price_monitor_runs SET rollback_available=FALSE,
		summary=jsonb_set(summary,'{rolled_back_at}',to_jsonb($2::text),true)
		WHERE id=$1 AND rollback_available=TRUE`, runID, rolledBackAt)
	if err != nil {
		return err
	}
	if affected, _ := result.RowsAffected(); affected != 1 {
		return service.ErrUpstreamPriceRunNotApplicable
	}
	return tx.Commit()
}

func rollbackSnapshotChangesPerRequest(snapshot upstreamPriceRollbackSnapshot) bool {
	for _, row := range snapshot.Channels {
		if !sameStringPtr(row.PerRequestPrice, row.AfterPerRequestPrice) {
			return true
		}
	}
	for _, row := range snapshot.Intervals {
		if !sameStringPtr(row.PerRequestPrice, row.AfterPerRequestPrice) {
			return true
		}
	}
	return false
}

func sameStringPtr(a, b *string) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

func loadTrustedApplyEvidence(
	ctx context.Context,
	tx *sql.Tx,
	runID int64,
	accountIDs []int64,
) ([]upstreamPriceApplyEvidence, []upstreamPriceApplySkippedModel, error) {
	rows, err := tx.QueryContext(ctx, `SELECT account_id,model_name,billing_mode,status,source,context_key,
		prices,current_prices,suggested_prices
		FROM upstream_price_monitor_evidence
		WHERE run_id=$1 AND source='active_probe' AND billing_mode='token' AND status='trusted'
		  AND context_key NOT LIKE '%-sample-%'
		ORDER BY LOWER(model_name),account_id,observed_at DESC,id DESC FOR UPDATE`, runID)
	if err != nil {
		return nil, nil, err
	}
	defer func() { _ = rows.Close() }()
	type tokenCandidate struct {
		model     string
		byAccount map[int64]upstreamPriceApplyEvidence
	}
	tokenCandidates := make(map[string]*tokenCandidate)
	skippedByModel := make(map[string]upstreamPriceApplySkippedModel)
	expectedAccounts := make(map[int64]struct{}, len(accountIDs))
	for _, accountID := range accountIDs {
		expectedAccounts[accountID] = struct{}{}
	}
	for rows.Next() {
		var accountID int64
		var model, billingMode, status, source, contextKey string
		var measuredRaw, currentRaw, suggestedRaw []byte
		if err := rows.Scan(&accountID, &model, &billingMode, &status, &source, &contextKey,
			&measuredRaw, &currentRaw, &suggestedRaw); err != nil {
			return nil, nil, err
		}
		if status != string(domain.UpstreamPriceEvidenceStatusTrusted) ||
			source != string(domain.UpstreamPriceEvidenceSourceActiveProbe) ||
			billingMode != service.DisplayBillingModeToken || strings.Contains(contextKey, "-sample-") {
			return nil, nil, fmt.Errorf("%w: non-active token evidence escaped the apply filter for model %s",
				service.ErrUpstreamPriceRunNotApplicable, model)
		}
		if _, selected := expectedAccounts[accountID]; !selected {
			continue
		}
		var measured, current, persistedSuggested domain.UpstreamPriceVector
		if err := json.Unmarshal(measuredRaw, &measured); err != nil {
			return nil, nil, err
		}
		if err := json.Unmarshal(currentRaw, &current); err != nil {
			return nil, nil, err
		}
		if err := json.Unmarshal(suggestedRaw, &persistedSuggested); err != nil {
			return nil, nil, err
		}
		if err := rejectUnrepresentableActivePriceVector(model, "measured", measured); err != nil {
			return nil, nil, err
		}
		if err := rejectUnrepresentableActivePriceVector(model, "suggested", persistedSuggested); err != nil {
			return nil, nil, err
		}
		key := strings.ToLower(strings.TrimSpace(model))
		if fixedFee, unrepresentable := unrepresentableFixedRequestFee(measured, persistedSuggested); unrepresentable {
			skippedByModel[key] = upstreamPriceApplySkippedModel{
				Model: model, Reason: "fixed_request_fee_not_representable", FixedPerRequest: fixedFee,
			}
			delete(tokenCandidates, key)
			continue
		}
		if _, skipped := skippedByModel[key]; skipped {
			continue
		}
		suggested := activeTokenChannelSuggestion(measured)
		if !sameTokenPriceSnapshot(persistedSuggested, suggested) {
			return nil, nil, fmt.Errorf("%w: model %s suggested token prices are not measured base prices multiplied by 1.2",
				service.ErrUpstreamPriceSnapshotMismatch, model)
		}
		item := upstreamPriceApplyEvidence{
			Model: model, Current: current, Suggested: suggested, BillingMode: billingMode,
		}
		if err := validateUpstreamPriceApplyEvidence([]upstreamPriceApplyEvidence{item}, false); err != nil {
			return nil, nil, err
		}
		candidate := tokenCandidates[key]
		if candidate == nil {
			candidate = &tokenCandidate{model: model, byAccount: make(map[int64]upstreamPriceApplyEvidence)}
			tokenCandidates[key] = candidate
		}
		if previous, exists := candidate.byAccount[accountID]; exists {
			merged, compatible := mergeCompatibleUpstreamPriceVectors(previous.Suggested, suggested)
			if !compatible || !sameTokenPriceSnapshot(previous.Current, current) {
				return nil, nil, fmt.Errorf("%w: duplicate trusted evidence disagrees for account %d model %s",
					service.ErrUpstreamPriceSnapshotMismatch, accountID, model)
			}
			item.Suggested = merged
		}
		candidate.byAccount[accountID] = item
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	out := make([]upstreamPriceApplyEvidence, 0, len(tokenCandidates))
	for _, candidate := range tokenCandidates {
		if len(candidate.byAccount) != len(expectedAccounts) {
			continue
		}
		vectors := make([]domain.UpstreamPriceVector, 0, len(accountIDs))
		var current domain.UpstreamPriceVector
		complete := true
		for index, accountID := range accountIDs {
			value, exists := candidate.byAccount[accountID]
			if !exists {
				complete = false
				break
			}
			vectors = append(vectors, value.Suggested)
			if index == 0 {
				current = value.Current
			} else if !sameTokenPriceSnapshot(current, value.Current) {
				return nil, nil, fmt.Errorf("%w: sampled current prices disagree across accounts for model %s",
					service.ErrUpstreamPriceSnapshotMismatch, candidate.model)
			}
		}
		if !complete {
			continue
		}
		merged := mergeMaximumCommonUpstreamPriceVector(vectors)
		if upstreamPriceVectorEmpty(merged) {
			continue
		}
		out = append(out, upstreamPriceApplyEvidence{
			Model: candidate.model, Current: current, Suggested: merged, BillingMode: service.DisplayBillingModeToken,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		left, right := strings.ToLower(out[i].Model), strings.ToLower(out[j].Model)
		if left != right {
			return left < right
		}
		return out[i].BillingMode < out[j].BillingMode
	})
	skipped := make([]upstreamPriceApplySkippedModel, 0, len(skippedByModel))
	for _, item := range skippedByModel {
		skipped = append(skipped, item)
	}
	sort.Slice(skipped, func(i, j int) bool {
		return strings.ToLower(skipped[i].Model) < strings.ToLower(skipped[j].Model)
	})
	return out, skipped, nil
}

func unrepresentableFixedRequestFee(measured, suggested domain.UpstreamPriceVector) (float64, bool) {
	for _, value := range []*float64{measured.FixedPerRequest, suggested.FixedPerRequest} {
		if value != nil && math.Abs(*value) > upstreamPriceFixedRequestFeeTolerance {
			fee := 0.0
			if measured.FixedPerRequest != nil {
				fee = *measured.FixedPerRequest
			} else {
				fee = *value / 1.2
			}
			return fee, true
		}
	}
	return 0, false
}

func activeTokenChannelSuggestion(measured domain.UpstreamPriceVector) domain.UpstreamPriceVector {
	return domain.UpstreamPriceVector{
		InputPerMillion:      multiplyActiveTokenPrice(measured.InputPerMillion),
		OutputPerMillion:     multiplyActiveTokenPrice(measured.OutputPerMillion),
		CacheWritePerMillion: multiplyActiveTokenPrice(measured.CacheWritePerMillion),
		CacheReadPerMillion:  multiplyActiveTokenPrice(measured.CacheReadPerMillion),
	}
}

func multiplyActiveTokenPrice(value *float64) *float64 {
	if value == nil {
		return nil
	}
	result := decimal.NewFromFloat(*value).Mul(decimal.NewFromFloat(1.2)).InexactFloat64()
	return &result
}

// rejectUnrepresentableActivePriceVector is intentionally fail closed. The
// live token channel can represent only its four token dimensions. Public
// three-tier request prices must never cross into channel token billing. A
// fixed request fee is handled as a model-level skip by the loader.
func rejectUnrepresentableActivePriceVector(
	model string,
	label string,
	value domain.UpstreamPriceVector,
) error {
	if value.PerRequestLTE256K != nil || value.PerRequest256K512K != nil || value.PerRequestGT512K != nil {
		return fmt.Errorf("%w: model %s %s evidence contains per-request pricing",
			service.ErrUpstreamPriceRunNotApplicable, model, label)
	}
	return nil
}

func mergeMaximumCommonUpstreamPriceVector(values []domain.UpstreamPriceVector) domain.UpstreamPriceVector {
	return domain.UpstreamPriceVector{
		InputPerMillion:      maximumCommonFloat(values, func(v domain.UpstreamPriceVector) *float64 { return v.InputPerMillion }),
		OutputPerMillion:     maximumCommonFloat(values, func(v domain.UpstreamPriceVector) *float64 { return v.OutputPerMillion }),
		CacheWritePerMillion: maximumCommonFloat(values, func(v domain.UpstreamPriceVector) *float64 { return v.CacheWritePerMillion }),
		CacheReadPerMillion:  maximumCommonFloat(values, func(v domain.UpstreamPriceVector) *float64 { return v.CacheReadPerMillion }),
	}
}

func mergeCompatibleUpstreamPriceVectors(a, b domain.UpstreamPriceVector) (domain.UpstreamPriceVector, bool) {
	var out domain.UpstreamPriceVector
	fields := []struct {
		a, b *float64
		set  func(*float64)
	}{
		{a.InputPerMillion, b.InputPerMillion, func(v *float64) { out.InputPerMillion = v }},
		{a.OutputPerMillion, b.OutputPerMillion, func(v *float64) { out.OutputPerMillion = v }},
		{a.CacheWritePerMillion, b.CacheWritePerMillion, func(v *float64) { out.CacheWritePerMillion = v }},
		{a.CacheReadPerMillion, b.CacheReadPerMillion, func(v *float64) { out.CacheReadPerMillion = v }},
		{a.PerRequestLTE256K, b.PerRequestLTE256K, func(v *float64) { out.PerRequestLTE256K = v }},
		{a.PerRequest256K512K, b.PerRequest256K512K, func(v *float64) { out.PerRequest256K512K = v }},
		{a.PerRequestGT512K, b.PerRequestGT512K, func(v *float64) { out.PerRequestGT512K = v }},
	}
	for _, field := range fields {
		merged, compatible := mergeCompatiblePrice(field.a, field.b)
		if !compatible {
			return domain.UpstreamPriceVector{}, false
		}
		field.set(merged)
	}
	return out, true
}

func mergeCompatiblePrice(a, b *float64) (*float64, bool) {
	if a == nil {
		return b, true
	}
	if b == nil {
		return a, true
	}
	tolerance := math.Max(1e-8, math.Max(math.Abs(*a), math.Abs(*b))*.005)
	if math.Abs(*a-*b) > tolerance {
		return nil, false
	}
	value := math.Max(*a, *b)
	return &value, true
}

func maximumCommonFloat(values []domain.UpstreamPriceVector, pick func(domain.UpstreamPriceVector) *float64) *float64 {
	if len(values) == 0 {
		return nil
	}
	var maximum float64
	for index, value := range values {
		selected := pick(value)
		if selected == nil {
			return nil
		}
		if index == 0 || *selected > maximum {
			maximum = *selected
		}
	}
	return &maximum
}

func upstreamPriceVectorEmpty(value domain.UpstreamPriceVector) bool {
	return value.InputPerMillion == nil && value.OutputPerMillion == nil &&
		value.CacheWritePerMillion == nil && value.CacheReadPerMillion == nil &&
		value.PerRequestLTE256K == nil && value.PerRequest256K512K == nil && value.PerRequestGT512K == nil
}

func validateUpstreamPriceApplyEvidence(items []upstreamPriceApplyEvidence, _ bool) error {
	for _, item := range items {
		if item.BillingMode != service.DisplayBillingModeToken {
			return fmt.Errorf("%w: model %s is not token billed",
				service.ErrUpstreamPriceRunNotApplicable, item.Model)
		}
		if item.Suggested.FixedPerRequest != nil && math.Abs(*item.Suggested.FixedPerRequest) > upstreamPriceFixedRequestFeeTolerance {
			return fmt.Errorf("%w: model %s has a fixed request fee which token channel pricing cannot represent",
				service.ErrUpstreamPriceRunNotApplicable, item.Model)
		}
		if item.Suggested.PerRequestLTE256K != nil || item.Suggested.PerRequest256K512K != nil ||
			item.Suggested.PerRequestGT512K != nil {
			return fmt.Errorf("%w: model %s contains per-request price-page evidence",
				service.ErrUpstreamPriceRunNotApplicable, item.Model)
		}
		fields := []struct {
			name      string
			suggested *float64
		}{
			{"input", item.Suggested.InputPerMillion},
			{"output", item.Suggested.OutputPerMillion},
			{"cache_write", item.Suggested.CacheWritePerMillion},
			{"cache_read", item.Suggested.CacheReadPerMillion},
		}
		for _, field := range fields {
			if field.suggested == nil {
				continue
			}
			if math.IsNaN(*field.suggested) || math.IsInf(*field.suggested, 0) || *field.suggested <= 0 {
				return fmt.Errorf("%w: model %s has non-positive %s price",
					service.ErrUpstreamPriceRunNotApplicable, item.Model, field.name)
			}
		}
	}
	return nil
}

func applyUpstreamPerRequestEvidence(
	ctx context.Context,
	tx *sql.Tx,
	evidence upstreamPriceApplyEvidence,
	channelIDs []int64,
	snapshot *upstreamPriceRollbackSnapshot,
) (int, bool, error) {
	return 0, false, fmt.Errorf("%w: per-request price-page evidence is display-only and cannot be applied to channel pricing",
		service.ErrUpstreamPriceRunNotApplicable)
}

func decimalString(value *float64) any {
	if value == nil {
		return nil
	}
	return decimal.NewFromFloat(*value).Round(12).String()
}

func numericAfter(before *string, next any) *string {
	if next == nil {
		return before
	}
	switch value := next.(type) {
	case string:
		out := value
		return &out
	case *string:
		return value
	default:
		out := fmt.Sprint(value)
		return &out
	}
}

func channelRollbackWithAfter(before upstreamPriceChannelRollback, input, output, cacheWrite, cacheRead, request any) upstreamPriceChannelRollback {
	before.AfterInputPrice = numericAfter(before.InputPrice, input)
	before.AfterOutputPrice = numericAfter(before.OutputPrice, output)
	before.AfterCacheWritePrice = numericAfter(before.CacheWritePrice, cacheWrite)
	before.AfterCacheReadPrice = numericAfter(before.CacheReadPrice, cacheRead)
	before.AfterPerRequestPrice = numericAfter(before.PerRequestPrice, request)
	return before
}

func intervalRollbackWithAfter(before upstreamPriceIntervalRollback, input, output, cacheWrite, cacheRead, request any) upstreamPriceIntervalRollback {
	before.AfterInputPrice = numericAfter(before.InputPrice, input)
	before.AfterOutputPrice = numericAfter(before.OutputPrice, output)
	before.AfterCacheWritePrice = numericAfter(before.CacheWritePrice, cacheWrite)
	before.AfterCacheReadPrice = numericAfter(before.CacheReadPrice, cacheRead)
	before.AfterPerRequestPrice = numericAfter(before.PerRequestPrice, request)
	return before
}

func displayRollbackWithAfter(before upstreamPriceDisplayRollback, multiplier, low, middle, high any) upstreamPriceDisplayRollback {
	before.AfterModelMultiplier = numericAfter(before.ModelMultiplier, multiplier)
	before.AfterPerRequestLTE256K = numericAfter(before.PerRequestLTE256K, low)
	before.AfterPerRequest256K512K = numericAfter(before.PerRequest256K512K, middle)
	before.AfterPerRequestGT512K = numericAfter(before.PerRequestGT512K, high)
	return before
}

func resolveDisplayMultiplier(ctx context.Context, tx *sql.Tx, evidence upstreamPriceApplyEvidence, decimals int) (decimal.Decimal, bool, error) {
	var officialInput, officialOutput sql.NullString
	err := tx.QueryRowContext(ctx, `SELECT official_input_per_million::text,official_output_per_million::text
		FROM display_model_prices WHERE platform='openai' AND billing_mode='token' AND LOWER(model_name)=LOWER($1)
		ORDER BY id LIMIT 1 FOR UPDATE`, evidence.Model).Scan(&officialInput, &officialOutput)
	if errors.Is(err, sql.ErrNoRows) {
		return decimal.Zero, false, nil
	}
	if err != nil {
		return decimal.Zero, false, err
	}
	return calculateDisplayMultiplier(evidence, officialInput, officialOutput, decimals)
}

func calculateDisplayMultiplier(evidence upstreamPriceApplyEvidence, officialInput, officialOutput sql.NullString, decimals int) (decimal.Decimal, bool, error) {
	var multiplier decimal.Decimal
	set := false
	if evidence.Suggested.InputPerMillion != nil && officialInput.Valid {
		base, err := decimal.NewFromString(officialInput.String)
		if err != nil || base.Sign() <= 0 {
			return decimal.Zero, false, nil
		}
		multiplier = decimal.NewFromFloat(*evidence.Suggested.InputPerMillion).Div(base)
		set = true
	}
	if evidence.Suggested.OutputPerMillion != nil && officialOutput.Valid {
		base, err := decimal.NewFromString(officialOutput.String)
		if err == nil && base.Sign() > 0 {
			candidate := decimal.NewFromFloat(*evidence.Suggested.OutputPerMillion).Div(base)
			if set && candidate.Sub(multiplier).Abs().GreaterThan(decimal.NewFromFloat(0.001)) {
				return decimal.Zero, false, nil
			}
			if !set {
				multiplier, set = candidate, true
			}
		}
	}
	if !set {
		return decimal.Zero, false, nil
	}
	return multiplier.Round(int32(decimals)), true, nil
}

func perMillionToPerToken(value *float64) any {
	if value == nil {
		return nil
	}
	return decimal.NewFromFloat(*value).Div(decimal.NewFromInt(1_000_000)).StringFixed(12)
}

func nullStringPtr(value sql.NullString) *string {
	if !value.Valid {
		return nil
	}
	out := value.String
	return &out
}

func sameUpstreamPriceVector(a, b domain.UpstreamPriceVector) bool {
	return sameFloatPtr(a.InputPerMillion, b.InputPerMillion) && sameFloatPtr(a.OutputPerMillion, b.OutputPerMillion) &&
		sameFloatPtr(a.CacheWritePerMillion, b.CacheWritePerMillion) && sameFloatPtr(a.CacheReadPerMillion, b.CacheReadPerMillion) &&
		sameFloatPtr(a.PerRequestLTE256K, b.PerRequestLTE256K) && sameFloatPtr(a.PerRequest256K512K, b.PerRequest256K512K) &&
		sameFloatPtr(a.PerRequestGT512K, b.PerRequestGT512K)
}

func sameFloatPtr(a, b *float64) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return decimal.NewFromFloat(*a).Sub(decimal.NewFromFloat(*b)).Abs().LessThanOrEqual(decimal.NewFromFloat(1e-9))
}
