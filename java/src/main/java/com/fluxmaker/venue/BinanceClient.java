package com.fluxmaker.venue;

import com.fasterxml.jackson.databind.JsonNode;
import com.fluxmaker.domain.Domain;
import com.fluxmaker.math.DecimalValue;

import java.io.IOException;
import java.net.URI;
import java.net.http.HttpClient;
import java.net.http.HttpRequest;
import java.net.http.HttpResponse;
import java.time.Duration;
import java.time.Instant;
import java.util.ArrayList;
import java.util.List;
import java.util.Map;
import java.util.concurrent.CompletableFuture;
import java.util.concurrent.atomic.AtomicLong;

public final class BinanceClient implements VenueClient {
    private final String name;
    private final String identity;
    private final String baseUrl;
    private final String apiKey;
    private final String secret;
    private final String stpMode;
    private final Duration timeout;
    private final HttpClient http;
    private final AtomicLong timeOffsetMillis = new AtomicLong();

    public BinanceClient(String name, String identity, String baseUrl, String apiKey, String secret, String stpMode, Duration timeout) {
        this.name = name; this.identity = identity; this.baseUrl = strip(baseUrl); this.apiKey = value(apiKey); this.secret = value(secret); this.stpMode = value(stpMode); this.timeout = timeout; this.http = HttpSupport.client();
    }

    @Override public String name() { return name; }
    @Override public String stateIdentity() { return identity; }
    @Override public Capabilities capabilities() { return new Capabilities(true, false, true, true, true, true); }

    @Override public Domain.Book topBook(String symbol) {
        Map<String, List<String>> values = HttpSupport.params(); HttpSupport.set(values, "symbol", symbol);
        JsonNode raw = request("GET", "/api/v3/ticker/bookTicker", values, false);
        Domain.Book book = new Domain.Book(); book.venue = name; book.symbol = raw.path("symbol").asText(symbol);
        book.bidPrice = decimal(raw, "bidPrice"); book.bidQty = decimal(raw, "bidQty"); book.askPrice = decimal(raw, "askPrice"); book.askQty = decimal(raw, "askQty"); book.timestamp = Instant.now(); return book;
    }

    @Override public Domain.MarketRules marketRules(String symbol) {
        Map<String, List<String>> values = HttpSupport.params(); HttpSupport.set(values, "symbol", symbol);
        JsonNode symbols = request("GET", "/api/v3/exchangeInfo", values, false).path("symbols");
        if (!symbols.isArray() || symbols.size() != 1) throw new IllegalArgumentException("binance symbol " + symbol + " not found");
        JsonNode item = symbols.get(0); Domain.MarketRules rules = new Domain.MarketRules(); rules.symbol = item.path("symbol").asText(); rules.baseAsset = item.path("baseAsset").asText(); rules.quoteAsset = item.path("quoteAsset").asText();
        for (JsonNode filter : item.path("filters")) {
            switch (filter.path("filterType").asText()) {
                case "PRICE_FILTER" -> { rules.priceTick = optional(filter, "tickSize"); rules.minPrice = optional(filter, "minPrice"); rules.maxPrice = optional(filter, "maxPrice"); }
                case "LOT_SIZE" -> { rules.quantityStep = optional(filter, "stepSize"); rules.minQuantity = optional(filter, "minQty"); rules.maxQuantity = optional(filter, "maxQty"); }
                case "MIN_NOTIONAL" -> { rules.minNotional = optional(filter, "minNotional"); if (rules.minNotional.isZero()) rules.minNotional = optional(filter, "notional"); }
                case "NOTIONAL" -> { rules.minNotional = optional(filter, "minNotional"); rules.maxNotional = optional(filter, "maxNotional"); }
                case "MAX_NUM_ORDERS" -> rules.maxOpenOrders = filter.path("maxNumOrders").asInt();
                default -> { }
            }
        }
        if (!rules.priceTick.isPositive() || !rules.quantityStep.isPositive()) throw new IllegalArgumentException("binance symbol " + symbol + " returned incomplete trading rules");
        return rules;
    }

    @Override public List<Domain.Balance> balances() {
        List<Domain.Balance> result = new ArrayList<>();
        for (JsonNode item : signed("GET", "/api/v3/account", HttpSupport.params()).path("balances")) {
            Domain.Balance balance = new Domain.Balance(); balance.asset = item.path("asset").asText(); balance.free = decimal(item, "free"); balance.locked = decimal(item, "locked"); result.add(balance);
        }
        return result;
    }

    @Override public List<Domain.Order> openOrders(String symbol) {
        Map<String, List<String>> values = HttpSupport.params(); HttpSupport.set(values, "symbol", symbol);
        List<Domain.Order> result = new ArrayList<>(); for (JsonNode item : signed("GET", "/api/v3/openOrders", values)) result.add(parseOrder(item, null)); return result;
    }

    @Override public Domain.Order order(String symbol, String orderId) {
        Map<String, List<String>> values = HttpSupport.params(); HttpSupport.set(values, "symbol", symbol); HttpSupport.set(values, "orderId", orderId); return parseOrder(signed("GET", "/api/v3/order", values), null);
    }

    @Override public List<Domain.Fill> recentFills(String symbol, int limit) {
        if (limit < 1 || limit > 1000) limit = 50;
        Map<String, List<String>> values = HttpSupport.params(); HttpSupport.set(values, "symbol", symbol); HttpSupport.set(values, "limit", limit);
        List<Domain.Fill> result = new ArrayList<>();
        for (JsonNode item : signed("GET", "/api/v3/myTrades", values)) {
            Domain.Fill fill = new Domain.Fill(); fill.venue = name; fill.tradeId = item.path("id").asText(); fill.orderId = item.path("orderId").asText(); fill.symbol = item.path("symbol").asText(); fill.side = item.path("isBuyer").asBoolean() ? Domain.Side.BUY : Domain.Side.SELL;
            fill.price = decimal(item, "price"); fill.quantity = decimal(item, "qty"); fill.quoteQuantity = decimal(item, "quoteQty"); fill.fee = decimal(item, "commission"); fill.feeAsset = item.path("commissionAsset").asText(); fill.maker = item.path("isMaker").asBoolean(); fill.timestamp = Instant.ofEpochMilli(item.path("time").asLong()); result.add(fill);
        }
        return result;
    }

    @Override public Domain.Order placePostOnly(PlaceRequest request) {
        Map<String, List<String>> values = HttpSupport.params(); HttpSupport.set(values, "symbol", request.symbol()); HttpSupport.set(values, "side", request.side()); HttpSupport.set(values, "type", "LIMIT_MAKER"); HttpSupport.set(values, "quantity", request.quantity()); HttpSupport.set(values, "price", request.price()); HttpSupport.set(values, "newClientOrderId", request.clientId()); HttpSupport.set(values, "newOrderRespType", "RESULT"); if (!stpMode.isEmpty()) HttpSupport.set(values, "selfTradePreventionMode", stpMode);
        return parseOrder(signed("POST", "/api/v3/order", values), request.side());
    }

    @Override public void cancelOrder(String symbol, String orderId) {
        Map<String, List<String>> values = HttpSupport.params(); HttpSupport.set(values, "symbol", symbol); HttpSupport.set(values, "orderId", orderId); signed("DELETE", "/api/v3/order", values);
    }

    @Override public void cancelOrders(String symbol, List<String> orderIds) {
        List<CompletableFuture<Void>> futures = new ArrayList<>();
        for (int start = 0; start < orderIds.size(); start += 5) {
            futures.clear();
            for (String id : orderIds.subList(start, Math.min(start + 5, orderIds.size()))) futures.add(CompletableFuture.runAsync(() -> cancelOrder(symbol, id)));
            CompletableFuture.allOf(futures.toArray(CompletableFuture[]::new)).join();
        }
    }

    private JsonNode signed(String method, String path, Map<String, List<String>> input) {
        if (apiKey.isEmpty() || secret.isEmpty()) throw new IllegalStateException("binance credentials are not configured");
        try { return signedOnce(method, path, input); }
        catch (ApiException e) { if (e.code != -1021) throw e; syncServerTime(); return signedOnce(method, path, input); }
    }

    private JsonNode signedOnce(String method, String path, Map<String, List<String>> input) {
        Map<String, List<String>> values = HttpSupport.copy(input); HttpSupport.set(values, "timestamp", System.currentTimeMillis() + timeOffsetMillis.get()); HttpSupport.set(values, "recvWindow", 5000); HttpSupport.set(values, "signature", HttpSupport.hmacSha256(secret, HttpSupport.encode(values))); return request(method, path, values, true);
    }

    private synchronized void syncServerTime() {
        long started = System.currentTimeMillis(); long server = request("GET", "/api/v3/time", HttpSupport.params(), false).path("serverTime").asLong(); if (server <= 0) throw new IllegalStateException("binance returned invalid server time"); timeOffsetMillis.set(server - (started + (System.currentTimeMillis() - started) / 2));
    }

    private JsonNode request(String method, String path, Map<String, List<String>> values, boolean authenticated) {
        String query = HttpSupport.encode(values); URI endpoint = URI.create(baseUrl + path + (query.isEmpty() ? "" : "?" + query));
        HttpRequest.Builder builder = HttpRequest.newBuilder(endpoint).timeout(timeout).method(method, HttpRequest.BodyPublishers.noBody()); if (authenticated) builder.header("X-MBX-APIKEY", apiKey);
        try {
            HttpResponse<String> response = http.send(builder.build(), HttpResponse.BodyHandlers.ofString()); JsonNode body = HttpSupport.json(response.body());
            if (response.statusCode() / 100 != 2) throw new ApiException(response.statusCode(), body.path("code").asInt(), body.path("msg").asText()); return body;
        } catch (IOException e) { throw new IllegalStateException("binance request: " + e.getMessage(), e); }
        catch (InterruptedException e) { Thread.currentThread().interrupt(); throw new IllegalStateException("binance request interrupted", e); }
    }

    private Domain.Order parseOrder(JsonNode item, Domain.Side fallbackSide) {
        Domain.Order order = new Domain.Order(); order.venue = name; order.orderId = item.path("orderId").asText(); order.clientId = item.path("clientOrderId").asText(); order.symbol = item.path("symbol").asText(); order.side = side(item.path("side").asText(), fallbackSide); order.price = decimal(item, "price"); order.quantity = decimal(item, "origQty"); order.executedQty = decimal(item, "executedQty"); order.state = state(item.path("status").asText()); order.createdAt = item.has("time") ? Instant.ofEpochMilli(item.path("time").asLong()) : Instant.now(); return order;
    }

    private static DecimalValue decimal(JsonNode node, String field) { return DecimalValue.parse(node.path(field).asText("0")); }
    private static DecimalValue optional(JsonNode node, String field) { try { String value = node.path(field).asText(); return value.isEmpty() ? DecimalValue.ZERO : DecimalValue.parse(value); } catch (RuntimeException e) { return DecimalValue.ZERO; } }
    private static Domain.Side side(String value, Domain.Side fallback) { try { return Domain.Side.valueOf(value); } catch (RuntimeException e) { return fallback; } }
    private static Domain.OrderState state(String value) { try { return Domain.OrderState.valueOf(value); } catch (RuntimeException e) { return Domain.OrderState.UNKNOWN; } }
    private static String value(String value) { return value == null ? "" : value; }
    private static String strip(String value) { return value.replaceAll("/+$", ""); }

    public static final class ApiException extends RuntimeException {
        public final int status; public final int code;
        ApiException(int status, int code, String message) { super("binance http " + status + " code=" + code + ": " + message); this.status = status; this.code = code; }
    }
}
