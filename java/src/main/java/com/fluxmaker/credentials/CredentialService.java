package com.fluxmaker.credentials;

import com.fluxmaker.config.AppConfig;
import com.fluxmaker.infra.Database;

import javax.crypto.AEADBadTagException;
import javax.crypto.Cipher;
import javax.crypto.spec.GCMParameterSpec;
import javax.crypto.spec.SecretKeySpec;
import java.nio.ByteBuffer;
import java.nio.charset.StandardCharsets;
import java.security.GeneralSecurityException;
import java.security.MessageDigest;
import java.security.SecureRandom;
import java.sql.Connection;
import java.sql.PreparedStatement;
import java.sql.ResultSet;
import java.sql.SQLException;
import java.time.Instant;
import java.util.ArrayList;
import java.util.Base64;
import java.util.HexFormat;
import java.util.List;
import java.util.Locale;

import static com.fluxmaker.config.ConfigStore.nullableLong;

public final class CredentialService {
    private static final SecureRandom RANDOM = new SecureRandom();
    private final Database database;
    private final byte[] masterKey;

    public static final class Metadata {
        public long id; public String name; public String venueType; public String apiKeyLast4; public String fingerprint; public boolean enabled; public Instant updatedAt;
    }
    public record Secret(String apiKey, String apiSecret) {}

    public CredentialService(Database database, String encodedKey) {
        this.database = database;
        try { this.masterKey = Base64.getDecoder().decode(encodedKey == null ? "" : encodedKey.trim()); }
        catch (IllegalArgumentException e) { throw invalidKey(); }
        if (masterKey.length != 32) throw invalidKey();
    }

    public List<Metadata> list() {
        try (Connection connection = database.connection(); PreparedStatement statement = connection.prepareStatement("SELECT id,name,venue_type,api_key_last4,fingerprint,enabled,updated_at FROM venue_credentials ORDER BY venue_type,name"); ResultSet rows = statement.executeQuery()) {
            List<Metadata> result = new ArrayList<>(); while (rows.next()) result.add(metadata(rows)); return result;
        } catch (SQLException e) { throw failure(e); }
    }

    public Metadata create(String rawName, String rawVenue, String rawApiKey, String apiSecret, long userId) {
        String name = trim(rawName), venue = trim(rawVenue).toLowerCase(Locale.ROOT), apiKey = trim(rawApiKey);
        if (name.isEmpty() || AppConfig.adapterSpec(venue) == null || apiKey.isEmpty() || apiSecret == null || apiSecret.isEmpty()) throw new IllegalArgumentException("name, supported venue, api key and secret are required");
        try (Connection connection = database.connection(); PreparedStatement statement = connection.prepareStatement("INSERT INTO venue_credentials(name,venue_type,api_key_cipher,api_secret_cipher,api_key_last4,fingerprint,created_by,updated_by) VALUES(?,?,?,?,?,?,?,?) RETURNING id,name,venue_type,api_key_last4,fingerprint,enabled,updated_at")) {
            statement.setString(1, name); statement.setString(2, venue); statement.setBytes(3, encrypt(apiKey, venue + ":api-key")); statement.setBytes(4, encrypt(apiSecret, venue + ":api-secret")); statement.setString(5, last4(apiKey)); statement.setString(6, fingerprint(apiKey)); nullableLong(statement, 7, userId); nullableLong(statement, 8, userId);
            try (ResultSet result = statement.executeQuery()) { result.next(); return metadata(result); }
        } catch (SQLException e) { throw failure(e); }
    }

    public Metadata update(long id, String rawName, String rawApiKey, String apiSecret, Boolean enabled, long userId) {
        if (id <= 0) throw new IllegalArgumentException("invalid credential id");
        try (Connection connection = database.connection()) {
            connection.setAutoCommit(false);
            try {
                String currentName, venue, last, currentFingerprint; byte[] keyCipher, secretCipher; boolean currentEnabled;
                try (PreparedStatement statement = connection.prepareStatement("SELECT name,venue_type,api_key_cipher,api_secret_cipher,api_key_last4,fingerprint,enabled FROM venue_credentials WHERE id=? FOR UPDATE")) {
                    statement.setLong(1, id);
                    try (ResultSet result = statement.executeQuery()) {
                        if (!result.next()) throw new IllegalArgumentException("credential not found");
                        currentName = result.getString(1); venue = result.getString(2); keyCipher = result.getBytes(3); secretCipher = result.getBytes(4);
                        last = result.getString(5); currentFingerprint = result.getString(6); currentEnabled = result.getBoolean(7);
                    }
                }
                String name = trim(rawName).isEmpty() ? currentName : trim(rawName);
                String apiKey = trim(rawApiKey);
                if (!apiKey.isEmpty()) { keyCipher = encrypt(apiKey, venue + ":api-key"); last = last4(apiKey); currentFingerprint = fingerprint(apiKey); }
                if (apiSecret != null && !apiSecret.isEmpty()) secretCipher = encrypt(apiSecret, venue + ":api-secret");
                if (enabled != null) currentEnabled = enabled;
                try (PreparedStatement statement = connection.prepareStatement("UPDATE venue_credentials SET name=?,api_key_cipher=?,api_secret_cipher=?,api_key_last4=?,fingerprint=?,enabled=?,updated_by=?,updated_at=now() WHERE id=? RETURNING id,name,venue_type,api_key_last4,fingerprint,enabled,updated_at")) {
                    statement.setString(1, name); statement.setBytes(2, keyCipher); statement.setBytes(3, secretCipher); statement.setString(4, last); statement.setString(5, currentFingerprint); statement.setBoolean(6, currentEnabled); nullableLong(statement, 7, userId); statement.setLong(8, id);
                    try (ResultSet result = statement.executeQuery()) { result.next(); Metadata item = metadata(result); connection.commit(); return item; }
                }
            } catch (RuntimeException | SQLException e) { connection.rollback(); throw e; }
        } catch (SQLException e) { throw failure(e); }
    }

    public Secret resolve(long id, String expectedVenue) {
        String expected = trim(expectedVenue).toLowerCase(Locale.ROOT);
        try (Connection connection = database.connection(); PreparedStatement statement = connection.prepareStatement("SELECT venue_type,api_key_cipher,api_secret_cipher,enabled FROM venue_credentials WHERE id=?")) {
            statement.setLong(1, id); try (ResultSet result = statement.executeQuery()) {
                if (!result.next()) throw new IllegalArgumentException("credential " + id + " not found");
                String venue = result.getString(1); if (!result.getBoolean(4)) throw new IllegalArgumentException("credential " + id + " is disabled");
                if (!venue.equals(expected)) throw new IllegalArgumentException("credential " + id + " belongs to " + venue + ", expected " + expected);
                return new Secret(decrypt(result.getBytes(2), venue + ":api-key"), decrypt(result.getBytes(3), venue + ":api-secret"));
            }
        } catch (SQLException e) { throw failure(e); }
    }

    public void validateReference(long id, String expectedVenue) { resolve(id, expectedVenue); }

    public byte[] encrypt(String value, String aad) {
        byte[] nonce = new byte[12]; RANDOM.nextBytes(nonce); return encrypt(value, aad, nonce);
    }

    byte[] encrypt(String value, String aad, byte[] nonce) {
        try {
            Cipher cipher = Cipher.getInstance("AES/GCM/NoPadding");
            cipher.init(Cipher.ENCRYPT_MODE, new SecretKeySpec(masterKey, "AES"), new GCMParameterSpec(128, nonce));
            cipher.updateAAD(aad.getBytes(StandardCharsets.UTF_8));
            return ByteBuffer.allocate(nonce.length + value.getBytes(StandardCharsets.UTF_8).length + 16).put(nonce).put(cipher.doFinal(value.getBytes(StandardCharsets.UTF_8))).array();
        } catch (GeneralSecurityException e) { throw new IllegalStateException("encrypt credential", e); }
    }

    public String decrypt(byte[] value, String aad) {
        if (value == null || value.length < 12) throw new IllegalArgumentException("invalid encrypted credential");
        try {
            Cipher cipher = Cipher.getInstance("AES/GCM/NoPadding");
            cipher.init(Cipher.DECRYPT_MODE, new SecretKeySpec(masterKey, "AES"), new GCMParameterSpec(128, value, 0, 12));
            cipher.updateAAD(aad.getBytes(StandardCharsets.UTF_8));
            return new String(cipher.doFinal(value, 12, value.length - 12), StandardCharsets.UTF_8);
        } catch (GeneralSecurityException e) { throw new IllegalArgumentException("decrypt credential", e); }
    }

    private static Metadata metadata(ResultSet result) throws SQLException {
        Metadata item = new Metadata(); item.id = result.getLong(1); item.name = result.getString(2); item.venueType = result.getString(3); item.apiKeyLast4 = result.getString(4); item.fingerprint = result.getString(5); item.enabled = result.getBoolean(6); item.updatedAt = result.getTimestamp(7).toInstant(); return item;
    }
    private static String trim(String value) { return value == null ? "" : value.trim(); }
    private static String last4(String value) { int points = value.codePointCount(0, value.length()); return points <= 4 ? value : value.substring(value.offsetByCodePoints(0, points - 4)); }
    private static String fingerprint(String value) { try { return HexFormat.of().formatHex(MessageDigest.getInstance("SHA-256").digest(value.getBytes(StandardCharsets.UTF_8))).substring(0, 12); } catch (GeneralSecurityException e) { throw new IllegalStateException(e); } }
    private static IllegalArgumentException invalidKey() { return new IllegalArgumentException("CREDENTIAL_MASTER_KEY must be base64 for exactly 32 bytes"); }
    private static IllegalStateException failure(SQLException e) { return new IllegalStateException(e.getMessage(), e); }
}
