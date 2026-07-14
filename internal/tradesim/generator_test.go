package tradesim

import (
	"testing"
	"time"

	"fluxmaker/internal/config"
	"fluxmaker/internal/domain"
	"fluxmaker/internal/num"
)

func simulationFixture() (config.InstrumentConfig, config.VenueMarketConfig, domain.Book) {
	instrument := config.InstrumentConfig{ID: "token_usdt", TradeSimulation: config.TradeSimulationConfig{
		Enabled: true, SourceVenue: "binance-testnet", MinQuantity: num.Must("1"), MaxQuantity: num.Must("2"),
		MinIntervalMS: 100, MaxIntervalMS: 100, BuyProbabilityBPS: 10_000, RecentLimit: 10, BatchSize: 1,
	}}
	market := config.VenueMarketConfig{Symbol: "TOKENUSDT", PriceTick: num.Must("0.01"), QuantityStep: num.Must("0.1"), MinNotional: num.Must("5")}
	book := domain.Book{BidPrice: num.Must("9.99"), AskPrice: num.Must("10.05")}
	return instrument, market, book
}

func TestGeneratorCreatesMarkedFillStrictlyInsideBook(t *testing.T) {
	instrument, market, book := simulationFixture()
	generator := New()
	start := time.Date(2026, 7, 12, 12, 0, 0, 0, time.UTC)
	if _, fills := generator.Observe(instrument, "binance-testnet", market, book, start); len(fills) > 0 {
		t.Fatal("first observation should only schedule generation")
	}
	snapshot, fills := generator.Observe(instrument, "binance-testnet", market, book, start.Add(100*time.Millisecond))
	if len(fills) == 0 {
		t.Fatal("expected a generated fill")
	}
	fill := fills[0]
	if !fill.Simulated || fill.Side != domain.Buy {
		t.Fatalf("expected marked BUY simulation, got %+v", fill)
	}
	if fill.Price.Cmp(book.BidPrice) <= 0 || fill.Price.Cmp(book.AskPrice) >= 0 {
		t.Fatalf("price %s must be strictly inside %s/%s", fill.Price, book.BidPrice, book.AskPrice)
	}
	if fill.Quantity.Cmp(instrument.TradeSimulation.MinQuantity) < 0 || fill.Quantity.Cmp(instrument.TradeSimulation.MaxQuantity) > 0 {
		t.Fatalf("quantity outside configured range: %s", fill.Quantity)
	}
	if snapshot.Status != "running" || len(snapshot.Fills) != 1 {
		t.Fatalf("unexpected snapshot: %+v", snapshot)
	}
}

func TestGeneratorSkipsWhenNoTickExistsInsideSpread(t *testing.T) {
	instrument, market, book := simulationFixture()
	book.BidPrice = num.Must("10.00")
	book.AskPrice = num.Must("10.01")
	generator := New()
	start := time.Now().UTC()
	generator.Observe(instrument, "binance-testnet", market, book, start)
	snapshot, fills := generator.Observe(instrument, "binance-testnet", market, book, start.Add(100*time.Millisecond))
	if len(fills) > 0 || snapshot.Status != "skipped" || snapshot.Error == "" {
		t.Fatalf("expected an explained skip, got fills=%+v snapshot=%+v", fills, snapshot)
	}
}

func TestGeneratorBatchSizeProducesMultipleFills(t *testing.T) {
	instrument, market, book := simulationFixture()
	instrument.TradeSimulation.BatchSize = 3
	generator := New()
	start := time.Date(2026, 7, 12, 12, 0, 0, 0, time.UTC)
	generator.Observe(instrument, "binance-testnet", market, book, start)
	snapshot, fills := generator.Observe(instrument, "binance-testnet", market, book, start.Add(100*time.Millisecond))
	if len(fills) != 3 {
		t.Fatalf("expected 3 fills, got %d", len(fills))
	}
	for i, fill := range fills {
		if !fill.Simulated {
			t.Fatalf("fill %d: expected simulated=true", i)
		}
		if fill.Price.Cmp(book.BidPrice) <= 0 || fill.Price.Cmp(book.AskPrice) >= 0 {
			t.Fatalf("fill %d: price %s must be strictly inside %s/%s", i, fill.Price, book.BidPrice, book.AskPrice)
		}
	}
	if snapshot.Status != "running" || len(snapshot.Fills) != 3 {
		t.Fatalf("unexpected snapshot: status=%s fills=%d", snapshot.Status, len(snapshot.Fills))
	}
}

func TestGeneratorBatchSizeZeroDefaultsToOne(t *testing.T) {
	instrument, market, book := simulationFixture()
	instrument.TradeSimulation.BatchSize = 0
	generator := New()
	start := time.Date(2026, 7, 12, 12, 0, 0, 0, time.UTC)
	generator.Observe(instrument, "binance-testnet", market, book, start)
	_, fills := generator.Observe(instrument, "binance-testnet", market, book, start.Add(100*time.Millisecond))
	if len(fills) != 1 {
		t.Fatalf("batchSize=0 should default to 1 fill, got %d", len(fills))
	}
}
