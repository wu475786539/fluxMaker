package com.fluxmaker.app;

import com.fluxmaker.config.AppConfig;
import com.fluxmaker.config.ConfigDiff;
import com.fluxmaker.config.ConfigStore;
import com.fluxmaker.credentials.CredentialService;
import com.fluxmaker.domain.Domain;
import com.fluxmaker.infra.Database;
import com.fluxmaker.infra.RedisClient;
import com.fluxmaker.runtime.RuntimeStore;

import java.time.Duration;
import java.time.Instant;
import java.util.concurrent.Executors;
import java.util.concurrent.ScheduledExecutorService;
import java.util.concurrent.TimeUnit;
import java.util.concurrent.atomic.AtomicBoolean;
import java.util.concurrent.atomic.AtomicLong;
import java.util.concurrent.atomic.AtomicReference;

public final class EngineMain {
    private EngineMain() {}

    public static void main(String[] args) {
        TimestampedStreams.install();
        boolean once = java.util.Arrays.asList(args).contains("--once") || java.util.Arrays.asList(args).contains("-once");
        try { run(once); }
        catch (RuntimeException e) { System.err.println("fluxmaker stopped: " + rootMessage(e)); System.exit(1); }
    }

    static void run(boolean once) {
        try (Database database = Database.fromEnv(); RedisClient redis = RedisClient.fromEnv()) {
            database.ping(); redis.ping();
            ConfigStore store = new ConfigStore(database, redis);
            RuntimeStore runtimeStore = new RuntimeStore(redis);
            CredentialService credentials = new CredentialService(database, System.getenv("CREDENTIAL_MASTER_KEY"));
            if (once) {
                ConfigStore.Snapshot snapshot = store.loadActive();
                if (snapshot.config.mode == Domain.Mode.live) throw new IllegalStateException("-once is disabled in live mode");
                AppRuntime runtime = RuntimeFactory.build(snapshot.config, credentials, runtimeStore, null);
                runtime.engine.runOnce(); HeartbeatFiles.touch(runtime.config.heartbeatPath); return;
            }

            AtomicBoolean stopping = new AtomicBoolean();
            AtomicReference<AppRuntime> heartbeatRuntime = new AtomicReference<>();
            AtomicLong activeVersionRef = new AtomicLong(), desiredVersionRef = new AtomicLong();
            AtomicReference<String> runtimeErrorRef = new AtomicReference<>("waiting for a published configuration");
            Runtime.getRuntime().addShutdownHook(new Thread(() -> {
                stopping.set(true);
                // Release market leases immediately so a replacement instance can take
                // over within seconds, instead of the new process waiting out the full
                // lease TTL (minutes) because SIGKILL cut off the graceful shutdown path.
                AppRuntime current = heartbeatRuntime.get();
                if (current != null) try { current.engine.releaseLeases(); } catch (RuntimeException ignored) {}
            }, "shutdown"));
            ScheduledExecutorService heartbeat = Executors.newSingleThreadScheduledExecutor(r -> new Thread(r, "heartbeat"));
            heartbeat.scheduleAtFixedRate(() -> {
                AppRuntime current = heartbeatRuntime.get();
                try {
                    String path = current == null ? "" : current.config.heartbeatPath;
                    HeartbeatFiles.touch(path);
                    runtimeStore.heartbeat(activeVersionRef.get(), desiredVersionRef.get(), current != null, runtimeErrorRef.get());
                } catch (RuntimeException e) { System.err.println("runtime heartbeat failed: " + rootMessage(e)); }
            }, 0, 2, TimeUnit.SECONDS);

            AppRuntime runtime = null, pendingRuntime = null;
            AppConfig activeRawConfig = null;
            long activeVersion = 0, pendingVersion = 0;
            ConfigDiff.Plan pendingPlan = null;
            Instant nextRun = Instant.EPOCH, nextReload = Instant.EPOCH, nextControl = Instant.EPOCH, nextRules = Instant.MAX, nextBlockedRetry = Instant.MAX;
            try {
                while (!stopping.get()) {
                    Instant now = Instant.now();
                    if (!now.isBefore(nextReload)) {
                        nextReload = now.plusSeconds(2);
                        try {
                            ConfigStore.Snapshot snapshot = store.loadActive();
                            if (snapshot.version != activeVersion) {
                                desiredVersionRef.set(snapshot.version);
                                ConfigDiff.Plan hotPlan = ConfigDiff.build(activeRawConfig, snapshot.config);
                                if (runtime != null && !hotPlan.structural) {
                                    runtime.engine.applyParameters(snapshot.config);
                                    runtime.config = runtime.engine.effectiveConfig();
                                    activeVersion = snapshot.version; activeRawConfig = snapshot.config; pendingRuntime = null; pendingVersion = 0;
                                    runtimeErrorRef.set(""); nextRun = Instant.EPOCH; runtimeStore.setAppliedVersion(activeVersion);
                                    System.out.println("parameters applied in place, version=" + activeVersion);
                                } else {
                                    if (pendingRuntime == null || pendingVersion != snapshot.version) {
                                        try {
                                            pendingRuntime = RuntimeFactory.build(snapshot.config, credentials, runtimeStore, runtime);
                                            pendingVersion = snapshot.version;
                                            pendingPlan = ConfigDiff.build(runtime == null ? null : runtime.config, pendingRuntime.config);
                                        } catch (RuntimeException e) {
                                            pendingRuntime = null; pendingVersion = 0;
                                            runtimeErrorRef.set("version v" + snapshot.version + " preparation failed: " + rootMessage(e));
                                            System.err.println(runtimeErrorRef.get());
                                        }
                                    }
                                    if (pendingRuntime != null) {
                                        try {
                                            pendingRuntime.prepare();
                                            if (runtime != null) runtime.applyCleanup(pendingPlan);
                                            // Release the superseded engine's worker pool. Do NOT shutdown()
                                            // it: orders and market leases carry over to the new engine, which
                                            // inherits the same owner id.
                                            if (runtime != null) runtime.engine.close();
                                            runtime = pendingRuntime; activeVersion = snapshot.version; activeRawConfig = snapshot.config;
                                            pendingRuntime = null; pendingVersion = 0; runtimeErrorRef.set(""); nextRun = Instant.EPOCH;
                                            nextRules = now.plusSeconds(runtime.config.rulesRefreshSeconds); nextBlockedRetry = now.plusSeconds(30);
                                            runtimeStore.setAppliedVersion(activeVersion);
                                            System.out.println("published configuration applied, version=" + activeVersion + ", blocked=" + runtime.engine.blockedInstruments());
                                        } catch (RuntimeException e) {
                                            String preflightError = rootMessage(e);
                                            if (shouldActivateDegradedRuntime(runtime != null, pendingRuntime.engine.blockedInstruments())) {
                                                // On a cold start there may be no runnable instrument because the
                                                // venue book itself needs a manual repair. Keep the engine in a
                                                // write-blocked control-plane mode so runtime actions (especially
                                                // manual book rebuild) can still be processed. Each instrument stays
                                                // protected by preflightBlocked until recovery succeeds.
                                                runtime = pendingRuntime; activeVersion = snapshot.version; activeRawConfig = snapshot.config;
                                                pendingRuntime = null; pendingVersion = 0;
                                                runtimeErrorRef.set("version v" + snapshot.version + " running degraded: " + preflightError);
                                                nextRun = Instant.EPOCH; nextRules = now.plusSeconds(runtime.config.rulesRefreshSeconds);
                                                nextBlockedRetry = now.plusSeconds(30); runtimeStore.setAppliedVersion(activeVersion);
                                                System.err.println(runtimeErrorRef.get());
                                            } else {
                                                runtimeErrorRef.set("version v" + snapshot.version + " preflight pending: " + preflightError);
                                                if (!preflightError.toLowerCase().contains("twap warming")) { pendingRuntime = null; pendingVersion = 0; }
                                                System.err.println(runtimeErrorRef.get());
                                            }
                                        }
                                    }
                                }
                            } else desiredVersionRef.set(activeVersion);
                        } catch (ConfigStore.NotFound e) {
                            if (runtime == null) runtimeErrorRef.set("waiting for a published configuration");
                        } catch (RuntimeException e) {
                            runtimeErrorRef.set("load active configuration: " + rootMessage(e));
                            System.err.println(runtimeErrorRef.get());
                        }
                    }
                    if (runtime != null && !now.isBefore(nextControl)) {
                        try { runtime.engine.applyControls(); clearDegradedErrorIfRecovered(runtimeErrorRef, runtime); } catch (RuntimeException e) { System.err.println("apply runtime controls failed: " + rootMessage(e)); }
                        nextControl = now.plusMillis(500);
                    }
                    if (runtime != null && !now.isBefore(nextRules)) {
                        try { int changes = runtime.refreshMarketRules(); if (changes > 0) System.out.println("trading rules changed=" + changes); }
                        catch (RuntimeException e) { System.err.println("periodic trading rule refresh failed: " + rootMessage(e)); }
                        nextRules = now.plusSeconds(runtime.config.rulesRefreshSeconds);
                    }
                    if (runtime != null && !now.isBefore(nextBlockedRetry)) {
                        try { int recovered = runtime.retryBlocked(); if (recovered > 0) System.out.println("blocked instruments recovered=" + recovered); clearDegradedErrorIfRecovered(runtimeErrorRef, runtime); }
                        catch (RuntimeException e) { System.err.println("blocked instruments remain degraded: " + rootMessage(e)); }
                        nextBlockedRetry = now.plusSeconds(30);
                    }
                    if (runtime != null && !now.isBefore(nextRun)) {
                        try { runtime.engine.runOnce(); } catch (RuntimeException e) { System.err.println("tick failed: " + rootMessage(e)); }
                        nextRun = now.plus(runtime.config.pollInterval());
                    }
                    heartbeatRuntime.set(runtime); activeVersionRef.set(activeVersion);
                    try { Thread.sleep(250); } catch (InterruptedException e) { Thread.currentThread().interrupt(); stopping.set(true); }
                }
            } finally {
                heartbeat.shutdownNow();
                if (runtime != null) runtime.engine.shutdown();
            }
        }
    }

    static String rootMessage(Throwable error) {
        Throwable current = error; while (current.getCause() != null) current = current.getCause();
        return current.getMessage() == null ? current.toString() : current.getMessage();
    }

    static boolean shouldActivateDegradedRuntime(boolean hasActiveRuntime, java.util.Map<String, String> blocked) {
        return !hasActiveRuntime && blocked != null && !blocked.isEmpty();
    }

    private static void clearDegradedErrorIfRecovered(AtomicReference<String> runtimeError, AppRuntime runtime) {
        if (runtime.engine.blockedInstruments().isEmpty() && runtimeError.get().contains(" running degraded: ")) runtimeError.set("");
    }
}
