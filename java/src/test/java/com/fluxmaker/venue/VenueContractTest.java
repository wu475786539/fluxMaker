package com.fluxmaker.venue;

import com.fluxmaker.math.DecimalValue;
import org.junit.jupiter.api.Test;

import javax.net.ssl.SSLException;
import java.io.IOException;
import java.net.http.HttpTimeoutException;
import java.time.Duration;
import java.util.List;
import java.util.Map;

import static org.junit.jupiter.api.Assertions.assertEquals;
import static org.junit.jupiter.api.Assertions.assertTrue;

class VenueContractTest {
    @Test void mgbxSignatureRetainsSeparatorForEmptyParameters() { assertEquals("&timestamp=123", MgbxClient.signaturePayload(Map.of(), "123")); }
    @Test void mgbxSignatureSortsRawValues() { assertEquals("a=[\"x\"]&z=a b&timestamp=123", MgbxClient.signaturePayload(Map.of("z", List.of("a b"), "a", List.of("[\"x\"]")), "123")); }
    @Test void precisionStepMatchesGo() { assertEquals(DecimalValue.parse("0.0001"), MgbxClient.precision(4)); }

    @Test void mgbxTransportFailureIncludesOperationTimingAndExceptionType() {
        String timeout = MgbxClient.transportFailureMessage(
                "mgbx/gdt_usdt", "POST", "/spot/v1/u/trade/order/batch/cancel",
                15_007, Duration.ofSeconds(15), new HttpTimeoutException("request timed out"));
        assertTrue(timeout.contains("venue=mgbx/gdt_usdt"));
        assertTrue(timeout.contains("method=POST path=/spot/v1/u/trade/order/batch/cancel"));
        assertTrue(timeout.contains("elapsed_ms=15007 timeout_ms=15000"));
        assertTrue(timeout.contains("exception=java.net.http.HttpTimeoutException"));
        assertTrue(timeout.contains("cause_chain=java.net.http.HttpTimeoutException(request timed out)"));

        IOException parserFailure = new IOException(
                "HTTP/1.1 header parser received no bytes", new SSLException("Tag mismatch"));
        String nested = MgbxClient.transportFailureMessage(
                "mgbx/gdt_usdt", "GET", "/spot/v1/u/trade/order/list",
                824, Duration.ofSeconds(15), parserFailure);
        assertTrue(nested.contains("java.io.IOException(HTTP/1.1 header parser received no bytes)"));
        assertTrue(nested.contains("javax.net.ssl.SSLException(Tag mismatch)"));
    }
}
