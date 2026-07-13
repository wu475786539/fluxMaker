package adminapi

import (
	"testing"

	"fluxmaker/internal/auth"
	"fluxmaker/internal/config"
	"fluxmaker/internal/domain"
	"fluxmaker/internal/oracle/pancakev2"
	"fluxmaker/internal/runtimeops"
	"fluxmaker/internal/tradesim"
)

func TestMergeScopedConfigPreservesHiddenPairsAndGlobalSettings(t *testing.T) {
	existing := config.Config{
		PollIntervalMS: 2000,
		Instruments: []config.InstrumentConfig{
			{ID: "allowed_pair"},
			{ID: "hidden_pair"},
		},
		Venues: map[string]config.VenueConfig{
			"binance": {
				Type:    "binance",
				Enabled: true,
				Markets: map[string]config.VenueMarketConfig{
					"allowed_pair": {Symbol: "OLD"},
					"hidden_pair":  {Symbol: "HIDDEN"},
				},
			},
		},
	}
	incoming := config.Config{
		PollIntervalMS: 9999,
		Instruments:    []config.InstrumentConfig{{ID: "allowed_pair", Base: config.AssetConfig{Symbol: "NEW"}}},
		Venues: map[string]config.VenueConfig{
			"binance": {
				Enabled: false,
				Markets: map[string]config.VenueMarketConfig{
					"allowed_pair": {Symbol: "NEW"},
				},
			},
		},
	}
	session := auth.Session{Instruments: []string{"allowed_pair"}}

	merged := mergeScopedConfig(existing, incoming, session)

	if merged.PollIntervalMS != 2000 {
		t.Fatalf("scoped edit changed global poll interval: %d", merged.PollIntervalMS)
	}
	if !merged.Venues["binance"].Enabled {
		t.Fatal("scoped edit changed global venue enabled state")
	}
	if merged.Venues["binance"].Markets["hidden_pair"].Symbol != "HIDDEN" {
		t.Fatal("hidden market mapping was not preserved")
	}
	if merged.Venues["binance"].Markets["allowed_pair"].Symbol != "NEW" {
		t.Fatal("allowed market mapping was not updated")
	}
	if len(merged.Instruments) != 2 || merged.Instruments[1].Base.Symbol != "NEW" {
		t.Fatalf("unexpected merged instruments: %#v", merged.Instruments)
	}
}

func TestOrientPairTokensUsesPathInputInsteadOfTokenOrdering(t *testing.T) {
	info := pancakev2.PairInfo{Token0: "0x1111111111111111111111111111111111111111", Token1: "0x2222222222222222222222222222222222222222"}
	base, quote, err := orientPairTokens(info, "0x2222222222222222222222222222222222222222")
	if err != nil || base != info.Token1 || quote != info.Token0 {
		t.Fatalf("base=%s quote=%s err=%v", base, quote, err)
	}
	if _, _, err := orientPairTokens(info, "0x3333333333333333333333333333333333333333"); err == nil {
		t.Fatal("expected a Pair that does not contain the path input to fail")
	}
}

func TestRedactRuntimeSnapshotUsesIndependentOrderAndFillPermissions(t *testing.T) {
	snapshot := runtimeops.InstrumentSnapshot{TradeSimulation: &tradesim.Snapshot{Fills: []domain.Fill{{TradeID: "sim"}}}, Venues: []runtimeops.VenueSnapshot{{
		OpenOrders: []domain.Order{{OrderID: "1"}},
		Fills:      []domain.Fill{{TradeID: "2"}},
	}}}

	redactRuntimeSnapshot(auth.Session{Permissions: []string{"runtime:view", "orders:view"}}, &snapshot)

	if len(snapshot.Venues[0].OpenOrders) != 1 {
		t.Fatal("orders were removed despite orders:view")
	}
	if len(snapshot.Venues[0].Fills) != 0 {
		t.Fatal("fills were exposed without fills:view")
	}
	if len(snapshot.TradeSimulation.Fills) != 0 {
		t.Fatal("simulated fills were exposed without fills:view")
	}
}

func TestRuntimeControlStateWaitsForEngineAcknowledgement(t *testing.T) {
	pause := runtimeops.PauseState{InstrumentID: "token_usdt", Paused: true, Reason: "emergency_cancel"}
	snapshot := runtimeops.InstrumentSnapshot{InstrumentID: "token_usdt", Status: "running"}

	mergeRuntimeControlState(&snapshot, pause, true)
	if snapshot.Paused || snapshot.Status != "pausing" || snapshot.Pause == nil {
		t.Fatalf("request was incorrectly reported as applied: %+v", snapshot)
	}

	snapshot.Paused = true
	mergeRuntimeControlState(&snapshot, pause, true)
	if snapshot.Status != "paused" {
		t.Fatalf("engine acknowledgement was not reported: %+v", snapshot)
	}

	mergeRuntimeControlState(&snapshot, runtimeops.PauseState{}, false)
	if snapshot.Status != "resuming" || snapshot.Pause != nil {
		t.Fatalf("resume request was not represented as pending: %+v", snapshot)
	}
}
