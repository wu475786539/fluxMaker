package com.fluxmaker.engine;

import com.fluxmaker.config.AppConfig;
import com.fluxmaker.domain.Domain;
import com.fluxmaker.math.DecimalValue;
import com.fluxmaker.risk.RiskEngine;
import com.fluxmaker.strategy.QuoteGenerator;
import org.junit.jupiter.api.Test;

import java.time.Instant;
import java.util.List;

import static org.junit.jupiter.api.Assertions.assertEquals;
import static org.junit.jupiter.api.Assertions.assertFalse;
import static org.junit.jupiter.api.Assertions.assertTrue;

class TradingEngineBookRebuildTest {
    @Test void rebuildsOnlyAfterAConfirmedIncompleteBookWithNoManagedOrders() {
        Domain.Book empty = new Domain.Book();
        assertTrue(TradingEngine.shouldStartBookRebuild(true, empty, List.of()));
        assertFalse(TradingEngine.shouldStartBookRebuild(false, empty, List.of()),
                "a failed book request must never be treated as a confirmed empty market");
        assertFalse(TradingEngine.shouldStartBookRebuild(true, empty, List.of(order())),
                "existing managed depth must be retained instead of entering startup rebuild");

        Domain.Book oneSided = new Domain.Book();
        oneSided.bidPrice = DecimalValue.parse("0.37");
        assertTrue(TradingEngine.shouldStartBookRebuild(true, oneSided, List.of()));

        Domain.Book healthy = new Domain.Book();
        healthy.bidPrice = DecimalValue.parse("0.37");
        healthy.askPrice = DecimalValue.parse("0.38");
        assertFalse(TradingEngine.shouldStartBookRebuild(true, healthy, List.of()));
    }

    @Test void configurationDefaultsToEnabledAndCanExplicitlyDisableRebuild() {
        AppConfig.StrategyConfig strategy = new AppConfig.StrategyConfig();
        assertTrue(strategy.effectiveStartupBookRebuildEnabled());
        strategy.startupBookRebuildEnabled = false;
        assertFalse(strategy.effectiveStartupBookRebuildEnabled());
    }

    @Test void manualRebuildSeedsAroundReferenceInsideAnExtremeButValidSpread() {
        AppConfig.InstrumentConfig instrument = new AppConfig.InstrumentConfig();
        instrument.id = "gdt_usdt";
        instrument.strategy.levels = 3;
        instrument.strategy.halfSpreadBps = 50;
        instrument.strategy.levelSpacingBps = 25;
        instrument.strategy.orderSize = DecimalValue.parse("100");
        instrument.strategy.repriceThresholdBps = 10;
        instrument.strategy.targetBase = DecimalValue.parse("50");
        instrument.strategy.maxBaseDeviation = DecimalValue.parse("10");

        AppConfig.VenueMarketConfig market = new AppConfig.VenueMarketConfig();
        market.symbol = "GDT_USDT";
        market.priceTick = DecimalValue.parse("0.00001");
        market.quantityStep = DecimalValue.parse("0.01");

        Domain.ReferencePrice reference = new Domain.ReferencePrice();
        reference.price = DecimalValue.parse("0.38768");
        reference.validUntil = Instant.now().plusSeconds(30);
        Domain.Book book = new Domain.Book();
        book.bidPrice = DecimalValue.parse("0.00035");
        book.askPrice = DecimalValue.parse("36589");
        book.timestamp = Instant.now();

        DecimalValue actualInventory = DecimalValue.parse("1000");
        List<Domain.Quote> ordinaryCandidates = new RiskEngine().filterQuotes(
                instrument, market, book, actualInventory,
                new QuoteGenerator().generate(instrument, "mgbx", market, reference, book, actualInventory));
        assertTrue(ordinaryCandidates.stream().allMatch(quote -> quote.side == Domain.Side.SELL),
                "ordinary inventory protection should still suppress buys outside the configured range");

        DecimalValue rebuildInventory = TradingEngine.manualRebuildInventoryAnchor(instrument);
        List<Domain.Quote> candidates = new RiskEngine().filterQuotes(
                instrument, market, book, rebuildInventory,
                new QuoteGenerator().generate(instrument, "mgbx", market, reference, book, rebuildInventory));
        List<Domain.Quote> seeds = TradingEngine.bestQuotePerSide(candidates);

        assertEquals(instrument.strategy.targetBase, rebuildInventory);
        assertEquals(2, seeds.size());
        assertEquals(Domain.Side.BUY, seeds.get(0).side);
        assertEquals(Domain.Side.SELL, seeds.get(1).side);
        assertTrue(seeds.get(0).price.compareTo(book.bidPrice) > 0);
        assertTrue(seeds.get(0).price.compareTo(reference.price) < 0);
        assertTrue(seeds.get(1).price.compareTo(reference.price) > 0);
        assertTrue(seeds.get(1).price.compareTo(book.askPrice) < 0);
    }

    private static Domain.Order order() {
        Domain.Order order = new Domain.Order();
        order.orderId = "existing";
        return order;
    }
}
