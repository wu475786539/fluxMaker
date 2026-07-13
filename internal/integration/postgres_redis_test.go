//go:build integration

package integration_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"fluxmaker/internal/adminapi"
	"fluxmaker/internal/auth"
	"fluxmaker/internal/config"
	"fluxmaker/internal/configstore"
	"fluxmaker/internal/credentials"
	"fluxmaker/internal/database"
	"fluxmaker/internal/domain"
	"fluxmaker/internal/num"
	"fluxmaker/internal/runtimeops"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"
)

const activeConfigCacheKey = "fluxmaker:config:active"

type integrationEnvironment struct {
	postgres *pgxpool.Pool
	redis    *redis.Client
}

func setupIntegrationEnvironment(t *testing.T) integrationEnvironment {
	t.Helper()
	databaseURL := requiredEnvironment(t, "INTEGRATION_DATABASE_URL")
	redisAddress := requiredEnvironment(t, "INTEGRATION_REDIS_ADDR")
	redisPassword := os.Getenv("INTEGRATION_REDIS_PASSWORD")
	redisDB := 15
	if raw := os.Getenv("INTEGRATION_REDIS_DB"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil {
			t.Fatalf("INTEGRATION_REDIS_DB: %v", err)
		}
		redisDB = parsed
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	adminConfig, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	adminPool, err := pgxpool.NewWithConfig(ctx, adminConfig)
	if err != nil {
		t.Fatal(err)
	}
	if err := adminPool.Ping(ctx); err != nil {
		adminPool.Close()
		t.Fatalf("ping integration PostgreSQL: %v", err)
	}
	schema := fmt.Sprintf("fluxmaker_it_%d", time.Now().UnixNano())
	if _, err := adminPool.Exec(ctx, "CREATE SCHEMA "+pgx.Identifier{schema}.Sanitize()); err != nil {
		adminPool.Close()
		t.Fatalf("create integration schema: %v", err)
	}

	testConfig, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	if testConfig.ConnConfig.RuntimeParams == nil {
		testConfig.ConnConfig.RuntimeParams = make(map[string]string)
	}
	testConfig.ConnConfig.RuntimeParams["search_path"] = schema
	testPool, err := pgxpool.NewWithConfig(ctx, testConfig)
	if err != nil {
		t.Fatal(err)
	}
	if err := database.Migrate(ctx, testPool); err != nil {
		testPool.Close()
		_, _ = adminPool.Exec(ctx, "DROP SCHEMA "+pgx.Identifier{schema}.Sanitize()+" CASCADE")
		adminPool.Close()
		t.Fatalf("migrate integration schema: %v", err)
	}

	redisClient := redis.NewClient(&redis.Options{Addr: redisAddress, Password: redisPassword, DB: redisDB})
	if err := redisClient.Ping(ctx).Err(); err != nil {
		testPool.Close()
		_, _ = adminPool.Exec(ctx, "DROP SCHEMA "+pgx.Identifier{schema}.Sanitize()+" CASCADE")
		adminPool.Close()
		t.Fatalf("ping integration Redis: %v", err)
	}
	if err := redisClient.FlushDB(ctx).Err(); err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cleanupCancel()
		_ = redisClient.FlushDB(cleanupCtx).Err()
		_ = redisClient.Close()
		testPool.Close()
		_, _ = adminPool.Exec(cleanupCtx, "DROP SCHEMA "+pgx.Identifier{schema}.Sanitize()+" CASCADE")
		adminPool.Close()
	})
	return integrationEnvironment{postgres: testPool, redis: redisClient}
}

func requiredEnvironment(t *testing.T, key string) string {
	t.Helper()
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		t.Fatalf("%s is required for integration tests", key)
	}
	return value
}

func TestPublishedConfigRejectsStaleRedisSnapshot(t *testing.T) {
	environment := setupIntegrationEnvironment(t)
	ctx := context.Background()
	store := configstore.New(environment.postgres, environment.redis)

	firstConfig := validConfig(1000)
	if err := store.PutDraft(ctx, firstConfig, 0); err != nil {
		t.Fatal(err)
	}
	first, err := store.PublishDraft(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	secondConfig := validConfig(2500)
	if err := store.PutDraft(ctx, secondConfig, 0); err != nil {
		t.Fatal(err)
	}
	second, err := store.PublishDraft(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	stalePayload, err := json.Marshal(first)
	if err != nil {
		t.Fatal(err)
	}
	if err := environment.redis.Set(ctx, activeConfigCacheKey, stalePayload, time.Hour).Err(); err != nil {
		t.Fatal(err)
	}

	loaded, err := store.LoadActive(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Version != second.Version || loaded.Config.PollIntervalMS != 2500 {
		t.Fatalf("stale Redis snapshot won: loaded=%+v current=%+v", loaded, second)
	}
	cachedPayload, err := environment.redis.Get(ctx, activeConfigCacheKey).Bytes()
	if err != nil {
		t.Fatal(err)
	}
	var cached configstore.Snapshot
	if err := json.Unmarshal(cachedPayload, &cached); err != nil {
		t.Fatal(err)
	}
	if cached.Version != second.Version {
		t.Fatalf("cache was not repaired: got v%d want v%d", cached.Version, second.Version)
	}
}

func TestQuoteNotionalRangeRemovesLegacySizeFromDraftAndSnapshot(t *testing.T) {
	environment := setupIntegrationEnvironment(t)
	ctx := context.Background()
	store := configstore.New(environment.postgres, environment.redis)

	cfg := validConfig(1000)
	cfg.Instruments[0].Strategy.MinOrderNotional = num.Must("10")
	cfg.Instruments[0].Strategy.MaxOrderNotional = num.Must("20")
	if err := store.PutDraft(ctx, cfg, 0); err != nil {
		t.Fatal(err)
	}
	draft, err := store.GetDraft(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !draft.Instruments[0].Strategy.OrderSize.IsZero() {
		t.Fatalf("draft retained legacy order size %s", draft.Instruments[0].Strategy.OrderSize)
	}

	snapshot, err := store.PublishDraft(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	strategy := snapshot.Config.Instruments[0].Strategy
	if !strategy.OrderSize.IsZero() || strategy.MinOrderNotional.Cmp(num.Must("10")) != 0 || strategy.MaxOrderNotional.Cmp(num.Must("20")) != 0 {
		t.Fatalf("published strategy was not normalized: %+v", strategy)
	}
}

func TestRedisRuntimeStateSurvivesStoreReplacementAndFencesOwners(t *testing.T) {
	environment := setupIntegrationEnvironment(t)
	ctx := context.Background()
	first := runtimeops.New(environment.redis)

	if _, err := first.SetPaused(ctx, "token_usdt", "integration", 7); err != nil {
		t.Fatal(err)
	}
	if err := first.SaveOMSState(ctx, "market", []byte(`{"pending":"order-1"}`)); err != nil {
		t.Fatal(err)
	}
	if err := first.SaveFaultState(ctx, "market", []byte(`{"status":"canceling"}`)); err != nil {
		t.Fatal(err)
	}

	replacement := runtimeops.New(environment.redis)
	paused, err := replacement.Paused(ctx)
	if err != nil || paused["token_usdt"].Reason != "integration" {
		t.Fatalf("pause state was not inherited: paused=%+v err=%v", paused, err)
	}
	omsPayload, err := replacement.LoadOMSState(ctx, "market")
	if err != nil || !bytes.Contains(omsPayload, []byte("order-1")) {
		t.Fatalf("OMS state was not inherited: payload=%s err=%v", omsPayload, err)
	}
	faultPayload, err := replacement.LoadFaultState(ctx, "market")
	if err != nil || !bytes.Contains(faultPayload, []byte("canceling")) {
		t.Fatalf("fault state was not inherited: payload=%s err=%v", faultPayload, err)
	}
	if ttl, err := environment.redis.TTL(ctx, "fluxmaker:fault:state:market").Result(); err != nil || ttl != -1 {
		t.Fatalf("fault state must not expire: ttl=%s err=%v", ttl, err)
	}

	firstGeneration, err := replacement.AcquireMarketLease(ctx, "market", "owner-a", 10*time.Second)
	if err != nil || firstGeneration == 0 {
		t.Fatalf("first lease generation=%d err=%v", firstGeneration, err)
	}
	if generation, err := replacement.AcquireMarketLease(ctx, "market", "owner-b", 10*time.Second); err != nil || generation != 0 {
		t.Fatalf("second owner acquired active lease: generation=%d err=%v", generation, err)
	}
	if err := replacement.ReleaseMarketLease(ctx, "market", "owner-a", firstGeneration); err != nil {
		t.Fatal(err)
	}
	secondGeneration, err := replacement.AcquireMarketLease(ctx, "market", "owner-b", 10*time.Second)
	if err != nil || secondGeneration <= firstGeneration {
		t.Fatalf("fencing generation did not increase: first=%d second=%d err=%v", firstGeneration, secondGeneration, err)
	}
}

func TestRoleUpdateImmediatelyInvalidatesExistingSession(t *testing.T) {
	environment := setupIntegrationEnvironment(t)
	ctx := context.Background()
	authService := auth.NewService(environment.postgres, environment.redis)
	if err := authService.BootstrapAdmin(ctx, "admin@integration.local", "integration-admin-password"); err != nil {
		t.Fatal(err)
	}
	passwordHash, err := auth.HashPassword("integration-user-password")
	if err != nil {
		t.Fatal(err)
	}
	var userID, roleID int64
	if err := environment.postgres.QueryRow(ctx, `INSERT INTO users(email,password_hash) VALUES('operator@integration.local',$1) RETURNING id`, passwordHash).Scan(&userID); err != nil {
		t.Fatal(err)
	}
	if err := environment.postgres.QueryRow(ctx, `SELECT id FROM roles WHERE code='operator'`).Scan(&roleID); err != nil {
		t.Fatal(err)
	}
	if _, err := environment.postgres.Exec(ctx, `INSERT INTO user_roles(user_id,role_id) VALUES($1,$2)`, userID, roleID); err != nil {
		t.Fatal(err)
	}
	operatorToken, originalSession, err := authService.Login(ctx, "operator@integration.local", "integration-user-password")
	if err != nil {
		t.Fatal(err)
	}
	adminToken, _, err := authService.Login(ctx, "admin@integration.local", "integration-admin-password")
	if err != nil {
		t.Fatal(err)
	}

	masterKey := base64.StdEncoding.EncodeToString([]byte("0123456789abcdef0123456789abcdef"))
	credentialService, err := credentials.NewService(environment.postgres, masterKey)
	if err != nil {
		t.Fatal(err)
	}
	server := adminapi.New(environment.postgres, environment.redis, authService, configstore.New(environment.postgres, environment.redis), credentialService, runtimeops.New(environment.redis))
	body := `{"name":"交易操作员","all_instruments":false,"permissions":["runtime:view"],"instruments":[]}`
	request := httptest.NewRequest(http.MethodPut, fmt.Sprintf("/api/roles/%d", roleID), strings.NewReader(body))
	request.Header.Set("Authorization", "Bearer "+adminToken)
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusNoContent {
		t.Fatalf("update role status=%d body=%s", response.Code, response.Body.String())
	}

	if _, err := authService.Authenticate(ctx, operatorToken); !errors.Is(err, auth.ErrInvalidCredentials) {
		t.Fatalf("old session remained valid after role revocation: %v", err)
	}
	_, updatedSession, err := authService.Login(ctx, "operator@integration.local", "integration-user-password")
	if err != nil {
		t.Fatal(err)
	}
	if updatedSession.AuthorizationVersion <= originalSession.AuthorizationVersion || !updatedSession.Has("runtime:view") || updatedSession.Has("runtime:start") {
		t.Fatalf("unexpected updated authorization snapshot: before=%+v after=%+v", originalSession, updatedSession)
	}
}

func TestUserLifecycleInvalidatesSessionsAndProtectsLastAdministrator(t *testing.T) {
	environment := setupIntegrationEnvironment(t)
	ctx := context.Background()
	authService := auth.NewService(environment.postgres, environment.redis)
	const adminEmail = "admin@lifecycle.local"
	const adminPassword = "lifecycle-admin-password"
	if err := authService.BootstrapAdmin(ctx, adminEmail, adminPassword); err != nil {
		t.Fatal(err)
	}
	adminToken, adminSession, err := authService.Login(ctx, adminEmail, adminPassword)
	if err != nil {
		t.Fatal(err)
	}
	masterKey := base64.StdEncoding.EncodeToString([]byte("0123456789abcdef0123456789abcdef"))
	credentialService, err := credentials.NewService(environment.postgres, masterKey)
	if err != nil {
		t.Fatal(err)
	}
	handler := adminapi.New(environment.postgres, environment.redis, authService, configstore.New(environment.postgres, environment.redis), credentialService, runtimeops.New(environment.redis)).Handler()

	created := performJSONRequest(t, handler, http.MethodPost, "/api/users", adminToken, `{"email":"operator@lifecycle.local","password":"lifecycle-user-password","roles":["operator"]}`)
	if created.Code != http.StatusCreated {
		t.Fatalf("create user status=%d body=%s", created.Code, created.Body.String())
	}
	var createdBody struct {
		ID int64 `json:"id"`
	}
	if err := json.Unmarshal(created.Body.Bytes(), &createdBody); err != nil || createdBody.ID == 0 {
		t.Fatalf("decode created user: id=%d err=%v", createdBody.ID, err)
	}
	userToken, _, err := authService.Login(ctx, "operator@lifecycle.local", "lifecycle-user-password")
	if err != nil {
		t.Fatal(err)
	}

	disabled := performJSONRequest(t, handler, http.MethodPut, fmt.Sprintf("/api/users/%d", createdBody.ID), adminToken, `{"email":"operator@lifecycle.local","enabled":false,"roles":["operator"]}`)
	if disabled.Code != http.StatusNoContent {
		t.Fatalf("disable user status=%d body=%s", disabled.Code, disabled.Body.String())
	}
	if _, err := authService.Authenticate(ctx, userToken); !errors.Is(err, auth.ErrInvalidCredentials) {
		t.Fatalf("disabled user session remained valid: %v", err)
	}
	if _, _, err := authService.Login(ctx, "operator@lifecycle.local", "lifecycle-user-password"); !errors.Is(err, auth.ErrInvalidCredentials) {
		t.Fatalf("disabled user could log in: %v", err)
	}

	enabled := performJSONRequest(t, handler, http.MethodPut, fmt.Sprintf("/api/users/%d", createdBody.ID), adminToken, `{"email":"operator@lifecycle.local","enabled":true,"roles":["operator"]}`)
	if enabled.Code != http.StatusNoContent {
		t.Fatalf("enable user status=%d body=%s", enabled.Code, enabled.Body.String())
	}
	userToken, _, err = authService.Login(ctx, "operator@lifecycle.local", "lifecycle-user-password")
	if err != nil {
		t.Fatal(err)
	}
	updated := performJSONRequest(t, handler, http.MethodPut, fmt.Sprintf("/api/users/%d", createdBody.ID), adminToken, `{"email":"viewer@lifecycle.local","enabled":true,"roles":["viewer"]}`)
	if updated.Code != http.StatusNoContent {
		t.Fatalf("update user status=%d body=%s", updated.Code, updated.Body.String())
	}
	if _, err := authService.Authenticate(ctx, userToken); !errors.Is(err, auth.ErrInvalidCredentials) {
		t.Fatalf("role/email update retained old session: %v", err)
	}
	if _, _, err := authService.Login(ctx, "operator@lifecycle.local", "lifecycle-user-password"); !errors.Is(err, auth.ErrInvalidCredentials) {
		t.Fatalf("old email remained usable: %v", err)
	}
	userToken, viewerSession, err := authService.Login(ctx, "viewer@lifecycle.local", "lifecycle-user-password")
	if err != nil {
		t.Fatal(err)
	}
	if !viewerSession.Has("runtime:view") || viewerSession.Has("runtime:start") {
		t.Fatalf("unexpected viewer permissions: %+v", viewerSession)
	}
	changedOwnPassword := performJSONRequest(t, handler, http.MethodPut, "/api/me/password", userToken, `{"current_password":"lifecycle-user-password","new_password":"lifecycle-password-self"}`)
	if changedOwnPassword.Code != http.StatusNoContent {
		t.Fatalf("change own password status=%d body=%s", changedOwnPassword.Code, changedOwnPassword.Body.String())
	}
	if _, err := authService.Authenticate(ctx, userToken); !errors.Is(err, auth.ErrInvalidCredentials) {
		t.Fatalf("own password change retained old session: %v", err)
	}
	if _, _, err := authService.Login(ctx, "viewer@lifecycle.local", "lifecycle-user-password"); !errors.Is(err, auth.ErrInvalidCredentials) {
		t.Fatalf("password before self-service change remained usable: %v", err)
	}
	userToken, _, err = authService.Login(ctx, "viewer@lifecycle.local", "lifecycle-password-self")
	if err != nil {
		t.Fatal(err)
	}

	reset := performJSONRequest(t, handler, http.MethodPut, fmt.Sprintf("/api/users/%d/password", createdBody.ID), adminToken, `{"password":"lifecycle-password-new"}`)
	if reset.Code != http.StatusNoContent {
		t.Fatalf("reset password status=%d body=%s", reset.Code, reset.Body.String())
	}
	if _, err := authService.Authenticate(ctx, userToken); !errors.Is(err, auth.ErrInvalidCredentials) {
		t.Fatalf("password reset retained old session: %v", err)
	}
	if _, _, err := authService.Login(ctx, "viewer@lifecycle.local", "lifecycle-password-self"); !errors.Is(err, auth.ErrInvalidCredentials) {
		t.Fatalf("old password remained usable: %v", err)
	}
	userToken, _, err = authService.Login(ctx, "viewer@lifecycle.local", "lifecycle-password-new")
	if err != nil {
		t.Fatal(err)
	}
	revoked := performJSONRequest(t, handler, http.MethodPost, fmt.Sprintf("/api/users/%d/revoke-sessions", createdBody.ID), adminToken, ``)
	if revoked.Code != http.StatusNoContent {
		t.Fatalf("revoke sessions status=%d body=%s", revoked.Code, revoked.Body.String())
	}
	if _, err := authService.Authenticate(ctx, userToken); !errors.Is(err, auth.ErrInvalidCredentials) {
		t.Fatalf("revoked session remained valid: %v", err)
	}

	lastAdmin := performJSONRequest(t, handler, http.MethodPut, fmt.Sprintf("/api/users/%d", adminSession.UserID), adminToken, fmt.Sprintf(`{"email":%q,"enabled":false,"roles":["super_admin"]}`, adminEmail))
	if lastAdmin.Code != http.StatusBadRequest {
		t.Fatalf("last administrator was disabled: status=%d body=%s", lastAdmin.Code, lastAdmin.Body.String())
	}
	var lastLoginAt, passwordChangedAt time.Time
	if err := environment.postgres.QueryRow(ctx, `SELECT last_login_at,password_changed_at FROM users WHERE id=$1`, createdBody.ID).Scan(&lastLoginAt, &passwordChangedAt); err != nil {
		t.Fatal(err)
	}
	if lastLoginAt.IsZero() || passwordChangedAt.IsZero() {
		t.Fatalf("user lifecycle timestamps missing: login=%s password=%s", lastLoginAt, passwordChangedAt)
	}
	if _, err := environment.postgres.Exec(ctx, `UPDATE users SET enabled=FALSE WHERE id=$1`, adminSession.UserID); err != nil {
		t.Fatal(err)
	}
	if err := authService.BootstrapAdmin(ctx, adminEmail, adminPassword); err != nil {
		t.Fatal(err)
	}
	var bootstrapEnabled bool
	if err := environment.postgres.QueryRow(ctx, `SELECT enabled FROM users WHERE id=$1`, adminSession.UserID).Scan(&bootstrapEnabled); err != nil || !bootstrapEnabled {
		t.Fatalf("bootstrap administrator was not restored: enabled=%v err=%v", bootstrapEnabled, err)
	}
}

func performJSONRequest(t *testing.T, handler http.Handler, method, path, token, body string) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(method, path, strings.NewReader(body))
	request.Header.Set("Authorization", "Bearer "+token)
	if body != "" {
		request.Header.Set("Content-Type", "application/json")
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

func validConfig(pollIntervalMS int) config.Config {
	baseToken := "0x1111111111111111111111111111111111111111"
	quoteToken := "0x2222222222222222222222222222222222222222"
	return config.Config{
		Mode:           domain.ModeShadow,
		PollIntervalMS: pollIntervalMS,
		RPC: config.RPCConfig{
			URLs: []string{"https://bsc.integration.invalid"}, ChainID: 56, RequestTimeoutMS: 1000,
		},
		Instruments: []config.InstrumentConfig{{
			ID:    "token_usdt",
			Base:  config.AssetConfig{Symbol: "TOKEN", Address: baseToken, Decimals: 18},
			Quote: config.AssetConfig{Symbol: "USDT", Address: quoteToken, Decimals: 18},
			Reference: config.ReferenceConfig{
				Type: "pancake_v2", TWAPWindowSeconds: 60, StaleAfterSeconds: 30, AllowSpotDuringWarmup: true,
				Legs: []config.PairLegConfig{{PairAddress: "0x3333333333333333333333333333333333333333", BaseToken: baseToken, QuoteToken: quoteToken}},
			},
			Strategy: config.StrategyConfig{HalfSpreadBPS: 50, LevelSpacingBPS: 10, Levels: 1, OrderSize: num.Must("1"), RepriceThresholdBPS: 10, TargetBase: num.Must("1"), MaxBaseDeviation: num.Must("10")},
		}},
		Venues: map[string]config.VenueConfig{"binance": {
			Type: "binance", Environment: "testnet", Enabled: true, BaseURL: "https://testnet.binance.vision", SelfTradePrevention: "EXPIRE_BOTH",
			Markets: map[string]config.VenueMarketConfig{"token_usdt": {Symbol: "TOKENUSDT", BaseAsset: "TOKEN", QuoteAsset: "USDT", PriceTick: num.Must("0.01"), QuantityStep: num.Must("1"), MinNotional: num.Must("1")}},
		}},
	}
}
