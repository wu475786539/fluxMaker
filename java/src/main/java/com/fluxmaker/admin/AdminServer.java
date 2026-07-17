package com.fluxmaker.admin;

import com.fasterxml.jackson.databind.JsonNode;
import com.fasterxml.jackson.databind.DeserializationFeature;
import com.fasterxml.jackson.databind.node.ArrayNode;
import com.fasterxml.jackson.databind.node.ObjectNode;
import com.fluxmaker.auth.AuthService;
import com.fluxmaker.auth.PasswordHasher;
import com.fluxmaker.config.AppConfig;
import com.fluxmaker.config.ConfigDiff;
import com.fluxmaker.config.ConfigStore;
import com.fluxmaker.credentials.CredentialService;
import com.fluxmaker.domain.Domain;
import com.fluxmaker.infra.Database;
import com.fluxmaker.infra.RedisClient;
import com.fluxmaker.json.Json;
import com.fluxmaker.oracle.RpcClient;
import com.fluxmaker.runtime.RuntimeStore;
import com.sun.net.httpserver.HttpExchange;
import com.sun.net.httpserver.HttpServer;

import java.io.IOException;
import java.net.InetSocketAddress;
import java.net.URI;
import java.nio.charset.StandardCharsets;
import java.nio.file.Files;
import java.nio.file.Path;
import java.security.MessageDigest;
import java.sql.Connection;
import java.sql.PreparedStatement;
import java.sql.ResultSet;
import java.sql.SQLException;
import java.sql.Timestamp;
import java.time.Instant;
import java.util.ArrayList;
import java.util.Arrays;
import java.util.Comparator;
import java.util.LinkedHashMap;
import java.util.LinkedHashSet;
import java.util.List;
import java.util.Locale;
import java.util.Map;
import java.util.Set;
import java.util.concurrent.ArrayBlockingQueue;
import java.util.concurrent.ExecutorService;
import java.util.concurrent.ThreadPoolExecutor;
import java.util.concurrent.TimeUnit;

public final class AdminServer implements AutoCloseable {
    private static final long MAX_BODY = 2L << 20;
    private static final int HTTP_CORE_THREADS = 8;
    private static final int HTTP_MAX_THREADS = 32;
    private static final int HTTP_QUEUE_CAPACITY = 64;
    private final Database database;
    private final RedisClient redis;
    private final AuthService auth;
    private final ConfigStore configs;
    private final CredentialService credentials;
    private final RuntimeStore runtime;
    private final String metricsToken;
    private final Path webRoot;
    private final HttpServer server;
    private final ExecutorService executor;

    public AdminServer(String address, Database database, RedisClient redis, AuthService auth, ConfigStore configs,
                       CredentialService credentials, RuntimeStore runtime, String metricsToken, Path webRoot) {
        this.database = database; this.redis = redis; this.auth = auth; this.configs = configs; this.credentials = credentials; this.runtime = runtime;
        this.metricsToken = metricsToken == null ? "" : metricsToken.trim(); this.webRoot = webRoot;
        ExecutorService createdExecutor = newHttpExecutor();
        try {
            HostPort bind = HostPort.parse(address);
            server = HttpServer.create(new InetSocketAddress(bind.host, bind.port), 0);
            server.createContext("/", this::handle);
            server.setExecutor(createdExecutor);
            executor = createdExecutor;
        } catch (IOException e) {
            createdExecutor.shutdownNow();
            throw new IllegalStateException("create admin server", e);
        }
    }

    public void start() { server.start(); }
    public int port() { return server.getAddress().getPort(); }
    @Override public void close() {
        server.stop(2);
        executor.shutdownNow();
        try { executor.awaitTermination(2, TimeUnit.SECONDS); }
        catch (InterruptedException e) { Thread.currentThread().interrupt(); }
    }

    static ExecutorService newHttpExecutor() {
        ThreadPoolExecutor executor = new ThreadPoolExecutor(
                HTTP_CORE_THREADS,
                HTTP_MAX_THREADS,
                60,
                TimeUnit.SECONDS,
                new ArrayBlockingQueue<>(HTTP_QUEUE_CAPACITY),
                Thread.ofPlatform().name("admin-http-", 0).factory(),
                new ThreadPoolExecutor.AbortPolicy());
        executor.allowCoreThreadTimeOut(true);
        return executor;
    }

    private void handle(HttpExchange exchange) {
        long started = System.nanoTime(); int status = 500;
        exchange.getResponseHeaders().set("X-Content-Type-Options", "nosniff");
        exchange.getResponseHeaders().set("X-Frame-Options", "DENY");
        exchange.getResponseHeaders().set("Cache-Control", "no-store");
        exchange.getResponseHeaders().set("X-Request-ID", "req-" + System.currentTimeMillis() + "-" + Math.abs(System.nanoTime()));
        try { status = dispatch(exchange); }
        catch (ApiError e) { status = errorResponse(exchange, e.status, e.getMessage(), e, false); }
        catch (AuthService.InvalidCredentials e) { status = errorResponse(exchange, 401, "authentication required", e, false); }
        catch (IllegalArgumentException e) { status = errorResponse(exchange, 400, e.getMessage(), e, false); }
        catch (RuntimeException e) { status = errorResponse(exchange, 500, "internal server error", e, true); }
        finally { exchange.close(); long elapsed = (System.nanoTime() - started) / 1_000_000; if ((status >= 400 && status != 499) || elapsed > 1000) System.out.println(exchange.getRequestMethod() + " " + exchange.getRequestURI().getPath() + " status=" + status + " duration_ms=" + elapsed); }
    }

    private int errorResponse(HttpExchange exchange, int status, String message, RuntimeException error, boolean report) {
        if (clientDisconnected(error)) return 499;
        if (report) error.printStackTrace(System.err);
        try { return sendError(exchange, status, message); }
        catch (RuntimeException writeError) {
            if (!clientDisconnected(writeError)) writeError.printStackTrace(System.err);
            return clientDisconnected(writeError) ? 499 : status;
        }
    }

    static boolean clientDisconnected(Throwable error) {
        for (Throwable current = error; current != null; current = current.getCause()) {
            if (!(current instanceof IOException) || current.getMessage() == null) continue;
            String message = current.getMessage().toLowerCase(Locale.ROOT);
            if (message.contains("broken pipe")
                    || message.contains("connection reset")
                    || message.contains("connection aborted")
                    || message.contains("socket closed")
                    || message.contains("stream is closed")
                    || message.contains("insufficient bytes written")) return true;
        }
        return false;
    }

    private int dispatch(HttpExchange exchange) {
        String method = exchange.getRequestMethod(), path = exchange.getRequestURI().getPath();
        if (method.equals("GET") && path.equals("/livez")) return send(exchange, 200, Map.of("status", "ok"));
        if (method.equals("GET") && (path.equals("/healthz") || path.equals("/readyz"))) { try { database.ping(); } catch (RuntimeException e) { throw new ApiError(503, "postgres unavailable"); } try { redis.ping(); } catch (RuntimeException e) { throw new ApiError(503, "redis unavailable"); } return send(exchange, 200, Map.of("status", "ok")); }
        if (method.equals("GET") && path.equals("/metrics")) return metrics(exchange);
        if (method.equals("POST") && path.equals("/api/login")) return login(exchange);
        if (method.equals("GET") && Set.of("/", "/app.js", "/styles.css").contains(path)) return staticFile(exchange, path);

        AuthService.Session session = require(exchange, permission(method, path));
        if (method.equals("POST") && path.equals("/api/logout")) { auth.logout(token(exchange)); clearCookie(exchange); return empty(exchange, 204); }
        if (method.equals("GET") && path.equals("/api/me")) return send(exchange, 200, session);
        if (method.equals("PUT") && path.equals("/api/me/password")) return changePassword(exchange, session);
        if (path.equals("/api/config/draft") && method.equals("GET")) return getDraft(exchange, session);
        if (path.equals("/api/config/draft") && method.equals("PUT")) return putDraft(exchange, session);
        if (path.equals("/api/config/publish") && method.equals("POST")) return active(exchange, session, true);
        if (path.equals("/api/config/active") && method.equals("GET")) return active(exchange, session, false);
        if (path.equals("/api/config/plan") && method.equals("POST")) return configPlan(exchange, session);
        if (path.equals("/api/runtime") && method.equals("GET")) return runtimeList(exchange, session);
        if (path.equals("/api/monitoring") && method.equals("GET")) return monitoring(exchange, session);
        if (path.startsWith("/api/runtime/")) return runtimeInstrumentRoute(exchange, session, method, path);
        if (path.equals("/api/oracle/pancake-v2/inspect-pair") && method.equals("POST")) return inspectPair(exchange, session);
        if (path.equals("/api/credential-options") && method.equals("GET")) return credentialOptions(exchange);
        if (path.equals("/api/venue-types") && method.equals("GET")) return send(exchange, 200, AppConfig.ADAPTER_SPECS);
        if (path.equals("/api/credentials") && method.equals("GET")) return send(exchange, 200, credentials.list());
        if (path.equals("/api/credentials") && method.equals("POST")) return createCredential(exchange, session);
        if (path.startsWith("/api/credentials/") && method.equals("PUT")) return updateCredential(exchange, session, id(path));
        if (path.equals("/api/users") && method.equals("GET")) return listUsers(exchange);
        if (path.equals("/api/users") && method.equals("POST")) return createUser(exchange, session);
        if (path.equals("/api/user-role-options") && method.equals("GET")) return simpleQuery(exchange, "SELECT code,name FROM roles ORDER BY id", "code", "name");
        if (path.equals("/api/permissions") && method.equals("GET")) return simpleQuery(exchange, "SELECT code,name FROM permissions ORDER BY code", "code", "name");
        if (path.equals("/api/roles") && method.equals("GET")) return listRoles(exchange);
        if (path.startsWith("/api/roles/") && method.equals("PUT")) return updateRole(exchange, session, id(path));
        if (path.startsWith("/api/users/")) return userRoute(exchange, session, method, path);
        throw new ApiError(404, "not found");
    }

    private static String permission(String method, String path) {
        if (path.startsWith("/api/config/")) return path.endsWith("/draft") && method.equals("PUT") ? "config:edit" : path.endsWith("/active") || (path.endsWith("/draft") && method.equals("GET")) ? "config:view" : "config:publish";
        if (path.startsWith("/api/runtime") || path.equals("/api/monitoring")) {
            if (path.endsWith("/pause")) return "runtime:stop"; if (path.endsWith("/emergency-cancel")) return "runtime:emergency_cancel"; if (path.endsWith("/resume") || path.endsWith("/reconcile") || path.endsWith("/rebuild-book")) return "runtime:start"; return "runtime:view";
        }
        if (path.startsWith("/api/oracle/")) return "config:edit";
        if (path.equals("/api/credential-options") || path.equals("/api/venue-types")) return "venue:view";
        if (path.startsWith("/api/credentials")) return "secrets:manage";
        if (path.startsWith("/api/users") || path.equals("/api/user-role-options")) return "users:manage";
        if (path.startsWith("/api/roles") || path.equals("/api/permissions")) return "roles:manage";
        return "";
    }

    private AuthService.Session require(HttpExchange exchange, String permission) {
        if (!Set.of("GET", "HEAD").contains(exchange.getRequestMethod())) {
            String origin = exchange.getRequestHeaders().getFirst("Origin");
            if (origin != null && !origin.isEmpty()) {
                URI parsed; try { parsed = URI.create(origin); } catch (RuntimeException e) { throw new ApiError(403, "cross-origin request blocked"); }
                String host = exchange.getRequestHeaders().getFirst("Host");
                if (host == null || !parsed.getAuthority().equalsIgnoreCase(host)) throw new ApiError(403, "cross-origin request blocked");
            }
        }
        AuthService.Session session = auth.authenticate(token(exchange));
        if (permission != null && !permission.isEmpty() && !session.has(permission)) throw new ApiError(403, "permission denied");
        return session;
    }

    private int login(HttpExchange exchange) {
        JsonNode body = body(exchange); AuthService.Login login;
        try { login = auth.login(text(body, "email"), text(body, "password")); }
        catch (AuthService.InvalidCredentials e) { throw new ApiError(401, "invalid credentials"); }
        boolean secure = "https".equalsIgnoreCase(exchange.getRequestHeaders().getFirst("X-Forwarded-Proto"));
        String cookie = "fluxmaker_session=" + login.token() + "; Path=/; HttpOnly; SameSite=Strict; Expires=" + java.time.format.DateTimeFormatter.RFC_1123_DATE_TIME.format(login.session().expiresAt.atZone(java.time.ZoneOffset.UTC)) + (secure ? "; Secure" : "");
        exchange.getResponseHeaders().add("Set-Cookie", cookie);
        return send(exchange, 200, Map.of("expires_at", login.session().expiresAt, "user", login.session()));
    }

    private int changePassword(HttpExchange exchange, AuthService.Session session) {
        JsonNode body = body(exchange);
        try { auth.changePassword(session.userId, text(body,"current_password"), text(body,"new_password")); }
        catch (AuthService.InvalidCredentials e) { throw new ApiError(400, "current password is incorrect"); }
        audit(session.userId, "user.password.change", "user", Long.toString(session.userId), Map.of());
        return empty(exchange, 204);
    }

    private int getDraft(HttpExchange exchange, AuthService.Session session) {
        try { return send(exchange, 200, scope(configs.getDraft(), session)); }
        catch (ConfigStore.NotFound e) { throw new ApiError(404, "no draft configuration"); }
    }

    private int putDraft(HttpExchange exchange, AuthService.Session session) {
        AppConfig incoming = read(exchange, AppConfig.class); enforceScope(incoming, session);
        if (!session.allInstruments) {
            AppConfig existing; try { existing = configs.getDraft(); } catch (ConfigStore.NotFound e) { throw new ApiError(400, "a global draft must exist before scoped editing"); }
            incoming = mergeScoped(existing, incoming, session);
        }
        validateCredentialBindings(incoming); configs.saveActive(incoming, session.userId);
        return send(exchange, 200, Map.of("status", "saved"));
    }

    private int active(HttpExchange exchange, AuthService.Session session, boolean publishCompatibility) {
        try { ConfigStore.Snapshot snapshot = configs.loadActive(); snapshot.config = scope(snapshot.config, session); return send(exchange, 200, snapshot); }
        catch (ConfigStore.NotFound e) { throw new ApiError(publishCompatibility ? 400 : 404, publishCompatibility ? "no configuration has been saved yet" : "no published configuration"); }
    }

    private int configPlan(HttpExchange exchange, AuthService.Session session) {
        AppConfig draft; try { draft = configs.getDraft(); } catch (ConfigStore.NotFound e) { throw new ApiError(400, "draft configuration not available"); }
        enforceScope(draft, session); draft.validate();
        try { ConfigStore.Snapshot active = configs.loadActive(); return send(exchange, 200, Map.of("from_version", active.version, "to_version", active.version + 1, "plan", ConfigDiff.build(active.config, draft))); }
        catch (ConfigStore.NotFound e) { return send(exchange, 200, Map.of("from_version", 0, "to_version", 1, "plan", ConfigDiff.build(null, draft))); }
    }

    private int runtimeList(HttpExchange exchange, AuthService.Session session) {
        RuntimeData data = runtimeData(session); Monitoring.Summary monitoring = Monitoring.build(data.config, data.engine, data.snapshots, Instant.now());
        return send(exchange, 200, Map.of("engine", data.engine, "instruments", data.snapshots, "monitoring", monitoring));
    }
    private int monitoring(HttpExchange exchange, AuthService.Session session) { RuntimeData data = runtimeData(session); return send(exchange, 200, Monitoring.build(data.config, data.engine, data.snapshots, Instant.now())); }

    private RuntimeData runtimeData(AuthService.Session session) {
        RuntimeStore.EngineStatus engine = runtime.engineStatus();
        if (!session.allInstruments) engine.ruleChanges.removeIf(change -> !session.canAccessInstrument(change.instrumentId));
        AppConfig config; try { config = runtimeConfig(); } catch (RuntimeException e) { return new RuntimeData(new AppConfig(), engine, new ArrayList<>()); }
        Map<String, RuntimeStore.PauseState> pauses = runtime.paused(); List<RuntimeStore.InstrumentSnapshot> snapshots = new ArrayList<>();
        for (AppConfig.InstrumentConfig instrument : config.instruments) if (session.canAccessInstrument(instrument.id)) {
            RuntimeStore.InstrumentSnapshot snapshot = runtime.get(instrument.id); if (snapshot == null) snapshot = waiting(config, instrument);
            mergePause(snapshot, pauses.get(instrument.id)); mergeBookRebuild(snapshot, runtime.bookRebuildStatus(instrument.id)); redact(snapshot, session); snapshots.add(snapshot);
        }
        return new RuntimeData(config, engine, snapshots);
    }

    private int runtimeInstrumentRoute(HttpExchange exchange, AuthService.Session session, String method, String path) {
        String relative = path.substring("/api/runtime/".length()); String[] parts = relative.split("/"); String instrument = parts[0];
        if (!session.canAccessInstrument(instrument)) throw new ApiError(403, "instrument is outside your data scope");
        AppConfig config = runtimeConfig(); AppConfig.InstrumentConfig configured = config.instruments.stream().filter(item -> item.id.equals(instrument)).findFirst().orElseThrow(() -> new ApiError(404, "instrument is not in the runtime configuration"));
        if (parts.length == 1 && method.equals("GET")) { RuntimeStore.InstrumentSnapshot value = runtime.get(instrument); if (value == null) value = waiting(config, configured); mergePause(value, runtime.paused().get(instrument)); mergeBookRebuild(value, runtime.bookRebuildStatus(instrument)); redact(value, session); return send(exchange, 200, value); }
        if (parts.length != 2 || !method.equals("POST")) throw new ApiError(404, "not found");
        return switch (parts[1]) {
            case "pause" -> { RuntimeStore.PauseState state = runtime.setPaused(instrument, RuntimeStore.REASON_MANUAL_PAUSE, session.userId); audit(session.userId,"runtime.pause","instrument",instrument,Map.of("reason",state.reason)); yield send(exchange,202,state); }
            case "emergency-cancel" -> { RuntimeStore.PauseState state = runtime.setPaused(instrument, RuntimeStore.REASON_EMERGENCY_CANCEL, session.userId); audit(session.userId,"runtime.emergency_cancel","instrument",instrument,Map.of("reason",state.reason)); yield send(exchange,202,state); }
            case "resume" -> { runtime.resume(instrument); audit(session.userId,"runtime.resume","instrument",instrument,Map.of()); yield send(exchange,202,Map.of("instrument_id",instrument,"paused",false)); }
            case "reconcile" -> { RuntimeStore.ReconcileRequest request = runtime.requestReconcile(instrument, session.userId); audit(session.userId,"runtime.reconcile","instrument",instrument,Map.of()); yield send(exchange,202,request); }
            case "rebuild-book" -> { RuntimeStore.BookRebuildRequest request = runtime.requestBookRebuild(instrument, session.userId); audit(session.userId,"runtime.book_rebuild","instrument",instrument,Map.of()); yield send(exchange,202,request); }
            default -> throw new ApiError(404,"not found");
        };
    }

    private int inspectPair(HttpExchange exchange, AuthService.Session session) {
        if (!session.has("instrument:edit")) throw new ApiError(403, "instrument:edit permission is required");
        JsonNode body = body(exchange); String pair=text(body,"pair_address"), input=text(body,"input_token"), expected=text(body,"expected_factory");
        if (!address(pair) || !address(input)) throw new ApiError(400,"valid pair_address and input_token are required"); if(!expected.isEmpty()&&!address(expected)) throw new ApiError(400,"expected_factory is invalid");
        AppConfig config; try { config=configs.getDraft(); } catch(RuntimeException e){throw new ApiError(400,"configure and save a BNB Chain RPC first");}
        RpcClient rpc = new RpcClient(config.rpc.urls, config.requestTimeout()); long chain=rpc.chainId(); if(chain!=config.rpc.chainId) throw new ApiError(400,"RPC chain id "+chain+" does not match configured "+config.rpc.chainId);
        RpcClient.PairInfo info; try { info=rpc.inspectPair(pair); } catch(RuntimeException e){throw new ApiError(502,"inspect Pair failed: "+e.getMessage());}
        if(!expected.isEmpty()&&!expected.equalsIgnoreCase(info.factory)) throw new ApiError(400,"Pair factory "+info.factory+" does not match expected "+expected);
        String base,quote; if(input.equalsIgnoreCase(info.token0)){base=info.token0;quote=info.token1;} else if(input.equalsIgnoreCase(info.token1)){base=info.token1;quote=info.token0;} else throw new ApiError(400,"Pair does not contain path input token "+input);
        Map<String,Object> response=new LinkedHashMap<>(); response.put("pair_address",info.pairAddress); response.put("token0",info.token0); response.put("token1",info.token1); response.put("factory",info.factory); response.put("base_token",base); response.put("quote_token",quote); return send(exchange,200,response);
    }

    private int credentialOptions(HttpExchange exchange) { List<Map<String,Object>> result=new ArrayList<>(); for(CredentialService.Metadata item:credentials.list()) result.add(Map.of("id",item.id,"name",item.name,"venue_type",item.venueType,"enabled",item.enabled)); return send(exchange,200,result); }
    private int createCredential(HttpExchange exchange,AuthService.Session session){JsonNode b=body(exchange); CredentialService.Metadata item=credentials.create(text(b,"name"),text(b,"venue_type"),text(b,"api_key"),text(b,"api_secret"),session.userId); audit(session.userId,"credential.create","venue_credential",Long.toString(item.id),Map.of("name",item.name,"venue_type",item.venueType)); return send(exchange,201,item);}
    private int updateCredential(HttpExchange exchange,AuthService.Session session,long id){JsonNode b=body(exchange); Boolean enabled=b.has("enabled")&&!b.get("enabled").isNull()?b.get("enabled").asBoolean():null; CredentialService.Metadata item=credentials.update(id,text(b,"name"),text(b,"api_key"),text(b,"api_secret"),enabled,session.userId); audit(session.userId,"credential.update","venue_credential",Long.toString(item.id),Map.of("name",item.name,"enabled",item.enabled)); return send(exchange,200,item);}

    private int metrics(HttpExchange exchange) {
        String provided=exchange.getRequestHeaders().getFirst("X-Metrics-Token"), authorization=exchange.getRequestHeaders().getFirst("Authorization"); if(authorization!=null&&authorization.startsWith("Bearer "))provided=authorization.substring(7).trim();
        if(metricsToken.isEmpty()||provided==null||!MessageDigest.isEqual(metricsToken.getBytes(StandardCharsets.UTF_8),provided.getBytes(StandardCharsets.UTF_8))) throw new ApiError(404,"not found");
        RuntimeStore.EngineStatus engine=runtime.engineStatus(); AppConfig config; try{config=runtimeConfig();}catch(RuntimeException e){config=new AppConfig();} List<RuntimeStore.InstrumentSnapshot> snapshots=new ArrayList<>(); for(AppConfig.InstrumentConfig instrument:config.instruments){RuntimeStore.InstrumentSnapshot value=runtime.get(instrument.id); snapshots.add(value==null?waiting(config,instrument):value);} Monitoring.Summary summary=Monitoring.build(config,engine,snapshots,Instant.now()); return sendText(exchange,200,"text/plain; version=0.0.4; charset=utf-8",Monitoring.prometheus(engine,snapshots,summary,Instant.now()));
    }

    private int staticFile(HttpExchange exchange,String path){String name=path.equals("/")?"index.html":path.substring(1); Path file=webRoot.resolve(name).normalize(); if(!file.startsWith(webRoot)||!Files.isRegularFile(file))throw new ApiError(404,"not found"); try{String type=name.endsWith(".html")?"text/html; charset=utf-8":name.endsWith(".js")?"text/javascript; charset=utf-8":"text/css; charset=utf-8"; return sendBytes(exchange,200,type,Files.readAllBytes(file));}catch(IOException e){throw new ApiError(404,"not found");}}

    private AppConfig runtimeConfig(){try{return configs.loadActive().config;}catch(ConfigStore.NotFound e){return configs.getDraft();}}
    private void validateCredentialBindings(AppConfig config){if(config.mode!=Domain.Mode.live)return; config.venues.forEach((name,venue)->{if(venue.enabled&&venue.tradingEnabled)venue.markets.forEach((instrument,market)->{try{credentials.validateReference(market.credentialId,venue.type);}catch(RuntimeException e){throw new IllegalArgumentException("venue "+name+" market "+instrument+": "+e.getMessage());}});});}
    private static void enforceScope(AppConfig config,AuthService.Session session){if(session.allInstruments)return; for(AppConfig.InstrumentConfig instrument:config.instruments)if(!session.canAccessInstrument(instrument.id))throw new ApiError(403,"instrument "+instrument.id+" is outside your data scope");}
    private static AppConfig scope(AppConfig source,AuthService.Session session){AppConfig config=copy(source); if(session.allInstruments)return config; config.instruments.removeIf(item->!session.canAccessInstrument(item.id)); config.venues.values().forEach(venue->venue.markets.entrySet().removeIf(entry->!session.canAccessInstrument(entry.getKey()))); return config;}
    private static AppConfig mergeScoped(AppConfig existing,AppConfig incoming,AuthService.Session session){AppConfig result=copy(existing); Set<String> allowed=new LinkedHashSet<>(session.instruments); result.instruments.removeIf(item->allowed.contains(item.id)); incoming.instruments.stream().filter(item->allowed.contains(item.id)).forEach(result.instruments::add); result.venues.forEach((name,venue)->{venue.markets.entrySet().removeIf(entry->allowed.contains(entry.getKey())); AppConfig.VenueConfig in=incoming.venues.get(name); if(in!=null)in.markets.forEach((id,market)->{if(allowed.contains(id))venue.markets.put(id,market);});}); return result;}
    private static RuntimeStore.InstrumentSnapshot waiting(AppConfig config,AppConfig.InstrumentConfig instrument){RuntimeStore.InstrumentSnapshot value=new RuntimeStore.InstrumentSnapshot(); value.instrumentId=instrument.id;value.baseSymbol=instrument.base.symbol;value.quoteSymbol=instrument.quote.symbol;value.mode=config.mode;value.status="waiting";value.targetInventory=instrument.strategy.targetBase;value.maxBaseDeviation=instrument.strategy.maxBaseDeviation;return value;}
    private static void mergePause(RuntimeStore.InstrumentSnapshot snapshot,RuntimeStore.PauseState pause){if(pause!=null){snapshot.pause=pause;snapshot.status=snapshot.paused?"paused":"pausing";}else if(snapshot.paused){snapshot.pause=null;snapshot.status="resuming";}}
    private static void mergeBookRebuild(RuntimeStore.InstrumentSnapshot snapshot,RuntimeStore.BookRebuildStatus status){snapshot.bookRebuild=status;}
    private static void redact(RuntimeStore.InstrumentSnapshot snapshot,AuthService.Session session){for(RuntimeStore.VenueSnapshot venue:snapshot.venues){if(!session.has("orders:view"))venue.openOrders=new ArrayList<>();if(!session.has("fills:view"))venue.fills=new ArrayList<>();}if(!session.has("fills:view")&&snapshot.tradeSimulation instanceof ObjectNode object)object.set("fills",Json.MAPPER.createArrayNode());}
    private static AppConfig copy(AppConfig value){return Json.read(Json.writeBytes(value),AppConfig.class);}

    private String token(HttpExchange exchange){String authorization=exchange.getRequestHeaders().getFirst("Authorization");if(authorization!=null){String[]parts=authorization.trim().split("\\s+");if(parts.length==2&&parts[0].equalsIgnoreCase("Bearer"))return parts[1];}String cookie=exchange.getRequestHeaders().getFirst("Cookie");if(cookie!=null)for(String part:cookie.split(";")){String[]pair=part.trim().split("=",2);if(pair.length==2&&pair[0].equals("fluxmaker_session"))return pair[1];}return "";}
    private static void clearCookie(HttpExchange exchange){exchange.getResponseHeaders().add("Set-Cookie","fluxmaker_session=; Path=/; HttpOnly; SameSite=Strict; Max-Age=0");}
    private JsonNode body(HttpExchange exchange){return read(exchange,JsonNode.class);}
    private <T>T read(HttpExchange exchange,Class<T> type){try{byte[]data=exchange.getRequestBody().readNBytes((int)MAX_BODY+1);if(data.length>MAX_BODY)throw new ApiError(400,"request body too large");if(type==JsonNode.class)return type.cast(Json.MAPPER.readTree(data));return Json.MAPPER.readerFor(type).with(DeserializationFeature.FAIL_ON_UNKNOWN_PROPERTIES).readValue(data);}catch(IOException e){throw new IllegalArgumentException("invalid JSON: "+e.getMessage());}}
    private static String text(JsonNode node,String key){return node!=null&&node.hasNonNull(key)?node.get(key).asText():"";} private static boolean address(String value){return value!=null&&value.matches("(?i)^0x[0-9a-f]{40}$");}
    private int send(HttpExchange exchange,int status,Object value){return sendBytes(exchange,status,"application/json",Json.writeBytes(value));} private int sendError(HttpExchange exchange,int status,String message){return send(exchange,status,Map.of("error",message==null?"":message));} private int empty(HttpExchange exchange,int status){try{exchange.sendResponseHeaders(status,-1);return status;}catch(IOException e){return status;}} private int sendText(HttpExchange exchange,int status,String type,String value){return sendBytes(exchange,status,type,value.getBytes(StandardCharsets.UTF_8));}
    private int sendBytes(HttpExchange exchange,int status,String type,byte[]data){try{exchange.getResponseHeaders().set("Content-Type",type);exchange.sendResponseHeaders(status,data.length);exchange.getResponseBody().write(data);return status;}catch(IOException e){throw new IllegalStateException(e);}}
    private static long id(String path){try{return Long.parseLong(path.substring(path.lastIndexOf('/')+1));}catch(NumberFormatException e){throw new ApiError(400,"invalid id");}}
    private void audit(long user,String action,String resourceType,String resourceId,Object details){try(Connection c=database.connection();PreparedStatement s=c.prepareStatement("INSERT INTO audit_logs(user_id,action,resource_type,resource_id,details) VALUES(?,?,?,?,?::jsonb)")){if(user>0)s.setLong(1,user);else s.setNull(1,java.sql.Types.BIGINT);s.setString(2,action);s.setString(3,resourceType);s.setString(4,resourceId);s.setString(5,Json.write(details));s.executeUpdate();}catch(SQLException ignored){}}
    private record RuntimeData(AppConfig config,RuntimeStore.EngineStatus engine,List<RuntimeStore.InstrumentSnapshot> snapshots){}
    private static final class ApiError extends RuntimeException{final int status;ApiError(int status,String message){super(message);this.status=status;}}
    private record HostPort(String host,int port){static HostPort parse(String raw){String value=raw==null||raw.isBlank()?":8080":raw.trim();if(value.startsWith(":"))return new HostPort("0.0.0.0",Integer.parseInt(value.substring(1)));URI uri=URI.create("http://"+value);return new HostPort(uri.getHost(),uri.getPort());}}

    // User and role handlers are kept below so the HTTP contract remains identical for the existing UI.
    private int simpleQuery(HttpExchange exchange,String sql,String first,String second){List<Map<String,Object>> values=new ArrayList<>();try(Connection c=database.connection();PreparedStatement s=c.prepareStatement(sql);ResultSet r=s.executeQuery()){while(r.next())values.add(Map.of(first,r.getObject(1),second,r.getObject(2)));}catch(SQLException e){throw new IllegalStateException(e);}return send(exchange,200,values);}

    private int listUsers(HttpExchange exchange) {
        List<Map<String,Object>> users=new ArrayList<>();
        try(Connection c=database.connection();PreparedStatement s=c.prepareStatement("SELECT id,email,enabled,created_at,updated_at,last_login_at,password_changed_at FROM users ORDER BY email");ResultSet r=s.executeQuery()){
            while(r.next()){
                long userId=r.getLong(1); Map<String,Object> user=new LinkedHashMap<>(); user.put("id",userId);user.put("email",r.getString(2));user.put("enabled",r.getBoolean(3));user.put("roles",strings(c,"SELECT role.code FROM user_roles link JOIN roles role ON role.id=link.role_id WHERE link.user_id=? ORDER BY role.code",userId));user.put("created_at",instant(r,4));user.put("updated_at",instant(r,5));Instant login=instant(r,6);if(login!=null)user.put("last_login_at",login);user.put("password_changed_at",instant(r,7));users.add(user);
            }
        }catch(SQLException e){throw new IllegalStateException("list users failed",e);}return send(exchange,200,users);
    }

    private int createUser(HttpExchange exchange,AuthService.Session actor){JsonNode body=body(exchange);String email=email(text(body,"email"));String password=text(body,"password");List<String>roles=stringArray(body.get("roles"));String hash=PasswordHasher.hash(password);long userId;
        try(Connection c=database.connection()){c.setAutoCommit(false);try{if(roles.contains("super_admin")&&!isSuperAdmin(c,actor.userId))throw new ApiError(403,"only a super administrator can assign super_admin");List<Long>roleIds=roleIds(c,roles);try(PreparedStatement s=c.prepareStatement("INSERT INTO users(email,password_hash) VALUES(?,?) RETURNING id")){s.setString(1,email);s.setString(2,hash);try(ResultSet r=s.executeQuery()){r.next();userId=r.getLong(1);}}for(long roleId:roleIds)try(PreparedStatement s=c.prepareStatement("INSERT INTO user_roles(user_id,role_id) VALUES(?,?)")){s.setLong(1,userId);s.setLong(2,roleId);s.executeUpdate();}c.commit();}catch(RuntimeException|SQLException e){c.rollback();throw e;}}
        catch(ApiError e){throw e;}catch(SQLException e){throw new ApiError(400,"email already exists or is invalid");}audit(actor.userId,"user.create","user",Long.toString(userId),Map.of("email",email,"roles",roles));return send(exchange,201,Map.of("id",userId,"email",email,"enabled",true,"roles",roles));}

    private int userRoute(HttpExchange exchange,AuthService.Session actor,String method,String path){String relative=path.substring("/api/users/".length());String[]parts=relative.split("/");long userId;try{userId=Long.parseLong(parts[0]);}catch(NumberFormatException e){throw new ApiError(400,"invalid user id");}if(parts.length==1&&method.equals("PUT"))return updateUser(exchange,actor,userId);if(parts.length==2&&method.equals("PUT")&&parts[1].equals("password"))return resetPassword(exchange,actor,userId);if(parts.length==2&&method.equals("POST")&&parts[1].equals("revoke-sessions"))return revokeSessions(exchange,actor,userId);throw new ApiError(404,"not found");}

    private int updateUser(HttpExchange exchange,AuthService.Session actor,long userId){JsonNode body=body(exchange);String email=email(text(body,"email"));List<String>roles=stringArray(body.get("roles"));Boolean enabled=body.has("enabled")&&!body.get("enabled").isNull()?body.get("enabled").asBoolean():null;
        try(Connection c=database.connection()){c.setAutoCommit(false);try{String currentEmail;boolean currentEnabled;try(PreparedStatement s=c.prepareStatement("SELECT email,enabled FROM users WHERE id=? FOR UPDATE")){s.setLong(1,userId);try(ResultSet r=s.executeQuery()){if(!r.next())throw new ApiError(404,"user not found");currentEmail=r.getString(1);currentEnabled=r.getBoolean(2);}}boolean actorSuper=isSuperAdmin(c,actor.userId),targetSuper=isSuperAdmin(c,userId),desiredSuper=roles.contains("super_admin");if((targetSuper||desiredSuper)&&!actorSuper)throw new ApiError(403,"only a super administrator can manage super_admin accounts");boolean nextEnabled=enabled==null?currentEnabled:enabled;if(userId==actor.userId&&!nextEnabled)throw new ApiError(400,"you cannot disable your current account");if(userId==actor.userId&&targetSuper&&!desiredSuper)throw new ApiError(400,"you cannot remove your own super_admin role");if(currentEnabled&&targetSuper&&(!nextEnabled||!desiredSuper)&&enabledSuperAdminsExcept(c,userId)==0)throw new ApiError(400,"at least one enabled super administrator is required");List<Long>roleIds=roleIds(c,roles);try(PreparedStatement s=c.prepareStatement("UPDATE users SET email=?,enabled=?,authorization_version=authorization_version+1,updated_at=now() WHERE id=?")){s.setString(1,email);s.setBoolean(2,nextEnabled);s.setLong(3,userId);s.executeUpdate();}try(PreparedStatement s=c.prepareStatement("DELETE FROM user_roles WHERE user_id=?")){s.setLong(1,userId);s.executeUpdate();}for(long roleId:roleIds)try(PreparedStatement s=c.prepareStatement("INSERT INTO user_roles(user_id,role_id) VALUES(?,?)")){s.setLong(1,userId);s.setLong(2,roleId);s.executeUpdate();}c.commit();audit(actor.userId,"user.update","user",Long.toString(userId),Map.of("previous_email",currentEmail,"email",email,"enabled",nextEnabled,"roles",roles));}catch(RuntimeException|SQLException e){c.rollback();throw e;}}
        catch(ApiError e){throw e;}catch(SQLException e){throw new ApiError(400,"email already exists or is invalid");}return empty(exchange,204);}

    private int resetPassword(HttpExchange exchange,AuthService.Session actor,long userId){String hash=PasswordHasher.hash(text(body(exchange),"password"));String targetEmail;
        try(Connection c=database.connection()){c.setAutoCommit(false);try{try(PreparedStatement s=c.prepareStatement("SELECT email FROM users WHERE id=? FOR UPDATE")){s.setLong(1,userId);try(ResultSet r=s.executeQuery()){if(!r.next())throw new ApiError(404,"user not found");targetEmail=r.getString(1);}}if(isSuperAdmin(c,userId)&&!isSuperAdmin(c,actor.userId))throw new ApiError(403,"only a super administrator can reset this password");try(PreparedStatement s=c.prepareStatement("UPDATE users SET password_hash=?,authorization_version=authorization_version+1,password_changed_at=now(),updated_at=now() WHERE id=?")){s.setString(1,hash);s.setLong(2,userId);s.executeUpdate();}c.commit();}catch(RuntimeException|SQLException e){c.rollback();throw e;}}
        catch(ApiError e){throw e;}catch(SQLException e){throw new IllegalStateException("reset password failed",e);}audit(actor.userId,"user.password.reset","user",Long.toString(userId),Map.of("email",targetEmail));return empty(exchange,204);}

    private int revokeSessions(HttpExchange exchange,AuthService.Session actor,long userId){String targetEmail;
        try(Connection c=database.connection()){c.setAutoCommit(false);try{try(PreparedStatement s=c.prepareStatement("SELECT email FROM users WHERE id=? FOR UPDATE")){s.setLong(1,userId);try(ResultSet r=s.executeQuery()){if(!r.next())throw new ApiError(404,"user not found");targetEmail=r.getString(1);}}if(isSuperAdmin(c,userId)&&!isSuperAdmin(c,actor.userId))throw new ApiError(403,"only a super administrator can revoke these sessions");try(PreparedStatement s=c.prepareStatement("UPDATE users SET authorization_version=authorization_version+1,updated_at=now() WHERE id=?")){s.setLong(1,userId);s.executeUpdate();}c.commit();}catch(RuntimeException|SQLException e){c.rollback();throw e;}}
        catch(ApiError e){throw e;}catch(SQLException e){throw new IllegalStateException("revoke sessions failed",e);}audit(actor.userId,"user.sessions.revoke","user",Long.toString(userId),Map.of("email",targetEmail));return empty(exchange,204);}

    private int listRoles(HttpExchange exchange){List<Map<String,Object>>roles=new ArrayList<>();try(Connection c=database.connection();PreparedStatement s=c.prepareStatement("SELECT id,code,name,all_instruments FROM roles ORDER BY id");ResultSet r=s.executeQuery()){while(r.next()){long roleId=r.getLong(1);Map<String,Object>role=new LinkedHashMap<>();role.put("id",roleId);role.put("code",r.getString(2));role.put("name",r.getString(3));role.put("all_instruments",r.getBoolean(4));role.put("permissions",strings(c,"SELECT permission_code FROM role_permissions WHERE role_id=? ORDER BY permission_code",roleId));role.put("instruments",strings(c,"SELECT instrument_id FROM role_instruments WHERE role_id=? ORDER BY instrument_id",roleId));roles.add(role);}}catch(SQLException e){throw new IllegalStateException("list roles failed",e);}return send(exchange,200,roles);}

    private int updateRole(HttpExchange exchange,AuthService.Session actor,long roleId){JsonNode body=body(exchange);String name=text(body,"name");boolean all=body.path("all_instruments").asBoolean();List<String>permissions=stringArrayAllowEmpty(body.get("permissions")),instruments=stringArrayPreserve(body.get("instruments"));
        try(Connection c=database.connection()){c.setAutoCommit(false);try{String code;try(PreparedStatement s=c.prepareStatement("SELECT code FROM roles WHERE id=? FOR UPDATE")){s.setLong(1,roleId);try(ResultSet r=s.executeQuery()){if(!r.next())throw new ApiError(404,"role not found");code=r.getString(1);}}if(code.equals("super_admin"))throw new ApiError(400,"super_admin cannot be modified");try(PreparedStatement s=c.prepareStatement("UPDATE roles SET name=?,all_instruments=? WHERE id=?")){s.setString(1,name);s.setBoolean(2,all);s.setLong(3,roleId);s.executeUpdate();}delete(c,"DELETE FROM role_permissions WHERE role_id=?",roleId);delete(c,"DELETE FROM role_instruments WHERE role_id=?",roleId);for(String permission:permissions)try(PreparedStatement s=c.prepareStatement("INSERT INTO role_permissions(role_id,permission_code) SELECT ?,code FROM permissions WHERE code=?")){s.setLong(1,roleId);s.setString(2,permission);if(s.executeUpdate()!=1)throw new ApiError(400,"unknown permission: "+permission);}if(!all)for(String instrument:instruments)try(PreparedStatement s=c.prepareStatement("INSERT INTO role_instruments(role_id,instrument_id) VALUES(?,?)")){s.setLong(1,roleId);s.setString(2,instrument);s.executeUpdate();}try(PreparedStatement s=c.prepareStatement("UPDATE users SET authorization_version=authorization_version+1,updated_at=now() WHERE id IN (SELECT user_id FROM user_roles WHERE role_id=?)")){s.setLong(1,roleId);s.executeUpdate();}c.commit();}catch(RuntimeException|SQLException e){c.rollback();throw e;}}
        catch(ApiError e){throw e;}catch(SQLException e){throw new IllegalStateException("update role failed",e);}audit(actor.userId,"role.update","role",Long.toString(roleId),Map.of("name",name));return empty(exchange,204);}

    private static List<Long> roleIds(Connection c,List<String>codes)throws SQLException{if(codes.isEmpty())throw new ApiError(400,"at least one role is required");List<Long>values=new ArrayList<>();for(String code:codes)try(PreparedStatement s=c.prepareStatement("SELECT id FROM roles WHERE code=?")){s.setString(1,code);try(ResultSet r=s.executeQuery()){if(!r.next())throw new ApiError(400,"unknown role: "+code);values.add(r.getLong(1));}}return values;}
    private static boolean isSuperAdmin(Connection c,long user)throws SQLException{try(PreparedStatement s=c.prepareStatement("SELECT EXISTS(SELECT 1 FROM user_roles link JOIN roles role ON role.id=link.role_id WHERE link.user_id=? AND role.code='super_admin')")){s.setLong(1,user);try(ResultSet r=s.executeQuery()){r.next();return r.getBoolean(1);}}}
    private static int enabledSuperAdminsExcept(Connection c,long user)throws SQLException{try(PreparedStatement s=c.prepareStatement("SELECT COUNT(DISTINCT account.id) FROM users account JOIN user_roles link ON link.user_id=account.id JOIN roles role ON role.id=link.role_id WHERE account.enabled=TRUE AND role.code='super_admin' AND account.id<>?")){s.setLong(1,user);try(ResultSet r=s.executeQuery()){r.next();return r.getInt(1);}}}
    private static void delete(Connection c,String sql,long id)throws SQLException{try(PreparedStatement s=c.prepareStatement(sql)){s.setLong(1,id);s.executeUpdate();}}
    private static List<String>strings(Connection c,String sql,long id)throws SQLException{List<String>values=new ArrayList<>();try(PreparedStatement s=c.prepareStatement(sql)){s.setLong(1,id);try(ResultSet r=s.executeQuery()){while(r.next())values.add(r.getString(1));}}return values;}
    private static Instant instant(ResultSet result,int column)throws SQLException{Timestamp value=result.getTimestamp(column);return value==null?null:value.toInstant();}
    private static String email(String raw){String value=raw==null?"":raw.trim().toLowerCase(Locale.ROOT);if(!value.matches("^[^@\\s]+@[^@\\s]+\\.[^@\\s]+$"))throw new ApiError(400,"a valid email address is required");return value;}
    private static List<String>stringArray(JsonNode node){List<String>values=stringArrayAllowEmpty(node);if(values.isEmpty())throw new ApiError(400,"at least one role is required");return values;}
    private static List<String>stringArrayAllowEmpty(JsonNode node){LinkedHashSet<String>values=new LinkedHashSet<>();if(node!=null&&node.isArray())for(JsonNode item:node){String value=item.asText().trim().toLowerCase(Locale.ROOT);if(!value.isEmpty())values.add(value);}return new ArrayList<>(values);}
    private static List<String>stringArrayPreserve(JsonNode node){LinkedHashSet<String>values=new LinkedHashSet<>();if(node!=null&&node.isArray())for(JsonNode item:node){String value=item.asText().trim();if(!value.isEmpty())values.add(value);}return new ArrayList<>(values);}
}
