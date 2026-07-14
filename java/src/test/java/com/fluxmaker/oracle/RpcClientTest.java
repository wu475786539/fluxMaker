package com.fluxmaker.oracle;

import com.fasterxml.jackson.databind.JsonNode;
import com.fasterxml.jackson.databind.node.ArrayNode;
import com.fasterxml.jackson.databind.node.ObjectNode;
import com.fluxmaker.json.Json;
import com.sun.net.httpserver.HttpExchange;
import com.sun.net.httpserver.HttpServer;
import org.junit.jupiter.api.Test;

import java.io.IOException;
import java.io.OutputStream;
import java.math.BigInteger;
import java.net.InetSocketAddress;
import java.nio.charset.StandardCharsets;
import java.time.Duration;
import java.util.HexFormat;
import java.util.List;
import java.util.concurrent.atomic.AtomicInteger;

import static org.junit.jupiter.api.Assertions.assertEquals;
import static org.junit.jupiter.api.Assertions.assertThrows;
import static org.junit.jupiter.api.Assertions.assertTrue;

class RpcClientTest {
    @Test void decodesHexAndWords() {
        assertEquals(56, RpcClient.hexLong("0x38"));
        byte[] word = new byte[32]; word[31] = 42;
        assertEquals(BigInteger.valueOf(42), RpcClient.wordInt(word, 0));
    }
    @Test void normalizesAddresses() { assertEquals("0xabc", RpcClient.normalizeAddress(" ABC ")); }

    @Test void batchIsOneRoundTripAndReordersByRequestId() throws IOException {
        AtomicInteger calls = new AtomicInteger();
        HttpServer server = start(exchange -> {
            calls.incrementAndGet();
            JsonNode batch = Json.MAPPER.readTree(exchange.getRequestBody());
            ArrayNode responses = Json.MAPPER.createArrayNode();
            // Echo each call's data selector as its result, in the SAME order...
            for (JsonNode item : batch) {
                ObjectNode node = responses.addObject();
                node.put("jsonrpc", "2.0"); node.put("id", item.path("id").asLong());
                node.put("result", item.path("params").get(0).path("data").asText());
            }
            // ...then reverse the array to prove BatchCall reorders by id, not position.
            ArrayNode shuffled = Json.MAPPER.createArrayNode();
            for (int i = responses.size() - 1; i >= 0; i--) shuffled.add(responses.get(i));
            respond(exchange, Json.write(shuffled));
        });
        try {
            RpcClient rpc = new RpcClient(List.of(baseUrl(server)), Duration.ofSeconds(2));
            List<byte[]> results = rpc.batchCall(List.of(
                    new RpcClient.BatchCall("0xa", "0x01"),
                    new RpcClient.BatchCall("0xb", "0x02"),
                    new RpcClient.BatchCall("0xc", "0x03")), "latest");
            assertEquals(1, calls.get(), "3 calls must be a single HTTP round-trip");
            assertEquals("01", HexFormat.of().formatHex(results.get(0)));
            assertEquals("02", HexFormat.of().formatHex(results.get(1)));
            assertEquals("03", HexFormat.of().formatHex(results.get(2)));
        } finally { server.stop(0); }
    }

    @Test void batchSurfacesSingleErrorObjectResponse() throws IOException {
        // A node that rejects the whole batch replies with one error object, not an
        // array; the client must surface that error message rather than a vague count.
        HttpServer server = start(exchange -> {
            ObjectNode node = Json.MAPPER.createObjectNode();
            node.put("jsonrpc", "2.0"); node.put("id", 1);
            ObjectNode error = node.putObject("error"); error.put("code", -32600); error.put("message", "batch too large");
            respond(exchange, Json.write(node));
        });
        try {
            RpcClient rpc = new RpcClient(List.of(baseUrl(server)), Duration.ofSeconds(2));
            IllegalStateException failure = assertThrows(IllegalStateException.class,
                    () -> rpc.batchCall(List.of(new RpcClient.BatchCall("0xa", "0x01")), "latest"));
            assertTrue(failure.getMessage().contains("batch too large"), failure.getMessage());
        } finally { server.stop(0); }
    }

    private interface Handler { void handle(HttpExchange exchange) throws IOException; }

    private static HttpServer start(Handler handler) throws IOException {
        HttpServer server = HttpServer.create(new InetSocketAddress("127.0.0.1", 0), 0);
        server.createContext("/", exchange -> { try { handler.handle(exchange); } finally { exchange.close(); } });
        server.start();
        return server;
    }

    private static String baseUrl(HttpServer server) { return "http://127.0.0.1:" + server.getAddress().getPort() + "/"; }

    private static void respond(HttpExchange exchange, String body) throws IOException {
        byte[] bytes = body.getBytes(StandardCharsets.UTF_8);
        exchange.getResponseHeaders().add("Content-Type", "application/json");
        exchange.sendResponseHeaders(200, bytes.length);
        try (OutputStream out = exchange.getResponseBody()) { out.write(bytes); }
    }
}
