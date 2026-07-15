package com.fluxmaker.venue;

import com.fasterxml.jackson.databind.JsonNode;
import com.fluxmaker.domain.Domain;
import com.fluxmaker.json.Json;
import com.fluxmaker.math.DecimalValue;
import com.sun.net.httpserver.HttpServer;
import org.junit.jupiter.api.Test;

import java.io.IOException;
import java.io.OutputStream;
import java.net.InetSocketAddress;
import java.net.URLDecoder;
import java.nio.charset.StandardCharsets;
import java.time.Duration;
import java.util.List;
import java.util.concurrent.atomic.AtomicInteger;
import java.util.concurrent.atomic.AtomicReference;

import static org.junit.jupiter.api.Assertions.assertEquals;

/** Verifies MgbxClient.placePostOnlyBatch talks to /batch/create once, keeps
 *  every order maker-only (timeInForce=GTX), and maps the returned order ids back
 *  in request order — all against a local stub, no real exchange. */
class MgbxBatchPlaceTest {

    @Test
    void submitsOnePostOnlyBatchAndMapsOrderIdsInOrder() throws IOException {
        AtomicInteger calls = new AtomicInteger();
        AtomicReference<String> path = new AtomicReference<>();
        AtomicReference<String> ordersJson = new AtomicReference<>();

        HttpServer server = HttpServer.create(new InetSocketAddress("127.0.0.1", 0), 0);
        server.createContext("/", exchange -> {
            try {
                calls.incrementAndGet();
                path.set(exchange.getRequestURI().getPath());
                ordersJson.set(param(exchange.getRequestURI().getRawQuery(), "ordersJsonStr"));
                byte[] body = ("{\"code\":0,\"msg\":\"ok\",\"data\":["
                        + "{\"code\":0,\"data\":\"1001\"},"
                        + "{\"code\":0,\"data\":\"1002\"},"
                        + "{\"code\":0,\"data\":\"1003\"}]}").getBytes(StandardCharsets.UTF_8);
                exchange.getResponseHeaders().add("Content-Type", "application/json");
                exchange.sendResponseHeaders(200, body.length);
                try (OutputStream out = exchange.getResponseBody()) { out.write(body); }
            } finally { exchange.close(); }
        });
        server.start();
        try {
            String baseUrl = "http://127.0.0.1:" + server.getAddress().getPort();
            MgbxClient client = new MgbxClient("t", "t", baseUrl, "key", "secret", Duration.ofSeconds(5));

            List<VenueClient.PlaceRequest> requests = List.of(
                    new VenueClient.PlaceRequest("GDT_USDT", Domain.Side.BUY, DecimalValue.parse("0.1"), DecimalValue.parse("100"), "fm-a", 0),
                    new VenueClient.PlaceRequest("GDT_USDT", Domain.Side.BUY, DecimalValue.parse("0.2"), DecimalValue.parse("100"), "fm-b", 0),
                    new VenueClient.PlaceRequest("GDT_USDT", Domain.Side.SELL, DecimalValue.parse("0.3"), DecimalValue.parse("100"), "fm-c", 0));

            List<Domain.Order> orders = client.placePostOnlyBatch(requests);

            assertEquals(1, calls.get(), "the whole batch must be a single signed request");
            assertEquals("/spot/v1/u/trade/order/batch/create", path.get());
            assertEquals(List.of("1001", "1002", "1003"),
                    orders.stream().map(order -> order.orderId).toList(),
                    "order ids map back in request order");

            JsonNode payload = Json.MAPPER.readTree(ordersJson.get());
            assertEquals(3, payload.size(), "all three orders are in the batch payload");
            for (JsonNode order : payload) {
                assertEquals("GTX", order.path("timeInForce").asText(), "batch orders must stay maker-only (post-only)");
                assertEquals("LIMIT", order.path("tradeType").asText());
            }
            assertEquals("BUY", payload.get(0).path("direction").asText());
            assertEquals("SELL", payload.get(2).path("direction").asText());
        } finally {
            server.stop(0);
        }
    }

    private static String param(String rawQuery, String key) {
        if (rawQuery == null) return null;
        for (String pair : rawQuery.split("&")) {
            int eq = pair.indexOf('=');
            if (eq > 0 && URLDecoder.decode(pair.substring(0, eq), StandardCharsets.UTF_8).equals(key))
                return URLDecoder.decode(pair.substring(eq + 1), StandardCharsets.UTF_8);
        }
        return null;
    }
}
