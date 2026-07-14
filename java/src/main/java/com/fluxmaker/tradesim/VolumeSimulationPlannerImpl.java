package com.fluxmaker.tradesim;

import com.fluxmaker.config.AppConfig;
import com.fluxmaker.domain.Domain;
import com.fluxmaker.math.DecimalValue;

public final class VolumeSimulationPlannerImpl implements VolumeSimulationPlanner {
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

        // 选择买一上方第一个合法 Tick。
        DecimalValue price = book.bidPrice
                .quantizeDown(priceTick)
                .add(priceTick);

        // 必须严格位于买一和卖一之间。
        if (price.compareTo(book.bidPrice) <= 0
                || price.compareTo(book.askPrice) >= 0) {
            throw new IllegalArgumentException(
                    "买一和卖一之间没有合法价格 Tick"
            );
        }

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
                "[volume-simulation] plan created instrument=%s sequence=%d side=%s price=%s quantity=%s%n",
                request.instrument().id,
                request.sequence(),
                side,
                price,
                minimumQuantity
        );
        return new EventPlan(
                side,
                price,
                minimumQuantity
        );
    }
}
