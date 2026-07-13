package strategy

import (
	"fmt"
	"hash/fnv"
	"time"

	"fluxmaker/internal/config"
	"fluxmaker/internal/domain"
	"fluxmaker/internal/num"
)

type Generator struct{}

func (Generator) Generate(in config.InstrumentConfig, venueName string, market config.VenueMarketConfig, ref domain.ReferencePrice, book domain.Book, inventory num.Decimal) ([]domain.Quote, error) {
	if !ref.Price.IsPositive() {
		return nil, fmt.Errorf("reference price is not positive")
	}
	if book.BidPrice.Cmp(book.AskPrice) >= 0 {
		return nil, fmt.Errorf("crossed or empty venue book")
	}
	mid := applyInventorySkew(ref.Price, in.Strategy, inventory)
	quotes := make([]domain.Quote, 0, in.Strategy.Levels*2)
	var previousBid, previousAsk num.Decimal
	for level := 0; level < in.Strategy.Levels; level++ {
		spreadBPS := in.Strategy.HalfSpreadBPS + level*in.Strategy.LevelSpacingBPS
		spread := num.FromInt64(int64(spreadBPS)).Div(num.TenThousand())
		bid := mid.Mul(num.One().Sub(spread)).QuantizeDown(market.PriceTick)
		ask := mid.Mul(num.One().Add(spread)).QuantizeUp(market.PriceTick)

		// A post-only bid must remain strictly below the best ask; the ask must
		// remain strictly above the best bid.
		maxBid := book.AskPrice.Sub(market.PriceTick)
		if bid.Cmp(maxBid) > 0 {
			bid = maxBid.QuantizeDown(market.PriceTick)
		}
		minAsk := book.BidPrice.Add(market.PriceTick)
		if ask.Cmp(minAsk) < 0 {
			ask = minAsk.QuantizeUp(market.PriceTick)
		}
		if level > 0 {
			maxLevelBid := previousBid.Sub(market.PriceTick)
			if bid.Cmp(previousBid) >= 0 {
				bid = maxLevelBid.QuantizeDown(market.PriceTick)
			}
			minLevelAsk := previousAsk.Add(market.PriceTick)
			if ask.Cmp(previousAsk) <= 0 {
				ask = minLevelAsk.QuantizeUp(market.PriceTick)
			}
		}
		if !bid.IsPositive() || !ask.IsPositive() {
			return nil, fmt.Errorf("quote rounded to zero")
		}
		if bid.Cmp(ask) >= 0 {
			return nil, fmt.Errorf("generated quotes cross")
		}
		bidQty, err := quoteQuantity(in, venueName, market, domain.Buy, level, bid)
		if err != nil {
			return nil, fmt.Errorf("buy level %d: %w", level+1, err)
		}
		askQty, err := quoteQuantity(in, venueName, market, domain.Sell, level, ask)
		if err != nil {
			return nil, fmt.Errorf("sell level %d: %w", level+1, err)
		}
		validUntil := ref.ValidUntil
		if validUntil.IsZero() {
			validUntil = time.Now().UTC().Add(10 * time.Second)
		}
		quotes = append(quotes,
			domain.Quote{InstrumentID: in.ID, Venue: venueName, Symbol: market.Symbol, Side: domain.Buy, Level: level, Price: bid, Quantity: bidQty, Reference: ref.Price, ValidUntil: validUntil},
			domain.Quote{InstrumentID: in.ID, Venue: venueName, Symbol: market.Symbol, Side: domain.Sell, Level: level, Price: ask, Quantity: askQty, Reference: ref.Price, ValidUntil: validUntil},
		)
		previousBid, previousAsk = bid, ask
	}
	return quotes, nil
}

// quoteQuantity converts a stable, per-level quote-asset amount into a valid
// base quantity. The stable hash prevents each engine tick from producing new
// random sizes, while the price bucket keeps quantities steady across small
// price movements that remain inside the reprice threshold.
func quoteQuantity(in config.InstrumentConfig, venueName string, market config.VenueMarketConfig, side domain.Side, level int, price num.Decimal) (num.Decimal, error) {
	if !in.Strategy.UsesOrderNotionalRange() {
		qty := in.Strategy.OrderSize.QuantizeDown(market.QuantityStep)
		if !qty.IsPositive() {
			return num.Decimal{}, fmt.Errorf("legacy order size rounded to zero")
		}
		return qty, nil
	}

	minimum := in.Strategy.MinOrderNotional.Max(market.MinNotional)
	maximum := in.Strategy.MaxOrderNotional
	if market.MaxNotional.IsPositive() {
		maximum = maximum.Min(market.MaxNotional)
	}
	if maximum.Cmp(minimum) < 0 {
		return num.Decimal{}, fmt.Errorf("configured notional range %s-%s is incompatible with exchange range %s-%s", in.Strategy.MinOrderNotional, in.Strategy.MaxOrderNotional, market.MinNotional, market.MaxNotional)
	}

	minimumQty := minimum.Div(price).QuantizeUp(market.QuantityStep)
	if market.MinQuantity.IsPositive() {
		minimumQty = minimumQty.Max(market.MinQuantity.QuantizeUp(market.QuantityStep))
	}
	maximumQty := maximum.Div(price).QuantizeDown(market.QuantityStep)
	if market.MaxQuantity.IsPositive() {
		maximumQty = maximumQty.Min(market.MaxQuantity.QuantizeDown(market.QuantityStep))
	}
	if !maximumQty.IsPositive() || minimumQty.Cmp(maximumQty) > 0 {
		return num.Decimal{}, fmt.Errorf("notional range %s-%s cannot satisfy quantity step %s and exchange limits at price %s", minimum, maximum, market.QuantityStep, price)
	}

	target := stableOrderNotional(in.ID, venueName, market.Symbol, side, level, minimum, maximum)
	anchor := stablePriceAnchor(price, market.PriceTick, in.Strategy.RepriceThresholdBPS)
	quantity := target.Div(anchor).QuantizeDown(market.QuantityStep)
	if quantity.Cmp(minimumQty) < 0 {
		quantity = minimumQty
	}
	if quantity.Cmp(maximumQty) > 0 {
		quantity = maximumQty
	}
	if !quantity.IsPositive() {
		return num.Decimal{}, fmt.Errorf("order quantity rounded to zero")
	}

	actual := price.Mul(quantity)
	if actual.Cmp(minimum) < 0 || actual.Cmp(maximum) > 0 {
		return num.Decimal{}, fmt.Errorf("rounded notional %s is outside effective range %s-%s", actual, minimum, maximum)
	}
	return quantity, nil
}

func stableOrderNotional(instrumentID, venueName, symbol string, side domain.Side, level int, minimum, maximum num.Decimal) num.Decimal {
	if minimum.Cmp(maximum) == 0 {
		return minimum
	}
	hash := fnv.New64a()
	_, _ = fmt.Fprintf(hash, "%s|%s|%s|%s|%d|%s|%s", instrumentID, venueName, symbol, side, level, minimum, maximum)
	// Stay strictly inside the configured range so exchange-step rounding and
	// small in-bucket price changes have room on both sides.
	const buckets int64 = 1_000_000
	fraction := num.FromInt64(int64(hash.Sum64()%uint64(buckets-1)) + 1).Div(num.FromInt64(buckets))
	return minimum.Add(maximum.Sub(minimum).Mul(fraction))
}

func stablePriceAnchor(price, tick num.Decimal, repriceThresholdBPS int) num.Decimal {
	if repriceThresholdBPS <= 0 {
		return price
	}
	width := price.Mul(num.FromInt64(int64(repriceThresholdBPS))).Div(num.TenThousand()).QuantizeUp(tick)
	if !width.IsPositive() {
		width = tick
	}
	anchor := price.QuantizeDown(width)
	if !anchor.IsPositive() {
		return price
	}
	return anchor
}

func applyInventorySkew(fair num.Decimal, cfg config.StrategyConfig, inventory num.Decimal) num.Decimal {
	if cfg.MaxBaseDeviation.Sign() <= 0 || cfg.InventorySkewBPS <= 0 {
		return fair
	}
	deviation := inventory.Sub(cfg.TargetBase).Div(cfg.MaxBaseDeviation)
	if deviation.Cmp(num.One()) > 0 {
		deviation = num.One()
	}
	if deviation.Cmp(num.FromInt64(-1)) < 0 {
		deviation = num.FromInt64(-1)
	}
	shift := deviation.Mul(num.FromInt64(int64(cfg.InventorySkewBPS))).Div(num.TenThousand())
	return fair.Mul(num.One().Sub(shift))
}
