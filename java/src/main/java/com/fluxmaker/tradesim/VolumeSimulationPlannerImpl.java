package com.fluxmaker.tradesim;

import com.fluxmaker.config.AppConfig;
import com.fluxmaker.domain.Domain;
import com.fluxmaker.math.DecimalValue;
import com.fluxmaker.venue.VenueClient;

import java.math.BigInteger;
import java.security.SecureRandom;
import java.util.ArrayList;
import java.util.List;

/**
 * 真实 MGBX 批量交易规划器。
 *
 * <p>在买一卖一价差范围内生成多笔价格/数量计划，并通过
 * {@link VenueClient#placePostOnly} 向 MGBX 提交真实 post-only 订单。
 * 每笔返回的 {@link EventPlan} 携带真实 {@code orderId}，
 * 下游可据此区分真实订单与仿真成交。
 *
 * <p>当 {@code request.venueClient()} 为 null 时，退化为纯仿真模式（不下真实订单）。
 */
public final class VolumeSimulationPlannerImpl implements VolumeSimulationPlanner {
    private static final SecureRandom RANDOM = new SecureRandom();

    @Override
    public List<EventPlan> plan(Request request) {
        // 1. 读取配置和市场数据
        AppConfig.TradeSimulationConfig config =
                request.instrument().tradeSimulation;

        AppConfig.VenueMarketConfig market = request.market();
        Domain.Book book = request.book();
        VenueClient venueClient = request.venueClient();

        // 批量大小：至少为 1，从配置中读取
        int batchSize = Math.max(1, config.batchSize);

        System.out.printf(
                "[volume-simulation] planner entered instrument=%s source_venue=%s symbol=%s sequence=%d "
                        + "bid=%s ask=%s price_tick=%s quantity_step=%s config_min_quantity=%s config_max_quantity=%s batch_size=%d real=%s%n",
                request.instrument().id,
                request.sourceVenue(),
                market.symbol,
                request.sequence(),
                book.bidPrice,
                book.askPrice,
                market.priceTick,
                market.quantityStep,
                config.minQuantity,
                config.maxQuantity,
                batchSize,
                venueClient != null
        );

        // 2. 获取市场精度约束
        DecimalValue priceTick = market.priceTick;
        DecimalValue quantityStep = market.quantityStep;

        // 3. 计算买一卖一之间的合法价格区间
        //    最低价 = 买一价向下对齐到 tick 后 + 1 tick（严格高于买一）
        DecimalValue minimumPrice = book.bidPrice
                .quantizeDown(priceTick)
                .add(priceTick);
        //    最高价 = 卖一价向上对齐到 tick 后 - 1 tick（严格低于卖一）
        DecimalValue maximumPrice = book.askPrice
                .quantizeUp(priceTick)
                .subtract(priceTick);

        // 4. 校验：必须存在至少一个合法价格 tick
        if (minimumPrice.compareTo(book.bidPrice) <= 0
                || maximumPrice.compareTo(book.askPrice) >= 0
                || minimumPrice.compareTo(maximumPrice) > 0) {
            throw new IllegalArgumentException(
                    "买一和卖一之间没有合法价格 Tick"
            );
        }

        // 5. 计算最小数量：从配置的最小数量开始，按数量精度向上对齐
        DecimalValue minimumQuantity =
                config.minQuantity.quantizeUp(quantityStep);

        // 6. 满足市场最小数量限制
        if (market.minQuantity.isPositive()) {
            minimumQuantity = minimumQuantity.max(
                    market.minQuantity.quantizeUp(quantityStep)
            );
        }

        // 7. 计算最大数量：从配置的最大数量开始，按数量精度向下对齐
        DecimalValue maximumQuantity =
                config.maxQuantity.quantizeDown(quantityStep);

        // 8. 满足市场最大数量限制
        if (market.maxQuantity.isPositive()) {
            maximumQuantity = maximumQuantity.min(
                    market.maxQuantity.quantizeDown(quantityStep)
            );
        }

        // 9. 校验：最小数量不能超过最大数量
        if (minimumQuantity.compareTo(maximumQuantity) > 0) {
            throw new IllegalArgumentException(
                    "配置的数量范围无法满足市场限制"
            );
        }

        // 10. 循环生成批量订单计划
        List<EventPlan> plans = new ArrayList<>(batchSize);
        for (int i = 0; i < batchSize; i++) {
            // 10a. 在合法价格区间内随机选择一个价格
            DecimalValue price = randomStep(
                    minimumPrice,
                    maximumPrice,
                    priceTick
            );

            // 10b. 每笔独立计算满足最小名义金额的数量下限
            DecimalValue effectiveMinQuantity = minimumQuantity;
            if (market.minNotional.isPositive()) {
                DecimalValue quantityForMinNotional = market.minNotional
                        .divide(price)
                        .quantizeUp(quantityStep);
                effectiveMinQuantity = effectiveMinQuantity.max(
                        quantityForMinNotional
                );
            }

            // 10c. 校验：满足最小金额后的数量不能超过最大数量
            if (effectiveMinQuantity.compareTo(maximumQuantity) > 0) {
                throw new IllegalArgumentException(
                        "配置的数量范围无法满足市场限制（含最小金额）"
                );
            }

            // 10d. 在有效数量区间内随机选择一个数量
            DecimalValue quantity = randomStep(
                    effectiveMinQuantity,
                    maximumQuantity,
                    quantityStep
            );

            // 10e. 根据序号交替生成买、卖方向
            Domain.Side side = (request.sequence() + i) % 2 == 0
                    ? Domain.Side.SELL
                    : Domain.Side.BUY;

            // 10f. 向 MGBX 提交真实 post-only 订单
            String orderId = null;
            if (venueClient != null) {
                try {
                    // 构造下单请求：symbol、方向、价格、数量
                    // clientId 为 null（MGBX 不支持 client order ID）
                    // fenceGeneration 为 0（仿真 venue 不适用租约机制）
                    VenueClient.PlaceRequest placeRequest = new VenueClient.PlaceRequest(
                            market.symbol, side, price, quantity,
                            null,
                            0
                    );
                    // 调用 MGBX POST /spot/v1/u/trade/order/create
                    // timeInForce=GTX（post-only，只挂单不立即成交）
                    Domain.Order order = venueClient.placePostOnly(placeRequest);
                    orderId = order.orderId;
                    System.out.printf(
                            "[volume-simulation] real order placed instrument=%s sequence=%d side=%s price=%s quantity=%s order_id=%s%n",
                            request.instrument().id,
                            request.sequence() + i,
                            side,
                            price,
                            quantity,
                            orderId
                    );
                } catch (RuntimeException e) {
                    System.err.printf(
                            "[volume-simulation] order placement failed instrument=%s sequence=%d side=%s price=%s quantity=%s error=%s%n",
                            request.instrument().id,
                            request.sequence() + i,
                            side,
                            price,
                            quantity,
                            e.getMessage()
                    );
                    throw e;
                }
            }

            // 10g. 将计划加入结果列表（orderId 非空表示真实订单，null 表示仿真）
            plans.add(new EventPlan(side, price, quantity, orderId));
        }
        return plans;
    }

    /**
     * 在 [minimum, maximum] 区间内按 step 步长随机选择一个值。
     * 使用加密安全的随机数生成器，确保均匀分布。
     */
    private static DecimalValue randomStep(
            DecimalValue minimum,
            DecimalValue maximum,
            DecimalValue step
    ) {
        // 计算可选步数 = (max - min) / step + 1
        BigInteger choices = maximum
                .subtract(minimum)
                .floorQuotient(step)
                .add(BigInteger.ONE);
        // 在 [0, choices) 范围内随机选择
        BigInteger selection;
        do {
            selection = new BigInteger(choices.bitLength(), RANDOM);
        } while (selection.compareTo(choices) >= 0);
        // 结果 = min + step * selection
        return minimum.add(
                step.multiply(DecimalValue.fraction(selection, BigInteger.ONE))
        );
    }
}
