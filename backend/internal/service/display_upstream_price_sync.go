package service

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"
)

const (
	DisplayUpstreamPriceMarkup    = 1.2
	DisplayOfficialPriceX5M5X     = "x5m5x_public_pricing"
	DisplayUpstreamPriceSourceURL = x5m5xPricingPageURL
)

type DisplayUpstreamTokenPriceUpdate struct {
	ModelID  int64
	Provider string

	OfficialInput      *float64
	OfficialOutput     *float64
	OfficialCacheWrite *float64
	OfficialCacheRead  *float64

	DisplayInput           *float64
	DisplayOutput          *float64
	DisplayCacheWrite      *float64
	DisplayCacheRead       *float64
	OfficialPriceSource    string
	OfficialPriceSourceURL string
	SyncedAt               time.Time
}

// DisplayUpstreamTokenPriceRepository is intentionally separate from the
// general display repository contract. Its implementation may update only
// display_model_prices; channel pricing and billing tables are out of scope.
type DisplayUpstreamTokenPriceRepository interface {
	ApplyUpstreamTokenDisplayPriceUpdates(context.Context, []DisplayUpstreamTokenPriceUpdate) (int, error)
}

type DisplayUpstreamTokenPriceSyncResult struct {
	SourceModels  int `json:"source_models"`
	MatchedModels int `json:"matched_models"`
	UpdatedModels int `json:"updated_models"`
	ChangedModels int `json:"changed_models"`
}

// SyncUpstreamTokenDisplayPrices copies the x5m5x public page's official
// references and exact downstream display prices (upstream selling × 1.2) into
// display_model_prices. It cannot mutate actual channel or user billing prices.
func (s *DisplayPricingService) SyncUpstreamTokenDisplayPrices(ctx context.Context, fetcher UpstreamTokenPricePageFetcher) (*DisplayUpstreamTokenPriceSyncResult, error) {
	if s == nil || s.repo == nil || fetcher == nil {
		return nil, ErrDisplayPriceInvalid
	}
	applyRepo, ok := s.repo.(DisplayUpstreamTokenPriceRepository)
	if !ok {
		return nil, fmt.Errorf("display upstream token sync repository unavailable")
	}
	source, err := fetcher.FetchTokenPrices(ctx)
	if err != nil {
		return nil, err
	}
	models, err := s.repo.ListModels(ctx)
	if err != nil {
		return nil, fmt.Errorf("list display models for upstream sync: %w", err)
	}
	now := time.Now().UTC()
	updates := make([]DisplayUpstreamTokenPriceUpdate, 0, len(models))
	missing := make([]string, 0)
	for i := range models {
		model := models[i]
		if !model.Enabled || model.BillingMode != DisplayBillingModeToken || !isDomesticDisplayProvider(model.Provider) {
			continue
		}
		upstream, exists := source[strings.ToLower(strings.TrimSpace(model.ModelName))]
		if !exists {
			missing = append(missing, model.ModelName)
			continue
		}
		update := DisplayUpstreamTokenPriceUpdate{
			ModelID: model.ID, Provider: model.Provider, SyncedAt: now,
			OfficialPriceSource: DisplayOfficialPriceX5M5X, OfficialPriceSourceURL: DisplayUpstreamPriceSourceURL,
			OfficialInput: cloneDisplayFloat(upstream.OfficialInput), OfficialOutput: cloneDisplayFloat(upstream.OfficialOutput),
			OfficialCacheWrite: cloneDisplayFloat(upstream.OfficialCacheWrite), OfficialCacheRead: cloneDisplayFloat(upstream.OfficialCacheRead),
			DisplayInput: markedUpDisplayPrice(upstream.SellingInput), DisplayOutput: markedUpDisplayPrice(upstream.SellingOutput),
			DisplayCacheWrite: markedUpDisplayPrice(upstream.SellingCacheWrite), DisplayCacheRead: markedUpDisplayPrice(upstream.SellingCacheRead),
		}
		updates = append(updates, update)
	}
	sort.SliceStable(updates, func(i, j int) bool { return updates[i].ModelID < updates[j].ModelID })
	if len(missing) > 0 {
		sort.Strings(missing)
		return nil, fmt.Errorf("x5m5x token pricing page omitted configured models: %s", strings.Join(missing, ", "))
	}
	if len(updates) == 0 {
		return nil, fmt.Errorf("x5m5x token pricing page matched no configured domestic display models")
	}
	changed, err := applyRepo.ApplyUpstreamTokenDisplayPriceUpdates(ctx, updates)
	if err != nil {
		return nil, fmt.Errorf("apply upstream token display prices: %w", err)
	}
	return &DisplayUpstreamTokenPriceSyncResult{SourceModels: len(source), MatchedModels: len(updates), UpdatedModels: len(updates), ChangedModels: changed}, nil
}

func markedUpDisplayPrice(value *float64) *float64 {
	if value == nil {
		return nil
	}
	markedUp := *value * DisplayUpstreamPriceMarkup
	if markedUp < 0 || math.IsNaN(markedUp) || math.IsInf(markedUp, 0) {
		return nil
	}
	return displayFloat64Ptr(markedUp)
}

func cloneDisplayFloat(value *float64) *float64 {
	if value == nil {
		return nil
	}
	return displayFloat64Ptr(*value)
}
