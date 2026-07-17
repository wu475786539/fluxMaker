package com.fluxmaker.admin;

import org.junit.jupiter.api.Test;

import java.io.IOException;
import java.util.concurrent.ExecutorService;
import java.util.concurrent.ThreadPoolExecutor;

import static org.junit.jupiter.api.Assertions.assertEquals;
import static org.junit.jupiter.api.Assertions.assertFalse;
import static org.junit.jupiter.api.Assertions.assertInstanceOf;
import static org.junit.jupiter.api.Assertions.assertTrue;

class AdminServerResilienceTest {
    @Test
    void usesBoundedPlatformExecutor() {
        ExecutorService service = AdminServer.newHttpExecutor();
        try {
            ThreadPoolExecutor executor = assertInstanceOf(ThreadPoolExecutor.class, service);
            assertEquals(8, executor.getCorePoolSize());
            assertEquals(32, executor.getMaximumPoolSize());
            assertEquals(64, executor.getQueue().remainingCapacity());
        } finally {
            service.shutdownNow();
        }
    }

    @Test
    void recognizesExpectedClientDisconnects() {
        assertTrue(AdminServer.clientDisconnected(
                new IllegalStateException(new IOException("Broken pipe"))));
        assertTrue(AdminServer.clientDisconnected(
                new IllegalStateException(new IOException("Connection reset by peer"))));
        assertFalse(AdminServer.clientDisconnected(
                new IllegalStateException(new IOException("database timed out"))));
    }
}
