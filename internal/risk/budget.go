package risk

import (
	"fluxmaker/internal/domain"
	"fluxmaker/internal/num"
)

// ApplyBalanceBudget keeps the closest quotes that fit the capital controlled
// by this market. Existing managed commitments are included so a stable book
// does not oscillate as free funds become locked by its own orders.
func ApplyBalanceBudget(quotes []domain.Quote, managed []domain.Order, baseFree, quoteFree num.Decimal, reserveBPS int, maxBase, maxQuote num.Decimal) ([]domain.Quote, domain.QuoteBudget) {
	if reserveBPS < 0 {
		reserveBPS = 0
	}
	if reserveBPS > 10_000 {
		reserveBPS = 10_000
	}
	baseCommitted := num.FromInt64(0)
	quoteCommitted := num.FromInt64(0)
	for _, order := range managed {
		remaining := order.Quantity.Sub(order.ExecutedQty)
		if remaining.Sign() <= 0 {
			continue
		}
		if order.Side == domain.Sell {
			baseCommitted = baseCommitted.Add(remaining)
		} else if order.Side == domain.Buy {
			quoteCommitted = quoteCommitted.Add(order.Price.Mul(remaining))
		}
	}
	if baseFree.Sign() < 0 {
		baseFree = num.FromInt64(0)
	}
	if quoteFree.Sign() < 0 {
		quoteFree = num.FromInt64(0)
	}
	factor := num.FromInt64(int64(10_000 - reserveBPS)).Div(num.TenThousand())
	budget := domain.QuoteBudget{
		ReserveBPS:   reserveBPS,
		TargetOrders: len(quotes),
		BaseBudget:   baseFree.Add(baseCommitted).Mul(factor),
		QuoteBudget:  quoteFree.Add(quoteCommitted).Mul(factor),
	}
	if maxBase.IsPositive() {
		budget.BaseBudget = budget.BaseBudget.Min(maxBase)
	}
	if maxQuote.IsPositive() {
		budget.QuoteBudget = budget.QuoteBudget.Min(maxQuote)
	}
	for _, quote := range quotes {
		if quote.Side == domain.Sell {
			budget.BaseRequired = budget.BaseRequired.Add(quote.Quantity)
		} else if quote.Side == domain.Buy {
			budget.QuoteRequired = budget.QuoteRequired.Add(quote.Price.Mul(quote.Quantity))
		}
	}

	usedBase := num.FromInt64(0)
	usedQuote := num.FromInt64(0)
	eligible := make([]domain.Quote, 0, len(quotes))
	for _, quote := range quotes {
		switch quote.Side {
		case domain.Sell:
			next := usedBase.Add(quote.Quantity)
			if next.Cmp(budget.BaseBudget) > 0 {
				budget.BaseLimited = true
				continue
			}
			usedBase = next
		case domain.Buy:
			next := usedQuote.Add(quote.Price.Mul(quote.Quantity))
			if next.Cmp(budget.QuoteBudget) > 0 {
				budget.QuoteLimited = true
				continue
			}
			usedQuote = next
		default:
			continue
		}
		eligible = append(eligible, quote)
	}
	budget.EligibleOrders = len(eligible)
	return eligible, budget
}

func ApplyOrderLimit(quotes []domain.Quote, currentOrders, managedOrders, maxOpenOrders int) []domain.Quote {
	if maxOpenOrders <= 0 {
		return quotes
	}
	unmanaged := currentOrders - managedOrders
	if unmanaged < 0 {
		unmanaged = 0
	}
	available := maxOpenOrders - unmanaged
	if available < 0 {
		available = 0
	}
	if available >= len(quotes) {
		return quotes
	}
	// Quotes are generated BUY/SELL by increasing level. Keep a balanced number
	// of closest orders when the exchange limit is smaller than the strategy.
	if available%2 != 0 {
		available--
	}
	return append([]domain.Quote(nil), quotes[:available]...)
}
