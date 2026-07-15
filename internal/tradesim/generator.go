package tradesim

import (
	"crypto/rand"
	"fmt"
	"math/big"
	"sync"
	"time"

	"fluxmaker/internal/config"
	"fluxmaker/internal/domain"
	"fluxmaker/internal/num"
)

type Snapshot struct {
	Enabled         bool          `json:"enabled"`
	SourceVenue     string        `json:"source_venue"`
	Status          string        `json:"status"`
	LastGeneratedAt time.Time     `json:"last_generated_at,omitempty"`
	NextAt          time.Time     `json:"next_at,omitempty"`
	Fills           []domain.Fill `json:"fills"`
	Error           string        `json:"error,omitempty"`
}

type state struct {
	nextAt          time.Time
	lastGeneratedAt time.Time
	fills           []domain.Fill
	sequence        uint64
}

type Generator struct {
	mu     sync.Mutex
	states map[string]*state
}

func New() *Generator { return &Generator{states: make(map[string]*state)} }

// Observe may emit at most one internal synthetic fill. It consumes only a
// public book snapshot and never has access to a venue order-placement client.
func (g *Generator) Observe(instrument config.InstrumentConfig, venueName string, market config.VenueMarketConfig, book domain.Book, now time.Time) (Snapshot, *domain.Fill) {
	cfg := instrument.TradeSimulation
	snapshot := Snapshot{Enabled: cfg.Enabled, SourceVenue: cfg.SourceVenue, Status: "disabled", Fills: []domain.Fill{}}
	if !cfg.Enabled || venueName != cfg.SourceVenue {
		return snapshot, nil
	}

	g.mu.Lock()
	defer g.mu.Unlock()
	current := g.states[instrument.ID]
	if current == nil {
		current = &state{}
		g.states[instrument.ID] = current
	}
	if current.nextAt.IsZero() {
		current.nextAt = now.Add(randomDuration(cfg.MinIntervalMS, cfg.MaxIntervalMS))
	}
	snapshot.Status = "waiting"
	if !now.Before(current.nextAt) {
		fill, err := generateFill(instrument, venueName, market, book, now, current.sequence+1)
		current.nextAt = now.Add(randomDuration(cfg.MinIntervalMS, cfg.MaxIntervalMS))
		if err != nil {
			snapshot.Status = "skipped"
			snapshot.Error = err.Error()
		} else {
			current.sequence++
			current.lastGeneratedAt = now
			current.fills = append([]domain.Fill{fill}, current.fills...)
			if len(current.fills) > cfg.RecentLimit {
				current.fills = current.fills[:cfg.RecentLimit]
			}
			snapshot.Status = "running"
			snapshot.LastGeneratedAt = current.lastGeneratedAt
			snapshot.NextAt = current.nextAt
			snapshot.Fills = append([]domain.Fill(nil), current.fills...)
			return snapshot, &fill
		}
	}
	snapshot.LastGeneratedAt = current.lastGeneratedAt
	snapshot.NextAt = current.nextAt
	snapshot.Fills = append([]domain.Fill(nil), current.fills...)
	return snapshot, nil
}

func generateFill(instrument config.InstrumentConfig, venueName string, market config.VenueMarketConfig, book domain.Book, now time.Time, sequence uint64) (domain.Fill, error) {
	firstPrice := book.BidPrice.QuantizeDown(market.PriceTick).Add(market.PriceTick)
	lastPrice := book.AskPrice.QuantizeUp(market.PriceTick).Sub(market.PriceTick)
	if firstPrice.Cmp(book.BidPrice) <= 0 || lastPrice.Cmp(book.AskPrice) >= 0 || firstPrice.Cmp(lastPrice) > 0 {
		return domain.Fill{}, fmt.Errorf("no price tick exists strictly inside bid/ask")
	}
	price, err := randomStep(firstPrice, lastPrice, market.PriceTick)
	if err != nil {
		return domain.Fill{}, err
	}

	cfg := instrument.TradeSimulation
	minQuantity := cfg.MinQuantity.QuantizeUp(market.QuantityStep)
	if market.MinQuantity.IsPositive() {
		minQuantity = minQuantity.Max(market.MinQuantity.QuantizeUp(market.QuantityStep))
	}
	if market.MinNotional.IsPositive() {
		minQuantity = minQuantity.Max(market.MinNotional.Div(price).QuantizeUp(market.QuantityStep))
	}
	maxQuantity := cfg.MaxQuantity.QuantizeDown(market.QuantityStep)
	if market.MaxQuantity.IsPositive() {
		maxQuantity = maxQuantity.Min(market.MaxQuantity.QuantizeDown(market.QuantityStep))
	}
	if minQuantity.Cmp(maxQuantity) > 0 {
		return domain.Fill{}, fmt.Errorf("configured quantity range cannot satisfy exchange minimums")
	}
	quantity, err := randomStep(minQuantity, maxQuantity, market.QuantityStep)
	if err != nil {
		return domain.Fill{}, err
	}
	side := domain.Sell
	if randomBelow(10_000) < int64(cfg.BuyProbabilityBPS) {
		side = domain.Buy
	}
	return domain.Fill{
		Venue: venueName, TradeID: fmt.Sprintf("SIM-%s-%d-%d", instrument.ID, now.UnixMilli(), sequence),
		Symbol: market.Symbol, Side: side, Price: price, Quantity: quantity,
		QuoteQuantity: price.Mul(quantity), Maker: false, Simulated: true, Timestamp: now.UTC(),
	}, nil
}

func randomStep(minimum, maximum, step num.Decimal) (num.Decimal, error) {
	delta := maximum.Sub(minimum)
	if delta.Sign() < 0 {
		return num.Decimal{}, fmt.Errorf("invalid random range")
	}
	quotient := new(big.Rat).Quo(delta.Rat(), step.Rat())
	steps := new(big.Int).Quo(quotient.Num(), quotient.Denom())
	steps.Add(steps, big.NewInt(1))
	choice, err := rand.Int(rand.Reader, steps)
	if err != nil {
		return num.Decimal{}, fmt.Errorf("secure random: %w", err)
	}
	return minimum.Add(step.Mul(num.FromRat(new(big.Rat).SetInt(choice)))), nil
}

func randomDuration(minMS, maxMS int) time.Duration {
	if maxMS <= minMS {
		return time.Duration(minMS) * time.Millisecond
	}
	return time.Duration(int64(minMS)+randomBelow(int64(maxMS-minMS+1))) * time.Millisecond
}

func randomBelow(max int64) int64 {
	if max <= 1 {
		return 0
	}
	value, err := rand.Int(rand.Reader, big.NewInt(max))
	if err != nil {
		return 0
	}
	return value.Int64()
}
