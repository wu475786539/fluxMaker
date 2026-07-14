package com.fluxmaker.config;

import com.fluxmaker.domain.Domain;
import com.fluxmaker.json.Json;

import java.util.ArrayList;
import java.util.Comparator;
import java.util.LinkedHashMap;
import java.util.LinkedHashSet;
import java.util.List;
import java.util.Map;
import java.util.Set;

/** Produces the same incremental apply plan as internal/configdiff in the Go backend. */
public final class ConfigDiff {
    private ConfigDiff() {}

    public static final class MarketTarget {
        public String instrumentId = "";
        public String venue = "";
        public String symbol = "";
        public String reason = "";
    }

    public static final class InstrumentChange {
        public String instrumentId = "";
        public String action = "";
        public List<String> reasons = new ArrayList<>();
    }

    public static final class Plan {
        public boolean firstPublish;
        public boolean cancelAll;
        public boolean structural;
        public List<String> hotChanges = new ArrayList<>();
        public List<InstrumentChange> instrumentChanges = new ArrayList<>();
        public List<MarketTarget> cancelTargets = new ArrayList<>();
        public int affectedInstruments;
        public int unchangedInstruments;
    }

    public static Plan build(AppConfig previous, AppConfig next) {
        Plan plan = new Plan();
        plan.firstPublish = previous == null;
        if (previous == null) {
            plan.structural = true;
            for (AppConfig.InstrumentConfig instrument : next.instruments) {
                InstrumentChange change = new InstrumentChange();
                change.instrumentId = instrument.id;
                change.action = "add";
                change.reasons.add("新增币对");
                plan.instrumentChanges.add(change);
            }
            finish(plan, next);
            return plan;
        }

        Map<String, InstrumentChange> changes = new LinkedHashMap<>();
        ChangeAdder add = (instrumentId, action, reason) -> {
            InstrumentChange change = changes.computeIfAbsent(instrumentId, ignored -> {
                InstrumentChange value = new InstrumentChange();
                value.instrumentId = instrumentId;
                value.action = action;
                return value;
            });
            if (priority(action) > priority(change.action)) change.action = action;
            if (reason != null && !reason.isEmpty() && !change.reasons.contains(reason)) change.reasons.add(reason);
        };

        if (previous.pollIntervalMs != next.pollIntervalMs || previous.maxConcurrentInstruments != next.maxConcurrentInstruments || previous.rulesRefreshSeconds != next.rulesRefreshSeconds)
            plan.hotChanges.add("轮询、币对并发与交易规则刷新");
        if (previous.marketFailureThreshold != next.marketFailureThreshold || previous.marketRecoveryThreshold != next.marketRecoveryThreshold || previous.marketErrorGraceSeconds != next.marketErrorGraceSeconds || previous.tradingProgressTimeoutSeconds != next.tradingProgressTimeoutSeconds)
            plan.hotChanges.add("市场故障宽限与恢复阈值");
        if (!same(previous.auditPath, next.auditPath) || previous.auditMaxBytes != next.auditMaxBytes || previous.auditBackups != next.auditBackups) {
            plan.hotChanges.add("审计文件与轮转"); plan.structural = true;
        }
        if (!same(previous.heartbeatPath, next.heartbeatPath) || previous.watchdogTimeoutSeconds != next.watchdogTimeoutSeconds) {
            plan.hotChanges.add("Watchdog 与心跳参数"); plan.structural = true;
        }
        if (!equal(previous.rpc, next.rpc)) {
            plan.hotChanges.add("BNB Chain RPC（验证后热切换）"); plan.structural = true;
            next.instruments.forEach(instrument -> add.add(instrument.id, "hot_reload", "价格源连接发生变化"));
        }
        if (previous.mode != next.mode) {
            plan.hotChanges.add("运行模式"); plan.structural = true;
            next.instruments.forEach(instrument -> add.add(instrument.id, "reconfigure", "运行模式发生变化"));
            if (previous.mode == Domain.Mode.live && next.mode == Domain.Mode.shadow) plan.cancelAll = true;
        }

        Map<String, AppConfig.InstrumentConfig> oldInstruments = instruments(previous);
        Map<String, AppConfig.InstrumentConfig> newInstruments = instruments(next);
        for (Map.Entry<String, AppConfig.InstrumentConfig> entry : oldInstruments.entrySet()) {
            String id = entry.getKey();
            AppConfig.InstrumentConfig current = newInstruments.get(id);
            if (current == null) {
                add.add(id, "remove", "删除币对");
                addInstrumentMarkets(plan, previous, id, "删除币对");
                continue;
            }
            AppConfig.InstrumentConfig old = entry.getValue();
            if (!equal(old.base, current.base) || !equal(old.quote, current.quote)) add.add(id, "reconfigure", "Token 信息发生变化");
            if (!equal(old.reference, current.reference)) add.add(id, "reconfigure", "Pancake 价格路径发生变化");
            if (!equal(old.strategy, current.strategy)) add.add(id, "reconcile", "策略或库存参数发生变化");
            if (!equal(old.tradeSimulation, current.tradeSimulation)) add.add(id, "hot_reload", "内部成交模拟参数发生变化");
        }
        newInstruments.keySet().stream().filter(id -> !oldInstruments.containsKey(id)).forEach(id -> add.add(id, "add", "新增币对"));
        diffVenues(plan, previous, next, add);
        changes.values().forEach(change -> {
            change.reasons.sort(String::compareTo);
            plan.instrumentChanges.add(change);
        });
        finish(plan, next);
        return plan;
    }

    private static void diffVenues(Plan plan, AppConfig oldConfig, AppConfig next, ChangeAdder add) {
        for (Map.Entry<String, AppConfig.VenueConfig> entry : oldConfig.venues.entrySet()) {
            String venueName = entry.getKey();
            AppConfig.VenueConfig oldVenue = entry.getValue(), newVenue = next.venues.get(venueName);
            boolean venueExists = newVenue != null;
            boolean connectionChanged = !venueExists || !same(oldVenue.type, newVenue.type) || !same(oldVenue.environment, newVenue.environment) || !same(oldVenue.baseUrl, newVenue.baseUrl);
            boolean executionChanged = connectionChanged || !venueExists || !same(oldVenue.selfTradePrevention, newVenue.selfTradePrevention) || oldVenue.dedicatedAccount != newVenue.dedicatedAccount;
            boolean stopped = !venueExists || !newVenue.enabled || !newVenue.tradingEnabled;
            for (Map.Entry<String, AppConfig.VenueMarketConfig> marketEntry : oldVenue.markets.entrySet()) {
                String instrumentId = marketEntry.getKey();
                AppConfig.VenueMarketConfig oldMarket = marketEntry.getValue();
                AppConfig.VenueMarketConfig newMarket = venueExists ? newVenue.markets.get(instrumentId) : null;
                boolean marketExists = newMarket != null;
                boolean mustCancel = oldConfig.mode == Domain.Mode.live && oldVenue.enabled && oldVenue.tradingEnabled &&
                        (stopped || !marketExists || connectionChanged || !same(oldMarket.symbol, newMarket.symbol) || oldMarket.credentialId != newMarket.credentialId);
                if (mustCancel) addTarget(plan, target(instrumentId, venueName, oldMarket.symbol, cancelReason(venueExists, newVenue, marketExists, connectionChanged, oldMarket, newMarket)));
                if (!venueExists || !marketExists) continue;
                if (executionChanged || oldVenue.enabled != newVenue.enabled || oldVenue.tradingEnabled != newVenue.tradingEnabled || !equal(oldMarket, newMarket))
                    add.add(instrumentId, "reconfigure", "交易市场 " + venueName + " 发生变化");
            }
        }
        for (Map.Entry<String, AppConfig.VenueConfig> entry : next.venues.entrySet()) {
            AppConfig.VenueConfig oldVenue = oldConfig.venues.get(entry.getKey());
            for (String instrumentId : entry.getValue().markets.keySet()) {
                if (oldVenue == null) add.add(instrumentId, "reconfigure", "新增交易所 " + entry.getKey());
                else if (!oldVenue.markets.containsKey(instrumentId)) add.add(instrumentId, "reconfigure", "新增交易市场 " + entry.getKey());
            }
        }
    }

    private static String cancelReason(boolean venueExists, AppConfig.VenueConfig venue, boolean marketExists, boolean connectionChanged, AppConfig.VenueMarketConfig oldMarket, AppConfig.VenueMarketConfig next) {
        if (!venueExists || !venue.enabled) return "交易所被删除或停用";
        if (!venue.tradingEnabled) return "交易所关闭实盘";
        if (!marketExists) return "交易市场被删除";
        if (connectionChanged) return "交易所连接发生变化";
        if (!same(oldMarket.symbol, next.symbol)) return "交易所 Symbol 发生变化";
        if (oldMarket.credentialId != next.credentialId) return "交易凭证发生变化";
        return "交易市场需要重建";
    }

    private static void addInstrumentMarkets(Plan plan, AppConfig config, String instrumentId, String reason) {
        if (config.mode != Domain.Mode.live) return;
        config.venues.forEach((name, venue) -> {
            AppConfig.VenueMarketConfig market = venue.markets.get(instrumentId);
            if (venue.enabled && venue.tradingEnabled && market != null) addTarget(plan, target(instrumentId, name, market.symbol, reason));
        });
    }

    private static MarketTarget target(String instrument, String venue, String symbol, String reason) {
        MarketTarget value = new MarketTarget(); value.instrumentId = instrument; value.venue = venue; value.symbol = symbol; value.reason = reason; return value;
    }

    private static void addTarget(Plan plan, MarketTarget target) {
        boolean exists = plan.cancelTargets.stream().anyMatch(item -> same(item.instrumentId, target.instrumentId) && same(item.venue, target.venue) && same(item.symbol, target.symbol));
        if (!exists) plan.cancelTargets.add(target);
    }

    private static void finish(Plan plan, AppConfig next) {
        if (plan.cancelAll || !plan.cancelTargets.isEmpty()) plan.structural = true;
        if (plan.instrumentChanges.stream().anyMatch(change -> Set.of("reconfigure", "add", "remove").contains(change.action))) plan.structural = true;
        plan.hotChanges.sort(String::compareTo);
        plan.instrumentChanges.sort(Comparator.comparing(change -> change.instrumentId));
        plan.cancelTargets.sort(Comparator.comparing((MarketTarget value) -> value.instrumentId).thenComparing(value -> value.venue));
        plan.affectedInstruments = plan.instrumentChanges.size();
        Set<String> affected = new LinkedHashSet<>();
        plan.instrumentChanges.forEach(change -> affected.add(change.instrumentId));
        plan.unchangedInstruments = (int) next.instruments.stream().filter(instrument -> !affected.contains(instrument.id)).count();
    }

    private static Map<String, AppConfig.InstrumentConfig> instruments(AppConfig config) {
        Map<String, AppConfig.InstrumentConfig> values = new LinkedHashMap<>(); config.instruments.forEach(value -> values.put(value.id, value)); return values;
    }
    private static boolean equal(Object left, Object right) { return Json.tree(left).equals(Json.tree(right)); }
    private static boolean same(String left, String right) { return left == null ? right == null : left.equals(right); }
    private static int priority(String action) { return Map.of("hot_reload", 1, "reconcile", 2, "reconfigure", 3, "add", 4, "remove", 5).getOrDefault(action, 0); }
    @FunctionalInterface private interface ChangeAdder { void add(String instrumentId, String action, String reason); }
}
