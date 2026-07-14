package com.fluxmaker.config;

import com.fluxmaker.infra.Database;
import com.fluxmaker.infra.RedisClient;
import com.fluxmaker.json.Json;
import org.postgresql.util.PGobject;

import java.sql.Connection;
import java.sql.PreparedStatement;
import java.sql.ResultSet;
import java.sql.SQLException;
import java.sql.Statement;
import java.sql.Timestamp;
import java.time.Duration;
import java.time.Instant;
import java.util.Map;

public final class ConfigStore {
    public static final String ACTIVE_CACHE_KEY = "fluxmaker:config:active";
    private final Database database;
    private final RedisClient redis;

    public static final class Snapshot {
        public long version;
        public AppConfig config;
        public Instant publishedAt;
        public Snapshot() {}
        public Snapshot(long version, AppConfig config, Instant publishedAt) {
            this.version = version;
            this.config = config;
            this.publishedAt = publishedAt;
        }
    }

    public ConfigStore(Database database, RedisClient redis) {
        this.database = database;
        this.redis = redis;
    }

    public AppConfig getDraft() {
        try (Connection connection = database.connection(); PreparedStatement statement = connection.prepareStatement("SELECT payload::text FROM draft_configs WHERE id=1"); ResultSet result = statement.executeQuery()) {
            if (!result.next()) throw new NotFound("no draft configuration");
            AppConfig config = Json.read(result.getString(1), AppConfig.class);
            config.normalizeStrategySizing();
            return config;
        } catch (SQLException e) { throw failure(e); }
    }

    public void putDraft(AppConfig config, long userId) {
        config.normalizeStrategySizing();
        try (Connection connection = database.connection(); PreparedStatement statement = connection.prepareStatement("INSERT INTO draft_configs(id,payload,updated_by,updated_at) VALUES(1,?, ?,now()) ON CONFLICT(id) DO UPDATE SET payload=excluded.payload,updated_by=excluded.updated_by,updated_at=now()")) {
            statement.setObject(1, jsonb(config));
            nullableLong(statement, 2, userId);
            statement.executeUpdate();
            audit(connection, userId, "config.draft.update", "config", "draft", Map.of());
        } catch (SQLException e) { throw failure(e); }
    }

    public Snapshot publishDraft(long userId) { return save(null, userId, false); }
    public Snapshot saveActive(AppConfig config, long userId) { return save(config, userId, true); }

    private Snapshot save(AppConfig incoming, long userId, boolean updateDraft) {
        try (Connection connection = database.connection()) {
            connection.setAutoCommit(false);
            try {
                try (StatementCloser lock = StatementCloser.execute(connection, "SELECT pg_advisory_xact_lock(764223)")) { /* held by transaction */ }
                AppConfig config = incoming;
                if (config == null) {
                    try (PreparedStatement statement = connection.prepareStatement("SELECT payload::text FROM draft_configs WHERE id=1 FOR UPDATE"); ResultSet result = statement.executeQuery()) {
                        if (!result.next()) throw new NotFound("no draft configuration");
                        config = Json.read(result.getString(1), AppConfig.class);
                    }
                }
                config.normalizeStrategySizing();
                config.validate();
                if (updateDraft) {
                    try (PreparedStatement statement = connection.prepareStatement("INSERT INTO draft_configs(id,payload,updated_by,updated_at) VALUES(1,?,?,now()) ON CONFLICT(id) DO UPDATE SET payload=excluded.payload,updated_by=excluded.updated_by,updated_at=now()")) {
                        statement.setObject(1, jsonb(config)); nullableLong(statement, 2, userId); statement.executeUpdate();
                    }
                }
                long version;
                try (PreparedStatement statement = connection.prepareStatement("SELECT COALESCE(MAX(version),0)+1 FROM config_snapshots"); ResultSet result = statement.executeQuery()) { result.next(); version = result.getLong(1); }
                try (PreparedStatement statement = connection.prepareStatement("UPDATE config_snapshots SET active=FALSE WHERE active")) { statement.executeUpdate(); }
                Instant publishedAt;
                try (PreparedStatement statement = connection.prepareStatement("INSERT INTO config_snapshots(version,payload,active,published_by) VALUES(?,?,TRUE,?) RETURNING published_at")) {
                    statement.setLong(1, version); statement.setObject(2, jsonb(config)); nullableLong(statement, 3, userId);
                    try (ResultSet result = statement.executeQuery()) { result.next(); publishedAt = result.getTimestamp(1).toInstant(); }
                }
                audit(connection, userId, updateDraft ? "config.save" : "config.publish", "config", Long.toString(version), Map.of("version", version));
                connection.commit();
                Snapshot snapshot = new Snapshot(version, config, publishedAt);
                cache(snapshot);
                return snapshot;
            } catch (RuntimeException | SQLException e) {
                connection.rollback();
                throw e;
            }
        } catch (SQLException e) { throw failure(e); }
    }

    public Snapshot loadActive() {
        try (Connection connection = database.connection(); PreparedStatement statement = connection.prepareStatement("SELECT version,published_at FROM config_snapshots WHERE active=TRUE ORDER BY version DESC LIMIT 1"); ResultSet result = statement.executeQuery()) {
            if (!result.next()) throw new NotFound("no published configuration");
            long version = result.getLong(1);
            Instant publishedAt = result.getTimestamp(2).toInstant();
            Snapshot cached = cached();
            if (cached != null && cached.version == version) return cached;
            return loadVersion(connection, version, publishedAt, true);
        } catch (SQLException e) {
            Snapshot cached = cached();
            if (cached != null) return cached;
            throw failure(e);
        }
    }

    public Snapshot loadVersion(long version) {
        try (Connection connection = database.connection()) { return loadVersion(connection, version, null, false); }
        catch (SQLException e) { throw failure(e); }
    }

    private Snapshot loadVersion(Connection connection, long version, Instant knownTime, boolean shouldCache) throws SQLException {
        try (PreparedStatement statement = connection.prepareStatement("SELECT payload::text,published_at FROM config_snapshots WHERE version=?")) {
            statement.setLong(1, version);
            try (ResultSet result = statement.executeQuery()) {
                if (!result.next()) throw new NotFound("no published configuration");
                AppConfig config = Json.read(result.getString(1), AppConfig.class);
                config.normalizeStrategySizing();
                config.validate();
                Snapshot snapshot = new Snapshot(version, config, knownTime == null ? result.getTimestamp(2).toInstant() : knownTime);
                if (shouldCache) cache(snapshot);
                return snapshot;
            }
        }
    }

    private Snapshot cached() {
        byte[] value = redis.get(ACTIVE_CACHE_KEY);
        if (value == null) return null;
        try {
            Snapshot snapshot = Json.read(value, Snapshot.class);
            if (snapshot.version <= 0) return null;
            snapshot.config.normalizeStrategySizing();
            snapshot.config.validate();
            return snapshot;
        } catch (RuntimeException ignored) { return null; }
    }

    private void cache(Snapshot snapshot) { redis.set(ACTIVE_CACHE_KEY, Json.writeBytes(snapshot), Duration.ofHours(24)); }

    private static void audit(Connection connection, long userId, String action, String type, String id, Object details) throws SQLException {
        try (PreparedStatement statement = connection.prepareStatement("INSERT INTO audit_logs(user_id,action,resource_type,resource_id,details) VALUES(?,?,?,?,?)")) {
            nullableLong(statement, 1, userId); statement.setString(2, action); statement.setString(3, type); statement.setString(4, id); statement.setObject(5, jsonb(details)); statement.executeUpdate();
        }
    }

    public static PGobject jsonb(Object value) {
        try { PGobject object = new PGobject(); object.setType("jsonb"); object.setValue(Json.write(value)); return object; }
        catch (SQLException e) { throw new IllegalArgumentException(e); }
    }

    public static void nullableLong(PreparedStatement statement, int index, long value) throws SQLException {
        if (value <= 0) statement.setNull(index, java.sql.Types.BIGINT); else statement.setLong(index, value);
    }

    private static IllegalStateException failure(SQLException e) { return new IllegalStateException(e.getMessage(), e); }

    public static final class NotFound extends RuntimeException { public NotFound(String message) { super(message); } }

    private static final class StatementCloser implements AutoCloseable {
        private final Statement statement;
        private StatementCloser(Statement statement) { this.statement = statement; }
        static StatementCloser execute(Connection connection, String sql) throws SQLException { Statement statement = connection.createStatement(); statement.execute(sql); return new StatementCloser(statement); }
        @Override public void close() throws SQLException { statement.close(); }
    }
}
