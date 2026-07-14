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
    private final String jdbcUrl;
    private final Properties properties;

    public Database(String databaseUrl) {
        if (databaseUrl == null || databaseUrl.isBlank()) throw new IllegalArgumentException("DATABASE_URL is required");
        URI uri = URI.create(databaseUrl.replaceFirst("^postgresql?://", "http://"));
        String query = uri.getRawQuery();
        this.jdbcUrl = "jdbc:postgresql://" + uri.getHost() + ":" + (uri.getPort() < 0 ? 5432 : uri.getPort()) + uri.getPath() + (query == null ? "" : "?" + query);
        this.properties = new Properties();
        if (uri.getUserInfo() != null) {
            String[] userInfo = uri.getRawUserInfo().split(":", 2);
            properties.setProperty("user", decode(userInfo[0]));
            if (userInfo.length > 1) properties.setProperty("password", decode(userInfo[1]));
        }
    }

    public static Database fromEnv() { return new Database(System.getenv("DATABASE_URL")); }

    public Connection connection() throws SQLException { return DriverManager.getConnection(jdbcUrl, properties); }

    public void ping() {
        try (Connection connection = connection(); Statement statement = connection.createStatement(); ResultSet ignored = statement.executeQuery("SELECT 1")) {
            // query success is sufficient
        } catch (SQLException e) {
            throw new IllegalStateException("ping postgres: " + e.getMessage(), e);
        }
    }

    private static String decode(String value) { return URLDecoder.decode(value, StandardCharsets.UTF_8); }
    @Override public void close() { /* DriverManager connections are method-scoped */ }
}
