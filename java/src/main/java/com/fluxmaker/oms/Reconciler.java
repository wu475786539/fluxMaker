package com.fluxmaker.oms;

import com.fluxmaker.domain.Domain;
import com.fluxmaker.json.Json;
import com.fluxmaker.math.DecimalValue;
import com.fluxmaker.runtime.RuntimeStore;
import com.fluxmaker.venue.VenueClient;

import java.nio.charset.StandardCharsets;
import java.security.MessageDigest;
import java.security.NoSuchAlgorithmException;
import java.time.Clock;
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
import java.util.concurrent.ThreadLocalRandom;

public final class Reconciler {
    public static final String MANAGED_PREFIX = "fm-";
    private static final int MAX_MUTATIONS = 20, MAX_BATCH_CANCEL = 20;
    private static final Duration LOOKUP_DELAY = Duration.ofSeconds(2), CREATE_MAX_AGE = Duration.ofSeconds(30), CANCEL_RETRY_DELAY = Duration.ofSeconds(5);
    // After an uncertain submit/cancel (e.g. a request timeout) we block the market
    // so we don't blindly retry a batch that may have partially landed. Once this
    // settle window passes the exchange has resolved the outcome, and the fresh
    // openOrders we read each cycle is authoritative (managesAllOrders venues, or our
    // deterministic clientId), so the block self-heals instead of wedging forever.
    private static final Duration BLOCK_RECOVERY_DELAY = Duration.ofSeconds(15);
    private static final int MAX_CANCEL_ATTEMPTS = 3;
    private final RuntimeStore store;
    private final Clock clock;
    private final Map<String, String> blocked = new HashMap<>();
    private final Map<String, Instant> blockedAt = new HashMap<>();
    private final Map<String, PendingCancelBatch> pendingCancels = new HashMap<>();
    private final Map<String, Map<String, PendingCreate>> pendingCreates = new HashMap<>();
    private final Map<String, Map<Domain.Side, PendingVacancy>> pendingVacancies = new HashMap<>();
    private final Map<String, Map<Domain.Side, Integer>> immediateReplacementCredits = new HashMap<>();
    private final Set<String> replenishmentInitialized = new LinkedHashSet<>();
    private final Set<String> replenishmentBootstrap = new LinkedHashSet<>();
    private final Set<String> loaded = new LinkedHashSet<>();

    public static final class Result {
        public int kept, canceled, placed, pending, delayed;
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
    public record ReplenishPolicy(Duration minDelay, Duration maxDelay, int maxPerCycle) {
        public ReplenishPolicy {
            minDelay = minDelay == null ? Duration.ZERO : minDelay;
            maxDelay = maxDelay == null ? minDelay : maxDelay;
            if (minDelay.isNegative() || maxDelay.compareTo(minDelay) < 0) throw new IllegalArgumentException("invalid fill replenishment delay range");
            if (maxPerCycle < 1) throw new IllegalArgumentException("fill replenishment max per cycle must be positive");
        }
        public static ReplenishPolicy immediate() { return new ReplenishPolicy(Duration.ZERO, Duration.ZERO, MAX_MUTATIONS); }
    }
    public static final class PendingCreate { public String orderId; public Domain.Quote quote; public Instant submittedAt; public Instant lastChecked; }
    public static final class PendingCancelBatch { public Map<String, Object> orderIds = new LinkedHashMap<>(); public Instant submittedAt; public int attempts; }
    public static final class PendingVacancy { public int count; public Instant detectedAt; public Instant replenishAt; }
    public static final class PersistedState {
        public String blocked;
        public Long blockedAtEpochMs;
        public PendingCancelBatch pendingCancels;
        public Map<String, PendingCreate> pendingCreates;
        public Map<Domain.Side, PendingVacancy> pendingVacancies;
        public Map<Domain.Side, Integer> immediateReplacementCredits;
        public boolean replenishmentInitialized;
        public boolean replenishmentBootstrap;
    }
    @FunctionalInterface public interface WriteGuard { void check(); }

    public Reconciler(RuntimeStore store) { this(store, Clock.systemUTC()); }
    Reconciler(RuntimeStore store, Clock clock) { this.store = store; this.clock = clock; }

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
        return reconcileWithOrders(client, instrumentId, quotes, thresholdBps, orders, guard, fenceGeneration, refresh, false);
    }

    public synchronized Result reconcileWithOrders(VenueClient client, String instrumentId, List<Domain.Quote> quotes,
                                                    int thresholdBps, List<Domain.Order> orders, WriteGuard guard,
                                                    long fenceGeneration, RefreshPolicy refresh,
                                                    boolean gradualMaterialReprice) {
        return reconcileWithOrders(client, instrumentId, quotes, thresholdBps, orders, guard, fenceGeneration,
                refresh, gradualMaterialReprice, ReplenishPolicy.immediate());
    }

    public synchronized Result reconcileWithOrders(VenueClient client, String instrumentId, List<Domain.Quote> quotes,
                                                    int thresholdBps, List<Domain.Order> orders, WriteGuard guard,
                                                    long fenceGeneration, RefreshPolicy refresh,
                                                    boolean gradualMaterialReprice, ReplenishPolicy replenish) {
        String key = stateKey(client, instrumentId); ensureLoaded(key);
        if (blocked.containsKey(key)) {
            Instant since = blockedAt.getOrDefault(key, Instant.EPOCH);
            if (Duration.between(since, clock.instant()).compareTo(BLOCK_RECOVERY_DELAY) < 0)
                throw new IllegalStateException("venue market blocked after uncertain order state: " + blocked.get(key));
            // Settle window elapsed: drop the block and resync against real openOrders below.
            blocked.remove(key); blockedAt.remove(key); persist(key);
        }
        if (quotes.isEmpty()) throw new IllegalArgumentException("empty quote target");
        String symbol = quotes.getFirst().symbol; List<Domain.Order> managed = managedOrdersFor(client, orders);
        boolean waiting = waitingForCancelConfirmation(key, managed); persist(key); if (waiting) return new Result();
        List<PendingCreate> pending = activePendingCreates(client, key, symbol, managed);
        if (!managed.isEmpty() || !pending.isEmpty()) replenishmentInitialized.add(key);
        else if (!replenishmentInitialized.contains(key)) replenishmentBootstrap.add(key);
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
        if (gradualMaterialReprice) {
            // Remaining unmatched orders are materially outside the normal price
            // threshold. Pair them with the closest still-unmatched target on the
            // same side so a real fill leaves a vacancy on the correct side. Then
            // rotate only a bounded number of these stale orders per cycle.
            List<Integer> candidates = new ArrayList<>();
            for (int index = 0; index < managed.size(); index++) {
                if (!matchedOrders[index] && !scheduledRefresh[index]) candidates.add(index);
            }
            candidates.sort(Comparator.comparing(
                    index -> managed.get(index).createdAt,
                    Comparator.nullsFirst(Comparator.naturalOrder())
            ));
            int gradualLimit = Math.max(1, refresh.maxRefreshesPerCycle());
            for (int orderIndex : candidates) {
                Domain.Order order = managed.get(orderIndex);
                int quoteIndex = closestUnmatchedQuote(order, quotes, matchedQuotes);
                if (quoteIndex < 0) continue; // A risk/budget rule removed this side: cancel immediately below.
                matchedQuotes[quoteIndex] = true;
                boolean partiallyFilled = order.executedQty.isPositive();
                boolean young = positive(refresh.minOrderAge())
                        && order.createdAt != null
                        && Duration.between(order.createdAt, now).compareTo(refresh.minOrderAge()) < 0;
                if (partiallyFilled || (!young && refreshCount < gradualLimit)) {
                    scheduledRefresh[orderIndex] = true;
                    if (!partiallyFilled) refreshCount++;
                } else {
                    matchedOrders[orderIndex] = true;
                    result.kept++;
                }
            }
        }
        int vacancies = quotes.size() - managed.size() - pending.size();
        if (vacancies > 0) {
            // A real vacancy takes priority over routine/gradual rotation. While
            // its replenishment cooldown is active, all refresh candidates stay
            // live instead of making the visible depth shrink a second time.
            for (int index = 0; index < scheduledRefresh.length; index++) {
                if (scheduledRefresh[index] && !matchedOrders[index]) {
                    matchedOrders[index] = true;
                    result.kept++;
                }
            }
            Map<Domain.Side, Integer> deficits = sideDeficits(quotes, managed, pending);
            List<Domain.Quote> selected = selectReplenishments(
                    key, quotes, matchedQuotes, deficits, Math.min(vacancies, MAX_MUTATIONS), replenish, clock.instant());
            result.delayed = delayedVacancies(key);
            if (selected.isEmpty()) { persist(key); return result; }
            placeMissing(client, key, instrumentId, selected, result, guard, fenceGeneration);
            consumeReplenishments(key, selected);
            result.delayed = delayedVacancies(key);
            persist(key);
            return result;
        }
        clearReplenishmentState(key);
        List<Domain.Order> toCancel = new ArrayList<>();
        for (int index = 0; index < managed.size() && toCancel.size() < MAX_MUTATIONS; index++) if (!matchedOrders[index] && !scheduledRefresh[index]) toCancel.add(managed.get(index));
        for (int index = 0; index < managed.size() && toCancel.size() < MAX_MUTATIONS; index++) if (scheduledRefresh[index]) toCancel.add(managed.get(index));
        result.canceled = toCancel.size();
        if (!toCancel.isEmpty()) { try { cancelOrders(client, symbol, toCancel, guard); } catch (RuntimeException e) { block(key, "cancel order batch: " + e.getMessage()); throw e; } creditImmediateReplacements(key, managed, toCancel, quotes); setPendingCancels(key, toCancel.stream().map(order -> order.orderId).toList()); persist(key); }
        return result;
    }

    private static boolean positive(Duration duration) { return !duration.isZero() && !duration.isNegative(); }

    private static int closestUnmatchedQuote(Domain.Order order, List<Domain.Quote> quotes, boolean[] matched) {
        int selected = -1;
        DecimalValue smallestDistance = null;
        for (int index = 0; index < quotes.size(); index++) {
            Domain.Quote quote = quotes.get(index);
            if (matched[index] || order.side != quote.side) continue;
            DecimalValue distance = order.price.subtract(quote.price).abs();
            if (smallestDistance == null || distance.compareTo(smallestDistance) < 0) {
                selected = index;
                smallestDistance = distance;
            }
        }
        return selected;
    }

    private Result placeMissing(VenueClient client, String key, String instrumentId, List<Domain.Quote> selected,
                                Result result, WriteGuard guard, long generation) {
        List<VenueClient.PlaceRequest> requests = new ArrayList<>();
        for (Domain.Quote quote : selected) requests.add(new VenueClient.PlaceRequest(quote.symbol, quote.side, quote.price, quote.quantity, clientId(instrumentId, quote, generation), generation));
        List<Domain.Order> orders;
        try { checkGuard(guard); orders = client.capabilities().nativeBatchPlace() ? client.placePostOnlyBatch(requests) : requests.stream().map(request -> { checkGuard(guard); return client.placePostOnly(request); }).toList(); }
        catch (RuntimeException e) { block(key, "submit batch may be partially unknown: " + e.getMessage()); throw e; }
        int confirmed = Math.min(orders.size(), selected.size());
        for (int index = 0; index < confirmed; index++) { Domain.Order order = orders.get(index); if (order.orderId == null || order.orderId.isEmpty()) continue; if (active(order.state)) { PendingCreate creation = new PendingCreate(); creation.orderId = order.orderId; creation.quote = selected.get(index); creation.submittedAt = Instant.now(); pendingCreates.computeIfAbsent(key, ignored -> new LinkedHashMap<>()).put(order.orderId, creation); persist(key); result.pending++; } result.placed++; }
        if (orders.size() != requests.size()) { String error = "venue returned " + orders.size() + " order results for " + requests.size() + " requests"; block(key, error); throw new IllegalStateException(error); }
        for (int index = 0; index < orders.size(); index++) if (orders.get(index).orderId == null || orders.get(index).orderId.isEmpty()) { String error = "venue returned an empty order id for batch item " + index; block(key, error); throw new IllegalStateException(error); }
        return result;
    }

    private List<Domain.Quote> selectReplenishments(String key, List<Domain.Quote> quotes, boolean[] matched,
                                                     Map<Domain.Side, Integer> deficits, int vacancyLimit,
                                                     ReplenishPolicy policy, Instant now) {
        Map<Domain.Side, Integer> allowed = new LinkedHashMap<>();
        if (replenishmentBootstrap.contains(key) || policy.minDelay().isZero() && policy.maxDelay().isZero()) {
            allowed.putAll(deficits);
        } else {
            Map<Domain.Side, Integer> credits = immediateReplacementCredits.computeIfAbsent(key, ignored -> new LinkedHashMap<>());
            Map<Domain.Side, PendingVacancy> delayed = pendingVacancies.computeIfAbsent(key, ignored -> new LinkedHashMap<>());
            int delayedBudget = policy.maxPerCycle();
            for (Domain.Side side : Domain.Side.values()) {
                int deficit = deficits.getOrDefault(side, 0);
                int credit = Math.min(deficit, credits.getOrDefault(side, 0));
                if (credit > 0) allowed.put(side, credit);
                int external = Math.max(0, deficit - credit);
                if (external == 0) { delayed.remove(side); continue; }
                PendingVacancy vacancy = delayed.get(side);
                if (vacancy == null) {
                    vacancy = new PendingVacancy();
                    vacancy.detectedAt = now;
                    vacancy.replenishAt = now.plus(randomDelay(policy));
                    delayed.put(side, vacancy);
                }
                vacancy.count = external;
            }
            boolean added;
            do {
                added = false;
                for (Domain.Side side : Domain.Side.values()) {
                    if (delayedBudget == 0) break;
                    PendingVacancy vacancy = delayed.get(side);
                    int alreadyAllowed = Math.max(0, allowed.getOrDefault(side, 0) - credits.getOrDefault(side, 0));
                    if (vacancy != null && !now.isBefore(vacancy.replenishAt) && alreadyAllowed < vacancy.count) {
                        allowed.merge(side, 1, Integer::sum);
                        delayedBudget--;
                        added = true;
                    }
                }
            } while (added && delayedBudget > 0);
        }
        List<Domain.Quote> selected = new ArrayList<>();
        Map<Domain.Side, Integer> used = new LinkedHashMap<>();
        for (int index = 0; index < quotes.size() && selected.size() < vacancyLimit; index++) {
            Domain.Quote quote = quotes.get(index);
            if (matched[index] || used.getOrDefault(quote.side, 0) >= allowed.getOrDefault(quote.side, 0)) continue;
            selected.add(quote);
            used.merge(quote.side, 1, Integer::sum);
        }
        return selected;
    }

    private static Map<Domain.Side, Integer> sideDeficits(List<Domain.Quote> quotes, List<Domain.Order> managed,
                                                           List<PendingCreate> pending) {
        Map<Domain.Side, Integer> targets = new LinkedHashMap<>(), active = new LinkedHashMap<>(), deficits = new LinkedHashMap<>();
        for (Domain.Quote quote : quotes) targets.merge(quote.side, 1, Integer::sum);
        for (Domain.Order order : managed) active.merge(order.side, 1, Integer::sum);
        for (PendingCreate create : pending) active.merge(create.quote.side, 1, Integer::sum);
        for (Domain.Side side : Domain.Side.values()) deficits.put(side, Math.max(0, targets.getOrDefault(side, 0) - active.getOrDefault(side, 0)));
        return deficits;
    }

    private Duration randomDelay(ReplenishPolicy policy) {
        long minimum = policy.minDelay().toMillis(), maximum = policy.maxDelay().toMillis();
        if (minimum == maximum) return Duration.ofMillis(minimum);
        return Duration.ofMillis(ThreadLocalRandom.current().nextLong(minimum, maximum + 1));
    }

    private void consumeReplenishments(String key, List<Domain.Quote> selected) {
        Map<Domain.Side, Integer> credits = immediateReplacementCredits.computeIfAbsent(key, ignored -> new LinkedHashMap<>());
        Map<Domain.Side, PendingVacancy> delayed = pendingVacancies.computeIfAbsent(key, ignored -> new LinkedHashMap<>());
        Map<Domain.Side, Integer> placed = new LinkedHashMap<>();
        for (Domain.Quote quote : selected) placed.merge(quote.side, 1, Integer::sum);
        for (Map.Entry<Domain.Side, Integer> entry : placed.entrySet()) {
            Domain.Side side = entry.getKey(); int remaining = entry.getValue();
            int credit = credits.getOrDefault(side, 0), usedCredit = Math.min(credit, remaining);
            if (credit == usedCredit) credits.remove(side); else credits.put(side, credit - usedCredit);
            remaining -= usedCredit;
            PendingVacancy vacancy = delayed.get(side);
            if (remaining > 0 && vacancy != null) {
                vacancy.count = Math.max(0, vacancy.count - remaining);
                if (vacancy.count == 0) delayed.remove(side);
            }
        }
        if (credits.isEmpty()) immediateReplacementCredits.remove(key);
        if (delayed.isEmpty()) pendingVacancies.remove(key);
    }

    private int delayedVacancies(String key) {
        return pendingVacancies.getOrDefault(key, Map.of()).values().stream().mapToInt(vacancy -> vacancy.count).sum();
    }

    private void creditImmediateReplacements(String key, List<Domain.Order> managed,
                                             List<Domain.Order> canceled, List<Domain.Quote> targets) {
        Map<Domain.Side, Integer> targetSides = new LinkedHashMap<>(), managedSides = new LinkedHashMap<>(), canceledSides = new LinkedHashMap<>();
        for (Domain.Quote quote : targets) targetSides.merge(quote.side, 1, Integer::sum);
        for (Domain.Order order : managed) managedSides.merge(order.side, 1, Integer::sum);
        for (Domain.Order order : canceled) canceledSides.merge(order.side, 1, Integer::sum);
        Map<Domain.Side, Integer> credits = immediateReplacementCredits.computeIfAbsent(key, ignored -> new LinkedHashMap<>());
        for (Domain.Side side : Domain.Side.values()) {
            int canceledCount = canceledSides.getOrDefault(side, 0);
            int remainingAfterCancel = Math.max(0, managedSides.getOrDefault(side, 0) - canceledCount);
            int replacementCount = Math.min(canceledCount, Math.max(0, targetSides.getOrDefault(side, 0) - remainingAfterCancel));
            if (replacementCount > 0) credits.merge(side, replacementCount, Integer::sum);
        }
        if (credits.isEmpty()) immediateReplacementCredits.remove(key);
    }

    private void clearReplenishmentState(String key) {
        pendingVacancies.remove(key);
        immediateReplacementCredits.remove(key);
        if (replenishmentBootstrap.remove(key)) replenishmentInitialized.add(key);
        persist(key);
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
        cancelOrders(client, symbol, managed, guard); setPendingCancels(key, managed.stream().map(order -> order.orderId).toList()); pendingCreates.remove(key); pendingVacancies.remove(key); immediateReplacementCredits.remove(key); persist(key); return managed.size();
    }

    public synchronized void clearBlocked(VenueClient client, String instrumentId) { String key = stateKey(client, instrumentId); ensureLoaded(key); blocked.remove(key); blockedAt.remove(key); persist(key); }
    public static List<Domain.Order> managedOrdersFor(VenueClient client, List<Domain.Order> orders) { if (client.managesAllOrders()) return new ArrayList<>(orders); return orders.stream().filter(Reconciler::isManaged).toList(); }
    public static boolean isManaged(Domain.Order order) { return order.clientId != null && order.clientId.startsWith(MANAGED_PREFIX); }
    public static boolean withinBps(DecimalValue left, DecimalValue right, int threshold) { if (left.isZero() || right.isZero()) return false; return left.subtract(right).abs().multiply(DecimalValue.TEN_THOUSAND).compareTo(right.multiply(DecimalValue.of(threshold))) <= 0; }

    private static void cancelOrders(VenueClient client, String symbol, List<Domain.Order> orders, WriteGuard guard) { if (orders.isEmpty()) return; if (client.capabilities().nativeBatchCancel()) { List<String> ids = orders.stream().map(order -> order.orderId).toList(); for (int start = 0; start < ids.size(); start += MAX_BATCH_CANCEL) { checkGuard(guard); client.cancelOrders(symbol, ids.subList(start, Math.min(start + MAX_BATCH_CANCEL, ids.size()))); } } else for (Domain.Order order : orders) { checkGuard(guard); client.cancelOrder(symbol, order.orderId); } }
    private static void checkGuard(WriteGuard guard) { if (guard != null) try { guard.check(); } catch (RuntimeException e) { throw new IllegalStateException("exchange write rejected by fencing guard: " + e.getMessage(), e); } }
    private static boolean sameTarget(Domain.Quote left, Domain.Quote right) { return left.side == right.side && left.price.equals(right.price) && left.quantity.equals(right.quantity); }
    private static boolean active(Domain.OrderState state) { return state == Domain.OrderState.NEW || state == Domain.OrderState.PARTIALLY_FILLED || state == Domain.OrderState.UNKNOWN; }
    private static String stateKey(VenueClient client, String instrument) { return (client.stateIdentity() == null || client.stateIdentity().isEmpty() ? client.name() : client.stateIdentity()) + ":" + instrument; }

    private void ensureLoaded(String key) { if (!loaded.add(key) || store == null) return; byte[] value = store.loadOmsState(key); if (value == null || value.length == 0) return; try { PersistedState saved = Json.read(value, PersistedState.class); if (saved.blocked != null && !saved.blocked.isEmpty()) { blocked.put(key, saved.blocked); blockedAt.put(key, saved.blockedAtEpochMs != null ? Instant.ofEpochMilli(saved.blockedAtEpochMs) : Instant.EPOCH); } if (saved.pendingCancels != null) pendingCancels.put(key, saved.pendingCancels); if (saved.pendingCreates != null) pendingCreates.put(key, saved.pendingCreates); if (saved.pendingVacancies != null) pendingVacancies.put(key, saved.pendingVacancies); if (saved.immediateReplacementCredits != null) immediateReplacementCredits.put(key, saved.immediateReplacementCredits); if (saved.replenishmentInitialized) replenishmentInitialized.add(key); if (saved.replenishmentBootstrap) replenishmentBootstrap.add(key); } catch (RuntimeException e) { loaded.remove(key); throw e; } }
    private void persist(String key) { if (store == null) return; PersistedState state = new PersistedState(); state.blocked = blocked.get(key); state.blockedAtEpochMs = blockedAt.containsKey(key) ? blockedAt.get(key).toEpochMilli() : null; state.pendingCancels = pendingCancels.get(key); state.pendingCreates = pendingCreates.get(key); state.pendingVacancies = pendingVacancies.get(key); state.immediateReplacementCredits = immediateReplacementCredits.get(key); state.replenishmentInitialized = replenishmentInitialized.contains(key); state.replenishmentBootstrap = replenishmentBootstrap.contains(key); if ((state.blocked == null || state.blocked.isEmpty()) && state.pendingCancels == null && (state.pendingCreates == null || state.pendingCreates.isEmpty()) && (state.pendingVacancies == null || state.pendingVacancies.isEmpty()) && (state.immediateReplacementCredits == null || state.immediateReplacementCredits.isEmpty()) && !state.replenishmentInitialized && !state.replenishmentBootstrap) store.deleteOmsState(key); else store.saveOmsState(key, Json.writeBytes(state)); }
    private void block(String key, String error) { blocked.put(key, error); blockedAt.put(key, clock.instant()); persist(key); }

    public static String clientId(String instrumentId, Domain.Quote quote, long generation) {
        String seed = instrumentId + "|" + quote.venue + "|" + quote.side + "|" + quote.level + "|" + generation + "|" + System.nanoTime();
        try { byte[] digest = MessageDigest.getInstance("SHA-256").digest(seed.getBytes(StandardCharsets.UTF_8)); return MANAGED_PREFIX + "g" + Long.toString(generation, 36) + "-" + HexFormat.of().formatHex(digest, 0, 8); }
        catch (NoSuchAlgorithmException e) { throw new IllegalStateException(e); }
    }
}
