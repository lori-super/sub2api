package service

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strings"

	"github.com/shopspring/decimal"
)

// UpstreamTokenPriceUpdate is one complete public-page token price snapshot.
// Page prices are expressed per million tokens. Downstream prices already
// include the fixed 1.20 markup; repositories convert them to per-token values
// only when writing the real channel tables.
type UpstreamTokenPriceUpdate struct {
	ModelName string
	Provider  string

	OfficialInput      *float64
	OfficialOutput     *float64
	OfficialCacheWrite *float64
	OfficialCacheRead  *float64

	InputPerMillion      *float64
	OutputPerMillion     *float64
	CacheWritePerMillion *float64
	CacheReadPerMillion  *float64
}

type UpstreamTokenPriceSyncResult struct {
	SourceModels        int `json:"source_models"`
	Models              int `json:"models"`
	MatchedModels       int `json:"matched_models"`
	UpdatedModels       int `json:"updated_models"`
	ChangedModels       int `json:"changed_models"`
	ChangedChannelRows  int `json:"changed_channel_rows"`
	ChangedIntervalRows int `json:"changed_interval_rows"`
	ChangedDisplayRows  int `json:"changed_display_rows"`
	CreatedDisplayRows  int `json:"created_display_rows"`
}

// UpstreamTokenPriceRepository owns the atomic public-page mutation across
// live channel prices, their normal-context intervals, and exact customer
// display prices. It is separate from paid-probe evidence and ApplyRun.
type UpstreamTokenPriceRepository interface {
	SyncTokenPrices(context.Context, []int64, []UpstreamTokenPriceUpdate) (*UpstreamTokenPriceSyncResult, error)
}

// SyncTokenPrices treats the x5m5x public price page as the authority for every
// configured managed token model. Paid probes are intentionally not consulted
// and this method never creates monitor evidence or enters ApplyRun.
func (s *UpstreamPriceMonitorService) SyncTokenPrices(ctx context.Context) (*UpstreamTokenPriceSyncResult, error) {
	if s == nil || s.repo == nil || s.pricePage == nil {
		return nil, ErrUpstreamPriceMonitorUnavailable
	}
	pricingRepo, ok := s.repo.(UpstreamTokenPriceRepository)
	if !ok {
		return nil, ErrUpstreamPriceMonitorUnavailable
	}
	fetcher, ok := s.pricePage.(UpstreamTokenPricePageFetcher)
	if !ok {
		return nil, ErrUpstreamPriceMonitorUnavailable
	}
	cfg, err := s.repo.GetConfig(ctx)
	if err != nil {
		return nil, fmt.Errorf("get token price sync scope: %w", err)
	}
	channelIDs := uniquePositiveInt64s(cfg.ChannelIDs)
	if len(channelIDs) == 0 {
		return nil, ErrUpstreamPriceMonitorInvalidConfig
	}

	models, err := normalizeDomesticModelAllowlist(cfg.DomesticModels)
	if err != nil || len(models) == 0 {
		return nil, ErrUpstreamPriceMonitorInvalidConfig
	}
	pagePrices, err := fetcher.FetchTokenPrices(ctx)
	if err != nil {
		return nil, err
	}
	byName := make(map[string]UpstreamTokenDisplayPrice, len(pagePrices))
	for rawName, pagePrice := range pagePrices {
		model := strings.TrimSpace(pagePrice.ModelName)
		if model == "" {
			model = strings.TrimSpace(rawName)
		}
		key := strings.ToLower(model)
		if key == "" || len(model) > 255 || strings.ContainsAny(model, " \t\r\n,") {
			return nil, fmt.Errorf("%w: invalid public token model %q", ErrUpstreamPriceMonitorInvalidConfig, model)
		}
		if _, duplicate := byName[key]; duplicate {
			return nil, fmt.Errorf("%w: duplicate public token model %s", ErrUpstreamPriceMonitorInvalidConfig, model)
		}
		pagePrice.ModelName = model
		byName[key] = pagePrice
	}

	updates := make([]UpstreamTokenPriceUpdate, 0, len(models))
	for _, configuredModel := range models {
		pagePrice, exists := byName[strings.ToLower(configuredModel)]
		if !exists {
			return nil, fmt.Errorf("%w: public token page omitted configured model %s",
				ErrUpstreamPriceMonitorInvalidConfig, configuredModel)
		}
		model := pagePrice.ModelName
		if !validPublicTokenRequiredPrice(pagePrice.SellingInput) ||
			!validPublicTokenRequiredPrice(pagePrice.SellingOutput) ||
			!validPublicTokenOptionalPrice(pagePrice.SellingCacheWrite) ||
			!validPublicTokenOptionalPrice(pagePrice.SellingCacheRead) ||
			!validPublicTokenOptionalPrice(pagePrice.OfficialInput) ||
			!validPublicTokenOptionalPrice(pagePrice.OfficialOutput) ||
			!validPublicTokenOptionalPrice(pagePrice.OfficialCacheWrite) ||
			!validPublicTokenOptionalPrice(pagePrice.OfficialCacheRead) {
			return nil, fmt.Errorf("%w: invalid public token prices for %s", ErrUpstreamPriceMonitorInvalidConfig, model)
		}
		updates = append(updates, UpstreamTokenPriceUpdate{
			ModelName:     model,
			Provider:      inferDisplayProvider("openai", model),
			OfficialInput: cloneDisplayFloat(pagePrice.OfficialInput), OfficialOutput: cloneDisplayFloat(pagePrice.OfficialOutput),
			OfficialCacheWrite: cloneDisplayFloat(pagePrice.OfficialCacheWrite), OfficialCacheRead: cloneDisplayFloat(pagePrice.OfficialCacheRead),
			InputPerMillion: markedUpPublicTokenPrice(pagePrice.SellingInput), OutputPerMillion: markedUpPublicTokenPrice(pagePrice.SellingOutput),
			CacheWritePerMillion: markedUpPublicTokenPrice(pagePrice.SellingCacheWrite), CacheReadPerMillion: markedUpPublicTokenPrice(pagePrice.SellingCacheRead),
		})
	}
	if len(updates) == 0 {
		return nil, fmt.Errorf("%w: public token price page is empty", ErrUpstreamPriceMonitorInvalidConfig)
	}
	sort.SliceStable(updates, func(i, j int) bool {
		return strings.ToLower(updates[i].ModelName) < strings.ToLower(updates[j].ModelName)
	})

	result, err := pricingRepo.SyncTokenPrices(ctx, channelIDs, updates)
	if err != nil {
		return nil, err
	}
	if result == nil || result.Models <= 0 || result.Models > len(updates) {
		return nil, fmt.Errorf("%w: incomplete token price sync", ErrUpstreamPriceRunNotApplicable)
	}
	if result.Models != len(updates) {
		return nil, fmt.Errorf("%w: token price sync matched %d of %d configured models",
			ErrUpstreamPriceRunNotApplicable, result.Models, len(updates))
	}
	result.SourceModels = len(pagePrices)
	result.MatchedModels = result.Models
	result.UpdatedModels = result.Models
	if (result.ChangedChannelRows > 0 || result.ChangedIntervalRows > 0) && s.cacheInvalidator != nil {
		s.cacheInvalidator.InvalidatePricingCache()
	}
	return result, nil
}

func validPublicTokenRequiredPrice(value *float64) bool {
	return value != nil && *value > 0 && !math.IsNaN(*value) && !math.IsInf(*value, 0)
}

func validPublicTokenOptionalPrice(value *float64) bool {
	return value == nil || (*value >= 0 && !math.IsNaN(*value) && !math.IsInf(*value, 0))
}

func markedUpPublicTokenPrice(value *float64) *float64 {
	if value == nil {
		return nil
	}
	markedUp, _ := decimal.NewFromFloat(*value).
		Mul(decimal.NewFromFloat(DisplayUpstreamPriceMarkup)).Float64()
	return &markedUp
}
