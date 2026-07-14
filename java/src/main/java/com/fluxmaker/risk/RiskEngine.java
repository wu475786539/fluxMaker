package com.fluxmaker.risk;

import com.fluxmaker.config.AppConfig;
import com.fluxmaker.domain.Domain;
import com.fluxmaker.math.DecimalValue;

import java.time.Duration;
import java.time.Instant;
import java.util.ArrayList;
import java.util.List;

public final class RiskEngine {
    public List<Domain.Quote> filterQuotes(AppConfig.InstrumentConfig instrument, AppConfig.VenueMarketConfig market,
                                           Domain.Book book, DecimalValue inventory, List<Domain.Quote> quotes) {
        if (book.hasPrices() && (book.timestamp == null || Duration.between(book.timestamp, Instant.now()).compareTo(Duration.ofSeconds(30)) > 0)) throw new IllegalArgumentException("venue book is stale");
        DecimalValue deviation = inventory.subtract(instrument.strategy.targetBase);
        List<Domain.Quote> result = new ArrayList<>();
        for (Domain.Quote quote : quotes) {
            if (quote.validUntil == null || Instant.now().isAfter(quote.validUntil)) throw new IllegalArgumentException("quote reference expired");
            if (book.hasAsk() && quote.side == Domain.Side.BUY && quote.price.compareTo(book.askPrice) >= 0) throw new IllegalArgumentException("buy quote would take liquidity");
            if (book.hasBid() && quote.side == Domain.Side.SELL && quote.price.compareTo(book.bidPrice) <= 0) throw new IllegalArgumentException("sell quote would take liquidity");
            DecimalValue notional = quote.price.multiply(quote.quantity);
            if (notional.compareTo(market.minNotional) < 0) throw new IllegalArgumentException("quote below minimum notional");
            if (market.minQuantity.isPositive() && quote.quantity.compareTo(market.minQuantity) < 0) throw new IllegalArgumentException("quote below exchange minimum quantity");
            if (market.maxQuantity.isPositive() && quote.quantity.compareTo(market.maxQuantity) > 0) throw new IllegalArgumentException("quote above exchange maximum quantity");
            if (market.maxNotional.isPositive() && notional.compareTo(market.maxNotional) > 0) throw new IllegalArgumentException("quote above exchange maximum notional");
            if (market.minPrice.isPositive() && quote.price.compareTo(market.minPrice) < 0) throw new IllegalArgumentException("quote below exchange minimum price");
            if (market.maxPrice.isPositive() && quote.price.compareTo(market.maxPrice) > 0) throw new IllegalArgumentException("quote above exchange maximum price");
            if (instrument.strategy.maxBaseDeviation.isPositive()) {
                if (deviation.compareTo(instrument.strategy.maxBaseDeviation) >= 0 && quote.side == Domain.Side.BUY) continue;
                if (deviation.compareTo(instrument.strategy.maxBaseDeviation.negate()) <= 0 && quote.side == Domain.Side.SELL) continue;
            }
            result.add(quote);
        }
        for (Domain.Quote left : result) for (Domain.Quote right : result) {
            if (left.side == Domain.Side.BUY && right.side == Domain.Side.SELL && left.price.compareTo(right.price) >= 0) throw new IllegalArgumentException("target quotes self-cross");
        }
        if (result.isEmpty()) throw new IllegalArgumentException("all quote sides blocked by inventory limits");
        return result;
    }

    public static void validateMarketReference(Domain.ReferencePrice reference, Domain.Book book, AppConfig.StrategyConfig strategy) {
        if (!reference.price.isPositive()) throw new IllegalArgumentException("invalid reference price");
        if (!book.twoSided()) return;
        if (book.bidPrice.compareTo(book.askPrice) >= 0) throw new IllegalArgumentException("crossed venue book");
        DecimalValue mid = book.bidPrice.add(book.askPrice).divide(DecimalValue.of(2));
        if (strategy.maxVenueReferenceDeviationBps > 0) {
            DecimalValue deviation = mid.subtract(reference.price).abs().divide(reference.price).multiply(DecimalValue.TEN_THOUSAND);
            if (deviation.compareTo(DecimalValue.of(strategy.maxVenueReferenceDeviationBps)) > 0) throw new IllegalArgumentException("venue/reference deviation " + deviation + " bps exceeds " + strategy.maxVenueReferenceDeviationBps);
        }
        if (strategy.maxVenueSpreadBps > 0) {
            DecimalValue spread = book.askPrice.subtract(book.bidPrice).divide(mid).multiply(DecimalValue.TEN_THOUSAND);
            if (spread.compareTo(DecimalValue.of(strategy.maxVenueSpreadBps)) > 0) throw new IllegalArgumentException("venue spread " + spread + " bps exceeds " + strategy.maxVenueSpreadBps);
        }
    }

    public static BudgetResult applyBalanceBudget(List<Domain.Quote> quotes, List<Domain.Order> managed,
                                                   DecimalValue baseFree, DecimalValue quoteFree, int reserveBps,
                                                   DecimalValue maxBase, DecimalValue maxQuote) {
        reserveBps = Math.max(0, Math.min(10_000, reserveBps));
        DecimalValue baseCommitted = DecimalValue.ZERO, quoteCommitted = DecimalValue.ZERO;
        for (Domain.Order order : managed) {
            DecimalValue remaining = order.quantity.subtract(order.executedQty);
            if (remaining.signum() <= 0) continue;
            if (order.side == Domain.Side.SELL) baseCommitted = baseCommitted.add(remaining);
            else if (order.side == Domain.Side.BUY) quoteCommitted = quoteCommitted.add(order.price.multiply(remaining));
        }
        baseFree = baseFree.signum() < 0 ? DecimalValue.ZERO : baseFree;
        quoteFree = quoteFree.signum() < 0 ? DecimalValue.ZERO : quoteFree;
        DecimalValue factor = DecimalValue.of(10_000 - reserveBps).divide(DecimalValue.TEN_THOUSAND);
        Domain.QuoteBudget budget = new Domain.QuoteBudget(); budget.reserveBps = reserveBps; budget.targetOrders = quotes.size();
        budget.baseBudget = baseFree.add(baseCommitted).multiply(factor); budget.quoteBudget = quoteFree.add(quoteCommitted).multiply(factor);
        if (maxBase.isPositive()) budget.baseBudget = budget.baseBudget.min(maxBase);
        if (maxQuote.isPositive()) budget.quoteBudget = budget.quoteBudget.min(maxQuote);
        for (Domain.Quote quote : quotes) {
            if (quote.side == Domain.Side.SELL) budget.baseRequired = budget.baseRequired.add(quote.quantity);
            else if (quote.side == Domain.Side.BUY) budget.quoteRequired = budget.quoteRequired.add(quote.price.multiply(quote.quantity));
        }
        DecimalValue usedBase = DecimalValue.ZERO, usedQuote = DecimalValue.ZERO;
        List<Domain.Quote> eligible = new ArrayList<>();
        for (Domain.Quote quote : quotes) {
            if (quote.side == Domain.Side.SELL) {
                DecimalValue next = usedBase.add(quote.quantity); if (next.compareTo(budget.baseBudget) > 0) { budget.baseLimited = true; continue; } usedBase = next;
            } else if (quote.side == Domain.Side.BUY) {
                DecimalValue next = usedQuote.add(quote.price.multiply(quote.quantity)); if (next.compareTo(budget.quoteBudget) > 0) { budget.quoteLimited = true; continue; } usedQuote = next;
            } else continue;
            eligible.add(quote);
        }
        budget.eligibleOrders = eligible.size(); return new BudgetResult(eligible, budget);
    }

    public static List<Domain.Quote> applyOrderLimit(List<Domain.Quote> quotes, int current, int managed, int maximum) {
        if (maximum <= 0) return quotes;
        int available = Math.max(0, maximum - Math.max(0, current - managed));
        if (available >= quotes.size()) return quotes;
        if ((available & 1) != 0) available--;
        return new ArrayList<>(quotes.subList(0, available));
    }

    public record BudgetResult(List<Domain.Quote> quotes, Domain.QuoteBudget budget) {}
}
