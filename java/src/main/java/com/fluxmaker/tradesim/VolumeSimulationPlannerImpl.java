package com.fluxmaker.tradesim;

import com.fluxmaker.config.AppConfig;
import com.fluxmaker.domain.Domain;
import com.fluxmaker.math.DecimalValue;

import java.math.BigInteger;
import java.security.SecureRandom;
import java.util.Objects;
import java.util.Random;

public final class VolumeSimulationPlannerImpl implements VolumeSimulationPlanner {
    private static final BigInteger WALK_RANGE_DIVISOR = BigInteger.valueOf(5);
    private static final int HOLD_PROBABILITY_BPS = 2_000;
    private final Random random;

    public VolumeSimulationPlannerImpl() {
        this(new SecureRandom());
    }

    VolumeSimulationPlannerImpl(Random random) {
        this.random = Objects.requireNonNull(random, "random");
    }

    @Override
    public EventPlan plan(Request request) {
        AppConfig.TradeSimulationConfig config =
                request.instrument().tradeSimulation;

        AppConfig.VenueMarketConfig market = request.market();
        Domain.Book book = request.book();

        System.out.printf(
                "[volume-simulation] planner entered instrument=%s source_venue=%s symbol=%s sequence=%d "
                        + "bid=%s ask=%s price_tick=%s quantity_step=%s config_min_quantity=%s config_max_quantity=%s%n",
                request.instrument().id,
                request.sourceVenue(),
                market.symbol,
                request.sequence(),
                book.bidPrice,
                book.askPrice,
                market.priceTick,
                market.quantityStep,
                config.minQuantity,
                config.maxQuantity
        );

        DecimalValue priceTick = market.priceTick;
        DecimalValue quantityStep = market.quantityStep;

        // 找出严格位于买一和卖一之间的合法 Tick 边界。
        DecimalValue minimumPrice = book.bidPrice
                .quantizeDown(priceTick)
                .add(priceTick);
        DecimalValue maximumPrice = book.askPrice
                .quantizeUp(priceTick)
                .subtract(priceTick);

        // 必须严格位于买一和卖一之间。
        if (minimumPrice.compareTo(book.bidPrice) <= 0
                || maximumPrice.compareTo(book.askPrice) >= 0
                || minimumPrice.compareTo(maximumPrice) > 0) {
            throw new IllegalArgumentException(
                    "买一和卖一之间没有合法价格 Tick"
            );
        }
        // 首次从价差中部开始；后续以上一次内部成交价为中心做局部随机游走，
        // 避免每条事件都在整个买卖价差中重新抽取导致价格大幅跳变。
        DecimalValue price = gradualPrice(
                request.previousPrice(),
                minimumPrice,
                maximumPrice,
                priceTick
        );

        // 从配置的最小数量开始，并按照数量精度向上对齐。
        DecimalValue minimumQuantity =
                config.minQuantity.quantizeUp(quantityStep);

        // 满足市场最小数量。
        if (market.minQuantity.isPositive()) {
            minimumQuantity = minimumQuantity.max(
                    market.minQuantity.quantizeUp(quantityStep)
            );
        }

        // 满足市场最小金额。
        if (market.minNotional.isPositive()) {
            DecimalValue quantityForMinNotional = market.minNotional
                    .divide(price)
                    .quantizeUp(quantityStep);

            minimumQuantity = minimumQuantity.max(
                    quantityForMinNotional
            );
        }

        // 计算允许的最大数量。
        DecimalValue maximumQuantity =
                config.maxQuantity.quantizeDown(quantityStep);

        if (market.maxQuantity.isPositive()) {
            maximumQuantity = maximumQuantity.min(
                    market.maxQuantity.quantizeDown(quantityStep)
            );
        }

        if (minimumQuantity.compareTo(maximumQuantity) > 0) {
            throw new IllegalArgumentException(
                    "配置的数量范围无法满足市场限制"
            );
        }
        // 示例：根据序号交替生成买、卖方向。
        Domain.Side side = request.sequence() % 2 == 0
                ? Domain.Side.SELL
                : Domain.Side.BUY;

        System.out.printf(
                "[volume-simulation] plan created instrument=%s sequence=%d side=%s bid=%s ask=%s previous_price=%s price=%s quantity=%s%n",
                request.instrument().id,
                request.sequence(),
                side,
                book.bidPrice,
                book.askPrice,
                request.previousPrice(),
                price,
                minimumQuantity
        );
        return new EventPlan(
                side,
                price,
                minimumQuantity
        );
    }

    private DecimalValue gradualPrice(
            DecimalValue previous,
            DecimalValue minimum,
            DecimalValue maximum,
            DecimalValue step
    ) {
        if (previous == null
                || previous.compareTo(minimum) < 0
                || previous.compareTo(maximum) > 0
                || !previous.equals(previous.quantizeDown(step))) {
            return minimum
                    .add(maximum)
                    .divide(DecimalValue.of(2))
                    .quantizeDown(step)
                    .max(minimum)
                    .min(maximum);
        }

        // 约 20% 的事件保持同价，使成交序列更像缓慢波动而不是机械跳动。
        if (random.nextInt(10_000) < HOLD_PROBABILITY_BPS) {
            return previous;
        }

        BigInteger legalTicks = maximum
                .subtract(minimum)
                .floorQuotient(step)
                .add(BigInteger.ONE);
        BigInteger maximumWalkTicks = legalTicks
                .add(WALK_RANGE_DIVISOR.subtract(BigInteger.ONE))
                .divide(WALK_RANGE_DIVISOR)
                .max(BigInteger.ONE);
        BigInteger walkTicks = randomBelow(maximumWalkTicks).add(BigInteger.ONE);
        DecimalValue movement = step.multiply(DecimalValue.fraction(walkTicks, BigInteger.ONE));

        boolean moveUp = random.nextBoolean();
        DecimalValue candidate = moveUp ? previous.add(movement) : previous.subtract(movement);
        if (candidate.compareTo(maximum) > 0 || candidate.compareTo(minimum) < 0) {
            candidate = moveUp ? previous.subtract(movement) : previous.add(movement);
        }
        return candidate.max(minimum).min(maximum);
    }

    private BigInteger randomBelow(BigInteger bound) {
        BigInteger selection;
        do {
            selection = new BigInteger(bound.bitLength(), random);
        } while (selection.compareTo(bound) >= 0);
        return selection;
    }
}
