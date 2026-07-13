package engine

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"fluxmaker/internal/audit"
	"fluxmaker/internal/config"
	"fluxmaker/internal/domain"
	"fluxmaker/internal/num"
)

type gatedOracle struct {
	entered chan string
	release chan struct{}
}

func (o *gatedOracle) Price(_ context.Context, instrument config.InstrumentConfig) (domain.ReferencePrice, error) {
	o.entered <- instrument.ID
	<-o.release
	return domain.ReferencePrice{InstrumentID: instrument.ID, Price: num.Must("1"), ValidUntil: time.Now().Add(time.Minute)}, nil
}

func TestRunOnceProcessesIndependentInstrumentsConcurrently(t *testing.T) {
	oracle := &gatedOracle{entered: make(chan string, 2), release: make(chan struct{}, 2)}
	cfg := config.Config{MaxConcurrentInstruments: 2, Instruments: []config.InstrumentConfig{{ID: "a"}, {ID: "b"}}}
	engine := New(cfg, oracle, nil, audit.New(""), nil, slog.Default())
	done := make(chan struct{})
	go func() {
		_ = engine.RunOnce(context.Background())
		close(done)
	}()
	waitEntered(t, oracle.entered)
	waitEntered(t, oracle.entered)
	oracle.release <- struct{}{}
	oracle.release <- struct{}{}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("parallel cycle did not finish")
	}
}

func TestRunOnceSerializesInstrumentsSharingCredential(t *testing.T) {
	oracle := &gatedOracle{entered: make(chan string, 2), release: make(chan struct{}, 2)}
	cfg := config.Config{
		MaxConcurrentInstruments: 2,
		Instruments:              []config.InstrumentConfig{{ID: "a"}, {ID: "b"}},
		Venues: map[string]config.VenueConfig{"binance": {Type: "binance", Enabled: true, Markets: map[string]config.VenueMarketConfig{
			"a": {CredentialID: 7}, "b": {CredentialID: 7},
		}}},
	}
	engine := New(cfg, oracle, nil, audit.New(""), nil, slog.Default())
	done := make(chan struct{})
	go func() {
		_ = engine.RunOnce(context.Background())
		close(done)
	}()
	waitEntered(t, oracle.entered)
	select {
	case second := <-oracle.entered:
		t.Fatalf("shared account entered concurrently: %s", second)
	case <-time.After(50 * time.Millisecond):
	}
	oracle.release <- struct{}{}
	waitEntered(t, oracle.entered)
	oracle.release <- struct{}{}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("serialized cycle did not finish")
	}
}

func waitEntered(t *testing.T, entered <-chan string) string {
	t.Helper()
	select {
	case instrumentID := <-entered:
		return instrumentID
	case <-time.After(time.Second):
		t.Fatal("instrument did not start")
		return ""
	}
}

func TestQuoteSummaryStaysCompactForDeepBooks(t *testing.T) {
	quotes := make([]domain.Quote, 0, 200)
	for level := 0; level < 100; level++ {
		quotes = append(quotes,
			domain.Quote{Side: domain.Buy, Price: num.FromInt64(int64(100 - level)), Quantity: num.Must("2")},
			domain.Quote{Side: domain.Sell, Price: num.FromInt64(int64(101 + level)), Quantity: num.Must("2")},
		)
	}
	summary := summarizeQuotes(quotes)
	if summary.Count != 200 || summary.BuyCount != 100 || summary.SellCount != 100 {
		t.Fatalf("unexpected counts: %+v", summary)
	}
	if summary.BestBid.Cmp(num.Must("100")) != 0 || summary.OuterBid.Cmp(num.Must("1")) != 0 || summary.BestAsk.Cmp(num.Must("101")) != 0 || summary.OuterAsk.Cmp(num.Must("200")) != 0 {
		t.Fatalf("unexpected price bounds: %+v", summary)
	}
	if summary.TotalQuantity.Cmp(num.Must("400")) != 0 {
		t.Fatalf("total quantity=%s", summary.TotalQuantity)
	}
}

func TestMetricsSurviveRuntimeHotSwap(t *testing.T) {
	first := New(config.Config{}, nil, nil, audit.New(""), nil, slog.Default())
	first.metrics.recordCycle(2, 1)
	second := New(config.Config{}, nil, nil, audit.New(""), nil, slog.Default())
	second.InheritMetricsFrom(first)
	second.metrics.recordCycle(1, 0)
	snapshot := second.metrics.snapshot(0)
	if snapshot.CyclesTotal != 2 || snapshot.InstrumentRunsTotal != 3 || snapshot.InstrumentFailuresTotal != 1 {
		t.Fatalf("metrics were reset during hot swap: %+v", snapshot)
	}
}
