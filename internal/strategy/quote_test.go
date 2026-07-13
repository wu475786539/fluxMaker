package strategy

import (
	"testing"
	"time"

	"fluxmaker/internal/config"
	"fluxmaker/internal/domain"
	"fluxmaker/internal/num"
)

func TestGenerateQuotes(t *testing.T) {
	in := config.InstrumentConfig{ID: "token_usdt", Strategy: config.StrategyConfig{
		HalfSpreadBPS: 50, LevelSpacingBPS: 25, Levels: 1, OrderSize: num.Must("1"),
		TargetBase: num.Must("10"), MaxBaseDeviation: num.Must("5"), InventorySkewBPS: 100,
	}}
	market := config.VenueMarketConfig{Symbol: "TOKENUSDT", PriceTick: num.Must("0.01"), QuantityStep: num.Must("0.1"), MinNotional: num.Must("1")}
	ref := domain.ReferencePrice{Price: num.Must("100"), ValidUntil: time.Now().Add(time.Minute)}
	book := domain.Book{BidPrice: num.Must("99"), AskPrice: num.Must("101"), Timestamp: time.Now()}
	quotes, err := (Generator{}).Generate(in, "binance", market, ref, book, num.Must("10"))
	if err != nil {
		t.Fatal(err)
	}
	if len(quotes) != 2 {
		t.Fatalf("len=%d", len(quotes))
	}
	if got := quotes[0].Price.String(); got != "99.5" {
		t.Fatalf("bid=%s", got)
	}
	if got := quotes[1].Price.String(); got != "100.5" {
		t.Fatalf("ask=%s", got)
	}
}

func TestLongInventoryMovesQuotesDown(t *testing.T) {
	cfg := config.StrategyConfig{TargetBase: num.Must("10"), MaxBaseDeviation: num.Must("5"), InventorySkewBPS: 100}
	if got := applyInventorySkew(num.Must("100"), cfg, num.Must("15")); got.Cmp(num.Must("99")) != 0 {
		t.Fatalf("mid=%s", got.String())
	}
}

func TestGenerateOneHundredLevels(t *testing.T) {
	in := config.InstrumentConfig{ID: "token_usdt", Strategy: config.StrategyConfig{
		HalfSpreadBPS: 25, LevelSpacingBPS: 25, Levels: 100, OrderSize: num.Must("1"),
	}}
	market := config.VenueMarketConfig{Symbol: "TOKENUSDT", PriceTick: num.Must("0.0001"), QuantityStep: num.Must("0.1"), MinNotional: num.Must("1")}
	ref := domain.ReferencePrice{Price: num.Must("100"), ValidUntil: time.Now().Add(time.Minute)}
	book := domain.Book{BidPrice: num.Must("99"), AskPrice: num.Must("101"), Timestamp: time.Now()}
	quotes, err := (Generator{}).Generate(in, "binance", market, ref, book, num.Must("0"))
	if err != nil {
		t.Fatal(err)
	}
	if len(quotes) != 200 {
		t.Fatalf("len=%d want=200", len(quotes))
	}
}

func TestLevelsRemainUniqueWhenSpacingIsBelowOneTick(t *testing.T) {
	in := config.InstrumentConfig{ID: "token_usdt", Strategy: config.StrategyConfig{HalfSpreadBPS: 1, LevelSpacingBPS: 1, Levels: 4, OrderSize: num.Must("1")}}
	market := config.VenueMarketConfig{Symbol: "TOKENUSDT", PriceTick: num.Must("1"), QuantityStep: num.Must("1"), MinNotional: num.Must("1")}
	ref := domain.ReferencePrice{Price: num.Must("100"), ValidUntil: time.Now().Add(time.Minute)}
	book := domain.Book{BidPrice: num.Must("99"), AskPrice: num.Must("101"), Timestamp: time.Now()}
	quotes, err := (Generator{}).Generate(in, "binance", market, ref, book, num.Must("0"))
	if err != nil {
		t.Fatal(err)
	}
	for level := 1; level < 4; level++ {
		previousBid, currentBid := quotes[(level-1)*2].Price, quotes[level*2].Price
		previousAsk, currentAsk := quotes[(level-1)*2+1].Price, quotes[level*2+1].Price
		if currentBid.Cmp(previousBid) >= 0 || currentAsk.Cmp(previousAsk) <= 0 {
			t.Fatalf("level %d is not unique: %s/%s after %s/%s", level, currentBid, currentAsk, previousBid, previousAsk)
		}
	}
}

func TestGenerateUsesStableQuoteNotionalRange(t *testing.T) {
	in := config.InstrumentConfig{ID: "gdt_usdt", Strategy: config.StrategyConfig{
		HalfSpreadBPS: 50, LevelSpacingBPS: 25, Levels: 5,
		MinOrderNotional: num.Must("10"), MaxOrderNotional: num.Must("20"), RepriceThresholdBPS: 10,
	}}
	market := config.VenueMarketConfig{
		Symbol: "GDTUSDT", PriceTick: num.Must("0.000001"), QuantityStep: num.Must("0.01"),
		MinNotional: num.Must("5"), MinQuantity: num.Must("0.01"), MaxQuantity: num.Must("100000"),
	}
	ref := domain.ReferencePrice{Price: num.Must("0.365841"), ValidUntil: time.Now().Add(time.Minute)}
	book := domain.Book{BidPrice: num.Must("0.36"), AskPrice: num.Must("0.37"), Timestamp: time.Now()}

	first, err := (Generator{}).Generate(in, "binance", market, ref, book, num.Decimal{})
	if err != nil {
		t.Fatal(err)
	}
	second, err := (Generator{}).Generate(in, "binance", market, ref, book, num.Decimal{})
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != 10 || len(second) != len(first) {
		t.Fatalf("quote counts=%d/%d", len(first), len(second))
	}

	quantities := map[string]struct{}{}
	for index, quote := range first {
		notional := quote.Price.Mul(quote.Quantity)
		if notional.Cmp(num.Must("10")) < 0 || notional.Cmp(num.Must("20")) > 0 {
			t.Fatalf("quote %d notional=%s outside 10-20", index, notional)
		}
		if quote.Quantity.Cmp(second[index].Quantity) != 0 {
			t.Fatalf("quote %d quantity changed between identical generations: %s/%s", index, quote.Quantity, second[index].Quantity)
		}
		if quote.Quantity.Cmp(quote.Quantity.QuantizeDown(market.QuantityStep)) != 0 {
			t.Fatalf("quote %d quantity=%s does not follow step %s", index, quote.Quantity, market.QuantityStep)
		}
		quantities[quote.Quantity.String()] = struct{}{}
	}
	if len(quantities) < 2 {
		t.Fatalf("expected randomized per-level quantities, got %v", quantities)
	}
}

func TestGenerateRejectsRangeBelowExchangeMinimumNotional(t *testing.T) {
	in := config.InstrumentConfig{ID: "token_usdt", Strategy: config.StrategyConfig{
		HalfSpreadBPS: 50, Levels: 1, MinOrderNotional: num.Must("2"), MaxOrderNotional: num.Must("4"),
	}}
	market := config.VenueMarketConfig{Symbol: "TOKENUSDT", PriceTick: num.Must("0.01"), QuantityStep: num.Must("0.1"), MinNotional: num.Must("5")}
	ref := domain.ReferencePrice{Price: num.Must("100"), ValidUntil: time.Now().Add(time.Minute)}
	book := domain.Book{BidPrice: num.Must("99"), AskPrice: num.Must("101"), Timestamp: time.Now()}

	if _, err := (Generator{}).Generate(in, "binance", market, ref, book, num.Decimal{}); err == nil {
		t.Fatal("expected exchange minimum notional conflict to fail")
	}
}

func TestGenerateKeepsLegacyFixedQuantity(t *testing.T) {
	in := config.InstrumentConfig{ID: "token_usdt", Strategy: config.StrategyConfig{
		HalfSpreadBPS: 50, Levels: 1, OrderSize: num.Must("1.23"),
	}}
	market := config.VenueMarketConfig{Symbol: "TOKENUSDT", PriceTick: num.Must("0.01"), QuantityStep: num.Must("0.1"), MinNotional: num.Must("1")}
	ref := domain.ReferencePrice{Price: num.Must("100"), ValidUntil: time.Now().Add(time.Minute)}
	book := domain.Book{BidPrice: num.Must("99"), AskPrice: num.Must("101"), Timestamp: time.Now()}

	quotes, err := (Generator{}).Generate(in, "binance", market, ref, book, num.Decimal{})
	if err != nil {
		t.Fatal(err)
	}
	for _, quote := range quotes {
		if quote.Quantity.Cmp(num.Must("1.2")) != 0 {
			t.Fatalf("legacy quantity=%s want=1.2", quote.Quantity)
		}
	}
}
