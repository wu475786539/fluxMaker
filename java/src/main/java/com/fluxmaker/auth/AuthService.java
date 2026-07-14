package com.fluxmaker.auth;

import com.fluxmaker.infra.Database;
import com.fluxmaker.infra.RedisClient;
import com.fluxmaker.json.Json;

import java.nio.charset.StandardCharsets;
import java.security.MessageDigest;
import java.security.NoSuchAlgorithmException;
import java.security.SecureRandom;
import java.sql.Connection;
import java.sql.PreparedStatement;
import java.sql.ResultSet;
import java.sql.SQLException;
import java.time.Duration;
import java.time.Instant;
import java.util.ArrayList;
import java.util.HexFormat;
import java.util.List;

import static com.fluxmaker.config.ConfigStore.nullableLong;

public final class AuthService {
    private static final Duration SESSION_TTL = Duration.ofHours(12);
    private static final SecureRandom RANDOM = new SecureRandom();
    private final Database database;
    private final RedisClient redis;

    public static final class Session {
        public long userId;
        public String email = "";
        public List<String> permissions = new ArrayList<>();
        public boolean allInstruments;
        public List<String> instruments = new ArrayList<>();
        public long authorizationVersion;
        public Instant expiresAt;
        public boolean has(String permission) { return permissions.contains(permission); }
        public boolean canAccessInstrument(String id) { return allInstruments || instruments.contains(id); }
    }

    public record Login(String token, Session session) {}

    public AuthService(Database database, RedisClient redis) { this.database = database; this.redis = redis; }

    public void bootstrapAdmin(String rawEmail, String password) {
        String email = rawEmail == null ? "" : rawEmail.trim().toLowerCase();
        if (email.isEmpty() || password == null || password.isEmpty()) throw new IllegalArgumentException("ADMIN_EMAIL and ADMIN_PASSWORD are required");
        try (Connection connection = database.connection()) {
            long userId;
            String storedHash = null;
            boolean enabled = false;
            try (PreparedStatement statement = connection.prepareStatement("SELECT id,password_hash,enabled FROM users WHERE lower(email)=lower(?)")) {
                statement.setString(1, email);
                try (ResultSet result = statement.executeQuery()) {
                    if (result.next()) { userId = result.getLong(1); storedHash = result.getString(2); enabled = result.getBoolean(3); }
                    else userId = 0;
                }
            }
            if (userId == 0) {
                try (PreparedStatement statement = connection.prepareStatement("INSERT INTO users(email,password_hash) VALUES(?,?) RETURNING id")) {
                    statement.setString(1, email); statement.setString(2, PasswordHasher.hash(password));
                    try (ResultSet result = statement.executeQuery()) { result.next(); userId = result.getLong(1); }
                }
            } else if (!PasswordHasher.verify(storedHash, password)) {
                try (PreparedStatement statement = connection.prepareStatement("UPDATE users SET password_hash=?,enabled=TRUE,authorization_version=authorization_version+1,password_changed_at=now(),updated_at=now() WHERE id=?")) {
                    statement.setString(1, PasswordHasher.hash(password)); statement.setLong(2, userId); statement.executeUpdate();
                }
            } else if (!enabled) {
                try (PreparedStatement statement = connection.prepareStatement("UPDATE users SET enabled=TRUE,authorization_version=authorization_version+1,updated_at=now() WHERE id=?")) { statement.setLong(1, userId); statement.executeUpdate(); }
            }
            try (PreparedStatement statement = connection.prepareStatement("INSERT INTO user_roles(user_id,role_id) SELECT ?,id FROM roles WHERE code='super_admin' ON CONFLICT DO NOTHING")) { statement.setLong(1, userId); statement.executeUpdate(); }
        } catch (SQLException e) { throw failure(e); }
    }

    public Login login(String email, String password) {
        try (Connection connection = database.connection()) {
            long userId; String canonicalEmail; String storedHash; boolean enabled;
            try (PreparedStatement statement = connection.prepareStatement("SELECT id,email,password_hash,enabled FROM users WHERE lower(email)=lower(?)")) {
                statement.setString(1, email == null ? "" : email.trim());
                try (ResultSet result = statement.executeQuery()) {
                    if (!result.next()) throw new InvalidCredentials();
                    userId = result.getLong(1); canonicalEmail = result.getString(2); storedHash = result.getString(3); enabled = result.getBoolean(4);
                }
            }
            if (!enabled || !PasswordHasher.verify(storedHash, password == null ? "" : password)) throw new InvalidCredentials();
            Session session = buildSession(connection, userId, canonicalEmail);
            try (PreparedStatement statement = connection.prepareStatement("UPDATE users SET last_login_at=now() WHERE id=?")) { statement.setLong(1, userId); statement.executeUpdate(); }
            byte[] tokenBytes = new byte[32]; RANDOM.nextBytes(tokenBytes);
            String token = HexFormat.of().formatHex(tokenBytes);
            redis.set(sessionKey(token), Json.writeBytes(session), SESSION_TTL);
            return new Login(token, session);
        } catch (SQLException e) { throw failure(e); }
    }

    public Session authenticate(String token) {
        if (token == null || token.isEmpty()) throw new InvalidCredentials();
        byte[] value = redis.get(sessionKey(token));
        if (value == null) throw new InvalidCredentials();
        Session session;
        try { session = Json.read(value, Session.class); } catch (RuntimeException e) { throw new InvalidCredentials(); }
        if (session.expiresAt == null || Instant.now().isAfter(session.expiresAt)) throw new InvalidCredentials();
        try (Connection connection = database.connection(); PreparedStatement statement = connection.prepareStatement("SELECT enabled,authorization_version FROM users WHERE id=?")) {
            statement.setLong(1, session.userId);
            try (ResultSet result = statement.executeQuery()) {
                if (!result.next() || !result.getBoolean(1) || result.getLong(2) != session.authorizationVersion) {
                    redis.delete(sessionKey(token)); throw new InvalidCredentials();
                }
            }
            return session;
        } catch (SQLException e) { redis.delete(sessionKey(token)); throw new InvalidCredentials(); }
    }

    public void logout(String token) { if (token != null) redis.delete(sessionKey(token)); }

    public void changePassword(long userId, String currentPassword, String newPassword) {
        try (Connection connection = database.connection()) {
            connection.setAutoCommit(false);
            try {
                String stored;
                try (PreparedStatement statement = connection.prepareStatement("SELECT password_hash FROM users WHERE id=? AND enabled=TRUE FOR UPDATE")) {
                    statement.setLong(1, userId); try (ResultSet result = statement.executeQuery()) { if (!result.next()) throw new InvalidCredentials(); stored = result.getString(1); }
                }
                if (!PasswordHasher.verify(stored, currentPassword)) throw new InvalidCredentials();
                try (PreparedStatement statement = connection.prepareStatement("UPDATE users SET password_hash=?,authorization_version=authorization_version+1,password_changed_at=now(),updated_at=now() WHERE id=?")) {
                    statement.setString(1, PasswordHasher.hash(newPassword)); statement.setLong(2, userId); statement.executeUpdate();
                }
                connection.commit();
            } catch (RuntimeException | SQLException e) { connection.rollback(); throw e; }
        } catch (SQLException e) { throw failure(e); }
    }

    private static Session buildSession(Connection connection, long userId, String email) throws SQLException {
        Session session = new Session(); session.userId = userId; session.email = email;
        session.permissions = strings(connection, "SELECT DISTINCT rp.permission_code FROM user_roles ur JOIN role_permissions rp ON rp.role_id=ur.role_id WHERE ur.user_id=? ORDER BY rp.permission_code", userId);
        session.instruments = strings(connection, "SELECT DISTINCT ri.instrument_id FROM user_roles ur JOIN role_instruments ri ON ri.role_id=ur.role_id WHERE ur.user_id=? ORDER BY ri.instrument_id", userId);
        try (PreparedStatement statement = connection.prepareStatement("SELECT COALESCE(bool_or(r.all_instruments),FALSE) FROM user_roles ur JOIN roles r ON r.id=ur.role_id WHERE ur.user_id=?")) { statement.setLong(1, userId); try (ResultSet result = statement.executeQuery()) { result.next(); session.allInstruments = result.getBoolean(1); } }
        try (PreparedStatement statement = connection.prepareStatement("SELECT authorization_version FROM users WHERE id=?")) { statement.setLong(1, userId); try (ResultSet result = statement.executeQuery()) { result.next(); session.authorizationVersion = result.getLong(1); } }
        session.expiresAt = Instant.now().plus(SESSION_TTL);
        return session;
    }

    private static List<String> strings(Connection connection, String sql, long userId) throws SQLException {
        List<String> result = new ArrayList<>();
        try (PreparedStatement statement = connection.prepareStatement(sql)) { statement.setLong(1, userId); try (ResultSet rows = statement.executeQuery()) { while (rows.next()) result.add(rows.getString(1)); } }
        return result;
    }

    public static String sessionKey(String token) {
        try { return "fluxmaker:session:" + HexFormat.of().formatHex(MessageDigest.getInstance("SHA-256").digest(token.getBytes(StandardCharsets.UTF_8))); }
        catch (NoSuchAlgorithmException e) { throw new IllegalStateException(e); }
    }

    private static IllegalStateException failure(SQLException e) { return new IllegalStateException(e.getMessage(), e); }
    public static final class InvalidCredentials extends RuntimeException { public InvalidCredentials() { super("invalid credentials"); } }
}
