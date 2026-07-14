package com.fluxmaker.venue;

import com.fluxmaker.domain.Domain;
import com.fluxmaker.math.DecimalValue;

import java.util.List;

public interface VenueClient {
    record PlaceRequest(String symbol, Domain.Side side, DecimalValue price, DecimalValue quantity, String clientId, long fenceGeneration) {}
    record Capabilities(boolean clientOrderIds, boolean nativeBatchPlace, boolean nativeBatchCancel, boolean orderLookup, boolean recentFills, boolean marketRules) {}

    String name();
    default String stateIdentity() { return name(); }
    Domain.Book topBook(String symbol);
    List<Domain.Balance> balances();
    List<Domain.Order> openOrders(String symbol);
    Domain.Order placePostOnly(PlaceRequest request);
    void cancelOrder(String symbol, String orderId);
    default Domain.Order order(String symbol, String orderId) { throw new UnsupportedOperationException("order lookup unavailable"); }
    default List<Domain.Fill> recentFills(String symbol, int limit) { throw new UnsupportedOperationException("recent fills unavailable"); }
    default Domain.MarketRules marketRules(String symbol) { throw new UnsupportedOperationException("market rules unavailable"); }
    default List<Domain.Order> placePostOnlyBatch(List<PlaceRequest> requests) {
        return requests.stream().map(this::placePostOnly).toList();
    }
    default void cancelOrders(String symbol, List<String> orderIds) { for (String id : orderIds) cancelOrder(symbol, id); }
    default Capabilities capabilities() { return new Capabilities(true, false, false, false, false, false); }
    default boolean managesAllOrders() { return !capabilities().clientOrderIds(); }
}
