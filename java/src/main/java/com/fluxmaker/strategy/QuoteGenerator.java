package com.fluxmaker.strategy;

import com.fluxmaker.config.AppConfig;
import com.fluxmaker.domain.Domain;
import com.fluxmaker.math.DecimalValue;

import java.nio.charset.StandardCharsets;
import java.time.Instant;
import java.util.ArrayList;
import java.util.List;

public final class QuoteGenerator {
    public List<Domain.Quote> generate(AppConfig.InstrumentConfig instrument, String venueName,
                                       AppConfig.VenueMarketConfig market, Domain.ReferencePrice reference,
                                       Domain.Book book, DecimalValue inventory) {
        return generateAt(instrument, venueName, market, reference, book, inventory, Instant.now());
    }

    public List<Domain.Quote> generateAt(AppConfig.InstrumentConfig instrument, String venueName,
                                         AppConfig.VenueMarketConfig market, Domain.ReferencePrice reference,
                                         Domain.Book book, DecimalValue inventory, Instant now) {
        if (!reference.price.isPositive()) throw new IllegalArgumentException("reference price is not positive");
        if (book.twoSided() && book.bidPrice.compareTo(book.askPrice) >= 0) throw new IllegalArgumentException("crossed venue book");
        DecimalValue mid = applyInventorySkew(reference.price, instrument.strategy, inventory);
        List<Domain.Quote> quotes = new ArrayList<>(instrument.strategy.levels * 2);
        DecimalValue previousBid = DecimalValue.ZERO, previousAsk = DecimalValue.ZERO;
        for (int level = 0; level < instrument.strategy.levels; level++) {
            int spreadBps = instrument.strategy.halfSpreadBps + level * instrument.strategy.levelSpacingBps;
            DecimalValue spread = DecimalValue.of(spreadBps).divide(DecimalValue.TEN_THOUSAND);
            DecimalValue bid = mid.multiply(DecimalValue.ONE.subtract(spread)).quantizeDown(market.priceTick);
            DecimalValue ask = mid.multiply(DecimalValue.ONE.add(spread)).quantizeUp(market.priceTick);
            long bidGeneration = refreshGeneration(instrument.strategy, Domain.Side.BUY, level, now);
            long askGeneration = refreshGeneration(instrument.strategy, Domain.Side.SELL, level, now);
            int bidJitterLimit = boundedJitterTicks(bid, market.priceTick, instrument.strategy.effectivePriceJitterTicks(), instrument.strategy.repriceThresholdBps);
            int askJitterLimit = boundedJitterTicks(ask, market.priceTick, instrument.strategy.effectivePriceJitterTicks(), instrument.strategy.repriceThresholdBps);
            bid = applyPriceJitter(bid, market.priceTick, stableJitterTicks(instrument.id, venueName, market.symbol, Domain.Side.BUY, level, bidGeneration, bidJitterLimit));
            ask = applyPriceJitter(ask, market.priceTick, stableJitterTicks(instrument.id, venueName, market.symbol, Domain.Side.SELL, level, askGeneration, askJitterLimit));
            if (book.hasAsk()) bid = bid.min(book.askPrice.subtract(market.priceTick)).quantizeDown(market.priceTick);
            if (book.hasBid()) ask = ask.max(book.bidPrice.add(market.priceTick)).quantizeUp(market.priceTick);
            if (level > 0) {
                if (bid.compareTo(previousBid) >= 0) bid = previousBid.subtract(market.priceTick).quantizeDown(market.priceTick);
                if (ask.compareTo(previousAsk) <= 0) ask = previousAsk.add(market.priceTick).quantizeUp(market.priceTick);
            }
            if (!bid.isPositive() || !ask.isPositive()) throw new IllegalArgumentException("quote rounded to zero");
            if (bid.compareTo(ask) >= 0) throw new IllegalArgumentException("generated quotes cross");
            DecimalValue bidQuantity = quantity(instrument, venueName, market, Domain.Side.BUY, level, bid, bidGeneration);
            DecimalValue askQuantity = quantity(instrument, venueName, market, Domain.Side.SELL, level, ask, askGeneration);
            Instant validUntil = reference.validUntil == null ? Instant.now().plusSeconds(10) : reference.validUntil;
            quotes.add(quote(instrument.id, venueName, market.symbol, Domain.Side.BUY, level, bid, bidQuantity, reference.price, validUntil));
            quotes.add(quote(instrument.id, venueName, market.symbol, Domain.Side.SELL, level, ask, askQuantity, reference.price, validUntil));
            previousBid = bid; previousAsk = ask;
        }
        return quotes;
    }

    private static DecimalValue quantity(AppConfig.InstrumentConfig instrument, String venueName,
                                         AppConfig.VenueMarketConfig market, Domain.Side side, int level,
                                         DecimalValue price, long refreshGeneration) {
        AppConfig.StrategyConfig strategy = instrument.strategy;
        if (!strategy.usesOrderNotionalRange()) {
            DecimalValue quantity = strategy.orderSize.quantizeDown(market.quantityStep);
            if (!quantity.isPositive()) throw new IllegalArgumentException("legacy order size rounded to zero");
            return quantity;
        }
        DecimalValue minimum = strategy.minOrderNotional.max(market.minNotional);
        DecimalValue maximum = strategy.maxOrderNotional;
        if (market.maxNotional.isPositive()) maximum = maximum.min(market.maxNotional);
        if (maximum.compareTo(minimum) < 0) throw new IllegalArgumentException("configured notional range is incompatible with exchange range");
        DecimalValue minimumQuantity = minimum.divide(price).quantizeUp(market.quantityStep);
        if (market.minQuantity.isPositive()) minimumQuantity = minimumQuantity.max(market.minQuantity.quantizeUp(market.quantityStep));
        DecimalValue maximumQuantity = maximum.divide(price).quantizeDown(market.quantityStep);
        if (market.maxQuantity.isPositive()) maximumQuantity = maximumQuantity.min(market.maxQuantity.quantizeDown(market.quantityStep));
        if (!maximumQuantity.isPositive() || minimumQuantity.compareTo(maximumQuantity) > 0) throw new IllegalArgumentException("notional range cannot satisfy exchange quantity limits");
        DecimalValue target = stableOrderNotional(instrument.id, venueName, market.symbol, side, level, minimum, maximum, refreshGeneration);
        DecimalValue anchor = stablePriceAnchor(price, market.priceTick, strategy.repriceThresholdBps);
        DecimalValue quantity = target.divide(anchor).quantizeDown(market.quantityStep).max(minimumQuantity).min(maximumQuantity);
        if (!quantity.isPositive()) throw new IllegalArgumentException("order quantity rounded to zero");
        DecimalValue actual = price.multiply(quantity);
        if (actual.compareTo(minimum) < 0 || actual.compareTo(maximum) > 0) throw new IllegalArgumentException("rounded notional is outside effective range");
        return quantity;
    }

    static DecimalValue stableOrderNotional(String instrumentId, String venueName, String symbol, Domain.Side side,
                                            int level, DecimalValue minimum, DecimalValue maximum, long refreshGeneration) {
        if (minimum.equals(maximum)) return minimum;
        String input = instrumentId + "|" + venueName + "|" + symbol + "|" + side + "|" + level + "|" + minimum + "|" + maximum + "|" + refreshGeneration;
        long hash = fnv64a(input);
        long bucket = Long.remainderUnsigned(hash, 999_999L) + 1;
        DecimalValue fraction = DecimalValue.of(bucket).divide(DecimalValue.of(1_000_000));
        return minimum.add(maximum.subtract(minimum).multiply(fraction));
    }

    static long refreshGeneration(AppConfig.StrategyConfig strategy, Domain.Side side, int level, Instant now) {
        if (strategy.quoteRefreshSeconds <= 0 || now == null) return 0;
        int bestLevels = strategy.effectiveBestLevels();
        if (level < bestLevels) return now.getEpochSecond() / strategy.effectiveBestLevelRefreshSeconds();
        int deepLevels = strategy.levels - bestLevels;
        if (deepLevels <= 0) return now.getEpochSecond() / strategy.effectiveBestLevelRefreshSeconds();
        int refreshCount = (deepLevels * strategy.effectiveQuoteRefreshRatioBps() + 9_999) / 10_000;
        if (refreshCount < 1) refreshCount = 1;
        int groups = (deepLevels + refreshCount - 1) / refreshCount;
        int deepIndex = level - bestLevels;
        if (side == Domain.Side.SELL && deepLevels > 1) deepIndex = (deepIndex + deepLevels / 2) % deepLevels;
        int phase = deepIndex / refreshCount;
        long window = now.getEpochSecond() / strategy.effectiveQuoteRefreshSeconds();
        if (window < phase) return 0;
        return (window - phase) / groups + 1;
    }

    static long stableJitterTicks(String instrumentId, String venueName, String symbol, Domain.Side side,
                                  int level, long generation, int maxTicks) {
        if (generation == 0 || maxTicks <= 0) return 0;
        String input = instrumentId + "|" + venueName + "|" + symbol + "|" + side + "|" + level + "|" + generation + "|jitter";
        long width = maxTicks * 2L + 1;
        return Long.remainderUnsigned(fnv64a(input), width) - maxTicks;
    }

    static DecimalValue applyPriceJitter(DecimalValue price, DecimalValue tick, long ticks) {
        return ticks == 0 ? price : price.add(tick.multiply(DecimalValue.of(ticks)));
    }

    static int boundedJitterTicks(DecimalValue price, DecimalValue tick, int configuredTicks, int repriceThresholdBps) {
        if (configuredTicks <= 0 || repriceThresholdBps <= 0 || !price.isPositive() || !tick.isPositive()) return 0;
        DecimalValue maximumMove = price.multiply(DecimalValue.of(repriceThresholdBps)).divide(DecimalValue.TEN_THOUSAND);
        while (configuredTicks > 0) {
            DecimalValue move = tick.multiply(DecimalValue.of(configuredTicks));
            if (move.compareTo(maximumMove) <= 0 && move.compareTo(price) < 0) break;
            configuredTicks--;
        }
        return configuredTicks;
    }

    private static long fnv64a(String input) {
        long hash = 0xcbf29ce484222325L;
        for (byte value : input.getBytes(StandardCharsets.UTF_8)) { hash ^= value & 0xffL; hash *= 0x100000001b3L; }
        return hash;
    }

    static DecimalValue stablePriceAnchor(DecimalValue price, DecimalValue tick, int thresholdBps) {
        if (thresholdBps <= 0) return price;
        DecimalValue width = price.multiply(DecimalValue.of(thresholdBps)).divide(DecimalValue.TEN_THOUSAND).quantizeUp(tick);
        if (!width.isPositive()) width = tick;
        DecimalValue anchor = price.quantizeDown(width);
        return anchor.isPositive() ? anchor : price;
    }

    static DecimalValue applyInventorySkew(DecimalValue fair, AppConfig.StrategyConfig strategy, DecimalValue inventory) {
        if (strategy.maxBaseDeviation.signum() <= 0 || strategy.inventorySkewBps <= 0) return fair;
        DecimalValue deviation = inventory.subtract(strategy.targetBase).divide(strategy.maxBaseDeviation).min(DecimalValue.ONE).max(DecimalValue.of(-1));
        DecimalValue shift = deviation.multiply(DecimalValue.of(strategy.inventorySkewBps)).divide(DecimalValue.TEN_THOUSAND);
        return fair.multiply(DecimalValue.ONE.subtract(shift));
    }

    private static Domain.Quote quote(String instrument, String venue, String symbol, Domain.Side side, int level,
                                      DecimalValue price, DecimalValue quantity, DecimalValue reference, Instant validUntil) {
        Domain.Quote result = new Domain.Quote(); result.instrumentId = instrument; result.venue = venue; result.symbol = symbol;
        result.side = side; result.level = level; result.price = price; result.quantity = quantity; result.reference = reference; result.validUntil = validUntil; return result;
    }
}
