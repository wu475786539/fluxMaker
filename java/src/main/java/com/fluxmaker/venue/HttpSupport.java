package com.fluxmaker.venue;

import com.fasterxml.jackson.databind.JsonNode;
import com.fluxmaker.json.Json;

import javax.crypto.Mac;
import javax.crypto.spec.SecretKeySpec;
import java.net.URLEncoder;
import java.net.http.HttpClient;
import java.nio.charset.StandardCharsets;
import java.security.GeneralSecurityException;
import java.time.Duration;
import java.util.ArrayList;
import java.util.HexFormat;
import java.util.LinkedHashMap;
import java.util.List;
import java.util.Map;
import java.util.TreeMap;

public final class HttpSupport {
    private HttpSupport() {}
    public static HttpClient client() { return HttpClient.newBuilder().connectTimeout(Duration.ofSeconds(10)).build(); }
    public static Map<String, List<String>> params() { return new LinkedHashMap<>(); }
    public static void set(Map<String, List<String>> values, String key, Object value) { values.put(key, new ArrayList<>(List.of(String.valueOf(value)))); }
    public static Map<String, List<String>> copy(Map<String, List<String>> input) { Map<String, List<String>> result = new LinkedHashMap<>(); input.forEach((key, values) -> result.put(key, new ArrayList<>(values))); return result; }
    public static String encode(Map<String, List<String>> values) {
        List<String> parts = new ArrayList<>();
        new TreeMap<>(values).forEach((key, entries) -> entries.forEach(value -> parts.add(url(key) + "=" + url(value))));
        return String.join("&", parts);
    }
    public static String rawSorted(Map<String, List<String>> values) {
        List<String> parts = new ArrayList<>();
        new TreeMap<>(values).forEach((key, entries) -> entries.forEach(value -> parts.add(key + "=" + value)));
        return String.join("&", parts);
    }
    public static String hmacSha256(String secret, String payload) {
        try { Mac mac = Mac.getInstance("HmacSHA256"); mac.init(new SecretKeySpec(secret.getBytes(StandardCharsets.UTF_8), "HmacSHA256")); return HexFormat.of().formatHex(mac.doFinal(payload.getBytes(StandardCharsets.UTF_8))); }
        catch (GeneralSecurityException e) { throw new IllegalStateException(e); }
    }
    public static JsonNode json(String body) { try { return Json.MAPPER.readTree(body); } catch (Exception e) { throw new IllegalArgumentException("decode response", e); } }
    private static String url(String value) { return URLEncoder.encode(value, StandardCharsets.UTF_8); }
}
