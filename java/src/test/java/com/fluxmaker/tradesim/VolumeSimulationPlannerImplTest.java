package com.fluxmaker.tradesim;

import com.fluxmaker.config.AppConfig;
import com.fluxmaker.domain.Domain;
import com.fluxmaker.math.DecimalValue;
import org.junit.jupiter.api.Test;

import java.time.Instant;
import java.util.HashSet;
import java.util.List;
import java.util.Set;

import static org.junit.jupiter.api.Assertions.assertEquals;
import static org.junit.jupiter.api.Assertions.assertTrue;

class VolumeSimulationPlannerImplTest {
    @Test void selectsVaryingTickAlignedPricesStrictlyInsideTheBook() {
        AppConfig.InstrumentConfig instrument = new AppConfig.InstrumentConfig();
        instrument.id = "gdt_usdt";
        instrument.tradeSimulation.minQuantity = DecimalValue.parse("30");
        instrument.tradeSimulation.maxQuantity = DecimalValue.parse("50");
        instrument.tradeSimulation.batchSize = 1;

        AppConfig.VenueMarketConfig market = new AppConfig.VenueMarketConfig();
        market.symbol = "GDT_USDT";
        market.priceTick = DecimalValue.parse("0.00001");
        market.quantityStep = DecimalValue.parse("0.01");

        Domain.Book book = new Domain.Book();
        book.bidPrice = DecimalValue.parse("0.37195");
        book.askPrice = DecimalValue.parse("0.37562");

        VolumeSimulationPlannerImpl planner = new VolumeSimulationPlannerImpl();
        Set<DecimalValue> prices = new HashSet<>();
        for (long sequence = 1; sequence <= 50; sequence++) {
            List<VolumeSimulationPlanner.EventPlan> plans = planner.plan(new VolumeSimulationPlanner.Request(
                    instrument, "mgbx", market, book, Instant.EPOCH, sequence));
            assertEquals(1, plans.size(), "batchSize=1 should return a single plan");
            VolumeSimulationPlanner.EventPlan plan = plans.get(0);
            assertTrue(plan.price().compareTo(book.bidPrice) > 0);
            assertTrue(plan.price().compareTo(book.askPrice) < 0);
            assertEquals(plan.price(), plan.price().quantizeDown(market.priceTick));
            prices.add(plan.price());
        }

        assertTrue(prices.size() > 1, "prices should vary across the legal ticks inside bid/ask");
    }

    @Test void batchSizeProducesMultiplePlans() {
        AppConfig.InstrumentConfig instrument = new AppConfig.InstrumentConfig();
        instrument.id = "gdt_usdt";
        instrument.tradeSimulation.minQuantity = DecimalValue.parse("10");
        instrument.tradeSimulation.maxQuantity = DecimalValue.parse("100");
        instrument.tradeSimulation.batchSize = 5;

        AppConfig.VenueMarketConfig market = new AppConfig.VenueMarketConfig();
        market.symbol = "GDT_USDT";
        market.priceTick = DecimalValue.parse("0.00001");
        market.quantityStep = DecimalValue.parse("0.01");

        Domain.Book book = new Domain.Book();
        book.bidPrice = DecimalValue.parse("0.37195");
        book.askPrice = DecimalValue.parse("0.37562");

        VolumeSimulationPlannerImpl planner = new VolumeSimulationPlannerImpl();
        List<VolumeSimulationPlanner.EventPlan> plans = planner.plan(new VolumeSimulationPlanner.Request(
                instrument, "mgbx", market, book, Instant.EPOCH, 1));

        assertEquals(5, plans.size(), "batchSize=5 should return 5 plans");
        for (VolumeSimulationPlanner.EventPlan plan : plans) {
            assertTrue(plan.price().compareTo(book.bidPrice) > 0,
                    "price " + plan.price() + " must be > bid " + book.bidPrice);
            assertTrue(plan.price().compareTo(book.askPrice) < 0,
                    "price " + plan.price() + " must be < ask " + book.askPrice);
            assertEquals(plan.price(), plan.price().quantizeDown(market.priceTick),
                    "price must be tick-aligned");
            assertTrue(plan.quantity().isPositive(), "quantity must be positive");
            assertEquals(plan.quantity(), plan.quantity().quantizeDown(market.quantityStep),
                    "quantity must be step-aligned");
        }
    }

    @Test void batchSizeZeroDefaultsToOne() {
        AppConfig.InstrumentConfig instrument = new AppConfig.InstrumentConfig();
        instrument.id = "gdt_usdt";
        instrument.tradeSimulation.minQuantity = DecimalValue.parse("30");
        instrument.tradeSimulation.maxQuantity = DecimalValue.parse("50");
        instrument.tradeSimulation.batchSize = 0;

        AppConfig.VenueMarketConfig market = new AppConfig.VenueMarketConfig();
        market.symbol = "GDT_USDT";
        market.priceTick = DecimalValue.parse("0.00001");
        market.quantityStep = DecimalValue.parse("0.01");

        Domain.Book book = new Domain.Book();
        book.bidPrice = DecimalValue.parse("0.37195");
        book.askPrice = DecimalValue.parse("0.37562");

        VolumeSimulationPlannerImpl planner = new VolumeSimulationPlannerImpl();
        List<VolumeSimulationPlanner.EventPlan> plans = planner.plan(new VolumeSimulationPlanner.Request(
                instrument, "mgbx", market, book, Instant.EPOCH, 1));

        assertEquals(1, plans.size(), "batchSize=0 should default to 1 plan");
    }
}
