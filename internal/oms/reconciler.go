package oms

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"fluxmaker/internal/domain"
	"fluxmaker/internal/num"
	"fluxmaker/internal/venue"
)

const managedPrefix = "fm-"

// Large books are converged incrementally so one engine tick cannot burst
// hundreds of REST requests or empty the whole market during a reprice.
const maxOrderMutationsPerCycle = 20
const maxBatchCancelSize = 20

const (
	pendingCreateLookupDelay = 2 * time.Second
	pendingCreateMaxAge      = 30 * time.Second
	pendingCancelRetryDelay  = 5 * time.Second
	maxCancelAttempts        = 3
)

type Result struct {
	Kept     int `json:"kept"`
	Canceled int `json:"canceled"`
	Placed   int `json:"placed"`
	Pending  int `json:"pending"`
}

// RefreshPolicy applies only to small, near-price maintenance changes. A
// material price move remains urgent and can use the normal mutation batch.
type RefreshPolicy struct {
	MinOrderAge          time.Duration
	MaxOrderAge          time.Duration
	MaxRefreshesPerCycle int
}

type pendingCreate struct {
	OrderID     string
	Quote       domain.Quote
	SubmittedAt time.Time
	LastChecked time.Time
}

type pendingCancelBatch struct {
	OrderIDs    map[string]struct{}
	SubmittedAt time.Time
	Attempts    int
}

type StateStore interface {
	LoadOMSState(ctx context.Context, key string) ([]byte, error)
	SaveOMSState(ctx context.Context, key string, payload []byte) error
	DeleteOMSState(ctx context.Context, key string) error
}

// WriteGuard is evaluated immediately before every exchange mutation. A
// fencing-aware engine uses it to reject writes from an expired lease holder.
type WriteGuard func(context.Context) error

type persistedState struct {
	Blocked        string                   `json:"blocked,omitempty"`
	BlockedAtMs    int64                    `json:"blocked_at_ms,omitempty"`
	PendingCancels *pendingCancelBatch      `json:"pending_cancels,omitempty"`
	PendingCreates map[string]pendingCreate `json:"pending_creates,omitempty"`
}

// blockRecoveryDelay is the settle window after an uncertain submit/cancel (e.g. a
// request timeout). We block the market so we don't blindly retry a batch that may
// have partially landed; once this window passes the exchange has resolved the
// outcome and the fresh openOrders read each cycle is authoritative (managesAllOrders
// venues, or our deterministic clientID), so the block self-heals instead of wedging.
const blockRecoveryDelay = 15 * time.Second

type Reconciler struct {
	mu             sync.Mutex
	blocked        map[string]error
	blockedAt      map[string]time.Time
	pendingCancels map[string]pendingCancelBatch
	pendingCreates map[string]map[string]pendingCreate
	loaded         map[string]bool
	store          StateStore
}

func New() *Reconciler {
	return NewWithStateStore(nil)
}

func NewWithStateStore(store StateStore) *Reconciler {
	return &Reconciler{blocked: map[string]error{}, blockedAt: map[string]time.Time{}, pendingCancels: map[string]pendingCancelBatch{}, pendingCreates: map[string]map[string]pendingCreate{}, loaded: map[string]bool{}, store: store}
}

func (r *Reconciler) Reconcile(ctx context.Context, client venue.Client, instrumentID string, quotes []domain.Quote, repriceThresholdBPS int) (Result, error) {
	if len(quotes) == 0 {
		return Result{}, fmt.Errorf("empty quote target")
	}
	orders, err := client.OpenOrders(ctx, quotes[0].Symbol)
	if err != nil {
		return Result{}, err
	}
	return r.ReconcileWithOrders(ctx, client, instrumentID, quotes, repriceThresholdBPS, orders)
}

func (r *Reconciler) ReconcileWithOrders(ctx context.Context, client venue.Client, instrumentID string, quotes []domain.Quote, repriceThresholdBPS int, orders []domain.Order) (Result, error) {
	return r.ReconcileWithOrdersGuarded(ctx, client, instrumentID, quotes, repriceThresholdBPS, orders, nil, 0)
}

func (r *Reconciler) ReconcileWithOrdersGuarded(ctx context.Context, client venue.Client, instrumentID string, quotes []domain.Quote, repriceThresholdBPS int, orders []domain.Order, guard WriteGuard, fenceGeneration uint64) (Result, error) {
	return r.ReconcileWithOrdersGuardedPolicy(ctx, client, instrumentID, quotes, repriceThresholdBPS, orders, guard, fenceGeneration, RefreshPolicy{})
}

func (r *Reconciler) ReconcileWithOrdersGuardedPolicy(ctx context.Context, client venue.Client, instrumentID string, quotes []domain.Quote, repriceThresholdBPS int, orders []domain.Order, guard WriteGuard, fenceGeneration uint64, refresh RefreshPolicy) (Result, error) {
	key := stateKey(client, instrumentID)
	if err := r.ensureLoaded(ctx, key); err != nil {
		return Result{}, fmt.Errorf("load OMS state: %w", err)
	}
	r.mu.Lock()
	blocked := r.blocked[key]
	since := r.blockedAt[key]
	r.mu.Unlock()
	if blocked != nil {
		if time.Since(since) < blockRecoveryDelay {
			return Result{}, fmt.Errorf("venue market blocked after uncertain order state: %w", blocked)
		}
		// Settle window elapsed: drop the block and resync against real openOrders below.
		r.mu.Lock()
		delete(r.blocked, key)
		delete(r.blockedAt, key)
		r.mu.Unlock()
		if err := r.persist(ctx, key); err != nil {
			return Result{}, fmt.Errorf("persist OMS state: %w", err)
		}
	}
	if len(quotes) == 0 {
		return Result{}, fmt.Errorf("empty quote target")
	}
	symbol := quotes[0].Symbol
	managed := ManagedOrdersFor(client, orders)
	waiting, err := r.waitingForCancelConfirmation(key, managed)
	if persistErr := r.persist(ctx, key); persistErr != nil {
		return Result{}, persistErr
	}
	if err != nil {
		r.block(ctx, key, err)
		return Result{}, err
	}
	if waiting {
		return Result{}, nil
	}
	pending, err := r.activePendingCreates(ctx, client, key, symbol, managed)
	if err != nil {
		r.block(ctx, key, err)
		return Result{}, err
	}

	matchedQuotes := make([]bool, len(quotes))
	matchedOrders := make([]bool, len(managed))
	result := Result{Pending: len(pending)}
	for _, creation := range pending {
		for i, quote := range quotes {
			if !matchedQuotes[i] && sameTarget(creation.Quote, quote) {
				matchedQuotes[i] = true
				break
			}
		}
	}
	scheduledRefresh := make(map[int]bool)
	refreshCount := 0
	if refresh.MaxRefreshesPerCycle > 0 && refresh.MaxOrderAge > 0 {
		expired := make([]int, 0, len(managed))
		for index, order := range managed {
			if !order.CreatedAt.IsZero() && time.Since(order.CreatedAt) >= refresh.MaxOrderAge {
				expired = append(expired, index)
			}
		}
		sort.Slice(expired, func(i, j int) bool { return managed[expired[i]].CreatedAt.Before(managed[expired[j]].CreatedAt) })
		for _, index := range expired {
			if refreshCount >= refresh.MaxRefreshesPerCycle {
				break
			}
			order := managed[index]
			remaining := order.Quantity.Sub(order.ExecutedQty)
			for quoteIndex, quote := range quotes {
				if matchedQuotes[quoteIndex] || order.Side != quote.Side || remaining.Cmp(quote.Quantity) != 0 || !withinBPS(order.Price, quote.Price, repriceThresholdBPS) {
					continue
				}
				// Reserve the target so a simultaneous vacancy cannot place a
				// duplicate before this aged order is confirmed canceled.
				matchedQuotes[quoteIndex] = true
				scheduledRefresh[index] = true
				refreshCount++
				break
			}
		}
	}
	for i, order := range managed {
		if scheduledRefresh[i] {
			continue
		}
		// remaining is loop-invariant across quotes; compute it once per order
		// instead of allocating a big.Rat subtraction on every pairing.
		remaining := order.Quantity.Sub(order.ExecutedQty)
		for j, quote := range quotes {
			if matchedQuotes[j] || order.Side != quote.Side {
				continue
			}
			// The exact-quantity comparison is a single cheap check; gate the
			// costlier basis-point price comparison behind it so withinBPS never
			// runs for a quote whose size cannot match this order anyway.
			if remaining.Cmp(quote.Quantity) != 0 {
				continue
			}
			if withinBPS(order.Price, quote.Price, repriceThresholdBPS) {
				matchedOrders[i], matchedQuotes[j] = true, true
				result.Kept++
				break
			}
		}
	}
	if refresh.MaxRefreshesPerCycle > 0 {
		// Quantity and tiny jitter changes are routine refreshes, not urgent
		// reprices. Pair them by side and nearby price, then rotate only the
		// configured number. Young orders and overflow are temporarily kept.
		for i, order := range managed {
			if matchedOrders[i] || scheduledRefresh[i] || order.ExecutedQty.IsPositive() {
				continue
			}
			for j, quote := range quotes {
				if matchedQuotes[j] || order.Side != quote.Side || !withinBPS(order.Price, quote.Price, repriceThresholdBPS) {
					continue
				}
				young := refresh.MinOrderAge > 0 && !order.CreatedAt.IsZero() && time.Since(order.CreatedAt) < refresh.MinOrderAge
				matchedQuotes[j] = true
				if young || refreshCount >= refresh.MaxRefreshesPerCycle {
					matchedOrders[i] = true
					result.Kept++
				} else {
					scheduledRefresh[i] = true
					refreshCount++
				}
				break
			}
		}
	}

	// Fill confirmed vacancies before canceling another stale batch. During a
	// large reprice this alternates cancel and place cycles, keeping most depth
	// continuously visible while respecting asynchronous cancel semantics.
	vacancies := len(quotes) - len(managed) - len(pending)
	if vacancies > 0 {
		return r.placeMissing(ctx, client, key, instrumentID, quotes, matchedQuotes, min(vacancies, maxOrderMutationsPerCycle), result, guard, fenceGeneration)
	}

	// Never place replacements in the same cycle as cancellations. MGBX cancel
	// acknowledgement is asynchronous, so a later cycle must observe removal.
	pendingCancel := make(map[string]struct{})
	toCancel := make([]domain.Order, 0, maxOrderMutationsPerCycle)
	// Materially stale orders are safety-sensitive and take priority over
	// periodic maintenance refreshes.
	for i, order := range managed {
		if matchedOrders[i] || scheduledRefresh[i] || len(toCancel) >= maxOrderMutationsPerCycle {
			continue
		}
		toCancel = append(toCancel, order)
	}
	for i, order := range managed {
		if !scheduledRefresh[i] || len(toCancel) >= maxOrderMutationsPerCycle {
			continue
		}
		toCancel = append(toCancel, order)
	}
	result.Canceled = len(toCancel)
	if result.Canceled > 0 {
		if err := cancelOrders(ctx, client, symbol, toCancel, guard); err != nil {
			r.block(ctx, key, fmt.Errorf("cancel order batch: %w", err))
			return Result{Kept: result.Kept, Pending: result.Pending}, err
		}
		for _, order := range toCancel {
			pendingCancel[order.OrderID] = struct{}{}
		}
		r.setPendingCancels(key, pendingCancel)
		if err := r.persist(ctx, key); err != nil {
			return result, fmt.Errorf("persist pending cancels: %w", err)
		}
		return result, nil
	}

	// At this point every visible order already matches a target. Any remaining
	// unmatched target is occupied by an asynchronous create that has not become
	// visible yet, so placing again would create a duplicate.
	return result, nil
}

func (r *Reconciler) placeMissing(ctx context.Context, client venue.Client, key, instrumentID string, quotes []domain.Quote, matched []bool, limit int, result Result, guard WriteGuard, fenceGeneration uint64) (Result, error) {
	selectedQuotes := make([]domain.Quote, 0, limit)
	requests := make([]venue.PlaceRequest, 0, limit)
	for i, quote := range quotes {
		if matched[i] || len(requests) >= limit {
			continue
		}
		request := venue.PlaceRequest{Symbol: quote.Symbol, Side: quote.Side, Price: quote.Price, Quantity: quote.Quantity, ClientID: clientID(instrumentID, quote, fenceGeneration), FenceGeneration: fenceGeneration}
		selectedQuotes = append(selectedQuotes, quote)
		requests = append(requests, request)
	}
	var guardErr error
	orders, submitErr := venue.PlacePostOnlyBatch(ctx, client, requests, func(ctx context.Context) error {
		guardErr = checkWriteGuard(ctx, guard)
		return guardErr
	})
	confirmed := min(len(orders), len(selectedQuotes))
	for index := 0; index < confirmed; index++ {
		order := orders[index]
		if order.OrderID == "" {
			continue
		}
		if isActiveOrder(order.State) {
			r.addPendingCreate(key, pendingCreate{OrderID: order.OrderID, Quote: selectedQuotes[index], SubmittedAt: time.Now().UTC()})
			if err := r.persist(ctx, key); err != nil {
				r.block(ctx, key, fmt.Errorf("persist accepted order %s: %w", order.OrderID, err))
				return result, err
			}
			result.Pending++
		}
		result.Placed++
	}
	if submitErr != nil {
		if guardErr != nil && errors.Is(submitErr, guardErr) {
			return result, submitErr
		}
		err := fmt.Errorf("submit batch may be partially unknown: %w", submitErr)
		r.block(ctx, key, err)
		return result, submitErr
	}
	if len(orders) != len(requests) {
		err := fmt.Errorf("venue returned %d order results for %d requests", len(orders), len(requests))
		r.block(ctx, key, err)
		return result, err
	}
	for index, order := range orders {
		if order.OrderID == "" {
			err := fmt.Errorf("venue returned an empty order id for batch item %d", index)
			r.block(ctx, key, err)
			return result, err
		}
	}
	return result, nil
}

func (r *Reconciler) activePendingCreates(ctx context.Context, client venue.Client, key, symbol string, orders []domain.Order) ([]pendingCreate, error) {
	visible := make(map[string]struct{}, len(orders))
	for _, order := range orders {
		visible[order.OrderID] = struct{}{}
	}
	now := time.Now().UTC()
	r.mu.Lock()
	for orderID := range visible {
		delete(r.pendingCreates[key], orderID)
	}
	values := make([]pendingCreate, 0, len(r.pendingCreates[key]))
	for _, creation := range r.pendingCreates[key] {
		values = append(values, creation)
	}
	r.mu.Unlock()
	if err := r.persist(ctx, key); err != nil {
		return nil, err
	}

	reader, canLookup := client.(venue.OrderReader)
	active := make([]pendingCreate, 0, len(values))
	for _, creation := range values {
		age := now.Sub(creation.SubmittedAt)
		shouldLookup := canLookup && age >= pendingCreateLookupDelay && (creation.LastChecked.IsZero() || now.Sub(creation.LastChecked) >= pendingCreateLookupDelay)
		if shouldLookup {
			order, err := reader.Order(ctx, symbol, creation.OrderID)
			creation.LastChecked = now
			if err == nil && !isActiveOrder(order.State) {
				r.removePendingCreate(key, creation.OrderID)
				if persistErr := r.persist(ctx, key); persistErr != nil {
					return nil, persistErr
				}
				continue
			}
			if err != nil && age >= pendingCreateMaxAge {
				return nil, fmt.Errorf("pending order %s status remains uncertain after %s: %w", creation.OrderID, age.Round(time.Second), err)
			}
			r.addPendingCreate(key, creation)
			if persistErr := r.persist(ctx, key); persistErr != nil {
				return nil, persistErr
			}
		} else if !canLookup && age >= pendingCreateMaxAge {
			return nil, fmt.Errorf("pending order %s was not confirmed after %s", creation.OrderID, age.Round(time.Second))
		}
		active = append(active, creation)
	}
	return active, nil
}

func (r *Reconciler) addPendingCreate(key string, creation pendingCreate) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.pendingCreates[key] == nil {
		r.pendingCreates[key] = make(map[string]pendingCreate)
	}
	r.pendingCreates[key][creation.OrderID] = creation
}

func (r *Reconciler) removePendingCreate(key, orderID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.pendingCreates[key], orderID)
}

func sameTarget(left, right domain.Quote) bool {
	return left.Side == right.Side && left.Price.Cmp(right.Price) == 0 && left.Quantity.Cmp(right.Quantity) == 0
}

func isActiveOrder(state domain.OrderState) bool {
	return state == domain.OrderNew || state == domain.OrderPartiallyFilled || state == domain.OrderUnknown
}

func (r *Reconciler) waitingForCancelConfirmation(key string, orders []domain.Order) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	pending, ok := r.pendingCancels[key]
	if !ok || len(pending.OrderIDs) == 0 {
		return false, nil
	}
	remaining := make(map[string]struct{})
	for _, order := range orders {
		if _, ok := pending.OrderIDs[order.OrderID]; ok {
			remaining[order.OrderID] = struct{}{}
		}
	}
	if len(remaining) == 0 {
		delete(r.pendingCancels, key)
		return false, nil
	}
	pending.OrderIDs = remaining
	r.pendingCancels[key] = pending
	if time.Since(pending.SubmittedAt) < pendingCancelRetryDelay {
		return true, nil
	}
	if pending.Attempts >= maxCancelAttempts {
		return false, fmt.Errorf("orders remain open after %d cancellation attempts", pending.Attempts)
	}
	return false, nil
}

func (r *Reconciler) setPendingCancels(key string, pending map[string]struct{}) {
	r.mu.Lock()
	defer r.mu.Unlock()
	attempts := 1
	if current, ok := r.pendingCancels[key]; ok {
		attempts = current.Attempts + 1
	}
	r.pendingCancels[key] = pendingCancelBatch{OrderIDs: pending, SubmittedAt: time.Now().UTC(), Attempts: attempts}
}

func (r *Reconciler) CancelManaged(ctx context.Context, client venue.Client, instrumentID, symbol string) error {
	return r.CancelManagedGuarded(ctx, client, instrumentID, symbol, nil)
}

func (r *Reconciler) CancelManagedGuarded(ctx context.Context, client venue.Client, instrumentID, symbol string, guard WriteGuard) error {
	_, err := r.CancelManagedGuardedWithResult(ctx, client, instrumentID, symbol, guard)
	return err
}

func (r *Reconciler) CancelManagedGuardedWithResult(ctx context.Context, client venue.Client, instrumentID, symbol string, guard WriteGuard) (int, error) {
	orders, err := client.OpenOrders(ctx, symbol)
	if err != nil {
		return 0, err
	}
	key := stateKey(client, instrumentID)
	if err := r.ensureLoaded(ctx, key); err != nil {
		return 0, fmt.Errorf("load OMS state: %w", err)
	}
	managed := ManagedOrdersFor(client, orders)
	waiting, waitErr := r.waitingForCancelConfirmation(key, managed)
	if persistErr := r.persist(ctx, key); persistErr != nil {
		return 0, persistErr
	}
	if waitErr != nil {
		return 0, waitErr
	}
	if waiting {
		return 0, nil
	}
	r.mu.Lock()
	seen := make(map[string]struct{}, len(managed)+len(r.pendingCreates[key]))
	for _, order := range managed {
		seen[order.OrderID] = struct{}{}
	}
	for orderID, creation := range r.pendingCreates[key] {
		if _, ok := seen[orderID]; !ok {
			managed = append(managed, domain.Order{OrderID: orderID, Symbol: creation.Quote.Symbol})
			seen[orderID] = struct{}{}
		}
	}
	r.mu.Unlock()
	if err := cancelOrders(ctx, client, symbol, managed, guard); err != nil {
		r.block(ctx, key, err)
		return 0, err
	}
	pending := make(map[string]struct{}, len(managed))
	for _, order := range managed {
		pending[order.OrderID] = struct{}{}
	}
	r.setPendingCancels(key, pending)
	r.mu.Lock()
	delete(r.pendingCreates, key)
	r.mu.Unlock()
	return len(managed), r.persist(ctx, key)
}

func (r *Reconciler) ClearBlocked(ctx context.Context, client venue.Client, instrumentID string) error {
	key := stateKey(client, instrumentID)
	if err := r.ensureLoaded(ctx, key); err != nil {
		return err
	}
	r.mu.Lock()
	delete(r.blocked, key)
	delete(r.blockedAt, key)
	r.mu.Unlock()
	return r.persist(ctx, key)
}

func ManagedOrders(orders []domain.Order, manageAll bool) []domain.Order {
	managed := make([]domain.Order, 0, len(orders))
	for _, order := range orders {
		if manageAll || IsManaged(order) {
			managed = append(managed, order)
		}
	}
	return managed
}

func ManagedOrdersFor(client venue.Client, orders []domain.Order) []domain.Order {
	return ManagedOrders(orders, venue.ManagesAllOrders(client))
}

func IsManaged(order domain.Order) bool {
	return strings.HasPrefix(order.ClientID, managedPrefix)
}

func cancelOrders(ctx context.Context, client venue.Client, symbol string, orders []domain.Order, guard WriteGuard) error {
	if len(orders) == 0 {
		return nil
	}
	if batch, ok := client.(venue.BatchCanceler); ok {
		ids := make([]string, 0, len(orders))
		for _, order := range orders {
			ids = append(ids, order.OrderID)
		}
		for start := 0; start < len(ids); start += maxBatchCancelSize {
			if err := checkWriteGuard(ctx, guard); err != nil {
				return err
			}
			end := min(start+maxBatchCancelSize, len(ids))
			if err := batch.CancelOrders(ctx, symbol, ids[start:end]); err != nil {
				return err
			}
		}
		return nil
	}
	for _, order := range orders {
		if err := checkWriteGuard(ctx, guard); err != nil {
			return err
		}
		if err := client.CancelOrder(ctx, symbol, order.OrderID); err != nil {
			return fmt.Errorf("cancel order %s: %w", order.OrderID, err)
		}
	}
	return nil
}

func checkWriteGuard(ctx context.Context, guard WriteGuard) error {
	if guard == nil {
		return nil
	}
	if err := guard(ctx); err != nil {
		return fmt.Errorf("exchange write rejected by fencing guard: %w", err)
	}
	return nil
}

func (r *Reconciler) block(ctx context.Context, key string, err error) {
	r.mu.Lock()
	r.blocked[key] = err
	r.blockedAt[key] = time.Now().UTC()
	r.mu.Unlock()
	_ = r.persist(ctx, key)
}

func stateKey(client venue.Client, instrumentID string) string {
	identity := client.Name()
	if provider, ok := client.(venue.StateIdentity); ok && provider.StateIdentity() != "" {
		identity = provider.StateIdentity()
	}
	return identity + ":" + instrumentID
}

func (r *Reconciler) ensureLoaded(ctx context.Context, key string) error {
	r.mu.Lock()
	if r.loaded[key] {
		r.mu.Unlock()
		return nil
	}
	r.loaded[key] = true
	r.mu.Unlock()
	if r.store == nil {
		return nil
	}
	payload, err := r.store.LoadOMSState(ctx, key)
	if err != nil || len(payload) == 0 {
		if err != nil {
			r.mu.Lock()
			delete(r.loaded, key)
			r.mu.Unlock()
		}
		return err
	}
	var saved persistedState
	if err := json.Unmarshal(payload, &saved); err != nil {
		r.mu.Lock()
		delete(r.loaded, key)
		r.mu.Unlock()
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if saved.Blocked != "" {
		r.blocked[key] = fmt.Errorf("%s", saved.Blocked)
		if saved.BlockedAtMs > 0 {
			r.blockedAt[key] = time.UnixMilli(saved.BlockedAtMs).UTC()
		}
	}
	if saved.PendingCancels != nil {
		r.pendingCancels[key] = *saved.PendingCancels
	}
	if len(saved.PendingCreates) > 0 {
		r.pendingCreates[key] = saved.PendingCreates
	}
	return nil
}

func (r *Reconciler) persist(ctx context.Context, key string) error {
	if r.store == nil {
		return nil
	}
	r.mu.Lock()
	state := persistedState{PendingCreates: r.pendingCreates[key]}
	if blocked := r.blocked[key]; blocked != nil {
		state.Blocked = blocked.Error()
		if at, ok := r.blockedAt[key]; ok {
			state.BlockedAtMs = at.UnixMilli()
		}
	}
	if pending, ok := r.pendingCancels[key]; ok && len(pending.OrderIDs) > 0 {
		copy := pending
		state.PendingCancels = &copy
	}
	empty := state.Blocked == "" && state.PendingCancels == nil && len(state.PendingCreates) == 0
	payload, err := json.Marshal(state)
	r.mu.Unlock()
	if empty {
		return r.store.DeleteOMSState(ctx, key)
	}
	if err != nil {
		return err
	}
	return r.store.SaveOMSState(ctx, key, payload)
}

func withinBPS(a, b num.Decimal, threshold int) bool {
	if a.IsZero() || b.IsZero() {
		return false
	}
	// |a-b|/b <= threshold/10000, cross-multiplied to |a-b|*10000 <= threshold*b
	// so no division is needed. b is a positive price at every call site, so the
	// inequality direction is preserved; big.Rat keeps both sides exact.
	deviation := a.Sub(b).Abs().Mul(num.TenThousand())
	tolerance := b.Mul(num.FromInt64(int64(threshold)))
	return deviation.Cmp(tolerance) <= 0
}

func clientID(instrumentID string, quote domain.Quote, fenceGeneration uint64) string {
	seed := fmt.Sprintf("%s|%s|%s|%d|%d|%d", instrumentID, quote.Venue, quote.Side, quote.Level, fenceGeneration, time.Now().UnixNano())
	sum := sha256.Sum256([]byte(seed))
	return managedPrefix + "g" + strconv.FormatUint(fenceGeneration, 36) + "-" + hex.EncodeToString(sum[:8])
}
