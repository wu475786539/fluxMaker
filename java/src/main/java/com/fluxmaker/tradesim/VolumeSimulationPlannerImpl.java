package com.fluxmaker.tradesim;

import com.fluxmaker.app.RuntimeFactory;
import com.fluxmaker.config.AppConfig;
import com.fluxmaker.config.ConfigStore;
import com.fluxmaker.credentials.CredentialService;
import com.fluxmaker.domain.Domain;
import com.fluxmaker.infra.Database;
import com.fluxmaker.infra.RedisClient;
import com.fluxmaker.math.DecimalValue;
import com.fluxmaker.venue.VenueClient;

import java.math.BigInteger;
import java.security.SecureRandom;
import java.util.ArrayList;
import java.util.List;
import java.util.Map;
import java.util.concurrent.ThreadLocalRandom;
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

    // ---- 指定测哪个交易(留空=自动挑第一个符合条件的;填了就锁定)----
    private static final String PIN_VENUE = "mgbx";       // 交易所名(config.venues 的 key),如 "mgbx" / "binance"
    private static final String PIN_INSTRUMENT = "gdt_usdt";  // 币对 id,如 "gdt_usdt" / "bnb_usdt"
    private static final String PIN_SYMBOL = "GDT_USDT";      // 交易所 symbol,如 "GDT_USDT" / "BNBUSDT"
    private static final String CLIENT_ID_PREFIX = "fm-it-";

    private record Target(String venueName, String instrumentId, String symbol, String type) {}
    private record Bootstrap(VenueClient client, Target target) {}

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

        DecimalValue quantity = randomQuantity(minimumQuantity, maximumQuantity);

        // 每次独立、等概率地随机生成买卖方向，不再依赖事件序号。
        Domain.Side side = RANDOM.nextBoolean()
                ? Domain.Side.BUY
                : Domain.Side.SELL;

        System.out.printf(
                "[volume-simulation] plan created instrument=%s sequence=%d side=%s bid=%s ask=%s previous_price=%s price=%s quantity=%s%n",
                request.instrument().id,
                request.sequence(),
                side,
                book.bidPrice,
                book.askPrice,
                request.previousPrice(),
                price,
                quantity
        );

        try (Database database = Database.fromEnv(); RedisClient redis = RedisClient.fromEnv()) {
            Bootstrap bootstrap = buildClient(database, redis);
            VenueClient client = bootstrap.client();
            String symbol = market.symbol;
            List<VenueClient.PlaceRequest> requests = new ArrayList<>();
            System.out.println(symbol);
            String clientId_buy = CLIENT_ID_PREFIX + Long.toString(System.nanoTime(), 36) + "-" + 1;
            String clientId_sell = CLIENT_ID_PREFIX + Long.toString(System.nanoTime(), 36) + "-" + 2;

            requests.add(new VenueClient.PlaceRequest(symbol, side == Domain.Side.BUY ?Domain.Side.BUY:Domain.Side.SELL, price, quantity, clientId_buy, 0));
            requests.add(new VenueClient.PlaceRequest(symbol, side == Domain.Side.BUY ?Domain.Side.SELL:Domain.Side.BUY, price, quantity, clientId_sell, 0));

            List<Domain.Order> placed = new ArrayList<>();
            placed = client.placePostOnlyBatch(requests);
            for (Domain.Order order : placed) System.out.println("  orderId=" + order.orderId+"  symbol=" + order.symbol + " price=" + order.price + " qty=" + order.quantity+ " state=" + order.state+" side="+order.side);

        }

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

    private static Target pickTarget(AppConfig config, String venueFilter) {
        for (Map.Entry<String, AppConfig.VenueConfig> venueEntry : config.venues.entrySet()) {
            String venueName = venueEntry.getKey();
            AppConfig.VenueConfig venue = venueEntry.getValue();
            if (!venue.enabled) continue;
            if (!venueFilter.isEmpty() && !venue.type.equalsIgnoreCase(venueFilter)) continue;   // 按类型过滤(env)
            if (!PIN_VENUE.isEmpty() && !venueName.equalsIgnoreCase(PIN_VENUE)) continue;         // 锁定交易所名
            for (Map.Entry<String, AppConfig.VenueMarketConfig> marketEntry : venue.markets.entrySet()) {
                String instrumentId = marketEntry.getKey();
                AppConfig.VenueMarketConfig market = marketEntry.getValue();
                if (market.credentialId <= 0) continue;                                          // 必须绑了凭证
                if (!PIN_INSTRUMENT.isEmpty() && !instrumentId.equalsIgnoreCase(PIN_INSTRUMENT)) continue; // 锁定币对 id
                if (!PIN_SYMBOL.isEmpty() && !market.symbol.equalsIgnoreCase(PIN_SYMBOL)) continue;        // 锁定 symbol
                return new Target(venueName, instrumentId, market.symbol, venue.type);
            }
        }
        return null;
    }

    /** Bootstraps everything the way the engine does: reads the active config, applies
     *  runtime defaults, picks the target market, and builds its venue client (resolving
     *  and decrypting the credential from the DB). Skips the test on any missing
     *  precondition (no config, no eligible market, or client build failure). */
    private static Bootstrap buildClient(Database database, RedisClient redis) {
        String venueFilter = System.getenv().getOrDefault("FLUXMAKER_IT_VENUE", "").trim();
        CredentialService credentials = new CredentialService(database, System.getenv("CREDENTIAL_MASTER_KEY"));

        AppConfig config;
        try { config = new ConfigStore(database, redis).loadActive().config; }
        catch (ConfigStore.NotFound e) { return null; }
        config.applyRuntimeSafetyDefaults();

        Target target = pickTarget(config, venueFilter);

        Map<String, VenueClient> clients = RuntimeFactory.buildVenuesIsolated(config, credentials).clients();
        VenueClient client = clients.get(RuntimeFactory.clientKey(target.venueName, target.instrumentId));
        return new Bootstrap(client, target);
    }

    /** 返回 [min, max] 之间的随机整数(含两端),结果仍是 DecimalValue。 */
    static DecimalValue randomQuantity(DecimalValue minimumQuantity, DecimalValue maximumQuantity) {
        long min = minimumQuantity.floorQuotient(DecimalValue.ONE).longValueExact();  // 取整→long
        long max = maximumQuantity.floorQuotient(DecimalValue.ONE).longValueExact();
        if (min > max) throw new IllegalArgumentException("min > max: " + min + " > " + max);
        long value = ThreadLocalRandom.current().nextLong(min, max + 1);              // 关键:+1 让上界含进去
        return DecimalValue.of(value);
    }

}
