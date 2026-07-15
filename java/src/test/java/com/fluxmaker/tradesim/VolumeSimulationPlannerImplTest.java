package com.fluxmaker.tradesim;

import com.fluxmaker.config.AppConfig;
import com.fluxmaker.domain.Domain;
import com.fluxmaker.math.DecimalValue;
import org.junit.jupiter.api.Test;

import java.time.Instant;
import java.util.HashSet;
import java.util.Set;

import static org.junit.jupiter.api.Assertions.assertEquals;
import static org.junit.jupiter.api.Assertions.assertTrue;

class VolumeSimulationPlannerImplTest {
    @Test void selectsVaryingSidesAndTickAlignedPricesIndependentlyOfSequence() {
        AppConfig.InstrumentConfig instrument = new AppConfig.InstrumentConfig();
        instrument.id = "gdt_usdt";
        instrument.tradeSimulation.minQuantity = DecimalValue.parse("30");
        instrument.tradeSimulation.maxQuantity = DecimalValue.parse("50");

        AppConfig.VenueMarketConfig market = new AppConfig.VenueMarketConfig();
        market.symbol = "GDT_USDT";
        market.priceTick = DecimalValue.parse("0.00001");
        market.quantityStep = DecimalValue.parse("0.01");

        Domain.Book book = new Domain.Book();
        book.bidPrice = DecimalValue.parse("0.37195");
        book.askPrice = DecimalValue.parse("0.37562");

        VolumeSimulationPlannerImpl planner = new VolumeSimulationPlannerImpl();
        Set<DecimalValue> prices = new HashSet<>();
        Set<Domain.Side> sides = new HashSet<>();
        for (int attempt = 0; attempt < 50; attempt++) {
            VolumeSimulationPlanner.EventPlan plan = planner.plan(new VolumeSimulationPlanner.Request(
                    instrument, "mgbx", market, book, Instant.EPOCH, 1));
            assertTrue(plan.price().compareTo(book.bidPrice) > 0);
            assertTrue(plan.price().compareTo(book.askPrice) < 0);
            assertEquals(plan.price(), plan.price().quantizeDown(market.priceTick));
            prices.add(plan.price());
            sides.add(plan.side());
        }

        assertTrue(prices.size() > 1, "prices should vary across the legal ticks inside bid/ask");
        assertEquals(Set.of(Domain.Side.BUY, Domain.Side.SELL), sides,
                "side should be random instead of being derived from sequence");
    }
}
