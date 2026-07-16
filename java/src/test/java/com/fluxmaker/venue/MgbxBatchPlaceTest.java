package com.fluxmaker.venue;

import com.fluxmaker.domain.Domain;
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
import java.util.concurrent.CopyOnWriteArrayList;
import java.util.concurrent.atomic.AtomicInteger;

import static org.junit.jupiter.api.Assertions.assertEquals;
import static org.junit.jupiter.api.Assertions.assertFalse;

/** Verifies that MGBX batch placement is deliberately disabled and every
 *  requested order is sent through the stable single-order endpoint instead. */
class MgbxBatchPlaceTest {

    @Test
    void fallsBackToSignedPostOnlySingleOrders() throws IOException {
        AtomicInteger calls = new AtomicInteger();
        List<String> paths = new CopyOnWriteArrayList<>();
        List<String> queries = new CopyOnWriteArrayList<>();
        List<String> contentTypes = new CopyOnWriteArrayList<>();
        List<String> bodies = new CopyOnWriteArrayList<>();

        HttpServer server = HttpServer.create(new InetSocketAddress("127.0.0.1", 0), 0);
        server.createContext("/", exchange -> {
            try {
                int call = calls.incrementAndGet();
                paths.add(exchange.getRequestURI().getPath());
                queries.add(exchange.getRequestURI().getRawQuery());
                contentTypes.add(exchange.getRequestHeaders().getFirst("Content-Type"));
                bodies.add(new String(exchange.getRequestBody().readAllBytes(), StandardCharsets.UTF_8));
                byte[] body = ("{\"code\":0,\"msg\":\"ok\",\"data\":\"100" + call + "\"}")
                        .getBytes(StandardCharsets.UTF_8);
                exchange.getResponseHeaders().add("Content-Type", "application/json");
                exchange.sendResponseHeaders(200, body.length);
                try (OutputStream out = exchange.getResponseBody()) { out.write(body); }
            } finally { exchange.close(); }
        });
        server.start();
        try {
            String baseUrl = "http://127.0.0.1:" + server.getAddress().getPort();
            MgbxClient client = new MgbxClient("t", "t", baseUrl, "key", "secret", Duration.ofSeconds(5));
            assertFalse(client.capabilities().nativeBatchPlace(), "OMS must not call the unreliable native batch endpoint");

            List<VenueClient.PlaceRequest> requests = List.of(
                    new VenueClient.PlaceRequest("GDT_USDT", Domain.Side.BUY, DecimalValue.parse("0.1"), DecimalValue.parse("100"), "fm-a", 0),
                    new VenueClient.PlaceRequest("GDT_USDT", Domain.Side.BUY, DecimalValue.parse("0.2"), DecimalValue.parse("100"), "fm-b", 0),
                    new VenueClient.PlaceRequest("GDT_USDT", Domain.Side.SELL, DecimalValue.parse("0.3"), DecimalValue.parse("100"), "fm-c", 0));

            List<Domain.Order> orders = client.placePostOnlyBatch(requests);

            assertEquals(3, calls.get(), "each requested order must use its own POST");
            assertEquals(List.of("1001", "1002", "1003"), orders.stream().map(order -> order.orderId).toList());
            assertEquals(List.of(
                    "/spot/v1/u/trade/order/create",
                    "/spot/v1/u/trade/order/create",
                    "/spot/v1/u/trade/order/create"), paths);
            for (int index = 0; index < queries.size(); index++) {
                String query = queries.get(index);
                assertEquals("GDT_USDT", param(query, "symbol"));
                assertEquals("LIMIT", param(query, "tradeType"));
                assertEquals("GTX", param(query, "timeInForce"), "single-order fallback must remain post-only");
                assertEquals(requests.get(index).side().name(), param(query, "direction"));
                assertEquals("application/x-www-form-urlencoded", contentTypes.get(index));
                assertEquals("", bodies.get(index));
            }
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
