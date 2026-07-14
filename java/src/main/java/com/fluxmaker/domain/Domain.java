package com.fluxmaker.domain;

import com.fasterxml.jackson.annotation.JsonInclude;
import com.fluxmaker.math.DecimalValue;

import java.time.Instant;
import java.util.ArrayList;
import java.util.List;

public final class Domain {
    private Domain() {}

    public enum Side { BUY, SELL }
    public enum Mode { shadow, live }
    public enum OrderState { NEW, PARTIALLY_FILLED, FILLED, CANCELED, REJECTED, EXPIRED, UNKNOWN }

    public static final class Book {
        public String venue = "";
        public String symbol = "";
        public DecimalValue bidPrice = DecimalValue.ZERO;
        public DecimalValue bidQty = DecimalValue.ZERO;
        public DecimalValue askPrice = DecimalValue.ZERO;
        public DecimalValue askQty = DecimalValue.ZERO;
        public Instant timestamp;
        public boolean hasBid() { return bidPrice != null && bidPrice.isPositive(); }
        public boolean hasAsk() { return askPrice != null && askPrice.isPositive(); }
        public boolean hasPrices() { return hasBid() || hasAsk(); }
        public boolean twoSided() { return hasBid() && hasAsk(); }
    }

    public static final class ReferencePrice {
        public String instrumentId = "";
        public DecimalValue price = DecimalValue.ZERO;
        public DecimalValue spot = DecimalValue.ZERO;
        public boolean twapReady;
        public String confidence = "";
        public long blockNumber;
        public Instant blockTime;
        public Instant validUntil;
    }

    public static final class Quote {
        public String instrumentId = "";
        public String venue = "";
        public String symbol = "";
        public Side side;
        public int level;
        public DecimalValue price = DecimalValue.ZERO;
        public DecimalValue quantity = DecimalValue.ZERO;
        public DecimalValue reference = DecimalValue.ZERO;
        public Instant validUntil;
    }

    public static final class Order {
        public String venue = "";
        public String orderId = "";
        @JsonInclude(JsonInclude.Include.NON_EMPTY)
        public String clientId = "";
        public String symbol = "";
        public Side side;
        public DecimalValue price = DecimalValue.ZERO;
        public DecimalValue quantity = DecimalValue.ZERO;
        public DecimalValue executedQty = DecimalValue.ZERO;
        public OrderState state = OrderState.UNKNOWN;
        public Instant createdAt;
    }

    public static final class Balance {
        public String asset = "";
        public DecimalValue free = DecimalValue.ZERO;
        public DecimalValue locked = DecimalValue.ZERO;
    }

    public static final class QuoteBudget {
        public int reserveBps;
        public int targetOrders;
        public int eligibleOrders;
        public DecimalValue baseBudget = DecimalValue.ZERO;
        public DecimalValue baseRequired = DecimalValue.ZERO;
        public DecimalValue quoteBudget = DecimalValue.ZERO;
        public DecimalValue quoteRequired = DecimalValue.ZERO;
        public boolean baseLimited;
        public boolean quoteLimited;
    }

    public static final class MarketRules {
        public String symbol = "";
        public String baseAsset = "";
        public String quoteAsset = "";
        public DecimalValue priceTick = DecimalValue.ZERO;
        public DecimalValue quantityStep = DecimalValue.ZERO;
        public DecimalValue minQuantity = DecimalValue.ZERO;
        public DecimalValue maxQuantity = DecimalValue.ZERO;
        public DecimalValue minNotional = DecimalValue.ZERO;
        public DecimalValue maxNotional = DecimalValue.ZERO;
        public DecimalValue minPrice = DecimalValue.ZERO;
        public DecimalValue maxPrice = DecimalValue.ZERO;
        public int maxOpenOrders;
    }

    public static final class Fill {
        public String venue = "";
        public String tradeId = "";
        public String orderId = "";
        public String symbol = "";
        public Side side;
        public DecimalValue price = DecimalValue.ZERO;
        public DecimalValue quantity = DecimalValue.ZERO;
        public DecimalValue quoteQuantity = DecimalValue.ZERO;
        public DecimalValue fee = DecimalValue.ZERO;
        @JsonInclude(JsonInclude.Include.NON_EMPTY)
        public String feeAsset = "";
        public boolean maker;
        @JsonInclude(JsonInclude.Include.NON_DEFAULT)
        public boolean aggregate;
        @JsonInclude(JsonInclude.Include.NON_DEFAULT)
        public boolean simulated;
        public Instant timestamp;
    }

    public static List<Order> copyOrders(List<Order> source) {
        return source == null ? new ArrayList<>() : new ArrayList<>(source);
    }
}
