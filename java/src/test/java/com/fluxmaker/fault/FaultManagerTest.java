package com.fluxmaker.fault;

import org.junit.jupiter.api.Test;

import static org.junit.jupiter.api.Assertions.assertEquals;
import static org.junit.jupiter.api.Assertions.assertFalse;
import static org.junit.jupiter.api.Assertions.assertTrue;

class FaultManagerTest {
    @Test void transientFailuresPauseWritesButRetainOrdersAtTheThreshold() {
        FaultManager manager = new FaultManager(2, 2, null);
        FaultManager.Decision first = manager.failure("market", "open_orders", new RuntimeException("timeout"), false);
        assertEquals(FaultManager.DEGRADED, first.state().status);
        assertFalse(first.shouldCancel());

        FaultManager.Decision second = manager.failure("market", "open_orders", new RuntimeException("timeout"), false);
        assertEquals(FaultManager.PAUSED, second.state().status);
        assertFalse(second.shouldCancel(), "a transient request threshold must not clear resting orders");
        assertTrue(second.state().ordersRetained);

        assertEquals(FaultManager.RECOVERING, manager.healthy("market", 3).state().status);
        assertTrue(manager.healthy("market", 3).allowQuotes());
    }

    @Test void explicitHardFailureStillCancelsAndRequiresRecovery() {
        FaultManager manager = new FaultManager(2, 2, null);

        FaultManager.Decision hard = manager.failure("market", "reference", new RuntimeException("stale"), true);

        assertEquals(FaultManager.CANCELING, hard.state().status);
        assertTrue(hard.shouldCancel());
        assertFalse(hard.state().ordersRetained);
        assertTrue(hard.state().hardCancel);
        assertTrue(manager.healthy("market", 1).shouldCancel());
        assertEquals(FaultManager.PAUSED, manager.healthy("market", 0).state().status);
        assertEquals(FaultManager.RECOVERING, manager.healthy("market", 0).state().status);
        assertTrue(manager.healthy("market", 0).allowQuotes());
    }
}
