package com.fluxmaker.infra;

import java.net.URI;
import java.net.URLDecoder;
import java.nio.charset.StandardCharsets;
import java.sql.Connection;
import java.sql.DriverManager;
import java.sql.ResultSet;
import java.sql.SQLException;
import java.sql.Statement;
import java.util.Properties;

public final class Database implements AutoCloseable {
    private static final int DEFAULT_CONNECT_TIMEOUT_SECONDS = 3;
    private static final int DEFAULT_LOGIN_TIMEOUT_SECONDS = 3;
    private static final int DEFAULT_SOCKET_TIMEOUT_SECONDS = 5;
    private static final int DEFAULT_QUERY_TIMEOUT_SECONDS = 5;
    private static final int DEFAULT_CANCEL_TIMEOUT_SECONDS = 2;
    private final String jdbcUrl;
    private final Properties properties;
    private final int queryTimeoutSeconds;

    public Database(String databaseUrl) {
        this(databaseUrl,
                DEFAULT_CONNECT_TIMEOUT_SECONDS,
                DEFAULT_LOGIN_TIMEOUT_SECONDS,
                DEFAULT_SOCKET_TIMEOUT_SECONDS,
                DEFAULT_QUERY_TIMEOUT_SECONDS,
                DEFAULT_CANCEL_TIMEOUT_SECONDS);
    }

    Database(String databaseUrl, int connectTimeoutSeconds, int loginTimeoutSeconds,
             int socketTimeoutSeconds, int queryTimeoutSeconds, int cancelTimeoutSeconds) {
        if (databaseUrl == null || databaseUrl.isBlank()) throw new IllegalArgumentException("DATABASE_URL is required");
        requirePositive("DB_CONNECT_TIMEOUT_SECONDS", connectTimeoutSeconds);
        requirePositive("DB_LOGIN_TIMEOUT_SECONDS", loginTimeoutSeconds);
        requirePositive("DB_SOCKET_TIMEOUT_SECONDS", socketTimeoutSeconds);
        requirePositive("DB_QUERY_TIMEOUT_SECONDS", queryTimeoutSeconds);
        requirePositive("DB_CANCEL_TIMEOUT_SECONDS", cancelTimeoutSeconds);
        URI uri = URI.create(databaseUrl.replaceFirst("^postgresql?://", "http://"));
        String query = uri.getRawQuery();
        this.jdbcUrl = "jdbc:postgresql://" + uri.getHost() + ":" + (uri.getPort() < 0 ? 5432 : uri.getPort()) + uri.getPath() + (query == null ? "" : "?" + query);
        this.properties = new Properties();
        properties.setProperty("connectTimeout", Integer.toString(connectTimeoutSeconds));
        properties.setProperty("loginTimeout", Integer.toString(loginTimeoutSeconds));
        properties.setProperty("socketTimeout", Integer.toString(socketTimeoutSeconds));
        properties.setProperty("queryTimeout", Integer.toString(queryTimeoutSeconds));
        properties.setProperty("cancelSignalTimeout", Integer.toString(cancelTimeoutSeconds));
        properties.setProperty("tcpKeepAlive", "true");
        this.queryTimeoutSeconds = queryTimeoutSeconds;
        if (uri.getUserInfo() != null) {
            String[] userInfo = uri.getRawUserInfo().split(":", 2);
            properties.setProperty("user", decode(userInfo[0]));
            if (userInfo.length > 1) properties.setProperty("password", decode(userInfo[1]));
        }
    }

    public static Database fromEnv() {
        return new Database(
                System.getenv("DATABASE_URL"),
                positiveEnv("DB_CONNECT_TIMEOUT_SECONDS", DEFAULT_CONNECT_TIMEOUT_SECONDS),
                positiveEnv("DB_LOGIN_TIMEOUT_SECONDS", DEFAULT_LOGIN_TIMEOUT_SECONDS),
                positiveEnv("DB_SOCKET_TIMEOUT_SECONDS", DEFAULT_SOCKET_TIMEOUT_SECONDS),
                positiveEnv("DB_QUERY_TIMEOUT_SECONDS", DEFAULT_QUERY_TIMEOUT_SECONDS),
                positiveEnv("DB_CANCEL_TIMEOUT_SECONDS", DEFAULT_CANCEL_TIMEOUT_SECONDS));
    }

    public Connection connection() throws SQLException { return DriverManager.getConnection(jdbcUrl, properties); }

    public void ping() {
        try (Connection connection = connection(); Statement statement = connection.createStatement()) {
            statement.setQueryTimeout(queryTimeoutSeconds);
            try (ResultSet ignored = statement.executeQuery("SELECT 1")) {
                // query success is sufficient
            }
        } catch (SQLException e) {
            throw new IllegalStateException("ping postgres: " + e.getMessage(), e);
        }
    }

    Properties connectionProperties() {
        Properties copy = new Properties();
        copy.putAll(properties);
        return copy;
    }

    int queryTimeoutSeconds() { return queryTimeoutSeconds; }

    private static int positiveEnv(String key, int fallback) {
        String value = System.getenv(key);
        if (value == null || value.isBlank()) return fallback;
        try {
            int parsed = Integer.parseInt(value.trim());
            requirePositive(key, parsed);
            return parsed;
        } catch (NumberFormatException e) {
            throw new IllegalArgumentException(key + " must be a positive integer", e);
        }
    }

    private static void requirePositive(String key, int value) {
        if (value <= 0 || value > 300) throw new IllegalArgumentException(key + " must be between 1 and 300 seconds");
    }

    private static String decode(String value) { return URLDecoder.decode(value, StandardCharsets.UTF_8); }
    @Override public void close() { /* DriverManager connections are method-scoped */ }
}
