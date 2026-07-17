package com.fluxmaker.fault;

import com.fluxmaker.json.Json;
import com.fluxmaker.runtime.RuntimeStore;

import java.time.Instant;
import java.util.HashMap;
import java.util.Map;

public final class FaultManager {
    public static final String NORMAL = "normal", DEGRADED = "degraded", CANCELING = "canceling", PAUSED = "paused", RECOVERING = "recovering";
    private final int failureThreshold, recoveryThreshold;
    private final RuntimeStore store;
    private final Map<String, Snapshot> states = new HashMap<>();
    private final Map<String, Boolean> loaded = new HashMap<>(), persisted = new HashMap<>();

    public static final class Snapshot {
        public String status;
        public String stage;
        public String error;
        public int consecutiveFailures;
        public int consecutiveSuccesses;
        public Instant since;
        public Instant updatedAt;
        public Instant lastHealthyAt;
        public boolean ordersRetained;
        /** Persisted discriminator so legacy transient CANCELING states do not
         *  continue deleting orders after the retain-on-timeout upgrade. */
        public boolean hardCancel;
    }
    public record Decision(Snapshot state, boolean allowQuotes, boolean shouldCancel) {}

    public FaultManager(int failureThreshold, int recoveryThreshold, RuntimeStore store) {
        this.failureThreshold = failureThreshold < 1 ? 3 : failureThreshold; this.recoveryThreshold = recoveryThreshold < 1 ? 3 : recoveryThreshold; this.store = store;
    }

    public synchronized Decision failure(String key, String stage, Throwable cause, boolean forceCancel) {
        ensureLoaded(key); Instant now = Instant.now(); Snapshot state = state(key, now); state.consecutiveFailures++; state.consecutiveSuccesses = 0; state.stage = stage; if (cause != null) state.error = cause.getMessage(); state.updatedAt = now;
        // A transient venue failure must stop new writes without destroying the
        // resting book. Only an explicitly classified hard failure may enter the
        // canceling state. This prevents a few fast request timeouts from turning
        // into a full-book cancel merely because the polling threshold was reached.
        boolean shouldCancel = forceCancel || (CANCELING.equals(state.status) && state.hardCancel);
        if (shouldCancel) {
            if (!CANCELING.equals(state.status)) state.since = now;
            state.status = CANCELING;
            state.ordersRetained = false;
            state.hardCancel = true;
        } else if (state.consecutiveFailures >= failureThreshold) {
            if (!PAUSED.equals(state.status)) state.since = now;
            state.status = PAUSED;
            state.ordersRetained = true;
            state.hardCancel = false;
        } else {
            if (!DEGRADED.equals(state.status)) state.since = now;
            state.status = DEGRADED;
            state.ordersRetained = true;
            state.hardCancel = false;
        }
        states.put(key, state); persist(key, state); return new Decision(state, false, shouldCancel);
    }

    public synchronized Decision healthy(String key, int managedOpenOrders) {
        ensureLoaded(key); Instant now = Instant.now(); Snapshot state = state(key, now); state.updatedAt = now; state.consecutiveFailures = 0; state.error = null; state.stage = null; boolean allowQuotes = false, shouldCancel = false;
        switch (state.status) {
            case NORMAL -> { state.lastHealthyAt = now; state.ordersRetained = true; allowQuotes = true; }
            case CANCELING -> {
                if (!state.hardCancel) {
                    // State written by the old timeout policy: recover without
                    // issuing another cancel against newly restored orders.
                    state.status = RECOVERING; state.since = now; state.consecutiveSuccesses = 1; state.ordersRetained = true;
                } else if (managedOpenOrders > 0) shouldCancel = true;
                else { state.status = PAUSED; state.since = now; state.ordersRetained = false; state.hardCancel = false; }
            }
            case PAUSED, DEGRADED -> { state.status = RECOVERING; state.since = now; state.consecutiveSuccesses = 1; }
            case RECOVERING -> state.consecutiveSuccesses++;
            default -> { state.status = DEGRADED; state.since = now; }
        }
        if (RECOVERING.equals(state.status) && state.consecutiveSuccesses >= recoveryThreshold) { state.status = NORMAL; state.since = now; state.lastHealthyAt = now; state.consecutiveSuccesses = 0; state.ordersRetained = true; state.hardCancel = false; allowQuotes = true; }
        states.put(key, state); persist(key, state); return new Decision(state, allowQuotes, shouldCancel);
    }

    public synchronized Snapshot snapshot(String key) { return state(key, Instant.now()); }
    public synchronized void reset(String key) { states.remove(key); loaded.put(key, true); persisted.remove(key); if (store != null) store.deleteFaultState(key); }

    private Snapshot state(String key, Instant now) {
        return states.computeIfAbsent(key, ignored -> { Snapshot state = new Snapshot(); state.status = NORMAL; state.since = now; state.updatedAt = now; state.lastHealthyAt = now; state.ordersRetained = true; return state; });
    }

    private void ensureLoaded(String key) {
        if (loaded.containsKey(key)) return; loaded.put(key, true); if (store == null) return; byte[] value = store.loadFaultState(key); if (value == null || value.length == 0) return;
        try { states.put(key, Json.read(value, Snapshot.class)); persisted.put(key, true); } catch (RuntimeException e) { loaded.remove(key); throw e; }
    }

    private void persist(String key, Snapshot state) {
        if (store == null) return;
        if (NORMAL.equals(state.status) && state.consecutiveFailures == 0 && state.consecutiveSuccesses == 0) { if (persisted.remove(key) != null) store.deleteFaultState(key); return; }
        store.saveFaultState(key, Json.writeBytes(state)); persisted.put(key, true);
    }
}
