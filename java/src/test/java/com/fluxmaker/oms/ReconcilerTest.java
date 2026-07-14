package com.fluxmaker.oms;

import com.fluxmaker.domain.Domain;
import com.fluxmaker.math.DecimalValue;
import com.fluxmaker.venue.VenueClient;
import org.junit.jupiter.api.Test;

import java.time.Duration;
import java.time.Instant;
import java.util.ArrayList;
import java.util.List;

import static org.junit.jupiter.api.Assertions.assertEquals;
import static org.junit.jupiter.api.Assertions.assertFalse;
import static org.junit.jupiter.api.Assertions.assertTrue;

class ReconcilerTest {
    @Test void comparesPricesWithExactBasisPoints() {
        assertTrue(Reconciler.withinBps(DecimalValue.parse("100.01"), DecimalValue.parse("100"), 1));
        assertFalse(Reconciler.withinBps(DecimalValue.parse("100.02"), DecimalValue.parse("100"), 1));
    }
    @Test void clientIdsCarryFenceGeneration() {
        Domain.Quote quote = new Domain.Quote(); quote.venue = "v"; quote.side = Domain.Side.BUY; quote.level = 1;
        assertTrue(Reconciler.clientId("A-B", quote, 35).startsWith("fm-gz-"));
    }

    @Test void keptRequiresExactQuantity() {
        // A managed order at an acceptable price but the wrong remaining size must
        // not be treated as a match — it has to be canceled, not kept. Guards the
        // quantity gate that short-circuits withinBps.
        List<Domain.Quote> quotes = target(1, 0); // one buy + one sell, quantity 1
        List<Domain.Order> orders = ordersFor(quotes, DecimalValue.parse("2"), Instant.now());
        FakeVenue venue = new FakeVenue(orders);
        Reconciler.Result result = new Reconciler(null).reconcileWithOrders(venue, "token_usdt", quotes, 10, orders, null, 0);
        assertEquals(0, result.kept, "price match with wrong quantity must not be kept");
        assertEquals(2, result.canceled, "wrong-sized orders must be canceled");
    }

    @Test void routineRefreshIsLimitedPerCycle() {
        List<Domain.Quote> quotes = target(5, 0);
        List<Domain.Order> orders = ordersFor(quotes, DecimalValue.parse("2"), Instant.now().minusSeconds(60));
        FakeVenue venue = new FakeVenue(orders);
        Reconciler.RefreshPolicy policy = new Reconciler.RefreshPolicy(Duration.ofSeconds(30), Duration.ofMinutes(5), 2);
        Reconciler.Result result = new Reconciler(null).reconcileWithOrders(venue, "token_usdt", quotes, 10, orders, null, 0, policy);
        assertEquals(2, result.canceled);
        assertEquals(8, result.kept);
        assertEquals(8, venue.orders.size());
    }

    @Test void routineRefreshKeepsYoungOrders() {
        List<Domain.Quote> quotes = target(5, 0);
        List<Domain.Order> orders = ordersFor(quotes, DecimalValue.parse("2"), Instant.now());
        FakeVenue venue = new FakeVenue(orders);
        Reconciler.RefreshPolicy policy = new Reconciler.RefreshPolicy(Duration.ofSeconds(30), Duration.ofMinutes(5), 2);
        Reconciler.Result result = new Reconciler(null).reconcileWithOrders(venue, "token_usdt", quotes, 10, orders, null, 0, policy);
        assertEquals(0, result.canceled);
        assertEquals(orders.size(), result.kept);
    }

    @Test void maximumAgeRefreshesGradually() {
        List<Domain.Quote> quotes = target(5, 0);
        List<Domain.Order> orders = ordersFor(quotes, null, Instant.now().minusSeconds(600));
        FakeVenue venue = new FakeVenue(orders);
        Reconciler.RefreshPolicy policy = new Reconciler.RefreshPolicy(Duration.ofSeconds(30), Duration.ofMinutes(5), 2);
        Reconciler.Result result = new Reconciler(null).reconcileWithOrders(venue, "token_usdt", quotes, 10, orders, null, 0, policy);
        assertEquals(2, result.canceled);
        assertEquals(8, result.kept);
    }

    @Test void materialRepriceIsNotLimitedByRefreshPolicy() {
        List<Domain.Quote> oldQuotes = target(5, 0);
        List<Domain.Quote> newQuotes = target(5, 500);
        List<Domain.Order> orders = ordersFor(oldQuotes, null, Instant.now().minusSeconds(60));
        FakeVenue venue = new FakeVenue(orders);
        Reconciler.RefreshPolicy policy = new Reconciler.RefreshPolicy(Duration.ofSeconds(30), Duration.ofMinutes(5), 2);
        Reconciler.Result result = new Reconciler(null).reconcileWithOrders(venue, "token_usdt", newQuotes, 10, orders, null, 0, policy);
        assertEquals(orders.size(), result.canceled);
    }

    private static List<Domain.Quote> target(int levels, int offsetBps) {
        List<Domain.Quote> quotes = new ArrayList<>();
        for (int level = 0; level < levels; level++) {
            DecimalValue offset = DecimalValue.of(offsetBps + level * 10).divide(DecimalValue.TEN_THOUSAND);
            quotes.add(quote(level, Domain.Side.BUY, DecimalValue.parse("100").multiply(DecimalValue.ONE.subtract(offset))));
            quotes.add(quote(level, Domain.Side.SELL, DecimalValue.parse("100").multiply(DecimalValue.ONE.add(offset))));
        }
        return quotes;
    }

    private static Domain.Quote quote(int level, Domain.Side side, DecimalValue price) {
        Domain.Quote quote = new Domain.Quote();
        quote.instrumentId = "token_usdt"; quote.venue = "fake"; quote.symbol = "TOKENUSDT";
        quote.side = side; quote.level = level; quote.price = price; quote.quantity = DecimalValue.ONE;
        return quote;
    }

    private static List<Domain.Order> ordersFor(List<Domain.Quote> quotes, DecimalValue quantity, Instant createdAt) {
        List<Domain.Order> orders = new ArrayList<>();
        for (int index = 0; index < quotes.size(); index++) {
            Domain.Quote quote = quotes.get(index); Domain.Order order = new Domain.Order();
            order.orderId = Integer.toString(index + 1); order.clientId = "fm-old"; order.symbol = quote.symbol;
            order.side = quote.side; order.price = quote.price; order.quantity = quantity == null ? quote.quantity : quantity;
            order.state = Domain.OrderState.NEW; order.createdAt = createdAt.minusSeconds(index); orders.add(order);
        }
        return orders;
    }

    private static final class FakeVenue implements VenueClient {
        private final List<Domain.Order> orders;
        private FakeVenue(List<Domain.Order> orders) { this.orders = new ArrayList<>(orders); }
        @Override public String name() { return "fake"; }
        @Override public Domain.Book topBook(String symbol) { return new Domain.Book(); }
        @Override public List<Domain.Balance> balances() { return List.of(); }
        @Override public List<Domain.Order> openOrders(String symbol) { return new ArrayList<>(orders); }
        @Override public Domain.Order placePostOnly(PlaceRequest request) { throw new UnsupportedOperationException(); }
        @Override public void cancelOrder(String symbol, String orderId) { orders.removeIf(order -> order.orderId.equals(orderId)); }
    }
}
