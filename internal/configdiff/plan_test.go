package configdiff

import (
	"testing"

	"fluxmaker/internal/config"
	"fluxmaker/internal/domain"
	"fluxmaker/internal/num"
)

func testConfig() config.Config {
	return config.Config{
		Mode:           domain.ModeLive,
		PollIntervalMS: 1000,
		RPC:            config.RPCConfig{URLs: []string{"https://rpc-a"}, ChainID: 56, RequestTimeoutMS: 1000},
		Instruments: []config.InstrumentConfig{
			{ID: "a", Strategy: config.StrategyConfig{OrderSize: num.Must("1")}},
			{ID: "b", Strategy: config.StrategyConfig{OrderSize: num.Must("1")}},
		},
		Venues: map[string]config.VenueConfig{"binance": {Type: "binance", Enabled: true, TradingEnabled: true, BaseURL: "https://api", Markets: map[string]config.VenueMarketConfig{
			"a": {Symbol: "AUSDT", CredentialID: 1}, "b": {Symbol: "BUSDT", CredentialID: 2},
		}}},
	}
}

func TestStrategyChangeDoesNotRequestGlobalOrMarketCancellation(t *testing.T) {
	old := testConfig()
	next := testConfig()
	next.Instruments[0].Strategy.OrderSize = num.Must("2")
	plan := Build(&old, next)
	if plan.CancelAll || len(plan.CancelTargets) != 0 {
		t.Fatalf("strategy change should reconcile without forced cancellation: %+v", plan)
	}
	if plan.AffectedInstruments != 1 || plan.UnchangedInstruments != 1 || plan.InstrumentChanges[0].InstrumentID != "a" {
		t.Fatalf("unexpected scope: %+v", plan)
	}
}

func TestStructuralClassification(t *testing.T) {
	strategyOnly := testConfig()
	strategyOnly.Instruments[0].Strategy.OrderSize = num.Must("2")

	timingOnly := testConfig()
	timingOnly.PollIntervalMS = 2500
	timingOnly.MaxConcurrentInstruments = 8

	rpcChange := testConfig()
	rpcChange.RPC.URLs = []string{"https://rpc-b"}

	modeChange := testConfig()
	modeChange.Mode = domain.ModeShadow

	symbolChange := testConfig()
	venue := symbolChange.Venues["binance"]
	market := venue.Markets["a"]
	market.Symbol = "AXUSDT"
	venue.Markets["a"] = market
	symbolChange.Venues["binance"] = venue

	removed := testConfig()
	removed.Instruments = removed.Instruments[:1]

	cases := []struct {
		name       string
		next       config.Config
		structural bool
	}{
		{"strategy-param only", strategyOnly, false},
		{"runtime timing only", timingOnly, false},
		{"rpc change", rpcChange, true},
		{"mode change", modeChange, true},
		{"market symbol change", symbolChange, true},
		{"instrument removed", removed, true},
	}
	for _, tc := range cases {
		old := testConfig()
		plan := Build(&old, tc.next)
		if plan.Structural != tc.structural {
			t.Errorf("%s: Structural=%v want %v (plan=%+v)", tc.name, plan.Structural, tc.structural, plan)
		}
	}

	if !Build(nil, testConfig()).Structural {
		t.Error("first publish must be structural")
	}
}

func TestRPCChangeIsHotAndDoesNotCancelOrders(t *testing.T) {
	old := testConfig()
	next := testConfig()
	next.RPC.URLs = []string{"https://rpc-b"}
	plan := Build(&old, next)
	if plan.CancelAll || len(plan.CancelTargets) != 0 || len(plan.HotChanges) == 0 {
		t.Fatalf("RPC should be prepared and hot-swapped: %+v", plan)
	}
}

func TestRuntimeTimingChangeLeavesEveryInstrumentRunning(t *testing.T) {
	old := testConfig()
	next := testConfig()
	next.PollIntervalMS = 2500
	next.MaxConcurrentInstruments = 8
	plan := Build(&old, next)
	if plan.CancelAll || len(plan.CancelTargets) != 0 || plan.AffectedInstruments != 0 || plan.UnchangedInstruments != 2 {
		t.Fatalf("runtime timing should be a non-disruptive hot update: %+v", plan)
	}
}

func TestRemovingOneMarketCancelsOnlyThatMarket(t *testing.T) {
	old := testConfig()
	next := testConfig()
	venueCfg := next.Venues["binance"]
	delete(venueCfg.Markets, "a")
	next.Venues["binance"] = venueCfg
	plan := Build(&old, next)
	if plan.CancelAll || len(plan.CancelTargets) != 1 || plan.CancelTargets[0].InstrumentID != "a" {
		t.Fatalf("unexpected cancellation scope: %+v", plan)
	}
}

func TestRemovingInstrumentStillCountsRemainingInstrumentAsUnchanged(t *testing.T) {
	old := testConfig()
	next := testConfig()
	next.Instruments = next.Instruments[1:]
	venueCfg := next.Venues["binance"]
	delete(venueCfg.Markets, "a")
	next.Venues["binance"] = venueCfg
	plan := Build(&old, next)
	if plan.UnchangedInstruments != 1 {
		t.Fatalf("remaining instrument should stay running: %+v", plan)
	}
}

func TestLiveToShadowIsExplicitGlobalCancellation(t *testing.T) {
	old := testConfig()
	next := testConfig()
	next.Mode = domain.ModeShadow
	plan := Build(&old, next)
	if !plan.CancelAll {
		t.Fatalf("live to shadow must cancel all: %+v", plan)
	}
}
