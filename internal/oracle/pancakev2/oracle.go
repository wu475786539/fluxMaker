package pancakev2

import (
	"context"
	"encoding/hex"
	"fmt"
	"math/big"
	"strings"
	"sync"
	"time"

	"fluxmaker/internal/config"
	"fluxmaker/internal/domain"
	"fluxmaker/internal/num"
)

const (
	selectorGetReserves          = "0x0902f1ac"
	selectorToken0               = "0x0dfe1681"
	selectorToken1               = "0xd21220a7"
	selectorDecimals             = "0x313ce567"
	selectorFactory              = "0xc45a0155"
	selectorPrice0CumulativeLast = "0x5909c0d5"
	selectorPrice1CumulativeLast = "0x5a3d5493"
)

var q112 = new(big.Int).Lsh(big.NewInt(1), 112)

type Oracle struct {
	rpc       *RPCClient
	mu        sync.Mutex
	metadata  map[string]pairMetadata
	snapshots map[string]cumulativeSnapshot
}

type pairMetadata struct {
	Token0    string
	Token1    string
	Decimals0 int
	Decimals1 int
	Factory   string
}

type cumulativeSnapshot struct {
	Price0   *big.Int
	Price1   *big.Int
	UnixTime uint64
	TWAP0    num.Decimal
	TWAP1    num.Decimal
	Ready    bool
}

type pairState struct {
	meta               pairMetadata
	reserve0           *big.Int
	reserve1           *big.Int
	reserveTimestamp   uint32
	currentCumulative0 *big.Int
	currentCumulative1 *big.Int
	block              Block
}

type legPrice struct {
	Spot  num.Decimal
	TWAP  num.Decimal
	Ready bool
}

func New(rpc *RPCClient) *Oracle {
	return &Oracle{rpc: rpc, metadata: map[string]pairMetadata{}, snapshots: map[string]cumulativeSnapshot{}}
}

func (o *Oracle) Price(ctx context.Context, in config.InstrumentConfig) (domain.ReferencePrice, error) {
	if in.Reference.Type != "pancake_v2" {
		return domain.ReferencePrice{}, fmt.Errorf("unsupported oracle type %s", in.Reference.Type)
	}
	spot := num.One()
	twap := num.One()
	allReady := true
	block, err := o.rpc.LatestBlock(ctx)
	if err != nil {
		return domain.ReferencePrice{}, err
	}
	for _, leg := range in.Reference.Legs {
		p, err := o.readLeg(ctx, leg, in.Reference.TWAPWindowSeconds, block)
		if err != nil {
			return domain.ReferencePrice{}, fmt.Errorf("pair %s: %w", leg.PairAddress, err)
		}
		spot = spot.Mul(p.Spot)
		if p.Ready {
			twap = twap.Mul(p.TWAP)
		} else {
			allReady = false
		}
	}

	price := twap
	confidence := "high"
	if !allReady {
		if !in.Reference.AllowSpotDuringWarmup {
			return domain.ReferencePrice{}, fmt.Errorf("TWAP warming up")
		}
		price = spot
		confidence = "warming_up"
	} else if in.Reference.MaxSpotTWAPDeviationBPS > 0 {
		deviation := spot.Sub(twap).Abs().Div(twap).Mul(num.TenThousand())
		limit := num.FromInt64(int64(in.Reference.MaxSpotTWAPDeviationBPS))
		if deviation.Cmp(limit) > 0 {
			return domain.ReferencePrice{}, fmt.Errorf("spot/TWAP deviation %s bps exceeds %d", deviation.String(), in.Reference.MaxSpotTWAPDeviationBPS)
		}
	}

	blockTime := time.Unix(int64(block.Timestamp), 0).UTC()
	if blockTime.After(time.Now().UTC().Add(15 * time.Second)) {
		return domain.ReferencePrice{}, fmt.Errorf("chain block timestamp is in the future: %s", blockTime)
	}
	if in.Reference.StaleAfterSeconds > 0 && time.Since(blockTime) > time.Duration(in.Reference.StaleAfterSeconds)*time.Second {
		return domain.ReferencePrice{}, fmt.Errorf("chain block is stale: %s", blockTime)
	}
	validFor := time.Duration(in.Reference.StaleAfterSeconds) * time.Second
	if validFor <= 0 {
		validFor = 15 * time.Second
	}
	return domain.ReferencePrice{
		InstrumentID: in.ID,
		Price:        price,
		Spot:         spot,
		TWAPReady:    allReady,
		Confidence:   confidence,
		BlockNumber:  block.Number,
		BlockTime:    blockTime,
		ValidUntil:   time.Now().UTC().Add(validFor),
	}, nil
}

func (o *Oracle) readLeg(ctx context.Context, leg config.PairLegConfig, windowSeconds int, block Block) (legPrice, error) {
	state, err := o.readPair(ctx, leg.PairAddress, block)
	if err != nil {
		return legPrice{}, err
	}
	if leg.ExpectedFactory != "" && normalizeAddress(leg.ExpectedFactory) != state.meta.Factory {
		return legPrice{}, fmt.Errorf("pair factory %s does not match expected %s", state.meta.Factory, normalizeAddress(leg.ExpectedFactory))
	}
	base := normalizeAddress(leg.BaseToken)
	quote := normalizeAddress(leg.QuoteToken)
	var reserveBase, reserveQuote *big.Int
	var baseDecimals, quoteDecimals int
	if base == state.meta.Token0 && quote == state.meta.Token1 {
		reserveBase, reserveQuote = state.reserve0, state.reserve1
		baseDecimals, quoteDecimals = state.meta.Decimals0, state.meta.Decimals1
	} else if base == state.meta.Token1 && quote == state.meta.Token0 {
		reserveBase, reserveQuote = state.reserve1, state.reserve0
		baseDecimals, quoteDecimals = state.meta.Decimals1, state.meta.Decimals0
	} else {
		return legPrice{}, fmt.Errorf("configured base/quote tokens do not match pair")
	}
	if reserveBase.Sign() <= 0 || reserveQuote.Sign() <= 0 {
		return legPrice{}, fmt.Errorf("empty reserves")
	}
	if leg.MaxIdleSeconds > 0 {
		idle := uint32(state.block.Timestamp) - state.reserveTimestamp
		if idle > uint32(leg.MaxIdleSeconds) {
			return legPrice{}, fmt.Errorf("pair has not updated for %d seconds", idle)
		}
	}
	if leg.MinQuoteReserve.IsPositive() {
		quoteUnits := num.FromRat(new(big.Rat).SetFrac(reserveQuote, pow10(quoteDecimals)))
		if quoteUnits.Cmp(leg.MinQuoteReserve) < 0 {
			return legPrice{}, fmt.Errorf("quote reserve %s below minimum %s", quoteUnits.String(), leg.MinQuoteReserve.String())
		}
	}
	adjust := decimalAdjustment(baseDecimals, quoteDecimals)
	spotRat := new(big.Rat).Mul(new(big.Rat).SetFrac(reserveQuote, reserveBase), adjust)
	result := legPrice{Spot: num.FromRat(spotRat)}

	o.mu.Lock()
	defer o.mu.Unlock()
	key := normalizeAddress(leg.PairAddress)
	previous, ok := o.snapshots[key]
	if !ok || state.block.Timestamp <= previous.UnixTime {
		o.snapshots[key] = cumulativeSnapshot{Price0: new(big.Int).Set(state.currentCumulative0), Price1: new(big.Int).Set(state.currentCumulative1), UnixTime: state.block.Timestamp}
		return result, nil
	}
	elapsed := state.block.Timestamp - previous.UnixTime
	if elapsed < uint64(windowSeconds) {
		if previous.Ready {
			if base == state.meta.Token0 {
				result.TWAP = previous.TWAP0
			} else {
				result.TWAP = previous.TWAP1
			}
			result.Ready = true
		}
		return result, nil
	}
	delta0 := new(big.Int).Sub(state.currentCumulative0, previous.Price0)
	delta1 := new(big.Int).Sub(state.currentCumulative1, previous.Price1)
	if delta0.Sign() <= 0 || delta1.Sign() <= 0 {
		return legPrice{}, fmt.Errorf("non-positive cumulative price delta")
	}
	denominator := new(big.Int).Mul(new(big.Int).SetUint64(elapsed), q112)
	rawTWAP0 := new(big.Rat).SetFrac(delta0, denominator)
	rawTWAP1 := new(big.Rat).SetFrac(delta1, denominator)
	twap0 := num.FromRat(new(big.Rat).Mul(rawTWAP0, decimalAdjustment(state.meta.Decimals0, state.meta.Decimals1)))
	twap1 := num.FromRat(new(big.Rat).Mul(rawTWAP1, decimalAdjustment(state.meta.Decimals1, state.meta.Decimals0)))
	if base == state.meta.Token0 {
		result.TWAP = twap0
	} else {
		result.TWAP = twap1
	}
	result.Ready = true
	o.snapshots[key] = cumulativeSnapshot{Price0: new(big.Int).Set(state.currentCumulative0), Price1: new(big.Int).Set(state.currentCumulative1), UnixTime: state.block.Timestamp, TWAP0: twap0, TWAP1: twap1, Ready: true}
	return result, nil
}

func (o *Oracle) readPair(ctx context.Context, pair string, block Block) (pairState, error) {
	pair = normalizeAddress(pair)
	if !validAddress(pair) {
		return pairState{}, fmt.Errorf("invalid pair address")
	}
	meta, err := o.getMetadata(ctx, pair, block.Tag)
	if err != nil {
		return pairState{}, err
	}
	// getReserves, price0CumulativeLast and price1CumulativeLast are independent
	// reads at the same block; batch them into a single round-trip.
	words, err := o.rpc.BatchCall(ctx, []BatchCall{
		{To: pair, Data: selectorGetReserves},
		{To: pair, Data: selectorPrice0CumulativeLast},
		{To: pair, Data: selectorPrice1CumulativeLast},
	}, block.Tag)
	if err != nil {
		return pairState{}, fmt.Errorf("read pair state: %w", err)
	}
	reservesRaw, cum0Raw, cum1Raw := words[0], words[1], words[2]
	if len(reservesRaw) < 96 {
		return pairState{}, fmt.Errorf("short getReserves response")
	}
	reserve0 := wordInt(reservesRaw, 0)
	reserve1 := wordInt(reservesRaw, 1)
	reserveTS := uint32(wordInt(reservesRaw, 2).Uint64())
	cum0, cum1 := wordInt(cum0Raw, 0), wordInt(cum1Raw, 0)
	elapsed := uint32(block.Timestamp) - reserveTS
	if elapsed > 0 && reserve0.Sign() > 0 && reserve1.Sign() > 0 {
		price0 := new(big.Int).Quo(new(big.Int).Lsh(new(big.Int).Set(reserve1), 112), reserve0)
		price1 := new(big.Int).Quo(new(big.Int).Lsh(new(big.Int).Set(reserve0), 112), reserve1)
		cum0.Add(cum0, new(big.Int).Mul(price0, new(big.Int).SetUint64(uint64(elapsed))))
		cum1.Add(cum1, new(big.Int).Mul(price1, new(big.Int).SetUint64(uint64(elapsed))))
	}
	return pairState{meta: meta, reserve0: reserve0, reserve1: reserve1, reserveTimestamp: reserveTS, currentCumulative0: cum0, currentCumulative1: cum1, block: block}, nil
}

func (o *Oracle) getMetadata(ctx context.Context, pair, blockTag string) (pairMetadata, error) {
	o.mu.Lock()
	meta, ok := o.metadata[pair]
	o.mu.Unlock()
	if ok {
		return meta, nil
	}
	t0raw, err := o.rpc.CallAt(ctx, pair, selectorToken0, blockTag)
	if err != nil {
		return pairMetadata{}, fmt.Errorf("token0: %w", err)
	}
	t1raw, err := o.rpc.CallAt(ctx, pair, selectorToken1, blockTag)
	if err != nil {
		return pairMetadata{}, fmt.Errorf("token1: %w", err)
	}
	t0, err := wordAddress(t0raw)
	if err != nil {
		return pairMetadata{}, err
	}
	t1, err := wordAddress(t1raw)
	if err != nil {
		return pairMetadata{}, err
	}
	d0raw, err := o.rpc.CallAt(ctx, t0, selectorDecimals, blockTag)
	if err != nil {
		return pairMetadata{}, fmt.Errorf("token0 decimals: %w", err)
	}
	d1raw, err := o.rpc.CallAt(ctx, t1, selectorDecimals, blockTag)
	if err != nil {
		return pairMetadata{}, fmt.Errorf("token1 decimals: %w", err)
	}
	factoryRaw, err := o.rpc.CallAt(ctx, pair, selectorFactory, blockTag)
	if err != nil {
		return pairMetadata{}, fmt.Errorf("factory: %w", err)
	}
	factory, err := wordAddress(factoryRaw)
	if err != nil {
		return pairMetadata{}, err
	}
	meta = pairMetadata{Token0: t0, Token1: t1, Decimals0: int(wordInt(d0raw, 0).Int64()), Decimals1: int(wordInt(d1raw, 0).Int64()), Factory: factory}
	o.mu.Lock()
	o.metadata[pair] = meta
	o.mu.Unlock()
	return meta, nil
}

func wordInt(data []byte, index int) *big.Int {
	start := index * 32
	if len(data) < start+32 {
		return new(big.Int)
	}
	return new(big.Int).SetBytes(data[start : start+32])
}

func wordAddress(data []byte) (string, error) {
	if len(data) < 32 {
		return "", fmt.Errorf("short address response")
	}
	return "0x" + hex.EncodeToString(data[12:32]), nil
}

func normalizeAddress(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	if !strings.HasPrefix(s, "0x") {
		s = "0x" + s
	}
	return s
}

func validAddress(s string) bool {
	if len(s) != 42 || !strings.HasPrefix(s, "0x") {
		return false
	}
	_, err := hex.DecodeString(s[2:])
	return err == nil
}

func decimalAdjustment(baseDecimals, quoteDecimals int) *big.Rat {
	numerator := pow10(baseDecimals)
	denominator := pow10(quoteDecimals)
	return new(big.Rat).SetFrac(numerator, denominator)
}

func pow10(decimals int) *big.Int {
	return new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(decimals)), nil)
}
