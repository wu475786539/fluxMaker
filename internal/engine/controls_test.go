package engine

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"fluxmaker/internal/audit"
	"fluxmaker/internal/config"
	"fluxmaker/internal/domain"
	"fluxmaker/internal/fault"
	"fluxmaker/internal/num"
	"fluxmaker/internal/runtimeops"
	"fluxmaker/internal/venue"
)

type controlStore struct {
	paused    map[string]runtimeops.PauseState
	published []runtimeops.InstrumentSnapshot
}

type ruleControlStore struct {
	controlStore
	changes []runtimeops.RuleChange
}

type faultControlStore struct {
	controlStore
	mu     sync.Mutex
	states map[string][]byte
}

func (s *faultControlStore) LoadFaultState(_ context.Context, key string) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]byte(nil), s.states[key]...), nil
}

func (s *faultControlStore) SaveFaultState(_ context.Context, key string, payload []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.states == nil {
		s.states = make(map[string][]byte)
	}
	s.states[key] = append([]byte(nil), payload...)
	return nil
}

func (s *faultControlStore) DeleteFaultState(_ context.Context, key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.states, key)
	return nil
}

func (s *ruleControlStore) ReportRuleChange(_ context.Context, change runtimeops.RuleChange) error {
	s.changes = append(s.changes, change)
	return nil
}

type leaseControlStore struct {
	controlStore
	owners      map[string]string
	held        map[string]uint64
	generations map[string]uint64
}

func (s *leaseControlStore) AcquireMarketLease(_ context.Context, key, owner string, _ time.Duration) (uint64, error) {
	if s.owners == nil {
		s.owners = make(map[string]string)
		s.held = make(map[string]uint64)
		s.generations = make(map[string]uint64)
	}
	current := s.owners[key]
	if current != "" && current != owner {
		return 0, nil
	}
	if current == owner {
		return s.held[key], nil
	}
	s.generations[key]++
	s.owners[key] = owner
	s.held[key] = s.generations[key]
	return s.held[key], nil
}

func (s *leaseControlStore) ValidateMarketLease(_ context.Context, key, owner string, generation uint64) (bool, error) {
	return s.owners[key] == owner && s.held[key] == generation, nil
}

func (s *leaseControlStore) ReleaseMarketLease(_ context.Context, key, owner string, generation uint64) error {
	if s.owners[key] == owner && s.held[key] == generation {
		delete(s.owners, key)
		delete(s.held, key)
	}
	return nil
}

func (s *controlStore) Publish(_ context.Context, snapshot runtimeops.InstrumentSnapshot) error {
	s.published = append(s.published, snapshot)
	return nil
}

func (s *controlStore) Get(context.Context, string) (runtimeops.InstrumentSnapshot, error) {
	return runtimeops.InstrumentSnapshot{}, nil
}

func (s *controlStore) Paused(context.Context) (map[string]runtimeops.PauseState, error) {
	return s.paused, nil
}

type controlVenue struct {
	orders         []domain.Order
	balances       []domain.Balance
	canceled       []string
	placed         int
	openOrderCalls int
	retainCanceled bool
}

type ruleControlVenue struct {
	controlVenue
	rules domain.MarketRules
}

func (v *ruleControlVenue) MarketRules(context.Context, string) (domain.MarketRules, error) {
	return v.rules, nil
}

func (v *controlVenue) Name() string { return "binance" }
func (v *controlVenue) TopBook(context.Context, string) (domain.Book, error) {
	return domain.Book{BidPrice: num.Must("0.9"), AskPrice: num.Must("1.1"), Timestamp: time.Now()}, nil
}
func (v *controlVenue) Balances(context.Context) ([]domain.Balance, error) {
	return append([]domain.Balance(nil), v.balances...), nil
}
func (v *controlVenue) OpenOrders(context.Context, string) ([]domain.Order, error) {
	v.openOrderCalls++
	return append([]domain.Order(nil), v.orders...), nil
}
func (v *controlVenue) PlacePostOnly(context.Context, venue.PlaceRequest) (domain.Order, error) {
	v.placed++
	return domain.Order{}, nil
}
func (v *controlVenue) CancelOrder(_ context.Context, _ string, orderID string) error {
	v.canceled = append(v.canceled, orderID)
	if v.retainCanceled {
		return nil
	}
	for index, order := range v.orders {
		if order.OrderID == orderID {
			v.orders = append(v.orders[:index], v.orders[index+1:]...)
			break
		}
	}
	return nil
}

type prepareOracle struct{}

func (prepareOracle) Price(context.Context, config.InstrumentConfig) (domain.ReferencePrice, error) {
	return domain.ReferencePrice{Price: num.Must("1")}, nil
}

type isolationOracle struct {
	mu       sync.RWMutex
	failures map[string]error
}

func (o *isolationOracle) Price(_ context.Context, instrument config.InstrumentConfig) (domain.ReferencePrice, error) {
	o.mu.RLock()
	err := o.failures[instrument.ID]
	o.mu.RUnlock()
	if err != nil {
		return domain.ReferencePrice{}, err
	}
	return domain.ReferencePrice{InstrumentID: instrument.ID, Price: num.Must("1"), ValidUntil: time.Now().Add(time.Minute)}, nil
}

func (o *isolationOracle) recover(instrumentID string) {
	o.mu.Lock()
	delete(o.failures, instrumentID)
	o.mu.Unlock()
}

func TestPrepareHasNoOrderSideEffects(t *testing.T) {
	client := &controlVenue{orders: []domain.Order{{OrderID: "managed", ClientID: "fm-123", Symbol: "TOKENUSDT"}}}
	cfg := config.Config{
		Mode:        domain.ModeShadow,
		Instruments: []config.InstrumentConfig{{ID: "token_usdt", Strategy: config.StrategyConfig{HalfSpreadBPS: 50, Levels: 1, OrderSize: num.Must("10")}}},
		Venues: map[string]config.VenueConfig{"binance": {
			Type: "binance", Enabled: true, Markets: map[string]config.VenueMarketConfig{"token_usdt": {Symbol: "TOKENUSDT", PriceTick: num.Must("0.01"), QuantityStep: num.Must("1"), MinNotional: num.Must("1")}},
		}},
	}
	engine := New(cfg, prepareOracle{}, map[string]venue.Client{venue.ClientKey("binance", "token_usdt"): client}, audit.New(""), nil, slog.Default())

	if err := engine.Prepare(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(client.canceled) != 0 || client.placed != 0 {
		t.Fatalf("candidate preflight changed orders: canceled=%v placed=%d", client.canceled, client.placed)
	}
}

func TestPrepareAndRunOnceIsolateBlockedInstrument(t *testing.T) {
	store := &controlStore{}
	oracle := &isolationOracle{failures: map[string]error{"gdt_usdt": errors.New("pancake pair stale")}}
	goodClient := &ruleControlVenue{rules: domain.MarketRules{BaseAsset: "BNB", QuoteAsset: "USDT", PriceTick: num.Must("0.01"), QuantityStep: num.Must("0.1"), MinNotional: num.Must("1")}}
	badClient := &ruleControlVenue{rules: domain.MarketRules{BaseAsset: "GDT", QuoteAsset: "USDT", PriceTick: num.Must("0.000001"), QuantityStep: num.Must("0.01"), MinNotional: num.Must("5")}}
	cfg := config.Config{
		Mode:                     domain.ModeShadow,
		MaxConcurrentInstruments: 1,
		Instruments: []config.InstrumentConfig{
			{ID: "bnb_usdt", Base: config.AssetConfig{Symbol: "BNB"}, Quote: config.AssetConfig{Symbol: "USDT"}, Strategy: config.StrategyConfig{HalfSpreadBPS: 50, Levels: 1, OrderSize: num.Must("10")}},
			{ID: "gdt_usdt", Base: config.AssetConfig{Symbol: "GDT"}, Quote: config.AssetConfig{Symbol: "USDT"}, Strategy: config.StrategyConfig{HalfSpreadBPS: 50, Levels: 1, OrderSize: num.Must("10")}},
		},
		Venues: map[string]config.VenueConfig{
			"binance": {Type: "binance", Enabled: true, Markets: map[string]config.VenueMarketConfig{"bnb_usdt": {Symbol: "BNBUSDT", BaseAsset: "BNB", QuoteAsset: "USDT", PriceTick: num.Must("0.01"), QuantityStep: num.Must("0.1"), MinNotional: num.Must("1")}}},
			"mgbx":    {Type: "mgbx", Enabled: true, Markets: map[string]config.VenueMarketConfig{"gdt_usdt": {Symbol: "GDT_USDT", BaseAsset: "GDT", QuoteAsset: "USDT", PriceTick: num.Must("0.000001"), QuantityStep: num.Must("0.01"), MinNotional: num.Must("5")}}},
		},
	}
	e := New(cfg, oracle, map[string]venue.Client{
		venue.ClientKey("binance", "bnb_usdt"): goodClient,
		venue.ClientKey("mgbx", "gdt_usdt"):    badClient,
	}, audit.New(""), store, slog.Default())

	if err := e.Prepare(context.Background()); err != nil {
		t.Fatalf("healthy peer should allow candidate activation: %v", err)
	}
	blocked := e.BlockedInstruments()
	if len(blocked) != 1 || blocked["gdt_usdt"] == "" {
		t.Fatalf("unexpected blocked instruments: %+v", blocked)
	}
	if err := e.RunOnce(context.Background()); err == nil {
		t.Fatal("cycle should report the degraded instrument")
	}
	statuses := map[string]string{}
	for _, snapshot := range store.published {
		statuses[snapshot.InstrumentID] = snapshot.Status
	}
	if statuses["bnb_usdt"] != "running" || statuses["gdt_usdt"] != "degraded" {
		t.Fatalf("healthy and degraded states were not isolated: %+v", statuses)
	}

	oracle.recover("gdt_usdt")
	recovered, err := e.RetryBlocked(context.Background())
	if err != nil || recovered != 1 {
		t.Fatalf("blocked instrument did not recover independently: recovered=%d err=%v", recovered, err)
	}
	if len(e.BlockedInstruments()) != 0 {
		t.Fatalf("recovered instrument remains blocked: %+v", e.BlockedInstruments())
	}
}

func TestPrepareRejectsCandidateWhenEveryInstrumentIsBlocked(t *testing.T) {
	oracle := &isolationOracle{failures: map[string]error{"gdt_usdt": errors.New("pancake pair stale")}}
	cfg := config.Config{Instruments: []config.InstrumentConfig{{ID: "gdt_usdt"}}}
	e := New(cfg, oracle, nil, audit.New(""), nil, slog.Default())
	if err := e.Prepare(context.Background()); err == nil || !strings.Contains(err.Error(), "no runnable instruments") {
		t.Fatalf("expected whole-candidate rejection, got %v", err)
	}
}

func TestCancelMarketSkipsInstrumentThatNeverPassedStartupPreflight(t *testing.T) {
	blockedClient := &controlVenue{}
	healthyClient := &controlVenue{}
	cfg := config.Config{
		Mode: domain.ModeShadow,
		Instruments: []config.InstrumentConfig{
			{ID: "healthy", Strategy: config.StrategyConfig{HalfSpreadBPS: 50, Levels: 1, OrderSize: num.Must("10")}},
			{ID: "blocked", Strategy: config.StrategyConfig{HalfSpreadBPS: 50, Levels: 1, OrderSize: num.Must("10")}},
		},
		Venues: map[string]config.VenueConfig{"mgbx": {
			Type: "mgbx", Enabled: true, TradingEnabled: true,
			Markets: map[string]config.VenueMarketConfig{
				"healthy": {Symbol: "HEALTHY_USDT", PriceTick: num.Must("0.01"), QuantityStep: num.Must("1"), MinNotional: num.Must("1")},
				"blocked": {Symbol: "INVALID", PriceTick: num.Must("0.01"), QuantityStep: num.Must("1"), MinNotional: num.Must("1")},
			},
		}},
	}
	e := New(cfg, prepareOracle{}, map[string]venue.Client{
		venue.ClientKey("mgbx", "healthy"): healthyClient,
		venue.ClientKey("mgbx", "blocked"): blockedClient,
	}, audit.New(""), nil, slog.Default())
	e.SetStartupFailures(map[string]string{"blocked": "symbol not found"})
	if err := e.Prepare(context.Background()); err != nil {
		t.Fatalf("healthy peer should allow candidate activation: %v", err)
	}

	if err := e.CancelMarket(context.Background(), "blocked", "mgbx"); err != nil {
		t.Fatalf("startup-blocked cleanup should be a no-op: %v", err)
	}
	if blockedClient.openOrderCalls != 0 {
		t.Fatalf("startup-blocked market was queried during cleanup: calls=%d", blockedClient.openOrderCalls)
	}
	if err := e.CancelMarket(context.Background(), "healthy", "mgbx"); err != nil {
		t.Fatalf("healthy cleanup failed: %v", err)
	}
	if healthyClient.openOrderCalls != 1 {
		t.Fatalf("healthy market cleanup was skipped: calls=%d", healthyClient.openOrderCalls)
	}
}

func TestCancelMarketOnlyTouchesRequestedVenueMarket(t *testing.T) {
	clientA := &controlVenue{orders: []domain.Order{{OrderID: "a", ClientID: "fm-a", Symbol: "AUSDT"}}}
	clientB := &controlVenue{orders: []domain.Order{{OrderID: "b", ClientID: "fm-b", Symbol: "BUSDT"}}}
	cfg := config.Config{
		Mode: domain.ModeLive,
		Venues: map[string]config.VenueConfig{"binance": {
			Type: "binance", Enabled: true, TradingEnabled: true, Markets: map[string]config.VenueMarketConfig{
				"a": {Symbol: "AUSDT"}, "b": {Symbol: "BUSDT"},
			},
		}},
	}
	engine := New(cfg, prepareOracle{}, map[string]venue.Client{
		venue.ClientKey("binance", "a"): clientA,
		venue.ClientKey("binance", "b"): clientB,
	}, audit.New(""), nil, slog.Default())

	if err := engine.CancelMarket(context.Background(), "a", "binance"); err != nil {
		t.Fatal(err)
	}
	if len(clientA.canceled) != 1 || len(clientB.canceled) != 0 {
		t.Fatalf("unexpected cancellation scope: a=%v b=%v", clientA.canceled, clientB.canceled)
	}
}

func TestMarketLeasePreventsSecondEngineOwner(t *testing.T) {
	store := &leaseControlStore{}
	first := NewWithOwner(config.Config{}, nil, nil, audit.New(""), store, slog.Default(), "owner-a")
	second := NewWithOwner(config.Config{}, nil, nil, audit.New(""), store, slog.Default(), "owner-b")
	firstGeneration, err := first.acquireMarketLease(context.Background(), "binance/1/token_usdt")
	if err != nil || firstGeneration == 0 {
		t.Fatalf("first acquire generation=%v err=%v", firstGeneration, err)
	}
	if generation, err := second.acquireMarketLease(context.Background(), "binance/1/token_usdt"); err != nil || generation != 0 {
		t.Fatalf("second acquire generation=%v err=%v", generation, err)
	}
	if err := first.releaseMarketLease(context.Background(), "binance/1/token_usdt", firstGeneration); err != nil {
		t.Fatal(err)
	}
	secondGeneration, err := second.acquireMarketLease(context.Background(), "binance/1/token_usdt")
	if err != nil || secondGeneration <= firstGeneration {
		t.Fatalf("second acquire after release generation=%v first=%v err=%v", secondGeneration, firstGeneration, err)
	}
	if valid, err := store.ValidateMarketLease(context.Background(), "binance/1/token_usdt", "owner-a", firstGeneration); err != nil || valid {
		t.Fatalf("stale generation valid=%v err=%v", valid, err)
	}
}

func TestRefreshMarketRulesAppliesOnlyChangedMarketAndReportsAlert(t *testing.T) {
	store := &ruleControlStore{}
	client := &ruleControlVenue{rules: domain.MarketRules{Symbol: "TOKENUSDT", BaseAsset: "TOKEN", QuoteAsset: "USDT", PriceTick: num.Must("0.001"), QuantityStep: num.Must("0.1"), MinNotional: num.Must("10"), MaxOpenOrders: 200}}
	cfg := config.Config{Venues: map[string]config.VenueConfig{"binance": {Type: "binance", Enabled: true, Markets: map[string]config.VenueMarketConfig{"token_usdt": {Symbol: "TOKENUSDT", BaseAsset: "TOKEN", QuoteAsset: "USDT", PriceTick: num.Must("0.01"), QuantityStep: num.Must("1"), MinNotional: num.Must("5"), MaxOpenOrders: 100}}}}}
	e := New(cfg, nil, map[string]venue.Client{venue.ClientKey("binance", "token_usdt"): client}, audit.New(""), store, slog.Default())

	changes, err := e.RefreshMarketRules(context.Background())
	if err != nil || changes != 1 {
		t.Fatalf("changes=%d err=%v", changes, err)
	}
	market := e.cfg.Venues["binance"].Markets["token_usdt"]
	if market.PriceTick.Cmp(num.Must("0.001")) != 0 || market.QuantityStep.Cmp(num.Must("0.1")) != 0 || market.MinNotional.Cmp(num.Must("10")) != 0 || market.MaxOpenOrders != 200 {
		t.Fatalf("rules not applied: %+v", market)
	}
	if len(store.changes) != 1 || store.changes[0].Previous.PriceTick.Cmp(num.Must("0.01")) != 0 || store.changes[0].Current.PriceTick.Cmp(num.Must("0.001")) != 0 {
		t.Fatalf("unexpected rule alerts: %+v", store.changes)
	}

	changes, err = e.RefreshMarketRules(context.Background())
	if err != nil || changes != 0 || len(store.changes) != 1 {
		t.Fatalf("unchanged refresh changes=%d alerts=%d err=%v", changes, len(store.changes), err)
	}
}

func TestVenueFailureGraceCancelsOnlyAtThreshold(t *testing.T) {
	client := &controlVenue{orders: []domain.Order{{OrderID: "managed", ClientID: "fm-1", Symbol: "TOKENUSDT"}}}
	cfg := config.Config{Mode: domain.ModeLive, MarketFailureThreshold: 3, MarketRecoveryThreshold: 3}
	e := New(cfg, nil, nil, audit.New(""), nil, slog.Default())
	venueCfg := config.VenueConfig{Type: "binance", TradingEnabled: true}
	market := config.VenueMarketConfig{Symbol: "TOKENUSDT"}
	for attempt := 1; attempt <= 2; attempt++ {
		state, err := e.markVenueFailure(context.Background(), "token_usdt", "binance", venueCfg, market, client, "book", fmt.Errorf("timeout"), false)
		if err != nil || state.Status != "degraded" || len(client.canceled) != 0 {
			t.Fatalf("attempt %d state=%+v canceled=%v err=%v", attempt, state, client.canceled, err)
		}
	}
	state, err := e.markVenueFailure(context.Background(), "token_usdt", "binance", venueCfg, market, client, "book", fmt.Errorf("timeout"), false)
	if err != nil || state.Status != "canceling" || len(client.canceled) != 1 {
		t.Fatalf("third state=%+v canceled=%v err=%v", state, client.canceled, err)
	}
}

func TestShadowHealthyCyclesClearPersistedFaultState(t *testing.T) {
	store := &faultControlStore{states: make(map[string][]byte)}
	marketKey := "binance/0/token_usdt/tokenusdt"
	seed := fault.NewWithStateStore(3, 3, store)
	for attempt := 0; attempt < 3; attempt++ {
		if _, err := seed.FailureWithContext(context.Background(), marketKey, "book", fmt.Errorf("timeout"), false); err != nil {
			t.Fatal(err)
		}
	}

	client := &controlVenue{}
	cfg := config.Config{
		Mode:                     domain.ModeShadow,
		MarketFailureThreshold:   3,
		MarketRecoveryThreshold:  3,
		MaxConcurrentInstruments: 1,
		Instruments: []config.InstrumentConfig{{
			ID:       "token_usdt",
			Strategy: config.StrategyConfig{HalfSpreadBPS: 50, Levels: 1, OrderSize: num.Must("10")},
		}},
		Venues: map[string]config.VenueConfig{"binance": {
			Type: "binance", Enabled: true, Markets: map[string]config.VenueMarketConfig{"token_usdt": {
				Symbol: "TOKENUSDT", PriceTick: num.Must("0.01"), QuantityStep: num.Must("1"), MinNotional: num.Must("1"),
			}},
		}},
	}
	e := New(cfg, prepareOracle{}, map[string]venue.Client{venue.ClientKey("binance", "token_usdt"): client}, audit.New(""), store, slog.Default())

	for cycle := 1; cycle <= 4; cycle++ {
		err := e.RunOnce(context.Background())
		if cycle < 4 && err == nil {
			t.Fatalf("cycle %d reported healthy before recovery threshold", cycle)
		}
		if cycle == 4 && err != nil {
			t.Fatalf("final recovery cycle failed: %v", err)
		}
	}
	payload, err := store.LoadFaultState(context.Background(), marketKey)
	if err != nil {
		t.Fatal(err)
	}
	if len(payload) != 0 {
		t.Fatalf("Shadow recovery retained persisted fault state: %s", payload)
	}
	latest := store.published[len(store.published)-1]
	if latest.Status != "running" || len(latest.Venues) != 1 || latest.Venues[0].Fault == nil || latest.Venues[0].Fault.Status != fault.Normal {
		t.Fatalf("unexpected recovered snapshot: %+v", latest)
	}
}

func TestPauseCancelsManagedOrdersOnceAndResumeClearsLocalState(t *testing.T) {
	store := &controlStore{paused: map[string]runtimeops.PauseState{
		"token_usdt": {InstrumentID: "token_usdt", Paused: true, Reason: "emergency_cancel", RequestedBy: 7},
	}}
	client := &controlVenue{orders: []domain.Order{
		{OrderID: "managed", ClientID: "fm-123", Symbol: "TOKENUSDT"},
		{OrderID: "manual", ClientID: "manual-123", Symbol: "TOKENUSDT"},
	}}
	cfg := config.Config{
		Mode: domain.ModeLive,
		Instruments: []config.InstrumentConfig{{
			ID:       "token_usdt",
			Base:     config.AssetConfig{Symbol: "TOKEN"},
			Quote:    config.AssetConfig{Symbol: "USDT"},
			Strategy: config.StrategyConfig{TargetBase: num.Must("100"), MaxBaseDeviation: num.Must("20")},
		}},
		Venues: map[string]config.VenueConfig{
			"binance-main": {Type: "binance", Enabled: true, TradingEnabled: true, Markets: map[string]config.VenueMarketConfig{"token_usdt": {Symbol: "TOKENUSDT"}}},
		},
	}
	engine := New(cfg, nil, map[string]venue.Client{venue.ClientKey("binance-main", "token_usdt"): client}, audit.New(""), store, slog.Default())

	if err := engine.ApplyControls(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(client.canceled) != 1 || client.canceled[0] != "managed" {
		t.Fatalf("canceled=%v, want only the managed order", client.canceled)
	}
	if len(store.published) != 1 || !store.published[0].Paused || store.published[0].Status != "paused" {
		t.Fatalf("published=%+v", store.published)
	}
	if err := engine.ApplyControls(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(client.canceled) != 1 {
		t.Fatalf("pause was applied twice: %v", client.canceled)
	}

	store.paused = map[string]runtimeops.PauseState{}
	if err := engine.ApplyControls(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, paused := engine.paused["token_usdt"]; paused {
		t.Fatal("resume did not clear the local pause state")
	}
}

func TestPauseWaitsForExchangeCancellationConfirmation(t *testing.T) {
	store := &controlStore{paused: map[string]runtimeops.PauseState{"token_usdt": {InstrumentID: "token_usdt", Paused: true, Reason: "manual_pause"}}}
	client := &controlVenue{retainCanceled: true, orders: []domain.Order{{OrderID: "managed", ClientID: "fm-123", Symbol: "TOKENUSDT"}}}
	cfg := config.Config{Mode: domain.ModeLive, Instruments: []config.InstrumentConfig{{ID: "token_usdt"}}, Venues: map[string]config.VenueConfig{"binance": {Type: "binance", Enabled: true, TradingEnabled: true, Markets: map[string]config.VenueMarketConfig{"token_usdt": {Symbol: "TOKENUSDT"}}}}}
	e := New(cfg, nil, map[string]venue.Client{venue.ClientKey("binance", "token_usdt"): client}, audit.New(""), store, slog.Default())

	if err := e.ApplyControls(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(store.published) != 1 || store.published[0].Paused || store.published[0].Status != "pausing" || len(store.published[0].Venues) != 0 {
		t.Fatalf("pause was acknowledged before exchange confirmation: %+v", store.published)
	}
	client.orders = nil
	if err := e.ApplyControls(context.Background()); err != nil {
		t.Fatal(err)
	}
	latest := store.published[len(store.published)-1]
	if !latest.Paused || latest.Status != "paused" {
		t.Fatalf("confirmed cancellation did not acknowledge pause: %+v", latest)
	}
}

func TestPausedInstrumentContinuesReadOnlyRuntimeRefresh(t *testing.T) {
	store := &controlStore{paused: map[string]runtimeops.PauseState{"token_usdt": {InstrumentID: "token_usdt", Paused: true, Reason: "manual_pause"}}}
	client := &controlVenue{balances: []domain.Balance{{Asset: "TOKEN", Free: num.Must("3")}, {Asset: "USDT", Free: num.Must("100")}}}
	cfg := config.Config{
		Mode:                     domain.ModeLive,
		MaxConcurrentInstruments: 1,
		Instruments: []config.InstrumentConfig{{
			ID:       "token_usdt",
			Base:     config.AssetConfig{Symbol: "TOKEN"},
			Quote:    config.AssetConfig{Symbol: "USDT"},
			Strategy: config.StrategyConfig{TargetBase: num.Must("2"), MaxBaseDeviation: num.Must("1")},
		}},
		Venues: map[string]config.VenueConfig{"binance": {
			Type: "binance", Enabled: true, TradingEnabled: true,
			Markets: map[string]config.VenueMarketConfig{"token_usdt": {Symbol: "TOKENUSDT", BaseAsset: "TOKEN", QuoteAsset: "USDT", CredentialID: 1, PriceTick: num.Must("0.01"), QuantityStep: num.Must("1")}},
		}},
	}
	e := New(cfg, prepareOracle{}, map[string]venue.Client{venue.ClientKey("binance", "token_usdt"): client}, audit.New(""), store, slog.Default())

	if err := e.ApplyControls(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := e.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	latest := store.published[len(store.published)-1]
	if !latest.Paused || latest.Status != "paused" || !latest.InventoryAvailable || latest.Inventory.Cmp(num.Must("3")) != 0 {
		t.Fatalf("paused snapshot=%+v", latest)
	}
	if len(latest.Venues) != 1 || !latest.Venues[0].MarketConnected || !latest.Venues[0].AccountConnected || latest.Venues[0].BaseBalance == nil || latest.Venues[0].BaseBalance.Free.Cmp(num.Must("3")) != 0 {
		t.Fatalf("paused venue snapshot=%+v", latest.Venues)
	}
	if client.placed != 0 || len(client.canceled) != 0 {
		t.Fatalf("read-only refresh mutated orders: placed=%d canceled=%v", client.placed, client.canceled)
	}
}
