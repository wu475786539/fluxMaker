package com.fluxmaker.tradesim;

import com.fluxmaker.config.AppConfig;
import com.fluxmaker.domain.Domain;
import com.fluxmaker.math.DecimalValue;

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
 * <p>This boundary has no venue order client. A planner can only return an
 * {@link VolumeSimulationPlanner.EventPlan}; this class always creates a
 * {@link Domain.Fill} with {@code simulated=true}.
 */
public final class TradeSimulator {
    private static final SecureRandom RANDOM = new SecureRandom();
    private final Map<String, State> states = new HashMap<>();
    private final VolumeSimulationPlanner planner;

    private static final class State {
        Instant nextAt;
        Instant lastGeneratedAt;
        List<Domain.Fill> fills = new ArrayList<>();
        DecimalValue lastPrice;
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

    public record Observation(Snapshot snapshot, Domain.Fill fill) {}

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
            Instant now
    ) {
        AppConfig.TradeSimulationConfig config = instrument.tradeSimulation;
        Snapshot snapshot = new Snapshot();
        snapshot.enabled = config.enabled;
        snapshot.sourceVenue = config.sourceVenue;
        snapshot.planner = planner.getClass().getSimpleName();
        snapshot.status = "disabled";
        if (!config.enabled || !venueName.equals(config.sourceVenue)) {
            return new Observation(snapshot, null);
        }

        State state = states.computeIfAbsent(instrument.id, ignored -> new State());
        if (state.nextAt == null) {
            state.nextAt = now.plusMillis(randomDuration(config.minIntervalMs, config.maxIntervalMs));
        }
        snapshot.status = "waiting";
        Domain.Fill generated = null;
        if (!now.isBefore(state.nextAt)) {
            state.nextAt = now.plusMillis(randomDuration(config.minIntervalMs, config.maxIntervalMs));
            long sequence = state.sequence + 1;
            try {
                VolumeSimulationPlanner.Request request = new VolumeSimulationPlanner.Request(
                        instrument, venueName, market, book, state.lastPrice, now, sequence);
                generated = materialize(instrument, venueName, market, book, now, sequence, planner.plan(request));
                state.sequence = sequence;
                state.lastGeneratedAt = now;
                state.lastPrice = generated.price;
                state.fills.addFirst(generated);
                if (state.fills.size() > config.recentLimit) {
                    state.fills = new ArrayList<>(state.fills.subList(0, config.recentLimit));
                }
                snapshot.status = "running";
            } catch (RuntimeException e) {
                snapshot.status = "skipped";
                snapshot.error = e.getMessage() == null ? e.getClass().getSimpleName() : e.getMessage();
                System.out.printf(
                        "[volume-simulation] skipped instrument=%s source_venue=%s symbol=%s sequence=%d "
                                + "bid=%s ask=%s error=%s%n",
                        instrument.id,
                        venueName,
                        market.symbol,
                        sequence,
                        book.bidPrice,
                        book.askPrice,
                        snapshot.error
                );
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

        Domain.Fill fill = new Domain.Fill();
        fill.venue = venue;
        fill.tradeId = "SIM-" + instrument.id + "-" + now.toEpochMilli() + "-" + sequence;
        fill.symbol = market.symbol;
        fill.side = plan.side();
        fill.price = price;
        fill.quantity = quantity;
        fill.quoteQuantity = quoteQuantity;
        fill.simulated = true;
        fill.timestamp = now;
        return fill;
    }

    private static long randomDuration(int minimum, int maximum) {
        return maximum <= minimum ? minimum : minimum + RANDOM.nextLong(maximum - (long) minimum + 1);
    }
}
