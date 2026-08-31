package service

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/domain"
)

// UpstreamPerRequestPriceUpdate is a complete downstream three-tier price.
// The public page's higher tiers are deliberately ignored: both channel and
// display pricing derive them from the first tier using the fixed 1/1.5/2
// shape.
type UpstreamPerRequestPriceUpdate struct {
	ModelName   string
	BasePrice   float64
	MiddlePrice float64
	HighPrice   float64
}

type UpstreamPerRequestPriceSyncResult struct {
	Models             int
	ChangedModels      int
	ChangedChannelRows int
}

// UpstreamPerRequestPriceRepository owns the atomic, price-only mutation of
// the pre-existing channel and display catalogue structures. It is kept out of
// UpstreamPriceMonitorRepository so the token evidence/apply contract remains
// unchanged.
type UpstreamPerRequestPriceRepository interface {
	SyncPerRequestPrices(context.Context, []int64, []UpstreamPerRequestPriceUpdate) (*UpstreamPerRequestPriceSyncResult, error)
}

// SyncPerRequestPrices independently synchronizes the x5m5x public per-request
// price page. It creates no evidence and never enters RunOnce or ApplyRun.
func (s *UpstreamPriceMonitorService) SyncPerRequestPrices(ctx context.Context) (*UpstreamPerRequestPriceSyncResult, error) {
	if s == nil || s.repo == nil || s.pricePage == nil {
		return nil, ErrUpstreamPriceMonitorUnavailable
	}
	pricingRepo, ok := s.repo.(UpstreamPerRequestPriceRepository)
	if !ok {
		return nil, ErrUpstreamPriceMonitorUnavailable
	}
	cfg, err := s.repo.GetConfig(ctx)
	if err != nil {
		return nil, fmt.Errorf("get per-request price sync scope: %w", err)
	}
	channelIDs := uniquePositiveInt64s(cfg.ChannelIDs)
	models, err := normalizePerRequestModelAllowlist(cfg.PerRequestModels)
	if err != nil || len(channelIDs) == 0 || len(models) == 0 {
		return nil, ErrUpstreamPriceMonitorInvalidConfig
	}

	pagePrices, err := s.pricePage.FetchPerRequestPrices(ctx)
	if err != nil {
		return nil, err
	}
	byName := make(map[string]domain.UpstreamPriceVector, len(pagePrices))
	for rawName, vector := range pagePrices {
		key := strings.ToLower(strings.TrimSpace(rawName))
		if key == "" {
			return nil, ErrUpstreamPriceMonitorInvalidConfig
		}
		if _, duplicate := byName[key]; duplicate {
			return nil, fmt.Errorf("%w: duplicate public per-request model %s", ErrUpstreamPriceMonitorInvalidConfig, rawName)
		}
		byName[key] = vector
	}

	updates := make([]UpstreamPerRequestPriceUpdate, 0, len(models))
	for _, model := range models {
		vector, exists := byName[strings.ToLower(model)]
		if !exists || vector.PerRequestLTE256K == nil || !validPositivePerRequestPrice(*vector.PerRequestLTE256K) {
			return nil, fmt.Errorf("%w: public per-request first tier missing for %s", ErrUpstreamPriceMonitorInvalidConfig, model)
		}
		base := *vector.PerRequestLTE256K * DisplayPerRequestMarkup
		middle := base * 1.5
		high := base * 2
		if !validPositivePerRequestPrice(base) || !validPositivePerRequestPrice(middle) || !validPositivePerRequestPrice(high) {
			return nil, fmt.Errorf("%w: invalid derived per-request prices for %s", ErrUpstreamPriceMonitorInvalidConfig, model)
		}
		updates = append(updates, UpstreamPerRequestPriceUpdate{
			ModelName: model, BasePrice: base, MiddlePrice: middle, HighPrice: high,
		})
	}
	sort.SliceStable(updates, func(i, j int) bool {
		return strings.ToLower(updates[i].ModelName) < strings.ToLower(updates[j].ModelName)
	})

	result, err := pricingRepo.SyncPerRequestPrices(ctx, channelIDs, updates)
	if err != nil {
		return nil, err
	}
	if result == nil || result.Models != len(updates) {
		return nil, fmt.Errorf("%w: incomplete per-request price sync", ErrUpstreamPriceRunNotApplicable)
	}
	if result.ChangedChannelRows > 0 && s.cacheInvalidator != nil {
		s.cacheInvalidator.InvalidatePricingCache()
	}
	return result, nil
}

func validPositivePerRequestPrice(value float64) bool {
	return value > 0 && !math.IsNaN(value) && !math.IsInf(value, 0)
}
