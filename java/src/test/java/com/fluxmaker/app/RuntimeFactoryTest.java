package com.fluxmaker.app;

import com.fluxmaker.config.AppConfig;
import com.fluxmaker.json.Json;
import com.fluxmaker.venue.VenueClient;
import org.junit.jupiter.api.Test;

import static org.junit.jupiter.api.Assertions.assertEquals;
import static org.junit.jupiter.api.Assertions.assertTrue;

class RuntimeFactoryTest {
    @Test
    void buildsPerInstrumentClientWithGoCompatibleIdentity() {
        AppConfig config = Json.read("""
                {"mode":"shadow","rpc":{"request_timeout_ms":1000},"venues":{"Primary":{"type":"binance","enabled":true,"base_url":"https://api.binance.com","markets":{"TOKEN-USDT":{"symbol":"TOKENUSDT","credential_id":0}}}}}
                """, AppConfig.class);

        RuntimeFactory.VenueBuild build = RuntimeFactory.buildVenuesIsolated(config, null);

        assertTrue(build.failures().isEmpty());
        VenueClient client = build.clients().get("primary|token-usdt");
        assertEquals("Primary/TOKEN-USDT", client.name());
        assertEquals("primary/token-usdt/credential-0", client.stateIdentity());
    }
}
