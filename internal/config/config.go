package config

import (
	"fmt"
	"net/url"
	"strings"
	"time"

	"fluxmaker/internal/domain"
	"fluxmaker/internal/num"
	"fluxmaker/internal/venue"
)

type Config struct {
	Mode                          domain.Mode            `json:"mode"`
	PollIntervalMS                int                    `json:"poll_interval_ms"`
	AuditPath                     string                 `json:"audit_path"`
	AuditMaxBytes                 int64                  `json:"audit_max_bytes"`
	AuditBackups                  int                    `json:"audit_backups"`
	HeartbeatPath                 string                 `json:"heartbeat_path"`
	WatchdogTimeoutSeconds        int                    `json:"watchdog_timeout_seconds"`
	MarketFailureThreshold        int                    `json:"market_failure_threshold"`
	MarketRecoveryThreshold       int                    `json:"market_recovery_threshold"`
	MarketErrorGraceSeconds       int                    `json:"market_error_grace_seconds"`
	TradingProgressTimeoutSeconds int                    `json:"trading_progress_timeout_seconds"`
	MaxConcurrentInstruments      int                    `json:"max_concurrent_instruments"`
	RulesRefreshSeconds           int                    `json:"rules_refresh_seconds"`
	RPC                           RPCConfig              `json:"rpc"`
	Instruments                   []InstrumentConfig     `json:"instruments"`
	Venues                        map[string]VenueConfig `json:"venues"`
}

type RPCConfig struct {
	URLs             []string `json:"urls"`
	ChainID          uint64   `json:"chain_id"`
	RequestTimeoutMS int      `json:"request_timeout_ms"`
}

type AssetConfig struct {
	Symbol   string `json:"symbol"`
	Address  string `json:"address,omitempty"`
	Decimals int    `json:"decimals"`
}

type ReferenceConfig struct {
	Type                    string          `json:"type"`
	Legs                    []PairLegConfig `json:"legs"`
	TWAPWindowSeconds       int             `json:"twap_window_seconds"`
	MaxSpotTWAPDeviationBPS int             `json:"max_spot_twap_deviation_bps"`
	StaleAfterSeconds       int             `json:"stale_after_seconds"`
	AllowSpotDuringWarmup   bool            `json:"allow_spot_during_warmup"`
}

type PairLegConfig struct {
	PairAddress     string      `json:"pair_address"`
	ExpectedFactory string      `json:"expected_factory,omitempty"`
	BaseToken       string      `json:"base_token"`
	QuoteToken      string      `json:"quote_token"`
	MinQuoteReserve num.Decimal `json:"min_quote_reserve"`
	MaxIdleSeconds  int         `json:"max_idle_seconds"`
}

type StrategyConfig struct {
	HalfSpreadBPS   int `json:"half_spread_bps"`
	LevelSpacingBPS int `json:"level_spacing_bps"`
	Levels          int `json:"levels"`
	// OrderSize is retained for published configurations created before quote
	// notional ranges were introduced. New configurations should use
	// MinOrderNotional and MaxOrderNotional instead.
	OrderSize                     num.Decimal `json:"order_size,omitempty"`
	MinOrderNotional              num.Decimal `json:"min_order_notional,omitempty"`
	MaxOrderNotional              num.Decimal `json:"max_order_notional,omitempty"`
	RepriceThresholdBPS           int         `json:"reprice_threshold_bps"`
	BalanceReserveBPS             int         `json:"balance_reserve_bps"`
	MaxVenueReferenceDeviationBPS int         `json:"max_venue_reference_deviation_bps"`
	MaxVenueSpreadBPS             int         `json:"max_venue_spread_bps"`
	TargetBase                    num.Decimal `json:"target_base"`
	MaxBaseDeviation              num.Decimal `json:"max_base_deviation"`
	InventorySkewBPS              int         `json:"inventory_skew_bps"`
	QuoteRefreshSeconds           int         `json:"quote_refresh_seconds,omitempty"`
	QuoteRefreshRatioBPS          int         `json:"quote_refresh_ratio_bps,omitempty"`
	MinOrderLifetimeSeconds       int         `json:"min_order_lifetime_seconds,omitempty"`
	MaxOrderLifetimeSeconds       int         `json:"max_order_lifetime_seconds,omitempty"`
	PriceJitterTicks              int         `json:"price_jitter_ticks,omitempty"`
	BestLevels                    int         `json:"best_levels,omitempty"`
	BestLevelRefreshSeconds       int         `json:"best_level_refresh_seconds,omitempty"`
}

const (
	DefaultQuoteRefreshSeconds     = 45
	DefaultQuoteRefreshRatioBPS    = 1_000
	DefaultMinOrderLifetimeSeconds = 30
	DefaultMaxOrderLifetimeSeconds = 300
	DefaultPriceJitterTicks        = 2
	DefaultBestLevels              = 3
	DefaultBestRefreshSeconds      = 90
)

func (s StrategyConfig) EffectiveQuoteRefreshSeconds() int {
	if s.QuoteRefreshSeconds > 0 {
		return s.QuoteRefreshSeconds
	}
	return DefaultQuoteRefreshSeconds
}

func (s StrategyConfig) EffectiveQuoteRefreshRatioBPS() int {
	if s.QuoteRefreshRatioBPS > 0 {
		return s.QuoteRefreshRatioBPS
	}
	return DefaultQuoteRefreshRatioBPS
}

func (s StrategyConfig) EffectiveMinOrderLifetime() time.Duration {
	seconds := s.MinOrderLifetimeSeconds
	if seconds <= 0 {
		seconds = DefaultMinOrderLifetimeSeconds
	}
	return time.Duration(seconds) * time.Second
}

func (s StrategyConfig) EffectiveMaxOrderLifetime() time.Duration {
	seconds := s.MaxOrderLifetimeSeconds
	if seconds <= 0 {
		seconds = DefaultMaxOrderLifetimeSeconds
	}
	return time.Duration(seconds) * time.Second
}

func (s StrategyConfig) EffectivePriceJitterTicks() int {
	if s.PriceJitterTicks > 0 {
		return s.PriceJitterTicks
	}
	return DefaultPriceJitterTicks
}

func (s StrategyConfig) EffectiveBestLevels() int {
	levels := s.BestLevels
	if levels <= 0 {
		levels = DefaultBestLevels
	}
	if levels > s.Levels {
		return s.Levels
	}
	return levels
}

func (s StrategyConfig) EffectiveBestLevelRefreshSeconds() int {
	if s.BestLevelRefreshSeconds > 0 {
		return s.BestLevelRefreshSeconds
	}
	return DefaultBestRefreshSeconds
}

func (s StrategyConfig) RefreshOrdersPerCycle(orderCount int) int {
	if orderCount <= 0 {
		return 0
	}
	count := (orderCount*s.EffectiveQuoteRefreshRatioBPS() + 9_999) / 10_000
	if count < 1 {
		return 1
	}
	if count > orderCount {
		return orderCount
	}
	return count
}

// UsesOrderNotionalRange reports whether the strategy opted into quote-asset
// sizing. Zero values mean the fields were absent (or explicitly disabled),
// which keeps old published configurations on their fixed base quantity.
func (s StrategyConfig) UsesOrderNotionalRange() bool {
	return !s.MinOrderNotional.IsZero() || !s.MaxOrderNotional.IsZero()
}

// NormalizeStrategySizing removes the legacy fallback once a complete quote
// notional range exists. Besides keeping snapshots unambiguous, this makes an
// older engine reject the new configuration instead of silently continuing to
// use a stale fixed base quantity it does not understand.
func (c *Config) NormalizeStrategySizing() {
	for index := range c.Instruments {
		strategy := &c.Instruments[index].Strategy
		if strategy.MinOrderNotional.IsPositive() && strategy.MaxOrderNotional.IsPositive() {
			strategy.OrderSize = num.Decimal{}
		}
		if strategy.QuoteRefreshSeconds == 0 {
			strategy.QuoteRefreshSeconds = DefaultQuoteRefreshSeconds
		}
		if strategy.QuoteRefreshRatioBPS == 0 {
			strategy.QuoteRefreshRatioBPS = DefaultQuoteRefreshRatioBPS
		}
		if strategy.MinOrderLifetimeSeconds == 0 {
			strategy.MinOrderLifetimeSeconds = DefaultMinOrderLifetimeSeconds
		}
		if strategy.MaxOrderLifetimeSeconds == 0 {
			strategy.MaxOrderLifetimeSeconds = DefaultMaxOrderLifetimeSeconds
		}
		if strategy.PriceJitterTicks == 0 {
			strategy.PriceJitterTicks = DefaultPriceJitterTicks
		}
		if strategy.BestLevels == 0 {
			strategy.BestLevels = min(DefaultBestLevels, strategy.Levels)
		}
		if strategy.BestLevelRefreshSeconds == 0 {
			strategy.BestLevelRefreshSeconds = max(DefaultBestRefreshSeconds, strategy.QuoteRefreshSeconds)
		}
	}
}

// TradeSimulationConfig drives internal-only synthetic trade events. It never
// submits an order to a venue; the selected venue is used only as the source
// of top-of-book data and market precision.
type TradeSimulationConfig struct {
	Enabled           bool        `json:"enabled"`
	SourceVenue       string      `json:"source_venue"`
	MinQuantity       num.Decimal `json:"min_quantity"`
	MaxQuantity       num.Decimal `json:"max_quantity"`
	MinIntervalMS     int         `json:"min_interval_ms"`
	MaxIntervalMS     int         `json:"max_interval_ms"`
	BuyProbabilityBPS int         `json:"buy_probability_bps"`
	RecentLimit       int         `json:"recent_limit"`
}

const MaxStrategyLevels = 100

type InstrumentConfig struct {
	ID              string                `json:"id"`
	Base            AssetConfig           `json:"base"`
	Quote           AssetConfig           `json:"quote"`
	Reference       ReferenceConfig       `json:"reference"`
	Strategy        StrategyConfig        `json:"strategy"`
	TradeSimulation TradeSimulationConfig `json:"trade_simulation"`
}

type VenueMarketConfig struct {
	Symbol             string      `json:"symbol"`
	BaseAsset          string      `json:"base_asset"`
	QuoteAsset         string      `json:"quote_asset"`
	PriceTick          num.Decimal `json:"price_tick"`
	QuantityStep       num.Decimal `json:"quantity_step"`
	MinNotional        num.Decimal `json:"min_notional"`
	MinQuantity        num.Decimal `json:"min_quantity,omitempty"`
	MaxQuantity        num.Decimal `json:"max_quantity,omitempty"`
	MaxNotional        num.Decimal `json:"max_notional,omitempty"`
	MinPrice           num.Decimal `json:"min_price,omitempty"`
	MaxPrice           num.Decimal `json:"max_price,omitempty"`
	MaxOpenOrders      int         `json:"max_open_orders,omitempty"`
	MaxBaseCommitment  num.Decimal `json:"max_base_commitment,omitempty"`
	MaxQuoteCommitment num.Decimal `json:"max_quote_commitment,omitempty"`
	CredentialID       int64       `json:"credential_id,omitempty"`
}

type VenueConfig struct {
	Type                string                       `json:"type"`
	Environment         string                       `json:"environment,omitempty"`
	Enabled             bool                         `json:"enabled"`
	TradingEnabled      bool                         `json:"trading_enabled"`
	DedicatedAccount    bool                         `json:"dedicated_account"`
	BaseURL             string                       `json:"base_url"`
	SelfTradePrevention string                       `json:"self_trade_prevention,omitempty"`
	Markets             map[string]VenueMarketConfig `json:"markets"`
}

func (c Config) PollInterval() time.Duration {
	return time.Duration(c.PollIntervalMS) * time.Millisecond
}

func (c Config) RequestTimeout() time.Duration {
	return time.Duration(c.RPC.RequestTimeoutMS) * time.Millisecond
}

func (c Config) Validate() error {
	if c.Mode != domain.ModeShadow && c.Mode != domain.ModeLive {
		return fmt.Errorf("mode must be shadow or live")
	}
	if c.PollIntervalMS < 250 {
		return fmt.Errorf("poll_interval_ms must be >= 250")
	}
	if c.Mode == domain.ModeLive && (c.HeartbeatPath == "" || c.WatchdogTimeoutSeconds <= 0) {
		return fmt.Errorf("live mode requires heartbeat_path and positive watchdog_timeout_seconds")
	}
	if c.MarketFailureThreshold < 0 || c.MarketRecoveryThreshold < 0 || c.MarketErrorGraceSeconds < 0 || c.TradingProgressTimeoutSeconds < 0 {
		return fmt.Errorf("market fault thresholds must not be negative")
	}
	if c.MaxConcurrentInstruments < 0 || c.MaxConcurrentInstruments > 32 {
		return fmt.Errorf("max_concurrent_instruments must be 0..32")
	}
	if c.RulesRefreshSeconds < 0 || (c.RulesRefreshSeconds > 0 && c.RulesRefreshSeconds < 30) {
		return fmt.Errorf("rules_refresh_seconds must be zero or at least 30")
	}
	if c.TradingProgressTimeoutSeconds > 0 && c.TradingProgressTimeoutSeconds < 30 {
		return fmt.Errorf("trading progress timeout must be at least 30 seconds")
	}
	if c.AuditMaxBytes < 0 || c.AuditBackups < 0 || c.AuditBackups > 30 {
		return fmt.Errorf("audit rotation limits are invalid")
	}
	if c.AuditMaxBytes > 0 && c.AuditMaxBytes < 1<<20 {
		return fmt.Errorf("audit rotation requires at least 1 MiB")
	}
	if len(c.RPC.URLs) == 0 {
		return fmt.Errorf("at least one rpc url is required")
	}
	if c.RPC.ChainID == 0 {
		return fmt.Errorf("rpc.chain_id must be positive")
	}
	if c.RPC.RequestTimeoutMS <= 0 {
		return fmt.Errorf("rpc.request_timeout_ms must be positive")
	}
	// An empty instrument list is a valid idle configuration; it lets the operator
	// save RPC/venues/settings incrementally before creating any pair.
	seen := map[string]bool{}
	for _, in := range c.Instruments {
		if strings.TrimSpace(in.ID) == "" || seen[in.ID] {
			return fmt.Errorf("instrument id must be unique and non-empty")
		}
		seen[in.ID] = true
		if !looksLikeAddress(in.Base.Address) || !looksLikeAddress(in.Quote.Address) {
			return fmt.Errorf("instrument %s: invalid base or quote token address", in.ID)
		}
		if in.Reference.Type != "pancake_v2" {
			return fmt.Errorf("instrument %s: unsupported reference type", in.ID)
		}
		if len(in.Reference.Legs) == 0 {
			return fmt.Errorf("instrument %s: reference legs required", in.ID)
		}
		if c.Mode == domain.ModeLive && in.Reference.AllowSpotDuringWarmup {
			return fmt.Errorf("instrument %s: live mode forbids allow_spot_during_warmup", in.ID)
		}
		expectedInput := in.Base.Address
		for legIndex, leg := range in.Reference.Legs {
			if !looksLikeAddress(leg.PairAddress) || !looksLikeAddress(leg.BaseToken) || !looksLikeAddress(leg.QuoteToken) {
				return fmt.Errorf("instrument %s: invalid Pancake V2 leg address", in.ID)
			}
			if !strings.EqualFold(strings.TrimSpace(leg.BaseToken), strings.TrimSpace(expectedInput)) {
				return fmt.Errorf("instrument %s: leg %d base token does not continue the price path", in.ID, legIndex+1)
			}
			if leg.ExpectedFactory != "" && !looksLikeAddress(leg.ExpectedFactory) {
				return fmt.Errorf("instrument %s: invalid expected factory", in.ID)
			}
			if leg.MaxIdleSeconds < 0 || leg.MinQuoteReserve.Sign() < 0 {
				return fmt.Errorf("instrument %s: invalid leg safety limits", in.ID)
			}
			expectedInput = leg.QuoteToken
		}
		if !strings.EqualFold(strings.TrimSpace(expectedInput), strings.TrimSpace(in.Quote.Address)) {
			return fmt.Errorf("instrument %s: price path does not end at quote token", in.ID)
		}
		if in.Reference.TWAPWindowSeconds <= 0 {
			return fmt.Errorf("instrument %s: twap window must be positive", in.ID)
		}
		if in.Strategy.Levels < 1 || in.Strategy.Levels > MaxStrategyLevels {
			return fmt.Errorf("instrument %s: levels must be 1..%d", in.ID, MaxStrategyLevels)
		}
		if in.Strategy.UsesOrderNotionalRange() {
			if !in.Strategy.MinOrderNotional.IsPositive() || !in.Strategy.MaxOrderNotional.IsPositive() {
				return fmt.Errorf("instrument %s: minimum and maximum order notional must be positive", in.ID)
			}
			if in.Strategy.MaxOrderNotional.Cmp(in.Strategy.MinOrderNotional) < 0 {
				return fmt.Errorf("instrument %s: maximum order notional must be greater than or equal to minimum order notional", in.ID)
			}
		} else if !in.Strategy.OrderSize.IsPositive() {
			return fmt.Errorf("instrument %s: order notional range or legacy order size is required", in.ID)
		}
		if in.Strategy.HalfSpreadBPS < 0 || in.Strategy.LevelSpacingBPS < 0 || in.Strategy.RepriceThresholdBPS < 0 {
			return fmt.Errorf("instrument %s: spread, spacing and reprice threshold must not be negative", in.ID)
		}
		strategy := in.Strategy
		if strategy.QuoteRefreshSeconds < 0 || (strategy.QuoteRefreshSeconds > 0 && strategy.QuoteRefreshSeconds < 10) {
			return fmt.Errorf("instrument %s: quote refresh interval must be zero or at least 10 seconds", in.ID)
		}
		if strategy.QuoteRefreshRatioBPS < 0 || strategy.QuoteRefreshRatioBPS > 10_000 {
			return fmt.Errorf("instrument %s: quote refresh ratio must be 0..10000 bps", in.ID)
		}
		if strategy.MinOrderLifetimeSeconds < 0 || (strategy.MinOrderLifetimeSeconds > 0 && strategy.MinOrderLifetimeSeconds < 5) {
			return fmt.Errorf("instrument %s: minimum order lifetime must be zero or at least 5 seconds", in.ID)
		}
		if strategy.MaxOrderLifetimeSeconds < 0 || (strategy.MaxOrderLifetimeSeconds > 0 && strategy.MaxOrderLifetimeSeconds < 10) {
			return fmt.Errorf("instrument %s: maximum order lifetime must be zero or at least 10 seconds", in.ID)
		}
		if strategy.EffectiveMaxOrderLifetime() < strategy.EffectiveMinOrderLifetime() {
			return fmt.Errorf("instrument %s: maximum order lifetime must be greater than or equal to minimum order lifetime", in.ID)
		}
		if strategy.PriceJitterTicks < 0 || strategy.PriceJitterTicks > 100 {
			return fmt.Errorf("instrument %s: price jitter must be 0..100 ticks", in.ID)
		}
		if strategy.BestLevels < 0 || strategy.BestLevels > strategy.Levels {
			return fmt.Errorf("instrument %s: best levels must be 0..levels", in.ID)
		}
		if strategy.BestLevelRefreshSeconds < 0 || (strategy.BestLevelRefreshSeconds > 0 && strategy.BestLevelRefreshSeconds < strategy.EffectiveQuoteRefreshSeconds()) {
			return fmt.Errorf("instrument %s: best-level refresh interval must not be shorter than the regular refresh interval", in.ID)
		}
		if in.Strategy.BalanceReserveBPS < 0 || in.Strategy.BalanceReserveBPS > 10_000 {
			return fmt.Errorf("instrument %s: balance reserve must be 0..10000 bps", in.ID)
		}
		if in.Strategy.MaxVenueReferenceDeviationBPS < 0 || in.Strategy.MaxVenueReferenceDeviationBPS > 10_000 || in.Strategy.MaxVenueSpreadBPS < 0 || in.Strategy.MaxVenueSpreadBPS > 10_000 {
			return fmt.Errorf("instrument %s: venue price protection limits must be 0..10000 bps", in.ID)
		}
		outermostSpreadBPS := int64(in.Strategy.HalfSpreadBPS) + int64(in.Strategy.Levels-1)*int64(in.Strategy.LevelSpacingBPS)
		if outermostSpreadBPS >= 10_000 {
			return fmt.Errorf("instrument %s: outermost quote spread must be below 10000 bps", in.ID)
		}
		if in.Strategy.TargetBase.Sign() < 0 || in.Strategy.MaxBaseDeviation.Sign() < 0 || in.Strategy.InventorySkewBPS < 0 {
			return fmt.Errorf("instrument %s: inventory controls must not be negative", in.ID)
		}
		if in.TradeSimulation.Enabled {
			sim := in.TradeSimulation
			if strings.TrimSpace(sim.SourceVenue) == "" {
				return fmt.Errorf("instrument %s: trade simulation source venue is required", in.ID)
			}
			if !sim.MinQuantity.IsPositive() || sim.MaxQuantity.Cmp(sim.MinQuantity) < 0 {
				return fmt.Errorf("instrument %s: trade simulation quantity range is invalid", in.ID)
			}
			if sim.MinIntervalMS < 100 || sim.MaxIntervalMS < sim.MinIntervalMS {
				return fmt.Errorf("instrument %s: trade simulation interval must be at least 100ms and max >= min", in.ID)
			}
			if sim.BuyProbabilityBPS < 0 || sim.BuyProbabilityBPS > 10_000 {
				return fmt.Errorf("instrument %s: trade simulation buy probability must be 0..10000 bps", in.ID)
			}
			if sim.RecentLimit < 1 || sim.RecentLimit > 200 {
				return fmt.Errorf("instrument %s: trade simulation recent limit must be 1..200", in.ID)
			}
		}
	}
	enabledMarkets := 0
	marketsByInstrument := map[string]int{}
	for name, v := range c.Venues {
		if !v.Enabled {
			continue
		}
		adapter, supported := venue.AdapterSpecFor(v.Type)
		if !supported {
			return fmt.Errorf("venue %s: unsupported type", name)
		}
		if v.BaseURL == "" {
			return fmt.Errorf("venue %s: base_url required", name)
		}
		if v.Environment != "" && v.Environment != "production" && v.Environment != "testnet" {
			return fmt.Errorf("venue %s: environment must be production or testnet", name)
		}
		parsedBaseURL, err := url.Parse(v.BaseURL)
		if err != nil || parsedBaseURL.Scheme != "https" || parsedBaseURL.Host == "" {
			return fmt.Errorf("venue %s: base_url must be an https URL", name)
		}
		if v.Environment == "testnet" && adapter.TestnetBaseURL != "" {
			testnetURL, _ := url.Parse(adapter.TestnetBaseURL)
			if !strings.EqualFold(parsedBaseURL.Hostname(), testnetURL.Hostname()) {
				return fmt.Errorf("venue %s: %s testnet must use %s", name, adapter.Name, adapter.TestnetBaseURL)
			}
		}
		if adapter.RequiresSelfTradePrevention && v.TradingEnabled && (v.SelfTradePrevention == "" || strings.EqualFold(v.SelfTradePrevention, "NONE")) {
			return fmt.Errorf("venue %s: %s trading requires self-trade prevention", name, adapter.Name)
		}
		if adapter.RequiresDedicatedAccount && v.TradingEnabled && !v.DedicatedAccount {
			return fmt.Errorf("venue %s: %s live trading requires dedicated_account=true", name, adapter.Name)
		}
		for id, m := range v.Markets {
			if !seen[id] {
				return fmt.Errorf("venue %s: market references unknown instrument %s", name, id)
			}
			if m.Symbol == "" || m.BaseAsset == "" || m.QuoteAsset == "" || !m.PriceTick.IsPositive() || !m.QuantityStep.IsPositive() || !m.MinNotional.IsPositive() {
				return fmt.Errorf("venue %s market %s: invalid symbol/ticks", name, id)
			}
			if m.MaxBaseCommitment.Sign() < 0 || m.MaxQuoteCommitment.Sign() < 0 {
				return fmt.Errorf("venue %s market %s: commitment limits must not be negative", name, id)
			}
			if c.Mode == domain.ModeLive && v.TradingEnabled && m.CredentialID <= 0 {
				return fmt.Errorf("venue %s market %s: credential is required for live trading", name, id)
			}
			enabledMarkets++
			marketsByInstrument[id]++
		}
	}
	// Only required once instruments exist (each pair still requires its own market
	// below). With no instruments this is a valid idle config.
	if len(c.Instruments) > 0 && enabledMarkets == 0 {
		return fmt.Errorf("at least one enabled venue market is required")
	}
	for _, in := range c.Instruments {
		if marketsByInstrument[in.ID] == 0 {
			return fmt.Errorf("instrument %s has no enabled venue market", in.ID)
		}
		if in.TradeSimulation.Enabled {
			venueConfig, exists := c.Venues[in.TradeSimulation.SourceVenue]
			if !exists || !venueConfig.Enabled {
				return fmt.Errorf("instrument %s: trade simulation source venue %s is not enabled", in.ID, in.TradeSimulation.SourceVenue)
			}
			if _, exists := venueConfig.Markets[in.ID]; !exists {
				return fmt.Errorf("instrument %s: trade simulation source venue %s has no market mapping", in.ID, in.TradeSimulation.SourceVenue)
			}
		}
	}
	return nil
}

// ValidateRuntime is kept as the runtime validation entry point. Credential
// existence, status and venue ownership are verified while clients are built.
func (c Config) ValidateRuntime() error {
	return c.Validate()
}

func looksLikeAddress(value string) bool {
	value = strings.TrimSpace(value)
	if len(value) != 42 || !strings.HasPrefix(strings.ToLower(value), "0x") {
		return false
	}
	for _, r := range value[2:] {
		if !((r >= '0' && r <= '9') || (r >= 'a' && r <= 'f') || (r >= 'A' && r <= 'F')) {
			return false
		}
	}
	return true
}
