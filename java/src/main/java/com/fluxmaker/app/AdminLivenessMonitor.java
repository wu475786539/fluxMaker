package com.fluxmaker.app;

import java.io.ByteArrayOutputStream;
import java.io.IOException;
import java.net.InetSocketAddress;
import java.net.Socket;
import java.nio.charset.StandardCharsets;
import java.time.Duration;
import java.util.concurrent.atomic.AtomicBoolean;

/**
 * Probes the real loopback HTTP path from a dedicated platform thread. If the
 * admin HTTP executor stops making progress, the process exits through the main
 * thread and Docker's restart policy can recover only the admin-api container.
 */
final class AdminLivenessMonitor implements AutoCloseable {
    @FunctionalInterface
    interface Probe { void check() throws Exception; }

    private final Probe probe;
    private final Duration startupGrace;
    private final Duration interval;
    private final int failureThreshold;
    private final Runnable terminalFailure;
    private final AtomicBoolean running = new AtomicBoolean();
    private final Thread thread;

    AdminLivenessMonitor(Probe probe, Duration startupGrace, Duration interval,
                         int failureThreshold, Runnable terminalFailure) {
        if (probe == null || terminalFailure == null) throw new IllegalArgumentException("admin liveness callbacks are required");
        if (startupGrace == null || startupGrace.isNegative()) throw new IllegalArgumentException("admin liveness startup grace must not be negative");
        if (interval == null || interval.isZero() || interval.isNegative()) throw new IllegalArgumentException("admin liveness interval must be positive");
        if (failureThreshold < 1) throw new IllegalArgumentException("admin liveness failure threshold must be positive");
        this.probe = probe;
        this.startupGrace = startupGrace;
        this.interval = interval;
        this.failureThreshold = failureThreshold;
        this.terminalFailure = terminalFailure;
        this.thread = Thread.ofPlatform().daemon(true).name("admin-liveness-monitor").unstarted(this::run);
    }

    static AdminLivenessMonitor loopback(int port, Duration startupGrace, Duration interval,
                                         Duration timeout, int failureThreshold, Runnable terminalFailure) {
        if (port < 1 || port > 65_535) throw new IllegalArgumentException("admin liveness port is invalid");
        if (timeout == null || timeout.isZero() || timeout.isNegative()) throw new IllegalArgumentException("admin liveness timeout must be positive");
        return new AdminLivenessMonitor(() -> probeLoopback(port, timeout), startupGrace, interval,
                failureThreshold, terminalFailure);
    }

    void start() {
        if (!running.compareAndSet(false, true)) throw new IllegalStateException("admin liveness monitor already started");
        thread.start();
    }

    private void run() {
        if (!waitFor(startupGrace)) return;
        int failures = 0;
        while (running.get()) {
            try {
                probe.check();
                if (failures > 0) System.out.println("admin liveness recovered after failures=" + failures);
                failures = 0;
            } catch (Exception e) {
                failures++;
                System.err.println("admin liveness failed attempt=" + failures + "/" + failureThreshold
                        + " reason=" + oneLine(e));
                if (failures >= failureThreshold) {
                    running.set(false);
                    terminalFailure.run();
                    return;
                }
            }
            if (!waitFor(interval)) return;
        }
    }

    private boolean waitFor(Duration duration) {
        if (duration.isZero()) return running.get();
        try {
            Thread.sleep(duration.toMillis());
            return running.get();
        } catch (InterruptedException e) {
            Thread.currentThread().interrupt();
            return false;
        }
    }

    private static void probeLoopback(int port, Duration timeout) throws IOException {
        int timeoutMillis = Math.toIntExact(Math.min(Integer.MAX_VALUE, Math.max(1, timeout.toMillis())));
        try (Socket socket = new Socket()) {
            socket.connect(new InetSocketAddress("127.0.0.1", port), timeoutMillis);
            socket.setSoTimeout(timeoutMillis);
            socket.getOutputStream().write(("GET /livez HTTP/1.1\r\n"
                    + "Host: 127.0.0.1\r\n"
                    + "Connection: close\r\n\r\n").getBytes(StandardCharsets.US_ASCII));
            socket.getOutputStream().flush();
            String status = readStatusLine(socket);
            if (!(status.startsWith("HTTP/1.1 200 ") || status.startsWith("HTTP/1.0 200 ")))
                throw new IOException("unexpected liveness response: " + status);
        }
    }

    private static String readStatusLine(Socket socket) throws IOException {
        ByteArrayOutputStream value = new ByteArrayOutputStream();
        for (int count = 0; count < 512; count++) {
            int next = socket.getInputStream().read();
            if (next < 0 || next == '\n') break;
            if (next != '\r') value.write(next);
        }
        if (value.size() == 0) throw new IOException("empty liveness response");
        return value.toString(StandardCharsets.US_ASCII);
    }

    private static String oneLine(Throwable error) {
        Throwable root = error;
        while (root.getCause() != null) root = root.getCause();
        String message = root.getMessage();
        return (message == null || message.isBlank() ? root.getClass().getSimpleName() : message)
                .replace('\n', ' ').replace('\r', ' ');
    }

    @Override public void close() {
        running.set(false);
        thread.interrupt();
        if (Thread.currentThread() == thread || !thread.isAlive()) return;
        try { thread.join(2_000); }
        catch (InterruptedException e) { Thread.currentThread().interrupt(); }
    }
}
