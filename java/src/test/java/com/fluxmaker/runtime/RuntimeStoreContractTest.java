package com.fluxmaker.runtime;

import org.junit.jupiter.api.Test;

import java.time.Duration;

import static org.junit.jupiter.api.Assertions.assertEquals;

class RuntimeStoreContractTest {
    @Test void keepsGoRedisKeyAndTtlContracts() {
        assertEquals("fluxmaker:config:active", com.fluxmaker.config.ConfigStore.ACTIVE_CACHE_KEY);
        assertEquals("fluxmaker:runtime:engine", RuntimeStore.HEARTBEAT_KEY);
        assertEquals("fluxmaker:lease:market:", RuntimeStore.MARKET_LEASE_PREFIX);
        assertEquals(Duration.ofSeconds(15), RuntimeStore.HEARTBEAT_TTL);
        assertEquals(Duration.ofSeconds(45), RuntimeStore.SNAPSHOT_TTL);
    }
}
