package com.fluxmaker.engine;

import com.fluxmaker.audit.AuditLogger;
import com.fluxmaker.config.AppConfig;
import com.fluxmaker.domain.Domain;
import com.fluxmaker.math.DecimalValue;
import com.fluxmaker.venue.VenueClient;
import org.junit.jupiter.api.Test;

import java.time.Instant;
import java.util.ArrayList;
import java.util.List;
import java.util.Map;

import static org.junit.jupiter.api.Assertions.assertEquals;

class TradingEngineShutdownTest {
    @Test void normalProcessShutdownRetainsRestingOrders() {
        Fixture fixture = fixture();

        fixture.engine.shutdown();

        assertEquals(2, fixture.venue.orders.size());
        assertEquals(0, fixture.venue.cancelCalls);
    }

    private static Fixture fixture() {
        AppConfig config = new AppConfig();
        config.mode = Domain.Mode.live;
        config.watchdogTimeoutSeconds = 15;
        AppConfig.InstrumentConfig instrument = new AppConfig.InstrumentConfig();
        instrument.id = "gdt_usdt";
        config.instruments.add(instrument);
        AppConfig.VenueConfig venueConfig = new AppConfig.VenueConfig();
        venueConfig.type = "mgbx";
        venueConfig.enabled = true;
        venueConfig.tradingEnabled = true;
        AppConfig.VenueMarketConfig market = new AppConfig.VenueMarketConfig();
        market.symbol = "GDT_USDT";
        market.credentialId = 1;
        venueConfig.markets.put(instrument.id, market);
        config.venues.put("mgbx", venueConfig);

        FakeVenue venue = new FakeVenue();
        TradingEngine engine = new TradingEngine(
                config,
                ignored -> new Domain.ReferencePrice(),
                Map.of("mgbx|gdt_usdt", venue),
                null,
                new AuditLogger("", 0, 0),
                null);
        return new Fixture(engine, venue);
    }

    private record Fixture(TradingEngine engine, FakeVenue venue) {}

    private static final class FakeVenue implements VenueClient {
        private final List<Domain.Order> orders = new ArrayList<>();
        private int cancelCalls;

        private FakeVenue() {
            orders.add(order("1", Domain.Side.BUY));
            orders.add(order("2", Domain.Side.SELL));
        }

        @Override public String name() { return "mgbx/gdt_usdt"; }
        @Override public Domain.Book topBook(String symbol) { return new Domain.Book(); }
        @Override public List<Domain.Balance> balances() { return List.of(); }
        @Override public List<Domain.Order> openOrders(String symbol) { return new ArrayList<>(orders); }
        @Override public Domain.Order placePostOnly(PlaceRequest request) { throw new UnsupportedOperationException(); }
        @Override public void cancelOrder(String symbol, String orderId) {
            cancelCalls++;
            orders.removeIf(order -> order.orderId.equals(orderId));
        }

        private static Domain.Order order(String id, Domain.Side side) {
            Domain.Order order = new Domain.Order();
            order.orderId = id;
            order.clientId = "fm-existing-" + id;
            order.symbol = "GDT_USDT";
            order.side = side;
            order.price = DecimalValue.parse("0.38");
            order.quantity = DecimalValue.ONE;
            order.state = Domain.OrderState.NEW;
            order.createdAt = Instant.now();
            return order;
        }
    }
}
