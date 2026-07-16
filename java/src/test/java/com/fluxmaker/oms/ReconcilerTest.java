package com.fluxmaker.oms;

import com.fluxmaker.domain.Domain;
import com.fluxmaker.math.DecimalValue;
import com.fluxmaker.venue.VenueClient;
import org.junit.jupiter.api.Test;

import java.time.Clock;
import java.time.Duration;
import java.time.Instant;
import java.time.ZoneId;
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

    @Test void inventoryDrivenMaterialRepriceIsLimitedPerCycle() {
        List<Domain.Quote> oldQuotes = target(5, 0);
        List<Domain.Quote> newQuotes = target(5, 500);
        List<Domain.Order> orders = ordersFor(oldQuotes, null, Instant.now().minusSeconds(60));
        FakeVenue venue = new FakeVenue(orders);
        Reconciler.RefreshPolicy policy = new Reconciler.RefreshPolicy(Duration.ofSeconds(30), Duration.ofMinutes(5), 2);

        Reconciler.Result result = new Reconciler(null).reconcileWithOrders(
                venue, "token_usdt", newQuotes, 10, orders, null, 0, policy, true);

        assertEquals(2, result.canceled, "inventory repricing should respect the gradual refresh budget");
        assertEquals(8, result.kept, "the remaining depth should stay on the book during gradual repricing");
        assertEquals(8, venue.orders.size());
    }

    @Test void filledLevelWaitsBeforeReplacementWithoutClearingTheRestOfTheBook() {
        List<Domain.Quote> oldQuotes = target(5, 0);
        List<Domain.Quote> newQuotes = target(5, 500);
        List<Domain.Order> orders = ordersFor(oldQuotes, null, Instant.now().minusSeconds(60));
        orders.remove(orders.stream()
                .filter(order -> order.side == Domain.Side.BUY)
                .findFirst()
                .orElseThrow());
        FakeVenue venue = new FakeVenue(orders);
        MutableClock clock = new MutableClock(Instant.parse("2026-07-16T10:00:00Z"));
        Reconciler reconciler = new Reconciler(null, clock);
        Reconciler.RefreshPolicy policy = new Reconciler.RefreshPolicy(Duration.ofSeconds(30), Duration.ofMinutes(5), 2);
        Reconciler.ReplenishPolicy replenish = new Reconciler.ReplenishPolicy(
                Duration.ofSeconds(3), Duration.ofSeconds(3), 2);

        Reconciler.Result waiting = reconciler.reconcileWithOrders(
                venue, "token_usdt", newQuotes, 10, orders, null, 0, policy, true, replenish);

        assertEquals(0, waiting.placed, "a real fill should enter replenishment cooldown");
        assertEquals(0, waiting.canceled, "the cooldown must not clear the remaining visible depth");
        assertEquals(1, waiting.delayed);
        assertEquals(9, waiting.kept);
        assertEquals(9, venue.orders.size());

        clock.advance(Duration.ofSeconds(2));
        Reconciler.Result stillWaiting = reconciler.reconcileWithOrders(
                venue, "token_usdt", newQuotes, 10, venue.openOrders("TOKENUSDT"), null, 0, policy, true, replenish);
        assertEquals(0, stillWaiting.placed);
        assertEquals(0, stillWaiting.canceled);

        clock.advance(Duration.ofSeconds(1));
        Reconciler.Result replenished = reconciler.reconcileWithOrders(
                venue, "token_usdt", newQuotes, 10, venue.openOrders("TOKENUSDT"), null, 0, policy, true, replenish);
        assertEquals(1, replenished.placed, "the missing buy level should be restored when its delay expires");
        assertEquals(0, replenished.canceled);
        assertEquals(10, venue.orders.size());
        assertEquals(5, venue.orders.stream().filter(order -> order.side == Domain.Side.BUY).count());
        assertEquals(5, venue.orders.stream().filter(order -> order.side == Domain.Side.SELL).count());
    }

    @Test void initialEmptyBookStillBootstrapsImmediately() {
        List<Domain.Quote> quotes = target(2, 0);
        FakeVenue venue = new FakeVenue(List.of());
        MutableClock clock = new MutableClock(Instant.parse("2026-07-16T10:00:00Z"));
        Reconciler.ReplenishPolicy replenish = new Reconciler.ReplenishPolicy(
                Duration.ofSeconds(3), Duration.ofSeconds(8), 2);

        Reconciler.Result result = new Reconciler(null, clock).reconcileWithOrders(
                venue, "token_usdt", quotes, 10, List.of(), null, 0,
                Reconciler.RefreshPolicy.disabled(), true, replenish);

        assertEquals(4, result.placed);
        assertEquals(4, venue.orders.size());
    }

    @Test void engineInitiatedRotationReplacesCanceledLevelsWithoutFillDelay() {
        List<Domain.Quote> oldQuotes = target(5, 0);
        List<Domain.Quote> newQuotes = target(5, 500);
        FakeVenue venue = new FakeVenue(ordersFor(oldQuotes, null, Instant.now().minusSeconds(60)));
        MutableClock clock = new MutableClock(Instant.parse("2026-07-16T10:00:00Z"));
        Reconciler reconciler = new Reconciler(null, clock);
        Reconciler.RefreshPolicy refresh = new Reconciler.RefreshPolicy(Duration.ZERO, Duration.ofMinutes(5), 2);
        Reconciler.ReplenishPolicy replenish = new Reconciler.ReplenishPolicy(
                Duration.ofSeconds(3), Duration.ofSeconds(8), 2);

        Reconciler.Result canceled = reconciler.reconcileWithOrders(
                venue, "token_usdt", newQuotes, 10, venue.openOrders("TOKENUSDT"), null, 0, refresh, true, replenish);
        assertEquals(2, canceled.canceled);

        Reconciler.Result replaced = reconciler.reconcileWithOrders(
                venue, "token_usdt", newQuotes, 10, venue.openOrders("TOKENUSDT"), null, 0, refresh, true, replenish);
        assertEquals(2, replaced.placed, "our own rotation cancels should not be mistaken for external fills");
        assertEquals(0, replaced.delayed);
        assertEquals(10, venue.orders.size());
    }

    @Test void partiallyFilledOrderIsCanceledImmediatelyDuringGradualReprice() {
        List<Domain.Quote> quotes = target(5, 0);
        List<Domain.Order> orders = ordersFor(quotes, null, Instant.now().minusSeconds(60));
        orders.getFirst().executedQty = DecimalValue.parse("0.5");
        FakeVenue venue = new FakeVenue(orders);
        Reconciler.RefreshPolicy policy = new Reconciler.RefreshPolicy(Duration.ofSeconds(30), Duration.ofMinutes(5), 2);

        Reconciler.Result result = new Reconciler(null).reconcileWithOrders(
                venue, "token_usdt", quotes, 10, orders, null, 0, policy, true);

        assertEquals(1, result.canceled, "the partially filled remainder must be replaced immediately");
        assertEquals(9, result.kept);
    }

    @Test void ordersOnARiskBlockedSideAreCanceledImmediatelyDuringGradualReprice() {
        List<Domain.Quote> oldQuotes = target(5, 0);
        List<Domain.Quote> buyOnly = oldQuotes.stream().filter(quote -> quote.side == Domain.Side.BUY).toList();
        List<Domain.Order> orders = ordersFor(oldQuotes, null, Instant.now().minusSeconds(60));
        FakeVenue venue = new FakeVenue(orders);
        Reconciler.RefreshPolicy policy = new Reconciler.RefreshPolicy(Duration.ofSeconds(30), Duration.ofMinutes(5), 2);

        Reconciler.Result result = new Reconciler(null).reconcileWithOrders(
                venue, "token_usdt", buyOnly, 10, orders, null, 0, policy, true);

        assertEquals(5, result.canceled, "orders left on a side removed by risk controls must not be retained");
        assertEquals(5, result.kept);
    }

    @Test void gradualMaterialRepriceKeepsDepthWhileConvergingAcrossCycles() {
        List<Domain.Quote> oldQuotes = target(5, 0);
        List<Domain.Quote> newQuotes = target(5, 500);
        FakeVenue venue = new FakeVenue(ordersFor(oldQuotes, null, Instant.now().minusSeconds(60)));
        Reconciler reconciler = new Reconciler(null);
        Reconciler.RefreshPolicy policy = new Reconciler.RefreshPolicy(Duration.ofSeconds(30), Duration.ofMinutes(5), 2);
        boolean converged = false;

        for (int cycle = 0; cycle < 20; cycle++) {
            Reconciler.Result result = reconciler.reconcileWithOrders(
                    venue, "token_usdt", newQuotes, 10, venue.openOrders("TOKENUSDT"), null, 0, policy, true);
            assertTrue(venue.orders.size() >= 8, "gradual repricing must retain most visible depth");
            assertTrue(venue.orders.size() <= 10, "gradual repricing must not overfill the target depth");
            if (result.kept == newQuotes.size() && result.canceled == 0 && result.placed == 0 && result.pending == 0) {
                converged = true;
                break;
            }
        }

        assertTrue(converged, "the gradually refreshed book should eventually match the complete target");
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
        private int nextOrderId = 1000;
        private FakeVenue(List<Domain.Order> orders) { this.orders = new ArrayList<>(orders); }
        @Override public String name() { return "fake"; }
        @Override public Domain.Book topBook(String symbol) { return new Domain.Book(); }
        @Override public List<Domain.Balance> balances() { return List.of(); }
        @Override public List<Domain.Order> openOrders(String symbol) { return new ArrayList<>(orders); }
        @Override public Domain.Order placePostOnly(PlaceRequest request) {
            Domain.Order order = new Domain.Order();
            order.orderId = Integer.toString(nextOrderId++); order.clientId = request.clientId(); order.symbol = request.symbol();
            order.side = request.side(); order.price = request.price(); order.quantity = request.quantity();
            order.state = Domain.OrderState.NEW; order.createdAt = Instant.now(); orders.add(order); return order;
        }
        @Override public void cancelOrder(String symbol, String orderId) { orders.removeIf(order -> order.orderId.equals(orderId)); }
    }

    private static final class MutableClock extends Clock {
        private Instant now;
        private MutableClock(Instant now) { this.now = now; }
        private void advance(Duration duration) { now = now.plus(duration); }
        @Override public ZoneId getZone() { return ZoneId.of("UTC"); }
        @Override public Clock withZone(ZoneId zone) { return this; }
        @Override public Instant instant() { return now; }
    }
}
