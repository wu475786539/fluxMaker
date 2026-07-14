package oms

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"fluxmaker/internal/domain"
	"fluxmaker/internal/num"
	"fluxmaker/internal/venue"
)

type fakeVenue struct {
	orders         []domain.Order
	next           int
	placeErr       error
	cancelErr      error
	cancelCalls    int
	retainCanceled bool
	hidePlaced     bool
	lookup         map[string]domain.Order
	requests       []venue.PlaceRequest
}

func (f *fakeVenue) Name() string                                         { return "fake" }
func (f *fakeVenue) TopBook(context.Context, string) (domain.Book, error) { return domain.Book{}, nil }
func (f *fakeVenue) Balances(context.Context) ([]domain.Balance, error)   { return nil, nil }
func (f *fakeVenue) OpenOrders(context.Context, string) ([]domain.Order, error) {
	return append([]domain.Order(nil), f.orders...), nil
}
func (f *fakeVenue) PlacePostOnly(_ context.Context, request venue.PlaceRequest) (domain.Order, error) {
	if f.placeErr != nil {
		return domain.Order{}, f.placeErr
	}
	f.requests = append(f.requests, request)
	f.next++
	o := domain.Order{Venue: "fake", OrderID: fmt.Sprint(f.next), ClientID: request.ClientID, Symbol: request.Symbol, Side: request.Side, Price: request.Price, Quantity: request.Quantity, State: domain.OrderNew}
	if f.lookup == nil {
		f.lookup = make(map[string]domain.Order)
	}
	f.lookup[o.OrderID] = o
	if !f.hidePlaced {
		f.orders = append(f.orders, o)
	}
	return o, nil
}

func (f *fakeVenue) Order(_ context.Context, _, orderID string) (domain.Order, error) {
	order, ok := f.lookup[orderID]
	if !ok {
		return domain.Order{}, fmt.Errorf("order not found")
	}
	return order, nil
}
func (f *fakeVenue) CancelOrder(_ context.Context, _ string, orderID string) error {
	if f.cancelErr != nil {
		return f.cancelErr
	}
	f.cancelCalls++
	if f.retainCanceled {
		return nil
	}
	for i, order := range f.orders {
		if order.OrderID == orderID {
			f.orders = append(f.orders[:i], f.orders[i+1:]...)
			return nil
		}
	}
	return nil
}

type fakeBatchVenue struct {
	*fakeVenue
	batches [][]string
}

type fakeBatchPlaceVenue struct {
	*fakeVenue
	placeBatches [][]venue.PlaceRequest
	batchErr     error
	omitLast     bool
}

type memoryStateStore struct {
	mu   sync.Mutex
	data map[string][]byte
}

func (s *memoryStateStore) LoadOMSState(_ context.Context, key string) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]byte(nil), s.data[key]...), nil
}

func (s *memoryStateStore) SaveOMSState(_ context.Context, key string, payload []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.data == nil {
		s.data = make(map[string][]byte)
	}
	s.data[key] = append([]byte(nil), payload...)
	return nil
}

func (s *memoryStateStore) DeleteOMSState(_ context.Context, key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.data, key)
	return nil
}

func (f *fakeBatchVenue) CancelOrders(_ context.Context, _ string, orderIDs []string) error {
	f.batches = append(f.batches, append([]string(nil), orderIDs...))
	remove := make(map[string]bool, len(orderIDs))
	for _, id := range orderIDs {
		remove[id] = true
	}
	kept := f.orders[:0]
	for _, order := range f.orders {
		if !remove[order.OrderID] {
			kept = append(kept, order)
		}
	}
	f.orders = kept
	return nil
}

func (f *fakeBatchPlaceVenue) PlacePostOnlyBatch(_ context.Context, requests []venue.PlaceRequest) ([]domain.Order, error) {
	f.placeBatches = append(f.placeBatches, append([]venue.PlaceRequest(nil), requests...))
	orders := make([]domain.Order, len(requests))
	for index, request := range requests {
		if f.omitLast && index == len(requests)-1 {
			continue
		}
		f.requests = append(f.requests, request)
		f.next++
		order := domain.Order{Venue: "fake", OrderID: fmt.Sprint(f.next), ClientID: request.ClientID, Symbol: request.Symbol, Side: request.Side, Price: request.Price, Quantity: request.Quantity, State: domain.OrderNew}
		orders[index] = order
		f.orders = append(f.orders, order)
	}
	return orders, f.batchErr
}

func largeTarget(levels, offset int) []domain.Quote {
	valid := time.Now().Add(time.Minute)
	quotes := make([]domain.Quote, 0, levels*2)
	for level := 0; level < levels; level++ {
		quotes = append(quotes,
			domain.Quote{InstrumentID: "token_usdt", Venue: "fake", Symbol: "TOKENUSDT", Side: domain.Buy, Level: level, Price: num.FromInt64(int64(1000 - level - offset)), Quantity: num.Must("1"), ValidUntil: valid},
			domain.Quote{InstrumentID: "token_usdt", Venue: "fake", Symbol: "TOKENUSDT", Side: domain.Sell, Level: level, Price: num.FromInt64(int64(1100 + level + offset)), Quantity: num.Must("1"), ValidUntil: valid},
		)
	}
	return quotes
}

func target(priceBuy, priceSell string) []domain.Quote {
	valid := time.Now().Add(time.Minute)
	return []domain.Quote{
		{InstrumentID: "token_usdt", Venue: "fake", Symbol: "TOKENUSDT", Side: domain.Buy, Price: num.Must(priceBuy), Quantity: num.Must("1"), ValidUntil: valid},
		{InstrumentID: "token_usdt", Venue: "fake", Symbol: "TOKENUSDT", Side: domain.Sell, Price: num.Must(priceSell), Quantity: num.Must("1"), ValidUntil: valid},
	}
}

func TestReconcileCancelThenReplaceNextCycle(t *testing.T) {
	r := New()
	v := &fakeVenue{}
	ctx := context.Background()
	result, err := r.Reconcile(ctx, v, "token_usdt", target("99", "101"), 0)
	if err != nil || result.Placed != 2 {
		t.Fatalf("first=%+v err=%v", result, err)
	}
	result, err = r.Reconcile(ctx, v, "token_usdt", target("99", "101"), 0)
	if err != nil || result.Kept != 2 {
		t.Fatalf("keep=%+v err=%v", result, err)
	}
	result, err = r.Reconcile(ctx, v, "token_usdt", target("98", "102"), 0)
	if err != nil || result.Canceled != 2 || result.Placed != 0 {
		t.Fatalf("cancel=%+v err=%v", result, err)
	}
	result, err = r.Reconcile(ctx, v, "token_usdt", target("98", "102"), 0)
	if err != nil || result.Placed != 2 {
		t.Fatalf("replace=%+v err=%v", result, err)
	}
}

func TestSubmissionErrorBlocksMarket(t *testing.T) {
	r := New()
	v := &fakeVenue{placeErr: fmt.Errorf("timeout")}
	_, err := r.Reconcile(context.Background(), v, "token_usdt", target("99", "101"), 0)
	if err == nil {
		t.Fatal("expected submission error")
	}
	v.placeErr = nil
	_, err = r.Reconcile(context.Background(), v, "token_usdt", target("99", "101"), 0)
	if err == nil {
		t.Fatal("expected blocked market")
	}
}

func TestLargeBookConvergesInMutationBatches(t *testing.T) {
	r := New()
	v := &fakeVenue{}
	ctx := context.Background()
	quotes := largeTarget(60, 0)
	for cycle := 0; cycle < 6; cycle++ {
		result, err := r.Reconcile(ctx, v, "token_usdt", quotes, 0)
		if err != nil {
			t.Fatal(err)
		}
		if result.Placed > maxOrderMutationsPerCycle {
			t.Fatalf("cycle placed %d orders", result.Placed)
		}
	}
	if len(v.orders) != 120 {
		t.Fatalf("orders=%d want=120", len(v.orders))
	}

	result, err := r.Reconcile(ctx, v, "token_usdt", largeTarget(60, 500), 0)
	if err != nil {
		t.Fatal(err)
	}
	if result.Canceled != maxOrderMutationsPerCycle || len(v.orders) != 100 {
		t.Fatalf("rolling cancel=%+v remaining=%d", result, len(v.orders))
	}
	result, err = r.Reconcile(ctx, v, "token_usdt", largeTarget(60, 500), 0)
	if err != nil {
		t.Fatal(err)
	}
	if result.Placed != maxOrderMutationsPerCycle || len(v.orders) != 120 {
		t.Fatalf("rolling replace=%+v orders=%d", result, len(v.orders))
	}
}

func TestRoutineRefreshIsLimitedPerCycle(t *testing.T) {
	quotes := largeTarget(5, 0)
	orders := make([]domain.Order, len(quotes))
	for index, quote := range quotes {
		orders[index] = domain.Order{OrderID: fmt.Sprint(index + 1), ClientID: "fm-old", Symbol: quote.Symbol, Side: quote.Side, Price: quote.Price, Quantity: num.Must("2"), State: domain.OrderNew, CreatedAt: time.Now().Add(-time.Minute)}
	}
	v := &fakeVenue{orders: orders}
	result, err := New().ReconcileWithOrdersGuardedPolicy(context.Background(), v, "token_usdt", quotes, 10, orders, nil, 0, RefreshPolicy{MinOrderAge: 30 * time.Second, MaxOrderAge: 5 * time.Minute, MaxRefreshesPerCycle: 2})
	if err != nil {
		t.Fatal(err)
	}
	if result.Canceled != 2 || result.Kept != 8 || len(v.orders) != 8 {
		t.Fatalf("routine refresh result=%+v remaining=%d", result, len(v.orders))
	}
}

func TestRoutineRefreshKeepsYoungOrders(t *testing.T) {
	quotes := largeTarget(5, 0)
	orders := make([]domain.Order, len(quotes))
	for index, quote := range quotes {
		orders[index] = domain.Order{OrderID: fmt.Sprint(index + 1), ClientID: "fm-young", Symbol: quote.Symbol, Side: quote.Side, Price: quote.Price, Quantity: num.Must("2"), State: domain.OrderNew, CreatedAt: time.Now()}
	}
	v := &fakeVenue{orders: orders}
	result, err := New().ReconcileWithOrdersGuardedPolicy(context.Background(), v, "token_usdt", quotes, 10, orders, nil, 0, RefreshPolicy{MinOrderAge: 30 * time.Second, MaxOrderAge: 5 * time.Minute, MaxRefreshesPerCycle: 2})
	if err != nil {
		t.Fatal(err)
	}
	if result.Canceled != 0 || result.Kept != len(orders) {
		t.Fatalf("young refresh result=%+v", result)
	}
}

func TestMaximumOrderAgeRefreshesOldestGradually(t *testing.T) {
	quotes := largeTarget(5, 0)
	orders := make([]domain.Order, len(quotes))
	for index, quote := range quotes {
		orders[index] = domain.Order{OrderID: fmt.Sprint(index + 1), ClientID: "fm-aged", Symbol: quote.Symbol, Side: quote.Side, Price: quote.Price, Quantity: quote.Quantity, State: domain.OrderNew, CreatedAt: time.Now().Add(-10*time.Minute - time.Duration(index)*time.Second)}
	}
	v := &fakeVenue{orders: orders}
	result, err := New().ReconcileWithOrdersGuardedPolicy(context.Background(), v, "token_usdt", quotes, 10, orders, nil, 0, RefreshPolicy{MinOrderAge: 30 * time.Second, MaxOrderAge: 5 * time.Minute, MaxRefreshesPerCycle: 2})
	if err != nil {
		t.Fatal(err)
	}
	if result.Canceled != 2 || result.Kept != 8 {
		t.Fatalf("aged refresh result=%+v", result)
	}
}

func TestMaterialRepriceIsNotLimitedByRefreshPolicy(t *testing.T) {
	oldQuotes := largeTarget(5, 0)
	newQuotes := largeTarget(5, 500)
	orders := make([]domain.Order, len(oldQuotes))
	for index, quote := range oldQuotes {
		orders[index] = domain.Order{OrderID: fmt.Sprint(index + 1), ClientID: "fm-stale", Symbol: quote.Symbol, Side: quote.Side, Price: quote.Price, Quantity: quote.Quantity, State: domain.OrderNew, CreatedAt: time.Now().Add(-time.Minute)}
	}
	v := &fakeVenue{orders: orders}
	result, err := New().ReconcileWithOrdersGuardedPolicy(context.Background(), v, "token_usdt", newQuotes, 10, orders, nil, 0, RefreshPolicy{MinOrderAge: 30 * time.Second, MaxOrderAge: 5 * time.Minute, MaxRefreshesPerCycle: 2})
	if err != nil {
		t.Fatal(err)
	}
	if result.Canceled != len(orders) {
		t.Fatalf("material reprice was throttled: result=%+v", result)
	}
}

func TestWaitsForAsynchronousCancelConfirmation(t *testing.T) {
	r := New()
	v := &fakeVenue{}
	ctx := context.Background()
	if _, err := r.Reconcile(ctx, v, "token_usdt", target("99", "101"), 0); err != nil {
		t.Fatal(err)
	}
	v.retainCanceled = true
	result, err := r.Reconcile(ctx, v, "token_usdt", target("98", "102"), 0)
	if err != nil || result.Canceled != 2 {
		t.Fatalf("cancel=%+v err=%v", result, err)
	}
	if _, err := r.Reconcile(ctx, v, "token_usdt", target("98", "102"), 0); err != nil {
		t.Fatal(err)
	}
	if v.cancelCalls != 2 {
		t.Fatalf("cancel calls=%d want=2 while confirmation is pending", v.cancelCalls)
	}
	v.orders = nil
	v.retainCanceled = false
	result, err = r.Reconcile(ctx, v, "token_usdt", target("98", "102"), 0)
	if err != nil || result.Placed != 2 {
		t.Fatalf("replace after confirmation=%+v err=%v", result, err)
	}
}

func TestPendingCreateReservesTargetUntilConfirmed(t *testing.T) {
	r := New()
	v := &fakeVenue{hidePlaced: true}
	ctx := context.Background()
	result, err := r.Reconcile(ctx, v, "token_usdt", target("99", "101"), 0)
	if err != nil || result.Placed != 2 || result.Pending != 2 {
		t.Fatalf("submit=%+v err=%v", result, err)
	}
	result, err = r.Reconcile(ctx, v, "token_usdt", target("99", "101"), 0)
	if err != nil || result.Placed != 0 || result.Pending != 2 || v.next != 2 {
		t.Fatalf("pending=%+v next=%d err=%v", result, v.next, err)
	}
	key := v.Name() + ":token_usdt"
	for id, creation := range r.pendingCreates[key] {
		creation.SubmittedAt = time.Now().Add(-3 * time.Second)
		r.pendingCreates[key][id] = creation
		order := v.lookup[id]
		order.State = domain.OrderCanceled
		v.lookup[id] = order
	}
	result, err = r.Reconcile(ctx, v, "token_usdt", target("99", "101"), 0)
	if err != nil || result.Placed != 2 || v.next != 4 {
		t.Fatalf("replace terminal pending=%+v next=%d err=%v", result, v.next, err)
	}
}

func TestStalePendingTargetsDoNotOverfillNewStrategy(t *testing.T) {
	r := New()
	v := &fakeVenue{hidePlaced: true}
	ctx := context.Background()
	if _, err := r.Reconcile(ctx, v, "token_usdt", largeTarget(2, 0), 0); err != nil {
		t.Fatal(err)
	}
	if v.next != 4 {
		t.Fatalf("submitted=%d", v.next)
	}
	result, err := r.Reconcile(ctx, v, "token_usdt", target("10", "20"), 0)
	if err != nil {
		t.Fatal(err)
	}
	if result.Placed != 0 || v.next != 4 {
		t.Fatalf("new strategy was overfilled: result=%+v submitted=%d", result, v.next)
	}
}

func TestUsesNativeBatchCancel(t *testing.T) {
	r := New()
	base := &fakeVenue{}
	v := &fakeBatchVenue{fakeVenue: base}
	ctx := context.Background()
	if _, err := r.Reconcile(ctx, v, "token_usdt", target("99", "101"), 0); err != nil {
		t.Fatal(err)
	}
	result, err := r.Reconcile(ctx, v, "token_usdt", target("98", "102"), 0)
	if err != nil || result.Canceled != 2 {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	if len(v.batches) != 1 || len(v.batches[0]) != 2 || v.cancelCalls != 0 {
		t.Fatalf("batches=%v individual=%d", v.batches, v.cancelCalls)
	}
}

func TestUsesNativeBatchPlace(t *testing.T) {
	r := New()
	v := &fakeBatchPlaceVenue{fakeVenue: &fakeVenue{}}
	result, err := r.Reconcile(context.Background(), v, "token_usdt", target("99", "101"), 0)
	if err != nil || result.Placed != 2 || result.Pending != 2 {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	if len(v.placeBatches) != 1 || len(v.placeBatches[0]) != 2 || len(v.requests) != 2 {
		t.Fatalf("batches=%v requests=%v", v.placeBatches, v.requests)
	}
}

func TestNativeBatchPartialSuccessPersistsConfirmedItemsBeforeBlocking(t *testing.T) {
	r := New()
	v := &fakeBatchPlaceVenue{fakeVenue: &fakeVenue{}, omitLast: true, batchErr: fmt.Errorf("second item rejected")}
	result, err := r.Reconcile(context.Background(), v, "token_usdt", target("99", "101"), 0)
	if err == nil || result.Placed != 1 || result.Pending != 1 {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	if len(r.pendingCreates["fake:token_usdt"]) != 1 {
		t.Fatalf("pending=%v", r.pendingCreates["fake:token_usdt"])
	}
	if _, err := r.Reconcile(context.Background(), v, "token_usdt", target("99", "101"), 0); err == nil {
		t.Fatal("partially failed native batch did not block the uncertain market")
	}
}

func TestPendingCreateSurvivesReconcilerRestart(t *testing.T) {
	store := &memoryStateStore{}
	v := &fakeVenue{hidePlaced: true}
	ctx := context.Background()
	first := NewWithStateStore(store)
	if result, err := first.Reconcile(ctx, v, "token_usdt", target("99", "101"), 0); err != nil || result.Placed != 2 {
		t.Fatalf("first=%+v err=%v", result, err)
	}
	second := NewWithStateStore(store)
	result, err := second.Reconcile(ctx, v, "token_usdt", target("99", "101"), 0)
	if err != nil || result.Placed != 0 || result.Pending != 2 || v.next != 2 {
		t.Fatalf("restored=%+v submitted=%d err=%v", result, v.next, err)
	}
}

func TestFencingGuardRejectsStaleWritesAndTagsClientID(t *testing.T) {
	r := New()
	v := &fakeVenue{}
	guardErr := fmt.Errorf("stale generation")
	_, err := r.ReconcileWithOrdersGuarded(context.Background(), v, "token_usdt", target("99", "101"), 0, nil, func(context.Context) error { return guardErr }, 41)
	if err == nil || v.next != 0 {
		t.Fatalf("stale guarded write err=%v submitted=%d", err, v.next)
	}

	result, err := r.ReconcileWithOrdersGuarded(context.Background(), v, "token_usdt", target("99", "101"), 0, nil, func(context.Context) error { return nil }, 42)
	if err != nil || result.Placed != 2 {
		t.Fatalf("valid guarded write result=%+v err=%v", result, err)
	}
	for _, request := range v.requests {
		if request.FenceGeneration != 42 || !strings.HasPrefix(request.ClientID, "fm-g16-") {
			t.Fatalf("request missing fence identity: %+v", request)
		}
	}
}

func TestFencingGuardRejectsStaleCancellation(t *testing.T) {
	r := New()
	v := &fakeVenue{orders: []domain.Order{{OrderID: "1", ClientID: "fm-g1-test", Symbol: "TOKENUSDT"}}}
	err := r.CancelManagedGuarded(context.Background(), v, "token_usdt", "TOKENUSDT", func(context.Context) error { return fmt.Errorf("stale generation") })
	if err == nil || v.cancelCalls != 0 {
		t.Fatalf("stale cancel err=%v cancel_calls=%d", err, v.cancelCalls)
	}
}

func TestCancelManagedReportsSubmittedCancellationCount(t *testing.T) {
	r := New()
	v := &fakeVenue{orders: []domain.Order{
		{OrderID: "1", ClientID: "fm-g1-a", Symbol: "TOKENUSDT"},
		{OrderID: "2", ClientID: "fm-g1-b", Symbol: "TOKENUSDT"},
		{OrderID: "manual", ClientID: "manual", Symbol: "TOKENUSDT"},
	}}
	canceled, err := r.CancelManagedGuardedWithResult(context.Background(), v, "token_usdt", "TOKENUSDT", nil)
	if err != nil || canceled != 2 || len(v.orders) != 1 || v.orders[0].OrderID != "manual" {
		t.Fatalf("canceled=%d remaining=%+v err=%v", canceled, v.orders, err)
	}
}
