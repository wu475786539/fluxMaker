package com.fluxmaker.tradesim;

import com.fluxmaker.config.AppConfig;
import com.fluxmaker.domain.Domain;
import com.fluxmaker.math.DecimalValue;
import org.junit.jupiter.api.Test;

import java.time.Instant;
import java.util.List;

import static org.junit.jupiter.api.Assertions.assertEquals;
import static org.junit.jupiter.api.Assertions.assertNotNull;
import static org.junit.jupiter.api.Assertions.assertTrue;

class TradeSimulatorTest {
    @Test void createsOnlyInternalFillsInsideBook() {
        AppConfig.InstrumentConfig instrument = new AppConfig.InstrumentConfig(); instrument.id = "A-B"; instrument.tradeSimulation.enabled = true; instrument.tradeSimulation.sourceVenue = "v"; instrument.tradeSimulation.minQuantity = DecimalValue.parse("1"); instrument.tradeSimulation.maxQuantity = DecimalValue.parse("2"); instrument.tradeSimulation.minIntervalMs = 100; instrument.tradeSimulation.maxIntervalMs = 100; instrument.tradeSimulation.buyProbabilityBps = 5000; instrument.tradeSimulation.recentLimit = 10; instrument.tradeSimulation.batchSize = 1;
        AppConfig.VenueMarketConfig market = new AppConfig.VenueMarketConfig(); market.symbol = "AB"; market.priceTick = DecimalValue.parse("1"); market.quantityStep = DecimalValue.parse("1");
        Domain.Book book = new Domain.Book(); book.bidPrice = DecimalValue.parse("100"); book.askPrice = DecimalValue.parse("104");
        TradeSimulator simulator = new TradeSimulator(); Instant start = Instant.now(); simulator.observe(instrument, "v", market, book, start); TradeSimulator.Observation result = simulator.observe(instrument, "v", market, book, start.plusMillis(100));
        assertEquals(1, result.fills().size()); Domain.Fill fill = result.fills().get(0); assertTrue(fill.simulated); assertTrue(fill.price.compareTo(book.bidPrice) > 0); assertTrue(fill.price.compareTo(book.askPrice) < 0);
    }

    @Test void customVolumePlannerCanOnlyReturnAnInternalEventPlan() {
        AppConfig.InstrumentConfig instrument = instrument();
        AppConfig.VenueMarketConfig market = market();
        Domain.Book book = book();
        VolumeSimulationPlanner planner = request -> List.of(new VolumeSimulationPlanner.EventPlan(
                Domain.Side.SELL, DecimalValue.parse("102"), DecimalValue.parse("3")));
        TradeSimulator simulator = new TradeSimulator(planner);
        Instant start = Instant.parse("2026-07-14T00:00:00Z");

        simulator.observe(instrument, "v", market, book, start);
        TradeSimulator.Observation result = simulator.observe(instrument, "v", market, book, start.plusMillis(100));

        assertEquals(1, result.fills().size());
        Domain.Fill fill = result.fills().get(0);
        assertTrue(fill.simulated, "the framework, not an extension, must set the internal-only marker");
        assertEquals("SIM-A-B-1783987200100-1", fill.tradeId);
        assertEquals(DecimalValue.parse("306"), fill.quoteQuantity);
        assertEquals(Domain.Side.SELL, fill.side);
    }

    @Test void frameworkRejectsAPlannerEventOutsideTheReadOnlyMarketEnvelope() {
        VolumeSimulationPlanner planner = request -> List.of(new VolumeSimulationPlanner.EventPlan(
                Domain.Side.BUY, DecimalValue.parse("104"), DecimalValue.parse("1")));
        TradeSimulator simulator = new TradeSimulator(planner);
        Instant start = Instant.parse("2026-07-14T00:00:00Z");

        simulator.observe(instrument(), "v", market(), book(), start);
        TradeSimulator.Observation result = simulator.observe(
                instrument(), "v", market(), book(), start.plusMillis(100));

        assertTrue(result.fills().isEmpty());
        assertEquals("skipped", result.snapshot().status);
        assertTrue(result.snapshot().error.contains("strictly inside bid/ask"));
    }

    @Test void batchSizeProducesMultipleFillsWithIncrementingSequence() {
        AppConfig.InstrumentConfig instrument = instrument();
        instrument.tradeSimulation.batchSize = 3;
        VolumeSimulationPlanner planner = request -> List.of(
                new VolumeSimulationPlanner.EventPlan(Domain.Side.SELL, DecimalValue.parse("101"), DecimalValue.parse("2")),
                new VolumeSimulationPlanner.EventPlan(Domain.Side.BUY, DecimalValue.parse("102"), DecimalValue.parse("2")),
                new VolumeSimulationPlanner.EventPlan(Domain.Side.SELL, DecimalValue.parse("103"), DecimalValue.parse("2"))
        );
        TradeSimulator simulator = new TradeSimulator(planner);
        Instant start = Instant.parse("2026-07-14T00:00:00Z");

        simulator.observe(instrument(), "v", market(), book(), start);
        TradeSimulator.Observation result = simulator.observe(
                instrument(), "v", market(), book(), start.plusMillis(100));

        assertEquals(3, result.fills().size());
        assertEquals("SIM-A-B-1783987200100-1", result.fills().get(0).tradeId);
        assertEquals("SIM-A-B-1783987200100-2", result.fills().get(1).tradeId);
        assertEquals("SIM-A-B-1783987200100-3", result.fills().get(2).tradeId);
        assertEquals(3, result.snapshot().fills.size(), "snapshot should contain all 3 fills");
    }

    private static AppConfig.InstrumentConfig instrument() {
        AppConfig.InstrumentConfig instrument = new AppConfig.InstrumentConfig(); instrument.id = "A-B"; instrument.tradeSimulation.enabled = true; instrument.tradeSimulation.sourceVenue = "v"; instrument.tradeSimulation.minQuantity = DecimalValue.parse("1"); instrument.tradeSimulation.maxQuantity = DecimalValue.parse("4"); instrument.tradeSimulation.minIntervalMs = 100; instrument.tradeSimulation.maxIntervalMs = 100; instrument.tradeSimulation.buyProbabilityBps = 5000; instrument.tradeSimulation.recentLimit = 10; instrument.tradeSimulation.batchSize = 1;
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
