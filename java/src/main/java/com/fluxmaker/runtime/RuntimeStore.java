package com.fluxmaker.runtime;

import com.fasterxml.jackson.databind.JsonNode;
import com.fluxmaker.domain.Domain;
import com.fluxmaker.infra.RedisClient;
import com.fluxmaker.json.Json;
import com.fluxmaker.math.DecimalValue;

import java.nio.charset.StandardCharsets;
import java.time.Duration;
import java.time.Instant;
import java.time.format.DateTimeParseException;
import java.util.ArrayList;
import java.util.LinkedHashMap;
import java.util.List;
import java.util.Map;

public final class RuntimeStore {
    public static final String SNAPSHOT_PREFIX = "fluxmaker:runtime:instrument:";
    public static final String HEARTBEAT_KEY = "fluxmaker:runtime:engine";
    public static final String APPLIED_VERSION_KEY = "fluxmaker:runtime:applied-version";
    public static final String PAUSED_KEY = "fluxmaker:control:paused";
    public static final String RECONCILE_KEY = "fluxmaker:control:reconcile";
    public static final String TRADING_PROGRESS_KEY = "fluxmaker:runtime:trading-progress";
    public static final String CYCLE_PERFORMANCE_KEY = "fluxmaker:runtime:cycle-performance";
    public static final String METRICS_KEY = "fluxmaker:runtime:metrics";
    public static final String WATCHDOG_KEY = "fluxmaker:runtime:watchdog";
    public static final String RULE_CHANGES_KEY = "fluxmaker:runtime:rule-changes";
    public static final String OMS_STATE_PREFIX = "fluxmaker:oms:state:";
    public static final String FAULT_STATE_PREFIX = "fluxmaker:fault:state:";
    public static final String MARKET_LEASE_PREFIX = "fluxmaker:lease:market:";
    public static final String MARKET_FENCE_PREFIX = "fluxmaker:lease:generation:";
    public static final String SIMULATION_PREFIX = "fluxmaker:simulation:fills:";
    public static final String REASON_MANUAL_PAUSE = "manual_pause";
    public static final String REASON_EMERGENCY_CANCEL = "emergency_cancel";
    public static final Duration SNAPSHOT_TTL = Duration.ofSeconds(45);
    public static final Duration HEARTBEAT_TTL = Duration.ofSeconds(15);

    private final RedisClient redis;
    private Instant lastProgress;

    public RuntimeStore(RedisClient redis) { this.redis = redis; }

    public static final class EngineStatus {
        public boolean online;
        public boolean ready;
        public long version;
        public long desiredVersion;
        public String error;
        public Instant lastHeartbeat;
        public Instant lastTradingProgress;
        public CyclePerformance performance;
        public MetricsSnapshot metrics;
        public WatchdogStatus watchdog;
        public List<RuleChange> ruleChanges = new ArrayList<>();
    }

    public static final class CyclePerformance {
        public Instant startedAt;
        public long durationMs;
        public int instruments;
        public int succeeded;
        public int failed;
        public int concurrentLimit;
    }

    public static final class MetricsSnapshot {
        public Instant startedAt;
        public Instant updatedAt;
        public long cyclesTotal;
        public long cycleFailuresTotal;
        public long instrumentRunsTotal;
        public long instrumentFailuresTotal;
        public long venueFaultEventsTotal;
        public long omsPlacedTotal;
        public long omsCanceledTotal;
        public long simulatedTradesTotal;
        public long auditFlushErrorsTotal;
        public int auditPendingEvents;
        public long ruleChangesTotal;
        public long leaseFenceRejectsTotal;
    }

    public static final class WatchdogStatus {
        public boolean healthy;
        public Instant lastCheckAt;
        public Instant lastTriggeredAt;
        public String reason;
        public String cancelError;
    }

    public static final class RuleChange {
        public String instrumentId;
        public String venue;
        public String symbol;
        public Domain.MarketRules previous;
        public Domain.MarketRules current;
        public Instant detectedAt;
    }

    public static final class PauseState {
        public String instrumentId;
        public boolean paused;
        public String reason;
        public long requestedBy;
        public Instant requestedAt;
    }

    public static final class ReconcileRequest {
        public String instrumentId;
        public long requestedBy;
        public Instant requestedAt;
    }

    public static final class VenueSnapshot {
        public String name;
        public String type;
        public String symbol;
        public boolean tradingEnabled;
        public boolean marketConnected;
        public boolean accountConnected;
        public Domain.Book book;
        public Domain.Balance baseBalance;
        public Domain.Balance quoteBalance;
        public Domain.QuoteBudget budget;
        public Domain.MarketRules rules;
        public JsonNode fault;
        public List<Domain.Order> openOrders = new ArrayList<>();
        public int pendingOrders;
        public List<Domain.Fill> fills = new ArrayList<>();
        public String error;
        public Instant updatedAt;
        public long bookDurationMs;
        public long ordersDurationMs;
        public long fillsDurationMs;
        public long omsDurationMs;
    }

    public static final class InstrumentSnapshot {
        public String instrumentId;
        public String baseSymbol;
        public String quoteSymbol;
        public Domain.Mode mode;
        public String status;
        public boolean paused;
        public PauseState pause;
        public Domain.ReferencePrice reference;
        public DecimalValue inventory = DecimalValue.ZERO;
        public boolean inventoryAvailable;
        public DecimalValue targetInventory = DecimalValue.ZERO;
        public DecimalValue maxBaseDeviation = DecimalValue.ZERO;
        public List<VenueSnapshot> venues = new ArrayList<>();
        public JsonNode tradeSimulation;
        public String error;
        public Instant updatedAt;
        public long tickDurationMs;
        public long referenceDurationMs;
        public long balanceDurationMs;
    }

    public byte[] loadOmsState(String key) { return redis.get(OMS_STATE_PREFIX + key); }
    public void saveOmsState(String key, byte[] payload) { redis.set(OMS_STATE_PREFIX + key, payload, Duration.ofDays(7)); }
    public void deleteOmsState(String key) { redis.delete(OMS_STATE_PREFIX + key); }
    public byte[] loadFaultState(String key) { return redis.get(FAULT_STATE_PREFIX + key); }
    public void saveFaultState(String key, byte[] payload) { redis.set(FAULT_STATE_PREFIX + key, payload, Duration.ZERO); }
    public void deleteFaultState(String key) { redis.delete(FAULT_STATE_PREFIX + key); }

    public long acquireMarketLease(String key, String owner, Duration ttl) {
        if (ttl == null || ttl.isZero() || ttl.isNegative()) ttl = Duration.ofSeconds(30);
        String script = """
                local current = redis.call('GET', KEYS[1])
                if current then
                  local currentOwner, currentGeneration = string.match(current, '^(.*):(%d+)$')
                  if currentOwner == ARGV[1] then
                    redis.call('PEXPIRE', KEYS[1], ARGV[2])
                    return tonumber(currentGeneration)
                  end
                  return 0
                end
                local generation = redis.call('INCR', KEYS[2])
                redis.call('SET', KEYS[1], ARGV[1] .. ':' .. generation, 'PX', ARGV[2])
                return generation
                """;
        return redis.evalLong(script, List.of(MARKET_LEASE_PREFIX + key, MARKET_FENCE_PREFIX + key), owner, Long.toString(ttl.toMillis()));
    }

    public boolean validateMarketLease(String key, String owner, long generation) {
        String script = "if redis.call('GET', KEYS[1]) == ARGV[1] then return 1 end return 0";
        return redis.evalLong(script, List.of(MARKET_LEASE_PREFIX + key), owner + ":" + generation) == 1;
    }

    public void releaseMarketLease(String key, String owner, long generation) {
        String script = "if redis.call('GET', KEYS[1]) == ARGV[1] then return redis.call('DEL', KEYS[1]) end return 0";
        redis.evalLong(script, List.of(MARKET_LEASE_PREFIX + key), owner + ":" + generation);
    }

    public void publish(InstrumentSnapshot snapshot) { redis.set(SNAPSHOT_PREFIX + snapshot.instrumentId, Json.writeBytes(snapshot), SNAPSHOT_TTL); }
    public void appendSimulatedFill(String instrumentId, Domain.Fill fill) { redis.xaddPayload(SIMULATION_PREFIX + instrumentId, 1000, Json.writeBytes(fill)); }

    public InstrumentSnapshot get(String instrumentId) {
        byte[] payload = redis.get(SNAPSHOT_PREFIX + instrumentId);
        if (payload == null) return null;
        return Json.read(payload, InstrumentSnapshot.class);
    }

    public void heartbeat(long version, long desiredVersion, boolean ready, String errorText) {
        EngineStatus status = new EngineStatus(); status.online = true; status.ready = ready; status.version = version; status.desiredVersion = desiredVersion; status.error = blankToNull(errorText); status.lastHeartbeat = Instant.now();
        redis.set(HEARTBEAT_KEY, Json.writeBytes(status), HEARTBEAT_TTL);
    }

    public EngineStatus engineStatus() {
        byte[] payload = redis.get(HEARTBEAT_KEY);
        EngineStatus status;
        try { status = payload == null ? new EngineStatus() : Json.read(payload, EngineStatus.class); }
        catch (RuntimeException e) { status = new EngineStatus(); }
        status.online = status.lastHeartbeat != null && Duration.between(status.lastHeartbeat, Instant.now()).compareTo(HEARTBEAT_TTL) < 0;
        status.lastTradingProgress = tradingProgress(); status.performance = cyclePerformance(); status.metrics = metrics(); status.watchdog = watchdogStatus(); status.ruleChanges = ruleChanges(50);
        return status;
    }

    public synchronized void reportTradingProgress() {
        Instant now = Instant.now();
        if (lastProgress != null && Duration.between(lastProgress, now).compareTo(Duration.ofSeconds(1)) < 0) return;
        lastProgress = now; redis.set(TRADING_PROGRESS_KEY, now.toString(), Duration.ZERO);
    }

    public void reportCyclePerformance(CyclePerformance performance) { redis.set(CYCLE_PERFORMANCE_KEY, Json.writeBytes(performance), Duration.ofMinutes(5)); }
    public CyclePerformance cyclePerformance() { return decode(CYCLE_PERFORMANCE_KEY, CyclePerformance.class); }
    public void reportMetrics(MetricsSnapshot metrics) { redis.set(METRICS_KEY, Json.writeBytes(metrics), Duration.ofMinutes(5)); }
    public MetricsSnapshot metrics() { return decode(METRICS_KEY, MetricsSnapshot.class); }

    public void reportWatchdog(boolean healthy, boolean actionTriggered, String reason, String cancelError) {
        WatchdogStatus previous = watchdogStatus();
        WatchdogStatus status = new WatchdogStatus(); status.healthy = healthy; status.lastCheckAt = Instant.now();
        if (previous != null) { status.lastTriggeredAt = previous.lastTriggeredAt; status.reason = previous.reason; status.cancelError = previous.cancelError; }
        if (!healthy) { status.reason = reason; status.cancelError = cancelError; }
        if (actionTriggered) status.lastTriggeredAt = status.lastCheckAt;
        redis.set(WATCHDOG_KEY, Json.writeBytes(status), Duration.ofMinutes(5));
    }

    public WatchdogStatus watchdogStatus() { return decode(WATCHDOG_KEY, WatchdogStatus.class); }

    public void reportRuleChange(RuleChange change) {
        redis.lpush(RULE_CHANGES_KEY, Json.writeBytes(change)); redis.ltrim(RULE_CHANGES_KEY, 0, 99); redis.expire(RULE_CHANGES_KEY, Duration.ofHours(24));
    }

    public List<RuleChange> ruleChanges(long limit) {
        if (limit <= 0 || limit > 100) limit = 50;
        List<RuleChange> result = new ArrayList<>();
        for (byte[] value : redis.lrange(RULE_CHANGES_KEY, 0, limit - 1)) {
            try { result.add(Json.read(value, RuleChange.class)); } catch (RuntimeException ignored) {}
        }
        return result;
    }

    public Instant tradingProgress() {
        String value = redis.getString(TRADING_PROGRESS_KEY);
        try { return value == null ? null : Instant.parse(value); } catch (DateTimeParseException e) { return null; }
    }

    public void setAppliedVersion(long version) { if (version > 0) redis.set(APPLIED_VERSION_KEY, Long.toString(version), Duration.ZERO); }
    public long appliedVersion() { try { String value = redis.getString(APPLIED_VERSION_KEY); return value == null ? 0 : Long.parseLong(value); } catch (NumberFormatException e) { return 0; } }

    public PauseState setPaused(String instrumentId, String reason, long userId) {
        if (instrumentId == null || instrumentId.isEmpty()) throw new IllegalArgumentException("instrument id is required");
        PauseState state = new PauseState(); state.instrumentId = instrumentId; state.paused = true; state.reason = reason; state.requestedBy = userId; state.requestedAt = Instant.now();
        redis.hset(PAUSED_KEY, instrumentId, Json.writeBytes(state)); return state;
    }

    public void resume(String instrumentId) { redis.hdel(PAUSED_KEY, instrumentId); }

    public ReconcileRequest requestReconcile(String instrumentId, long userId) {
        ReconcileRequest request = new ReconcileRequest(); request.instrumentId = instrumentId; request.requestedBy = userId; request.requestedAt = Instant.now();
        redis.hset(RECONCILE_KEY, instrumentId, Json.writeBytes(request)); return request;
    }

    public Map<String, ReconcileRequest> reconciles() { return decodeHash(RECONCILE_KEY, ReconcileRequest.class); }
    public void clearReconcile(String instrumentId) { redis.hdel(RECONCILE_KEY, instrumentId); }
    public Map<String, PauseState> paused() { return decodeHash(PAUSED_KEY, PauseState.class); }

    private <T> T decode(String key, Class<T> type) {
        byte[] payload = redis.get(key); if (payload == null) return null;
        try { return Json.read(payload, type); } catch (RuntimeException ignored) { return null; }
    }

    private <T> Map<String, T> decodeHash(String key, Class<T> type) {
        Map<String, T> result = new LinkedHashMap<>();
        for (Map.Entry<String, byte[]> entry : redis.hgetall(key).entrySet()) {
            try { result.put(entry.getKey(), Json.read(entry.getValue(), type)); } catch (RuntimeException ignored) {}
        }
        return result;
    }

    private static String blankToNull(String value) { return value == null || value.isEmpty() ? null : value; }
}
