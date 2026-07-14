package app

import (
	"context"
	"fmt"
	"testing"

	"fluxmaker/internal/config"
	"fluxmaker/internal/domain"
	"fluxmaker/internal/num"
	"fluxmaker/internal/venue"
)

type isolatedRuleClient struct {
	registryTestClient
	rules domain.MarketRules
	err   error
	calls int
}

func (c *isolatedRuleClient) MarketRules(context.Context, string) (domain.MarketRules, error) {
	c.calls++
	return c.rules, c.err
}

func TestSyncMarketRulesIsolatesInstrumentFailure(t *testing.T) {
	cfg := config.Config{
		RPC: config.RPCConfig{RequestTimeoutMS: 1000},
		Venues: map[string]config.VenueConfig{"exchange": {
			Enabled: true,
			Markets: map[string]config.VenueMarketConfig{
				"good": {Symbol: "GOODUSDT", BaseAsset: "GOOD", QuoteAsset: "USDT", PriceTick: num.Must("0.1"), QuantityStep: num.Must("1")},
				"bad":  {Symbol: "BADUSDT", BaseAsset: "BAD", QuoteAsset: "USDT", PriceTick: num.Must("0.1"), QuantityStep: num.Must("1")},
			},
		}},
	}
	clients := map[string]venue.Client{
		venue.ClientKey("exchange", "good"): &isolatedRuleClient{rules: domain.MarketRules{BaseAsset: "GOOD", QuoteAsset: "USDT", PriceTick: num.Must("0.001"), QuantityStep: num.Must("0.01")}},
		venue.ClientKey("exchange", "bad"):  &isolatedRuleClient{err: fmt.Errorf("rules endpoint down")},
	}

	failures := syncMarketRulesIsolated(context.Background(), &cfg, clients, nil)
	if len(failures) != 1 || len(failures["bad"]) != 1 {
		t.Fatalf("rule failure was not isolated: %+v", failures)
	}
	good := cfg.Venues["exchange"].Markets["good"]
	if good.PriceTick.Cmp(num.Must("0.001")) != 0 || good.QuantityStep.Cmp(num.Must("0.01")) != 0 {
		t.Fatalf("healthy market rules were not applied: %+v", good)
	}
	bad := cfg.Venues["exchange"].Markets["bad"]
	if bad.PriceTick.Cmp(num.Must("0.1")) != 0 || bad.QuantityStep.Cmp(num.Must("1")) != 0 {
		t.Fatalf("failed market rules changed unexpectedly: %+v", bad)
	}
}

func TestSyncMarketRulesReusesUnchangedMarketsWithoutFetching(t *testing.T) {
	// previous holds already-synced rules for an unchanged market and a market
	// whose symbol later changes.
	previous := config.Config{
		Venues: map[string]config.VenueConfig{"exchange": {
			Enabled: true, Type: "binance", BaseURL: "https://api",
			Markets: map[string]config.VenueMarketConfig{
				"keep":  {Symbol: "KEEPUSDT", BaseAsset: "KEEP", QuoteAsset: "USDT", PriceTick: num.Must("0.001"), QuantityStep: num.Must("0.01"), MaxOpenOrders: 200},
				"resym": {Symbol: "OLDUSDT", BaseAsset: "RES", QuoteAsset: "USDT", PriceTick: num.Must("0.5")},
			},
		}},
	}
	cfg := config.Config{
		RPC: config.RPCConfig{RequestTimeoutMS: 1000},
		Venues: map[string]config.VenueConfig{"exchange": {
			Enabled: true, Type: "binance", BaseURL: "https://api",
			Markets: map[string]config.VenueMarketConfig{
				// same symbol as previous, but a stale tick that must be overwritten by the reused synced value
				"keep": {Symbol: "KEEPUSDT", BaseAsset: "KEEP", QuoteAsset: "USDT", PriceTick: num.Must("9")},
				// symbol changed -> must re-fetch from the exchange
				"resym": {Symbol: "NEWUSDT", BaseAsset: "RES", QuoteAsset: "USDT", PriceTick: num.Must("0.5")},
			},
		}},
	}
	keepClient := &isolatedRuleClient{rules: domain.MarketRules{BaseAsset: "KEEP", QuoteAsset: "USDT", PriceTick: num.Must("0.001")}}
	resymClient := &isolatedRuleClient{rules: domain.MarketRules{BaseAsset: "RES", QuoteAsset: "USDT", PriceTick: num.Must("0.02"), QuantityStep: num.Must("0.1")}}
	clients := map[string]venue.Client{
		venue.ClientKey("exchange", "keep"):  keepClient,
		venue.ClientKey("exchange", "resym"): resymClient,
	}

	failures := syncMarketRulesIsolated(context.Background(), &cfg, clients, &previous)
	if len(failures) != 0 {
		t.Fatalf("unexpected failures: %+v", failures)
	}
	// Unchanged market: reused previous rules, no exchange call.
	if keepClient.calls != 0 {
		t.Fatalf("unchanged market should not hit the exchange, calls=%d", keepClient.calls)
	}
	keep := cfg.Venues["exchange"].Markets["keep"]
	if keep.PriceTick.Cmp(num.Must("0.001")) != 0 || keep.MaxOpenOrders != 200 {
		t.Fatalf("reused rules not carried forward: %+v", keep)
	}
	// Changed symbol: fetched fresh rules from the exchange.
	if resymClient.calls != 1 {
		t.Fatalf("changed market should re-fetch exactly once, calls=%d", resymClient.calls)
	}
	resym := cfg.Venues["exchange"].Markets["resym"]
	if resym.PriceTick.Cmp(num.Must("0.02")) != 0 {
		t.Fatalf("changed market did not pick up fresh rules: %+v", resym)
	}
}
