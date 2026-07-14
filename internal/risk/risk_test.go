package risk

import (
	"testing"
	"time"

	"fluxmaker/internal/config"
	"fluxmaker/internal/domain"
	"fluxmaker/internal/num"
)

func TestInventoryLimitKeepsReducingSide(t *testing.T) {
	in := config.InstrumentConfig{Strategy: config.StrategyConfig{TargetBase: num.Must("10"), MaxBaseDeviation: num.Must("5")}}
	market := config.VenueMarketConfig{MinNotional: num.Must("1")}
	book := domain.Book{BidPrice: num.Must("99"), AskPrice: num.Must("101"), Timestamp: time.Now()}
	valid := time.Now().Add(time.Minute)
	quotes := []domain.Quote{
		{Side: domain.Buy, Price: num.Must("99.5"), Quantity: num.Must("1"), ValidUntil: valid},
		{Side: domain.Sell, Price: num.Must("100.5"), Quantity: num.Must("1"), ValidUntil: valid},
	}
	filtered, err := (Engine{}).FilterQuotes(in, market, book, num.Must("15"), quotes)
	if err != nil {
		t.Fatal(err)
	}
	if len(filtered) != 1 || filtered[0].Side != domain.Sell {
		t.Fatalf("filtered=%+v", filtered)
	}
}

func TestFilterQuotesAllowsEmptyBookBootstrap(t *testing.T) {
	in := config.InstrumentConfig{Strategy: config.StrategyConfig{TargetBase: num.Must("10")}}
	market := config.VenueMarketConfig{MinNotional: num.Must("1")}
	valid := time.Now().Add(time.Minute)
	quotes := []domain.Quote{
		{Side: domain.Buy, Price: num.Must("99.5"), Quantity: num.Must("1"), ValidUntil: valid},
		{Side: domain.Sell, Price: num.Must("100.5"), Quantity: num.Must("1"), ValidUntil: valid},
	}
	filtered, err := (Engine{}).FilterQuotes(in, market, domain.Book{}, num.Must("10"), quotes)
	if err != nil || len(filtered) != 2 {
		t.Fatalf("filtered=%+v err=%v", filtered, err)
	}
}
