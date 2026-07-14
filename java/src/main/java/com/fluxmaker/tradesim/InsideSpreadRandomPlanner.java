package com.fluxmaker.tradesim;

import com.fluxmaker.config.AppConfig;
import com.fluxmaker.domain.Domain;
import com.fluxmaker.math.DecimalValue;

import java.math.BigInteger;
import java.security.SecureRandom;
import java.util.ArrayList;
import java.util.List;

/** Default internal-only planner matching the original random simulation logic. */
public final class InsideSpreadRandomPlanner implements VolumeSimulationPlanner {
    private static final SecureRandom RANDOM = new SecureRandom();

    @Override
    public List<EventPlan> plan(Request request) {
        AppConfig.VenueMarketConfig market = request.market();
        Domain.Book book = request.book();
        DecimalValue first = book.bidPrice.quantizeDown(market.priceTick).add(market.priceTick);
        DecimalValue last = book.askPrice.quantizeUp(market.priceTick).subtract(market.priceTick);
        if (first.compareTo(book.bidPrice) <= 0
                || last.compareTo(book.askPrice) >= 0
                || first.compareTo(last) > 0) {
            throw new IllegalArgumentException("no price tick exists strictly inside bid/ask");
        }

        AppConfig.TradeSimulationConfig config = request.instrument().tradeSimulation;
        int batchSize = Math.max(1, config.batchSize);

        DecimalValue minimum = config.minQuantity.quantizeUp(market.quantityStep);
        if (market.minQuantity.isPositive()) {
            minimum = minimum.max(market.minQuantity.quantizeUp(market.quantityStep));
        }
        DecimalValue maximum = config.maxQuantity.quantizeDown(market.quantityStep);
        if (market.maxQuantity.isPositive()) {
            maximum = maximum.min(market.maxQuantity.quantizeDown(market.quantityStep));
        }
        if (minimum.compareTo(maximum) > 0) {
            throw new IllegalArgumentException("configured quantity range cannot satisfy exchange minimums");
        }

        List<EventPlan> plans = new ArrayList<>(batchSize);
        for (int i = 0; i < batchSize; i++) {
            DecimalValue price = randomStep(first, last, market.priceTick);

            DecimalValue effectiveMin = minimum;
            if (market.minNotional.isPositive()) {
                effectiveMin = effectiveMin.max(market.minNotional.divide(price).quantizeUp(market.quantityStep));
            }
            if (effectiveMin.compareTo(maximum) > 0) {
                throw new IllegalArgumentException("configured quantity range cannot satisfy exchange minimums (with min notional)");
            }

            DecimalValue quantity = randomStep(effectiveMin, maximum, market.quantityStep);
            Domain.Side side = RANDOM.nextInt(10_000) < config.buyProbabilityBps
                    ? Domain.Side.BUY
                    : Domain.Side.SELL;
            plans.add(new EventPlan(side, price, quantity));
        }
        return plans;
    }

    private static DecimalValue randomStep(DecimalValue minimum, DecimalValue maximum, DecimalValue step) {
        DecimalValue delta = maximum.subtract(minimum);
        if (delta.signum() < 0) throw new IllegalArgumentException("invalid random range");
        BigInteger choices = delta.floorQuotient(step).add(BigInteger.ONE);
        BigInteger selection;
        do {
            selection = new BigInteger(choices.bitLength(), RANDOM);
        } while (selection.compareTo(choices) >= 0);
        return minimum.add(step.multiply(DecimalValue.fraction(selection, BigInteger.ONE)));
    }
}
