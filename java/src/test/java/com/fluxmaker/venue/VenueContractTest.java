package com.fluxmaker.venue;

import com.fluxmaker.math.DecimalValue;
import org.junit.jupiter.api.Test;

import java.util.List;
import java.util.Map;

import static org.junit.jupiter.api.Assertions.assertEquals;

class VenueContractTest {
    @Test void mgbxSignatureRetainsSeparatorForEmptyParameters() { assertEquals("&timestamp=123", MgbxClient.signaturePayload(Map.of(), "123")); }
    @Test void mgbxSignatureSortsRawValues() { assertEquals("a=[\"x\"]&z=a b&timestamp=123", MgbxClient.signaturePayload(Map.of("z", List.of("a b"), "a", List.of("[\"x\"]")), "123")); }
    @Test void precisionStepMatchesGo() { assertEquals(DecimalValue.parse("0.0001"), MgbxClient.precision(4)); }
}
