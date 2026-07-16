package com.fluxmaker.infra;

import java.io.IOException;
import java.nio.charset.StandardCharsets;
import java.nio.file.Files;
import java.nio.file.Path;
import java.sql.Connection;
import java.sql.PreparedStatement;
import java.sql.ResultSet;
import java.sql.SQLException;
import java.sql.Statement;
import java.util.List;

public final class MigrationRunner {
    private final Database database;
    private final Path directory;

    public MigrationRunner(Database database, Path directory) {
        this.database = database;
        this.directory = directory;
    }

    public void migrate() {
        try (Connection connection = database.connection(); Statement statement = connection.createStatement()) {
            statement.execute("CREATE TABLE IF NOT EXISTS schema_migrations (version TEXT PRIMARY KEY, applied_at TIMESTAMPTZ NOT NULL DEFAULT now())");
            List<Path> files;
            try (var stream = Files.list(directory)) {
                // Only real .sql migrations. Skip dotfiles first: a macOS AppleDouble
                // sidecar is named "._001_init.sql" — it ENDS in .sql but is binary, so
                // an extension check alone would still try (and fail) to read it. This
                // also drops .DS_Store and mirrors what a clean checkout ships.
                files = stream.filter(Files::isRegularFile)
                        .filter(path -> {
                            String fileName = path.getFileName().toString();
                            return !fileName.startsWith(".") && fileName.toLowerCase().endsWith(".sql");
                        })
                        .sorted()
                        .toList();
            }
            for (Path file : files) apply(connection, file);
        } catch (SQLException | IOException e) {
            throw new IllegalStateException("migrate: " + e.getMessage(), e);
        }
    }

    private static void apply(Connection connection, Path file) throws SQLException, IOException {
        try (PreparedStatement check = connection.prepareStatement("SELECT EXISTS(SELECT 1 FROM schema_migrations WHERE version=?)")) {
            check.setString(1, file.getFileName().toString());
            try (ResultSet result = check.executeQuery()) {
                result.next();
                if (result.getBoolean(1)) return;
            }
        }
        boolean autoCommit = connection.getAutoCommit();
        connection.setAutoCommit(false);
        try (Statement statement = connection.createStatement()) {
            // Read as bytes and decode UTF-8 leniently (Files.readString is strict and
            // aborts the whole run on a single bad byte). A genuinely malformed .sql
            // then surfaces as a clear SQL error instead of a cryptic decode crash.
            statement.execute(new String(Files.readAllBytes(file), StandardCharsets.UTF_8));
            try (PreparedStatement insert = connection.prepareStatement("INSERT INTO schema_migrations(version) VALUES(?)")) {
                insert.setString(1, file.getFileName().toString());
                insert.executeUpdate();
            }
            connection.commit();
        } catch (SQLException | IOException e) {
            connection.rollback();
            throw e;
        } finally {
            connection.setAutoCommit(autoCommit);
        }
    }
}
