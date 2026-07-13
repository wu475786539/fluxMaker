package engine

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"

	"fluxmaker/internal/domain"
	"fluxmaker/internal/num"
)

// countingBalanceVenue counts how many account lookups reach the exchange so a
// test can assert the per-cycle cache collapses duplicate requests.
type countingBalanceVenue struct {
	controlVenue
	calls int64
}

func (v *countingBalanceVenue) Balances(context.Context) ([]domain.Balance, error) {
	atomic.AddInt64(&v.calls, 1)
	return []domain.Balance{{Asset: "TOKEN", Free: num.Must("10")}}, nil
}

func TestBalanceCacheDeduplicatesPerAccount(t *testing.T) {
	cache := newBalanceCache()
	client := &countingBalanceVenue{}
	key := accountCacheKey("binance", 7)
	for i := 0; i < 5; i++ {
		balances, err := cache.fetch(context.Background(), key, client)
		if err != nil {
			t.Fatalf("fetch: %v", err)
		}
		if len(balances) != 1 {
			t.Fatalf("expected cached balances, got %d entries", len(balances))
		}
	}
	if got := atomic.LoadInt64(&client.calls); got != 1 {
		t.Fatalf("expected a single account lookup for a shared key, got %d", got)
	}
	// A distinct account performs its own lookup.
	if _, err := cache.fetch(context.Background(), accountCacheKey("binance", 8), client); err != nil {
		t.Fatalf("fetch other key: %v", err)
	}
	if got := atomic.LoadInt64(&client.calls); got != 2 {
		t.Fatalf("expected 2 lookups across two accounts, got %d", got)
	}
}

// TestBalanceCacheConcurrentDistinctKeys exercises the cache mutex under -race
// for accounts that fetch in parallel (different credentials are not serialized
// by account locks).
func TestBalanceCacheConcurrentDistinctKeys(t *testing.T) {
	cache := newBalanceCache()
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(id int64) {
			defer wg.Done()
			client := &countingBalanceVenue{}
			if _, err := cache.fetch(context.Background(), accountCacheKey("binance", id), client); err != nil {
				t.Errorf("fetch: %v", err)
			}
		}(int64(i))
	}
	wg.Wait()
}
