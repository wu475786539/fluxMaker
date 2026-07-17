package com.fluxmaker.infra;

import org.junit.jupiter.api.Test;

import java.util.Properties;

import static org.junit.jupiter.api.Assertions.assertEquals;
import static org.junit.jupiter.api.Assertions.assertThrows;

class DatabaseTimeoutTest {
    @Test
    void appliesBoundedDriverTimeouts() {
        Database database = new Database(
                "postgres://user:password@postgres:5432/fluxmaker?sslmode=disable",
                2, 2, 3, 2, 1);

        Properties properties = database.connectionProperties();
        assertEquals("2", properties.getProperty("connectTimeout"));
        assertEquals("2", properties.getProperty("loginTimeout"));
        assertEquals("3", properties.getProperty("socketTimeout"));
        assertEquals("2", properties.getProperty("queryTimeout"));
        assertEquals("1", properties.getProperty("cancelSignalTimeout"));
        assertEquals("true", properties.getProperty("tcpKeepAlive"));
        assertEquals(2, database.queryTimeoutSeconds());
    }

    @Test
    void rejectsDisabledTimeouts() {
        assertThrows(IllegalArgumentException.class, () -> new Database(
                "postgres://user:password@postgres:5432/fluxmaker",
                0, 2, 3, 2, 1));
    }
}
