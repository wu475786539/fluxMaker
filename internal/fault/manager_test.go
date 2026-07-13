package fault

import (
	"context"
	"fmt"
	"sync"
	"testing"
)

type memoryStateStore struct {
	mu     sync.Mutex
	states map[string][]byte
}

func (s *memoryStateStore) LoadFaultState(_ context.Context, key string) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]byte(nil), s.states[key]...), nil
}

func (s *memoryStateStore) SaveFaultState(_ context.Context, key string, payload []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.states == nil {
		s.states = make(map[string][]byte)
	}
	s.states[key] = append([]byte(nil), payload...)
	return nil
}

func (s *memoryStateStore) DeleteFaultState(_ context.Context, key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.states, key)
	return nil
}

func TestFailureGraceCancelAndRecovery(t *testing.T) {
	m := New(3, 3)
	if d := m.Failure("market", "book", fmt.Errorf("timeout"), false); d.State.Status != Degraded || d.ShouldCancel {
		t.Fatalf("first=%+v", d)
	}
	m.Failure("market", "book", fmt.Errorf("timeout"), false)
	if d := m.Failure("market", "book", fmt.Errorf("timeout"), false); d.State.Status != Canceling || !d.ShouldCancel {
		t.Fatalf("third=%+v", d)
	}
	if d := m.Healthy("market", 2); !d.ShouldCancel || d.State.Status != Canceling {
		t.Fatalf("cancel confirmation=%+v", d)
	}
	if d := m.Healthy("market", 0); d.State.Status != Paused || d.AllowQuotes {
		t.Fatalf("paused=%+v", d)
	}
	if d := m.Healthy("market", 0); d.State.Status != Recovering || d.AllowQuotes {
		t.Fatalf("recovering1=%+v", d)
	}
	if d := m.Healthy("market", 0); d.AllowQuotes {
		t.Fatalf("recovering2=%+v", d)
	}
	if d := m.Healthy("market", 0); d.State.Status != Normal || !d.AllowQuotes {
		t.Fatalf("recovered=%+v", d)
	}
}

func TestPersistedFailureStateIsInheritedByReplacementManager(t *testing.T) {
	store := &memoryStateStore{states: make(map[string][]byte)}
	first := NewWithStateStore(3, 3, store)
	if decision, err := first.FailureWithContext(context.Background(), "binance/token", "book", fmt.Errorf("timeout"), false); err != nil || decision.State.ConsecutiveFailures != 1 {
		t.Fatalf("first failure decision=%+v err=%v", decision, err)
	}
	if _, err := first.FailureWithContext(context.Background(), "binance/token", "book", fmt.Errorf("timeout"), false); err != nil {
		t.Fatal(err)
	}

	// This represents a hot-switched engine with a fresh in-memory manager.
	replacement := NewWithStateStore(3, 3, store)
	decision, err := replacement.FailureWithContext(context.Background(), "binance/token", "book", fmt.Errorf("timeout"), false)
	if err != nil {
		t.Fatal(err)
	}
	if decision.State.Status != Canceling || decision.State.ConsecutiveFailures != 3 || !decision.ShouldCancel {
		t.Fatalf("replacement did not inherit failure state: %+v", decision)
	}

	if err := replacement.ResetWithContext(context.Background(), "binance/token"); err != nil {
		t.Fatal(err)
	}
	store.mu.Lock()
	_, retained := store.states["binance/token"]
	store.mu.Unlock()
	if retained {
		t.Fatal("reset retained persisted fault state")
	}
}
