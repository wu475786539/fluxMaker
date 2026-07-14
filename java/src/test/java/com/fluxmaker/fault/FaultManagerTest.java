package com.fluxmaker.fault;

import org.junit.jupiter.api.Test;

import static org.junit.jupiter.api.Assertions.assertEquals;
import static org.junit.jupiter.api.Assertions.assertTrue;

class FaultManagerTest {
    @Test void followsGoStateMachine() {
        FaultManager manager = new FaultManager(2, 2, null);
        assertEquals(FaultManager.DEGRADED, manager.failure("market", "book", new RuntimeException("x"), false).state().status);
        assertTrue(manager.failure("market", "book", new RuntimeException("x"), false).shouldCancel());
        assertEquals(FaultManager.PAUSED, manager.healthy("market", 0).state().status);
        assertEquals(FaultManager.RECOVERING, manager.healthy("market", 0).state().status);
        assertTrue(manager.healthy("market", 0).allowQuotes());
    }
}
