package com.fluxmaker.tradesim;

import com.fluxmaker.config.AppConfig;
import com.fluxmaker.domain.Domain;
import com.fluxmaker.math.DecimalValue;

import java.math.BigInteger;
import java.security.SecureRandom;

/** Default internal-only planner matching the original random simulation logic. */
public final class InsideSpreadRandomPlanner implements VolumeSimulationPlanner {
    private static final SecureRandom RANDOM = new SecureRandom();

    @Override
    public EventPlan plan(Request request) {
        AppConfig.VenueMarketConfig market = request.market();
        Domain.Book book = request.book();
        DecimalValue first = book.bidPrice.quantizeDown(market.priceTick).add(market.priceTick);
        DecimalValue last = book.askPrice.quantizeUp(market.priceTick).subtract(market.priceTick);
        if (first.compareTo(book.bidPrice) <= 0
                || last.compareTo(book.askPrice) >= 0
                || first.compareTo(last) > 0) {
            throw new IllegalArgumentException("no price tick exists strictly inside bid/ask");
        }
        DecimalValue price = randomStep(first, last, market.priceTick);

        AppConfig.TradeSimulationConfig config = request.instrument().tradeSimulation;
        DecimalValue minimum = config.minQuantity.quantizeUp(market.quantityStep);
        if (market.minQuantity.isPositive()) {
            minimum = minimum.max(market.minQuantity.quantizeUp(market.quantityStep));
        }
        if (market.minNotional.isPositive()) {
            minimum = minimum.max(market.minNotional.divide(price).quantizeUp(market.quantityStep));
        }
        DecimalValue maximum = config.maxQuantity.quantizeDown(market.quantityStep);
        if (market.maxQuantity.isPositive()) {
            maximum = maximum.min(market.maxQuantity.quantizeDown(market.quantityStep));
        }
        if (minimum.compareTo(maximum) > 0) {
            throw new IllegalArgumentException("configured quantity range cannot satisfy exchange minimums");
        }
        DecimalValue quantity = randomStep(minimum, maximum, market.quantityStep);
        Domain.Side side = RANDOM.nextInt(10_000) < config.buyProbabilityBps
                ? Domain.Side.BUY
                : Domain.Side.SELL;
        return new EventPlan(side, price, quantity);
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
