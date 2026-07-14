package com.fluxmaker.strategy;

import com.fluxmaker.config.AppConfig;
import com.fluxmaker.domain.Domain;
import com.fluxmaker.math.DecimalValue;
import org.junit.jupiter.api.Test;

import java.time.Instant;
import java.util.List;

import static org.junit.jupiter.api.Assertions.assertEquals;
import static org.junit.jupiter.api.Assertions.assertFalse;
import static org.junit.jupiter.api.Assertions.assertTrue;

class QuoteGeneratorTest {
    @Test void seedsEmptyBookAndKeepsUniqueLevels() {
        AppConfig.InstrumentConfig instrument = new AppConfig.InstrumentConfig(); instrument.id = "A-B";
        instrument.strategy.levels = 3; instrument.strategy.halfSpreadBps = 10; instrument.strategy.levelSpacingBps = 1; instrument.strategy.orderSize = DecimalValue.parse("1");
        AppConfig.VenueMarketConfig market = new AppConfig.VenueMarketConfig(); market.symbol = "AB"; market.priceTick = DecimalValue.parse("0.1"); market.quantityStep = DecimalValue.parse("0.01"); market.minNotional = DecimalValue.parse("1");
        Domain.ReferencePrice reference = new Domain.ReferencePrice(); reference.price = DecimalValue.parse("100"); reference.validUntil = Instant.now().plusSeconds(10);
        List<Domain.Quote> quotes = new QuoteGenerator().generate(instrument, "venue", market, reference, new Domain.Book(), DecimalValue.ZERO);
        assertEquals(6, quotes.size());
        assertTrue(quotes.get(2).price.compareTo(quotes.get(0).price) < 0);
        assertTrue(quotes.get(3).price.compareTo(quotes.get(1).price) > 0);
    }

    @Test void rotatesOnlyScheduledDeepLevels() {
        AppConfig.InstrumentConfig instrument = rangedInstrument(10);
        instrument.strategy.quoteRefreshSeconds = 45;
        instrument.strategy.quoteRefreshRatioBps = 2500;
        instrument.strategy.priceJitterTicks = 2;
        instrument.strategy.bestLevels = 2;
        instrument.strategy.bestLevelRefreshSeconds = 3600;
        AppConfig.VenueMarketConfig market = market();
        Domain.ReferencePrice reference = reference();
        Instant start = Instant.ofEpochSecond(4 * 3600 + 5);
        QuoteGenerator generator = new QuoteGenerator();
        List<Domain.Quote> first = generator.generateAt(instrument, "binance", market, reference, new Domain.Book(), DecimalValue.ZERO, start);
        List<Domain.Quote> second = generator.generateAt(instrument, "binance", market, reference, new Domain.Book(), DecimalValue.ZERO, start.plusSeconds(45));

        int changedBuy = 0, changedSell = 0;
        for (int index = 0; index < first.size(); index++) {
            boolean changed = !first.get(index).price.equals(second.get(index).price) || !first.get(index).quantity.equals(second.get(index).quantity);
            if (index < 4) assertFalse(changed, "best level changed before its slower refresh window");
            if (!changed) continue;
            if (first.get(index).side == Domain.Side.BUY) changedBuy++; else changedSell++;
        }
        assertEquals(2, changedBuy);
        assertEquals(2, changedSell);
    }

    @Test void remainsStableInsideRefreshWindow() {
        AppConfig.InstrumentConfig instrument = rangedInstrument(5);
        instrument.strategy.quoteRefreshSeconds = 45;
        instrument.strategy.quoteRefreshRatioBps = 1000;
        instrument.strategy.priceJitterTicks = 2;
        instrument.strategy.bestLevels = 3;
        instrument.strategy.bestLevelRefreshSeconds = 90;
        Instant start = Instant.ofEpochSecond(18_005);
        QuoteGenerator generator = new QuoteGenerator();
        List<Domain.Quote> first = generator.generateAt(instrument, "binance", market(), reference(), new Domain.Book(), DecimalValue.ZERO, start);
        List<Domain.Quote> second = generator.generateAt(instrument, "binance", market(), reference(), new Domain.Book(), DecimalValue.ZERO, start.plusSeconds(10));
        for (int index = 0; index < first.size(); index++) {
            assertEquals(first.get(index).price, second.get(index).price);
            assertEquals(first.get(index).quantity, second.get(index).quantity);
        }
    }

    @Test void capsPriceJitterByRepriceThreshold() {
        assertEquals(0, QuoteGenerator.boundedJitterTicks(DecimalValue.parse("1"), DecimalValue.parse("0.1"), 3, 10));
        assertEquals(3, QuoteGenerator.boundedJitterTicks(DecimalValue.parse("100"), DecimalValue.parse("0.01"), 3, 10));
    }

    private static AppConfig.InstrumentConfig rangedInstrument(int levels) {
        AppConfig.InstrumentConfig instrument = new AppConfig.InstrumentConfig();
        instrument.id = "token_usdt";
        instrument.strategy.halfSpreadBps = 50;
        instrument.strategy.levelSpacingBps = 25;
        instrument.strategy.levels = levels;
        instrument.strategy.minOrderNotional = DecimalValue.parse("10");
        instrument.strategy.maxOrderNotional = DecimalValue.parse("20");
        instrument.strategy.repriceThresholdBps = 10;
        return instrument;
    }

    private static AppConfig.VenueMarketConfig market() {
        AppConfig.VenueMarketConfig market = new AppConfig.VenueMarketConfig();
        market.symbol = "TOKENUSDT";
        market.priceTick = DecimalValue.parse("0.01");
        market.quantityStep = DecimalValue.parse("0.01");
        market.minNotional = DecimalValue.parse("5");
        return market;
    }

    private static Domain.ReferencePrice reference() {
        Domain.ReferencePrice reference = new Domain.ReferencePrice();
        reference.price = DecimalValue.parse("100");
        reference.validUntil = Instant.now().plusSeconds(60);
        return reference;
    }
}
