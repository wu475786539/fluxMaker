package com.fluxmaker.oms;

import com.fluxmaker.domain.Domain;
import com.fluxmaker.json.Json;
import com.fluxmaker.math.DecimalValue;
import com.fluxmaker.runtime.RuntimeStore;
import com.fluxmaker.venue.VenueClient;

import java.nio.charset.StandardCharsets;
import java.security.MessageDigest;
import java.security.NoSuchAlgorithmException;
import java.time.Duration;
import java.time.Instant;
import java.util.ArrayList;
import java.util.Comparator;
import java.util.HashMap;
import java.util.HexFormat;
import java.util.LinkedHashMap;
import java.util.LinkedHashSet;
import java.util.List;
import java.util.Map;
import java.util.Set;

public final class Reconciler {
    public static final String MANAGED_PREFIX = "fm-";
    private static final int MAX_MUTATIONS = 20, MAX_BATCH_CANCEL = 20;
    private static final Duration LOOKUP_DELAY = Duration.ofSeconds(2), CREATE_MAX_AGE = Duration.ofSeconds(30), CANCEL_RETRY_DELAY = Duration.ofSeconds(5);
    private static final int MAX_CANCEL_ATTEMPTS = 3;
    private final RuntimeStore store;
    private final Map<String, String> blocked = new HashMap<>();
    private final Map<String, PendingCancelBatch> pendingCancels = new HashMap<>();
    private final Map<String, Map<String, PendingCreate>> pendingCreates = new HashMap<>();
    private final Set<String> loaded = new LinkedHashSet<>();

    public static final class Result {
        public int kept, canceled, placed, pending;
        public Result() {}
        public Result(int kept, int canceled, int placed, int pending) { this.kept = kept; this.canceled = canceled; this.placed = placed; this.pending = pending; }
    }
    public record RefreshPolicy(Duration minOrderAge, Duration maxOrderAge, int maxRefreshesPerCycle) {
        public RefreshPolicy {
            minOrderAge = minOrderAge == null ? Duration.ZERO : minOrderAge;
            maxOrderAge = maxOrderAge == null ? Duration.ZERO : maxOrderAge;
        }
        public static RefreshPolicy disabled() { return new RefreshPolicy(Duration.ZERO, Duration.ZERO, 0); }
    }
    public static final class PendingCreate { public String orderId; public Domain.Quote quote; public Instant submittedAt; public Instant lastChecked; }
    public static final class PendingCancelBatch { public Map<String, Object> orderIds = new LinkedHashMap<>(); public Instant submittedAt; public int attempts; }
    public static final class PersistedState { public String blocked; public PendingCancelBatch pendingCancels; public Map<String, PendingCreate> pendingCreates; }
    @FunctionalInterface public interface WriteGuard { void check(); }

    public Reconciler(RuntimeStore store) { this.store = store; }

    public Result reconcile(VenueClient client, String instrumentId, List<Domain.Quote> quotes, int thresholdBps) {
        if (quotes.isEmpty()) throw new IllegalArgumentException("empty quote target"); return reconcileWithOrders(client, instrumentId, quotes, thresholdBps, client.openOrders(quotes.getFirst().symbol), null, 0);
    }

    public synchronized Result reconcileWithOrders(VenueClient client, String instrumentId, List<Domain.Quote> quotes,
                                                    int thresholdBps, List<Domain.Order> orders, WriteGuard guard,
                                                    long fenceGeneration) {
        return reconcileWithOrders(client, instrumentId, quotes, thresholdBps, orders, guard, fenceGeneration, RefreshPolicy.disabled());
    }

    public synchronized Result reconcileWithOrders(VenueClient client, String instrumentId, List<Domain.Quote> quotes,
                                                    int thresholdBps, List<Domain.Order> orders, WriteGuard guard,
                                                    long fenceGeneration, RefreshPolicy refresh) {
        String key = stateKey(client, instrumentId); ensureLoaded(key); if (blocked.containsKey(key)) throw new IllegalStateException("venue market blocked after uncertain order state: " + blocked.get(key)); if (quotes.isEmpty()) throw new IllegalArgumentException("empty quote target");
        String symbol = quotes.getFirst().symbol; List<Domain.Order> managed = managedOrdersFor(client, orders);
        boolean waiting = waitingForCancelConfirmation(key, managed); persist(key); if (waiting) return new Result();
        List<PendingCreate> pending = activePendingCreates(client, key, symbol, managed);
        boolean[] matchedQuotes = new boolean[quotes.size()], matchedOrders = new boolean[managed.size()]; Result result = new Result(); result.pending = pending.size();
        for (PendingCreate creation : pending) for (int index = 0; index < quotes.size(); index++) if (!matchedQuotes[index] && sameTarget(creation.quote, quotes.get(index))) { matchedQuotes[index] = true; break; }

        boolean[] scheduledRefresh = new boolean[managed.size()];
        int refreshCount = 0;
        Instant now = Instant.now();
        if (refresh.maxRefreshesPerCycle() > 0 && positive(refresh.maxOrderAge())) {
            List<Integer> expired = new ArrayList<>();
            for (int index = 0; index < managed.size(); index++) {
                Domain.Order order = managed.get(index);
                if (order.createdAt != null && Duration.between(order.createdAt, now).compareTo(refresh.maxOrderAge()) >= 0) expired.add(index);
            }
            expired.sort(Comparator.comparing(index -> managed.get(index).createdAt));
            for (int orderIndex : expired) {
                if (refreshCount >= refresh.maxRefreshesPerCycle()) break;
                Domain.Order order = managed.get(orderIndex);
                DecimalValue remaining = order.quantity.subtract(order.executedQty);
                for (int quoteIndex = 0; quoteIndex < quotes.size(); quoteIndex++) {
                    Domain.Quote quote = quotes.get(quoteIndex);
                    if (matchedQuotes[quoteIndex] || order.side != quote.side || !remaining.equals(quote.quantity) || !withinBps(order.price, quote.price, thresholdBps)) continue;
                    matchedQuotes[quoteIndex] = true;
                    scheduledRefresh[orderIndex] = true;
                    refreshCount++;
                    break;
                }
            }
        }
        for (int orderIndex = 0; orderIndex < managed.size(); orderIndex++) {
            if (scheduledRefresh[orderIndex]) continue;
            Domain.Order order = managed.get(orderIndex); DecimalValue remaining = order.quantity.subtract(order.executedQty);
            for (int quoteIndex = 0; quoteIndex < quotes.size(); quoteIndex++) { Domain.Quote quote = quotes.get(quoteIndex); if (matchedQuotes[quoteIndex] || order.side != quote.side || !remaining.equals(quote.quantity)) continue; if (withinBps(order.price, quote.price, thresholdBps)) { matchedOrders[orderIndex] = true; matchedQuotes[quoteIndex] = true; result.kept++; break; } }
        }
        if (refresh.maxRefreshesPerCycle() > 0) {
            for (int orderIndex = 0; orderIndex < managed.size(); orderIndex++) {
                Domain.Order order = managed.get(orderIndex);
                if (matchedOrders[orderIndex] || scheduledRefresh[orderIndex] || order.executedQty.isPositive()) continue;
                for (int quoteIndex = 0; quoteIndex < quotes.size(); quoteIndex++) {
                    Domain.Quote quote = quotes.get(quoteIndex);
                    if (matchedQuotes[quoteIndex] || order.side != quote.side || !withinBps(order.price, quote.price, thresholdBps)) continue;
                    boolean young = positive(refresh.minOrderAge()) && order.createdAt != null && Duration.between(order.createdAt, now).compareTo(refresh.minOrderAge()) < 0;
                    matchedQuotes[quoteIndex] = true;
                    if (young || refreshCount >= refresh.maxRefreshesPerCycle()) {
                        matchedOrders[orderIndex] = true;
                        result.kept++;
                    } else {
                        scheduledRefresh[orderIndex] = true;
                        refreshCount++;
                    }
                    break;
                }
            }
        }
        int vacancies = quotes.size() - managed.size() - pending.size(); if (vacancies > 0) return placeMissing(client, key, instrumentId, quotes, matchedQuotes, Math.min(vacancies, MAX_MUTATIONS), result, guard, fenceGeneration);
        List<Domain.Order> toCancel = new ArrayList<>();
        for (int index = 0; index < managed.size() && toCancel.size() < MAX_MUTATIONS; index++) if (!matchedOrders[index] && !scheduledRefresh[index]) toCancel.add(managed.get(index));
        for (int index = 0; index < managed.size() && toCancel.size() < MAX_MUTATIONS; index++) if (scheduledRefresh[index]) toCancel.add(managed.get(index));
        result.canceled = toCancel.size();
        if (!toCancel.isEmpty()) { try { cancelOrders(client, symbol, toCancel, guard); } catch (RuntimeException e) { block(key, "cancel order batch: " + e.getMessage()); throw e; } setPendingCancels(key, toCancel.stream().map(order -> order.orderId).toList()); persist(key); }
        return result;
    }

    private static boolean positive(Duration duration) { return !duration.isZero() && !duration.isNegative(); }

    private Result placeMissing(VenueClient client, String key, String instrumentId, List<Domain.Quote> quotes,
                                boolean[] matched, int limit, Result result, WriteGuard guard, long generation) {
        List<Domain.Quote> selected = new ArrayList<>(); List<VenueClient.PlaceRequest> requests = new ArrayList<>();
        for (int index = 0; index < quotes.size() && requests.size() < limit; index++) if (!matched[index]) { Domain.Quote quote = quotes.get(index); selected.add(quote); requests.add(new VenueClient.PlaceRequest(quote.symbol, quote.side, quote.price, quote.quantity, clientId(instrumentId, quote, generation), generation)); }
        List<Domain.Order> orders;
        try { checkGuard(guard); orders = client.capabilities().nativeBatchPlace() ? client.placePostOnlyBatch(requests) : requests.stream().map(request -> { checkGuard(guard); return client.placePostOnly(request); }).toList(); }
        catch (RuntimeException e) { block(key, "submit batch may be partially unknown: " + e.getMessage()); throw e; }
        int confirmed = Math.min(orders.size(), selected.size());
        for (int index = 0; index < confirmed; index++) { Domain.Order order = orders.get(index); if (order.orderId == null || order.orderId.isEmpty()) continue; if (active(order.state)) { PendingCreate creation = new PendingCreate(); creation.orderId = order.orderId; creation.quote = selected.get(index); creation.submittedAt = Instant.now(); pendingCreates.computeIfAbsent(key, ignored -> new LinkedHashMap<>()).put(order.orderId, creation); persist(key); result.pending++; } result.placed++; }
        if (orders.size() != requests.size()) { String error = "venue returned " + orders.size() + " order results for " + requests.size() + " requests"; block(key, error); throw new IllegalStateException(error); }
        for (int index = 0; index < orders.size(); index++) if (orders.get(index).orderId == null || orders.get(index).orderId.isEmpty()) { String error = "venue returned an empty order id for batch item " + index; block(key, error); throw new IllegalStateException(error); }
        return result;
    }

    private List<PendingCreate> activePendingCreates(VenueClient client, String key, String symbol, List<Domain.Order> orders) {
        Map<String, PendingCreate> state = pendingCreates.computeIfAbsent(key, ignored -> new LinkedHashMap<>()); for (Domain.Order order : orders) state.remove(order.orderId); persist(key); Instant now = Instant.now(); List<PendingCreate> active = new ArrayList<>();
        for (PendingCreate creation : new ArrayList<>(state.values())) {
            Duration age = Duration.between(creation.submittedAt, now); boolean canLookup = client.capabilities().orderLookup(); boolean shouldLookup = canLookup && age.compareTo(LOOKUP_DELAY) >= 0 && (creation.lastChecked == null || Duration.between(creation.lastChecked, now).compareTo(LOOKUP_DELAY) >= 0);
            if (shouldLookup) { try { Domain.Order order = client.order(symbol, creation.orderId); creation.lastChecked = now; if (!active(order.state)) { state.remove(creation.orderId); persist(key); continue; } } catch (RuntimeException e) { creation.lastChecked = now; if (age.compareTo(CREATE_MAX_AGE) >= 0) throw new IllegalStateException("pending order " + creation.orderId + " status remains uncertain after " + age.toSeconds() + "s", e); } state.put(creation.orderId, creation); persist(key); }
            else if (!canLookup && age.compareTo(CREATE_MAX_AGE) >= 0) throw new IllegalStateException("pending order " + creation.orderId + " was not confirmed after " + age.toSeconds() + "s");
            active.add(creation);
        }
        return active;
    }

    private boolean waitingForCancelConfirmation(String key, List<Domain.Order> orders) {
        PendingCancelBatch pending = pendingCancels.get(key); if (pending == null || pending.orderIds.isEmpty()) return false; Map<String, Object> remaining = new LinkedHashMap<>(); for (Domain.Order order : orders) if (pending.orderIds.containsKey(order.orderId)) remaining.put(order.orderId, Map.of()); if (remaining.isEmpty()) { pendingCancels.remove(key); return false; } pending.orderIds = remaining; pendingCancels.put(key, pending); if (Duration.between(pending.submittedAt, Instant.now()).compareTo(CANCEL_RETRY_DELAY) < 0) return true; if (pending.attempts >= MAX_CANCEL_ATTEMPTS) throw new IllegalStateException("orders remain open after " + pending.attempts + " cancellation attempts"); return false;
    }

    private void setPendingCancels(String key, List<String> ids) { PendingCancelBatch previous = pendingCancels.get(key); PendingCancelBatch pending = new PendingCancelBatch(); for (String id : ids) pending.orderIds.put(id, Map.of()); pending.submittedAt = Instant.now(); pending.attempts = previous == null ? 1 : previous.attempts + 1; pendingCancels.put(key, pending); }

    public synchronized int cancelManaged(VenueClient client, String instrumentId, String symbol, WriteGuard guard) {
        String key = stateKey(client, instrumentId); ensureLoaded(key); List<Domain.Order> managed = managedOrdersFor(client, client.openOrders(symbol)); if (waitingForCancelConfirmation(key, managed)) { persist(key); return 0; }
        Set<String> seen = new LinkedHashSet<>(); for (Domain.Order order : managed) seen.add(order.orderId); for (PendingCreate creation : pendingCreates.computeIfAbsent(key, ignored -> new LinkedHashMap<>()).values()) if (seen.add(creation.orderId)) { Domain.Order order = new Domain.Order(); order.orderId = creation.orderId; order.symbol = creation.quote.symbol; managed.add(order); }
        cancelOrders(client, symbol, managed, guard); setPendingCancels(key, managed.stream().map(order -> order.orderId).toList()); pendingCreates.remove(key); persist(key); return managed.size();
    }

    public synchronized void clearBlocked(VenueClient client, String instrumentId) { String key = stateKey(client, instrumentId); ensureLoaded(key); blocked.remove(key); persist(key); }
    public static List<Domain.Order> managedOrdersFor(VenueClient client, List<Domain.Order> orders) { if (client.managesAllOrders()) return new ArrayList<>(orders); return orders.stream().filter(Reconciler::isManaged).toList(); }
    public static boolean isManaged(Domain.Order order) { return order.clientId != null && order.clientId.startsWith(MANAGED_PREFIX); }
    public static boolean withinBps(DecimalValue left, DecimalValue right, int threshold) { if (left.isZero() || right.isZero()) return false; return left.subtract(right).abs().multiply(DecimalValue.TEN_THOUSAND).compareTo(right.multiply(DecimalValue.of(threshold))) <= 0; }

    private static void cancelOrders(VenueClient client, String symbol, List<Domain.Order> orders, WriteGuard guard) { if (orders.isEmpty()) return; if (client.capabilities().nativeBatchCancel()) { List<String> ids = orders.stream().map(order -> order.orderId).toList(); for (int start = 0; start < ids.size(); start += MAX_BATCH_CANCEL) { checkGuard(guard); client.cancelOrders(symbol, ids.subList(start, Math.min(start + MAX_BATCH_CANCEL, ids.size()))); } } else for (Domain.Order order : orders) { checkGuard(guard); client.cancelOrder(symbol, order.orderId); } }
    private static void checkGuard(WriteGuard guard) { if (guard != null) try { guard.check(); } catch (RuntimeException e) { throw new IllegalStateException("exchange write rejected by fencing guard: " + e.getMessage(), e); } }
    private static boolean sameTarget(Domain.Quote left, Domain.Quote right) { return left.side == right.side && left.price.equals(right.price) && left.quantity.equals(right.quantity); }
    private static boolean active(Domain.OrderState state) { return state == Domain.OrderState.NEW || state == Domain.OrderState.PARTIALLY_FILLED || state == Domain.OrderState.UNKNOWN; }
    private static String stateKey(VenueClient client, String instrument) { return (client.stateIdentity() == null || client.stateIdentity().isEmpty() ? client.name() : client.stateIdentity()) + ":" + instrument; }

    private void ensureLoaded(String key) { if (!loaded.add(key) || store == null) return; byte[] value = store.loadOmsState(key); if (value == null || value.length == 0) return; try { PersistedState saved = Json.read(value, PersistedState.class); if (saved.blocked != null && !saved.blocked.isEmpty()) blocked.put(key, saved.blocked); if (saved.pendingCancels != null) pendingCancels.put(key, saved.pendingCancels); if (saved.pendingCreates != null) pendingCreates.put(key, saved.pendingCreates); } catch (RuntimeException e) { loaded.remove(key); throw e; } }
    private void persist(String key) { if (store == null) return; PersistedState state = new PersistedState(); state.blocked = blocked.get(key); state.pendingCancels = pendingCancels.get(key); state.pendingCreates = pendingCreates.get(key); if ((state.blocked == null || state.blocked.isEmpty()) && state.pendingCancels == null && (state.pendingCreates == null || state.pendingCreates.isEmpty())) store.deleteOmsState(key); else store.saveOmsState(key, Json.writeBytes(state)); }
    private void block(String key, String error) { blocked.put(key, error); persist(key); }

    public static String clientId(String instrumentId, Domain.Quote quote, long generation) {
        String seed = instrumentId + "|" + quote.venue + "|" + quote.side + "|" + quote.level + "|" + generation + "|" + System.nanoTime();
        try { byte[] digest = MessageDigest.getInstance("SHA-256").digest(seed.getBytes(StandardCharsets.UTF_8)); return MANAGED_PREFIX + "g" + Long.toString(generation, 36) + "-" + HexFormat.of().formatHex(digest, 0, 8); }
        catch (NoSuchAlgorithmException e) { throw new IllegalStateException(e); }
    }
}
