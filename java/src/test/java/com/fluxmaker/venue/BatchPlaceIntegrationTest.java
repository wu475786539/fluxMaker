package com.fluxmaker.venue;

import com.fluxmaker.app.RuntimeFactory;
import com.fluxmaker.config.AppConfig;
import com.fluxmaker.config.ConfigStore;
import com.fluxmaker.credentials.CredentialService;
import com.fluxmaker.domain.Domain;
import com.fluxmaker.infra.Database;
import com.fluxmaker.infra.RedisClient;
import com.fluxmaker.math.DecimalValue;
import com.fluxmaker.runtime.RuntimeStore;
import org.junit.jupiter.api.Assumptions;
import org.junit.jupiter.api.Test;
import org.junit.jupiter.api.condition.EnabledIfEnvironmentVariable;

import java.util.ArrayList;
import java.util.List;
import java.util.Map;

import static org.junit.jupiter.api.Assertions.assertEquals;
import static org.junit.jupiter.api.Assertions.assertFalse;

/**
 * REAL batch order placement against whatever venue is configured in the database
 * (use a TESTNET only). Nothing is hand-typed: venue, base URL, symbol and the
 * (encrypted) API credential are all read from the published config exactly the
 * way the engine bootstraps — via ConfigStore + CredentialService + RuntimeFactory.
 *
 * Disabled by default; only runs when FLUXMAKER_IT=1. It needs the same connection
 * env the app uses (the API secret stays encrypted in Postgres, decrypted only in
 * memory with the master key):
 *   DATABASE_URL, REDIS_ADDR, REDIS_PASSWORD, CREDENTIAL_MASTER_KEY
 * Optional FLUXMAKER_IT_VENUE=binance|mgbx picks which venue type to exercise;
 * otherwise the first enabled market with a bound credential is used.
 *
 *   FLUXMAKER_IT=1 DATABASE_URL=postgres://fluxmaker:pw@localhost:5432/fluxmaker \
 *   REDIS_ADDR=localhost:6379 REDIS_PASSWORD=pw CREDENTIAL_MASTER_KEY=... \
 *   mvn -Dtest=BatchPlaceIntegrationTest test
 *
 * NOTE: it places small post-only BUY orders far below the market (they rest as
 * makers and never fill), then cancels every order it created.
 */
@EnabledIfEnvironmentVariable(named = "FLUXMAKER_IT", matches = "1|true|yes")
class BatchPlaceIntegrationTest {

    private static final String CLIENT_ID_PREFIX = "fm-it-";
    private static final int BATCH_SIZE = 3;

    // ---- 指定测哪个交易(留空=自动挑第一个符合条件的;填了就锁定)----
    private static final String PIN_VENUE = "mgbx";       // 交易所名(config.venues 的 key),如 "mgbx" / "binance"
    private static final String PIN_INSTRUMENT = "gdt_usdt";  // 币对 id,如 "gdt_usdt" / "bnb_usdt"
    private static final String PIN_SYMBOL = "GDT_USDT";      // 交易所 symbol,如 "GDT_USDT" / "BNBUSDT"

    private record Target(String venueName, String instrumentId, String symbol, String type) {}

    private record Bootstrap(VenueClient client, Target target) {}

    @Test
    void placesAndCancelsARealBatchFromDbConfig() {
        Assumptions.assumeTrue(set("DATABASE_URL") && set("REDIS_ADDR") && set("CREDENTIAL_MASTER_KEY"),
                "set DATABASE_URL / REDIS_ADDR / CREDENTIAL_MASTER_KEY to run");
        try (Database database = Database.fromEnv(); RedisClient redis = RedisClient.fromEnv()) {
            Bootstrap bootstrap = buildClient(database, redis);
            VenueClient client = bootstrap.client();
            Target target = bootstrap.target();
            String symbol = target.symbol;
            System.out.println("batch place IT: venue=" + target.venueName + " type=" + target.type + " symbol=" + symbol);

            Domain.MarketRules rules = client.marketRules(symbol);
            Domain.Book book = client.topBook(symbol);
            boolean liveBook = book.hasPrices();
            // Empty book (e.g. a thin testnet pair): fall back to the engine's own
            // reference price from the runtime snapshot so we can still price safely.
            DecimalValue reference = liveBook
                    ? (book.hasBid() ? book.bidPrice : book.askPrice)
                    : engineReference(redis, target.instrumentId);
            Assumptions.assumeTrue(reference.isPositive(),
                    "no live book and no engine reference price for " + symbol);

            DecimalValue tick = rules.priceTick.isPositive() ? rules.priceTick : DecimalValue.parse("0.01");
            DecimalValue step = rules.quantityStep.isPositive() ? rules.quantityStep : DecimalValue.parse("0.001");
            // Live book: place well below the bid so post-only cannot cross. Empty
            // book: nothing to cross, so place just below the reference (stays inside
            // exchange price bands while still resting as a maker).
            DecimalValue factor = liveBook ? DecimalValue.parse("0.5") : DecimalValue.parse("0.9");
            DecimalValue basePrice = reference.multiply(factor).quantizeDown(tick);

            DecimalValue minNotional = rules.minNotional.isPositive() ? rules.minNotional : DecimalValue.parse("5");
            DecimalValue quantity = minNotional.multiply(DecimalValue.parse("1.05")).divide(basePrice)
                    .max(rules.minQuantity.isPositive() ? rules.minQuantity : step)
                    .quantizeUp(step);
            quantity = quantity.multiply(DecimalValue.parse("10"));
            List<VenueClient.PlaceRequest> requests = new ArrayList<>();
            for (int i = 0; i < BATCH_SIZE; i++) {
                DecimalValue price = basePrice.subtract(tick.multiply(DecimalValue.of(i))).quantizeDown(tick);
                String clientId = CLIENT_ID_PREFIX + Long.toString(System.nanoTime(), 36) + "-" + i;
                requests.add(new VenueClient.PlaceRequest(symbol, Domain.Side.BUY, price, quantity, clientId, 0));
                System.out.println("symbol=" + symbol + " type=" + Domain.Side.BUY + " price=" + price+ " quantity=" + quantity);
            }

            boolean keep = "1".equals(System.getenv("FLUXMAKER_IT_KEEP")) || "true".equalsIgnoreCase(System.getenv().getOrDefault("FLUXMAKER_IT_KEEP", ""));
            List<Domain.Order> placed = new ArrayList<>();
            try {
                placed = client.placePostOnlyBatch(requests);
                assertEquals(BATCH_SIZE, placed.size(), "every request should come back as an order");
                for (Domain.Order order : placed)
                    assertFalse(order.orderId == null || order.orderId.isEmpty(), "exchange returned an order id");

                // Proof they are actually live on the exchange, not just an acknowledgement:
                // query the venue's own open-orders endpoint and match the ids we placed.
                long resting = restingCount(client, symbol, placed);
                System.out.println("verified " + resting + "/" + BATCH_SIZE + " orders resting on the exchange:");
                for (Domain.Order order : placed) System.out.println("  orderId=" + order.orderId + " price=" + order.price + " qty=" + order.quantity);
                assertEquals(BATCH_SIZE, resting, "all placed orders should be live on the exchange before cleanup");

                if (keep) System.out.println("FLUXMAKER_IT_KEEP set: leaving these orders on the exchange — go check the testnet book, then cancel them yourself.");
            } finally {
                if (!keep) cleanup(client, symbol, placed);
            }
        }
    }

    /** Enabled venue market with a bound credential. Honors the PIN_* constants
     *  (and the FLUXMAKER_IT_VENUE type filter); with all blank it takes the first match. */
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
        catch (ConfigStore.NotFound e) { return Assumptions.abort("no published configuration in the database"); }
        config.applyRuntimeSafetyDefaults();

        Target target = pickTarget(config, venueFilter);
        Assumptions.assumeTrue(target != null,
                "no enabled venue market with a bound credential" + (venueFilter.isEmpty() ? "" : " for venue type " + venueFilter));

        Map<String, VenueClient> clients = RuntimeFactory.buildVenuesIsolated(config, credentials).clients();
        VenueClient client = clients.get(RuntimeFactory.clientKey(target.venueName, target.instrumentId));
        Assumptions.assumeTrue(client != null, "failed to build client for " + target.venueName + "/" + target.instrumentId);
        return new Bootstrap(client, target);
    }

    /** The engine's reference price for the instrument. Polls a few seconds because
     *  a single cycle can momentarily lack a reference (e.g. a transient chain RPC
     *  blip); the engine refreshes it roughly every second. Zero if none appears. */
    private static DecimalValue engineReference(RedisClient redis, String instrumentId) {
        RuntimeStore store = new RuntimeStore(redis);
        for (int attempt = 0; attempt < 10; attempt++) {
            RuntimeStore.InstrumentSnapshot snapshot = store.get(instrumentId);
            if (snapshot != null && snapshot.reference != null && snapshot.reference.price.isPositive())
                return snapshot.reference.price;
            try { Thread.sleep(1000); } catch (InterruptedException e) { Thread.currentThread().interrupt(); break; }
        }
        return DecimalValue.ZERO;
    }

    /** How many of the just-placed orders are actually open on the exchange (polls
     *  briefly to allow for the venue's order-registration latency). */
    private static long restingCount(VenueClient client, String symbol, List<Domain.Order> placed) {
        java.util.Set<String> ids = new java.util.HashSet<>();
        for (Domain.Order order : placed) if (order.orderId != null && !order.orderId.isEmpty()) ids.add(order.orderId);
        long count = 0;
        for (int attempt = 0; attempt < 5; attempt++) {
            count = client.openOrders(symbol).stream().filter(order -> ids.contains(order.orderId)).count();
            if (count >= ids.size()) return count;
            try { Thread.sleep(500); } catch (InterruptedException e) { Thread.currentThread().interrupt(); break; }
        }
        return count;
    }

    /** Cancel exactly what we placed, plus a safety sweep for our test-prefixed orders. */
    private static void cleanup(VenueClient client, String symbol, List<Domain.Order> placed) {
        List<String> ids = new ArrayList<>();
        for (Domain.Order order : placed) if (order.orderId != null && !order.orderId.isEmpty()) ids.add(order.orderId);
        try {
            for (Domain.Order open : client.openOrders(symbol))
                if (open.clientId != null && open.clientId.startsWith(CLIENT_ID_PREFIX) && !ids.contains(open.orderId)) ids.add(open.orderId);
        } catch (RuntimeException ignored) { /* best effort */ }
        if (ids.isEmpty()) return;
        try { client.cancelOrders(symbol, ids); }
        catch (RuntimeException e) { System.err.println("cleanup: failed to cancel test orders " + ids + " -> " + e.getMessage()); }
    }

    private static boolean set(String key) { String value = System.getenv(key); return value != null && !value.isBlank(); }
}
