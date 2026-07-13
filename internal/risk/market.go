package risk

import (
	"fmt"

	"fluxmaker/internal/config"
	"fluxmaker/internal/domain"
	"fluxmaker/internal/num"
)

func ValidateMarketReference(ref domain.ReferencePrice, book domain.Book, strategy config.StrategyConfig) error {
	if !ref.Price.IsPositive() || !book.BidPrice.IsPositive() || !book.AskPrice.IsPositive() || book.BidPrice.Cmp(book.AskPrice) >= 0 {
		return fmt.Errorf("invalid reference or venue book")
	}
	mid := book.BidPrice.Add(book.AskPrice).Div(num.FromInt64(2))
	if strategy.MaxVenueReferenceDeviationBPS > 0 {
		deviation := mid.Sub(ref.Price).Abs().Div(ref.Price).Mul(num.TenThousand())
		if deviation.Cmp(num.FromInt64(int64(strategy.MaxVenueReferenceDeviationBPS))) > 0 {
			return fmt.Errorf("venue/reference deviation %s bps exceeds %d", deviation.String(), strategy.MaxVenueReferenceDeviationBPS)
		}
	}
	if strategy.MaxVenueSpreadBPS > 0 {
		spread := book.AskPrice.Sub(book.BidPrice).Div(mid).Mul(num.TenThousand())
		if spread.Cmp(num.FromInt64(int64(strategy.MaxVenueSpreadBPS))) > 0 {
			return fmt.Errorf("venue spread %s bps exceeds %d", spread.String(), strategy.MaxVenueSpreadBPS)
		}
	}
	return nil
}
