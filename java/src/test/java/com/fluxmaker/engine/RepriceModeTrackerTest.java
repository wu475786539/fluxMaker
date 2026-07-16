package com.fluxmaker.engine;

import com.fluxmaker.math.DecimalValue;
import com.fluxmaker.oms.Reconciler;
import org.junit.jupiter.api.Test;

import static org.junit.jupiter.api.Assertions.assertFalse;
import static org.junit.jupiter.api.Assertions.assertTrue;

class RepriceModeTrackerTest {
    @Test void onlyProtectionLevelReferenceMovesStayUrgentUntilTargetsConverge() {
        RepriceModeTracker tracker = new RepriceModeTracker();
        String market = "mgbx/gdt_usdt";

        assertTrue(tracker.gradualAllowed(market, DecimalValue.parse("0.37000"), 500));
        assertTrue(tracker.gradualAllowed(market, DecimalValue.parse("0.37100"), 500));

        assertFalse(tracker.gradualAllowed(market, DecimalValue.parse("0.40000"), 500));
        tracker.observeResult(market, result(2, 0, 0, 0), 10);
        assertFalse(tracker.gradualAllowed(market, DecimalValue.parse("0.40000"), 500));
        tracker.observeResult(market, result(0, 10, 0, 0), 10);
        assertTrue(tracker.gradualAllowed(market, DecimalValue.parse("0.40000"), 500));
    }

    @Test void strategyChangesForceUrgentModeUntilTargetsConverge() {
        RepriceModeTracker tracker = new RepriceModeTracker();
        String market = "mgbx/gdt_usdt";
        tracker.gradualAllowed(market, DecimalValue.parse("0.37000"), 500);
        tracker.observeResult(market, result(0, 10, 0, 0), 10);

        tracker.markUrgent(market);

        assertFalse(tracker.gradualAllowed(market, DecimalValue.parse("0.37000"), 500));
        tracker.observeResult(market, result(0, 10, 0, 0), 10);
        assertTrue(tracker.gradualAllowed(market, DecimalValue.parse("0.37000"), 500));
    }

    private static Reconciler.Result result(int canceled, int kept, int placed, int pending) {
        Reconciler.Result result = new Reconciler.Result();
        result.canceled = canceled;
        result.kept = kept;
        result.placed = placed;
        result.pending = pending;
        return result;
    }
}
