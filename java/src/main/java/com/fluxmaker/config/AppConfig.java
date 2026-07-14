package com.fluxmaker.config;

import com.fasterxml.jackson.annotation.JsonInclude;
import com.fluxmaker.domain.Domain;
import com.fluxmaker.math.DecimalValue;

import java.net.URI;
import java.time.Duration;
import java.util.ArrayList;
import java.util.LinkedHashMap;
import java.util.List;
import java.util.Locale;
import java.util.Map;
import java.util.Set;

public final class AppConfig {
    public static final int MAX_STRATEGY_LEVELS = 100;
    public static final int DEFAULT_QUOTE_REFRESH_SECONDS = 45;
    public static final int DEFAULT_QUOTE_REFRESH_RATIO_BPS = 1_000;
    public static final int DEFAULT_MIN_ORDER_LIFETIME_SECONDS = 30;
    public static final int DEFAULT_MAX_ORDER_LIFETIME_SECONDS = 300;
    public static final int DEFAULT_PRICE_JITTER_TICKS = 2;
    public static final int DEFAULT_BEST_LEVELS = 3;
    public static final int DEFAULT_BEST_REFRESH_SECONDS = 90;

    public Domain.Mode mode;
    public int pollIntervalMs;
    public String auditPath = "";
    public long auditMaxBytes;
    public int auditBackups;
    public String heartbeatPath = "";
    public int watchdogTimeoutSeconds;
    public int marketFailureThreshold;
    public int marketRecoveryThreshold;
    public int marketErrorGraceSeconds;
    public int tradingProgressTimeoutSeconds;
    public int maxConcurrentInstruments;
    public int rulesRefreshSeconds;
    public RpcConfig rpc = new RpcConfig();
    public List<InstrumentConfig> instruments = new ArrayList<>();
    public Map<String, VenueConfig> venues = new LinkedHashMap<>();

    public static final class RpcConfig {
        public List<String> urls = new ArrayList<>();
        public long chainId;
        public int requestTimeoutMs;
    }

    public static final class AssetConfig {
        public String symbol = "";
        @JsonInclude(JsonInclude.Include.NON_EMPTY)
        public String address = "";
        public int decimals;
    }

    public static final class ReferenceConfig {
        public String type = "";
        public List<PairLegConfig> legs = new ArrayList<>();
        public int twapWindowSeconds;
        public int maxSpotTwapDeviationBps;
        public int staleAfterSeconds;
        public boolean allowSpotDuringWarmup;
    }

    public static final class PairLegConfig {
        public String pairAddress = "";
        @JsonInclude(JsonInclude.Include.NON_EMPTY)
        public String expectedFactory = "";
        public String baseToken = "";
        public String quoteToken = "";
        public DecimalValue minQuoteReserve = DecimalValue.ZERO;
        public int maxIdleSeconds;
    }

    public static final class StrategyConfig {
        public int halfSpreadBps;
        public int levelSpacingBps;
        public int levels;
        @JsonInclude(JsonInclude.Include.NON_DEFAULT)
        public DecimalValue orderSize = DecimalValue.ZERO;
        @JsonInclude(JsonInclude.Include.NON_DEFAULT)
        public DecimalValue minOrderNotional = DecimalValue.ZERO;
        @JsonInclude(JsonInclude.Include.NON_DEFAULT)
        public DecimalValue maxOrderNotional = DecimalValue.ZERO;
        public int repriceThresholdBps;
        public int balanceReserveBps;
        public int maxVenueReferenceDeviationBps;
        public int maxVenueSpreadBps;
        public DecimalValue targetBase = DecimalValue.ZERO;
        public DecimalValue maxBaseDeviation = DecimalValue.ZERO;
        public int inventorySkewBps;
        @JsonInclude(JsonInclude.Include.NON_DEFAULT)
        public int quoteRefreshSeconds;
        @JsonInclude(JsonInclude.Include.NON_DEFAULT)
        public int quoteRefreshRatioBps;
        @JsonInclude(JsonInclude.Include.NON_DEFAULT)
        public int minOrderLifetimeSeconds;
        @JsonInclude(JsonInclude.Include.NON_DEFAULT)
        public int maxOrderLifetimeSeconds;
        @JsonInclude(JsonInclude.Include.NON_DEFAULT)
        public int priceJitterTicks;
        @JsonInclude(JsonInclude.Include.NON_DEFAULT)
        public int bestLevels;
        @JsonInclude(JsonInclude.Include.NON_DEFAULT)
        public int bestLevelRefreshSeconds;
        public boolean usesOrderNotionalRange() { return !minOrderNotional.isZero() || !maxOrderNotional.isZero(); }
        public int effectiveQuoteRefreshSeconds() { return quoteRefreshSeconds > 0 ? quoteRefreshSeconds : DEFAULT_QUOTE_REFRESH_SECONDS; }
        public int effectiveQuoteRefreshRatioBps() { return quoteRefreshRatioBps > 0 ? quoteRefreshRatioBps : DEFAULT_QUOTE_REFRESH_RATIO_BPS; }
        public Duration effectiveMinOrderLifetime() { return Duration.ofSeconds(minOrderLifetimeSeconds > 0 ? minOrderLifetimeSeconds : DEFAULT_MIN_ORDER_LIFETIME_SECONDS); }
        public Duration effectiveMaxOrderLifetime() { return Duration.ofSeconds(maxOrderLifetimeSeconds > 0 ? maxOrderLifetimeSeconds : DEFAULT_MAX_ORDER_LIFETIME_SECONDS); }
        public int effectivePriceJitterTicks() { return priceJitterTicks > 0 ? priceJitterTicks : DEFAULT_PRICE_JITTER_TICKS; }
        public int effectiveBestLevels() {
            int effective = bestLevels > 0 ? bestLevels : DEFAULT_BEST_LEVELS;
            return Math.min(effective, levels);
        }
        public int effectiveBestLevelRefreshSeconds() { return bestLevelRefreshSeconds > 0 ? bestLevelRefreshSeconds : DEFAULT_BEST_REFRESH_SECONDS; }
        public int refreshOrdersPerCycle(int orderCount) {
            if (orderCount <= 0) return 0;
            int count = (orderCount * effectiveQuoteRefreshRatioBps() + 9_999) / 10_000;
            return Math.max(1, Math.min(count, orderCount));
        }
    }

    public static final class TradeSimulationConfig {
        public boolean enabled;
        public String sourceVenue = "";
        public DecimalValue minQuantity = DecimalValue.ZERO;
        public DecimalValue maxQuantity = DecimalValue.ZERO;
        public int minIntervalMs;
        public int maxIntervalMs;
        public int buyProbabilityBps;
        public int recentLimit;
    }

    public static final class InstrumentConfig {
        public String id = "";
        public AssetConfig base = new AssetConfig();
        public AssetConfig quote = new AssetConfig();
        public ReferenceConfig reference = new ReferenceConfig();
        public StrategyConfig strategy = new StrategyConfig();
        public TradeSimulationConfig tradeSimulation = new TradeSimulationConfig();
    }

    public static final class VenueMarketConfig {
        public String symbol = "";
        public String baseAsset = "";
        public String quoteAsset = "";
        public DecimalValue priceTick = DecimalValue.ZERO;
        public DecimalValue quantityStep = DecimalValue.ZERO;
        public DecimalValue minNotional = DecimalValue.ZERO;
        @JsonInclude(JsonInclude.Include.NON_DEFAULT)
        public DecimalValue minQuantity = DecimalValue.ZERO;
        @JsonInclude(JsonInclude.Include.NON_DEFAULT)
        public DecimalValue maxQuantity = DecimalValue.ZERO;
        @JsonInclude(JsonInclude.Include.NON_DEFAULT)
        public DecimalValue maxNotional = DecimalValue.ZERO;
        @JsonInclude(JsonInclude.Include.NON_DEFAULT)
        public DecimalValue minPrice = DecimalValue.ZERO;
        @JsonInclude(JsonInclude.Include.NON_DEFAULT)
        public DecimalValue maxPrice = DecimalValue.ZERO;
        @JsonInclude(JsonInclude.Include.NON_DEFAULT)
        public int maxOpenOrders;
        @JsonInclude(JsonInclude.Include.NON_DEFAULT)
        public DecimalValue maxBaseCommitment = DecimalValue.ZERO;
        @JsonInclude(JsonInclude.Include.NON_DEFAULT)
        public DecimalValue maxQuoteCommitment = DecimalValue.ZERO;
        @JsonInclude(JsonInclude.Include.NON_DEFAULT)
        public long credentialId;
    }

    public static final class VenueConfig {
        public String type = "";
        @JsonInclude(JsonInclude.Include.NON_EMPTY)
        public String environment = "";
        public boolean enabled;
        public boolean tradingEnabled;
        public boolean dedicatedAccount;
        public String baseUrl = "";
        @JsonInclude(JsonInclude.Include.NON_EMPTY)
        public String selfTradePrevention = "";
        public Map<String, VenueMarketConfig> markets = new LinkedHashMap<>();
    }

    public record AdapterSpec(String type, String name, String productionBaseUrl, String testnetBaseUrl,
                              String defaultSelfTradePrevention, boolean requiresSelfTradePrevention,
                              boolean requiresDedicatedAccount) {}

    public static final List<AdapterSpec> ADAPTER_SPECS = List.of(
            new AdapterSpec("binance", "Binance", "https://api.binance.com", "https://testnet.binance.vision", "EXPIRE_BOTH", true, false),
            new AdapterSpec("mgbx", "MGBX", "https://open.mgbx.com", "", "", false, true)
    );

    public Duration pollInterval() { return Duration.ofMillis(pollIntervalMs); }
    public Duration requestTimeout() { return Duration.ofMillis(rpc.requestTimeoutMs); }

    public void normalizeStrategySizing() {
        for (InstrumentConfig instrument : instruments) {
            StrategyConfig strategy = instrument.strategy;
            if (strategy.minOrderNotional.isPositive() && strategy.maxOrderNotional.isPositive()) strategy.orderSize = DecimalValue.ZERO;
            if (strategy.quoteRefreshSeconds == 0) strategy.quoteRefreshSeconds = DEFAULT_QUOTE_REFRESH_SECONDS;
            if (strategy.quoteRefreshRatioBps == 0) strategy.quoteRefreshRatioBps = DEFAULT_QUOTE_REFRESH_RATIO_BPS;
            if (strategy.minOrderLifetimeSeconds == 0) strategy.minOrderLifetimeSeconds = DEFAULT_MIN_ORDER_LIFETIME_SECONDS;
            if (strategy.maxOrderLifetimeSeconds == 0) strategy.maxOrderLifetimeSeconds = DEFAULT_MAX_ORDER_LIFETIME_SECONDS;
            if (strategy.priceJitterTicks == 0) strategy.priceJitterTicks = DEFAULT_PRICE_JITTER_TICKS;
            if (strategy.bestLevels == 0) strategy.bestLevels = Math.min(DEFAULT_BEST_LEVELS, strategy.levels);
            if (strategy.bestLevelRefreshSeconds == 0) strategy.bestLevelRefreshSeconds = Math.max(DEFAULT_BEST_REFRESH_SECONDS, strategy.quoteRefreshSeconds);
        }
    }

    public void applyRuntimeSafetyDefaults() {
        if (marketFailureThreshold == 0) marketFailureThreshold = 3;
        if (marketRecoveryThreshold == 0) marketRecoveryThreshold = 3;
        if (marketErrorGraceSeconds == 0) marketErrorGraceSeconds = 15;
        if (tradingProgressTimeoutSeconds == 0) tradingProgressTimeoutSeconds = 120;
        if (maxConcurrentInstruments == 0) maxConcurrentInstruments = 4;
        if (auditMaxBytes == 0) auditMaxBytes = 100L * 1024 * 1024;
        if (auditBackups == 0) auditBackups = 7;
        if (rulesRefreshSeconds == 0) rulesRefreshSeconds = 300;
        for (InstrumentConfig instrument : instruments) {
            if (instrument.strategy.maxVenueReferenceDeviationBps == 0) instrument.strategy.maxVenueReferenceDeviationBps = 500;
            if (instrument.strategy.maxVenueSpreadBps == 0) instrument.strategy.maxVenueSpreadBps = 1000;
        }
    }

    public void validate() {
        require(mode == Domain.Mode.shadow || mode == Domain.Mode.live, "mode must be shadow or live");
        require(pollIntervalMs >= 250, "poll_interval_ms must be >= 250");
        require(mode != Domain.Mode.live || (!blank(heartbeatPath) && watchdogTimeoutSeconds > 0), "live mode requires heartbeat_path and positive watchdog_timeout_seconds");
        require(marketFailureThreshold >= 0 && marketRecoveryThreshold >= 0 && marketErrorGraceSeconds >= 0 && tradingProgressTimeoutSeconds >= 0, "market fault thresholds must not be negative");
        require(maxConcurrentInstruments >= 0 && maxConcurrentInstruments <= 32, "max_concurrent_instruments must be 0..32");
        require(rulesRefreshSeconds >= 0 && (rulesRefreshSeconds == 0 || rulesRefreshSeconds >= 30), "rules_refresh_seconds must be zero or at least 30");
        require(tradingProgressTimeoutSeconds == 0 || tradingProgressTimeoutSeconds >= 30, "trading progress timeout must be at least 30 seconds");
        require(auditMaxBytes >= 0 && auditBackups >= 0 && auditBackups <= 30, "audit rotation limits are invalid");
        require(auditMaxBytes == 0 || auditMaxBytes >= 1L << 20, "audit rotation requires at least 1 MiB");
        require(rpc != null && rpc.urls != null && !rpc.urls.isEmpty(), "at least one rpc url is required");
        require(rpc.chainId > 0, "rpc.chain_id must be positive");
        require(rpc.requestTimeoutMs > 0, "rpc.request_timeout_ms must be positive");
        require(instruments != null && !instruments.isEmpty(), "at least one instrument is required");

        Set<String> seen = new java.util.HashSet<>();
        for (InstrumentConfig instrument : instruments) {
            require(!blank(instrument.id) && seen.add(instrument.id), "instrument id must be unique and non-empty");
            require(address(instrument.base.address) && address(instrument.quote.address), "instrument " + instrument.id + ": invalid base or quote token address");
            require("pancake_v2".equals(instrument.reference.type), "instrument " + instrument.id + ": unsupported reference type");
            require(instrument.reference.legs != null && !instrument.reference.legs.isEmpty(), "instrument " + instrument.id + ": reference legs required");
            require(mode != Domain.Mode.live || !instrument.reference.allowSpotDuringWarmup, "instrument " + instrument.id + ": live mode forbids allow_spot_during_warmup");
            String expectedInput = instrument.base.address;
            for (int index = 0; index < instrument.reference.legs.size(); index++) {
                PairLegConfig leg = instrument.reference.legs.get(index);
                require(address(leg.pairAddress) && address(leg.baseToken) && address(leg.quoteToken), "instrument " + instrument.id + ": invalid Pancake V2 leg address");
                require(leg.baseToken.trim().equalsIgnoreCase(expectedInput.trim()), "instrument " + instrument.id + ": leg " + (index + 1) + " base token does not continue the price path");
                require(blank(leg.expectedFactory) || address(leg.expectedFactory), "instrument " + instrument.id + ": invalid expected factory");
                require(leg.maxIdleSeconds >= 0 && leg.minQuoteReserve.signum() >= 0, "instrument " + instrument.id + ": invalid leg safety limits");
                expectedInput = leg.quoteToken;
            }
            require(expectedInput.trim().equalsIgnoreCase(instrument.quote.address.trim()), "instrument " + instrument.id + ": price path does not end at quote token");
            StrategyConfig strategy = instrument.strategy;
            require(instrument.reference.twapWindowSeconds > 0, "instrument " + instrument.id + ": twap window must be positive");
            require(strategy.levels >= 1 && strategy.levels <= MAX_STRATEGY_LEVELS, "instrument " + instrument.id + ": levels must be 1.." + MAX_STRATEGY_LEVELS);
            if (strategy.usesOrderNotionalRange()) {
                require(strategy.minOrderNotional.isPositive() && strategy.maxOrderNotional.isPositive(), "instrument " + instrument.id + ": minimum and maximum order notional must be positive");
                require(strategy.maxOrderNotional.compareTo(strategy.minOrderNotional) >= 0, "instrument " + instrument.id + ": maximum order notional must be greater than or equal to minimum order notional");
            } else require(strategy.orderSize.isPositive(), "instrument " + instrument.id + ": order notional range or legacy order size is required");
            require(strategy.halfSpreadBps >= 0 && strategy.levelSpacingBps >= 0 && strategy.repriceThresholdBps >= 0, "instrument " + instrument.id + ": spread, spacing and reprice threshold must not be negative");
            require(strategy.quoteRefreshSeconds >= 0 && (strategy.quoteRefreshSeconds == 0 || strategy.quoteRefreshSeconds >= 10), "instrument " + instrument.id + ": quote refresh interval must be zero or at least 10 seconds");
            require(strategy.quoteRefreshRatioBps >= 0 && strategy.quoteRefreshRatioBps <= 10_000, "instrument " + instrument.id + ": quote refresh ratio must be 0..10000 bps");
            require(strategy.minOrderLifetimeSeconds >= 0 && (strategy.minOrderLifetimeSeconds == 0 || strategy.minOrderLifetimeSeconds >= 5), "instrument " + instrument.id + ": minimum order lifetime must be zero or at least 5 seconds");
            require(strategy.maxOrderLifetimeSeconds >= 0 && (strategy.maxOrderLifetimeSeconds == 0 || strategy.maxOrderLifetimeSeconds >= 10), "instrument " + instrument.id + ": maximum order lifetime must be zero or at least 10 seconds");
            require(strategy.effectiveMaxOrderLifetime().compareTo(strategy.effectiveMinOrderLifetime()) >= 0, "instrument " + instrument.id + ": maximum order lifetime must be greater than or equal to minimum order lifetime");
            require(strategy.priceJitterTicks >= 0 && strategy.priceJitterTicks <= 100, "instrument " + instrument.id + ": price jitter must be 0..100 ticks");
            require(strategy.bestLevels >= 0 && strategy.bestLevels <= strategy.levels, "instrument " + instrument.id + ": best levels must be 0..levels");
            require(strategy.bestLevelRefreshSeconds >= 0 && (strategy.bestLevelRefreshSeconds == 0 || strategy.bestLevelRefreshSeconds >= strategy.effectiveQuoteRefreshSeconds()), "instrument " + instrument.id + ": best-level refresh interval must not be shorter than the regular refresh interval");
            require(strategy.balanceReserveBps >= 0 && strategy.balanceReserveBps <= 10_000, "instrument " + instrument.id + ": balance reserve must be 0..10000 bps");
            require(strategy.maxVenueReferenceDeviationBps >= 0 && strategy.maxVenueReferenceDeviationBps <= 10_000 && strategy.maxVenueSpreadBps >= 0 && strategy.maxVenueSpreadBps <= 10_000, "instrument " + instrument.id + ": venue price protection limits must be 0..10000 bps");
            long outermost = (long) strategy.halfSpreadBps + (long) (strategy.levels - 1) * strategy.levelSpacingBps;
            require(outermost < 10_000, "instrument " + instrument.id + ": outermost quote spread must be below 10000 bps");
            require(strategy.targetBase.signum() >= 0 && strategy.maxBaseDeviation.signum() >= 0 && strategy.inventorySkewBps >= 0, "instrument " + instrument.id + ": inventory controls must not be negative");
            if (instrument.tradeSimulation.enabled) {
                TradeSimulationConfig simulation = instrument.tradeSimulation;
                require(!blank(simulation.sourceVenue), "instrument " + instrument.id + ": trade simulation source venue is required");
                require(simulation.minQuantity.isPositive() && simulation.maxQuantity.compareTo(simulation.minQuantity) >= 0, "instrument " + instrument.id + ": trade simulation quantity range is invalid");
                require(simulation.minIntervalMs >= 100 && simulation.maxIntervalMs >= simulation.minIntervalMs, "instrument " + instrument.id + ": trade simulation interval must be at least 100ms and max >= min");
                require(simulation.buyProbabilityBps >= 0 && simulation.buyProbabilityBps <= 10_000, "instrument " + instrument.id + ": trade simulation buy probability must be 0..10000 bps");
                require(simulation.recentLimit >= 1 && simulation.recentLimit <= 200, "instrument " + instrument.id + ": trade simulation recent limit must be 1..200");
            }
        }

        int enabledMarkets = 0;
        Map<String, Integer> marketsByInstrument = new LinkedHashMap<>();
        for (Map.Entry<String, VenueConfig> entry : venues.entrySet()) {
            String name = entry.getKey();
            VenueConfig venue = entry.getValue();
            if (!venue.enabled) continue;
            AdapterSpec spec = adapterSpec(venue.type);
            require(spec != null, "venue " + name + ": unsupported type");
            require(!blank(venue.baseUrl), "venue " + name + ": base_url required");
            require(blank(venue.environment) || venue.environment.equals("production") || venue.environment.equals("testnet"), "venue " + name + ": environment must be production or testnet");
            URI base;
            try { base = URI.create(venue.baseUrl); } catch (RuntimeException e) { throw invalid("venue " + name + ": base_url must be an https URL"); }
            require("https".equals(base.getScheme()) && base.getHost() != null, "venue " + name + ": base_url must be an https URL");
            if (venue.environment.equals("testnet") && !blank(spec.testnetBaseUrl())) {
                require(base.getHost().equalsIgnoreCase(URI.create(spec.testnetBaseUrl()).getHost()), "venue " + name + ": " + spec.name() + " testnet must use " + spec.testnetBaseUrl());
            }
            require(!spec.requiresSelfTradePrevention() || !venue.tradingEnabled || (!blank(venue.selfTradePrevention) && !venue.selfTradePrevention.equalsIgnoreCase("NONE")), "venue " + name + ": " + spec.name() + " trading requires self-trade prevention");
            require(!spec.requiresDedicatedAccount() || !venue.tradingEnabled || venue.dedicatedAccount, "venue " + name + ": " + spec.name() + " live trading requires dedicated_account=true");
            for (Map.Entry<String, VenueMarketConfig> marketEntry : venue.markets.entrySet()) {
                String id = marketEntry.getKey();
                VenueMarketConfig market = marketEntry.getValue();
                require(seen.contains(id), "venue " + name + ": market references unknown instrument " + id);
                require(!blank(market.symbol) && !blank(market.baseAsset) && !blank(market.quoteAsset) && market.priceTick.isPositive() && market.quantityStep.isPositive() && market.minNotional.isPositive(), "venue " + name + " market " + id + ": invalid symbol/ticks");
                require(market.maxBaseCommitment.signum() >= 0 && market.maxQuoteCommitment.signum() >= 0, "venue " + name + " market " + id + ": commitment limits must not be negative");
                require(mode != Domain.Mode.live || !venue.tradingEnabled || market.credentialId > 0, "venue " + name + " market " + id + ": credential is required for live trading");
                enabledMarkets++;
                marketsByInstrument.merge(id, 1, Integer::sum);
            }
        }
        require(enabledMarkets > 0, "at least one enabled venue market is required");
        for (InstrumentConfig instrument : instruments) {
            require(marketsByInstrument.getOrDefault(instrument.id, 0) > 0, "instrument " + instrument.id + " has no enabled venue market");
            if (instrument.tradeSimulation.enabled) {
                VenueConfig venue = venues.get(instrument.tradeSimulation.sourceVenue);
                require(venue != null && venue.enabled, "instrument " + instrument.id + ": trade simulation source venue " + instrument.tradeSimulation.sourceVenue + " is not enabled");
                require(venue.markets.containsKey(instrument.id), "instrument " + instrument.id + ": trade simulation source venue " + instrument.tradeSimulation.sourceVenue + " has no market mapping");
            }
        }
    }

    public static AdapterSpec adapterSpec(String type) {
        String normalized = type == null ? "" : type.trim().toLowerCase(Locale.ROOT);
        return ADAPTER_SPECS.stream().filter(spec -> spec.type().equals(normalized)).findFirst().orElse(null);
    }

    private static boolean blank(String value) { return value == null || value.trim().isEmpty(); }
    private static boolean address(String value) { return value != null && value.trim().matches("(?i)^0x[0-9a-f]{40}$"); }
    private static void require(boolean condition, String message) { if (!condition) throw invalid(message); }
    private static IllegalArgumentException invalid(String message) { return new IllegalArgumentException(message); }
}
