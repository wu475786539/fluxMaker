package com.fluxmaker.tradesim;

import com.fluxmaker.config.AppConfig;
import com.fluxmaker.domain.Domain;
import com.fluxmaker.math.DecimalValue;
import org.junit.jupiter.api.Test;

import java.time.Instant;

import static org.junit.jupiter.api.Assertions.assertTrue;

class TradeSimulatorTest {
    @Test void createsOnlyInternalFillsInsideBook() {
        AppConfig.InstrumentConfig instrument = new AppConfig.InstrumentConfig(); instrument.id = "A-B"; instrument.tradeSimulation.enabled = true; instrument.tradeSimulation.sourceVenue = "v"; instrument.tradeSimulation.minQuantity = DecimalValue.parse("1"); instrument.tradeSimulation.maxQuantity = DecimalValue.parse("2"); instrument.tradeSimulation.minIntervalMs = 100; instrument.tradeSimulation.maxIntervalMs = 100; instrument.tradeSimulation.buyProbabilityBps = 5000; instrument.tradeSimulation.recentLimit = 10;
        AppConfig.VenueMarketConfig market = new AppConfig.VenueMarketConfig(); market.symbol = "AB"; market.priceTick = DecimalValue.parse("1"); market.quantityStep = DecimalValue.parse("1");
        Domain.Book book = new Domain.Book(); book.bidPrice = DecimalValue.parse("100"); book.askPrice = DecimalValue.parse("104");
        TradeSimulator simulator = new TradeSimulator(); Instant start = Instant.now(); simulator.observe(instrument, "v", market, book, start); TradeSimulator.Observation result = simulator.observe(instrument, "v", market, book, start.plusMillis(100));
        assertTrue(result.fill().simulated); assertTrue(result.fill().price.compareTo(book.bidPrice) > 0); assertTrue(result.fill().price.compareTo(book.askPrice) < 0);
    }
}
