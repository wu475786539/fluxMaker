package oms

import (
	"context"
	"testing"

	"fluxmaker/internal/domain"
	"fluxmaker/internal/num"
)

// withinBPSDivision is the original division-based formulation. The optimized
// withinBPS cross-multiplies to avoid the division; this reference keeps the two
// provably identical, including at the exact tolerance boundary.
func withinBPSDivision(a, b num.Decimal, threshold int) bool {
	if a.IsZero() || b.IsZero() {
		return false
	}
	diff := a.Sub(b).Abs().Div(b).Mul(num.TenThousand())
	return diff.Cmp(num.FromInt64(int64(threshold))) <= 0
}

func TestWithinBPSMatchesDivisionForm(t *testing.T) {
	prices := []string{"0.0001", "1", "99", "100", "100.5", "100.50000001", "101", "12345.678", "0.987654321"}
	thresholds := []int{0, 1, 5, 25, 50, 100, 1000}
	for _, aStr := range prices {
		for _, bStr := range prices {
			for _, threshold := range thresholds {
				a := num.Must(aStr)
				b := num.Must(bStr)
				got := withinBPS(a, b, threshold)
				want := withinBPSDivision(a, b, threshold)
				if got != want {
					t.Fatalf("withinBPS(%s,%s,%d)=%v, division form=%v", aStr, bStr, threshold, got, want)
				}
			}
		}
	}
	// Exact boundary: b=100, threshold=50bps => tolerance 0.5, so a=100.5 is
	// inclusive and a hair above is excluded, matching <= semantics.
	if !withinBPS(num.Must("100.5"), num.Must("100"), 50) {
		t.Fatal("expected exact 50bps boundary to be within tolerance")
	}
	if withinBPS(num.Must("100.50000001"), num.Must("100"), 50) {
		t.Fatal("expected a price past the 50bps boundary to be rejected")
	}
}

// TestReconcileKeepRequiresExactQuantity guards the reordered match condition:
// a managed order at an acceptable price but the wrong remaining size must not
// be treated as a match. It has to be canceled, not kept.
func TestReconcileKeepRequiresExactQuantity(t *testing.T) {
	ctx := context.Background()
	r := New()
	v := &fakeVenue{orders: []domain.Order{
		{Venue: "fake", OrderID: "b", ClientID: "fm-b", Symbol: "TOKENUSDT", Side: domain.Buy, Price: num.Must("99"), Quantity: num.Must("1"), State: domain.OrderNew},
		{Venue: "fake", OrderID: "s", ClientID: "fm-s", Symbol: "TOKENUSDT", Side: domain.Sell, Price: num.Must("101"), Quantity: num.Must("2"), State: domain.OrderNew},
	}}
	result, err := r.Reconcile(ctx, v, "token_usdt", target("99", "101"), 0)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	// Buy order matches its quote exactly; the sell order's size (2) differs from
	// the sell quote's size (1), so it must be canceled rather than kept.
	if result.Kept != 1 {
		t.Fatalf("expected exactly the correctly-sized order kept, got Kept=%d", result.Kept)
	}
	if result.Canceled != 1 {
		t.Fatalf("expected the wrong-sized order canceled, got Canceled=%d", result.Canceled)
	}
}
