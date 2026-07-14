package com.fluxmaker.tradesim;

import com.fluxmaker.config.AppConfig;
import com.fluxmaker.domain.Domain;
import com.fluxmaker.math.DecimalValue;
import org.junit.jupiter.api.Test;

import java.time.Instant;

import static org.junit.jupiter.api.Assertions.assertEquals;
import static org.junit.jupiter.api.Assertions.assertNotNull;
import static org.junit.jupiter.api.Assertions.assertNull;
import static org.junit.jupiter.api.Assertions.assertTrue;

class TradeSimulatorTest {
    @Test void createsOnlyInternalFillsInsideBook() {
        AppConfig.InstrumentConfig instrument = new AppConfig.InstrumentConfig(); instrument.id = "A-B"; instrument.tradeSimulation.enabled = true; instrument.tradeSimulation.sourceVenue = "v"; instrument.tradeSimulation.minQuantity = DecimalValue.parse("1"); instrument.tradeSimulation.maxQuantity = DecimalValue.parse("2"); instrument.tradeSimulation.minIntervalMs = 100; instrument.tradeSimulation.maxIntervalMs = 100; instrument.tradeSimulation.buyProbabilityBps = 5000; instrument.tradeSimulation.recentLimit = 10;
        AppConfig.VenueMarketConfig market = new AppConfig.VenueMarketConfig(); market.symbol = "AB"; market.priceTick = DecimalValue.parse("1"); market.quantityStep = DecimalValue.parse("1");
        Domain.Book book = new Domain.Book(); book.bidPrice = DecimalValue.parse("100"); book.askPrice = DecimalValue.parse("104");
        TradeSimulator simulator = new TradeSimulator(); Instant start = Instant.now(); simulator.observe(instrument, "v", market, book, start); TradeSimulator.Observation result = simulator.observe(instrument, "v", market, book, start.plusMillis(100));
        assertTrue(result.fill().simulated); assertTrue(result.fill().price.compareTo(book.bidPrice) > 0); assertTrue(result.fill().price.compareTo(book.askPrice) < 0);
    }

    @Test void customVolumePlannerCanOnlyReturnAnInternalEventPlan() {
        AppConfig.InstrumentConfig instrument = instrument();
        AppConfig.VenueMarketConfig market = market();
        Domain.Book book = book();
        VolumeSimulationPlanner planner = request -> new VolumeSimulationPlanner.EventPlan(
                Domain.Side.SELL, DecimalValue.parse("102"), DecimalValue.parse("3"));
        TradeSimulator simulator = new TradeSimulator(planner);
        Instant start = Instant.parse("2026-07-14T00:00:00Z");

        simulator.observe(instrument, "v", market, book, start);
        TradeSimulator.Observation result = simulator.observe(instrument, "v", market, book, start.plusMillis(100));

        assertNotNull(result.fill());
        assertTrue(result.fill().simulated, "the framework, not an extension, must set the internal-only marker");
        assertEquals("SIM-A-B-1783987200100-1", result.fill().tradeId);
        assertEquals(DecimalValue.parse("306"), result.fill().quoteQuantity);
        assertEquals(Domain.Side.SELL, result.fill().side);
    }

    @Test void frameworkRejectsAPlannerEventOutsideTheReadOnlyMarketEnvelope() {
        VolumeSimulationPlanner planner = request -> new VolumeSimulationPlanner.EventPlan(
                Domain.Side.BUY, DecimalValue.parse("104"), DecimalValue.parse("1"));
        TradeSimulator simulator = new TradeSimulator(planner);
        Instant start = Instant.parse("2026-07-14T00:00:00Z");

        simulator.observe(instrument(), "v", market(), book(), start);
        TradeSimulator.Observation result = simulator.observe(
                instrument(), "v", market(), book(), start.plusMillis(100));

        assertNull(result.fill());
        assertEquals("skipped", result.snapshot().status);
        assertTrue(result.snapshot().error.contains("strictly inside bid/ask"));
    }

    private static AppConfig.InstrumentConfig instrument() {
        AppConfig.InstrumentConfig instrument = new AppConfig.InstrumentConfig(); instrument.id = "A-B"; instrument.tradeSimulation.enabled = true; instrument.tradeSimulation.sourceVenue = "v"; instrument.tradeSimulation.minQuantity = DecimalValue.parse("1"); instrument.tradeSimulation.maxQuantity = DecimalValue.parse("4"); instrument.tradeSimulation.minIntervalMs = 100; instrument.tradeSimulation.maxIntervalMs = 100; instrument.tradeSimulation.buyProbabilityBps = 5000; instrument.tradeSimulation.recentLimit = 10;
        return instrument;
    }

    private static AppConfig.VenueMarketConfig market() {
        AppConfig.VenueMarketConfig market = new AppConfig.VenueMarketConfig(); market.symbol = "AB"; market.priceTick = DecimalValue.parse("1"); market.quantityStep = DecimalValue.parse("1");
        return market;
    }

    private static Domain.Book book() {
        Domain.Book book = new Domain.Book(); book.bidPrice = DecimalValue.parse("100"); book.askPrice = DecimalValue.parse("104");
        return book;
    }
}
