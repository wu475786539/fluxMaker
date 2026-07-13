package runtimeops

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"fluxmaker/internal/domain"
	"fluxmaker/internal/fault"
	"fluxmaker/internal/num"
	"fluxmaker/internal/tradesim"

	"github.com/redis/go-redis/v9"
)

const (
	snapshotPrefix      = "fluxmaker:runtime:instrument:"
	heartbeatKey        = "fluxmaker:runtime:engine"
	appliedVersionKey   = "fluxmaker:runtime:applied-version"
	pausedKey           = "fluxmaker:control:paused"
	reconcileKey        = "fluxmaker:control:reconcile"
	tradingProgressKey  = "fluxmaker:runtime:trading-progress"
	cyclePerformanceKey = "fluxmaker:runtime:cycle-performance"
	metricsKey          = "fluxmaker:runtime:metrics"
	watchdogKey         = "fluxmaker:runtime:watchdog"
	ruleChangesKey      = "fluxmaker:runtime:rule-changes"
	omsStatePrefix      = "fluxmaker:oms:state:"
	faultStatePrefix    = "fluxmaker:fault:state:"
	marketLeasePrefix   = "fluxmaker:lease:market:"
	marketFencePrefix   = "fluxmaker:lease:generation:"
	simulationPrefix    = "fluxmaker:simulation:fills:"
	snapshotTTL         = 45 * time.Second
	heartbeatTTL        = 15 * time.Second
)

type EngineStatus struct {
	Online              bool              `json:"online"`
	Ready               bool              `json:"ready"`
	Version             int64             `json:"version"`
	DesiredVersion      int64             `json:"desired_version"`
	Error               string            `json:"error,omitempty"`
	LastHeartbeat       time.Time         `json:"last_heartbeat"`
	LastTradingProgress time.Time         `json:"last_trading_progress,omitempty"`
	Performance         *CyclePerformance `json:"performance,omitempty"`
	Metrics             *MetricsSnapshot  `json:"metrics,omitempty"`
	Watchdog            *WatchdogStatus   `json:"watchdog,omitempty"`
	RuleChanges         []RuleChange      `json:"rule_changes,omitempty"`
}

type CyclePerformance struct {
	StartedAt       time.Time `json:"started_at"`
	DurationMS      int64     `json:"duration_ms"`
	Instruments     int       `json:"instruments"`
	Succeeded       int       `json:"succeeded"`
	Failed          int       `json:"failed"`
	ConcurrentLimit int       `json:"concurrent_limit"`
}

type MetricsSnapshot struct {
	StartedAt               time.Time `json:"started_at"`
	UpdatedAt               time.Time `json:"updated_at"`
	CyclesTotal             uint64    `json:"cycles_total"`
	CycleFailuresTotal      uint64    `json:"cycle_failures_total"`
	InstrumentRunsTotal     uint64    `json:"instrument_runs_total"`
	InstrumentFailuresTotal uint64    `json:"instrument_failures_total"`
	VenueFaultEventsTotal   uint64    `json:"venue_fault_events_total"`
	OMSPlacedTotal          uint64    `json:"oms_placed_total"`
	OMSCanceledTotal        uint64    `json:"oms_canceled_total"`
	SimulatedTradesTotal    uint64    `json:"simulated_trades_total"`
	AuditFlushErrorsTotal   uint64    `json:"audit_flush_errors_total"`
	AuditPendingEvents      int       `json:"audit_pending_events"`
	RuleChangesTotal        uint64    `json:"rule_changes_total"`
	LeaseFenceRejectsTotal  uint64    `json:"lease_fence_rejects_total"`
}

type WatchdogStatus struct {
	Healthy         bool      `json:"healthy"`
	LastCheckAt     time.Time `json:"last_check_at"`
	LastTriggeredAt time.Time `json:"last_triggered_at,omitempty"`
	Reason          string    `json:"reason,omitempty"`
	CancelError     string    `json:"cancel_error,omitempty"`
}

type RuleChange struct {
	InstrumentID string             `json:"instrument_id"`
	Venue        string             `json:"venue"`
	Symbol       string             `json:"symbol"`
	Previous     domain.MarketRules `json:"previous"`
	Current      domain.MarketRules `json:"current"`
	DetectedAt   time.Time          `json:"detected_at"`
}

func (s *Store) LoadOMSState(ctx context.Context, key string) ([]byte, error) {
	if s == nil || s.redis == nil {
		return nil, nil
	}
	payload, err := s.redis.Get(ctx, omsStatePrefix+key).Bytes()
	if err == redis.Nil {
		return nil, nil
	}
	return payload, err
}

func (s *Store) SaveOMSState(ctx context.Context, key string, payload []byte) error {
	if s == nil || s.redis == nil {
		return nil
	}
	return s.redis.Set(ctx, omsStatePrefix+key, payload, 7*24*time.Hour).Err()
}

func (s *Store) DeleteOMSState(ctx context.Context, key string) error {
	if s == nil || s.redis == nil {
		return nil
	}
	return s.redis.Del(ctx, omsStatePrefix+key).Err()
}

func (s *Store) LoadFaultState(ctx context.Context, key string) ([]byte, error) {
	if s == nil || s.redis == nil {
		return nil, nil
	}
	payload, err := s.redis.Get(ctx, faultStatePrefix+key).Bytes()
	if err == redis.Nil {
		return nil, nil
	}
	return payload, err
}

func (s *Store) SaveFaultState(ctx context.Context, key string, payload []byte) error {
	if s == nil || s.redis == nil {
		return nil
	}
	// Abnormal states are safety-critical and must survive an arbitrarily long
	// outage. The manager deletes this key only after a confirmed normal state.
	return s.redis.Set(ctx, faultStatePrefix+key, payload, 0).Err()
}

func (s *Store) DeleteFaultState(ctx context.Context, key string) error {
	if s == nil || s.redis == nil {
		return nil
	}
	return s.redis.Del(ctx, faultStatePrefix+key).Err()
}

func (s *Store) AcquireMarketLease(ctx context.Context, key, owner string, ttl time.Duration) (uint64, error) {
	if s == nil || s.redis == nil {
		return 1, nil
	}
	if ttl <= 0 {
		ttl = 30 * time.Second
	}
	result, err := s.redis.Eval(ctx, `
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
return generation`, []string{marketLeasePrefix + key, marketFencePrefix + key}, owner, ttl.Milliseconds()).Int64()
	if err != nil || result <= 0 {
		return 0, err
	}
	return uint64(result), nil
}

func (s *Store) ValidateMarketLease(ctx context.Context, key, owner string, generation uint64) (bool, error) {
	if s == nil || s.redis == nil {
		return true, nil
	}
	expected := fmt.Sprintf("%s:%d", owner, generation)
	result, err := s.redis.Eval(ctx, `
if redis.call('GET', KEYS[1]) == ARGV[1] then
  return 1
end
return 0`, []string{marketLeasePrefix + key}, expected).Int()
	return result == 1, err
}

func (s *Store) ReleaseMarketLease(ctx context.Context, key, owner string, generation uint64) error {
	if s == nil || s.redis == nil {
		return nil
	}
	expected := fmt.Sprintf("%s:%d", owner, generation)
	return s.redis.Eval(ctx, `
if redis.call('GET', KEYS[1]) == ARGV[1] then
  return redis.call('DEL', KEYS[1])
end
return 0`, []string{marketLeasePrefix + key}, expected).Err()
}

type PauseState struct {
	InstrumentID string    `json:"instrument_id"`
	Paused       bool      `json:"paused"`
	Reason       string    `json:"reason"`
	RequestedBy  int64     `json:"requested_by"`
	RequestedAt  time.Time `json:"requested_at"`
}

type ReconcileRequest struct {
	InstrumentID string    `json:"instrument_id"`
	RequestedBy  int64     `json:"requested_by"`
	RequestedAt  time.Time `json:"requested_at"`
}

type VenueSnapshot struct {
	Name             string              `json:"name"`
	Type             string              `json:"type"`
	Symbol           string              `json:"symbol"`
	TradingEnabled   bool                `json:"trading_enabled"`
	MarketConnected  bool                `json:"market_connected"`
	AccountConnected bool                `json:"account_connected"`
	Book             *domain.Book        `json:"book,omitempty"`
	BaseBalance      *domain.Balance     `json:"base_balance,omitempty"`
	QuoteBalance     *domain.Balance     `json:"quote_balance,omitempty"`
	Budget           *domain.QuoteBudget `json:"budget,omitempty"`
	Rules            *domain.MarketRules `json:"rules,omitempty"`
	Fault            *fault.Snapshot     `json:"fault,omitempty"`
	OpenOrders       []domain.Order      `json:"open_orders"`
	PendingOrders    int                 `json:"pending_orders"`
	Fills            []domain.Fill       `json:"fills"`
	Error            string              `json:"error,omitempty"`
	UpdatedAt        time.Time           `json:"updated_at"`
	BookDurationMS   int64               `json:"book_duration_ms"`
	OrdersDurationMS int64               `json:"orders_duration_ms"`
	FillsDurationMS  int64               `json:"fills_duration_ms"`
	OMSDurationMS    int64               `json:"oms_duration_ms"`
}

type InstrumentSnapshot struct {
	InstrumentID        string                 `json:"instrument_id"`
	BaseSymbol          string                 `json:"base_symbol"`
	QuoteSymbol         string                 `json:"quote_symbol"`
	Mode                domain.Mode            `json:"mode"`
	Status              string                 `json:"status"`
	Paused              bool                   `json:"paused"`
	Pause               *PauseState            `json:"pause,omitempty"`
	Reference           *domain.ReferencePrice `json:"reference,omitempty"`
	Inventory           num.Decimal            `json:"inventory"`
	InventoryAvailable  bool                   `json:"inventory_available"`
	TargetInventory     num.Decimal            `json:"target_inventory"`
	MaxBaseDeviation    num.Decimal            `json:"max_base_deviation"`
	Venues              []VenueSnapshot        `json:"venues"`
	TradeSimulation     *tradesim.Snapshot     `json:"trade_simulation,omitempty"`
	Error               string                 `json:"error,omitempty"`
	UpdatedAt           time.Time              `json:"updated_at"`
	TickDurationMS      int64                  `json:"tick_duration_ms"`
	ReferenceDurationMS int64                  `json:"reference_duration_ms"`
	BalanceDurationMS   int64                  `json:"balance_duration_ms"`
}

type Store struct {
	redis        *redis.Client
	progressMu   sync.Mutex
	lastProgress time.Time
}

func New(redisClient *redis.Client) *Store {
	return &Store{redis: redisClient}
}

func (s *Store) Publish(ctx context.Context, snapshot InstrumentSnapshot) error {
	if s == nil || s.redis == nil {
		return nil
	}
	payload, err := json.Marshal(snapshot)
	if err != nil {
		return err
	}
	return s.redis.Set(ctx, snapshotPrefix+snapshot.InstrumentID, payload, snapshotTTL).Err()
}

// AppendSimulatedFill publishes an explicitly synthetic event for internal
// consumers. The separate stream prevents it from ever being confused with
// exchange-reported fills.
func (s *Store) AppendSimulatedFill(ctx context.Context, instrumentID string, fill domain.Fill) error {
	if s == nil || s.redis == nil {
		return nil
	}
	payload, err := json.Marshal(fill)
	if err != nil {
		return err
	}
	return s.redis.XAdd(ctx, &redis.XAddArgs{
		Stream: simulationPrefix + instrumentID,
		MaxLen: 1000,
		Approx: true,
		Values: map[string]any{"payload": string(payload)},
	}).Err()
}

func (s *Store) Get(ctx context.Context, instrumentID string) (InstrumentSnapshot, error) {
	if s == nil || s.redis == nil {
		return InstrumentSnapshot{}, redis.Nil
	}
	payload, err := s.redis.Get(ctx, snapshotPrefix+instrumentID).Bytes()
	if err != nil {
		return InstrumentSnapshot{}, err
	}
	var snapshot InstrumentSnapshot
	if err := json.Unmarshal(payload, &snapshot); err != nil {
		return InstrumentSnapshot{}, err
	}
	return snapshot, nil
}

func (s *Store) Heartbeat(ctx context.Context, version, desiredVersion int64, ready bool, errorText string) error {
	if s == nil || s.redis == nil {
		return nil
	}
	status := EngineStatus{Online: true, Ready: ready, Version: version, DesiredVersion: desiredVersion, Error: errorText, LastHeartbeat: time.Now().UTC()}
	payload, _ := json.Marshal(status)
	return s.redis.Set(ctx, heartbeatKey, payload, heartbeatTTL).Err()
}

func (s *Store) EngineStatus(ctx context.Context) EngineStatus {
	if s == nil || s.redis == nil {
		return EngineStatus{}
	}
	payload, err := s.redis.Get(ctx, heartbeatKey).Bytes()
	if err != nil {
		return EngineStatus{}
	}
	var status EngineStatus
	if json.Unmarshal(payload, &status) != nil {
		return EngineStatus{}
	}
	status.Online = time.Since(status.LastHeartbeat) < heartbeatTTL
	status.LastTradingProgress = s.TradingProgress(ctx)
	status.Performance = s.CyclePerformance(ctx)
	status.Metrics = s.Metrics(ctx)
	status.Watchdog = s.WatchdogStatus(ctx)
	status.RuleChanges = s.RuleChanges(ctx, 50)
	return status
}

func (s *Store) ReportTradingProgress(ctx context.Context) error {
	if s == nil || s.redis == nil {
		return nil
	}
	now := time.Now().UTC()
	s.progressMu.Lock()
	if !s.lastProgress.IsZero() && now.Sub(s.lastProgress) < time.Second {
		s.progressMu.Unlock()
		return nil
	}
	s.lastProgress = now
	s.progressMu.Unlock()
	return s.redis.Set(ctx, tradingProgressKey, now.Format(time.RFC3339Nano), 0).Err()
}

func (s *Store) ReportCyclePerformance(ctx context.Context, performance CyclePerformance) error {
	if s == nil || s.redis == nil {
		return nil
	}
	payload, err := json.Marshal(performance)
	if err != nil {
		return err
	}
	return s.redis.Set(ctx, cyclePerformanceKey, payload, 5*time.Minute).Err()
}

func (s *Store) CyclePerformance(ctx context.Context) *CyclePerformance {
	if s == nil || s.redis == nil {
		return nil
	}
	payload, err := s.redis.Get(ctx, cyclePerformanceKey).Bytes()
	if err != nil {
		return nil
	}
	var performance CyclePerformance
	if json.Unmarshal(payload, &performance) != nil {
		return nil
	}
	return &performance
}

func (s *Store) ReportMetrics(ctx context.Context, metrics MetricsSnapshot) error {
	if s == nil || s.redis == nil {
		return nil
	}
	payload, err := json.Marshal(metrics)
	if err != nil {
		return err
	}
	return s.redis.Set(ctx, metricsKey, payload, 5*time.Minute).Err()
}

func (s *Store) Metrics(ctx context.Context) *MetricsSnapshot {
	if s == nil || s.redis == nil {
		return nil
	}
	payload, err := s.redis.Get(ctx, metricsKey).Bytes()
	if err != nil {
		return nil
	}
	var metrics MetricsSnapshot
	if json.Unmarshal(payload, &metrics) != nil {
		return nil
	}
	return &metrics
}

func (s *Store) ReportWatchdog(ctx context.Context, healthy, actionTriggered bool, reason, cancelError string) error {
	if s == nil || s.redis == nil {
		return nil
	}
	status := WatchdogStatus{Healthy: healthy, LastCheckAt: time.Now().UTC()}
	if previous := s.WatchdogStatus(ctx); previous != nil {
		status.LastTriggeredAt = previous.LastTriggeredAt
		status.Reason = previous.Reason
		status.CancelError = previous.CancelError
	}
	if !healthy {
		status.Reason = reason
		status.CancelError = cancelError
	}
	if actionTriggered {
		status.LastTriggeredAt = status.LastCheckAt
	}
	payload, err := json.Marshal(status)
	if err != nil {
		return err
	}
	return s.redis.Set(ctx, watchdogKey, payload, 5*time.Minute).Err()
}

func (s *Store) WatchdogStatus(ctx context.Context) *WatchdogStatus {
	if s == nil || s.redis == nil {
		return nil
	}
	payload, err := s.redis.Get(ctx, watchdogKey).Bytes()
	if err != nil {
		return nil
	}
	var status WatchdogStatus
	if json.Unmarshal(payload, &status) != nil {
		return nil
	}
	return &status
}

func (s *Store) ReportRuleChange(ctx context.Context, change RuleChange) error {
	if s == nil || s.redis == nil {
		return nil
	}
	payload, err := json.Marshal(change)
	if err != nil {
		return err
	}
	pipe := s.redis.TxPipeline()
	pipe.LPush(ctx, ruleChangesKey, payload)
	pipe.LTrim(ctx, ruleChangesKey, 0, 99)
	pipe.Expire(ctx, ruleChangesKey, 24*time.Hour)
	_, err = pipe.Exec(ctx)
	return err
}

func (s *Store) RuleChanges(ctx context.Context, limit int64) []RuleChange {
	if s == nil || s.redis == nil {
		return nil
	}
	if limit <= 0 || limit > 100 {
		limit = 50
	}
	values, err := s.redis.LRange(ctx, ruleChangesKey, 0, limit-1).Result()
	if err != nil {
		return nil
	}
	changes := make([]RuleChange, 0, len(values))
	for _, value := range values {
		var change RuleChange
		if json.Unmarshal([]byte(value), &change) == nil {
			changes = append(changes, change)
		}
	}
	return changes
}

func (s *Store) TradingProgress(ctx context.Context) time.Time {
	if s == nil || s.redis == nil {
		return time.Time{}
	}
	value, err := s.redis.Get(ctx, tradingProgressKey).Result()
	if err != nil {
		return time.Time{}
	}
	parsed, _ := time.Parse(time.RFC3339Nano, value)
	return parsed
}

func (s *Store) SetAppliedVersion(ctx context.Context, version int64) error {
	if s == nil || s.redis == nil || version <= 0 {
		return nil
	}
	return s.redis.Set(ctx, appliedVersionKey, version, 0).Err()
}

func (s *Store) AppliedVersion(ctx context.Context) int64 {
	if s == nil || s.redis == nil {
		return 0
	}
	version, err := s.redis.Get(ctx, appliedVersionKey).Int64()
	if err != nil {
		return 0
	}
	return version
}

func (s *Store) SetPaused(ctx context.Context, instrumentID, reason string, userID int64) (PauseState, error) {
	if instrumentID == "" {
		return PauseState{}, fmt.Errorf("instrument id is required")
	}
	state := PauseState{InstrumentID: instrumentID, Paused: true, Reason: reason, RequestedBy: userID, RequestedAt: time.Now().UTC()}
	payload, _ := json.Marshal(state)
	if err := s.redis.HSet(ctx, pausedKey, instrumentID, payload).Err(); err != nil {
		return PauseState{}, err
	}
	return state, nil
}

func (s *Store) Resume(ctx context.Context, instrumentID string) error {
	return s.redis.HDel(ctx, pausedKey, instrumentID).Err()
}

func (s *Store) RequestReconcile(ctx context.Context, instrumentID string, userID int64) (ReconcileRequest, error) {
	request := ReconcileRequest{InstrumentID: instrumentID, RequestedBy: userID, RequestedAt: time.Now().UTC()}
	payload, _ := json.Marshal(request)
	return request, s.redis.HSet(ctx, reconcileKey, instrumentID, payload).Err()
}

func (s *Store) Reconciles(ctx context.Context) (map[string]ReconcileRequest, error) {
	result := make(map[string]ReconcileRequest)
	values, err := s.redis.HGetAll(ctx, reconcileKey).Result()
	if err != nil {
		return nil, err
	}
	for id, payload := range values {
		var request ReconcileRequest
		if json.Unmarshal([]byte(payload), &request) == nil {
			result[id] = request
		}
	}
	return result, nil
}

func (s *Store) ClearReconcile(ctx context.Context, instrumentID string) error {
	return s.redis.HDel(ctx, reconcileKey, instrumentID).Err()
}

func (s *Store) Paused(ctx context.Context) (map[string]PauseState, error) {
	result := make(map[string]PauseState)
	if s == nil || s.redis == nil {
		return result, nil
	}
	values, err := s.redis.HGetAll(ctx, pausedKey).Result()
	if err != nil {
		return nil, err
	}
	for instrumentID, payload := range values {
		var state PauseState
		if json.Unmarshal([]byte(payload), &state) == nil {
			result[instrumentID] = state
		}
	}
	return result, nil
}
