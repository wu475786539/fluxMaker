package com.fluxmaker.tradesim;

import com.fluxmaker.config.AppConfig;
import com.fluxmaker.domain.Domain;
import com.fluxmaker.math.DecimalValue;
import com.fluxmaker.venue.VenueClient;

import java.security.SecureRandom;
import java.time.Instant;
import java.util.ArrayList;
import java.util.HashMap;
import java.util.List;
import java.util.Map;
import java.util.Objects;

/**
 * Schedules and materializes internal volume-simulation events.
 *
 * <p>This boundary has no venue order client. A planner can return
 * {@link VolumeSimulationPlanner.EventPlan} lists with optional real
 * {@code orderId} fields. When {@code orderId} is present the fill is
 * materialized as a real (non-simulated) fill; otherwise it is marked
 * {@code simulated=true}.
 */
public final class TradeSimulator {
    private static final SecureRandom RANDOM = new SecureRandom();
    private final Map<String, State> states = new HashMap<>();
    private final VolumeSimulationPlanner planner;

    private static final class State {
        Instant nextAt;
        Instant lastGeneratedAt;
        List<Domain.Fill> fills = new ArrayList<>();
        long sequence;
    }

    public static final class Snapshot {
        public boolean enabled;
        public String sourceVenue;
        public String planner;
        public String status;
        public Instant lastGeneratedAt;
        public Instant nextAt;
        public List<Domain.Fill> fills = new ArrayList<>();
        public String error;
    }

    public record Observation(Snapshot snapshot, List<Domain.Fill> fills) {}

    public TradeSimulator() {
        this(new InsideSpreadRandomPlanner());
    }

    public TradeSimulator(VolumeSimulationPlanner planner) {
        this.planner = Objects.requireNonNull(planner, "planner");
    }

    public synchronized Observation observe(
            AppConfig.InstrumentConfig instrument,
            String venueName,
            AppConfig.VenueMarketConfig market,
            Domain.Book book,
            Instant now,
            VenueClient venueClient
    ) {
        AppConfig.TradeSimulationConfig config = instrument.tradeSimulation;
        Snapshot snapshot = new Snapshot();
        snapshot.enabled = config.enabled;
        snapshot.sourceVenue = config.sourceVenue;
        snapshot.planner = planner.getClass().getSimpleName();
        snapshot.status = "disabled";
        if (!config.enabled || !venueName.equals(config.sourceVenue)) {
            return new Observation(snapshot, List.of());
        }

        State state = states.computeIfAbsent(instrument.id, ignored -> new State());
        if (state.nextAt == null) {
            state.nextAt = now.plusMillis(randomDuration(config.minIntervalMs, config.maxIntervalMs));
        }
        snapshot.status = "waiting";
        List<Domain.Fill> generated = List.of();
        if (!now.isBefore(state.nextAt)) {
            state.nextAt = now.plusMillis(randomDuration(config.minIntervalMs, config.maxIntervalMs));
            try {
                long baseSequence = state.sequence;
                VolumeSimulationPlanner.Request request = new VolumeSimulationPlanner.Request(
                        instrument, venueName, market, book, now, baseSequence + 1, venueClient);
                List<VolumeSimulationPlanner.EventPlan> plans = planner.plan(request);
                List<Domain.Fill> fills = new ArrayList<>(plans.size());
                for (int i = 0; i < plans.size(); i++) {
                    long seq = baseSequence + 1 + i;
                    fills.add(materialize(instrument, venueName, market, book, now, seq, plans.get(i)));
                }
                state.sequence = baseSequence + plans.size();
                state.lastGeneratedAt = now;
                for (int i = fills.size() - 1; i >= 0; i--) {
                    state.fills.addFirst(fills.get(i));
                }
                if (state.fills.size() > config.recentLimit) {
                    state.fills = new ArrayList<>(state.fills.subList(0, config.recentLimit));
                }
                generated = fills;
                snapshot.status = "running";
            } catch (RuntimeException e) {
                snapshot.status = "skipped";
                snapshot.error = e.getMessage();
            }
        }
        snapshot.lastGeneratedAt = state.lastGeneratedAt;
        snapshot.nextAt = state.nextAt;
        snapshot.fills = new ArrayList<>(state.fills);
        return new Observation(snapshot, generated);
    }

    private static Domain.Fill materialize(
            AppConfig.InstrumentConfig instrument,
            String venue,
            AppConfig.VenueMarketConfig market,
            Domain.Book book,
            Instant now,
            long sequence,
            VolumeSimulationPlanner.EventPlan plan
    ) {
        if (plan == null || plan.side() == null || plan.price() == null || plan.quantity() == null) {
            throw new IllegalArgumentException("planner returned an incomplete internal event");
        }
        DecimalValue price = plan.price();
        DecimalValue quantity = plan.quantity();
        if (price.compareTo(book.bidPrice) <= 0 || price.compareTo(book.askPrice) >= 0) {
            throw new IllegalArgumentException("planned price must be strictly inside bid/ask");
        }
        if (!price.equals(price.quantizeDown(market.priceTick))) {
            throw new IllegalArgumentException("planned price is not aligned to price tick");
        }
        if (!quantity.isPositive() || !quantity.equals(quantity.quantizeDown(market.quantityStep))) {
            throw new IllegalArgumentException("planned quantity must be positive and aligned to quantity step");
        }
        AppConfig.TradeSimulationConfig config = instrument.tradeSimulation;
        if (config.minQuantity.isPositive() && quantity.compareTo(config.minQuantity) < 0) {
            throw new IllegalArgumentException("planned quantity is below the configured minimum");
        }
        if (config.maxQuantity.isPositive() && quantity.compareTo(config.maxQuantity) > 0) {
            throw new IllegalArgumentException("planned quantity is above the configured maximum");
        }
        if (market.minQuantity.isPositive() && quantity.compareTo(market.minQuantity) < 0) {
            throw new IllegalArgumentException("planned quantity is below the market minimum");
        }
        if (market.maxQuantity.isPositive() && quantity.compareTo(market.maxQuantity) > 0) {
            throw new IllegalArgumentException("planned quantity is above the market maximum");
        }
        DecimalValue quoteQuantity = price.multiply(quantity);
        if (market.minNotional.isPositive() && quoteQuantity.compareTo(market.minNotional) < 0) {
            throw new IllegalArgumentException("planned notional is below the market minimum");
        }

        boolean isReal = plan.isReal();
        Domain.Fill fill = new Domain.Fill();
        fill.venue = venue;
        fill.tradeId = isReal
                ? "REAL-" + instrument.id + "-" + now.toEpochMilli() + "-" + sequence
                : "SIM-" + instrument.id + "-" + now.toEpochMilli() + "-" + sequence;
        fill.orderId = isReal ? plan.orderId() : "";
        fill.symbol = market.symbol;
        fill.side = plan.side();
        fill.price = price;
        fill.quantity = quantity;
        fill.quoteQuantity = quoteQuantity;
        fill.simulated = !isReal;
        fill.timestamp = now;
        return fill;
    }

    private static long randomDuration(int minimum, int maximum) {
        return maximum <= minimum ? minimum : minimum + RANDOM.nextLong(maximum - (long) minimum + 1);
    }
}
