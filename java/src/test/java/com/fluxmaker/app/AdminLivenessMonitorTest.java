package com.fluxmaker.app;

import org.junit.jupiter.api.Test;

import java.time.Duration;
import java.util.concurrent.CountDownLatch;
import java.util.concurrent.TimeUnit;
import java.util.concurrent.atomic.AtomicInteger;

import static org.junit.jupiter.api.Assertions.assertEquals;
import static org.junit.jupiter.api.Assertions.assertTrue;

class AdminLivenessMonitorTest {
    @Test
    void triggersOnlyAfterConsecutiveFailures() throws Exception {
        AtomicInteger attempts = new AtomicInteger();
        CountDownLatch terminal = new CountDownLatch(1);
        try (AdminLivenessMonitor monitor = new AdminLivenessMonitor(
                () -> { attempts.incrementAndGet(); throw new IllegalStateException("stalled"); },
                Duration.ZERO, Duration.ofMillis(5), 3, terminal::countDown)) {
            monitor.start();
            assertTrue(terminal.await(1, TimeUnit.SECONDS));
        }
        assertEquals(3, attempts.get());
    }

    @Test
    void successfulProbeResetsFailureCounter() throws Exception {
        AtomicInteger attempts = new AtomicInteger();
        CountDownLatch terminal = new CountDownLatch(1);
        try (AdminLivenessMonitor monitor = new AdminLivenessMonitor(() -> {
            int attempt = attempts.incrementAndGet();
            if (attempt != 3) throw new IllegalStateException("stalled");
        }, Duration.ZERO, Duration.ofMillis(5), 3, terminal::countDown)) {
            monitor.start();
            assertTrue(terminal.await(1, TimeUnit.SECONDS));
        }
        assertEquals(6, attempts.get());
    }
}
