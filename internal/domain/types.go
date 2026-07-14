package domain

import (
	"time"

	"fluxmaker/internal/num"
)

type Side string

const (
	Buy  Side = "BUY"
	Sell Side = "SELL"
)

type Mode string

const (
	ModeShadow Mode = "shadow"
	ModeLive   Mode = "live"
)

type Book struct {
	Venue     string      `json:"venue"`
	Symbol    string      `json:"symbol"`
	BidPrice  num.Decimal `json:"bid_price"`
	BidQty    num.Decimal `json:"bid_qty"`
	AskPrice  num.Decimal `json:"ask_price"`
	AskQty    num.Decimal `json:"ask_qty"`
	Timestamp time.Time   `json:"timestamp"`
}

// HasBid and HasAsk distinguish a reachable but empty/one-sided market from a
// transport failure. A market maker must be able to seed an empty book from its
// external reference price; existing sides are only used to keep Post-Only
// quotes from taking liquidity.
func (b Book) HasBid() bool { return b.BidPrice.IsPositive() }
func (b Book) HasAsk() bool { return b.AskPrice.IsPositive() }
func (b Book) HasPrices() bool {
	return b.HasBid() || b.HasAsk()
}
func (b Book) TwoSided() bool { return b.HasBid() && b.HasAsk() }

type ReferencePrice struct {
	InstrumentID string      `json:"instrument_id"`
	Price        num.Decimal `json:"price"`
	Spot         num.Decimal `json:"spot"`
	TWAPReady    bool        `json:"twap_ready"`
	Confidence   string      `json:"confidence"`
	BlockNumber  uint64      `json:"block_number"`
	BlockTime    time.Time   `json:"block_time"`
	ValidUntil   time.Time   `json:"valid_until"`
}

type Quote struct {
	InstrumentID string      `json:"instrument_id"`
	Venue        string      `json:"venue"`
	Symbol       string      `json:"symbol"`
	Side         Side        `json:"side"`
	Level        int         `json:"level"`
	Price        num.Decimal `json:"price"`
	Quantity     num.Decimal `json:"quantity"`
	Reference    num.Decimal `json:"reference"`
	ValidUntil   time.Time   `json:"valid_until"`
}

type OrderState string

const (
	OrderNew             OrderState = "NEW"
	OrderPartiallyFilled OrderState = "PARTIALLY_FILLED"
	OrderFilled          OrderState = "FILLED"
	OrderCanceled        OrderState = "CANCELED"
	OrderRejected        OrderState = "REJECTED"
	OrderExpired         OrderState = "EXPIRED"
	OrderUnknown         OrderState = "UNKNOWN"
)

type Order struct {
	Venue       string      `json:"venue"`
	OrderID     string      `json:"order_id"`
	ClientID    string      `json:"client_id,omitempty"`
	Symbol      string      `json:"symbol"`
	Side        Side        `json:"side"`
	Price       num.Decimal `json:"price"`
	Quantity    num.Decimal `json:"quantity"`
	ExecutedQty num.Decimal `json:"executed_qty"`
	State       OrderState  `json:"state"`
	CreatedAt   time.Time   `json:"created_at"`
}

type Balance struct {
	Asset  string      `json:"asset"`
	Free   num.Decimal `json:"free"`
	Locked num.Decimal `json:"locked"`
}

type QuoteBudget struct {
	ReserveBPS     int         `json:"reserve_bps"`
	TargetOrders   int         `json:"target_orders"`
	EligibleOrders int         `json:"eligible_orders"`
	BaseBudget     num.Decimal `json:"base_budget"`
	BaseRequired   num.Decimal `json:"base_required"`
	QuoteBudget    num.Decimal `json:"quote_budget"`
	QuoteRequired  num.Decimal `json:"quote_required"`
	BaseLimited    bool        `json:"base_limited"`
	QuoteLimited   bool        `json:"quote_limited"`
}

type MarketRules struct {
	Symbol        string      `json:"symbol"`
	BaseAsset     string      `json:"base_asset"`
	QuoteAsset    string      `json:"quote_asset"`
	PriceTick     num.Decimal `json:"price_tick"`
	QuantityStep  num.Decimal `json:"quantity_step"`
	MinQuantity   num.Decimal `json:"min_quantity"`
	MaxQuantity   num.Decimal `json:"max_quantity"`
	MinNotional   num.Decimal `json:"min_notional"`
	MaxNotional   num.Decimal `json:"max_notional"`
	MinPrice      num.Decimal `json:"min_price"`
	MaxPrice      num.Decimal `json:"max_price"`
	MaxOpenOrders int         `json:"max_open_orders"`
}

type Fill struct {
	Venue         string      `json:"venue"`
	TradeID       string      `json:"trade_id"`
	OrderID       string      `json:"order_id"`
	Symbol        string      `json:"symbol"`
	Side          Side        `json:"side"`
	Price         num.Decimal `json:"price"`
	Quantity      num.Decimal `json:"quantity"`
	QuoteQuantity num.Decimal `json:"quote_quantity"`
	Fee           num.Decimal `json:"fee"`
	FeeAsset      string      `json:"fee_asset,omitempty"`
	Maker         bool        `json:"maker"`
	Aggregate     bool        `json:"aggregate,omitempty"`
	Simulated     bool        `json:"simulated,omitempty"`
	Timestamp     time.Time   `json:"timestamp"`
}
