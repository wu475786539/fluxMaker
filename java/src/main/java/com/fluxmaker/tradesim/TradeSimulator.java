package com.fluxmaker.tradesim;

import com.fluxmaker.config.AppConfig;
import com.fluxmaker.domain.Domain;
import com.fluxmaker.math.DecimalValue;

import java.math.BigInteger;
import java.security.SecureRandom;
import java.time.Instant;
import java.util.ArrayList;
import java.util.HashMap;
import java.util.List;
import java.util.Map;

public final class TradeSimulator {
    private static final SecureRandom RANDOM = new SecureRandom();
    private final Map<String, State> states = new HashMap<>();
    private static final class State { Instant nextAt, lastGeneratedAt; List<Domain.Fill> fills = new ArrayList<>(); long sequence; }
    public static final class Snapshot { public boolean enabled; public String sourceVenue; public String status; public Instant lastGeneratedAt; public Instant nextAt; public List<Domain.Fill> fills = new ArrayList<>(); public String error; }
    public record Observation(Snapshot snapshot, Domain.Fill fill) {}

    public synchronized Observation observe(AppConfig.InstrumentConfig instrument, String venueName, AppConfig.VenueMarketConfig market, Domain.Book book, Instant now) {
        AppConfig.TradeSimulationConfig config = instrument.tradeSimulation; Snapshot snapshot = new Snapshot(); snapshot.enabled = config.enabled; snapshot.sourceVenue = config.sourceVenue; snapshot.status = "disabled";
        if (!config.enabled || !venueName.equals(config.sourceVenue)) return new Observation(snapshot, null); State state = states.computeIfAbsent(instrument.id, ignored -> new State()); if (state.nextAt == null) state.nextAt = now.plusMillis(randomDuration(config.minIntervalMs, config.maxIntervalMs)); snapshot.status = "waiting"; Domain.Fill generated = null;
        if (!now.isBefore(state.nextAt)) { state.nextAt = now.plusMillis(randomDuration(config.minIntervalMs, config.maxIntervalMs)); try { generated = generate(instrument, venueName, market, book, now, state.sequence + 1); state.sequence++; state.lastGeneratedAt = now; state.fills.addFirst(generated); if (state.fills.size() > config.recentLimit) state.fills = new ArrayList<>(state.fills.subList(0, config.recentLimit)); snapshot.status = "running"; } catch (RuntimeException e) { snapshot.status = "skipped"; snapshot.error = e.getMessage(); } }
        snapshot.lastGeneratedAt = state.lastGeneratedAt; snapshot.nextAt = state.nextAt; snapshot.fills = new ArrayList<>(state.fills); return new Observation(snapshot, generated);
    }

    private static Domain.Fill generate(AppConfig.InstrumentConfig instrument, String venue, AppConfig.VenueMarketConfig market, Domain.Book book, Instant now, long sequence) {
        DecimalValue first = book.bidPrice.quantizeDown(market.priceTick).add(market.priceTick), last = book.askPrice.quantizeUp(market.priceTick).subtract(market.priceTick); if (first.compareTo(book.bidPrice) <= 0 || last.compareTo(book.askPrice) >= 0 || first.compareTo(last) > 0) throw new IllegalArgumentException("no price tick exists strictly inside bid/ask"); DecimalValue price = randomStep(first, last, market.priceTick);
        AppConfig.TradeSimulationConfig config = instrument.tradeSimulation; DecimalValue minimum = config.minQuantity.quantizeUp(market.quantityStep); if (market.minQuantity.isPositive()) minimum = minimum.max(market.minQuantity.quantizeUp(market.quantityStep)); if (market.minNotional.isPositive()) minimum = minimum.max(market.minNotional.divide(price).quantizeUp(market.quantityStep)); DecimalValue maximum = config.maxQuantity.quantizeDown(market.quantityStep); if (market.maxQuantity.isPositive()) maximum = maximum.min(market.maxQuantity.quantizeDown(market.quantityStep)); if (minimum.compareTo(maximum) > 0) throw new IllegalArgumentException("configured quantity range cannot satisfy exchange minimums"); DecimalValue quantity = randomStep(minimum, maximum, market.quantityStep);
        Domain.Fill fill = new Domain.Fill(); fill.venue = venue; fill.tradeId = "SIM-" + instrument.id + "-" + now.toEpochMilli() + "-" + sequence; fill.symbol = market.symbol; fill.side = RANDOM.nextInt(10_000) < config.buyProbabilityBps ? Domain.Side.BUY : Domain.Side.SELL; fill.price = price; fill.quantity = quantity; fill.quoteQuantity = price.multiply(quantity); fill.simulated = true; fill.timestamp = now; return fill;
    }

    private static DecimalValue randomStep(DecimalValue minimum, DecimalValue maximum, DecimalValue step) { DecimalValue delta = maximum.subtract(minimum); if (delta.signum() < 0) throw new IllegalArgumentException("invalid random range"); BigInteger choices = delta.floorQuotient(step).add(BigInteger.ONE); BigInteger selection; do { selection = new BigInteger(choices.bitLength(), RANDOM); } while (selection.compareTo(choices) >= 0); return minimum.add(step.multiply(DecimalValue.fraction(selection, BigInteger.ONE))); }
    private static long randomDuration(int minimum, int maximum) { return maximum <= minimum ? minimum : minimum + RANDOM.nextLong(maximum - (long) minimum + 1); }
}
