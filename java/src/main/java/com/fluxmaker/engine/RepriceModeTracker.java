package com.fluxmaker.engine;

import com.fluxmaker.math.DecimalValue;
import com.fluxmaker.oms.Reconciler;

import java.util.Map;
import java.util.Set;
import java.util.concurrent.ConcurrentHashMap;

/** Keeps urgent repricing active until the whole market target is aligned. */
final class RepriceModeTracker {
    private final Map<String, DecimalValue> previousReferences = new ConcurrentHashMap<>();
    private final Set<String> urgentMarkets = ConcurrentHashMap.newKeySet();

    boolean gradualAllowed(String market, DecimalValue reference, int abnormalMoveBps) {
        DecimalValue previous = previousReferences.put(market, reference);
        if (previous != null
                && abnormalMoveBps > 0
                && !Reconciler.withinBps(previous, reference, abnormalMoveBps)) {
            urgentMarkets.add(market);
        }
        return !urgentMarkets.contains(market);
    }

    void markUrgent(String market) {
        urgentMarkets.add(market);
    }

    void observeResult(String market, Reconciler.Result result, int targetOrders) {
        if (targetOrders > 0
                && result.canceled == 0
                && result.placed == 0
                && result.pending == 0
                && result.kept >= targetOrders) {
            urgentMarkets.remove(market);
        }
    }
}
