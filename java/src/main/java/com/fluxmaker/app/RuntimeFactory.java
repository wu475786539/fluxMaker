package com.fluxmaker.app;

import com.fluxmaker.audit.AuditLogger;
import com.fluxmaker.config.AppConfig;
import com.fluxmaker.credentials.CredentialService;
import com.fluxmaker.domain.Domain;
import com.fluxmaker.engine.TradingEngine;
import com.fluxmaker.json.Json;
import com.fluxmaker.oracle.PancakeV2Oracle;
import com.fluxmaker.oracle.RpcClient;
import com.fluxmaker.runtime.RuntimeStore;
import com.fluxmaker.venue.BinanceClient;
import com.fluxmaker.venue.MgbxClient;
import com.fluxmaker.venue.VenueClient;

import java.time.Duration;
import java.util.ArrayList;
import java.util.LinkedHashMap;
import java.util.List;
import java.util.Locale;
import java.util.Map;

public final class RuntimeFactory {
    private RuntimeFactory() {}

    public record VenueBuild(Map<String, VenueClient> clients, Map<String, List<String>> failures) {}

    public static AppRuntime build(AppConfig rawConfig, CredentialService credentials, RuntimeStore runtimeStore, AppRuntime previous) {
        AppConfig config = copy(rawConfig);
        config.applyRuntimeSafetyDefaults();
        config.validate();
        if (config.mode == Domain.Mode.live && !"I_UNDERSTAND".equals(System.getenv("FLUXMAKER_ENABLE_LIVE_TRADING")))
            throw new IllegalStateException("live mode requires FLUXMAKER_ENABLE_LIVE_TRADING=I_UNDERSTAND");

        VenueBuild venues = buildVenuesIsolated(config, credentials);
        merge(venues.failures(), synchronizeRules(config, venues.clients(), previous == null ? null : previous.config));
        config.validate();

        PancakeV2Oracle oracle;
        if (previous != null && previous.oracle != null && Json.tree(previous.config.rpc).equals(Json.tree(config.rpc))) oracle = previous.oracle;
        else {
            RpcClient rpc = new RpcClient(config.rpc.urls, config.requestTimeout());
            long chainId = rpc.chainId();
            if (chainId != config.rpc.chainId) throw new IllegalStateException("rpc chain id " + chainId + " does not match configured " + config.rpc.chainId);
            oracle = new PancakeV2Oracle(rpc);
        }
        String owner = previous == null ? "" : previous.engine.ownerId();
        TradingEngine engine = new TradingEngine(config, oracle::price, venues.clients(), runtimeStore,
                new AuditLogger(config.auditPath, config.auditMaxBytes, config.auditBackups), owner);
        Map<String, String> flattened = new LinkedHashMap<>();
        venues.failures().forEach((instrument, failures) -> {
            failures.sort(String::compareTo);
            flattened.put(instrument, String.join("; ", failures));
        });
        engine.setStartupFailures(flattened);
        return new AppRuntime(config, engine, oracle);
    }

    public static Map<String, VenueClient> buildVenues(AppConfig config, CredentialService credentials) {
        VenueBuild build = buildVenuesIsolated(config, credentials);
        if (!build.failures().isEmpty()) {
            String instrument = build.failures().keySet().stream().sorted().findFirst().orElse("");
            throw new IllegalStateException("instrument " + instrument + ": " + String.join("; ", build.failures().get(instrument)));
        }
        return build.clients();
    }

    public static VenueBuild buildVenuesIsolated(AppConfig config, CredentialService credentials) {
        Map<String, VenueClient> clients = new LinkedHashMap<>();
        Map<String, List<String>> failures = new LinkedHashMap<>();
        Duration timeout = config.requestTimeout().isNegative() || config.requestTimeout().isZero() ? Duration.ofSeconds(5) : config.requestTimeout();
        config.venues.forEach((name, venue) -> {
            if (!venue.enabled) return;
            venue.markets.forEach((instrumentId, market) -> {
                String apiKey = "", secret = "";
                try {
                    if (market.credentialId > 0) {
                        if (credentials == null) throw new IllegalStateException("credential service unavailable");
                        CredentialService.Secret resolved = credentials.resolve(market.credentialId, venue.type);
                        apiKey = resolved.apiKey(); secret = resolved.apiSecret();
                    } else if (config.mode == Domain.Mode.live && venue.tradingEnabled) throw new IllegalStateException("credential is required for live trading");
                    String clientName = name + "/" + instrumentId;
                    String identity = name.toLowerCase(Locale.ROOT) + "/" + instrumentId.toLowerCase(Locale.ROOT) + "/credential-" + market.credentialId;
                    VenueClient client = switch (venue.type.toLowerCase(Locale.ROOT)) {
                        case "binance" -> new BinanceClient(clientName, identity, venue.baseUrl, apiKey, secret, venue.selfTradePrevention, timeout);
                        case "mgbx" -> new MgbxClient(clientName, identity, venue.baseUrl, apiKey, secret, timeout);
                        default -> throw new IllegalArgumentException("unsupported venue type " + venue.type);
                    };
                    clients.put(clientKey(name, instrumentId), client);
                } catch (RuntimeException e) {
                    failures.computeIfAbsent(instrumentId, ignored -> new ArrayList<>()).add("venue " + name + ": " + e.getMessage());
                }
            });
        });
        return new VenueBuild(clients, failures);
    }

    static Map<String, List<String>> synchronizeRules(AppConfig config, Map<String, VenueClient> clients, AppConfig previous) {
        Map<String, List<String>> failures = new LinkedHashMap<>();
        config.venues.forEach((venueName, venue) -> {
            if (!venue.enabled) return;
            venue.markets.forEach((instrumentId, market) -> {
                AppConfig.VenueMarketConfig reusable = reusableRules(previous, venueName, venue, instrumentId, market);
                if (reusable != null) { copyRules(reusable, market); return; }
                VenueClient client = clients.get(clientKey(venueName, instrumentId));
                if (client == null) { failure(failures, instrumentId, venueName + ": client unavailable"); return; }
                if (!client.capabilities().marketRules()) { failure(failures, instrumentId, venueName + ": trading rules unavailable"); return; }
                try {
                    Domain.MarketRules rules = client.marketRules(market.symbol);
                    if (rules.baseAsset != null && !rules.baseAsset.isEmpty() && !rules.baseAsset.equalsIgnoreCase(market.baseAsset)) throw new IllegalStateException("base asset " + market.baseAsset + " does not match exchange " + rules.baseAsset);
                    if (rules.quoteAsset != null && !rules.quoteAsset.isEmpty() && !rules.quoteAsset.equalsIgnoreCase(market.quoteAsset)) throw new IllegalStateException("quote asset " + market.quoteAsset + " does not match exchange " + rules.quoteAsset);
                    applyRules(rules, market);
                } catch (RuntimeException e) { failure(failures, instrumentId, venueName + ": load trading rules: " + e.getMessage()); }
            });
        });
        return failures;
    }

    private static AppConfig.VenueMarketConfig reusableRules(AppConfig previous, String venueName, AppConfig.VenueConfig venue, String instrument, AppConfig.VenueMarketConfig market) {
        if (previous == null) return null;
        AppConfig.VenueConfig oldVenue = previous.venues.get(venueName);
        if (oldVenue == null || !same(oldVenue.type, venue.type) || !same(oldVenue.environment, venue.environment) || !same(oldVenue.baseUrl, venue.baseUrl)) return null;
        AppConfig.VenueMarketConfig old = oldVenue.markets.get(instrument);
        return old != null && same(old.symbol, market.symbol) && old.priceTick.isPositive() ? old : null;
    }

    private static void applyRules(Domain.MarketRules source, AppConfig.VenueMarketConfig target) {
        if (source.priceTick.isPositive()) target.priceTick = source.priceTick;
        if (source.quantityStep.isPositive()) target.quantityStep = source.quantityStep;
        if (source.minNotional.isPositive()) target.minNotional = source.minNotional;
        target.minQuantity = source.minQuantity; target.maxQuantity = source.maxQuantity; target.maxNotional = source.maxNotional;
        target.minPrice = source.minPrice; target.maxPrice = source.maxPrice; target.maxOpenOrders = source.maxOpenOrders;
    }
    private static void copyRules(AppConfig.VenueMarketConfig source, AppConfig.VenueMarketConfig target) {
        target.priceTick = source.priceTick; target.quantityStep = source.quantityStep; target.minNotional = source.minNotional;
        target.minQuantity = source.minQuantity; target.maxQuantity = source.maxQuantity; target.maxNotional = source.maxNotional;
        target.minPrice = source.minPrice; target.maxPrice = source.maxPrice; target.maxOpenOrders = source.maxOpenOrders;
    }
    private static void merge(Map<String, List<String>> into, Map<String, List<String>> from) { from.forEach((key, values) -> into.computeIfAbsent(key, ignored -> new ArrayList<>()).addAll(values)); }
    private static void failure(Map<String, List<String>> values, String instrument, String message) { values.computeIfAbsent(instrument, ignored -> new ArrayList<>()).add(message); }
    private static AppConfig copy(AppConfig config) { return Json.read(Json.writeBytes(config), AppConfig.class); }
    private static boolean same(String left, String right) { return left == null ? right == null : left.equals(right); }
    public static String clientKey(String venue, String instrument) { return venue.trim().toLowerCase(Locale.ROOT) + "|" + instrument.trim().toLowerCase(Locale.ROOT); }
}
