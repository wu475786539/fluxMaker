package risk

import (
	"testing"

	"fluxmaker/internal/config"
	"fluxmaker/internal/domain"
	"fluxmaker/internal/num"
)

func TestMarketReferenceProtection(t *testing.T) {
	ref := domain.ReferencePrice{Price: num.Must("100")}
	strategy := config.StrategyConfig{MaxVenueReferenceDeviationBPS: 100, MaxVenueSpreadBPS: 100}
	if err := ValidateMarketReference(ref, domain.Book{BidPrice: num.Must("99.9"), AskPrice: num.Must("100.1")}, strategy); err != nil {
		t.Fatal(err)
	}
	if err := ValidateMarketReference(ref, domain.Book{BidPrice: num.Must("109"), AskPrice: num.Must("111")}, strategy); err == nil {
		t.Fatal("expected venue/reference deviation rejection")
	}
	if err := ValidateMarketReference(ref, domain.Book{BidPrice: num.Must("99"), AskPrice: num.Must("101")}, strategy); err == nil {
		t.Fatal("expected venue spread rejection")
	}
}
