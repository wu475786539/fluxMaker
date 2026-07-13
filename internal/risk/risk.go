package risk

import (
	"fmt"
	"time"

	"fluxmaker/internal/config"
	"fluxmaker/internal/domain"
	"fluxmaker/internal/num"
)

type Engine struct{}

func (Engine) FilterQuotes(in config.InstrumentConfig, market config.VenueMarketConfig, book domain.Book, inventory num.Decimal, quotes []domain.Quote) ([]domain.Quote, error) {
	if time.Since(book.Timestamp) > 30*time.Second {
		return nil, fmt.Errorf("venue book is stale")
	}
	deviation := inventory.Sub(in.Strategy.TargetBase)
	result := make([]domain.Quote, 0, len(quotes))
	for _, q := range quotes {
		if time.Now().After(q.ValidUntil) {
			return nil, fmt.Errorf("quote reference expired")
		}
		if q.Side == domain.Buy && q.Price.Cmp(book.AskPrice) >= 0 {
			return nil, fmt.Errorf("buy quote would take liquidity")
		}
		if q.Side == domain.Sell && q.Price.Cmp(book.BidPrice) <= 0 {
			return nil, fmt.Errorf("sell quote would take liquidity")
		}
		if q.Price.Mul(q.Quantity).Cmp(market.MinNotional) < 0 {
			return nil, fmt.Errorf("quote below minimum notional")
		}
		if market.MinQuantity.IsPositive() && q.Quantity.Cmp(market.MinQuantity) < 0 {
			return nil, fmt.Errorf("quote below exchange minimum quantity")
		}
		if market.MaxQuantity.IsPositive() && q.Quantity.Cmp(market.MaxQuantity) > 0 {
			return nil, fmt.Errorf("quote above exchange maximum quantity")
		}
		notional := q.Price.Mul(q.Quantity)
		if market.MaxNotional.IsPositive() && notional.Cmp(market.MaxNotional) > 0 {
			return nil, fmt.Errorf("quote above exchange maximum notional")
		}
		if market.MinPrice.IsPositive() && q.Price.Cmp(market.MinPrice) < 0 {
			return nil, fmt.Errorf("quote below exchange minimum price")
		}
		if market.MaxPrice.IsPositive() && q.Price.Cmp(market.MaxPrice) > 0 {
			return nil, fmt.Errorf("quote above exchange maximum price")
		}
		if in.Strategy.MaxBaseDeviation.IsPositive() {
			if deviation.Cmp(in.Strategy.MaxBaseDeviation) >= 0 && q.Side == domain.Buy {
				continue
			}
			if deviation.Cmp(in.Strategy.MaxBaseDeviation.Neg()) <= 0 && q.Side == domain.Sell {
				continue
			}
		}
		result = append(result, q)
	}
	for i := range result {
		for j := range result {
			if result[i].Side == domain.Buy && result[j].Side == domain.Sell && result[i].Price.Cmp(result[j].Price) >= 0 {
				return nil, fmt.Errorf("target quotes self-cross")
			}
		}
	}
	if len(result) == 0 {
		return nil, fmt.Errorf("all quote sides blocked by inventory limits")
	}
	return result, nil
}
