package com.fluxmaker.app;

import com.fluxmaker.config.AppConfig;
import com.fluxmaker.config.ConfigStore;
import com.fluxmaker.credentials.CredentialService;
import com.fluxmaker.domain.Domain;
import com.fluxmaker.infra.Database;
import com.fluxmaker.infra.RedisClient;
import com.fluxmaker.oms.Reconciler;
import com.fluxmaker.runtime.RuntimeStore;
import com.fluxmaker.venue.VenueClient;

import java.time.Duration;
import java.time.Instant;
import java.util.ArrayList;
import java.util.List;
import java.util.Map;
import java.util.concurrent.atomic.AtomicBoolean;

public final class WatchdogMain {
    private WatchdogMain() {}

    public static void main(String[] args) {
        boolean once = java.util.Arrays.asList(args).contains("--once") || java.util.Arrays.asList(args).contains("-once");
        try { run(once); }
        catch (RuntimeException e) { System.err.println("watchdog stopped: " + EngineMain.rootMessage(e)); System.exit(1); }
    }

    static void run(boolean once) {
        AtomicBoolean stopping = new AtomicBoolean();
        Runtime.getRuntime().addShutdownHook(new Thread(() -> stopping.set(true), "shutdown"));
        try (Database database = Database.fromEnv(); RedisClient redis = RedisClient.fromEnv()) {
            database.ping(); redis.ping();
            ConfigStore configs = new ConfigStore(database, redis);
            RuntimeStore runtime = new RuntimeStore(redis);
            CredentialService credentials = new CredentialService(database, System.getenv("CREDENTIAL_MASTER_KEY"));
            boolean episodeActive = false;
            while (!stopping.get()) {
                ConfigStore.Snapshot snapshot;
                try {
                    long applied = runtime.appliedVersion();
                    snapshot = applied > 0 ? configs.loadVersion(applied) : configs.loadActive();
                } catch (ConfigStore.NotFound e) {
                    if (once) throw e;
                    sleep(Duration.ofSeconds(5)); continue;
                } catch (RuntimeException e) {
                    if (once) throw e;
                    System.err.println("load configuration failed: " + EngineMain.rootMessage(e)); sleep(Duration.ofSeconds(5)); continue;
                }
                AppConfig config = snapshot.config;
                if (config.mode != Domain.Mode.live) {
                    runtime.reportWatchdog(true, false, "", "");
                    if (once) return; sleep(Duration.ofSeconds(5)); continue;
                }
                config.applyRuntimeSafetyDefaults(); config.validate();
                Duration age = Duration.ZERO; RuntimeException heartbeatError = null;
                try { age = HeartbeatFiles.age(config.heartbeatPath); } catch (RuntimeException e) { heartbeatError = e; }
                Instant progress = runtime.tradingProgress();
                boolean progressStale = progress != null && Duration.between(progress, Instant.now()).compareTo(Duration.ofSeconds(config.tradingProgressTimeoutSeconds)) > 0;
                boolean stale = heartbeatError != null || age.compareTo(Duration.ofSeconds(config.watchdogTimeoutSeconds)) > 0 || progressStale;
                if (stale && !episodeActive) {
                    String reason = reason(age, heartbeatError, progressStale);
                    System.err.println("liveness stale; canceling managed orders: " + reason);
                    try {
                        Map<String, VenueClient> clients = RuntimeFactory.buildVenues(config, credentials);
                        cancelAll(config, clients, runtime);
                        episodeActive = true; runtime.reportWatchdog(false, true, reason, "");
                    } catch (RuntimeException e) {
                        runtime.reportWatchdog(false, true, reason, EngineMain.rootMessage(e)); throw e;
                    }
                } else if (stale) runtime.reportWatchdog(false, false, reason(age, heartbeatError, progressStale), "");
                else {
                    if (episodeActive) System.out.println("liveness recovered");
                    episodeActive = false; runtime.reportWatchdog(true, false, "", "");
                }
                if (once) return;
                sleep(Duration.ofSeconds(Math.max(1, config.watchdogTimeoutSeconds / 3)));
            }
        }
    }

    static void cancelAll(AppConfig config, Map<String, VenueClient> clients, RuntimeStore runtime) {
        Reconciler reconciler = new Reconciler(runtime); List<String> failures = new ArrayList<>();
        for (AppConfig.InstrumentConfig instrument : config.instruments) for (Map.Entry<String, AppConfig.VenueConfig> entry : config.venues.entrySet()) {
            AppConfig.VenueConfig venue = entry.getValue(); AppConfig.VenueMarketConfig market = venue.markets.get(instrument.id);
            if (!venue.enabled || !venue.tradingEnabled || market == null) continue;
            VenueClient client = clients.get(RuntimeFactory.clientKey(entry.getKey(), instrument.id));
            if (client == null) { failures.add(entry.getKey() + "/" + instrument.id + ": client missing"); continue; }
            try { reconciler.cancelManaged(client, instrument.id, market.symbol, () -> {}); }
            catch (RuntimeException e) { failures.add(entry.getKey() + "/" + instrument.id + ": " + e.getMessage()); }
        }
        if (!failures.isEmpty()) throw new IllegalStateException("watchdog cancel failures: " + String.join("; ", failures));
    }

    private static String reason(Duration age, RuntimeException heartbeatError, boolean progressStale) {
        if (heartbeatError != null) return "process heartbeat unavailable: " + EngineMain.rootMessage(heartbeatError);
        if (progressStale) return "trading progress timeout";
        return "process heartbeat stale: " + age.toSeconds() + "s";
    }
    private static void sleep(Duration duration) { try { Thread.sleep(duration.toMillis()); } catch (InterruptedException e) { Thread.currentThread().interrupt(); } }
}
