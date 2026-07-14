package com.fluxmaker.oracle;

import com.fasterxml.jackson.databind.JsonNode;
import com.fasterxml.jackson.databind.node.ArrayNode;
import com.fasterxml.jackson.databind.node.ObjectNode;
import com.fluxmaker.json.Json;

import java.io.IOException;
import java.math.BigInteger;
import java.net.URI;
import java.net.http.HttpClient;
import java.net.http.HttpRequest;
import java.net.http.HttpResponse;
import java.time.Duration;
import java.time.Instant;
import java.util.ArrayList;
import java.util.HexFormat;
import java.util.LinkedHashMap;
import java.util.List;
import java.util.Map;
import java.util.concurrent.atomic.AtomicLong;

public final class RpcClient {
    public static final String SELECTOR_GET_RESERVES = "0x0902f1ac", SELECTOR_TOKEN0 = "0x0dfe1681", SELECTOR_TOKEN1 = "0xd21220a7", SELECTOR_DECIMALS = "0x313ce567", SELECTOR_FACTORY = "0xc45a0155", SELECTOR_PRICE0 = "0x5909c0d5", SELECTOR_PRICE1 = "0x5a3d5493";
    private final List<String> urls;
    private final Duration timeout;
    private final HttpClient http;
    private final AtomicLong nextId = new AtomicLong();
    private Block latestBlock;
    private Instant blockFetchedAt;

    public record Block(long number, long timestamp, String tag) {}
    public record BatchCall(String to, String data) {}
    public static final class PairInfo { public String pairAddress; public String token0; public String token1; public String factory; }

    public RpcClient(List<String> urls, Duration timeout) { this.urls = new ArrayList<>(urls); this.timeout = timeout; this.http = HttpClient.newBuilder().connectTimeout(timeout).build(); }

    public synchronized Block latestBlock() {
        if (latestBlock != null && blockFetchedAt != null && Duration.between(blockFetchedAt, Instant.now()).compareTo(Duration.ofMillis(500)) < 0) return latestBlock;
        JsonNode raw = request("eth_getBlockByNumber", List.of("latest", false)); long number = hexLong(raw.path("number").asText()); long timestamp = hexLong(raw.path("timestamp").asText()); latestBlock = new Block(number, timestamp, "0x" + Long.toHexString(number)); blockFetchedAt = Instant.now(); return latestBlock;
    }

    public long chainId() { return hexLong(request("eth_chainId", List.of()).asText()); }

    public byte[] callAt(String to, String data, String blockTag) { return decodeHex(request("eth_call", List.of(Map.of("to", to, "data", data), blockTag)).asText()); }

    public List<byte[]> batchCall(List<BatchCall> calls, String blockTag) {
        if (calls.isEmpty()) return List.of();
        ArrayNode requests = Json.MAPPER.createArrayNode(); Map<Long, Integer> indices = new LinkedHashMap<>();
        for (int index = 0; index < calls.size(); index++) { long id = nextId.incrementAndGet(); indices.put(id, index); ObjectNode request = requests.addObject(); request.put("jsonrpc", "2.0"); request.put("id", id); request.put("method", "eth_call"); request.set("params", Json.tree(List.of(Map.of("to", calls.get(index).to(), "data", calls.get(index).data()), blockTag))); }
        RuntimeException last = null;
        for (String endpoint : urls) try {
            JsonNode decoded = post(endpoint, Json.write(requests)); if (!decoded.isArray()) { if (decoded.hasNonNull("error")) throw rpcError(decoded.path("error")); throw new IllegalStateException("rpc batch returned a non-array response"); } if (decoded.size() != calls.size()) throw new IllegalStateException("rpc batch returned " + decoded.size() + " results for " + calls.size() + " requests");
            byte[][] result = new byte[calls.size()][]; boolean[] filled = new boolean[calls.size()];
            for (JsonNode item : decoded) { if (item.hasNonNull("error")) throw rpcError(item.path("error")); long id = item.path("id").asLong(); Integer index = indices.get(id); if (index == null || filled[index]) throw new IllegalStateException("rpc batch returned unexpected or duplicate id " + id); result[index] = decodeHex(item.path("result").asText()); filled[index] = true; }
            return List.of(result);
        } catch (RuntimeException e) { last = e; }
        throw last == null ? new IllegalStateException("no rpc endpoints configured") : last;
    }

    public PairInfo inspectPair(String rawAddress) {
        String pair = normalizeAddress(rawAddress); String token0 = wordAddress(callAt(pair, SELECTOR_TOKEN0, "latest")); String token1 = wordAddress(callAt(pair, SELECTOR_TOKEN1, "latest")); String factory = wordAddress(callAt(pair, SELECTOR_FACTORY, "latest"));
        if (token0.equals(token1) || token0.endsWith("0000000000000000000000000000000000000000") || token1.endsWith("0000000000000000000000000000000000000000")) throw new IllegalArgumentException("pair returned invalid token addresses"); PairInfo result = new PairInfo(); result.pairAddress = pair; result.token0 = token0; result.token1 = token1; result.factory = factory; return result;
    }

    private JsonNode request(String method, Object params) {
        RuntimeException last = null;
        for (String endpoint : urls) try { ObjectNode request = Json.MAPPER.createObjectNode(); request.put("jsonrpc", "2.0"); request.put("id", nextId.incrementAndGet()); request.put("method", method); request.set("params", Json.tree(params)); JsonNode response = post(endpoint, Json.write(request)); if (response.hasNonNull("error")) throw rpcError(response.path("error")); return response.path("result"); }
        catch (RuntimeException e) { last = e; }
        throw last == null ? new IllegalStateException("no rpc endpoints configured") : last;
    }

    private JsonNode post(String endpoint, String payload) {
        HttpRequest request = HttpRequest.newBuilder(URI.create(endpoint)).timeout(timeout).header("Content-Type", "application/json").POST(HttpRequest.BodyPublishers.ofString(payload)).build();
        try { HttpResponse<String> response = http.send(request, HttpResponse.BodyHandlers.ofString()); if (response.statusCode() / 100 != 2) throw new IllegalStateException("rpc http " + response.statusCode() + ": " + response.body()); return Json.MAPPER.readTree(response.body()); }
        catch (IOException e) { throw new IllegalStateException("rpc request: " + e.getMessage(), e); }
        catch (InterruptedException e) { Thread.currentThread().interrupt(); throw new IllegalStateException("rpc request interrupted", e); }
    }

    private static IllegalStateException rpcError(JsonNode error) { return new IllegalStateException("rpc error " + error.path("code").asInt() + ": " + error.path("message").asText()); }
    public static long hexLong(String value) { String normalized = value == null ? "" : value.replaceFirst("^0x", ""); return normalized.isEmpty() ? 0 : new BigInteger(normalized, 16).longValueExact(); }
    public static byte[] decodeHex(String raw) { String value = raw == null ? "" : raw.replaceFirst("^0x", ""); if ((value.length() & 1) != 0) value = "0" + value; return HexFormat.of().parseHex(value); }
    public static BigInteger wordInt(byte[] data, int index) { int start = index * 32; if (data.length < start + 32) return BigInteger.ZERO; return new BigInteger(1, java.util.Arrays.copyOfRange(data, start, start + 32)); }
    public static String wordAddress(byte[] data) { if (data.length < 32) throw new IllegalArgumentException("short address response"); return "0x" + HexFormat.of().formatHex(data, 12, 32); }
    public static String normalizeAddress(String value) { String result = value == null ? "" : value.trim().toLowerCase(); return result.startsWith("0x") ? result : "0x" + result; }
}
