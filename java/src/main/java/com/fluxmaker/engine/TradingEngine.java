package com.fluxmaker.engine;

import com.fluxmaker.audit.AuditLogger;
import com.fluxmaker.config.AppConfig;
import com.fluxmaker.domain.Domain;
import com.fluxmaker.fault.FaultManager;
import com.fluxmaker.json.Json;
import com.fluxmaker.math.DecimalValue;
import com.fluxmaker.oms.Reconciler;
import com.fluxmaker.risk.RiskEngine;
import com.fluxmaker.runtime.RuntimeStore;
import com.fluxmaker.strategy.QuoteGenerator;
import com.fluxmaker.tradesim.TradeSimulator;
import com.fluxmaker.tradesim.VolumeSimulationPlannerImpl;
import com.fluxmaker.venue.VenueClient;
import com.fasterxml.jackson.databind.node.ObjectNode;

import java.security.SecureRandom;
import java.time.Duration;
import java.time.Instant;
import java.util.ArrayList;
import java.util.Comparator;
import java.util.HashMap;
import java.util.HexFormat;
import java.util.LinkedHashMap;
import java.util.LinkedHashSet;
import java.util.List;
import java.util.Locale;
import java.util.Map;
import java.util.Set;
import java.util.concurrent.ConcurrentHashMap;
import java.util.concurrent.ExecutorService;
import java.util.concurrent.Executors;
import java.util.concurrent.Future;
import java.util.concurrent.ScheduledExecutorService;
import java.util.concurrent.TimeUnit;
import java.util.concurrent.atomic.AtomicBoolean;
import java.util.concurrent.locks.ReentrantLock;

public final class TradingEngine {
    @FunctionalInterface public interface PriceSource { Domain.ReferencePrice price(AppConfig.InstrumentConfig instrument); }
    private AppConfig config;
    private final PriceSource oracle;
    private final Map<String, VenueClient> venues;
    private final RuntimeStore runtime;
    private final AuditLogger audit;
    private final Reconciler reconciler;
    private final RepriceModeTracker repriceModes = new RepriceModeTracker();
    private final QuoteGenerator strategy = new QuoteGenerator();
    private final RiskEngine risk = new RiskEngine();
    private final FaultManager faults;
    private final TradeSimulator simulator =
            new TradeSimulator(new VolumeSimulationPlannerImpl());
    private final String ownerId;
    private final Map<String, Long> heldLeases = new ConcurrentHashMap<>();
    private final Map<String, RuntimeStore.PauseState> paused = new ConcurrentHashMap<>();
    private final Set<String> pauseApplied = ConcurrentHashMap.newKeySet();
    private final Map<String, String> startupFailures = new ConcurrentHashMap<>(), preflightBlocked = new ConcurrentHashMap<>();
    private final Map<String, Instant> lastReferenceValidUntil = new ConcurrentHashMap<>();
    private final Map<String, DecimalValue> lastInventory = new ConcurrentHashMap<>();
    private final Map<String, FillCache> fillCache = new ConcurrentHashMap<>();
    private final Map<String, List<ReentrantLock>> accountLocks;
    private final Metrics metrics = new Metrics();
    // Reused across cycles (Go spawns cheap goroutines per cycle; JVM OS threads
    // are not free, so we keep a fixed pool and only rebuild it when the
    // configured concurrency changes).
    private ExecutorService workerPool;
    private int workerPoolLimit;

    private record FillCache(List<Domain.Fill> fills, String error, Instant at) {}
    private record BalanceResult(Map<String, List<Domain.Balance>> byVenue, DecimalValue inventory, int accountCount, Map<String, RuntimeException> errors) {}

    public TradingEngine(AppConfig config, PriceSource oracle, Map<String, VenueClient> venues, RuntimeStore runtime, AuditLogger audit, String previousOwnerId) {
        this.config = config; this.oracle = oracle; this.venues = venues; this.runtime = runtime; this.audit = audit; this.reconciler = new Reconciler(runtime); this.faults = new FaultManager(config.marketFailureThreshold, config.marketRecoveryThreshold, runtime); this.ownerId = previousOwnerId == null || previousOwnerId.isEmpty() ? newOwnerId() : previousOwnerId; this.accountLocks = accountLocks(config);
    }

    public String ownerId() { return ownerId; }
    public AppConfig effectiveConfig() { return config; }
    public void setStartupFailures(Map<String, String> failures) { startupFailures.clear(); failures.forEach((id, failure) -> { if (failure != null && !failure.isBlank()) startupFailures.put(id, failure); }); }
    public Map<String, String> blockedInstruments() { return new LinkedHashMap<>(preflightBlocked); }

    /** Hot-applies only fields classified as non-structural by ConfigDiff. Exchange-synchronized market rules stay intact. */
    public void applyParameters(AppConfig next) {
        Map<String, AppConfig.InstrumentConfig> byId = new HashMap<>();
        next.instruments.forEach(instrument -> byId.put(instrument.id, instrument));
        for (AppConfig.InstrumentConfig current : config.instruments) {
            AppConfig.InstrumentConfig updated = byId.get(current.id);
            if (updated == null) continue;
            if (updated.strategy.maxVenueReferenceDeviationBps == 0) updated.strategy.maxVenueReferenceDeviationBps = 500;
            if (updated.strategy.maxVenueSpreadBps == 0) updated.strategy.maxVenueSpreadBps = 1000;
            if (requiresUrgentReprice(current.strategy, updated.strategy)) markUrgentMarkets(current.id);
            current.strategy = updated.strategy;
            current.tradeSimulation = updated.tradeSimulation;
        }
        if (next.pollIntervalMs > 0) config.pollIntervalMs = next.pollIntervalMs;
        if (next.maxConcurrentInstruments > 0) config.maxConcurrentInstruments = next.maxConcurrentInstruments;
        if (next.rulesRefreshSeconds > 0) config.rulesRefreshSeconds = next.rulesRefreshSeconds;
        if (next.marketFailureThreshold > 0) config.marketFailureThreshold = next.marketFailureThreshold;
        if (next.marketRecoveryThreshold > 0) config.marketRecoveryThreshold = next.marketRecoveryThreshold;
        if (next.marketErrorGraceSeconds > 0) config.marketErrorGraceSeconds = next.marketErrorGraceSeconds;
        if (next.tradingProgressTimeoutSeconds > 0) config.tradingProgressTimeoutSeconds = next.tradingProgressTimeoutSeconds;
    }

    public void prepare() {
        preflightBlocked.clear(); int ready = 0;
        for (AppConfig.InstrumentConfig instrument : config.instruments) try { withLocks(instrument.id, () -> prepareInstrument(instrument, startupFailures.get(instrument.id))); ready++; } catch (RuntimeException e) { preflightBlocked.put(instrument.id, e.getMessage()); }
        if (!config.instruments.isEmpty() && ready == 0) throw new IllegalStateException("candidate preflight: no runnable instruments: " + preflightBlocked);
    }

    private void prepareInstrument(AppConfig.InstrumentConfig instrument, String startupFailure) {
        List<String> failures = new ArrayList<>(); if (startupFailure != null && !startupFailure.isEmpty()) failures.add("startup: " + startupFailure); Domain.ReferencePrice reference = null;
        try { reference = oracle.price(instrument); lastReferenceValidUntil.put(instrument.id, reference.validUntil); } catch (RuntimeException e) { failures.add("reference: " + e.getMessage()); }
        if (startupFailure != null || reference == null) throw joined(failures);
        int active = 0;
        for (Map.Entry<String, AppConfig.VenueConfig> entry : config.venues.entrySet()) {
            String venueName = entry.getKey(); AppConfig.VenueConfig venue = entry.getValue(); AppConfig.VenueMarketConfig market = venue.markets.get(instrument.id); if (!venue.enabled || market == null) continue; active++; VenueClient client = venues.get(clientKey(venueName, instrument.id)); if (client == null) { failures.add(venueName + ": client missing"); continue; }
            Domain.Book book; try { book = client.topBook(market.symbol); } catch (RuntimeException e) { book = emptyBook(venueName, market.symbol); }
            try { RiskEngine.validateMarketReference(reference, book, instrument.strategy); List<Domain.Quote> quotes = risk.filterQuotes(instrument, market, book, instrument.strategy.targetBase, strategy.generate(instrument, venueName, market, reference, book, instrument.strategy.targetBase)); if (config.mode == Domain.Mode.live && venue.tradingEnabled) { List<Domain.Balance> balances = client.balances(); List<Domain.Order> orders = client.openOrders(market.symbol); List<Domain.Order> managed = Reconciler.managedOrdersFor(client, orders); quotes = RiskEngine.applyOrderLimit(quotes, orders.size(), managed.size(), market.maxOpenOrders); Domain.Balance base = findBalance(balances, market.baseAsset), quote = findBalance(balances, market.quoteAsset); quotes = RiskEngine.applyBalanceBudget(quotes, managed, base == null ? DecimalValue.ZERO : base.free, quote == null ? DecimalValue.ZERO : quote.free, instrument.strategy.balanceReserveBps, market.maxBaseCommitment, market.maxQuoteCommitment).quotes(); if (quotes.isEmpty()) throw new IllegalStateException("budget allows no orders"); } }
            catch (RuntimeException e) { failures.add(venueName + ": " + e.getMessage()); }
        }
        if (active == 0) failures.add("no enabled venue markets"); if (!failures.isEmpty()) throw joined(failures);
    }

    public void runOnce() {
        Instant started = Instant.now(); int limit = Math.max(1, Math.min(config.maxConcurrentInstruments <= 0 ? 4 : config.maxConcurrentInstruments, config.instruments.size())); Map<String, BalanceCacheEntry> balances = new ConcurrentHashMap<>(); List<String> failures = java.util.Collections.synchronizedList(new ArrayList<>()); int succeeded = 0;
        ExecutorService executor = workerPool(limit);
        List<Future<?>> futures = new ArrayList<>(); for (AppConfig.InstrumentConfig instrument : config.instruments) futures.add(executor.submit(() -> { try { withLocks(instrument.id, () -> runInstrumentGuarded(instrument, balances)); } catch (Throwable e) { failures.add(instrument.id + ": " + (e.getMessage() != null ? e.getMessage() : e.toString())); System.err.println("instrument cycle failed hard: " + instrument.id + " -> " + e); } finally { runtime.reportTradingProgress(); } })); for (Future<?> future : futures) try { future.get(); } catch (Exception e) { failures.add("worker: " + e.getMessage()); }
        try { audit.flush(); } catch (RuntimeException e) { metrics.auditFlushError(); }
        metrics.cycle(config.instruments.size(), failures.size()); succeeded = config.instruments.size() - failures.size();
        RuntimeStore.CyclePerformance performance = new RuntimeStore.CyclePerformance(); performance.startedAt = started; performance.durationMs = Duration.between(started, Instant.now()).toMillis(); performance.instruments = config.instruments.size(); performance.succeeded = Math.max(0, succeeded); performance.failed = failures.size(); performance.concurrentLimit = limit; runtime.reportCyclePerformance(performance); runtime.reportMetrics(metrics.snapshot(audit.pendingCount()));
        if (!failures.isEmpty()) throw new IllegalStateException("tick failures: " + String.join("; ", failures));
    }

    private synchronized ExecutorService workerPool(int limit) {
        if (workerPool == null || workerPoolLimit != limit) {
            ExecutorService previous = workerPool;
            workerPool = Executors.newFixedThreadPool(limit, runnable -> { Thread thread = new Thread(runnable, "instrument-worker"); thread.setDaemon(true); return thread; });
            workerPoolLimit = limit;
            if (previous != null) previous.shutdown();
        }
        return workerPool;
    }

    /** Releases the reused worker pool; call when discarding this engine instance. */
    public synchronized void close() { if (workerPool != null) { workerPool.shutdown(); workerPool = null; workerPoolLimit = 0; } }

    private void runInstrumentGuarded(AppConfig.InstrumentConfig instrument, Map<String, BalanceCacheEntry> balances) {
        if (paused.containsKey(instrument.id)) { runPaused(instrument, balances); return; }
        if (preflightBlocked.containsKey(instrument.id)) { publishPreflightBlocked(instrument, preflightBlocked.get(instrument.id)); throw new IllegalStateException("preflight blocked: " + preflightBlocked.get(instrument.id)); }
        runInstrument(instrument, balances);
    }

    private void runInstrument(AppConfig.InstrumentConfig instrument, Map<String, BalanceCacheEntry> balanceCache) {
        Instant started = Instant.now(); RuntimeStore.InstrumentSnapshot snapshot = newSnapshot(instrument); List<String> failures = new ArrayList<>(); RuntimeException finalError = null;
        try {
            Instant referenceStarted = Instant.now(); Domain.ReferencePrice reference;
            try { reference = oracle.price(instrument); snapshot.referenceDurationMs = millis(referenceStarted); lastReferenceValidUntil.put(instrument.id, reference.validUntil); snapshot.reference = reference; audit.record("reference_price", reference); }
            catch (RuntimeException e) { snapshot.referenceDurationMs = millis(referenceStarted); handleReferenceFailure(instrument, snapshot, e); throw e; }
            DecimalValue strategyInventory = instrument.strategy.targetBase; Instant balanceStarted = Instant.now(); BalanceResult balances = collectBalances(instrument, balanceCache); snapshot.balanceDurationMs = millis(balanceStarted); if (balances.accountCount > 0) { snapshot.inventory = balances.inventory; snapshot.inventoryAvailable = true; }
            if (config.mode == Domain.Mode.live) { if (balances.errors.isEmpty() && balances.accountCount > 0) { strategyInventory = balances.inventory; lastInventory.put(instrument.id, balances.inventory); } else if (lastInventory.containsKey(instrument.id)) strategyInventory = lastInventory.get(instrument.id); }
            int active = 0;
            for (Map.Entry<String, AppConfig.VenueConfig> entry : config.venues.entrySet()) {
                String venueName = entry.getKey(); AppConfig.VenueConfig venue = entry.getValue(); AppConfig.VenueMarketConfig market = venue.markets.get(instrument.id); if (!venue.enabled || market == null) continue; active++; RuntimeStore.VenueSnapshot venueSnapshot = venueSnapshot(venueName, venue, market); snapshot.venues.add(venueSnapshot); VenueClient client = venues.get(clientKey(venueName, instrument.id)); if (client == null) { failures.add(venueName + ": missing client"); venueSnapshot.error = "client missing"; continue; }
                RuntimeException balanceError = balances.errors.get(venueName); if (balanceError != null && config.mode == Domain.Mode.live && venue.tradingEnabled) { markFailure(snapshot, venueSnapshot, instrument, venueName, venue, market, client, "balance", balanceError, false); failures.add(venueName + " balance: " + balanceError.getMessage()); continue; }
                List<Domain.Balance> venueBalances = balances.byVenue.get(venueName); if (venueBalances != null) { venueSnapshot.accountConnected = true; venueSnapshot.baseBalance = orEmpty(findBalance(venueBalances, market.baseAsset), market.baseAsset); venueSnapshot.quoteBalance = orEmpty(findBalance(venueBalances, market.quoteAsset), market.quoteAsset); }
                Instant bookStarted = Instant.now(); Domain.Book book; try { book = client.topBook(market.symbol); venueSnapshot.marketConnected = true; venueSnapshot.book = book; } catch (RuntimeException e) { venueSnapshot.error = append(venueSnapshot.error, "盘口不可用，按指数价铺单: " + e.getMessage()); book = emptyBook(venueName, market.symbol); } venueSnapshot.bookDurationMs = millis(bookStarted);
                try { RiskEngine.validateMarketReference(reference, book, instrument.strategy); } catch (RuntimeException e) { markFailure(snapshot, venueSnapshot, instrument, venueName, venue, market, client, "market_reference", e, true); failures.add(venueName + " price protection: " + e.getMessage()); continue; }
                if (instrument.tradeSimulation.enabled && venueName.equals(instrument.tradeSimulation.sourceVenue)) { TradeSimulator.Observation observation = simulator.observe(instrument, venueName, market, book, Instant.now()); snapshot.tradeSimulation = Json.tree(observation.snapshot()); if (observation.fill() != null) { runtime.appendSimulatedFill(instrument.id, observation.fill()); audit.record("simulated_trade", Map.of("instrument", instrument.id, "source_venue", venueName, "fill", observation.fill())); metrics.simulatedTrade(); } }
                RuntimeException ordersError = null; if (market.credentialId > 0) { Instant ordersStarted = Instant.now(); try { venueSnapshot.openOrders = client.openOrders(market.symbol); venueSnapshot.accountConnected = true; } catch (RuntimeException e) { ordersError = e; venueSnapshot.accountConnected = false; venueSnapshot.error = append(venueSnapshot.error, "orders: " + e.getMessage()); } venueSnapshot.ordersDurationMs = millis(ordersStarted); Instant fillsStarted = Instant.now(); try { venueSnapshot.fills = recentFills(venueName, instrument.id, client, market.symbol); } catch (RuntimeException e) { venueSnapshot.error = append(venueSnapshot.error, "fills: " + e.getMessage()); } venueSnapshot.fillsDurationMs = millis(fillsStarted); }
                List<Domain.Quote> quotes; try { quotes = risk.filterQuotes(instrument, market, book, strategyInventory, strategy.generate(instrument, venueName, market, reference, book, strategyInventory)); } catch (RuntimeException e) { markFailure(snapshot, venueSnapshot, instrument, venueName, venue, market, client, "strategy/risk", e, true); failures.add(venueName + " strategy/risk: " + e.getMessage()); continue; }
                List<Domain.Order> managed = Reconciler.managedOrdersFor(client, venueSnapshot.openOrders);
                if (config.mode != Domain.Mode.live || !venue.tradingEnabled) { FaultManager.Decision health = faults.healthy(faultKey(venueName, instrument.id, market), 0); venueSnapshot.fault = Json.tree(health.state()); if (!FaultManager.NORMAL.equals(health.state().status)) failures.add(venueName + " fault state: " + health.state().status); audit.record("quote_plan", Map.of("instrument", instrument.id, "venue", venueName, "quote_count", quotes.size())); continue; }
                if (ordersError != null) { markFailure(snapshot, venueSnapshot, instrument, venueName, venue, market, client, "open_orders", ordersError, false); failures.add(venueName + " orders: " + ordersError.getMessage()); continue; }
                String leaseKey = leaseKey(venueName, instrument.id, market); long generation = acquireLease(leaseKey); if (generation == 0) { failures.add(venueName + " lease: market is owned by another engine instance"); continue; }
                try (LeaseKeeper keeper = new LeaseKeeper(leaseKey, generation)) {
                quotes = RiskEngine.applyOrderLimit(quotes, venueSnapshot.openOrders.size(), managed.size(), market.maxOpenOrders); DecimalValue baseFree = venueSnapshot.baseBalance == null ? DecimalValue.ZERO : venueSnapshot.baseBalance.free, quoteFree = venueSnapshot.quoteBalance == null ? DecimalValue.ZERO : venueSnapshot.quoteBalance.free; RiskEngine.BudgetResult budget = RiskEngine.applyBalanceBudget(quotes, managed, baseFree, quoteFree, instrument.strategy.balanceReserveBps, market.maxBaseCommitment, market.maxQuoteCommitment); quotes = budget.quotes(); venueSnapshot.budget = budget.budget();
                if (quotes.isEmpty()) { RuntimeException error = new IllegalStateException("balance budget allows no orders"); markFailure(snapshot, venueSnapshot, instrument, venueName, venue, market, client, "budget", error, true); failures.add(venueName + " budget: " + error.getMessage()); continue; }
                String marketStateKey = faultKey(venueName, instrument.id, market);
                FaultManager.Decision health = faults.healthy(marketStateKey, managed.size()); venueSnapshot.fault = Json.tree(health.state()); Reconciler.WriteGuard guard = keeper.guard();
                if (health.shouldCancel()) { int canceled = reconciler.cancelManaged(client, instrument.id, market.symbol, guard); metrics.oms(0, canceled); failures.add(venueName + " fault state: " + health.state().status); continue; }
                if (!health.allowQuotes()) { failures.add(venueName + " fault state: " + health.state().status); continue; }
                Reconciler.RefreshPolicy refreshPolicy = Reconciler.RefreshPolicy.disabled();
                if (instrument.strategy.quoteRefreshSeconds > 0) {
                    refreshPolicy = new Reconciler.RefreshPolicy(
                            instrument.strategy.effectiveMinOrderLifetime(),
                            instrument.strategy.effectiveMaxOrderLifetime(),
                            instrument.strategy.refreshOrdersPerCycle(quotes.size()));
                }
                boolean gradualMaterialReprice = refreshPolicy.maxRefreshesPerCycle() > 0
                        && repriceModes.gradualAllowed(
                                marketStateKey,
                                reference.price,
                                instrument.strategy.maxVenueReferenceDeviationBps);
                Reconciler.ReplenishPolicy replenishPolicy = new Reconciler.ReplenishPolicy(
                        instrument.strategy.effectiveFillReplenishMinDelay(),
                        instrument.strategy.effectiveFillReplenishMaxDelay(),
                        2);
                audit.record("quote_plan", Map.of("instrument", instrument.id, "venue", venueName, "budget", budget.budget(), "quote_count", quotes.size()));
                Instant omsStarted = Instant.now();
                try {
                    Reconciler.Result result = reconciler.reconcileWithOrders(
                            client, instrument.id, quotes, instrument.strategy.repriceThresholdBps,
                            venueSnapshot.openOrders, guard, generation, refreshPolicy,
                            gradualMaterialReprice, replenishPolicy);
                    repriceModes.observeResult(marketStateKey, result, quotes.size());
                    venueSnapshot.pendingOrders = result.pending;
                    metrics.oms(result.placed, result.canceled);
                    audit.record("oms_result", Map.of(
                            "instrument", instrument.id,
                            "venue", venueName,
                            "reprice_mode", gradualMaterialReprice ? "gradual" : "urgent",
                            "result", result));
                } catch (RuntimeException e) { try { metrics.oms(0, reconciler.cancelManaged(client, instrument.id, market.symbol, guard)); } catch (RuntimeException ignored) {} markFailure(snapshot, venueSnapshot, instrument, venueName, venue, market, client, "oms", e, true); failures.add(venueName + " OMS: " + e.getMessage()); }
                venueSnapshot.omsDurationMs = millis(omsStarted);
                }
            }
            if (active == 0) failures.add("no enabled venue markets"); if (!failures.isEmpty()) throw new IllegalStateException(String.join("; ", failures));
        } catch (RuntimeException e) { finalError = e; throw e; }
        finally { snapshot.tickDurationMs = millis(started); snapshot.updatedAt = Instant.now(); if (finalError != null) { snapshot.status = "degraded"; snapshot.error = finalError.getMessage(); } runtime.publish(snapshot); }
    }

    private void handleReferenceFailure(AppConfig.InstrumentConfig instrument, RuntimeStore.InstrumentSnapshot snapshot, RuntimeException error) {
        audit.record("reference_rejected", Map.of("instrument", instrument.id, "error", error.getMessage())); if (config.mode != Domain.Mode.live) return; boolean stale = !lastReferenceValidUntil.containsKey(instrument.id) || Instant.now().isAfter(lastReferenceValidUntil.get(instrument.id));
        for (Map.Entry<String, AppConfig.VenueConfig> entry : config.venues.entrySet()) { AppConfig.VenueConfig venue = entry.getValue(); AppConfig.VenueMarketConfig market = venue.markets.get(instrument.id); VenueClient client = venues.get(clientKey(entry.getKey(), instrument.id)); if (!venue.enabled || !venue.tradingEnabled || market == null || client == null) continue; RuntimeStore.VenueSnapshot view = venueSnapshot(entry.getKey(), venue, market); snapshot.venues.add(view); markFailure(snapshot, view, instrument, entry.getKey(), venue, market, client, "reference", error, stale); }
    }

    private void markFailure(RuntimeStore.InstrumentSnapshot snapshot, RuntimeStore.VenueSnapshot view, AppConfig.InstrumentConfig instrument, String venueName, AppConfig.VenueConfig venue, AppConfig.VenueMarketConfig market, VenueClient client, String stage, RuntimeException cause, boolean forceCancel) {
        metrics.venueFault(); FaultManager.Decision decision = faults.failure(faultKey(venueName, instrument.id, market), stage, cause, forceCancel); view.fault = Json.tree(decision.state()); view.error = append(view.error, stage + ": " + cause.getMessage()); if (decision.shouldCancel() && config.mode == Domain.Mode.live && venue.tradingEnabled) try { cancelManagedFenced(venueName, instrument.id, market, client); } catch (RuntimeException e) { view.error = append(view.error, "cancel: " + e.getMessage()); } audit.record("venue_fault", Map.of("instrument", instrument.id, "venue", venueName, "stage", stage, "cause", cause.getMessage(), "fault", decision.state()));
    }

    public void applyControls() {
        Map<String, RuntimeStore.PauseState> desired = runtime.paused(); List<String> failures = new ArrayList<>();
        for (AppConfig.InstrumentConfig instrument : config.instruments) { RuntimeStore.PauseState state = desired.get(instrument.id); if (state == null) { boolean wasPaused = paused.remove(instrument.id) != null; pauseApplied.remove(instrument.id); if (wasPaused) try { clearBlocks(instrument); } catch (RuntimeException e) { failures.add(instrument.id + " unblock: " + e.getMessage()); } continue; } paused.put(instrument.id, state); if (pauseApplied.contains(instrument.id)) continue; if (!RuntimeStore.REASON_EMERGENCY_CANCEL.equals(state.reason)) { pauseApplied.add(instrument.id); audit.record("instrument_paused", Map.of("instrument", instrument.id, "reason", state.reason, "requested_by", state.requestedBy, "orders_retained", true)); publishPaused(instrument, null); continue; } try { cancelInstrument(instrument); if (cancellationConfirmed(instrument)) pauseApplied.add(instrument.id); } catch (RuntimeException e) { failures.add(instrument.id + ": " + e.getMessage()); publishPaused(instrument, e.getMessage()); } }
        for (RuntimeStore.ReconcileRequest request : runtime.reconciles().values()) { AppConfig.InstrumentConfig instrument = instrument(request.instrumentId); if (instrument == null) continue; try { cancelInstrument(instrument); clearBlocks(instrument); runtime.clearReconcile(instrument.id); audit.record("instrument_reconciled", request); } catch (RuntimeException e) { failures.add(instrument.id + " reconcile: " + e.getMessage()); } }
        audit.flush(); if (!failures.isEmpty()) throw new IllegalStateException("apply controls: " + String.join("; ", failures));
    }

    private void runPaused(AppConfig.InstrumentConfig instrument, Map<String, BalanceCacheEntry> cache) {
        RuntimeStore.InstrumentSnapshot snapshot = newSnapshot(instrument); snapshot.pause = paused.get(instrument.id); snapshot.paused = pauseApplied.contains(instrument.id); snapshot.status = snapshot.paused ? "paused" : "pausing"; Instant started = Instant.now(); List<String> failures = new ArrayList<>(); try { snapshot.reference = oracle.price(instrument); } catch (RuntimeException e) { failures.add("reference: " + e.getMessage()); }
        BalanceResult balances = collectBalances(instrument, cache); if (balances.accountCount > 0) { snapshot.inventory = balances.inventory; snapshot.inventoryAvailable = true; lastInventory.put(instrument.id, balances.inventory); }
        for (Map.Entry<String, AppConfig.VenueConfig> entry : config.venues.entrySet()) { AppConfig.VenueConfig venue = entry.getValue(); AppConfig.VenueMarketConfig market = venue.markets.get(instrument.id); if (!venue.enabled || market == null) continue; RuntimeStore.VenueSnapshot view = venueSnapshot(entry.getKey(), venue, market); snapshot.venues.add(view); VenueClient client = venues.get(clientKey(entry.getKey(), instrument.id)); if (client == null) { view.error = "client missing"; continue; } List<Domain.Balance> venueBalances = balances.byVenue.get(entry.getKey()); if (venueBalances != null) { view.accountConnected = true; view.baseBalance = orEmpty(findBalance(venueBalances, market.baseAsset), market.baseAsset); view.quoteBalance = orEmpty(findBalance(venueBalances, market.quoteAsset), market.quoteAsset); } try { view.book = client.topBook(market.symbol); view.marketConnected = true; } catch (RuntimeException e) { view.error = append(view.error, "book: " + e.getMessage()); } if (market.credentialId > 0) { try { view.openOrders = client.openOrders(market.symbol); view.accountConnected = true; } catch (RuntimeException e) { view.error = append(view.error, "orders: " + e.getMessage()); } try { view.fills = recentFills(entry.getKey(), instrument.id, client, market.symbol); } catch (RuntimeException e) { view.error = append(view.error, "fills: " + e.getMessage()); } } }
        snapshot.tickDurationMs = millis(started); snapshot.updatedAt = Instant.now(); if (!failures.isEmpty()) snapshot.error = String.join("; ", failures); runtime.publish(snapshot);
    }

    public int refreshMarketRules() {
        int changes = 0; List<String> failures = new ArrayList<>();
        for (Map.Entry<String, AppConfig.VenueConfig> entry : config.venues.entrySet()) if (entry.getValue().enabled) for (Map.Entry<String, AppConfig.VenueMarketConfig> marketEntry : entry.getValue().markets.entrySet()) { VenueClient client = venues.get(clientKey(entry.getKey(), marketEntry.getKey())); if (client == null || !client.capabilities().marketRules()) { failures.add(entry.getKey() + "/" + marketEntry.getKey() + ": trading rules unavailable"); continue; } try { Domain.MarketRules rules = client.marketRules(marketEntry.getValue().symbol); Domain.MarketRules previous = rules(marketEntry.getValue()); applyRules(marketEntry.getValue(), rules); if (!Json.tree(previous).equals(Json.tree(rules(marketEntry.getValue())))) { repriceModes.markUrgent(faultKey(entry.getKey(), marketEntry.getKey(), marketEntry.getValue())); RuntimeStore.RuleChange change = new RuntimeStore.RuleChange(); change.instrumentId = marketEntry.getKey(); change.venue = entry.getKey(); change.symbol = marketEntry.getValue().symbol; change.previous = previous; change.current = rules(marketEntry.getValue()); change.detectedAt = Instant.now(); runtime.reportRuleChange(change); audit.record("trading_rules_changed", change); metrics.ruleChange(); changes++; } } catch (RuntimeException e) { failures.add(entry.getKey() + "/" + marketEntry.getKey() + ": " + e.getMessage()); } }
        audit.flush(); if (!failures.isEmpty()) throw new IllegalStateException("refresh trading rules: " + String.join("; ", failures)); return changes;
    }

    public int retryBlocked() { int recovered = 0; for (String id : new ArrayList<>(preflightBlocked.keySet())) { AppConfig.InstrumentConfig instrument = instrument(id); if (instrument == null) continue; try { prepareInstrument(instrument, null); preflightBlocked.remove(id); startupFailures.remove(id); recovered++; } catch (RuntimeException e) { preflightBlocked.put(id, e.getMessage()); } } return recovered; }

    private void markUrgentMarkets(String instrumentId) {
        for (Map.Entry<String, AppConfig.VenueConfig> entry : config.venues.entrySet()) {
            AppConfig.VenueMarketConfig market = entry.getValue().markets.get(instrumentId);
            if (entry.getValue().enabled && market != null) {
                repriceModes.markUrgent(faultKey(entry.getKey(), instrumentId, market));
            }
        }
    }

    private static boolean requiresUrgentReprice(AppConfig.StrategyConfig current, AppConfig.StrategyConfig updated) {
        ObjectNode previous = (ObjectNode) Json.tree(current);
        ObjectNode next = (ObjectNode) Json.tree(updated);
        previous.remove(List.of("fill_replenish_min_delay_seconds", "fill_replenish_max_delay_seconds"));
        next.remove(List.of("fill_replenish_min_delay_seconds", "fill_replenish_max_delay_seconds"));
        return !previous.equals(next);
    }

    public void cancelMarket(String instrumentId, String venueName) { AppConfig.VenueConfig venue = config.venues.get(venueName); if (venue == null || !venue.enabled || !venue.tradingEnabled) return; AppConfig.VenueMarketConfig market = venue.markets.get(instrumentId); if (market == null) return; VenueClient client = venues.get(clientKey(venueName, instrumentId)); if (client == null) throw new IllegalStateException("client missing"); cancelManagedFenced(venueName, instrumentId, market, client); faults.reset(faultKey(venueName, instrumentId, market)); }
    public void shutdown() { List<String> failures = new ArrayList<>(); for (AppConfig.InstrumentConfig instrument : config.instruments) try { cancelInstrument(instrument); } catch (RuntimeException e) { failures.add(instrument.id + ": " + e.getMessage()); } for (Map.Entry<String, Long> lease : new HashMap<>(heldLeases).entrySet()) try { runtime.releaseMarketLease(lease.getKey(), ownerId, lease.getValue()); heldLeases.remove(lease.getKey()); } catch (RuntimeException e) { failures.add("lease " + lease.getKey() + ": " + e.getMessage()); } audit.flush(); close(); if (!failures.isEmpty()) throw new IllegalStateException("shutdown cancel failures: " + String.join("; ", failures)); }

    private void cancelInstrument(AppConfig.InstrumentConfig instrument) { if (preflightBlocked.containsKey(instrument.id)) return; List<String> failures = new ArrayList<>(); for (Map.Entry<String, AppConfig.VenueConfig> entry : config.venues.entrySet()) { AppConfig.VenueConfig venue = entry.getValue(); AppConfig.VenueMarketConfig market = venue.markets.get(instrument.id); VenueClient client = venues.get(clientKey(entry.getKey(), instrument.id)); if (!venue.enabled || !venue.tradingEnabled || market == null || client == null) continue; try { cancelManagedFenced(entry.getKey(), instrument.id, market, client); } catch (RuntimeException e) { failures.add(entry.getKey() + ": " + e.getMessage()); } } if (!failures.isEmpty()) throw new IllegalStateException("cancel failures: " + String.join("; ", failures)); }
    private void cancelManagedFenced(String venue, String instrument, AppConfig.VenueMarketConfig market, VenueClient client) { String key = leaseKey(venue, instrument, market); long generation = acquireLease(key); if (generation == 0) throw new IllegalStateException("market is owned by another engine instance"); try (LeaseKeeper keeper = new LeaseKeeper(key, generation)) { metrics.oms(0, reconciler.cancelManaged(client, instrument, market.symbol, keeper.guard())); } }
    private long acquireLease(String key) { long generation = runtime.acquireMarketLease(key, ownerId, leaseTtl()); if (generation > 0) heldLeases.put(key, generation); return generation; }
    private Reconciler.WriteGuard guard(String key, long generation) { return () -> { if (!runtime.validateMarketLease(key, ownerId, generation)) { metrics.leaseFenceReject(); audit.record("lease_fence_rejected", Map.of("market", key, "owner", ownerId, "generation", generation)); throw new IllegalStateException("stale market lease generation " + generation); } }; }
    private Duration leaseTtl() { return Duration.ofSeconds(Math.max(30, config.watchdogTimeoutSeconds * 2L)); }

    private final class LeaseKeeper implements AutoCloseable {
        private final String key; private final long generation; private final AtomicBoolean current = new AtomicBoolean(true); private final ScheduledExecutorService renewer;
        private LeaseKeeper(String key, long generation) {
            this.key = key; this.generation = generation;
            renewer = Executors.newSingleThreadScheduledExecutor(runnable -> { Thread thread = new Thread(runnable, "lease-renewal"); thread.setDaemon(true); return thread; });
            long interval = Math.max(1000, leaseTtl().toMillis() / 3);
            renewer.scheduleAtFixedRate(() -> { try { if (runtime.acquireMarketLease(key, ownerId, leaseTtl()) != generation) current.set(false); } catch (RuntimeException e) { current.set(false); } }, interval, interval, TimeUnit.MILLISECONDS);
        }
        private Reconciler.WriteGuard guard() { return () -> { if (!current.get()) throw new IllegalStateException("stale market lease generation " + generation); TradingEngine.this.guard(key, generation).check(); }; }
        @Override public void close() { renewer.shutdownNow(); }
    }

    private BalanceResult collectBalances(AppConfig.InstrumentConfig instrument, Map<String, BalanceCacheEntry> cache) { Map<String, List<Domain.Balance>> byVenue = new HashMap<>(); Map<String, RuntimeException> errors = new HashMap<>(); DecimalValue inventory = DecimalValue.ZERO; int count = 0; for (Map.Entry<String, AppConfig.VenueConfig> entry : config.venues.entrySet()) { AppConfig.VenueConfig venue = entry.getValue(); AppConfig.VenueMarketConfig market = venue.markets.get(instrument.id); if (!venue.enabled || (config.mode == Domain.Mode.live && !venue.tradingEnabled) || market == null || market.credentialId <= 0) continue; VenueClient client = venues.get(clientKey(entry.getKey(), instrument.id)); if (client == null) { errors.put(entry.getKey(), new IllegalStateException("client missing")); continue; } String key = venue.type.toLowerCase(Locale.ROOT) + "/" + market.credentialId; BalanceCacheEntry cached = cache.computeIfAbsent(key, ignored -> { try { return new BalanceCacheEntry(client.balances(), null); } catch (RuntimeException e) { return new BalanceCacheEntry(List.of(), e); } }); if (cached.error != null) { errors.put(entry.getKey(), cached.error); continue; } byVenue.put(entry.getKey(), cached.balances); Domain.Balance base = findBalance(cached.balances, market.baseAsset); if (base != null) inventory = inventory.add(base.free).add(base.locked); count++; } return new BalanceResult(byVenue, inventory, count, errors); }
    private record BalanceCacheEntry(List<Domain.Balance> balances, RuntimeException error) {}
    private List<Domain.Fill> recentFills(String venue, String instrument, VenueClient client, String symbol) { if (!client.capabilities().recentFills()) return List.of(); String key = clientKey(venue, instrument); FillCache cached = fillCache.get(key); if (cached != null && Duration.between(cached.at, Instant.now()).compareTo(Duration.ofSeconds(10)) < 0) { if (cached.error != null) throw new IllegalStateException(cached.error); return new ArrayList<>(cached.fills); } try { List<Domain.Fill> fills = client.recentFills(symbol, 50); fillCache.put(key, new FillCache(new ArrayList<>(fills), null, Instant.now())); return fills; } catch (RuntimeException e) { fillCache.put(key, new FillCache(List.of(), e.getMessage(), Instant.now())); throw e; } }

    private void clearBlocks(AppConfig.InstrumentConfig instrument) { for (Map.Entry<String, AppConfig.VenueConfig> entry : config.venues.entrySet()) { AppConfig.VenueMarketConfig market = entry.getValue().markets.get(instrument.id); VenueClient client = venues.get(clientKey(entry.getKey(), instrument.id)); if (!entry.getValue().enabled || !entry.getValue().tradingEnabled || market == null || client == null) continue; reconciler.clearBlocked(client, instrument.id); faults.reset(faultKey(entry.getKey(), instrument.id, market)); } }
    private boolean cancellationConfirmed(AppConfig.InstrumentConfig instrument) { for (Map.Entry<String, AppConfig.VenueConfig> entry : config.venues.entrySet()) { AppConfig.VenueMarketConfig market = entry.getValue().markets.get(instrument.id); VenueClient client = venues.get(clientKey(entry.getKey(), instrument.id)); if (!entry.getValue().enabled || !entry.getValue().tradingEnabled || market == null || client == null) continue; if (!Reconciler.managedOrdersFor(client, client.openOrders(market.symbol)).isEmpty()) return false; } return true; }
    private void publishPaused(AppConfig.InstrumentConfig instrument, String error) { RuntimeStore.InstrumentSnapshot snapshot = runtime.get(instrument.id); if (snapshot == null) snapshot = newSnapshot(instrument); snapshot.pause = paused.get(instrument.id); snapshot.paused = pauseApplied.contains(instrument.id); snapshot.status = snapshot.paused ? "paused" : "pausing"; snapshot.error = error; snapshot.updatedAt = Instant.now(); runtime.publish(snapshot); }
    private void publishPreflightBlocked(AppConfig.InstrumentConfig instrument, String failure) { RuntimeStore.InstrumentSnapshot snapshot = newSnapshot(instrument); snapshot.status = "degraded"; snapshot.error = "startup preflight: " + failure; snapshot.updatedAt = Instant.now(); for (Map.Entry<String, AppConfig.VenueConfig> entry : config.venues.entrySet()) { AppConfig.VenueMarketConfig market = entry.getValue().markets.get(instrument.id); if (entry.getValue().enabled && market != null) { RuntimeStore.VenueSnapshot view = venueSnapshot(entry.getKey(), entry.getValue(), market); view.error = failure; snapshot.venues.add(view); } } runtime.publish(snapshot); }

    private RuntimeStore.InstrumentSnapshot newSnapshot(AppConfig.InstrumentConfig instrument) { RuntimeStore.InstrumentSnapshot snapshot = new RuntimeStore.InstrumentSnapshot(); snapshot.instrumentId = instrument.id; snapshot.baseSymbol = instrument.base.symbol; snapshot.quoteSymbol = instrument.quote.symbol; snapshot.mode = config.mode; snapshot.status = "running"; snapshot.targetInventory = instrument.strategy.targetBase; snapshot.maxBaseDeviation = instrument.strategy.maxBaseDeviation; if (instrument.tradeSimulation.enabled) { TradeSimulator.Snapshot simulation = new TradeSimulator.Snapshot(); simulation.enabled = true; simulation.sourceVenue = instrument.tradeSimulation.sourceVenue; simulation.status = "waiting"; snapshot.tradeSimulation = Json.tree(simulation); } return snapshot; }
    private static RuntimeStore.VenueSnapshot venueSnapshot(String name, AppConfig.VenueConfig venue, AppConfig.VenueMarketConfig market) { RuntimeStore.VenueSnapshot view = new RuntimeStore.VenueSnapshot(); view.name = name; view.type = venue.type; view.symbol = market.symbol; view.tradingEnabled = venue.tradingEnabled; view.rules = rules(market); view.updatedAt = Instant.now(); return view; }
    private static Domain.MarketRules rules(AppConfig.VenueMarketConfig market) { Domain.MarketRules rules = new Domain.MarketRules(); rules.symbol = market.symbol; rules.baseAsset = market.baseAsset; rules.quoteAsset = market.quoteAsset; rules.priceTick = market.priceTick; rules.quantityStep = market.quantityStep; rules.minQuantity = market.minQuantity; rules.maxQuantity = market.maxQuantity; rules.minNotional = market.minNotional; rules.maxNotional = market.maxNotional; rules.minPrice = market.minPrice; rules.maxPrice = market.maxPrice; rules.maxOpenOrders = market.maxOpenOrders; return rules; }
    private static void applyRules(AppConfig.VenueMarketConfig market, Domain.MarketRules rules) { market.priceTick = rules.priceTick; market.quantityStep = rules.quantityStep; if (rules.minNotional.isPositive()) market.minNotional = rules.minNotional; market.minQuantity = rules.minQuantity; market.maxQuantity = rules.maxQuantity; market.maxNotional = rules.maxNotional; market.minPrice = rules.minPrice; market.maxPrice = rules.maxPrice; market.maxOpenOrders = rules.maxOpenOrders; }
    private static String clientKey(String venue, String instrument) { return venue.trim().toLowerCase(Locale.ROOT) + "|" + instrument.trim().toLowerCase(Locale.ROOT); }
    private static String faultKey(String venue, String instrument, AppConfig.VenueMarketConfig market) { return (venue.trim() + "/" + market.credentialId + "/" + instrument.trim() + "/" + market.symbol.trim()).toLowerCase(Locale.ROOT); }
    private static String leaseKey(String venue, String instrument, AppConfig.VenueMarketConfig market) { return faultKey(venue, instrument, market); }
    private AppConfig.InstrumentConfig instrument(String id) { return config.instruments.stream().filter(value -> value.id.equals(id)).findFirst().orElse(null); }
    private static Domain.Book emptyBook(String venue, String symbol) { Domain.Book book = new Domain.Book(); book.venue = venue; book.symbol = symbol; return book; }
    private static Domain.Balance findBalance(List<Domain.Balance> balances, String asset) { return balances.stream().filter(balance -> balance.asset.equalsIgnoreCase(asset)).findFirst().orElse(null); }
    private static Domain.Balance orEmpty(Domain.Balance value, String asset) { if (value != null) return value; Domain.Balance balance = new Domain.Balance(); balance.asset = asset; return balance; }
    private static String append(String current, String next) { return current == null || current.isEmpty() ? next : current + "; " + next; }
    private static long millis(Instant start) { return Duration.between(start, Instant.now()).toMillis(); }
    private static RuntimeException joined(List<String> failures) { return new IllegalStateException(String.join("; ", failures)); }
    private static String newOwnerId() { byte[] value = new byte[16]; new SecureRandom().nextBytes(value); return HexFormat.of().formatHex(value); }

    private void withLocks(String instrument, Runnable action) { List<ReentrantLock> locks = accountLocks.getOrDefault(instrument, List.of()); locks.forEach(ReentrantLock::lock); try { action.run(); } finally { for (int index = locks.size() - 1; index >= 0; index--) locks.get(index).unlock(); } }
    private static Map<String, List<ReentrantLock>> accountLocks(AppConfig config) { Map<String, ReentrantLock> byAccount = new HashMap<>(); Map<String, Set<String>> keys = new HashMap<>(); for (AppConfig.VenueConfig venue : config.venues.values()) if (venue.enabled) for (Map.Entry<String, AppConfig.VenueMarketConfig> market : venue.markets.entrySet()) if (market.getValue().credentialId > 0) { String key = venue.type.toLowerCase(Locale.ROOT) + "/" + market.getValue().credentialId; byAccount.computeIfAbsent(key, ignored -> new ReentrantLock()); keys.computeIfAbsent(market.getKey(), ignored -> new LinkedHashSet<>()).add(key); } Map<String, List<ReentrantLock>> result = new HashMap<>(); keys.forEach((instrument, values) -> result.put(instrument, values.stream().sorted().map(byAccount::get).toList())); return result; }

    private static final class Metrics {
        final Instant startedAt = Instant.now(); long cyclesTotal, cycleFailuresTotal, instrumentRunsTotal, instrumentFailuresTotal, venueFaultEventsTotal, omsPlacedTotal, omsCanceledTotal, simulatedTradesTotal, auditFlushErrorsTotal, ruleChangesTotal, leaseFenceRejectsTotal;
        synchronized void cycle(int instruments, int failures) { cyclesTotal++; instrumentRunsTotal += instruments; instrumentFailuresTotal += failures; if (failures > 0) cycleFailuresTotal++; }
        synchronized void venueFault() { venueFaultEventsTotal++; }
        synchronized void oms(long placed, long canceled) { omsPlacedTotal += placed; omsCanceledTotal += canceled; }
        synchronized void simulatedTrade() { simulatedTradesTotal++; }
        synchronized void auditFlushError() { auditFlushErrorsTotal++; }
        synchronized void ruleChange() { ruleChangesTotal++; }
        synchronized void leaseFenceReject() { leaseFenceRejectsTotal++; }
        synchronized RuntimeStore.MetricsSnapshot snapshot(int auditPending) { RuntimeStore.MetricsSnapshot value = new RuntimeStore.MetricsSnapshot(); value.startedAt = startedAt; value.updatedAt = Instant.now(); value.cyclesTotal = cyclesTotal; value.cycleFailuresTotal = cycleFailuresTotal; value.instrumentRunsTotal = instrumentRunsTotal; value.instrumentFailuresTotal = instrumentFailuresTotal; value.venueFaultEventsTotal = venueFaultEventsTotal; value.omsPlacedTotal = omsPlacedTotal; value.omsCanceledTotal = omsCanceledTotal; value.simulatedTradesTotal = simulatedTradesTotal; value.auditFlushErrorsTotal = auditFlushErrorsTotal; value.auditPendingEvents = auditPending; value.ruleChangesTotal = ruleChangesTotal; value.leaseFenceRejectsTotal = leaseFenceRejectsTotal; return value; }
    }
}
