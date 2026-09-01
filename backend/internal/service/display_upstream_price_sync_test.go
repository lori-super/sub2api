package service

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

type upstreamTokenPageStub struct {
	prices map[string]UpstreamTokenDisplayPrice
}

func (s upstreamTokenPageStub) FetchTokenPrices(context.Context) (map[string]UpstreamTokenDisplayPrice, error) {
	return s.prices, nil
}

type upstreamDisplaySyncRepoStub struct {
	stubDisplayPricingRepo
	updates []DisplayUpstreamTokenPriceUpdate
}

func (r *upstreamDisplaySyncRepoStub) ApplyUpstreamTokenDisplayPriceUpdates(_ context.Context, updates []DisplayUpstreamTokenPriceUpdate) (int, error) {
	r.updates = append([]DisplayUpstreamTokenPriceUpdate(nil), updates...)
	return len(updates), nil
}

func TestSyncUpstreamTokenDisplayPricesWritesOnlyMatchedDomesticDisplayRows(t *testing.T) {
	officialInput, sellingInput, sellingCacheWrite, zero := 9.8, 0.98, 0.196, 0.0
	repo := &upstreamDisplaySyncRepoStub{stubDisplayPricingRepo: stubDisplayPricingRepo{models: []DisplayModelPrice{
		{ID: 8, Platform: "openai", ModelName: "glm-5.1", Provider: "zhipu", BillingMode: DisplayBillingModeToken, Currency: "CNY", Enabled: true},
		{ID: 9, Platform: "openai", ModelName: "glm-disabled", Provider: "zhipu", BillingMode: DisplayBillingModeToken, Currency: "CNY", Enabled: false},
		{ID: 10, Platform: "openai", ModelName: "glm-once", Provider: "zhipu", BillingMode: DisplayBillingModePerRequest, Currency: "CNY", Enabled: true},
	}}}
	fetcher := upstreamTokenPageStub{prices: map[string]UpstreamTokenDisplayPrice{
		"glm-5.1":        {OfficialInput: &officialInput, SellingInput: &sellingInput, SellingCacheWrite: &sellingCacheWrite, SellingCacheRead: &zero},
		"not-configured": {SellingInput: &sellingInput},
	}}

	result, err := NewDisplayPricingService(repo).SyncUpstreamTokenDisplayPrices(context.Background(), fetcher)
	require.NoError(t, err)
	require.Equal(t, &DisplayUpstreamTokenPriceSyncResult{SourceModels: 2, MatchedModels: 1, UpdatedModels: 1, ChangedModels: 1}, result)
	require.Len(t, repo.updates, 1)
	update := repo.updates[0]
	require.EqualValues(t, 8, update.ModelID)
	require.InDelta(t, 9.8, *update.OfficialInput, 1e-12)
	require.InDelta(t, 1.176, *update.DisplayInput, 1e-12)
	require.InDelta(t, 0.2352, *update.DisplayCacheWrite, 1e-12)
	require.InDelta(t, 0, *update.DisplayCacheRead, 1e-12)
	require.Equal(t, DisplayOfficialPriceX5M5X, update.OfficialPriceSource)
}

func TestSyncUpstreamTokenDisplayPricesFailsClosedWhenConfiguredModelMissing(t *testing.T) {
	price := 0.1
	repo := &upstreamDisplaySyncRepoStub{stubDisplayPricingRepo: stubDisplayPricingRepo{models: []DisplayModelPrice{
		{ID: 1, Platform: "openai", ModelName: "deepseek-a", Provider: "deepseek", BillingMode: DisplayBillingModeToken, Currency: "CNY", Enabled: true},
		{ID: 2, Platform: "openai", ModelName: "deepseek-b", Provider: "deepseek", BillingMode: DisplayBillingModeToken, Currency: "CNY", Enabled: true},
	}}}
	_, err := NewDisplayPricingService(repo).SyncUpstreamTokenDisplayPrices(context.Background(), upstreamTokenPageStub{prices: map[string]UpstreamTokenDisplayPrice{
		"deepseek-a": {SellingInput: &price},
	}})
	require.ErrorContains(t, err, "deepseek-b")
	require.Empty(t, repo.updates, "a partial source snapshot must not write any model")
}
