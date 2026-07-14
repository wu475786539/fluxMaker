package com.fluxmaker.config;

import com.fluxmaker.domain.Domain;
import com.fluxmaker.json.Json;
import org.junit.jupiter.api.Test;

import static org.junit.jupiter.api.Assertions.assertEquals;
import static org.junit.jupiter.api.Assertions.assertFalse;
import static org.junit.jupiter.api.Assertions.assertTrue;

class ConfigDiffTest {
    @Test
    void strategyOnlyChangeUsesHotPath() {
        AppConfig previous = copy(AppConfigTest.minimal());
        AppConfig next = copy(previous);
        next.instruments.getFirst().strategy.halfSpreadBps++;

        ConfigDiff.Plan plan = ConfigDiff.build(previous, next);

        assertFalse(plan.structural);
        assertEquals("reconcile", plan.instrumentChanges.getFirst().action);
        assertTrue(plan.cancelTargets.isEmpty());
    }

    @Test
    void liveSymbolChangeTargetsOnlyChangedMarket() {
        AppConfig previous = copy(AppConfigTest.minimal()); previous.mode = Domain.Mode.live;
        previous.venues.get("binance").tradingEnabled = true;
        AppConfig next = copy(previous); next.venues.get("binance").markets.get("TEST-USDT").symbol = "TESTUSDC";

        ConfigDiff.Plan plan = ConfigDiff.build(previous, next);

        assertTrue(plan.structural);
        assertEquals(1, plan.cancelTargets.size());
        assertEquals("TESTUSDT", plan.cancelTargets.getFirst().symbol);
    }

    @Test
    void liveToShadowCancelsEverything() {
        AppConfig previous = copy(AppConfigTest.minimal()); previous.mode = Domain.Mode.live;
        AppConfig next = copy(previous); next.mode = Domain.Mode.shadow;
        ConfigDiff.Plan plan = ConfigDiff.build(previous, next);
        assertTrue(plan.cancelAll);
        assertTrue(plan.structural);
    }

    private static AppConfig copy(AppConfig value) { return Json.read(Json.writeBytes(value), AppConfig.class); }
}
