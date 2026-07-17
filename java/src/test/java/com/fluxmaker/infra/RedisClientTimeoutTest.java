package com.fluxmaker.infra;

import org.junit.jupiter.api.Test;

import static org.junit.jupiter.api.Assertions.assertEquals;
import static org.junit.jupiter.api.Assertions.assertThrows;

class RedisClientTimeoutTest {
    @Test
    void usesConfiguredTimeout() {
        RedisClient client = new RedisClient("redis:6379", "secret", 0, 2_000);
        assertEquals(2_000, client.timeoutMs());
    }

    @Test
    void rejectsUnboundedTimeouts() {
        assertThrows(IllegalArgumentException.class,
                () -> new RedisClient("redis:6379", "secret", 0, 0));
    }
}
