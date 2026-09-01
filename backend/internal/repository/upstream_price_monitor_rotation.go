package repository

import (
	"context"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/lib/pq"
)

// SelectActiveProbeModels returns a persistent least-recently-attempted slice
// of the current managed scope. Every selected model writes active-probe
// baseline evidence before a paid request, so failures and unobservable
// attempts advance the rotation too instead of starving the rest of the
// catalogue. The caller-provided order is the deterministic tie-breaker.
func (r *upstreamPriceMonitorRepository) SelectActiveProbeModels(
	ctx context.Context,
	managed []string,
	limit int,
) ([]string, error) {
	if r == nil || r.db == nil || limit <= 0 {
		return nil, service.ErrUpstreamPriceMonitorInvalidConfig
	}
	normalized := make([]string, 0, len(managed))
	seen := make(map[string]struct{}, len(managed))
	for _, raw := range managed {
		model := strings.TrimSpace(raw)
		key := strings.ToLower(model)
		if key == "" {
			continue
		}
		if _, duplicate := seen[key]; duplicate {
			continue
		}
		seen[key] = struct{}{}
		normalized = append(normalized, model)
	}
	if len(normalized) == 0 {
		return []string{}, nil
	}
	if limit > len(normalized) {
		limit = len(normalized)
	}
	rows, err := r.db.QueryContext(ctx, `WITH requested AS (
		SELECT model_name,ordinality
		FROM UNNEST($1::text[]) WITH ORDINALITY AS requested_models(model_name,ordinality)
	), latest_attempt AS (
		SELECT LOWER(model_name) AS model_key,MAX(created_at) AS attempted_at
		FROM upstream_price_monitor_evidence
		WHERE source='active_probe' AND billing_mode='token'
		GROUP BY LOWER(model_name)
	)
	SELECT requested.model_name
	FROM requested
	LEFT JOIN latest_attempt ON latest_attempt.model_key=LOWER(requested.model_name)
	ORDER BY latest_attempt.attempted_at ASC NULLS FIRST,requested.ordinality
	LIMIT $2`, pq.Array(normalized), limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	selected := make([]string, 0, limit)
	for rows.Next() {
		var model string
		if err := rows.Scan(&model); err != nil {
			return nil, err
		}
		selected = append(selected, model)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return selected, nil
}
