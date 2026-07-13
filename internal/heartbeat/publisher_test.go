package heartbeat

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

type statusCall struct {
	version int64
	desired int64
	ready   bool
}

type fakeStatusStore struct {
	mu    sync.Mutex
	calls []statusCall
}

func (f *fakeStatusStore) Heartbeat(_ context.Context, version, desired int64, ready bool, _ string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, statusCall{version: version, desired: desired, ready: ready})
	return nil
}

func (f *fakeStatusStore) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

func TestPublisherRunsIndependentlyAndUsesLatestState(t *testing.T) {
	store := &fakeStatusStore{}
	path := filepath.Join(t.TempDir(), "engine-heartbeat")
	publisher := NewPublisher(store)
	publisher.Update(PublisherState{Path: path, Version: 1, DesiredVersion: 2, Ready: true})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go publisher.Run(ctx, 10*time.Millisecond, nil)
	deadline := time.Now().Add(time.Second)
	for store.count() < 2 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if store.count() < 2 {
		t.Fatal("publisher did not continue while the caller was idle")
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("heartbeat file: %v", err)
	}
	publisher.Update(PublisherState{Path: path, Version: 2, DesiredVersion: 2, Ready: true})
	if state := publisher.State(); state.Version != 2 || state.DesiredVersion != 2 {
		t.Fatalf("state=%+v", state)
	}
}
