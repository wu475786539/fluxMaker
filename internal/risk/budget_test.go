package risk

import (
	"testing"

	"fluxmaker/internal/domain"
	"fluxmaker/internal/num"
)

func TestBalanceBudgetPrioritizesInnerLevels(t *testing.T) {
	quotes := []domain.Quote{
		{Side: domain.Buy, Level: 0, Price: num.Must("10"), Quantity: num.Must("2")},
		{Side: domain.Sell, Level: 0, Price: num.Must("11"), Quantity: num.Must("2")},
		{Side: domain.Buy, Level: 1, Price: num.Must("9"), Quantity: num.Must("2")},
		{Side: domain.Sell, Level: 1, Price: num.Must("12"), Quantity: num.Must("2")},
	}
	eligible, budget := ApplyBalanceBudget(quotes, nil, num.Must("2"), num.Must("20"), 0, num.Decimal{}, num.Decimal{})
	if len(eligible) != 2 || eligible[0].Level != 0 || eligible[1].Level != 0 {
		t.Fatalf("eligible=%+v", eligible)
	}
	if !budget.BaseLimited || !budget.QuoteLimited || budget.EligibleOrders != 2 {
		t.Fatalf("budget=%+v", budget)
	}
}

func TestBalanceBudgetIncludesExistingManagedCommitments(t *testing.T) {
	quotes := []domain.Quote{
		{Side: domain.Buy, Price: num.Must("10"), Quantity: num.Must("5")},
		{Side: domain.Sell, Price: num.Must("11"), Quantity: num.Must("5")},
	}
	managed := []domain.Order{
		{Side: domain.Buy, Price: num.Must("10"), Quantity: num.Must("5")},
		{Side: domain.Sell, Price: num.Must("11"), Quantity: num.Must("5")},
	}
	eligible, budget := ApplyBalanceBudget(quotes, managed, num.Must("1"), num.Must("10"), 0, num.Decimal{}, num.Decimal{})
	if len(eligible) != 2 {
		t.Fatalf("eligible=%+v budget=%+v", eligible, budget)
	}
}

func TestBalanceReserveAppliesToTotalControlledCapital(t *testing.T) {
	quotes := []domain.Quote{
		{Side: domain.Buy, Price: num.Must("1"), Quantity: num.Must("95")},
		{Side: domain.Buy, Price: num.Must("1"), Quantity: num.Must("1")},
	}
	managed := []domain.Order{{Side: domain.Buy, Price: num.Must("1"), Quantity: num.Must("90")}}
	eligible, budget := ApplyBalanceBudget(quotes, managed, num.Must("0"), num.Must("10"), 500, num.Decimal{}, num.Decimal{})
	if budget.QuoteBudget.Cmp(num.Must("95")) != 0 || len(eligible) != 1 {
		t.Fatalf("eligible=%d budget=%+v", len(eligible), budget)
	}
}

func TestOrderLimitPreservesBalancedInnerLevels(t *testing.T) {
	quotes := make([]domain.Quote, 0, 10)
	for level := 0; level < 5; level++ {
		quotes = append(quotes, domain.Quote{Side: domain.Buy, Level: level}, domain.Quote{Side: domain.Sell, Level: level})
	}
	limited := ApplyOrderLimit(quotes, 3, 2, 6)
	if len(limited) != 4 || limited[3].Level != 1 {
		t.Fatalf("limited=%+v", limited)
	}
}

func TestBalanceBudgetHonorsPerMarketHardCaps(t *testing.T) {
	quotes := []domain.Quote{{Side: domain.Buy, Price: num.Must("10"), Quantity: num.Must("2")}, {Side: domain.Sell, Price: num.Must("10"), Quantity: num.Must("2")}}
	eligible, budget := ApplyBalanceBudget(quotes, nil, num.Must("100"), num.Must("1000"), 0, num.Must("1"), num.Must("15"))
	if len(eligible) != 0 || budget.BaseBudget.Cmp(num.Must("1")) != 0 || budget.QuoteBudget.Cmp(num.Must("15")) != 0 {
		t.Fatalf("eligible=%+v budget=%+v", eligible, budget)
	}
}
