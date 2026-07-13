package fault

import (
	"context"
	"encoding/json"
	"sync"
	"time"
)

const (
	Normal     = "normal"
	Degraded   = "degraded"
	Canceling  = "canceling"
	Paused     = "paused"
	Recovering = "recovering"
)

type Snapshot struct {
	Status               string    `json:"status"`
	Stage                string    `json:"stage,omitempty"`
	Error                string    `json:"error,omitempty"`
	ConsecutiveFailures  int       `json:"consecutive_failures"`
	ConsecutiveSuccesses int       `json:"consecutive_successes"`
	Since                time.Time `json:"since"`
	UpdatedAt            time.Time `json:"updated_at"`
	LastHealthyAt        time.Time `json:"last_healthy_at"`
	OrdersRetained       bool      `json:"orders_retained"`
}

type Decision struct {
	State        Snapshot
	AllowQuotes  bool
	ShouldCancel bool
}

type StateStore interface {
	LoadFaultState(ctx context.Context, key string) ([]byte, error)
	SaveFaultState(ctx context.Context, key string, payload []byte) error
	DeleteFaultState(ctx context.Context, key string) error
}

type Manager struct {
	mu                sync.Mutex
	failureThreshold  int
	recoveryThreshold int
	states            map[string]Snapshot
	loaded            map[string]bool
	persisted         map[string]bool
	store             StateStore
}

func New(failureThreshold, recoveryThreshold int) *Manager {
	return NewWithStateStore(failureThreshold, recoveryThreshold, nil)
}

func NewWithStateStore(failureThreshold, recoveryThreshold int, store StateStore) *Manager {
	if failureThreshold < 1 {
		failureThreshold = 3
	}
	if recoveryThreshold < 1 {
		recoveryThreshold = 3
	}
	return &Manager{failureThreshold: failureThreshold, recoveryThreshold: recoveryThreshold, states: make(map[string]Snapshot), loaded: make(map[string]bool), persisted: make(map[string]bool), store: store}
}

func (m *Manager) Failure(key, stage string, cause error, forceCancel bool) Decision {
	decision, _ := m.FailureWithContext(context.Background(), key, stage, cause, forceCancel)
	return decision
}

func (m *Manager) FailureWithContext(ctx context.Context, key, stage string, cause error, forceCancel bool) (Decision, error) {
	if err := m.ensureLoaded(ctx, key); err != nil {
		return Decision{}, err
	}
	m.mu.Lock()
	now := time.Now().UTC()
	state := m.state(key, now)
	state.ConsecutiveFailures++
	state.ConsecutiveSuccesses = 0
	state.Stage = stage
	if cause != nil {
		state.Error = cause.Error()
	}
	state.UpdatedAt = now
	decision := Decision{}
	if forceCancel || state.ConsecutiveFailures >= m.failureThreshold || state.Status == Canceling {
		if state.Status != Canceling {
			state.Since = now
		}
		state.Status = Canceling
		state.OrdersRetained = false
		decision.ShouldCancel = true
	} else {
		if state.Status == Normal || state.Status == Recovering || state.Status == Paused {
			state.Since = now
		}
		state.Status = Degraded
		state.OrdersRetained = true
	}
	m.states[key] = state
	m.mu.Unlock()
	decision.State = state
	return decision, m.persist(ctx, key, state)
}

func (m *Manager) Healthy(key string, managedOpenOrders int) Decision {
	decision, _ := m.HealthyWithContext(context.Background(), key, managedOpenOrders)
	return decision
}

func (m *Manager) HealthyWithContext(ctx context.Context, key string, managedOpenOrders int) (Decision, error) {
	if err := m.ensureLoaded(ctx, key); err != nil {
		return Decision{}, err
	}
	m.mu.Lock()
	now := time.Now().UTC()
	state := m.state(key, now)
	state.UpdatedAt = now
	state.ConsecutiveFailures = 0
	state.Error = ""
	state.Stage = ""
	decision := Decision{}
	switch state.Status {
	case Normal:
		state.LastHealthyAt = now
		state.OrdersRetained = true
		decision.AllowQuotes = true
	case Canceling:
		if managedOpenOrders > 0 {
			decision.ShouldCancel = true
		} else {
			state.Status = Paused
			state.Since = now
			state.OrdersRetained = false
		}
	case Paused, Degraded:
		state.Status = Recovering
		state.Since = now
		state.ConsecutiveSuccesses = 1
	case Recovering:
		state.ConsecutiveSuccesses++
	}
	if state.Status == Recovering && state.ConsecutiveSuccesses >= m.recoveryThreshold {
		state.Status = Normal
		state.Since = now
		state.LastHealthyAt = now
		state.ConsecutiveSuccesses = 0
		state.OrdersRetained = true
		decision.AllowQuotes = true
	}
	m.states[key] = state
	m.mu.Unlock()
	decision.State = state
	return decision, m.persist(ctx, key, state)
}

func (m *Manager) Snapshot(key string) Snapshot {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.state(key, time.Now().UTC())
}

func (m *Manager) Reset(key string) {
	_ = m.ResetWithContext(context.Background(), key)
}

func (m *Manager) ResetWithContext(ctx context.Context, key string) error {
	m.mu.Lock()
	delete(m.states, key)
	m.loaded[key] = true
	delete(m.persisted, key)
	m.mu.Unlock()
	if m.store != nil {
		return m.store.DeleteFaultState(ctx, key)
	}
	return nil
}

func (m *Manager) state(key string, now time.Time) Snapshot {
	state, ok := m.states[key]
	if !ok {
		state = Snapshot{Status: Normal, Since: now, UpdatedAt: now, LastHealthyAt: now, OrdersRetained: true}
	}
	return state
}

func (m *Manager) ensureLoaded(ctx context.Context, key string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.loaded[key] {
		return nil
	}
	if m.store == nil {
		m.loaded[key] = true
		return nil
	}
	payload, err := m.store.LoadFaultState(ctx, key)
	if err != nil {
		return err
	}
	m.loaded[key] = true
	if len(payload) == 0 {
		return nil
	}
	var state Snapshot
	if err := json.Unmarshal(payload, &state); err != nil {
		delete(m.loaded, key)
		return err
	}
	m.states[key] = state
	m.persisted[key] = true
	return nil
}

func (m *Manager) persist(ctx context.Context, key string, state Snapshot) error {
	if m.store == nil {
		return nil
	}
	if state.Status == Normal && state.ConsecutiveFailures == 0 && state.ConsecutiveSuccesses == 0 {
		m.mu.Lock()
		wasPersisted := m.persisted[key]
		delete(m.persisted, key)
		m.mu.Unlock()
		if !wasPersisted {
			return nil
		}
		if err := m.store.DeleteFaultState(ctx, key); err != nil {
			m.mu.Lock()
			m.persisted[key] = true
			m.mu.Unlock()
			return err
		}
		return nil
	}
	payload, err := json.Marshal(state)
	if err != nil {
		return err
	}
	if err := m.store.SaveFaultState(ctx, key, payload); err != nil {
		return err
	}
	m.mu.Lock()
	m.persisted[key] = true
	m.mu.Unlock()
	return nil
}
