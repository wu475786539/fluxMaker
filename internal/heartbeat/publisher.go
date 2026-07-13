package heartbeat

import (
	"context"
	"errors"
	"sync"
	"time"
)

type StatusStore interface {
	Heartbeat(ctx context.Context, version, desiredVersion int64, ready bool, errorText string) error
}

type PublisherState struct {
	Path           string
	Version        int64
	DesiredVersion int64
	Ready          bool
	Error          string
}

// Publisher owns liveness publication independently from the trading loop, so
// long but bounded exchange reconciliation cannot starve the watchdog signal.
type Publisher struct {
	store StatusStore
	mu    sync.RWMutex
	state PublisherState
}

func NewPublisher(store StatusStore) *Publisher {
	return &Publisher{store: store}
}

func (p *Publisher) Update(state PublisherState) {
	p.mu.Lock()
	p.state = state
	p.mu.Unlock()
}

func (p *Publisher) State() PublisherState {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.state
}

func (p *Publisher) Publish(ctx context.Context) error {
	state := p.State()
	var result error
	if p.store != nil {
		result = p.store.Heartbeat(ctx, state.Version, state.DesiredVersion, state.Ready, state.Error)
	}
	if state.Ready {
		result = errors.Join(result, Touch(state.Path))
	}
	return result
}

func (p *Publisher) Run(ctx context.Context, interval time.Duration, onError func(error)) {
	if interval <= 0 {
		interval = 2 * time.Second
	}
	publish := func() {
		if err := p.Publish(ctx); err != nil && onError != nil {
			onError(err)
		}
	}
	publish()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			publish()
		}
	}
}
