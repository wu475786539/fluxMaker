package com.fluxmaker.oracle;

import com.fluxmaker.config.AppConfig;
import com.fluxmaker.domain.Domain;
import com.fluxmaker.math.DecimalValue;

import java.math.BigInteger;
import java.time.Duration;
import java.time.Instant;
import java.util.HashMap;
import java.util.List;
import java.util.Map;

public final class PancakeV2Oracle {
    private static final BigInteger Q112 = BigInteger.ONE.shiftLeft(112);
    private final RpcClient rpc;
    // Guards the metadata and snapshot maps only. RPC calls run outside this lock
    // so concurrent instruments read the chain in parallel (matches Go's o.mu,
    // which never wraps the eth_call round-trips).
    private final Object lock = new Object();
    private final Map<String, PairMetadata> metadata = new HashMap<>();
    private final Map<String, CumulativeSnapshot> snapshots = new HashMap<>();

    private record PairMetadata(String token0, String token1, int decimals0, int decimals1, String factory) {}
    private record PairState(PairMetadata metadata, BigInteger reserve0, BigInteger reserve1, long reserveTimestamp, BigInteger cumulative0, BigInteger cumulative1, RpcClient.Block block) {}
    private static final class CumulativeSnapshot { BigInteger price0, price1; long unixTime; DecimalValue twap0 = DecimalValue.ZERO, twap1 = DecimalValue.ZERO; boolean ready; }
    private record LegPrice(DecimalValue spot, DecimalValue twap, boolean ready) {}

    public PancakeV2Oracle(RpcClient rpc) { this.rpc = rpc; }

    public Domain.ReferencePrice price(AppConfig.InstrumentConfig instrument) {
        if (!"pancake_v2".equals(instrument.reference.type)) throw new IllegalArgumentException("unsupported oracle type " + instrument.reference.type);
        DecimalValue spot = DecimalValue.ONE, twap = DecimalValue.ONE; boolean allReady = true; RpcClient.Block block = rpc.latestBlock();
        for (AppConfig.PairLegConfig leg : instrument.reference.legs) { LegPrice value = readLeg(leg, instrument.reference.twapWindowSeconds, block); spot = spot.multiply(value.spot); if (value.ready) twap = twap.multiply(value.twap); else allReady = false; }
        DecimalValue price = twap; String confidence = "high";
        if (!allReady) { if (!instrument.reference.allowSpotDuringWarmup) throw new IllegalStateException("TWAP warming up"); price = spot; confidence = "warming_up"; }
        else if (instrument.reference.maxSpotTwapDeviationBps > 0) { DecimalValue deviation = spot.subtract(twap).abs().divide(twap).multiply(DecimalValue.TEN_THOUSAND); if (deviation.compareTo(DecimalValue.of(instrument.reference.maxSpotTwapDeviationBps)) > 0) throw new IllegalStateException("spot/TWAP deviation " + deviation + " bps exceeds " + instrument.reference.maxSpotTwapDeviationBps); }
        Instant blockTime = Instant.ofEpochSecond(block.timestamp()); if (blockTime.isAfter(Instant.now().plusSeconds(15))) throw new IllegalStateException("chain block timestamp is in the future: " + blockTime); if (instrument.reference.staleAfterSeconds > 0 && Duration.between(blockTime, Instant.now()).compareTo(Duration.ofSeconds(instrument.reference.staleAfterSeconds)) > 0) throw new IllegalStateException("chain block is stale: " + blockTime);
        long validSeconds = instrument.reference.staleAfterSeconds > 0 ? instrument.reference.staleAfterSeconds : 15; Domain.ReferencePrice result = new Domain.ReferencePrice(); result.instrumentId = instrument.id; result.price = price; result.spot = spot; result.twapReady = allReady; result.confidence = confidence; result.blockNumber = block.number(); result.blockTime = blockTime; result.validUntil = Instant.now().plusSeconds(validSeconds); return result;
    }

    private LegPrice readLeg(AppConfig.PairLegConfig leg, int windowSeconds, RpcClient.Block block) {
        PairState state = readPair(leg.pairAddress, block); if (leg.expectedFactory != null && !leg.expectedFactory.isEmpty() && !RpcClient.normalizeAddress(leg.expectedFactory).equals(state.metadata.factory)) throw new IllegalStateException("pair factory " + state.metadata.factory + " does not match expected " + RpcClient.normalizeAddress(leg.expectedFactory));
        String base = RpcClient.normalizeAddress(leg.baseToken), quote = RpcClient.normalizeAddress(leg.quoteToken); BigInteger reserveBase, reserveQuote; int baseDecimals, quoteDecimals; boolean baseIsZero;
        if (base.equals(state.metadata.token0) && quote.equals(state.metadata.token1)) { reserveBase = state.reserve0; reserveQuote = state.reserve1; baseDecimals = state.metadata.decimals0; quoteDecimals = state.metadata.decimals1; baseIsZero = true; }
        else if (base.equals(state.metadata.token1) && quote.equals(state.metadata.token0)) { reserveBase = state.reserve1; reserveQuote = state.reserve0; baseDecimals = state.metadata.decimals1; quoteDecimals = state.metadata.decimals0; baseIsZero = false; }
        else throw new IllegalStateException("configured base/quote tokens do not match pair");
        if (reserveBase.signum() <= 0 || reserveQuote.signum() <= 0) throw new IllegalStateException("empty reserves"); long idle = (state.block.timestamp() - state.reserveTimestamp) & 0xffff_ffffL; if (leg.maxIdleSeconds > 0 && idle > leg.maxIdleSeconds) throw new IllegalStateException("pair has not updated for " + idle + " seconds");
        DecimalValue quoteUnits = DecimalValue.fraction(reserveQuote, BigInteger.TEN.pow(quoteDecimals)); if (leg.minQuoteReserve.isPositive() && quoteUnits.compareTo(leg.minQuoteReserve) < 0) throw new IllegalStateException("quote reserve " + quoteUnits + " below minimum " + leg.minQuoteReserve);
        DecimalValue spot = DecimalValue.fraction(reserveQuote.multiply(BigInteger.TEN.pow(baseDecimals)), reserveBase.multiply(BigInteger.TEN.pow(quoteDecimals))); String key = RpcClient.normalizeAddress(leg.pairAddress);
        synchronized (lock) {
            CumulativeSnapshot previous = snapshots.get(key);
            if (previous == null || state.block.timestamp() <= previous.unixTime) { CumulativeSnapshot initial = new CumulativeSnapshot(); initial.price0 = state.cumulative0; initial.price1 = state.cumulative1; initial.unixTime = state.block.timestamp(); snapshots.put(key, initial); return new LegPrice(spot, DecimalValue.ZERO, false); }
            long elapsed = state.block.timestamp() - previous.unixTime; if (elapsed < windowSeconds) return previous.ready ? new LegPrice(spot, baseIsZero ? previous.twap0 : previous.twap1, true) : new LegPrice(spot, DecimalValue.ZERO, false);
            BigInteger delta0 = state.cumulative0.subtract(previous.price0), delta1 = state.cumulative1.subtract(previous.price1); if (delta0.signum() <= 0 || delta1.signum() <= 0) throw new IllegalStateException("non-positive cumulative price delta"); BigInteger denominator = BigInteger.valueOf(elapsed).multiply(Q112);
            DecimalValue twap0 = DecimalValue.fraction(delta0.multiply(BigInteger.TEN.pow(state.metadata.decimals0)), denominator.multiply(BigInteger.TEN.pow(state.metadata.decimals1))); DecimalValue twap1 = DecimalValue.fraction(delta1.multiply(BigInteger.TEN.pow(state.metadata.decimals1)), denominator.multiply(BigInteger.TEN.pow(state.metadata.decimals0)));
            CumulativeSnapshot next = new CumulativeSnapshot(); next.price0 = state.cumulative0; next.price1 = state.cumulative1; next.unixTime = state.block.timestamp(); next.twap0 = twap0; next.twap1 = twap1; next.ready = true; snapshots.put(key, next); return new LegPrice(spot, baseIsZero ? twap0 : twap1, true);
        }
    }

    private PairState readPair(String rawPair, RpcClient.Block block) {
        String pair = RpcClient.normalizeAddress(rawPair); if (!pair.matches("^0x[0-9a-f]{40}$")) throw new IllegalArgumentException("invalid pair address"); PairMetadata meta = metadata(pair, block.tag());
        List<byte[]> words = rpc.batchCall(List.of(new RpcClient.BatchCall(pair, RpcClient.SELECTOR_GET_RESERVES), new RpcClient.BatchCall(pair, RpcClient.SELECTOR_PRICE0), new RpcClient.BatchCall(pair, RpcClient.SELECTOR_PRICE1)), block.tag()); byte[] reserves = words.get(0); if (reserves.length < 96) throw new IllegalStateException("short getReserves response"); BigInteger reserve0 = RpcClient.wordInt(reserves, 0), reserve1 = RpcClient.wordInt(reserves, 1); long reserveTimestamp = RpcClient.wordInt(reserves, 2).longValue() & 0xffff_ffffL; BigInteger cumulative0 = RpcClient.wordInt(words.get(1), 0), cumulative1 = RpcClient.wordInt(words.get(2), 0); long elapsed = (block.timestamp() - reserveTimestamp) & 0xffff_ffffL;
        if (elapsed > 0 && reserve0.signum() > 0 && reserve1.signum() > 0) { BigInteger price0 = reserve1.shiftLeft(112).divide(reserve0), price1 = reserve0.shiftLeft(112).divide(reserve1); cumulative0 = cumulative0.add(price0.multiply(BigInteger.valueOf(elapsed))); cumulative1 = cumulative1.add(price1.multiply(BigInteger.valueOf(elapsed))); }
        return new PairState(meta, reserve0, reserve1, reserveTimestamp, cumulative0, cumulative1, block);
    }

    private PairMetadata metadata(String pair, String blockTag) {
        synchronized (lock) { PairMetadata cached = metadata.get(pair); if (cached != null) return cached; }
        String token0 = RpcClient.wordAddress(rpc.callAt(pair, RpcClient.SELECTOR_TOKEN0, blockTag)), token1 = RpcClient.wordAddress(rpc.callAt(pair, RpcClient.SELECTOR_TOKEN1, blockTag)); int decimals0 = RpcClient.wordInt(rpc.callAt(token0, RpcClient.SELECTOR_DECIMALS, blockTag), 0).intValue(), decimals1 = RpcClient.wordInt(rpc.callAt(token1, RpcClient.SELECTOR_DECIMALS, blockTag), 0).intValue(); String factory = RpcClient.wordAddress(rpc.callAt(pair, RpcClient.SELECTOR_FACTORY, blockTag)); PairMetadata value = new PairMetadata(token0, token1, decimals0, decimals1, factory);
        synchronized (lock) { metadata.put(pair, value); return value; }
    }
}
