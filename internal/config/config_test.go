package config

import (
	"testing"

	"fluxmaker/internal/domain"
	"fluxmaker/internal/num"
)

func minimalConfig() Config {
	address := "0x1111111111111111111111111111111111111111"
	return Config{
		Mode: domain.ModeShadow, PollIntervalMS: 1000,
		RPC: RPCConfig{URLs: []string{"http://rpc.invalid"}, ChainID: 56, RequestTimeoutMS: 1000},
		Instruments: []InstrumentConfig{{
			ID:        "token_usdt",
			Base:      AssetConfig{Symbol: "TOKEN", Address: address, Decimals: 18},
			Quote:     AssetConfig{Symbol: "USDT", Address: "0x2222222222222222222222222222222222222222", Decimals: 18},
			Reference: ReferenceConfig{Type: "pancake_v2", TWAPWindowSeconds: 60, Legs: []PairLegConfig{{PairAddress: address, BaseToken: address, QuoteToken: "0x2222222222222222222222222222222222222222"}}},
			Strategy:  StrategyConfig{Levels: 1, OrderSize: num.Must("1")},
		}},
		Venues: map[string]VenueConfig{
			"binance": {Type: "binance", Enabled: true, BaseURL: "https://api.binance.com", Markets: map[string]VenueMarketConfig{
				"token_usdt": {Symbol: "TOKENUSDT", BaseAsset: "TOKEN", QuoteAsset: "USDT", PriceTick: num.Must("0.01"), QuantityStep: num.Must("0.1"), MinNotional: num.Must("5")},
			}},
			"mgbx": {Type: "mgbx", Enabled: false},
		},
	}
}

func TestPricePathMustBeContinuousAndEndAtInstrumentQuote(t *testing.T) {
	cfg := minimalConfig()
	cfg.Instruments[0].Reference.Legs[0].BaseToken = "0x3333333333333333333333333333333333333333"
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected discontinuous base token to fail")
	}

	cfg = minimalConfig()
	cfg.Instruments[0].Reference.Legs[0].QuoteToken = "0x3333333333333333333333333333333333333333"
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected path ending at the wrong quote token to fail")
	}
}

func TestSingleVenueIsValid(t *testing.T) {
	if err := minimalConfig().Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestStrategySupportsUpToOneHundredLevels(t *testing.T) {
	cfg := minimalConfig()
	cfg.Instruments[0].Strategy.Levels = 100
	cfg.Instruments[0].Strategy.HalfSpreadBPS = 25
	cfg.Instruments[0].Strategy.LevelSpacingBPS = 25
	if err := cfg.Validate(); err != nil {
		t.Fatalf("100 levels should be valid: %v", err)
	}

	cfg.Instruments[0].Strategy.Levels = 101
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected 101 levels to fail")
	}
}

func TestStrategyRejectsOutermostSpreadAtOrAboveOneHundredPercent(t *testing.T) {
	cfg := minimalConfig()
	cfg.Instruments[0].Strategy.Levels = 100
	cfg.Instruments[0].Strategy.HalfSpreadBPS = 100
	cfg.Instruments[0].Strategy.LevelSpacingBPS = 100
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected an invalid outermost spread to fail")
	}
}

func TestStrategyRejectsInvalidBalanceReserve(t *testing.T) {
	cfg := minimalConfig()
	cfg.Instruments[0].Strategy.BalanceReserveBPS = 10_001
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected balance reserve above 100% to fail")
	}
}

func TestStrategyAcceptsQuoteNotionalRangeWithoutLegacyOrderSize(t *testing.T) {
	cfg := minimalConfig()
	cfg.Instruments[0].Strategy.OrderSize = num.Decimal{}
	cfg.Instruments[0].Strategy.MinOrderNotional = num.Must("10")
	cfg.Instruments[0].Strategy.MaxOrderNotional = num.Must("20")
	if err := cfg.Validate(); err != nil {
		t.Fatalf("valid quote notional range should pass: %v", err)
	}
}

func TestStrategyRejectsIncompleteOrReversedQuoteNotionalRange(t *testing.T) {
	cfg := minimalConfig()
	cfg.Instruments[0].Strategy.MinOrderNotional = num.Must("10")
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected incomplete quote notional range to fail")
	}

	cfg = minimalConfig()
	cfg.Instruments[0].Strategy.MinOrderNotional = num.Must("20")
	cfg.Instruments[0].Strategy.MaxOrderNotional = num.Must("10")
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected reversed quote notional range to fail")
	}
}

func TestNormalizeStrategySizingClearsLegacyOrderSizeForNewRange(t *testing.T) {
	cfg := minimalConfig()
	cfg.Instruments[0].Strategy.MinOrderNotional = num.Must("10")
	cfg.Instruments[0].Strategy.MaxOrderNotional = num.Must("20")
	cfg.NormalizeStrategySizing()
	if !cfg.Instruments[0].Strategy.OrderSize.IsZero() {
		t.Fatalf("legacy order size should be cleared, got %s", cfg.Instruments[0].Strategy.OrderSize)
	}

	cfg = minimalConfig()
	cfg.Instruments[0].Strategy.MinOrderNotional = num.Must("10")
	cfg.NormalizeStrategySizing()
	if !cfg.Instruments[0].Strategy.OrderSize.IsPositive() {
		t.Fatal("incomplete range must not clear the legacy order size")
	}
}

func TestMGBXTradingRequiresDedicatedAccount(t *testing.T) {
	cfg := minimalConfig()
	cfg.Venues["binance"] = VenueConfig{Type: "binance", Enabled: false}
	cfg.Venues["mgbx"] = VenueConfig{Type: "mgbx", Enabled: true, TradingEnabled: true, BaseURL: "https://open.mgbx.com", Markets: map[string]VenueMarketConfig{
		"token_usdt": {Symbol: "TOKEN_USDT", BaseAsset: "TOKEN", QuoteAsset: "USDT", PriceTick: num.Must("0.01"), QuantityStep: num.Must("0.1"), MinNotional: num.Must("5")},
	}}
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected dedicated account validation error")
	}
}

func TestTradeSimulationRequiresValidBoundSource(t *testing.T) {
	cfg := minimalConfig()
	cfg.Instruments[0].TradeSimulation = TradeSimulationConfig{
		Enabled: true, SourceVenue: "binance", MinQuantity: num.Must("1"), MaxQuantity: num.Must("2"),
		MinIntervalMS: 100, MaxIntervalMS: 200, BuyProbabilityBPS: 5000, RecentLimit: 50,
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("valid simulation should pass: %v", err)
	}
	cfg.Instruments[0].TradeSimulation.SourceVenue = "missing"
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected missing simulation source venue to fail")
	}
}

func TestBinanceTestnetAndSTPSafety(t *testing.T) {
	cfg := minimalConfig()
	venue := cfg.Venues["binance"]
	venue.Environment = "testnet"
	venue.BaseURL = "https://testnet.binance.vision"
	venue.TradingEnabled = true
	venue.SelfTradePrevention = "EXPIRE_BOTH"
	cfg.Venues["binance"] = venue
	if err := cfg.Validate(); err != nil {
		t.Fatalf("valid Binance testnet configuration should pass: %v", err)
	}
	venue.SelfTradePrevention = "NONE"
	cfg.Venues["binance"] = venue
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected disabled STP to fail")
	}
	venue.SelfTradePrevention = "EXPIRE_BOTH"
	venue.BaseURL = "https://api.binance.com"
	cfg.Venues["binance"] = venue
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected production URL in testnet mode to fail")
	}
}

func TestConcurrentInstrumentLimit(t *testing.T) {
	cfg := minimalConfig()
	cfg.MaxConcurrentInstruments = 32
	if err := cfg.Validate(); err != nil {
		t.Fatalf("32 workers should be valid: %v", err)
	}
	cfg.MaxConcurrentInstruments = 33
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected more than 32 workers to fail")
	}
}

func TestAuditRotationValidation(t *testing.T) {
	cfg := minimalConfig()
	cfg.AuditMaxBytes = 1024 * 1024
	cfg.AuditBackups = 7
	if err := cfg.Validate(); err != nil {
		t.Fatalf("valid audit rotation should pass: %v", err)
	}
	cfg.AuditMaxBytes = 1024
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected undersized audit rotation to fail")
	}
}

func TestNormalizeStrategyAddsQuoteRefreshDefaults(t *testing.T) {
	cfg := minimalConfig()
	cfg.NormalizeStrategySizing()
	strategy := cfg.Instruments[0].Strategy
	if strategy.QuoteRefreshSeconds != DefaultQuoteRefreshSeconds || strategy.QuoteRefreshRatioBPS != DefaultQuoteRefreshRatioBPS {
		t.Fatalf("refresh defaults=%+v", strategy)
	}
	if strategy.MinOrderLifetimeSeconds != DefaultMinOrderLifetimeSeconds || strategy.MaxOrderLifetimeSeconds != DefaultMaxOrderLifetimeSeconds {
		t.Fatalf("lifetime defaults=%+v", strategy)
	}
	if strategy.PriceJitterTicks != DefaultPriceJitterTicks || strategy.BestLevelRefreshSeconds != DefaultBestRefreshSeconds {
		t.Fatalf("jitter/best defaults=%+v", strategy)
	}
}

func TestQuoteRefreshValidation(t *testing.T) {
	cfg := minimalConfig()
	cfg.NormalizeStrategySizing()
	cfg.Instruments[0].Strategy.QuoteRefreshSeconds = 9
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected too-fast quote refresh to fail")
	}
	cfg.Instruments[0].Strategy.QuoteRefreshSeconds = 45
	cfg.Instruments[0].Strategy.MaxOrderLifetimeSeconds = 20
	cfg.Instruments[0].Strategy.MinOrderLifetimeSeconds = 30
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected maximum lifetime below minimum to fail")
	}
}
