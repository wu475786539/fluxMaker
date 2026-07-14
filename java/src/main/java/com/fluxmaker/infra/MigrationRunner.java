package com.fluxmaker.infra;

import java.io.IOException;
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
                files = stream.filter(Files::isRegularFile).sorted().toList();
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
            statement.execute(Files.readString(file));
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
