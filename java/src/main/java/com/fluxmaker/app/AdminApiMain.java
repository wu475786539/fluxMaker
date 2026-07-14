package com.fluxmaker.app;

import com.fluxmaker.admin.AdminServer;
import com.fluxmaker.auth.AuthService;
import com.fluxmaker.config.ConfigStore;
import com.fluxmaker.credentials.CredentialService;
import com.fluxmaker.infra.Database;
import com.fluxmaker.infra.MigrationRunner;
import com.fluxmaker.infra.RedisClient;
import com.fluxmaker.runtime.RuntimeStore;

import java.nio.file.Files;
import java.nio.file.Path;
import java.util.concurrent.CountDownLatch;

public final class AdminApiMain {
    private AdminApiMain() {}

    public static void main(String[] args) {
        try { run(); }
        catch (RuntimeException e) { System.err.println("admin api stopped: " + EngineMain.rootMessage(e)); e.printStackTrace(System.err); System.exit(1); }
    }

    static void run() {
        try (Database database = Database.fromEnv(); RedisClient redis = RedisClient.fromEnv()) {
            database.ping(); redis.ping();
            new MigrationRunner(database, migrations()).migrate();
            CredentialService credentials = new CredentialService(database, System.getenv("CREDENTIAL_MASTER_KEY"));
            AuthService auth = new AuthService(database, redis);
            auth.bootstrapAdmin(System.getenv("ADMIN_EMAIL"), System.getenv("ADMIN_PASSWORD"));
            try (AdminServer server = new AdminServer(env("ADMIN_ADDR", ":8080"), database, redis, auth,
                    new ConfigStore(database, redis), credentials, new RuntimeStore(redis), System.getenv("METRICS_TOKEN"), webRoot())) {
                CountDownLatch stop = new CountDownLatch(1);
                Runtime.getRuntime().addShutdownHook(new Thread(stop::countDown, "shutdown"));
                server.start(); System.out.println("admin api listening on port " + server.port());
                try { stop.await(); } catch (InterruptedException e) { Thread.currentThread().interrupt(); }
            }
        }
    }

    private static Path migrations() {
        String configured = System.getenv("MIGRATIONS_DIR");
        if (configured != null && !configured.isBlank()) return Path.of(configured);
        for (Path candidate : new Path[]{Path.of("/app/migrations"), Path.of("internal/database/migrations"), Path.of("../internal/database/migrations")}) if (Files.isDirectory(candidate)) return candidate;
        throw new IllegalStateException("migration directory not found");
    }
    private static Path webRoot() {
        String configured = System.getenv("WEB_ROOT");
        if (configured != null && !configured.isBlank()) return Path.of(configured).toAbsolutePath().normalize();
        for (Path candidate : new Path[]{Path.of("/app/web"), Path.of("internal/adminapi/web"), Path.of("../internal/adminapi/web")}) if (Files.isDirectory(candidate)) return candidate.toAbsolutePath().normalize();
        throw new IllegalStateException("admin web directory not found");
    }
    private static String env(String key, String fallback) { String value = System.getenv(key); return value == null || value.isBlank() ? fallback : value; }
}
