package com.fluxmaker.app;

import org.junit.jupiter.api.Test;

import java.util.Map;

import static org.junit.jupiter.api.Assertions.assertFalse;
import static org.junit.jupiter.api.Assertions.assertTrue;

class EngineMainTest {
    @Test void coldStartCanActivateAWriteBlockedRuntimeForManualRecovery() {
        assertTrue(EngineMain.shouldActivateDegradedRuntime(false, Map.of(
                "gdt_usdt", "mgbx: venue/reference deviation exceeds limit")));
        assertFalse(EngineMain.shouldActivateDegradedRuntime(true, Map.of(
                "gdt_usdt", "mgbx: venue/reference deviation exceeds limit")));
        assertFalse(EngineMain.shouldActivateDegradedRuntime(false, Map.of()));
    }
}
