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
}

func (c *isolatedRuleClient) MarketRules(context.Context, string) (domain.MarketRules, error) {
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

	failures := syncMarketRulesIsolated(context.Background(), &cfg, clients)
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
