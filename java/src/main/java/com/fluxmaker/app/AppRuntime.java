package com.fluxmaker.app;

import com.fluxmaker.config.AppConfig;
import com.fluxmaker.config.ConfigDiff;
import com.fluxmaker.engine.TradingEngine;
import com.fluxmaker.oracle.PancakeV2Oracle;

import java.util.ArrayList;
import java.util.List;

public final class AppRuntime {
    public AppConfig config;
    public final TradingEngine engine;
    public final PancakeV2Oracle oracle;

    public AppRuntime(AppConfig config, TradingEngine engine, PancakeV2Oracle oracle) {
        this.config = config;
        this.engine = engine;
        this.oracle = oracle;
    }

    public void prepare() { engine.prepare(); }
    public int refreshMarketRules() { int changed = engine.refreshMarketRules(); config = engine.effectiveConfig(); return changed; }
    public int retryBlocked() { int recovered = engine.retryBlocked(); config = engine.effectiveConfig(); return recovered; }

    public void applyCleanup(ConfigDiff.Plan plan) {
        if (plan == null) return;
        if (plan.cancelAll) { engine.cancelAll(); return; }
        List<String> failures = new ArrayList<>();
        for (ConfigDiff.MarketTarget target : plan.cancelTargets) {
            try { engine.cancelMarket(target.instrumentId, target.venue); }
            catch (RuntimeException e) { failures.add(target.venue + "/" + target.instrumentId + ": " + e.getMessage()); }
        }
        if (!failures.isEmpty()) throw new IllegalStateException("targeted cleanup failed: " + String.join("; ", failures));
    }
}
