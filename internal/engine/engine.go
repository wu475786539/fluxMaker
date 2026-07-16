package engine

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"
	"runtime/debug"
	"sort"
	"strings"
	"sync"
	"time"

	"fluxmaker/internal/audit"
	"fluxmaker/internal/config"
	"fluxmaker/internal/domain"
	"fluxmaker/internal/fault"
	"fluxmaker/internal/num"
	"fluxmaker/internal/oms"
	"fluxmaker/internal/risk"
	"fluxmaker/internal/runtimeops"
	"fluxmaker/internal/strategy"
	"fluxmaker/internal/tradesim"
	"fluxmaker/internal/venue"
)

type PriceSource interface {
	Price(ctx context.Context, instrument config.InstrumentConfig) (domain.ReferencePrice, error)
}

type RuntimeStore interface {
	Publish(ctx context.Context, snapshot runtimeops.InstrumentSnapshot) error
	Get(ctx context.Context, instrumentID string) (runtimeops.InstrumentSnapshot, error)
	Paused(ctx context.Context) (map[string]runtimeops.PauseState, error)
}

type MarketLocker interface {
	AcquireMarketLease(ctx context.Context, key, owner string, ttl time.Duration) (uint64, error)
	ValidateMarketLease(ctx context.Context, key, owner string, generation uint64) (bool, error)
	ReleaseMarketLease(ctx context.Context, key, owner string, generation uint64) error
}

type ReconcileController interface {
	Reconciles(ctx context.Context) (map[string]runtimeops.ReconcileRequest, error)
	ClearReconcile(ctx context.Context, instrumentID string) error
}

type TradingProgressReporter interface {
	ReportTradingProgress(ctx context.Context) error
}

type CyclePerformanceReporter interface {
	ReportCyclePerformance(ctx context.Context, performance runtimeops.CyclePerformance) error
}

type MetricsReporter interface {
	ReportMetrics(ctx context.Context, metrics runtimeops.MetricsSnapshot) error
}

type RuleChangeReporter interface {
	ReportRuleChange(ctx context.Context, change runtimeops.RuleChange) error
}

type SimulatedFillPublisher interface {
	AppendSimulatedFill(ctx context.Context, instrumentID string, fill domain.Fill) error
}

type Engine struct {
	cfg                     config.Config
	oracle                  PriceSource
	venues                  map[string]venue.Client
	audit                   *audit.Logger
	reconciler              *oms.Reconciler
	strategy                strategy.Generator
	risk                    risk.Engine
	logger                  *slog.Logger
	runtime                 RuntimeStore
	paused                  map[string]runtimeops.PauseState
	pauseApplied            map[string]bool
	fillCache               map[string]fillCacheEntry
	ownerID                 string
	heldLeases              map[string]uint64
	faults                  *fault.Manager
	lastReferenceValidUntil map[string]time.Time
	lastBookAt              map[string]time.Time
	lastInventory           map[string]num.Decimal
	startupFailures         map[string]string
	preflightBlocked        map[string]string
	tradeSimulator          *tradesim.Generator
	pauseMu                 sync.RWMutex // guards paused, pauseApplied
	fillMu                  sync.RWMutex // guards fillCache
	referenceMu             sync.RWMutex // guards lastReferenceValidUntil
	bookMu                  sync.RWMutex // guards lastBookAt
	inventoryMu             sync.RWMutex // guards lastInventory
	preflightMu             sync.RWMutex // guards startupFailures and preflightBlocked
	leaseMu                 sync.RWMutex // guards heldLeases
	instrumentAccountLocks  map[string][]*sync.Mutex
	metrics                 *engineMetrics
	logMu                   sync.Mutex
	lastCycleError          string
	lastCycleErrorAt        time.Time
}

type engineMetrics struct {
	mu                      sync.Mutex
	startedAt               time.Time
	cyclesTotal             uint64
	cycleFailuresTotal      uint64
	instrumentRunsTotal     uint64
	instrumentFailuresTotal uint64
	venueFaultEventsTotal   uint64
	omsPlacedTotal          uint64
	omsCanceledTotal        uint64
	simulatedTradesTotal    uint64
	auditFlushErrorsTotal   uint64
	ruleChangesTotal        uint64
	leaseFenceRejectsTotal  uint64
}

type fillCacheEntry struct {
	fills     []domain.Fill
	errorText string
	fetchedAt time.Time
}

// balanceCache deduplicates account balance lookups within a single engine
// cycle. Multiple instruments bound to the same credential otherwise each issue
// an identical account REST call every cycle. Same-account instruments are
// already serialized by account locks, so the first populates the cache under
// the lock and the rest reuse it; different accounts fetch in parallel.
type balanceCache struct {
	mu      sync.Mutex
	entries map[string]balanceCacheEntry
}

type balanceCacheEntry struct {
	balances []domain.Balance
	err      error
}

func newBalanceCache() *balanceCache {
	return &balanceCache{entries: map[string]balanceCacheEntry{}}
}

// fetch returns cached balances for the account key or performs a single
// lookup. The client call runs outside the lock so unrelated accounts are not
// serialized; correctness for a shared key relies on account locks preventing
// concurrent access to the same key.
func (c *balanceCache) fetch(ctx context.Context, key string, client venue.Client) ([]domain.Balance, error) {
	if c == nil {
		return client.Balances(ctx)
	}
	c.mu.Lock()
	entry, ok := c.entries[key]
	c.mu.Unlock()
	if ok {
		return entry.balances, entry.err
	}
	balances, err := client.Balances(ctx)
	c.mu.Lock()
	c.entries[key] = balanceCacheEntry{balances: balances, err: err}
	c.mu.Unlock()
	return balances, err
}

func accountCacheKey(venueType string, credentialID int64) string {
	return fmt.Sprintf("%s/%d", strings.ToLower(venueType), credentialID)
}

func New(cfg config.Config, oracle PriceSource, venues map[string]venue.Client, auditLog *audit.Logger, runtimeStore RuntimeStore, logger *slog.Logger) *Engine {
	return NewWithOwner(cfg, oracle, venues, auditLog, runtimeStore, logger, "")
}

func NewWithOwner(cfg config.Config, oracle PriceSource, venues map[string]venue.Client, auditLog *audit.Logger, runtimeStore RuntimeStore, logger *slog.Logger, ownerID string) *Engine {
	reconciler := oms.New()
	if stateStore, ok := runtimeStore.(oms.StateStore); ok {
		reconciler = oms.NewWithStateStore(stateStore)
	}
	faultManager := fault.New(cfg.MarketFailureThreshold, cfg.MarketRecoveryThreshold)
	if stateStore, ok := runtimeStore.(fault.StateStore); ok {
		faultManager = fault.NewWithStateStore(cfg.MarketFailureThreshold, cfg.MarketRecoveryThreshold, stateStore)
	}
	if ownerID == "" {
		ownerID = newOwnerID()
	}
	return &Engine{cfg: cfg, oracle: oracle, venues: venues, audit: auditLog, reconciler: reconciler, runtime: runtimeStore, paused: map[string]runtimeops.PauseState{}, pauseApplied: map[string]bool{}, fillCache: map[string]fillCacheEntry{}, logger: logger, ownerID: ownerID, heldLeases: map[string]uint64{}, faults: faultManager, lastReferenceValidUntil: map[string]time.Time{}, lastBookAt: map[string]time.Time{}, lastInventory: map[string]num.Decimal{}, startupFailures: map[string]string{}, preflightBlocked: map[string]string{}, tradeSimulator: tradesim.New(), instrumentAccountLocks: buildInstrumentAccountLocks(cfg), metrics: &engineMetrics{startedAt: time.Now().UTC()}}
}

func (e *Engine) OwnerID() string { return e.ownerID }

// SetStartupFailures installs per-instrument dependency failures found while
// building venue clients or synchronizing exchange rules. Prepare augments
// these with reference/book/account checks without rejecting healthy peers.
func (e *Engine) SetStartupFailures(failures map[string]string) {
	e.preflightMu.Lock()
	defer e.preflightMu.Unlock()
	e.startupFailures = make(map[string]string, len(failures))
	for instrumentID, failure := range failures {
		if strings.TrimSpace(failure) != "" {
			e.startupFailures[instrumentID] = failure
		}
	}
}

func (e *Engine) BlockedInstruments() map[string]string {
	e.preflightMu.RLock()
	defer e.preflightMu.RUnlock()
	result := make(map[string]string, len(e.preflightBlocked))
	for instrumentID, failure := range e.preflightBlocked {
		result[instrumentID] = failure
	}
	return result
}

func (e *Engine) startupFailure(instrumentID string) string {
	e.preflightMu.RLock()
	defer e.preflightMu.RUnlock()
	return e.startupFailures[instrumentID]
}

func (e *Engine) preflightFailure(instrumentID string) (string, bool) {
	e.preflightMu.RLock()
	defer e.preflightMu.RUnlock()
	failure, blocked := e.preflightBlocked[instrumentID]
	return failure, blocked
}

// EffectiveConfig includes exchange-owned trading rules learned at runtime.
// The scheduler uses it when comparing a later published configuration so a
// previously acknowledged rule change does not cause a redundant cleanup.
func (e *Engine) EffectiveConfig() config.Config { return e.cfg }

// ApplyParameters hot-swaps strategy, trade-simulation and live scalar tuning
// from a newly published configuration into the running engine without
// rebuilding venue clients, re-syncing exchange rules, or canceling orders. It
// is only valid for non-structural changes (configdiff.Plan.Structural == false)
// and, like the runtime rule refresh, must be called from the single scheduler
// goroutine between cycles so it never races the worker pool's reads of e.cfg.
// Exchange-synced venue rules already held in e.cfg are deliberately preserved.
func (e *Engine) ApplyParameters(next config.Config) {
	byID := make(map[string]config.InstrumentConfig, len(next.Instruments))
	for _, instrument := range next.Instruments {
		byID[instrument.ID] = instrument
	}
	instruments := make([]config.InstrumentConfig, len(e.cfg.Instruments))
	for i, current := range e.cfg.Instruments {
		if updated, ok := byID[current.ID]; ok {
			strategy := updated.Strategy
			// Mirror applyRuntimeSafetyDefaults so a hot-applied strategy gets the
			// same guardrails the full build path would have supplied.
			if strategy.MaxVenueReferenceDeviationBPS == 0 {
				strategy.MaxVenueReferenceDeviationBPS = 500
			}
			if strategy.MaxVenueSpreadBPS == 0 {
				strategy.MaxVenueSpreadBPS = 1000
			}
			current.Strategy = strategy
			current.TradeSimulation = updated.TradeSimulation
		}
		instruments[i] = current
	}
	e.cfg.Instruments = instruments
	if next.PollIntervalMS > 0 {
		e.cfg.PollIntervalMS = next.PollIntervalMS
	}
	if next.MaxConcurrentInstruments > 0 {
		e.cfg.MaxConcurrentInstruments = next.MaxConcurrentInstruments
	}
	if next.RulesRefreshSeconds > 0 {
		e.cfg.RulesRefreshSeconds = next.RulesRefreshSeconds
	}
	if next.MarketFailureThreshold > 0 {
		e.cfg.MarketFailureThreshold = next.MarketFailureThreshold
	}
	if next.MarketRecoveryThreshold > 0 {
		e.cfg.MarketRecoveryThreshold = next.MarketRecoveryThreshold
	}
	if next.MarketErrorGraceSeconds > 0 {
		e.cfg.MarketErrorGraceSeconds = next.MarketErrorGraceSeconds
	}
	if next.TradingProgressTimeoutSeconds > 0 {
		e.cfg.TradingProgressTimeoutSeconds = next.TradingProgressTimeoutSeconds
	}
}

// InheritMetricsFrom keeps Prometheus counters monotonic across non-disruptive
// configuration hot swaps within the same engine process.
func (e *Engine) InheritMetricsFrom(previous *Engine) {
	if previous != nil && previous.metrics != nil {
		e.metrics = previous.metrics
	}
}

func (e *Engine) RunOnce(ctx context.Context) error {
	started := time.Now()
	startedAt := started.UTC()
	cycleID := fmt.Sprintf("cycle-%d", startedAt.UnixNano())
	instruments := e.cfg.Instruments
	limit := e.cfg.MaxConcurrentInstruments
	if limit <= 0 {
		limit = 4
	}
	if limit > len(instruments) {
		limit = len(instruments)
	}
	if limit < 1 {
		limit = 1
	}
	errorsByInstrument := make([]error, len(instruments))
	balances := newBalanceCache()
	jobs := make(chan int)
	var workers sync.WaitGroup
	for worker := 0; worker < limit; worker++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for index := range jobs {
				instrument := instruments[index]
				errorsByInstrument[index] = e.runInstrumentGuarded(ctx, instrument, cycleID, balances)
				if reporter, ok := e.runtime.(TradingProgressReporter); ok {
					_ = reporter.ReportTradingProgress(ctx)
				}
			}
		}()
	}
	for index := range instruments {
		jobs <- index
	}
	close(jobs)
	workers.Wait()

	var failures []string
	succeeded := 0
	for index, err := range errorsByInstrument {
		if err != nil {
			failures = append(failures, instruments[index].ID+": "+err.Error())
		} else {
			succeeded++
		}
	}
	if flushErr := e.audit.Flush(); flushErr != nil {
		e.metrics.recordAuditFlushError()
		e.logger.Error("flush audit events failed", "error", flushErr)
	}
	e.metrics.recordCycle(len(instruments), len(failures))
	if reporter, ok := e.runtime.(CyclePerformanceReporter); ok {
		_ = reporter.ReportCyclePerformance(ctx, runtimeops.CyclePerformance{StartedAt: startedAt, DurationMS: time.Since(started).Milliseconds(), Instruments: len(instruments), Succeeded: succeeded, Failed: len(failures), ConcurrentLimit: limit})
	}
	if reporter, ok := e.runtime.(MetricsReporter); ok {
		_ = reporter.ReportMetrics(ctx, e.metrics.snapshot(e.audit.PendingCount()))
	}
	e.logCycle(cycleID, time.Since(started), failures)
	if len(failures) > 0 {
		return fmt.Errorf("tick failures: %s", strings.Join(failures, "; "))
	}
	return nil
}

// runInstrumentGuarded runs one instrument's cycle under its account locks and a
// panic barrier. A panic or bug in one pair is converted into that pair's own
// error; it never crashes the engine goroutine or disturbs the other pairs, so
// instrument isolation holds even for programming faults.
func (e *Engine) runInstrumentGuarded(ctx context.Context, instrument config.InstrumentConfig, cycleID string, balances *balanceCache) (runErr error) {
	unlock := e.lockInstrumentAccounts(instrument.ID)
	defer unlock()
	defer func() {
		if r := recover(); r != nil {
			runErr = fmt.Errorf("panic: %v", r)
			e.logger.Error("instrument cycle panicked", "instrument", instrument.ID, "panic", r, "stack", string(debug.Stack()))
		}
	}()
	// A closed (paused) instrument wins over a preflight block: once the operator
	// turns a pair off it must stop reporting failures, even if its venue is
	// unreachable. runPausedInstrument is a best-effort read-only refresh that
	// never degrades the cycle.
	if e.isPaused(instrument.ID) {
		return e.runPausedInstrument(ctx, instrument, balances)
	}
	if failure, blocked := e.preflightFailure(instrument.ID); blocked {
		return e.publishPreflightBlocked(ctx, instrument, failure)
	}
	return e.runInstrument(ctx, instrument, cycleID, balances)
}

// Prepare verifies candidate dependencies without placing or canceling orders.
// Failures are isolated by instrument: a healthy instrument may activate even
// when another one is degraded. Only a candidate with no runnable instruments
// is rejected as a whole.
func (e *Engine) Prepare(ctx context.Context) error {
	type result struct {
		instrumentID string
		err          error
	}
	results := make(chan result, len(e.cfg.Instruments))
	var workers sync.WaitGroup
	for _, instrument := range e.cfg.Instruments {
		instrument := instrument
		workers.Add(1)
		go func() {
			defer workers.Done()
			unlock := e.lockInstrumentAccounts(instrument.ID)
			defer unlock()
			results <- result{instrumentID: instrument.ID, err: e.prepareInstrument(ctx, instrument, e.startupFailure(instrument.ID))}
		}()
	}
	workers.Wait()
	close(results)

	blocked := make(map[string]string)
	ready := 0
	for result := range results {
		if result.err != nil {
			blocked[result.instrumentID] = result.err.Error()
		} else {
			ready++
		}
	}
	e.preflightMu.Lock()
	e.preflightBlocked = blocked
	e.preflightMu.Unlock()
	// ready > 0: at least one instrument is runnable. No instruments at all: a
	// valid idle configuration (nothing to prepare). Only fail when instruments
	// exist but none of them could be prepared.
	if ready > 0 || len(e.cfg.Instruments) == 0 {
		return nil
	}
	failures := make([]string, 0, len(blocked))
	for instrumentID, failure := range blocked {
		failures = append(failures, instrumentID+": "+failure)
	}
	sort.Strings(failures)
	return fmt.Errorf("candidate preflight: no runnable instruments: %s", strings.Join(failures, "; "))
}

func (e *Engine) prepareInstrument(ctx context.Context, instrument config.InstrumentConfig, startupFailure string) error {
	var failures []string
	if startupFailure != "" {
		failures = append(failures, "startup: "+startupFailure)
	}
	ref, err := e.oracle.Price(ctx, instrument)
	if err != nil {
		failures = append(failures, "reference: "+err.Error())
	} else {
		e.setLastReferenceValidUntil(instrument.ID, ref.ValidUntil)
	}
	// A client/rule construction failure makes exchange precision untrusted.
	// Still check the independent reference source above, but never continue to
	// quote or query the affected account with fallback rules.
	if startupFailure != "" || err != nil {
		return joinedError(failures)
	}

	activeMarkets := 0
	for venueName, venueCfg := range e.cfg.Venues {
		if !venueCfg.Enabled {
			continue
		}
		market, ok := venueCfg.Markets[instrument.ID]
		if !ok {
			continue
		}
		activeMarkets++
		client := e.venues[venue.ClientKey(venueName, instrument.ID)]
		if client == nil {
			failures = append(failures, venueName+": client missing")
			continue
		}
		book, bookErr := client.TopBook(ctx, market.Symbol)
		if bookErr == nil {
			e.setLastBookAt(e.marketFaultKey(venueName, instrument.ID, market), time.Now().UTC())
		} else {
			// Public depth is advisory for a reference-price market maker. A new
			// market may be empty and a venue feed may be temporarily unavailable;
			// neither prevents us from validating an index-anchored Post-Only plan.
			book = domain.Book{Venue: venueName, Symbol: market.Symbol}
		}
		if protectionErr := risk.ValidateMarketReference(ref, book, instrument.Strategy); protectionErr != nil {
			failures = append(failures, venueName+" price protection: "+protectionErr.Error())
			continue
		}
		quotes, quoteErr := e.strategy.Generate(instrument, venueName, market, ref, book, instrument.Strategy.TargetBase)
		if quoteErr != nil {
			failures = append(failures, venueName+" strategy: "+quoteErr.Error())
			continue
		}
		quotes, quoteErr = e.risk.FilterQuotes(instrument, market, book, instrument.Strategy.TargetBase, quotes)
		if quoteErr != nil {
			failures = append(failures, venueName+" risk: "+quoteErr.Error())
			continue
		}
		if e.cfg.Mode != domain.ModeLive || !venueCfg.TradingEnabled {
			continue
		}
		balances, balanceErr := client.Balances(ctx)
		if balanceErr != nil {
			failures = append(failures, venueName+" account: "+balanceErr.Error())
			continue
		}
		orders, ordersErr := client.OpenOrders(ctx, market.Symbol)
		if ordersErr != nil {
			failures = append(failures, venueName+" orders: "+ordersErr.Error())
			continue
		}
		managed := oms.ManagedOrdersFor(client, orders)
		quotes = risk.ApplyOrderLimit(quotes, len(orders), len(managed), market.MaxOpenOrders)
		baseFree, quoteFree := num.Decimal{}, num.Decimal{}
		if balance := findBalance(balances, market.BaseAsset); balance != nil {
			baseFree = balance.Free
		}
		if balance := findBalance(balances, market.QuoteAsset); balance != nil {
			quoteFree = balance.Free
		}
		quotes, _ = risk.ApplyBalanceBudget(quotes, managed, baseFree, quoteFree, instrument.Strategy.BalanceReserveBPS, market.MaxBaseCommitment, market.MaxQuoteCommitment)
		if len(quotes) == 0 {
			failures = append(failures, venueName+" budget allows no orders")
		}
	}
	if activeMarkets == 0 {
		failures = append(failures, "no enabled venue markets")
	}
	return joinedError(failures)
}

func joinedError(failures []string) error {
	if len(failures) == 0 {
		return nil
	}
	return fmt.Errorf("%s", strings.Join(failures, "; "))
}

// RetryBlocked re-runs only the dependencies and preflight checks belonging to
// currently blocked instruments. It never cancels or places orders. Successful
// instruments are left untouched and resume on the next normal engine cycle.
func (e *Engine) RetryBlocked(ctx context.Context) (int, error) {
	blocked := e.BlockedInstruments()
	if len(blocked) == 0 {
		return 0, nil
	}
	instruments := make(map[string]config.InstrumentConfig, len(e.cfg.Instruments))
	for _, instrument := range e.cfg.Instruments {
		instruments[instrument.ID] = instrument
	}
	ids := make([]string, 0, len(blocked))
	for instrumentID := range blocked {
		ids = append(ids, instrumentID)
	}
	sort.Strings(ids)

	recovered := 0
	var failures []string
	for _, instrumentID := range ids {
		instrument, ok := instruments[instrumentID]
		if !ok {
			failures = append(failures, instrumentID+": instrument configuration missing")
			continue
		}
		unlock := e.lockInstrumentAccounts(instrumentID)
		rulesErr := e.refreshInstrumentRules(ctx, instrumentID)
		startupFailure := ""
		if rulesErr != nil {
			startupFailure = "rule refresh: " + rulesErr.Error()
		} else {
			e.preflightMu.Lock()
			delete(e.startupFailures, instrumentID)
			e.preflightMu.Unlock()
		}
		prepareErr := e.prepareInstrument(ctx, instrument, startupFailure)
		unlock()

		e.preflightMu.Lock()
		if prepareErr == nil {
			delete(e.preflightBlocked, instrumentID)
			delete(e.startupFailures, instrumentID)
			recovered++
		} else {
			e.preflightBlocked[instrumentID] = prepareErr.Error()
		}
		e.preflightMu.Unlock()
		if prepareErr != nil {
			failures = append(failures, instrumentID+": "+prepareErr.Error())
		}
	}
	if len(failures) > 0 {
		return recovered, fmt.Errorf("blocked instrument retry: %s", strings.Join(failures, "; "))
	}
	return recovered, nil
}

func (e *Engine) refreshInstrumentRules(ctx context.Context, instrumentID string) error {
	activeMarkets := 0
	var failures []string
	for venueName, venueCfg := range e.cfg.Venues {
		if !venueCfg.Enabled {
			continue
		}
		market, ok := venueCfg.Markets[instrumentID]
		if !ok {
			continue
		}
		activeMarkets++
		client := e.venues[venue.ClientKey(venueName, instrumentID)]
		if client == nil {
			failures = append(failures, venueName+": client unavailable")
			continue
		}
		reader, ok := client.(venue.RuleReader)
		if !ok {
			failures = append(failures, venueName+": trading rules unavailable")
			continue
		}
		rules, err := reader.MarketRules(ctx, market.Symbol)
		if err != nil {
			failures = append(failures, venueName+": load trading rules: "+err.Error())
			continue
		}
		if rules.BaseAsset != "" && !strings.EqualFold(rules.BaseAsset, market.BaseAsset) {
			failures = append(failures, fmt.Sprintf("%s: base asset %s does not match exchange %s", venueName, market.BaseAsset, rules.BaseAsset))
			continue
		}
		if rules.QuoteAsset != "" && !strings.EqualFold(rules.QuoteAsset, market.QuoteAsset) {
			failures = append(failures, fmt.Sprintf("%s: quote asset %s does not match exchange %s", venueName, market.QuoteAsset, rules.QuoteAsset))
			continue
		}
		if !rules.PriceTick.IsPositive() || !rules.QuantityStep.IsPositive() {
			failures = append(failures, venueName+": exchange returned non-positive price or quantity precision")
			continue
		}
		market = applyMarketRules(market, rules)
		venueCfg.Markets[instrumentID] = market
		e.cfg.Venues[venueName] = venueCfg
	}
	if activeMarkets == 0 {
		failures = append(failures, "no enabled venue markets")
	}
	return joinedError(failures)
}

func (e *Engine) publishPreflightBlocked(ctx context.Context, instrument config.InstrumentConfig, failure string) error {
	snapshot := e.newSnapshot(instrument)
	now := time.Now().UTC()
	snapshot.Status = "degraded"
	snapshot.Error = "startup preflight: " + failure
	snapshot.UpdatedAt = now
	for venueName, venueCfg := range e.cfg.Venues {
		if !venueCfg.Enabled {
			continue
		}
		market, ok := venueCfg.Markets[instrument.ID]
		if !ok {
			continue
		}
		rules := marketRulesFromConfig(market)
		snapshot.Venues = append(snapshot.Venues, runtimeops.VenueSnapshot{
			Name:           venueName,
			Type:           venueCfg.Type,
			Symbol:         market.Symbol,
			TradingEnabled: venueCfg.TradingEnabled,
			Rules:          &rules,
			OpenOrders:     []domain.Order{},
			Fills:          []domain.Fill{},
			Error:          failure,
			UpdatedAt:      now,
		})
	}
	sort.Slice(snapshot.Venues, func(i, j int) bool { return snapshot.Venues[i].Name < snapshot.Venues[j].Name })
	if e.runtime != nil {
		if err := e.runtime.Publish(ctx, snapshot); err != nil {
			return fmt.Errorf("preflight blocked (%s); publish state: %w", failure, err)
		}
	}
	return fmt.Errorf("preflight blocked: %s", failure)
}

// RefreshMarketRules reloads venue filters without rebuilding the runtime. It
// only changes the affected market's in-memory rules; the next normal OMS cycle
// converges that market and no unrelated market is canceled.
func (e *Engine) RefreshMarketRules(ctx context.Context) (int, error) {
	changes := 0
	var failures []string
	for venueName, venueCfg := range e.cfg.Venues {
		if !venueCfg.Enabled {
			continue
		}
		for instrumentID, market := range venueCfg.Markets {
			client := e.venues[venue.ClientKey(venueName, instrumentID)]
			reader, ok := client.(venue.RuleReader)
			if !ok {
				failures = append(failures, venueName+"/"+instrumentID+": trading rules unavailable")
				continue
			}
			rules, err := reader.MarketRules(ctx, market.Symbol)
			if err != nil {
				failures = append(failures, venueName+"/"+instrumentID+": "+err.Error())
				continue
			}
			if rules.BaseAsset != "" && !strings.EqualFold(rules.BaseAsset, market.BaseAsset) {
				failures = append(failures, fmt.Sprintf("%s/%s: base asset changed from %s to %s", venueName, instrumentID, market.BaseAsset, rules.BaseAsset))
				continue
			}
			if rules.QuoteAsset != "" && !strings.EqualFold(rules.QuoteAsset, market.QuoteAsset) {
				failures = append(failures, fmt.Sprintf("%s/%s: quote asset changed from %s to %s", venueName, instrumentID, market.QuoteAsset, rules.QuoteAsset))
				continue
			}
			previous := marketRulesFromConfig(market)
			market = applyMarketRules(market, rules)
			current := marketRulesFromConfig(market)
			if equalMarketRules(previous, current) {
				continue
			}
			venueCfg.Markets[instrumentID] = market
			change := runtimeops.RuleChange{InstrumentID: instrumentID, Venue: venueName, Symbol: market.Symbol, Previous: previous, Current: current, DetectedAt: time.Now().UTC()}
			if reporter, ok := e.runtime.(RuleChangeReporter); ok {
				if err := reporter.ReportRuleChange(ctx, change); err != nil {
					failures = append(failures, venueName+"/"+instrumentID+" alert: "+err.Error())
				}
			}
			e.metrics.recordRuleChange()
			_ = e.audit.Record("trading_rules_changed", change)
			e.logger.Warn("trading rules changed", "instrument", instrumentID, "venue", venueName, "symbol", market.Symbol, "previous", previous, "current", current)
			changes++
		}
		e.cfg.Venues[venueName] = venueCfg
	}
	if err := e.audit.Flush(); err != nil {
		e.metrics.recordAuditFlushError()
		failures = append(failures, "audit: "+err.Error())
	}
	if len(failures) > 0 {
		return changes, fmt.Errorf("refresh trading rules: %s", strings.Join(failures, "; "))
	}
	return changes, nil
}

func marketRulesFromConfig(market config.VenueMarketConfig) domain.MarketRules {
	return domain.MarketRules{
		Symbol: market.Symbol, BaseAsset: market.BaseAsset, QuoteAsset: market.QuoteAsset,
		PriceTick: market.PriceTick, QuantityStep: market.QuantityStep,
		MinQuantity: market.MinQuantity, MaxQuantity: market.MaxQuantity,
		MinNotional: market.MinNotional, MaxNotional: market.MaxNotional,
		MinPrice: market.MinPrice, MaxPrice: market.MaxPrice, MaxOpenOrders: market.MaxOpenOrders,
	}
}

func applyMarketRules(market config.VenueMarketConfig, rules domain.MarketRules) config.VenueMarketConfig {
	market.PriceTick = rules.PriceTick
	market.QuantityStep = rules.QuantityStep
	if rules.MinNotional.IsPositive() {
		market.MinNotional = rules.MinNotional
	}
	market.MinQuantity = rules.MinQuantity
	market.MaxQuantity = rules.MaxQuantity
	market.MaxNotional = rules.MaxNotional
	market.MinPrice = rules.MinPrice
	market.MaxPrice = rules.MaxPrice
	market.MaxOpenOrders = rules.MaxOpenOrders
	return market
}

func equalMarketRules(a, b domain.MarketRules) bool {
	return strings.EqualFold(a.Symbol, b.Symbol) && strings.EqualFold(a.BaseAsset, b.BaseAsset) && strings.EqualFold(a.QuoteAsset, b.QuoteAsset) &&
		a.PriceTick.Cmp(b.PriceTick) == 0 && a.QuantityStep.Cmp(b.QuantityStep) == 0 &&
		a.MinQuantity.Cmp(b.MinQuantity) == 0 && a.MaxQuantity.Cmp(b.MaxQuantity) == 0 &&
		a.MinNotional.Cmp(b.MinNotional) == 0 && a.MaxNotional.Cmp(b.MaxNotional) == 0 &&
		a.MinPrice.Cmp(b.MinPrice) == 0 && a.MaxPrice.Cmp(b.MaxPrice) == 0 && a.MaxOpenOrders == b.MaxOpenOrders
}

func (e *Engine) ApplyControls(ctx context.Context) error {
	if e.runtime == nil {
		return nil
	}
	paused, err := e.runtime.Paused(ctx)
	if err != nil {
		return err
	}
	var failures []string
	for _, instrument := range e.cfg.Instruments {
		state, shouldPause := paused[instrument.ID]
		if !shouldPause {
			e.pauseMu.Lock()
			_, wasPaused := e.paused[instrument.ID]
			delete(e.paused, instrument.ID)
			delete(e.pauseApplied, instrument.ID)
			e.pauseMu.Unlock()
			if wasPaused {
				if err := e.clearInstrumentBlocks(ctx, instrument); err != nil {
					failures = append(failures, instrument.ID+" unblock: "+err.Error())
				}
			}
			continue
		}
		e.pauseMu.Lock()
		e.paused[instrument.ID] = state
		alreadyApplied := e.pauseApplied[instrument.ID]
		e.pauseMu.Unlock()
		if alreadyApplied {
			continue
		}
		if state.Reason != runtimeops.ReasonEmergencyCancel {
			// Soft close: stop quoting but leave any resting orders untouched.
			// Applies immediately and does not depend on venue reachability.
			e.pauseMu.Lock()
			e.pauseApplied[instrument.ID] = true
			e.pauseMu.Unlock()
			_ = e.audit.Record("instrument_paused", map[string]any{"instrument": instrument.ID, "reason": state.Reason, "requested_by": state.RequestedBy, "orders_retained": true})
			e.publishPaused(ctx, instrument, "", nil)
			continue
		}
		cancelErr := e.cancelInstrument(ctx, instrument)
		confirmed := false
		var ordersByVenue map[string][]domain.Order
		if cancelErr == nil {
			ordersByVenue, confirmed, cancelErr = e.instrumentCancellationConfirmed(ctx, instrument)
		}
		if confirmed && cancelErr == nil {
			e.pauseMu.Lock()
			e.pauseApplied[instrument.ID] = true
			e.pauseMu.Unlock()
			_ = e.audit.Record("instrument_paused", map[string]any{"instrument": instrument.ID, "reason": state.Reason, "requested_by": state.RequestedBy})
		} else {
			if cancelErr != nil {
				failures = append(failures, instrument.ID+": "+cancelErr.Error())
			}
		}
		e.publishPaused(ctx, instrument, errorString(cancelErr), ordersByVenue)
	}
	if controller, ok := e.runtime.(ReconcileController); ok {
		requests, err := controller.Reconciles(ctx)
		if err != nil {
			failures = append(failures, "load reconcile controls: "+err.Error())
		} else {
			for _, instrument := range e.cfg.Instruments {
				request, requested := requests[instrument.ID]
				if !requested {
					continue
				}
				if err := e.cancelInstrument(ctx, instrument); err != nil {
					failures = append(failures, instrument.ID+" reconcile cancel: "+err.Error())
					continue
				}
				if err := e.clearInstrumentBlocks(ctx, instrument); err != nil {
					failures = append(failures, instrument.ID+" reconcile unblock: "+err.Error())
					continue
				}
				if err := controller.ClearReconcile(ctx, instrument.ID); err != nil {
					failures = append(failures, instrument.ID+" clear reconcile control: "+err.Error())
					continue
				}
				_ = e.audit.Record("instrument_reconciled", map[string]any{"instrument": instrument.ID, "requested_by": request.RequestedBy, "requested_at": request.RequestedAt})
			}
		}
	}
	if len(failures) > 0 {
		_ = e.audit.Flush()
		return fmt.Errorf("apply controls: %s", strings.Join(failures, "; "))
	}
	_ = e.audit.Flush()
	return nil
}

func (e *Engine) clearInstrumentBlocks(ctx context.Context, instrument config.InstrumentConfig) error {
	var failures []string
	for venueName, venueCfg := range e.cfg.Venues {
		market, ok := venueCfg.Markets[instrument.ID]
		if !ok || !venueCfg.Enabled || !venueCfg.TradingEnabled {
			continue
		}
		client := e.venues[venue.ClientKey(venueName, instrument.ID)]
		if client == nil {
			continue
		}
		if err := e.reconciler.ClearBlocked(ctx, client, instrument.ID); err != nil {
			failures = append(failures, venueName+"/"+market.Symbol+": "+err.Error())
		}
		if err := e.faults.ResetWithContext(ctx, e.marketFaultKey(venueName, instrument.ID, market)); err != nil {
			failures = append(failures, venueName+"/"+market.Symbol+" fault reset: "+err.Error())
		}
	}
	if len(failures) > 0 {
		return fmt.Errorf("clear OMS blocks: %s", strings.Join(failures, "; "))
	}
	return nil
}

func (e *Engine) publishPaused(ctx context.Context, instrument config.InstrumentConfig, errorText string, ordersByVenue map[string][]domain.Order) {
	if e.runtime == nil {
		return
	}
	snapshot, _ := e.runtime.Get(ctx, instrument.ID)
	if snapshot.InstrumentID == "" {
		snapshot = e.newSnapshot(instrument)
	}
	e.pauseMu.RLock()
	state, ok := e.paused[instrument.ID]
	applied := e.pauseApplied[instrument.ID]
	e.pauseMu.RUnlock()
	if ok {
		snapshot.Pause = &state
	}
	snapshot.Paused = applied
	if applied {
		snapshot.Status = "paused"
	} else {
		snapshot.Status = "pausing"
	}
	for index := range snapshot.Venues {
		if orders, exists := ordersByVenue[snapshot.Venues[index].Name]; exists {
			snapshot.Venues[index].OpenOrders = orders
		}
	}
	snapshot.Error = errorText
	snapshot.UpdatedAt = time.Now().UTC()
	_ = e.runtime.Publish(ctx, snapshot)
}

// A pause disables every exchange mutation, but operators still need current
// balances, orders, fills and connection state to decide whether it is safe to
// resume. Rebuild the read-only portion of the snapshot on every normal engine
// cycle instead of indefinitely refreshing stale pre-pause data.
func (e *Engine) runPausedInstrument(ctx context.Context, instrument config.InstrumentConfig, balances *balanceCache) error {
	if e.runtime == nil {
		return nil
	}
	startedAt := time.Now()
	snapshot := e.newSnapshot(instrument)
	e.pauseMu.RLock()
	state, hasPause := e.paused[instrument.ID]
	applied := e.pauseApplied[instrument.ID]
	e.pauseMu.RUnlock()
	if hasPause {
		snapshot.Pause = &state
	}
	snapshot.Paused = applied
	if applied {
		snapshot.Status = "paused"
	} else {
		snapshot.Status = "pausing"
	}

	var failures []string
	if e.oracle != nil {
		referenceStartedAt := time.Now()
		ref, err := e.oracle.Price(ctx, instrument)
		snapshot.ReferenceDurationMS = time.Since(referenceStartedAt).Milliseconds()
		if err != nil {
			failures = append(failures, "reference: "+err.Error())
		} else {
			snapshot.Reference = &ref
		}
	}

	balanceStartedAt := time.Now()
	balancesByVenue, inventory, accountCount, balanceErrors := e.collectBalances(ctx, instrument, balances)
	snapshot.BalanceDurationMS = time.Since(balanceStartedAt).Milliseconds()
	if accountCount > 0 {
		snapshot.Inventory = inventory
		snapshot.InventoryAvailable = true
		e.setLastInventory(instrument.ID, inventory)
	} else if cached, ok := e.getLastInventory(instrument.ID); ok {
		snapshot.Inventory = cached
	}

	for venueName, venueCfg := range e.cfg.Venues {
		market, ok := venueCfg.Markets[instrument.ID]
		if !ok || !venueCfg.Enabled {
			continue
		}
		venueSnapshot := runtimeops.VenueSnapshot{
			Name:           venueName,
			Type:           venueCfg.Type,
			Symbol:         market.Symbol,
			TradingEnabled: venueCfg.TradingEnabled,
			OpenOrders:     []domain.Order{},
			Fills:          []domain.Fill{},
			Rules:          &domain.MarketRules{Symbol: market.Symbol, BaseAsset: market.BaseAsset, QuoteAsset: market.QuoteAsset, PriceTick: market.PriceTick, QuantityStep: market.QuantityStep, MinQuantity: market.MinQuantity, MaxQuantity: market.MaxQuantity, MinNotional: market.MinNotional, MaxNotional: market.MaxNotional, MinPrice: market.MinPrice, MaxPrice: market.MaxPrice, MaxOpenOrders: market.MaxOpenOrders},
			UpdatedAt:      time.Now().UTC(),
		}
		client := e.venues[venue.ClientKey(venueName, instrument.ID)]
		if client == nil {
			venueSnapshot.Error = "client missing"
			failures = append(failures, venueName+": client missing")
			snapshot.Venues = append(snapshot.Venues, venueSnapshot)
			continue
		}
		if balances, exists := balancesByVenue[venueName]; exists {
			venueSnapshot.AccountConnected = true
			venueSnapshot.BaseBalance = findBalance(balances, market.BaseAsset)
			venueSnapshot.QuoteBalance = findBalance(balances, market.QuoteAsset)
			if venueSnapshot.BaseBalance == nil {
				venueSnapshot.BaseBalance = &domain.Balance{Asset: market.BaseAsset}
			}
			if venueSnapshot.QuoteBalance == nil {
				venueSnapshot.QuoteBalance = &domain.Balance{Asset: market.QuoteAsset}
			}
		} else if balanceErr := balanceErrors[venueName]; balanceErr != nil {
			venueSnapshot.Error = appendError(venueSnapshot.Error, "balance: "+balanceErr.Error())
			failures = append(failures, venueName+" balance: "+balanceErr.Error())
		}

		bookStartedAt := time.Now()
		book, bookErr := client.TopBook(ctx, market.Symbol)
		venueSnapshot.BookDurationMS = time.Since(bookStartedAt).Milliseconds()
		if bookErr != nil {
			venueSnapshot.Error = appendError(venueSnapshot.Error, "book: "+bookErr.Error())
			failures = append(failures, venueName+" book: "+bookErr.Error())
		} else {
			venueSnapshot.MarketConnected = true
			venueSnapshot.Book = &book
		}

		if market.CredentialID > 0 {
			ordersStartedAt := time.Now()
			orders, ordersErr := client.OpenOrders(ctx, market.Symbol)
			venueSnapshot.OrdersDurationMS = time.Since(ordersStartedAt).Milliseconds()
			if ordersErr != nil {
				venueSnapshot.AccountConnected = false
				venueSnapshot.Error = appendError(venueSnapshot.Error, "orders: "+ordersErr.Error())
				failures = append(failures, venueName+" orders: "+ordersErr.Error())
			} else {
				venueSnapshot.AccountConnected = true
				venueSnapshot.OpenOrders = orders
			}
			fillsStartedAt := time.Now()
			fills, fillsErr := e.recentFills(ctx, venueName, instrument.ID, client, market.Symbol)
			venueSnapshot.FillsDurationMS = time.Since(fillsStartedAt).Milliseconds()
			if fillsErr != nil {
				venueSnapshot.Error = appendError(venueSnapshot.Error, "fills: "+fillsErr.Error())
				failures = append(failures, venueName+" fills: "+fillsErr.Error())
			} else {
				venueSnapshot.Fills = fills
			}
		}
		snapshot.Venues = append(snapshot.Venues, venueSnapshot)
	}

	snapshot.TickDurationMS = time.Since(startedAt).Milliseconds()
	snapshot.UpdatedAt = time.Now().UTC()
	if len(failures) > 0 {
		snapshot.Error = strings.Join(failures, "; ")
	}
	return e.runtime.Publish(ctx, snapshot)
}

func (e *Engine) instrumentCancellationConfirmed(ctx context.Context, instrument config.InstrumentConfig) (map[string][]domain.Order, bool, error) {
	ordersByVenue := make(map[string][]domain.Order)
	confirmed := true
	var failures []string
	for venueName, venueCfg := range e.cfg.Venues {
		if !venueCfg.Enabled || !venueCfg.TradingEnabled {
			continue
		}
		market, ok := venueCfg.Markets[instrument.ID]
		if !ok {
			continue
		}
		client := e.venues[venue.ClientKey(venueName, instrument.ID)]
		if client == nil {
			failures = append(failures, venueName+": client missing")
			confirmed = false
			continue
		}
		orders, err := client.OpenOrders(ctx, market.Symbol)
		if err != nil {
			failures = append(failures, venueName+": "+err.Error())
			confirmed = false
			continue
		}
		ordersByVenue[venueName] = orders
		if len(oms.ManagedOrdersFor(client, orders)) > 0 {
			confirmed = false
		}
	}
	if len(failures) > 0 {
		return ordersByVenue, false, fmt.Errorf("confirm cancellations: %s", strings.Join(failures, "; "))
	}
	return ordersByVenue, confirmed, nil
}

func (e *Engine) runInstrument(ctx context.Context, instrument config.InstrumentConfig, cycleID string, balances *balanceCache) (runErr error) {
	startedAt := time.Now()
	snapshot := e.newSnapshot(instrument)
	defer func() {
		snapshot.TickDurationMS = time.Since(startedAt).Milliseconds()
		snapshot.UpdatedAt = time.Now().UTC()
		if runErr != nil {
			snapshot.Status = "degraded"
			snapshot.Error = runErr.Error()
		}
		if e.runtime != nil {
			_ = e.runtime.Publish(ctx, snapshot)
		}
	}()

	referenceStartedAt := time.Now()
	ref, err := e.oracle.Price(ctx, instrument)
	snapshot.ReferenceDurationMS = time.Since(referenceStartedAt).Milliseconds()
	if err != nil {
		_ = e.audit.Record("reference_rejected", map[string]any{"instrument": instrument.ID, "error": err.Error()})
		if e.cfg.Mode == domain.ModeLive {
			lastValidUntil := e.getLastReferenceValidUntil(instrument.ID)
			stale := lastValidUntil.IsZero() || time.Now().After(lastValidUntil)
			for venueName, venueCfg := range e.cfg.Venues {
				market, ok := venueCfg.Markets[instrument.ID]
				if !ok || !venueCfg.Enabled || !venueCfg.TradingEnabled {
					continue
				}
				client := e.venues[venue.ClientKey(venueName, instrument.ID)]
				if client == nil {
					continue
				}
				state, cancelErr := e.markVenueFailure(ctx, instrument.ID, venueName, venueCfg, market, client, "reference", err, stale)
				snapshot.Venues = append(snapshot.Venues, runtimeops.VenueSnapshot{Name: venueName, Type: venueCfg.Type, Symbol: market.Symbol, TradingEnabled: venueCfg.TradingEnabled, Fault: &state, Error: appendError(err.Error(), errorString(cancelErr)), UpdatedAt: time.Now().UTC()})
			}
		}
		return err
	}
	e.setLastReferenceValidUntil(instrument.ID, ref.ValidUntil)
	snapshot.Reference = &ref
	strategyInventory := instrument.Strategy.TargetBase
	balanceStartedAt := time.Now()
	balancesByVenue, accountInventory, accountCount, balanceErrors := e.collectBalances(ctx, instrument, balances)
	snapshot.BalanceDurationMS = time.Since(balanceStartedAt).Milliseconds()
	if accountCount > 0 {
		snapshot.Inventory = accountInventory
		snapshot.InventoryAvailable = true
	}
	if e.cfg.Mode == domain.ModeLive {
		if len(balanceErrors) == 0 && accountCount > 0 {
			strategyInventory = accountInventory
			e.setLastInventory(instrument.ID, accountInventory)
		} else if cached, ok := e.getLastInventory(instrument.ID); ok {
			strategyInventory = cached
		}
	}
	_ = e.audit.Record("reference_price", ref)

	var failures []string
	activeMarkets := 0
	for venueName, venueCfg := range e.cfg.Venues {
		if !venueCfg.Enabled {
			continue
		}
		market, ok := venueCfg.Markets[instrument.ID]
		if !ok {
			continue
		}
		activeMarkets++
		venueSnapshot := runtimeops.VenueSnapshot{Name: venueName, Type: venueCfg.Type, Symbol: market.Symbol, TradingEnabled: venueCfg.TradingEnabled, OpenOrders: []domain.Order{}, Fills: []domain.Fill{}, UpdatedAt: time.Now().UTC()}
		venueSnapshot.Rules = &domain.MarketRules{Symbol: market.Symbol, BaseAsset: market.BaseAsset, QuoteAsset: market.QuoteAsset, PriceTick: market.PriceTick, QuantityStep: market.QuantityStep, MinQuantity: market.MinQuantity, MaxQuantity: market.MaxQuantity, MinNotional: market.MinNotional, MaxNotional: market.MaxNotional, MinPrice: market.MinPrice, MaxPrice: market.MaxPrice, MaxOpenOrders: market.MaxOpenOrders}
		client := e.venues[venue.ClientKey(venueName, instrument.ID)]
		if client == nil {
			failures = append(failures, venueName+": missing client")
			venueSnapshot.Error = "client missing"
			snapshot.Venues = append(snapshot.Venues, venueSnapshot)
			continue
		}
		if balanceErr := balanceErrors[venueName]; balanceErr != nil && e.cfg.Mode == domain.ModeLive && venueCfg.TradingEnabled {
			state, cancelErr := e.markVenueFailure(ctx, instrument.ID, venueName, venueCfg, market, client, "balance", balanceErr, false)
			venueSnapshot.Fault = &state
			venueSnapshot.Error = appendError("balance: "+balanceErr.Error(), errorString(cancelErr))
			failures = append(failures, venueName+" balance: "+balanceErr.Error())
			snapshot.Venues = append(snapshot.Venues, venueSnapshot)
			continue
		}
		if balances, ok := balancesByVenue[venueName]; ok {
			venueSnapshot.AccountConnected = true
			venueSnapshot.BaseBalance = findBalance(balances, market.BaseAsset)
			venueSnapshot.QuoteBalance = findBalance(balances, market.QuoteAsset)
			if venueSnapshot.BaseBalance == nil {
				venueSnapshot.BaseBalance = &domain.Balance{Asset: market.BaseAsset}
			}
			if venueSnapshot.QuoteBalance == nil {
				venueSnapshot.QuoteBalance = &domain.Balance{Asset: market.QuoteAsset}
			}
		} else if balanceErr := balanceErrors[venueName]; balanceErr != nil {
			venueSnapshot.Error = appendError(venueSnapshot.Error, "balance: "+balanceErr.Error())
		}
		bookStartedAt := time.Now()
		book, bookErr := client.TopBook(ctx, market.Symbol)
		venueSnapshot.BookDurationMS = time.Since(bookStartedAt).Milliseconds()
		if bookErr != nil {
			// Quoting remains anchored to the external reference. Keep the public
			// market-data outage visible to operators, but do not block or withdraw
			// liquidity solely because an empty/new market has no usable top book.
			venueSnapshot.Error = appendError(venueSnapshot.Error, "盘口不可用，按指数价铺单: "+bookErr.Error())
			book = domain.Book{Venue: venueName, Symbol: market.Symbol}
		} else {
			venueSnapshot.MarketConnected = true
			venueSnapshot.Book = &book
			e.setLastBookAt(e.marketFaultKey(venueName, instrument.ID, market), time.Now().UTC())
		}
		if err := risk.ValidateMarketReference(ref, book, instrument.Strategy); err != nil {
			state, cancelErr := e.markVenueFailure(ctx, instrument.ID, venueName, venueCfg, market, client, "market_reference", err, true)
			venueSnapshot.Fault = &state
			failures = append(failures, venueName+" price protection: "+err.Error())
			venueSnapshot.Error = appendError(venueSnapshot.Error, appendError("price protection: "+err.Error(), errorString(cancelErr)))
			snapshot.Venues = append(snapshot.Venues, venueSnapshot)
			continue
		}
		if instrument.TradeSimulation.Enabled && venueName == instrument.TradeSimulation.SourceVenue {
			simulation, generated := e.tradeSimulator.Observe(instrument, venueName, market, book, time.Now().UTC())
			snapshot.TradeSimulation = &simulation
			if generated != nil {
				if publisher, ok := e.runtime.(SimulatedFillPublisher); ok {
					if publishErr := publisher.AppendSimulatedFill(ctx, instrument.ID, *generated); publishErr != nil {
						simulation.Error = appendError(simulation.Error, "publish stream: "+publishErr.Error())
						snapshot.TradeSimulation = &simulation
					}
				}
				_ = e.audit.Record("simulated_trade", map[string]any{"instrument": instrument.ID, "source_venue": venueName, "fill": generated})
				e.metrics.recordSimulatedTrade()
			}
		}

		var openOrdersErr error
		if market.CredentialID > 0 {
			ordersStartedAt := time.Now()
			venueSnapshot.OpenOrders, openOrdersErr = client.OpenOrders(ctx, market.Symbol)
			venueSnapshot.OrdersDurationMS = time.Since(ordersStartedAt).Milliseconds()
			if openOrdersErr != nil {
				venueSnapshot.AccountConnected = false
				venueSnapshot.Error = appendError(venueSnapshot.Error, "orders: "+openOrdersErr.Error())
			} else {
				venueSnapshot.AccountConnected = true
			}
			fillsStartedAt := time.Now()
			venueSnapshot.Fills, err = e.recentFills(ctx, venueName, instrument.ID, client, market.Symbol)
			venueSnapshot.FillsDurationMS = time.Since(fillsStartedAt).Milliseconds()
			if err != nil {
				venueSnapshot.Error = appendError(venueSnapshot.Error, "fills: "+err.Error())
			}
		}

		quotes, err := e.strategy.Generate(instrument, venueName, market, ref, book, strategyInventory)
		if err != nil {
			state, cancelErr := e.markVenueFailure(ctx, instrument.ID, venueName, venueCfg, market, client, "strategy", err, true)
			venueSnapshot.Fault = &state
			failures = append(failures, venueName+" strategy: "+err.Error())
			venueSnapshot.Error = appendError(venueSnapshot.Error, appendError("strategy: "+err.Error(), errorString(cancelErr)))
			snapshot.Venues = append(snapshot.Venues, venueSnapshot)
			continue
		}
		quotes, err = e.risk.FilterQuotes(instrument, market, book, strategyInventory, quotes)
		if err != nil {
			state, cancelErr := e.markVenueFailure(ctx, instrument.ID, venueName, venueCfg, market, client, "risk", err, true)
			venueSnapshot.Fault = &state
			failures = append(failures, venueName+" risk: "+err.Error())
			venueSnapshot.Error = appendError(venueSnapshot.Error, appendError("risk: "+err.Error(), errorString(cancelErr)))
			snapshot.Venues = append(snapshot.Venues, venueSnapshot)
			continue
		}
		managedOrders := oms.ManagedOrdersFor(client, venueSnapshot.OpenOrders)
		if e.cfg.Mode != domain.ModeLive || !venueCfg.TradingEnabled {
			// Shadow/disabled markets still persist observations into the fault
			// state machine. A healthy observation must therefore advance recovery
			// as well; otherwise a transient Shadow failure is inherited forever
			// when the market is later switched to Live. No exchange mutation is
			// allowed on this path, so order confirmation is intentionally zero.
			health, healthErr := e.faults.HealthyWithContext(ctx, e.marketFaultKey(venueName, instrument.ID, market), 0)
			if healthErr != nil {
				failures = append(failures, venueName+" fault persistence: "+healthErr.Error())
				venueSnapshot.Error = appendError(venueSnapshot.Error, "fault persistence: "+healthErr.Error())
				snapshot.Venues = append(snapshot.Venues, venueSnapshot)
				continue
			}
			venueSnapshot.Fault = &health.State
			if health.State.Status != fault.Normal {
				failures = append(failures, venueName+" fault state: "+health.State.Status)
			}
			quoteSummary := summarizeQuotes(quotes)
			_ = e.audit.Record("quote_plan", map[string]any{"instrument": instrument.ID, "venue": venueName, "book": book, "inventory": strategyInventory, "quote_summary": quoteSummary})
			e.logger.Debug("quote plan", "cycle_id", cycleID, "instrument", instrument.ID, "venue", venueName, "reference", ref.Price.String(), "inventory", strategyInventory.String(), "quote_summary", quoteSummary, "mode", e.effectiveMode(venueCfg))
			snapshot.Venues = append(snapshot.Venues, venueSnapshot)
			continue
		}
		if openOrdersErr != nil {
			state, cancelErr := e.markVenueFailure(ctx, instrument.ID, venueName, venueCfg, market, client, "open_orders", openOrdersErr, false)
			venueSnapshot.Fault = &state
			failures = append(failures, venueName+" orders: "+openOrdersErr.Error())
			venueSnapshot.Error = appendError(venueSnapshot.Error, errorString(cancelErr))
			snapshot.Venues = append(snapshot.Venues, venueSnapshot)
			continue
		}
		leaseKey := e.marketLeaseKey(venueName, instrument.ID, market)
		leaseGeneration, leaseErr := e.acquireMarketLease(ctx, leaseKey)
		if leaseErr != nil || leaseGeneration == 0 {
			if leaseErr == nil {
				leaseErr = fmt.Errorf("market is owned by another engine instance")
			}
			failures = append(failures, venueName+" lease: "+leaseErr.Error())
			venueSnapshot.Error = appendError(venueSnapshot.Error, "lease: "+leaseErr.Error())
			snapshot.Venues = append(snapshot.Venues, venueSnapshot)
			continue
		}
		quotes = risk.ApplyOrderLimit(quotes, len(venueSnapshot.OpenOrders), len(managedOrders), market.MaxOpenOrders)
		baseFree := num.FromInt64(0)
		quoteFree := num.FromInt64(0)
		if venueSnapshot.BaseBalance != nil {
			baseFree = venueSnapshot.BaseBalance.Free
		}
		if venueSnapshot.QuoteBalance != nil {
			quoteFree = venueSnapshot.QuoteBalance.Free
		}
		var budget domain.QuoteBudget
		quotes, budget = risk.ApplyBalanceBudget(quotes, managedOrders, baseFree, quoteFree, instrument.Strategy.BalanceReserveBPS, market.MaxBaseCommitment, market.MaxQuoteCommitment)
		venueSnapshot.Budget = &budget
		if len(quotes) == 0 {
			err = fmt.Errorf("balance budget allows no orders")
			state, cancelErr := e.markVenueFailure(ctx, instrument.ID, venueName, venueCfg, market, client, "budget", err, true)
			venueSnapshot.Fault = &state
			if cancelErr != nil {
				err = fmt.Errorf("%w; cancel: %v", err, cancelErr)
			}
			failures = append(failures, venueName+" budget: "+err.Error())
			venueSnapshot.Error = appendError(venueSnapshot.Error, "budget: "+err.Error())
			snapshot.Venues = append(snapshot.Venues, venueSnapshot)
			continue
		}
		health, healthErr := e.faults.HealthyWithContext(ctx, e.marketFaultKey(venueName, instrument.ID, market), len(managedOrders))
		if healthErr != nil {
			failures = append(failures, venueName+" fault persistence: "+healthErr.Error())
			venueSnapshot.Error = appendError(venueSnapshot.Error, "fault persistence: "+healthErr.Error())
			snapshot.Venues = append(snapshot.Venues, venueSnapshot)
			continue
		}
		venueSnapshot.Fault = &health.State
		if health.ShouldCancel {
			guard := e.marketWriteGuard(leaseKey, leaseGeneration)
			canceled, cancelErr := e.reconciler.CancelManagedGuardedWithResult(ctx, client, instrument.ID, market.Symbol, guard)
			e.metrics.recordOMS(0, canceled)
			if cancelErr != nil {
				venueSnapshot.Error = appendError(venueSnapshot.Error, "fault cancel: "+cancelErr.Error())
			}
			failures = append(failures, venueName+" fault state: "+health.State.Status)
			snapshot.Venues = append(snapshot.Venues, venueSnapshot)
			continue
		}
		if !health.AllowQuotes {
			failures = append(failures, venueName+" fault state: "+health.State.Status)
			snapshot.Venues = append(snapshot.Venues, venueSnapshot)
			continue
		}
		quoteSummary := summarizeQuotes(quotes)
		_ = e.audit.Record("quote_plan", map[string]any{"instrument": instrument.ID, "venue": venueName, "book": book, "inventory": strategyInventory, "budget": budget, "quote_summary": quoteSummary})
		e.logger.Debug("quote plan", "cycle_id", cycleID, "instrument", instrument.ID, "venue", venueName, "reference", ref.Price.String(), "inventory", strategyInventory.String(), "eligible_orders", budget.EligibleOrders, "target_orders", budget.TargetOrders, "quote_summary", quoteSummary, "mode", e.effectiveMode(venueCfg))
		leaseCtx, stopLeaseRenewal := e.keepMarketLease(ctx, leaseKey, leaseGeneration)
		guard := e.marketWriteGuard(leaseKey, leaseGeneration)
		omsStartedAt := time.Now()
		refreshPolicy := oms.RefreshPolicy{}
		if instrument.Strategy.QuoteRefreshSeconds > 0 {
			refreshPolicy = oms.RefreshPolicy{
				MinOrderAge:          instrument.Strategy.EffectiveMinOrderLifetime(),
				MaxOrderAge:          instrument.Strategy.EffectiveMaxOrderLifetime(),
				MaxRefreshesPerCycle: instrument.Strategy.RefreshOrdersPerCycle(len(quotes)),
			}
		}
		result, err := e.reconciler.ReconcileWithOrdersGuardedPolicy(leaseCtx, client, instrument.ID, quotes, instrument.Strategy.RepriceThresholdBPS, venueSnapshot.OpenOrders, guard, leaseGeneration, refreshPolicy)
		venueSnapshot.OMSDurationMS = time.Since(omsStartedAt).Milliseconds()
		if err != nil {
			canceled, cancelErr := e.reconciler.CancelManagedGuardedWithResult(leaseCtx, client, instrument.ID, market.Symbol, guard)
			e.metrics.recordOMS(0, canceled)
			stopLeaseRenewal()
			state, _ := e.markVenueFailure(ctx, instrument.ID, venueName, venueCfg, market, client, "oms", err, true)
			venueSnapshot.Fault = &state
			_ = e.audit.Record("oms_error", map[string]any{"instrument": instrument.ID, "venue": venueName, "error": err.Error()})
			if cancelErr != nil {
				_ = e.audit.Record("oms_cancel_error", map[string]any{"instrument": instrument.ID, "venue": venueName, "error": cancelErr.Error()})
			}
			failures = append(failures, venueName+" OMS: "+err.Error())
			venueSnapshot.Error = appendError(venueSnapshot.Error, "OMS: "+err.Error())
			snapshot.Venues = append(snapshot.Venues, venueSnapshot)
			continue
		}
		stopLeaseRenewal()
		venueSnapshot.PendingOrders = result.Pending
		_ = e.audit.Record("oms_result", map[string]any{"instrument": instrument.ID, "venue": venueName, "result": result})
		e.metrics.recordOMS(result.Placed, result.Canceled)
		snapshot.Venues = append(snapshot.Venues, venueSnapshot)
	}
	if activeMarkets == 0 {
		return fmt.Errorf("no enabled venue markets")
	}
	if len(failures) > 0 {
		return fmt.Errorf("%s", strings.Join(failures, "; "))
	}
	return nil
}

func (e *Engine) newSnapshot(instrument config.InstrumentConfig) runtimeops.InstrumentSnapshot {
	snapshot := runtimeops.InstrumentSnapshot{InstrumentID: instrument.ID, BaseSymbol: instrument.Base.Symbol, QuoteSymbol: instrument.Quote.Symbol, Mode: e.cfg.Mode, Status: "running", TargetInventory: instrument.Strategy.TargetBase, MaxBaseDeviation: instrument.Strategy.MaxBaseDeviation, Venues: []runtimeops.VenueSnapshot{}}
	if instrument.TradeSimulation.Enabled {
		snapshot.TradeSimulation = &tradesim.Snapshot{Enabled: true, SourceVenue: instrument.TradeSimulation.SourceVenue, Status: "waiting", Fills: []domain.Fill{}}
	}
	return snapshot
}

func (e *Engine) collectBalances(ctx context.Context, instrument config.InstrumentConfig, cache *balanceCache) (map[string][]domain.Balance, num.Decimal, int, map[string]error) {
	byVenue := make(map[string][]domain.Balance)
	errorsByVenue := make(map[string]error)
	total := num.FromInt64(0)
	count := 0
	for venueName, venueCfg := range e.cfg.Venues {
		if !venueCfg.Enabled || (e.cfg.Mode == domain.ModeLive && !venueCfg.TradingEnabled) {
			continue
		}
		market, ok := venueCfg.Markets[instrument.ID]
		if !ok || market.CredentialID <= 0 {
			continue
		}
		client := e.venues[venue.ClientKey(venueName, instrument.ID)]
		if client == nil {
			errorsByVenue[venueName] = fmt.Errorf("client missing")
			continue
		}
		balances, err := cache.fetch(ctx, accountCacheKey(venueCfg.Type, market.CredentialID), client)
		if err != nil {
			errorsByVenue[venueName] = err
			continue
		}
		for _, balance := range balances {
			if strings.EqualFold(balance.Asset, market.BaseAsset) {
				total = total.Add(balance.Free).Add(balance.Locked)
				break
			}
		}
		byVenue[venueName] = balances
		count++
	}
	return byVenue, total, count, errorsByVenue
}

func (e *Engine) recentFills(ctx context.Context, venueName, instrumentID string, client venue.Client, symbol string) ([]domain.Fill, error) {
	reader, ok := client.(venue.FillReader)
	if !ok {
		return []domain.Fill{}, nil
	}
	key := venue.ClientKey(venueName, instrumentID)
	e.fillMu.RLock()
	cached, cacheOK := e.fillCache[key]
	e.fillMu.RUnlock()
	if cacheOK && time.Since(cached.fetchedAt) < 10*time.Second {
		if cached.errorText != "" {
			return append([]domain.Fill(nil), cached.fills...), fmt.Errorf("%s", cached.errorText)
		}
		return append([]domain.Fill(nil), cached.fills...), nil
	}
	fills, err := reader.RecentFills(ctx, symbol, 50)
	entry := fillCacheEntry{fills: append([]domain.Fill(nil), fills...), fetchedAt: time.Now().UTC()}
	if err != nil {
		entry.errorText = err.Error()
	}
	e.fillMu.Lock()
	e.fillCache[key] = entry
	e.fillMu.Unlock()
	return fills, err
}

func findBalance(balances []domain.Balance, asset string) *domain.Balance {
	for _, balance := range balances {
		if strings.EqualFold(balance.Asset, asset) {
			copy := balance
			return &copy
		}
	}
	return nil
}

func joinErrors(values map[string]error) string {
	parts := make([]string, 0, len(values))
	for name, err := range values {
		parts = append(parts, name+": "+err.Error())
	}
	return strings.Join(parts, "; ")
}

func appendError(current, next string) string {
	if current == "" {
		return next
	}
	return current + "; " + next
}

func errorString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func (e *Engine) cancelInstrument(ctx context.Context, instrument config.InstrumentConfig) error {
	// An instrument that never passed startup preflight has never entered the
	// order-writing path in this runtime. Trying to clean it up can deadlock a
	// safe config switch when the old symbol itself is invalid at the venue.
	if _, blocked := e.preflightFailure(instrument.ID); blocked {
		return nil
	}
	var failures []string
	for venueName, venueCfg := range e.cfg.Venues {
		if !venueCfg.Enabled || !venueCfg.TradingEnabled {
			continue
		}
		market, ok := venueCfg.Markets[instrument.ID]
		if !ok {
			continue
		}
		client := e.venues[venue.ClientKey(venueName, instrument.ID)]
		if client == nil {
			continue
		}
		if err := e.cancelManagedFenced(ctx, venueName, instrument.ID, venueCfg, market, client); err != nil {
			failures = append(failures, venueName+": "+err.Error())
		}
	}
	if len(failures) > 0 {
		return fmt.Errorf("cancel failures: %s", strings.Join(failures, "; "))
	}
	return nil
}

func (e *Engine) CancelMarket(ctx context.Context, instrumentID, venueName string) error {
	// See cancelInstrument: startup-blocked instruments cannot own orders from
	// this runtime, so an invalid old symbol must not prevent a corrected symbol
	// from being activated.
	if _, blocked := e.preflightFailure(instrumentID); blocked {
		return nil
	}
	venueCfg, ok := e.cfg.Venues[venueName]
	if !ok || !venueCfg.Enabled || !venueCfg.TradingEnabled {
		return nil
	}
	market, ok := venueCfg.Markets[instrumentID]
	if !ok {
		return nil
	}
	client := e.venues[venue.ClientKey(venueName, instrumentID)]
	if client == nil {
		return fmt.Errorf("client missing")
	}
	leaseKey := e.marketLeaseKey(venueName, instrumentID, market)
	leaseGeneration, err := e.acquireMarketLease(ctx, leaseKey)
	if err != nil || leaseGeneration == 0 {
		if err != nil {
			return err
		}
		return fmt.Errorf("market is owned by another engine instance")
	}
	leaseCtx, stopLeaseRenewal := e.keepMarketLease(ctx, leaseKey, leaseGeneration)
	var canceled int
	canceled, err = e.reconciler.CancelManagedGuardedWithResult(leaseCtx, client, instrumentID, market.Symbol, e.marketWriteGuard(leaseKey, leaseGeneration))
	e.metrics.recordOMS(0, canceled)
	stopLeaseRenewal()
	if err == nil {
		err = e.faults.ResetWithContext(ctx, e.marketFaultKey(venueName, instrumentID, market))
	}
	if releaseErr := e.releaseMarketLease(ctx, leaseKey, leaseGeneration); err == nil {
		err = releaseErr
	}
	return err
}

func (e *Engine) marketFaultKey(venueName, instrumentID string, market config.VenueMarketConfig) string {
	return strings.ToLower(fmt.Sprintf("%s/%d/%s/%s", strings.TrimSpace(venueName), market.CredentialID, strings.TrimSpace(instrumentID), strings.TrimSpace(market.Symbol)))
}

func (e *Engine) markVenueFailure(ctx context.Context, instrumentID, venueName string, venueCfg config.VenueConfig, market config.VenueMarketConfig, client venue.Client, stage string, cause error, forceCancel bool) (fault.Snapshot, error) {
	e.metrics.recordVenueFault()
	decision, persistErr := e.faults.FailureWithContext(ctx, e.marketFaultKey(venueName, instrumentID, market), stage, cause, forceCancel)
	shouldCancel := decision.ShouldCancel || persistErr != nil
	if persistErr != nil {
		decision.State.Status = fault.Canceling
		decision.State.OrdersRetained = false
	}
	if !shouldCancel || e.cfg.Mode != domain.ModeLive || !venueCfg.TradingEnabled {
		if persistErr != nil {
			return decision.State, fmt.Errorf("persist fault state: %w", persistErr)
		}
		return decision.State, nil
	}
	err := e.cancelManagedFenced(ctx, venueName, instrumentID, venueCfg, market, client)
	data := map[string]any{"instrument": instrumentID, "venue": venueName, "stage": stage, "cause": cause.Error(), "fault": decision.State}
	if err != nil {
		data["cancel_error"] = err.Error()
	}
	if persistErr != nil {
		data["persist_error"] = persistErr.Error()
		err = fmt.Errorf("%s", appendError(errorString(err), "persist fault state: "+persistErr.Error()))
	}
	_ = e.audit.Record("venue_fault", data)
	return decision.State, err
}

func (e *Engine) cancelManagedFenced(ctx context.Context, venueName, instrumentID string, venueCfg config.VenueConfig, market config.VenueMarketConfig, client venue.Client) error {
	leaseKey := e.marketLeaseKey(venueName, instrumentID, market)
	generation, err := e.acquireMarketLease(ctx, leaseKey)
	if err != nil {
		return fmt.Errorf("acquire market lease: %w", err)
	}
	if generation == 0 {
		return fmt.Errorf("market is owned by another engine instance")
	}
	leaseCtx, stopLeaseRenewal := e.keepMarketLease(ctx, leaseKey, generation)
	defer stopLeaseRenewal()
	canceled, err := e.reconciler.CancelManagedGuardedWithResult(leaseCtx, client, instrumentID, market.Symbol, e.marketWriteGuard(leaseKey, generation))
	e.metrics.recordOMS(0, canceled)
	return err
}

func (e *Engine) Shutdown(ctx context.Context) error {
	var failures []string
	// Release market leases first (Redis-only, fast) so a replacement instance can
	// take over immediately, instead of waiting out the lease TTL (minutes) because a
	// slow exchange cancel below burned the shutdown budget and the process got
	// SIGKILLed before reaching this point. cancelInstrument re-acquires per market as
	// needed; a successor that already grabbed the lease simply adopts the orders.
	e.leaseMu.RLock()
	heldLeases := make(map[string]uint64, len(e.heldLeases))
	for key, generation := range e.heldLeases {
		heldLeases[key] = generation
	}
	e.leaseMu.RUnlock()
	for key, generation := range heldLeases {
		if err := e.releaseMarketLease(ctx, key, generation); err != nil {
			failures = append(failures, "lease "+key+": "+err.Error())
		}
	}
	for _, instrument := range e.cfg.Instruments {
		if err := e.cancelInstrument(ctx, instrument); err != nil {
			failures = append(failures, instrument.ID+": "+err.Error())
		}
	}
	_ = e.audit.Flush()
	if len(failures) > 0 {
		return fmt.Errorf("shutdown cancel failures: %s", strings.Join(failures, "; "))
	}
	return nil
}

func (e *Engine) marketLeaseKey(venueName, instrumentID string, market config.VenueMarketConfig) string {
	return strings.ToLower(fmt.Sprintf("%s/%d/%s/%s", venueName, market.CredentialID, instrumentID, market.Symbol))
}

func (e *Engine) acquireMarketLease(ctx context.Context, key string) (uint64, error) {
	locker, ok := e.runtime.(MarketLocker)
	if !ok {
		return 1, nil
	}
	ttl := e.marketLeaseTTL()
	generation, err := locker.AcquireMarketLease(ctx, key, e.ownerID, ttl)
	if generation > 0 && err == nil {
		e.leaseMu.Lock()
		e.heldLeases[key] = generation
		e.leaseMu.Unlock()
	}
	return generation, err
}

func (e *Engine) marketLeaseTTL() time.Duration {
	ttl := 30 * time.Second
	if configured := time.Duration(e.cfg.WatchdogTimeoutSeconds) * time.Second * 2; configured > ttl {
		ttl = configured
	}
	return ttl
}

func (e *Engine) keepMarketLease(parent context.Context, key string, generation uint64) (context.Context, func()) {
	locker, ok := e.runtime.(MarketLocker)
	if !ok {
		return parent, func() {}
	}
	ctx, cancel := context.WithCancel(parent)
	ttl := e.marketLeaseTTL()
	go func() {
		ticker := time.NewTicker(ttl / 3)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				current, err := locker.AcquireMarketLease(ctx, key, e.ownerID, ttl)
				if err != nil || current != generation {
					cancel()
					return
				}
			}
		}
	}()
	return ctx, cancel
}

func (e *Engine) releaseMarketLease(ctx context.Context, key string, generation uint64) error {
	locker, ok := e.runtime.(MarketLocker)
	e.leaseMu.RLock()
	heldGeneration, held := e.heldLeases[key]
	e.leaseMu.RUnlock()
	if !ok || !held || heldGeneration != generation {
		return nil
	}
	err := locker.ReleaseMarketLease(ctx, key, e.ownerID, generation)
	if err == nil {
		e.leaseMu.Lock()
		if e.heldLeases[key] == generation {
			delete(e.heldLeases, key)
		}
		e.leaseMu.Unlock()
	}
	return err
}

func (e *Engine) marketWriteGuard(key string, generation uint64) oms.WriteGuard {
	locker, ok := e.runtime.(MarketLocker)
	if !ok {
		return nil
	}
	return func(ctx context.Context) error {
		valid, err := locker.ValidateMarketLease(ctx, key, e.ownerID, generation)
		if err != nil {
			e.recordLeaseFenceRejection(key, generation, err)
			return fmt.Errorf("validate market lease generation %d: %w", generation, err)
		}
		if !valid {
			e.recordLeaseFenceRejection(key, generation, fmt.Errorf("lease is no longer current"))
			return fmt.Errorf("stale market lease generation %d", generation)
		}
		return nil
	}
}

func (e *Engine) recordLeaseFenceRejection(key string, generation uint64, cause error) {
	e.metrics.recordLeaseFenceRejection()
	_ = e.audit.Record("lease_fence_rejected", map[string]any{"market": key, "owner": e.ownerID, "generation": generation, "error": cause.Error()})
	e.logger.Error("exchange write rejected by lease fence", "market", key, "owner", e.ownerID, "generation", generation, "error", cause)
}

func newOwnerID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err == nil {
		return hex.EncodeToString(b)
	}
	return fmt.Sprintf("engine-%d", time.Now().UnixNano())
}

func (e *Engine) effectiveMode(v config.VenueConfig) string {
	if e.cfg.Mode == domain.ModeLive && v.TradingEnabled {
		return "live"
	}
	return "shadow"
}

func buildInstrumentAccountLocks(cfg config.Config) map[string][]*sync.Mutex {
	lockByAccount := make(map[string]*sync.Mutex)
	keysByInstrument := make(map[string]map[string]struct{})
	for _, venueCfg := range cfg.Venues {
		if !venueCfg.Enabled {
			continue
		}
		for instrumentID, market := range venueCfg.Markets {
			if market.CredentialID <= 0 {
				continue
			}
			key := fmt.Sprintf("%s/%d", strings.ToLower(venueCfg.Type), market.CredentialID)
			if lockByAccount[key] == nil {
				lockByAccount[key] = &sync.Mutex{}
			}
			if keysByInstrument[instrumentID] == nil {
				keysByInstrument[instrumentID] = make(map[string]struct{})
			}
			keysByInstrument[instrumentID][key] = struct{}{}
		}
	}
	result := make(map[string][]*sync.Mutex, len(keysByInstrument))
	for instrumentID, keySet := range keysByInstrument {
		keys := make([]string, 0, len(keySet))
		for key := range keySet {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			result[instrumentID] = append(result[instrumentID], lockByAccount[key])
		}
	}
	return result
}

func (e *Engine) lockInstrumentAccounts(instrumentID string) func() {
	locks := e.instrumentAccountLocks[instrumentID]
	for _, lock := range locks {
		lock.Lock()
	}
	return func() {
		for index := len(locks) - 1; index >= 0; index-- {
			locks[index].Unlock()
		}
	}
}

func (e *Engine) isPaused(instrumentID string) bool {
	e.pauseMu.RLock()
	defer e.pauseMu.RUnlock()
	_, paused := e.paused[instrumentID]
	return paused
}

func (e *Engine) setLastReferenceValidUntil(instrumentID string, value time.Time) {
	e.referenceMu.Lock()
	e.lastReferenceValidUntil[instrumentID] = value
	e.referenceMu.Unlock()
}

func (e *Engine) getLastReferenceValidUntil(instrumentID string) time.Time {
	e.referenceMu.RLock()
	defer e.referenceMu.RUnlock()
	return e.lastReferenceValidUntil[instrumentID]
}

func (e *Engine) setLastBookAt(key string, value time.Time) {
	e.bookMu.Lock()
	e.lastBookAt[key] = value
	e.bookMu.Unlock()
}

func (e *Engine) getLastBookAt(key string) time.Time {
	e.bookMu.RLock()
	defer e.bookMu.RUnlock()
	return e.lastBookAt[key]
}

func (e *Engine) setLastInventory(instrumentID string, value num.Decimal) {
	e.inventoryMu.Lock()
	e.lastInventory[instrumentID] = value
	e.inventoryMu.Unlock()
}

func (e *Engine) getLastInventory(instrumentID string) (num.Decimal, bool) {
	e.inventoryMu.RLock()
	defer e.inventoryMu.RUnlock()
	value, ok := e.lastInventory[instrumentID]
	return value, ok
}

func (m *engineMetrics) recordCycle(instruments, failures int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.cyclesTotal++
	if failures > 0 {
		m.cycleFailuresTotal++
	}
	m.instrumentRunsTotal += uint64(instruments)
	m.instrumentFailuresTotal += uint64(failures)
}

func (m *engineMetrics) recordVenueFault() {
	m.mu.Lock()
	m.venueFaultEventsTotal++
	m.mu.Unlock()
}

func (m *engineMetrics) recordOMS(placed, canceled int) {
	m.mu.Lock()
	m.omsPlacedTotal += uint64(max(0, placed))
	m.omsCanceledTotal += uint64(max(0, canceled))
	m.mu.Unlock()
}

func (m *engineMetrics) recordSimulatedTrade() {
	m.mu.Lock()
	m.simulatedTradesTotal++
	m.mu.Unlock()
}

func (m *engineMetrics) recordAuditFlushError() {
	m.mu.Lock()
	m.auditFlushErrorsTotal++
	m.mu.Unlock()
}

func (m *engineMetrics) recordRuleChange() {
	m.mu.Lock()
	m.ruleChangesTotal++
	m.mu.Unlock()
}

func (m *engineMetrics) recordLeaseFenceRejection() {
	m.mu.Lock()
	m.leaseFenceRejectsTotal++
	m.mu.Unlock()
}

func (m *engineMetrics) snapshot(auditPending int) runtimeops.MetricsSnapshot {
	m.mu.Lock()
	defer m.mu.Unlock()
	return runtimeops.MetricsSnapshot{
		StartedAt: m.startedAt, UpdatedAt: time.Now().UTC(), CyclesTotal: m.cyclesTotal,
		CycleFailuresTotal: m.cycleFailuresTotal, InstrumentRunsTotal: m.instrumentRunsTotal,
		InstrumentFailuresTotal: m.instrumentFailuresTotal, VenueFaultEventsTotal: m.venueFaultEventsTotal,
		OMSPlacedTotal: m.omsPlacedTotal, OMSCanceledTotal: m.omsCanceledTotal,
		SimulatedTradesTotal: m.simulatedTradesTotal, AuditFlushErrorsTotal: m.auditFlushErrorsTotal,
		AuditPendingEvents:     auditPending,
		RuleChangesTotal:       m.ruleChangesTotal,
		LeaseFenceRejectsTotal: m.leaseFenceRejectsTotal,
	}
}

type quoteSummary struct {
	Count         int         `json:"count"`
	BuyCount      int         `json:"buy_count"`
	SellCount     int         `json:"sell_count"`
	BestBid       num.Decimal `json:"best_bid"`
	OuterBid      num.Decimal `json:"outer_bid"`
	BestAsk       num.Decimal `json:"best_ask"`
	OuterAsk      num.Decimal `json:"outer_ask"`
	TotalQuantity num.Decimal `json:"total_quantity"`
}

func summarizeQuotes(quotes []domain.Quote) quoteSummary {
	summary := quoteSummary{Count: len(quotes)}
	for _, quote := range quotes {
		summary.TotalQuantity = summary.TotalQuantity.Add(quote.Quantity)
		if quote.Side == domain.Buy {
			summary.BuyCount++
			if summary.BuyCount == 1 || quote.Price.Cmp(summary.BestBid) > 0 {
				summary.BestBid = quote.Price
			}
			if summary.BuyCount == 1 || quote.Price.Cmp(summary.OuterBid) < 0 {
				summary.OuterBid = quote.Price
			}
			continue
		}
		summary.SellCount++
		if summary.SellCount == 1 || quote.Price.Cmp(summary.BestAsk) < 0 {
			summary.BestAsk = quote.Price
		}
		if summary.SellCount == 1 || quote.Price.Cmp(summary.OuterAsk) > 0 {
			summary.OuterAsk = quote.Price
		}
	}
	return summary
}

func (e *Engine) logCycle(cycleID string, duration time.Duration, failures []string) {
	errorText := strings.Join(failures, "; ")
	e.logMu.Lock()
	defer e.logMu.Unlock()
	if errorText == "" {
		if e.lastCycleError != "" {
			e.logger.Info("trading cycle recovered", "cycle_id", cycleID, "duration_ms", duration.Milliseconds())
		}
		e.lastCycleError = ""
		return
	}
	now := time.Now().UTC()
	if errorText == e.lastCycleError && now.Sub(e.lastCycleErrorAt) < time.Minute {
		e.logger.Debug("trading cycle still degraded", "cycle_id", cycleID, "duration_ms", duration.Milliseconds(), "failure_count", len(failures))
		return
	}
	e.lastCycleError = errorText
	e.lastCycleErrorAt = now
	e.logger.Warn("trading cycle degraded", "cycle_id", cycleID, "duration_ms", duration.Milliseconds(), "failure_count", len(failures), "error", errorText)
}
