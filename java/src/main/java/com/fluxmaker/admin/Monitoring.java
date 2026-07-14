package com.fluxmaker.admin;

import com.fluxmaker.config.AppConfig;
import com.fluxmaker.domain.Domain;
import com.fluxmaker.runtime.RuntimeStore;

import java.time.Duration;
import java.time.Instant;
import java.util.ArrayList;
import java.util.Comparator;
import java.util.List;

public final class Monitoring {
    private Monitoring() {}

    public static final class Alert {
        public String severity, code, message, instrumentId, venue;
        public Instant since;
    }
    public static final class Summary {
        public String status = "healthy";
        public Instant generatedAt;
        public int critical, warnings;
        public List<Alert> alerts = new ArrayList<>();
    }

    public static Summary build(AppConfig config, RuntimeStore.EngineStatus engine, List<RuntimeStore.InstrumentSnapshot> instruments, Instant now) {
        Summary result = new Summary(); result.generatedAt = now;
        if (!engine.online) add(result, "critical", "engine_offline", "交易引擎心跳已离线", null, null, engine.lastHeartbeat);
        else if (!engine.ready) add(result, "warning", "engine_not_ready", "交易引擎在线但运行配置未就绪", null, null, engine.lastHeartbeat);
        if (engine.error != null && !engine.error.isEmpty()) add(result, "warning", "engine_error", engine.error, null, null, engine.lastHeartbeat);
        int progressTimeout = config == null || config.tradingProgressTimeoutSeconds <= 0 ? 120 : config.tradingProgressTimeoutSeconds;
        boolean progressStale = engine.lastTradingProgress != null && Duration.between(engine.lastTradingProgress, now).compareTo(Duration.ofSeconds(progressTimeout)) > 0;
        if (engine.lastTradingProgress == null && engine.metrics != null && engine.metrics.startedAt != null) progressStale = Duration.between(engine.metrics.startedAt, now).compareTo(Duration.ofSeconds(progressTimeout)) > 0;
        if (engine.online && engine.ready && progressStale) add(result, "critical", "trading_progress_stale", "交易循环长时间没有完成进度", null, null, engine.lastTradingProgress);
        if (engine.performance != null) {
            if (engine.performance.failed > 0) add(result, "warning", "cycle_failures", "最近一轮有 " + engine.performance.failed + "/" + engine.performance.instruments + " 个币对失败", null, null, engine.performance.startedAt);
            if (config != null && config.pollIntervalMs > 0 && engine.performance.durationMs > config.pollIntervalMs) add(result, "warning", "cycle_slow", "最近一轮耗时 " + engine.performance.durationMs + "ms，超过轮询间隔 " + config.pollIntervalMs + "ms", null, null, engine.performance.startedAt);
        }
        if (engine.metrics != null && engine.metrics.auditPendingEvents > 0) add(result, "critical", "audit_write_pending", "审计写入异常 " + engine.metrics.auditFlushErrorsTotal + " 次，待写事件 " + engine.metrics.auditPendingEvents + " 条", null, null, engine.metrics.updatedAt);
        for (RuntimeStore.RuleChange change : engine.ruleChanges) if (change.detectedAt != null && Duration.between(change.detectedAt, now).compareTo(Duration.ofHours(1)) <= 0)
            add(result, "warning", "trading_rules_changed", "交易所交易规则发生变化，已按新规则热更新", change.instrumentId, change.venue, change.detectedAt);
        if (config != null && config.mode == Domain.Mode.live) {
            if (engine.watchdog == null && engine.online) add(result, "warning", "watchdog_unknown", "尚未收到 Watchdog 检查状态", null, null, null);
            else if (engine.watchdog != null) {
                int timeout = config.watchdogTimeoutSeconds <= 0 ? 15 : config.watchdogTimeoutSeconds;
                if (engine.watchdog.lastCheckAt == null || Duration.between(engine.watchdog.lastCheckAt, now).compareTo(Duration.ofSeconds(timeout * 2L)) > 0)
                    add(result, "critical", "watchdog_offline", "Watchdog 长时间没有更新检查状态", null, null, engine.watchdog.lastCheckAt);
                else if (!engine.watchdog.healthy) {
                    String message = "Watchdog 已触发保护撤单：" + text(engine.watchdog.reason);
                    if (engine.watchdog.cancelError != null && !engine.watchdog.cancelError.isEmpty()) message += "；撤单错误：" + engine.watchdog.cancelError;
                    add(result, "critical", "watchdog_triggered", message, null, null, engine.watchdog.lastTriggeredAt);
                }
            }
        }
        if (engine.online) for (RuntimeStore.InstrumentSnapshot instrument : instruments) {
            if ("waiting".equals(instrument.status)) add(result, "warning", "instrument_waiting", "币对尚未产生运行快照", instrument.instrumentId, null, null);
            if ("degraded".equals(instrument.status)) add(result, "warning", "instrument_degraded", "币对运行降级", instrument.instrumentId, null, instrument.updatedAt);
            for (RuntimeStore.VenueSnapshot venue : instrument.venues) {
                if (!venue.marketConnected) add(result, "warning", "market_disconnected", "交易所行情连接异常", instrument.instrumentId, venue.name, venue.updatedAt);
                if (config != null && config.mode == Domain.Mode.live && venue.tradingEnabled && !venue.accountConnected) add(result, "critical", "account_disconnected", "实盘账户连接异常", instrument.instrumentId, venue.name, venue.updatedAt);
                String status = venue.fault == null ? "normal" : venue.fault.path("status").asText("normal");
                if (!"normal".equals(status)) add(result, ("canceling".equals(status) || "paused".equals(status)) ? "critical" : "warning", "venue_fault_" + status, "交易市场故障状态：" + status, instrument.instrumentId, venue.name, venue.fault.hasNonNull("since") ? Instant.parse(venue.fault.get("since").asText()) : venue.updatedAt);
            }
        }
        result.alerts.sort(Comparator.comparingInt((Alert alert) -> rank(alert.severity)).reversed());
        result.alerts.forEach(alert -> { if ("critical".equals(alert.severity)) result.critical++; else if ("warning".equals(alert.severity)) result.warnings++; });
        result.status = result.critical > 0 ? "critical" : result.warnings > 0 ? "degraded" : "healthy";
        return result;
    }

    public static String prometheus(RuntimeStore.EngineStatus engine, List<RuntimeStore.InstrumentSnapshot> instruments, Summary summary, Instant now) {
        StringBuilder out = new StringBuilder();
        metric(out, "fluxmaker_engine_up", "Whether the trading engine heartbeat is online.", "gauge", bool(engine.online));
        metric(out, "fluxmaker_engine_ready", "Whether the trading engine has an applied runtime.", "gauge", bool(engine.ready));
        metric(out, "fluxmaker_engine_version", "Currently applied configuration version.", "gauge", engine.version);
        metric(out, "fluxmaker_engine_heartbeat_age_seconds", "Age of the latest engine heartbeat.", "gauge", age(now, engine.lastHeartbeat));
        metric(out, "fluxmaker_trading_progress_age_seconds", "Age of the latest completed instrument progress.", "gauge", age(now, engine.lastTradingProgress));
        if (engine.performance != null) { metric(out, "fluxmaker_cycle_duration_seconds", "Duration of the latest trading cycle.", "gauge", engine.performance.durationMs / 1000.0); metric(out, "fluxmaker_cycle_failed_instruments", "Failed instruments in the latest trading cycle.", "gauge", engine.performance.failed); }
        if (engine.metrics != null) {
            metric(out,"fluxmaker_cycles_total","Trading cycles since engine start.","counter",engine.metrics.cyclesTotal); metric(out,"fluxmaker_cycle_failures_total","Trading cycles with at least one failure.","counter",engine.metrics.cycleFailuresTotal);
            metric(out,"fluxmaker_instrument_runs_total","Instrument executions since engine start.","counter",engine.metrics.instrumentRunsTotal); metric(out,"fluxmaker_instrument_failures_total","Failed instrument executions since engine start.","counter",engine.metrics.instrumentFailuresTotal);
            metric(out,"fluxmaker_venue_fault_events_total","Venue fault observations since engine start.","counter",engine.metrics.venueFaultEventsTotal); metric(out,"fluxmaker_oms_placed_orders_total","Orders accepted by OMS since engine start.","counter",engine.metrics.omsPlacedTotal);
            metric(out,"fluxmaker_oms_canceled_orders_total","Orders submitted for cancellation by OMS since engine start.","counter",engine.metrics.omsCanceledTotal); metric(out,"fluxmaker_simulated_trades_total","Internal simulated trade events since engine start.","counter",engine.metrics.simulatedTradesTotal);
            metric(out,"fluxmaker_audit_flush_errors_total","Audit flush failures since engine start.","counter",engine.metrics.auditFlushErrorsTotal); metric(out,"fluxmaker_audit_pending_events","Audit events waiting for durable flush.","gauge",engine.metrics.auditPendingEvents);
            metric(out,"fluxmaker_rule_changes_total","Trading rule changes detected since engine start.","counter",engine.metrics.ruleChangesTotal); metric(out,"fluxmaker_lease_fence_rejections_total","Exchange writes rejected because the market lease was stale or unverifiable.","counter",engine.metrics.leaseFenceRejectsTotal);
        }
        metric(out,"fluxmaker_recent_rule_changes","Trading rule changes retained in the runtime alert window.","gauge",engine.ruleChanges.size()); metric(out,"fluxmaker_monitoring_critical_alerts","Current critical monitoring alerts.","gauge",summary.critical); metric(out,"fluxmaker_monitoring_warning_alerts","Current warning monitoring alerts.","gauge",summary.warnings);
        for (RuntimeStore.InstrumentSnapshot instrument : instruments) {
            String labels = "instrument=\"" + escape(instrument.instrumentId) + "\"";
            sample(out,"fluxmaker_instrument_running",labels,bool("running".equals(instrument.status))); sample(out,"fluxmaker_instrument_tick_duration_seconds",labels,instrument.tickDurationMs/1000.0); sample(out,"fluxmaker_instrument_reference_duration_seconds",labels,instrument.referenceDurationMs/1000.0); sample(out,"fluxmaker_instrument_balance_duration_seconds",labels,instrument.balanceDurationMs/1000.0);
            for (RuntimeStore.VenueSnapshot venue : instrument.venues) { String both=labels+",venue=\""+escape(venue.name)+"\""; sample(out,"fluxmaker_venue_market_connected",both,bool(venue.marketConnected)); sample(out,"fluxmaker_venue_account_connected",both,bool(venue.accountConnected)); sample(out,"fluxmaker_venue_open_orders",both,venue.openOrders.size()); sample(out,"fluxmaker_venue_pending_orders",both,venue.pendingOrders); }
        }
        return out.toString();
    }

    private static void add(Summary summary, String severity, String code, String message, String instrument, String venue, Instant since) { Alert value = new Alert(); value.severity=severity; value.code=code; value.message=message; value.instrumentId=instrument; value.venue=venue; value.since=since; summary.alerts.add(value); }
    private static int rank(String value) { return "critical".equals(value) ? 2 : "warning".equals(value) ? 1 : 0; }
    private static void metric(StringBuilder out,String name,String help,String type,Object value){ out.append("# HELP ").append(name).append(' ').append(help).append("\n# TYPE ").append(name).append(' ').append(type).append('\n'); sample(out,name,"",value); }
    private static void sample(StringBuilder out,String name,String labels,Object value){ out.append(name); if(!labels.isEmpty()) out.append('{').append(labels).append('}'); out.append(' ').append(value).append('\n'); }
    private static int bool(boolean value){return value?1:0;} private static double age(Instant now,Instant value){return value==null?-1:Math.max(0,Duration.between(value,now).toMillis()/1000.0);} private static String escape(String value){return text(value).replace("\\","\\\\").replace("\n","\\n").replace("\"","\\\"");} private static String text(String value){return value==null?"":value;}
}
