package engine

import (
	"log/slog"
	"testing"

	"fluxmaker/internal/audit"
	"fluxmaker/internal/config"
	"fluxmaker/internal/num"
)

func TestApplyParametersHotSwapsStrategyAndPreservesSyncedRules(t *testing.T) {
	cfg := config.Config{
		PollIntervalMS:           1000,
		MaxConcurrentInstruments: 4,
		Instruments: []config.InstrumentConfig{
			{ID: "a", Strategy: config.StrategyConfig{OrderSize: num.Must("1"), HalfSpreadBPS: 50, Levels: 3}},
		},
		Venues: map[string]config.VenueConfig{
			"binance": {Type: "binance", Enabled: true, Markets: map[string]config.VenueMarketConfig{
				// PriceTick here stands in for an exchange-synced rule learned at runtime.
				"a": {Symbol: "AUSDT", PriceTick: num.Must("0.01")},
			}},
		},
	}
	eng := New(cfg, nil, nil, audit.New(""), nil, slog.Default())

	next := config.Config{
		PollIntervalMS:           2000,
		MaxConcurrentInstruments: 8,
		Instruments: []config.InstrumentConfig{
			{ID: "a", Strategy: config.StrategyConfig{OrderSize: num.Must("5"), HalfSpreadBPS: 80, Levels: 6}},
		},
		Venues: map[string]config.VenueConfig{
			"binance": {Type: "binance", Enabled: true, Markets: map[string]config.VenueMarketConfig{
				// A stale pre-sync PriceTick that must NOT clobber the synced value.
				"a": {Symbol: "AUSDT", PriceTick: num.Must("999")},
			}},
		},
	}

	eng.ApplyParameters(next)
	got := eng.EffectiveConfig()

	strat := got.Instruments[0].Strategy
	if strat.OrderSize.Cmp(num.Must("5")) != 0 || strat.HalfSpreadBPS != 80 || strat.Levels != 6 {
		t.Fatalf("strategy not hot-swapped: %+v", strat)
	}
	if strat.MaxVenueReferenceDeviationBPS != 500 || strat.MaxVenueSpreadBPS != 1000 {
		t.Fatalf("strategy safety defaults not applied: %+v", strat)
	}
	if got.PollIntervalMS != 2000 || got.MaxConcurrentInstruments != 8 {
		t.Fatalf("scalar knobs not applied: poll=%d conc=%d", got.PollIntervalMS, got.MaxConcurrentInstruments)
	}
	if tick := got.Venues["binance"].Markets["a"].PriceTick; tick.Cmp(num.Must("0.01")) != 0 {
		t.Fatalf("exchange-synced PriceTick was overwritten: %s", tick.String())
	}
}

func TestApplyParametersLeavesZeroScalarsUntouched(t *testing.T) {
	cfg := config.Config{
		MaxConcurrentInstruments: 4,
		Instruments:              []config.InstrumentConfig{{ID: "a", Strategy: config.StrategyConfig{OrderSize: num.Must("1")}}},
	}
	eng := New(cfg, nil, nil, audit.New(""), nil, slog.Default())
	// next leaves MaxConcurrentInstruments at 0 (unset); the running default must hold.
	eng.ApplyParameters(config.Config{Instruments: []config.InstrumentConfig{{ID: "a", Strategy: config.StrategyConfig{OrderSize: num.Must("2")}}}})
	if got := eng.EffectiveConfig().MaxConcurrentInstruments; got != 4 {
		t.Fatalf("zero scalar overwrote running value: got %d want 4", got)
	}
}
