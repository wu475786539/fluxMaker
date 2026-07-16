package com.fluxmaker.tradesim;

import com.fluxmaker.config.AppConfig;
import com.fluxmaker.domain.Domain;
import com.fluxmaker.math.DecimalValue;
import org.junit.jupiter.api.Test;

import java.time.Instant;
import java.util.HashSet;
import java.util.Random;
import java.util.Set;

import static org.junit.jupiter.api.Assertions.assertEquals;
import static org.junit.jupiter.api.Assertions.assertTrue;

class VolumeSimulationPlannerImplTest {
    @Test void walksGraduallyBetweenTickAlignedPricesStrictlyInsideTheBook() {
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

        VolumeSimulationPlannerImpl planner = new VolumeSimulationPlannerImpl(new Random(7));
        Set<DecimalValue> prices = new HashSet<>();
        DecimalValue previousPrice = null;
        DecimalValue maximumMovement = DecimalValue.parse("0.00074");
        for (long sequence = 1; sequence <= 50; sequence++) {
            VolumeSimulationPlanner.EventPlan plan = planner.plan(new VolumeSimulationPlanner.Request(
                    instrument, "mgbx", market, book, previousPrice, Instant.EPOCH, sequence));
            assertTrue(plan.price().compareTo(book.bidPrice) > 0);
            assertTrue(plan.price().compareTo(book.askPrice) < 0);
            assertEquals(plan.price(), plan.price().quantizeDown(market.priceTick));
            if (previousPrice != null) {
                assertTrue(
                        plan.price().subtract(previousPrice).abs().compareTo(maximumMovement) <= 0,
                        "the next simulated price should stay close to the previous event"
                );
            }
            prices.add(plan.price());
            previousPrice = plan.price();
        }

        assertTrue(prices.size() > 1, "the gradual walk should still move over time");
    }
}
