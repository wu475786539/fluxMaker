package com.fluxmaker.oms;

import com.fluxmaker.domain.Domain;
import com.fluxmaker.math.DecimalValue;
import com.fluxmaker.venue.VenueClient;
import org.junit.jupiter.api.Test;

import java.util.ArrayList;
import java.util.List;

import static org.junit.jupiter.api.Assertions.assertEquals;
import static org.junit.jupiter.api.Assertions.assertTrue;

/** Drives the reconciler down the native batch-place path and checks the whole
 *  batch of post-only orders is submitted in a single call. */
class BatchPlaceTest {

    @Test
    void placesAllMissingQuotesInOneNativeBatch() {
        // 4 target quotes (2 levels, buy+sell); no existing orders -> all are vacancies.
        List<Domain.Quote> quotes = new ArrayList<>();
        for (int level = 0; level < 2; level++) {
            quotes.add(quote(level, Domain.Side.BUY, "99." + level));
            quotes.add(quote(level, Domain.Side.SELL, "101." + level));
        }
        BatchVenue venue = new BatchVenue();

        // fence generation = 7 (carried into every order's client id / place request).
        Reconciler.Result result = new Reconciler(null)
                .reconcileWithOrders(venue, "token_usdt", quotes, 10, List.of(), null, 7);

        assertEquals(1, venue.batchCalls, "all orders must go out in a single native batch call");
        assertEquals(0, venue.singleCalls, "must not fall back to per-order placement");
        assertEquals(4, venue.lastBatch.size(), "the whole batch is sent at once");
        assertEquals(4, result.placed, "all four quotes were placed");
        for (VenueClient.PlaceRequest request : venue.lastBatch) {
            assertTrue(request.clientId().startsWith("fm-"), "orders carry a managed client id");
            assertEquals(7, request.fenceGeneration(), "orders carry the fence generation");
        }
    }

    private static Domain.Quote quote(int level, Domain.Side side, String price) {
        Domain.Quote quote = new Domain.Quote();
        quote.instrumentId = "token_usdt"; quote.venue = "fake"; quote.symbol = "TOKENUSDT";
        quote.side = side; quote.level = level; quote.price = DecimalValue.parse(price); quote.quantity = DecimalValue.ONE;
        return quote;
    }

    /** A venue that advertises native batch placement and records what it receives. */
    private static final class BatchVenue implements VenueClient {
        int batchCalls, singleCalls, nextId;
        List<PlaceRequest> lastBatch = new ArrayList<>();

        @Override public String name() { return "fake"; }
        @Override public Capabilities capabilities() {
            // clientOrderIds + nativeBatchPlace enabled; the rest off.
            return new Capabilities(true, true, false, false, false, false);
        }
        @Override public Domain.Book topBook(String symbol) { return new Domain.Book(); }
        @Override public List<Domain.Balance> balances() { return List.of(); }
        @Override public List<Domain.Order> openOrders(String symbol) { return List.of(); }
        @Override public void cancelOrder(String symbol, String orderId) {}

        @Override public List<Domain.Order> placePostOnlyBatch(List<PlaceRequest> requests) {
            batchCalls++;
            lastBatch = new ArrayList<>(requests);
            List<Domain.Order> placed = new ArrayList<>();
            for (PlaceRequest request : requests) placed.add(confirm(request));
            return placed;
        }

        @Override public Domain.Order placePostOnly(PlaceRequest request) {
            singleCalls++; // should never be reached on the batch path
            return confirm(request);
        }

        private Domain.Order confirm(PlaceRequest request) {
            Domain.Order order = new Domain.Order();
            order.orderId = Integer.toString(++nextId);
            order.clientId = request.clientId();
            order.symbol = request.symbol();
            order.side = request.side();
            order.price = request.price();
            order.quantity = request.quantity();
            order.state = Domain.OrderState.NEW;
            return order;
        }
    }
}
