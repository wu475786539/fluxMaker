package com.fluxmaker.venue;

import com.fasterxml.jackson.databind.JsonNode;
import com.fluxmaker.domain.Domain;
import com.fluxmaker.json.Json;
import com.fluxmaker.math.DecimalValue;

import java.io.IOException;
import java.net.URI;
import java.net.http.HttpClient;
import java.net.http.HttpRequest;
import java.net.http.HttpResponse;
import java.security.SecureRandom;
import java.time.Duration;
import java.time.Instant;
import java.util.ArrayList;
import java.util.HexFormat;
import java.util.List;
import java.util.Map;

public final class MgbxClient implements VenueClient {
    private static final SecureRandom RANDOM = new SecureRandom();
    private final String name, identity, baseUrl, apiKey, secret;
    private final Duration timeout;
    private final HttpClient http;

    public MgbxClient(String name, String identity, String baseUrl, String apiKey, String secret, Duration timeout) {
        this.name = name; this.identity = identity; this.baseUrl = baseUrl.replaceAll("/+$", ""); this.apiKey = value(apiKey); this.secret = value(secret); this.timeout = timeout; this.http = HttpSupport.client();
    }

    @Override public String name() { return name; }
    @Override public String stateIdentity() { return identity; }
    @Override public Capabilities capabilities() { return new Capabilities(false, false, true, true, true, true); }

    @Override public Domain.Book topBook(String symbol) {
        Map<String, List<String>> values = HttpSupport.params(); HttpSupport.set(values, "symbol", symbol); HttpSupport.set(values, "level", 5);
        JsonNode raw = request("GET", "/spot/v1/p/quotation/depth", values, false); Domain.Book book = new Domain.Book(); book.venue = name; book.symbol = raw.path("s").asText(symbol); book.timestamp = raw.path("t").asLong() > 0 ? Instant.ofEpochMilli(raw.path("t").asLong()) : Instant.now();
        JsonNode bids = raw.path("b"), asks = raw.path("a"); if (bids.isArray() && !bids.isEmpty()) { book.bidPrice = DecimalValue.parse(bids.get(0).get(0).asText()); book.bidQty = DecimalValue.parse(bids.get(0).get(1).asText()); } if (asks.isArray() && !asks.isEmpty()) { book.askPrice = DecimalValue.parse(asks.get(0).get(0).asText()); book.askQty = DecimalValue.parse(asks.get(0).get(1).asText()); } return book;
    }

    @Override public Domain.MarketRules marketRules(String symbol) {
        for (JsonNode item : request("GET", "/spot/v1/p/symbol/configs", HttpSupport.params(), false)) if (item.path("symbol").asText().equalsIgnoreCase(symbol)) {
            Domain.MarketRules rules = new Domain.MarketRules(); rules.symbol = item.path("symbol").asText(); rules.baseAsset = item.path("baseAsset").asText(); rules.quoteAsset = item.path("quoteAsset").asText(); rules.priceTick = precision(item.path("pricePrecision").asInt()); rules.quantityStep = precision(item.path("quantityPrecision").asInt()); return rules;
        }
        throw new IllegalArgumentException("MGBX symbol " + symbol + " not found");
    }

    @Override public List<Domain.Balance> balances() {
        List<Domain.Balance> result = new ArrayList<>(); for (JsonNode item : request("GET", "/spot/v1/u/balance/spot", HttpSupport.params(), true)) { Domain.Balance balance = new Domain.Balance(); balance.asset = item.path("coin").asText(); balance.free = DecimalValue.parse(item.path("availableBalance").asText("0")); balance.locked = DecimalValue.parse(item.path("freeze").asText("0")); result.add(balance); } return result;
    }

    @Override public List<Domain.Order> openOrders(String symbol) {
        List<Domain.Order> result = new ArrayList<>();
        for (int page = 1; page <= 1000; page++) {
            Map<String, List<String>> values = HttpSupport.params(); HttpSupport.set(values, "symbol", symbol); HttpSupport.set(values, "state", 9); HttpSupport.set(values, "page", page); HttpSupport.set(values, "size", 100);
            JsonNode raw = request("GET", "/spot/v1/u/trade/order/list", values, true); JsonNode items = raw.path("items"); for (JsonNode item : items) result.add(parseOrder(item)); long total = raw.path("total").asLong(); if (items.isEmpty() || (total > 0 && result.size() >= total) || (total == 0 && items.size() < 100)) return result;
        }
        throw new IllegalStateException("MGBX open-order pagination exceeded safety limit");
    }

    @Override public Domain.Order order(String symbol, String orderId) { Map<String, List<String>> values = HttpSupport.params(); HttpSupport.set(values, "orderId", orderId); return parseOrder(request("GET", "/spot/v1/u/trade/order/detail", values, true)); }

    @Override public List<Domain.Fill> recentFills(String symbol, int limit) {
        if (limit < 1 || limit > 100) limit = 50; Map<String, List<String>> values = HttpSupport.params(); HttpSupport.set(values, "symbol", symbol); HttpSupport.set(values, "limit", limit); List<Domain.Fill> result = new ArrayList<>();
        for (JsonNode item : request("GET", "/spot/v1/u/trade/order/history", values, true).path("items")) { DecimalValue quantity = DecimalValue.parse(item.path("executedQty").asText("0")); if (!quantity.isPositive()) continue; DecimalValue price = DecimalValue.parse(item.path("avgPrice").asText("0")); Domain.Fill fill = new Domain.Fill(); fill.venue = name; fill.orderId = item.path("orderId").asText(); fill.tradeId = "order:" + fill.orderId; fill.symbol = item.path("symbol").asText(); fill.side = side(item.path("orderSide").asText()); fill.price = price; fill.quantity = quantity; fill.quoteQuantity = price.multiply(quantity); fill.aggregate = true; fill.timestamp = Instant.ofEpochMilli(item.path("createdTime").asLong()); result.add(fill); } return result;
    }

    @Override public Domain.Order placePostOnly(PlaceRequest request) {
        Map<String, List<String>> values = HttpSupport.params(); HttpSupport.set(values, "symbol", request.symbol()); HttpSupport.set(values, "direction", request.side()); HttpSupport.set(values, "tradeType", "LIMIT"); HttpSupport.set(values, "totalAmount", request.quantity()); HttpSupport.set(values, "price", request.price()); HttpSupport.set(values, "timeInForce", "GTX"); JsonNode raw = request("POST", "/spot/v1/u/trade/order/create", values, true); Domain.Order order = new Domain.Order(); order.venue = name; order.orderId = raw.asText(); order.symbol = request.symbol(); order.side = request.side(); order.price = request.price(); order.quantity = request.quantity(); order.state = Domain.OrderState.UNKNOWN; order.createdAt = Instant.now(); return order;
    }

    /** MGBX's native batch-create route is intermittently terminated by its edge.
     *  Keep this method safe for direct callers by falling back to the stable
     *  single-order route; capabilities() also tells the OMS not to use native batch. */
    @Override public List<Domain.Order> placePostOnlyBatch(List<PlaceRequest> requests) {
        return VenueClient.super.placePostOnlyBatch(requests);
    }

    @Override public void cancelOrder(String symbol, String orderId) { Map<String, List<String>> values = HttpSupport.params(); HttpSupport.set(values, "orderId", orderId); request("POST", "/spot/v1/u/trade/order/cancel", values, true); }
    @Override public void cancelOrders(String symbol, List<String> orderIds) { for (int start = 0; start < orderIds.size(); start += 20) { Map<String, List<String>> values = HttpSupport.params(); HttpSupport.set(values, "orderIdsJson", Json.write(orderIds.subList(start, Math.min(start + 20, orderIds.size())))); request("POST", "/spot/v1/u/trade/order/batch/cancel", values, true); } }

    private JsonNode request(String method, String path, Map<String, List<String>> values, boolean authenticated) {
        HttpRequest.Builder builder = HttpRequest.newBuilder();
        if (authenticated) {
            if (apiKey.isEmpty() || secret.isEmpty()) throw new IllegalStateException("MGBX credentials are not configured"); String timestamp = Long.toString(System.currentTimeMillis()); byte[] nonce = new byte[16]; RANDOM.nextBytes(nonce); builder.header("X-Access-Key", apiKey).header("X-Signature", HttpSupport.hmacSha256(secret, signaturePayload(values, timestamp))).header("X-Request-Timestamp", timestamp).header("X-Request-Nonce", HexFormat.of().formatHex(nonce));
        }
        String query = HttpSupport.encode(values); builder.uri(URI.create(baseUrl + path + (query.isEmpty() ? "" : "?" + query))).timeout(timeout).header("Content-Type", "application/x-www-form-urlencoded").method(method, HttpRequest.BodyPublishers.noBody());
        long started = System.nanoTime();
        try {
            HttpResponse<String> response = http.send(builder.build(), HttpResponse.BodyHandlers.ofString());
            if (response.statusCode() / 100 != 2) throw new IllegalStateException(
                    "MGBX HTTP error method=" + method + " path=" + path
                            + " status=" + response.statusCode() + " body=" + oneLine(response.body()));
            JsonNode envelope = HttpSupport.json(response.body());
            int code = envelope.path("code").asInt();
            if (code != 0) {
                String message = envelope.path("msg").asText();
                if (message.isEmpty()) message = envelope.path("message").asText();
                throw new IllegalStateException(
                        "MGBX API error method=" + method + " path=" + path
                                + " code=" + code + " message=" + oneLine(message));
            }
            return envelope.path("data");
        } catch (IOException e) {
            throw transportFailure(method, path, started, e);
        } catch (InterruptedException e) {
            Thread.currentThread().interrupt();
            throw transportFailure(method, path, started, e);
        }
    }

    private IllegalStateException transportFailure(String method, String path, long startedNanos, Throwable error) {
        long elapsedMillis = Math.max(0, (System.nanoTime() - startedNanos) / 1_000_000L);
        String diagnostic = transportFailureMessage(name, method, path, elapsedMillis, timeout, error);
        // Keep this as one structured line: the engine currently logs exception messages,
        // not stack traces. The original throwable is still retained as the cause.
        System.err.println("[mgbx-http] " + diagnostic);
        return new IllegalStateException(diagnostic, error);
    }

    static String transportFailureMessage(String venue, String method, String path,
                                          long elapsedMillis, Duration timeout, Throwable error) {
        return "MGBX transport failure venue=" + oneLine(venue)
                + " method=" + oneLine(method)
                + " path=" + oneLine(path)
                + " elapsed_ms=" + Math.max(0, elapsedMillis)
                + " timeout_ms=" + Math.max(0, timeout.toMillis())
                + " exception=" + error.getClass().getName()
                + " cause_chain=" + causeChain(error);
    }

    private static String causeChain(Throwable error) {
        StringBuilder result = new StringBuilder();
        Throwable current = error;
        for (int depth = 0; current != null && depth < 8; depth++) {
            if (depth > 0) result.append(" <- ");
            result.append(current.getClass().getName());
            String message = current.getMessage();
            if (message != null && !message.isBlank()) result.append('(').append(oneLine(message)).append(')');
            Throwable next = current.getCause();
            if (next == current) break;
            current = next;
        }
        return result.toString();
    }

    private static String oneLine(String value) {
        String normalized = value == null ? "" : value.replace('\r', ' ').replace('\n', ' ').replace('\t', ' ').trim();
        return normalized.length() <= 1_000 ? normalized : normalized.substring(0, 1_000) + "...";
    }

    static String signaturePayload(Map<String, List<String>> values, String timestamp) { return HttpSupport.rawSorted(values) + "&timestamp=" + timestamp; }
    static DecimalValue precision(int precision) { if (precision < 0 || precision > 30) throw new IllegalArgumentException("unsupported precision " + precision); return precision == 0 ? DecimalValue.ONE : DecimalValue.parse("0." + "0".repeat(precision - 1) + "1"); }
    private Domain.Order parseOrder(JsonNode item) { Domain.Order order = new Domain.Order(); order.venue = name; order.orderId = item.path("orderId").asText(); order.symbol = item.path("symbol").asText(); order.side = side(item.path("orderSide").asText()); order.price = DecimalValue.parse(item.path("price").asText("0")); order.quantity = DecimalValue.parse(item.path("origQty").asText("0")); order.executedQty = DecimalValue.parse(item.path("executedQty").asText("0")); order.state = state(item.path("state").asText()); order.createdAt = Instant.ofEpochMilli(item.path("createdTime").asLong()); return order; }
    private static Domain.Side side(String value) { try { return Domain.Side.valueOf(value); } catch (RuntimeException e) { return Domain.Side.SELL; } }
    private static Domain.OrderState state(String value) { try { return Domain.OrderState.valueOf(value); } catch (RuntimeException e) { return Domain.OrderState.UNKNOWN; } }
    private static DecimalValue precisionStep(int value) { return precision(value); }
    private static String value(String value) { return value == null ? "" : value; }
}
