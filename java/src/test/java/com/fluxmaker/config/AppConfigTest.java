package com.fluxmaker.config;

import com.fluxmaker.domain.Domain;
import com.fluxmaker.json.Json;
import com.fluxmaker.math.DecimalValue;
import org.junit.jupiter.api.Test;

import java.util.List;
import java.util.Map;

import static org.junit.jupiter.api.Assertions.assertDoesNotThrow;
import static org.junit.jupiter.api.Assertions.assertEquals;
import static org.junit.jupiter.api.Assertions.assertThrows;
import static org.junit.jupiter.api.Assertions.assertTrue;

class AppConfigTest {
    @Test
    void validatesMinimalGoCompatibleConfiguration() {
        AppConfig config = minimal();
        assertDoesNotThrow(config::validate);
        config.instruments.getFirst().strategy.levels = 101;
        assertThrows(IllegalArgumentException.class, config::validate);
    }

    @Test
    void normalizesNewSizingAndAppliesRuntimeDefaults() {
        AppConfig config = minimal();
        config.instruments.getFirst().strategy.minOrderNotional = DecimalValue.parse("10");
        config.instruments.getFirst().strategy.maxOrderNotional = DecimalValue.parse("20");
        config.normalizeStrategySizing();
        config.applyRuntimeSafetyDefaults();
        assertEquals(DecimalValue.ZERO, config.instruments.getFirst().strategy.orderSize);
        assertEquals(4, config.maxConcurrentInstruments);
        assertEquals(300, config.rulesRefreshSeconds);
    }

    @Test
    void normalizesQuoteRefreshDefaults() {
        AppConfig config = minimal();
        config.normalizeStrategySizing();
        AppConfig.StrategyConfig strategy = config.instruments.getFirst().strategy;
        assertEquals(AppConfig.DEFAULT_QUOTE_REFRESH_SECONDS, strategy.quoteRefreshSeconds);
        assertEquals(AppConfig.DEFAULT_QUOTE_REFRESH_RATIO_BPS, strategy.quoteRefreshRatioBps);
        assertEquals(AppConfig.DEFAULT_MIN_ORDER_LIFETIME_SECONDS, strategy.minOrderLifetimeSeconds);
        assertEquals(AppConfig.DEFAULT_MAX_ORDER_LIFETIME_SECONDS, strategy.maxOrderLifetimeSeconds);
        assertEquals(AppConfig.DEFAULT_FILL_REPLENISH_MIN_DELAY_SECONDS, strategy.fillReplenishMinDelaySeconds);
        assertEquals(AppConfig.DEFAULT_FILL_REPLENISH_MAX_DELAY_SECONDS, strategy.fillReplenishMaxDelaySeconds);
        assertEquals(AppConfig.DEFAULT_PRICE_JITTER_TICKS, strategy.priceJitterTicks);
        assertEquals(2, strategy.bestLevels);
        assertEquals(AppConfig.DEFAULT_BEST_REFRESH_SECONDS, strategy.bestLevelRefreshSeconds);
    }

    @Test
    void validatesQuoteRefreshConfiguration() {
        AppConfig config = minimal();
        config.normalizeStrategySizing();
        config.instruments.getFirst().strategy.quoteRefreshSeconds = 4;
        assertThrows(IllegalArgumentException.class, config::validate);
        config.instruments.getFirst().strategy.quoteRefreshSeconds = 5;
        assertDoesNotThrow(config::validate);
        config.instruments.getFirst().strategy.quoteRefreshSeconds = 45;
        config.instruments.getFirst().strategy.maxOrderLifetimeSeconds = 20;
        config.instruments.getFirst().strategy.minOrderLifetimeSeconds = 30;
        assertThrows(IllegalArgumentException.class, config::validate);
    }

    @Test
    void serializesQuoteRefreshFieldsWithGoCompatibleNames() {
        AppConfig config = minimal();
        config.normalizeStrategySizing();
        String payload = Json.write(config);
        assertTrue(payload.contains("\"quote_refresh_seconds\":45"));
        assertTrue(payload.contains("\"quote_refresh_ratio_bps\":1000"));
        assertTrue(payload.contains("\"min_order_lifetime_seconds\":30"));
        assertTrue(payload.contains("\"fill_replenish_min_delay_seconds\":3"));
        assertTrue(payload.contains("\"fill_replenish_max_delay_seconds\":8"));
        assertTrue(payload.contains("\"best_level_refresh_seconds\":90"));
        AppConfig decoded = Json.read(payload, AppConfig.class);
        assertEquals(45, decoded.instruments.getFirst().strategy.quoteRefreshSeconds);
    }

    static AppConfig minimal() {
        AppConfig config = new AppConfig();
        config.mode = Domain.Mode.shadow;
        config.pollIntervalMs = 1000;
        config.rpc.urls = List.of("https://rpc.example");
        config.rpc.chainId = 56;
        config.rpc.requestTimeoutMs = 1000;

        AppConfig.InstrumentConfig instrument = new AppConfig.InstrumentConfig();
        instrument.id = "TEST-USDT";
        instrument.base.address = "0x0000000000000000000000000000000000000001";
        instrument.quote.address = "0x0000000000000000000000000000000000000002";
        instrument.reference.type = "pancake_v2";
        instrument.reference.twapWindowSeconds = 60;
        AppConfig.PairLegConfig leg = new AppConfig.PairLegConfig();
        leg.pairAddress = "0x0000000000000000000000000000000000000003";
        leg.baseToken = instrument.base.address;
        leg.quoteToken = instrument.quote.address;
        instrument.reference.legs = List.of(leg);
        instrument.strategy.levels = 2;
        instrument.strategy.orderSize = DecimalValue.parse("1");
        config.instruments = List.of(instrument);

        AppConfig.VenueMarketConfig market = new AppConfig.VenueMarketConfig();
        market.symbol = "TESTUSDT";
        market.baseAsset = "TEST";
        market.quoteAsset = "USDT";
        market.priceTick = DecimalValue.parse("0.01");
        market.quantityStep = DecimalValue.parse("0.001");
        market.minNotional = DecimalValue.parse("5");
        AppConfig.VenueConfig venue = new AppConfig.VenueConfig();
        venue.type = "binance";
        venue.environment = "production";
        venue.enabled = true;
        venue.baseUrl = "https://api.binance.com";
        venue.markets = Map.of(instrument.id, market);
        config.venues = Map.of("binance", venue);
        return config;
    }
}
