package adminapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/mail"
	"net/url"
	"strconv"
	"strings"
	"time"

	"fluxmaker/internal/auth"
	"fluxmaker/internal/config"
	"fluxmaker/internal/configdiff"
	"fluxmaker/internal/configstore"
	"fluxmaker/internal/credentials"
	"fluxmaker/internal/domain"
	"fluxmaker/internal/oracle/pancakev2"
	"fluxmaker/internal/runtimeops"
	"fluxmaker/internal/venue"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"
)

type contextKey string

const sessionContextKey contextKey = "session"

type Server struct {
	db           *pgxpool.Pool
	redis        *redis.Client
	auth         *auth.Service
	configs      *configstore.Store
	secrets      *credentials.Service
	runtime      *runtimeops.Store
	mux          *http.ServeMux
	logger       *slog.Logger
	metricsToken string
}

type Option func(*Server)

func WithLogger(logger *slog.Logger) Option { return func(s *Server) { s.logger = logger } }
func WithMetricsToken(token string) Option {
	return func(s *Server) { s.metricsToken = strings.TrimSpace(token) }
}

func New(db *pgxpool.Pool, redisClient *redis.Client, authService *auth.Service, configs *configstore.Store, credentialService *credentials.Service, runtimeStore *runtimeops.Store, options ...Option) *Server {
	s := &Server{db: db, redis: redisClient, auth: authService, configs: configs, secrets: credentialService, runtime: runtimeStore, mux: http.NewServeMux(), logger: slog.Default()}
	for _, option := range options {
		option(s)
	}
	s.routes()
	return s
}

func (s *Server) Handler() http.Handler { return requestLogging(s.logger, securityHeaders(s.mux)) }

func (s *Server) routes() {
	s.registerUI()
	s.mux.HandleFunc("GET /livez", s.live)
	s.mux.HandleFunc("GET /healthz", s.health)
	s.mux.HandleFunc("GET /readyz", s.health)
	s.mux.HandleFunc("GET /metrics", s.prometheusMetrics)
	s.mux.HandleFunc("POST /api/login", s.login)
	s.mux.Handle("POST /api/logout", s.require("", http.HandlerFunc(s.logout)))
	s.mux.Handle("GET /api/me", s.require("", http.HandlerFunc(s.me)))
	s.mux.Handle("PUT /api/me/password", s.require("", http.HandlerFunc(s.changeOwnPassword)))
	s.mux.Handle("GET /api/config/draft", s.require("config:view", http.HandlerFunc(s.getDraft)))
	s.mux.Handle("PUT /api/config/draft", s.require("config:edit", http.HandlerFunc(s.putDraft)))
	s.mux.Handle("POST /api/config/publish", s.require("config:publish", http.HandlerFunc(s.publish)))
	s.mux.Handle("POST /api/config/plan", s.require("config:publish", http.HandlerFunc(s.configPlan)))
	s.mux.Handle("GET /api/config/active", s.require("config:view", http.HandlerFunc(s.active)))
	s.mux.Handle("GET /api/runtime", s.require("runtime:view", http.HandlerFunc(s.runtimeList)))
	s.mux.Handle("GET /api/monitoring", s.require("runtime:view", http.HandlerFunc(s.monitoring)))
	s.mux.Handle("GET /api/runtime/{id}", s.require("runtime:view", http.HandlerFunc(s.runtimeInstrument)))
	s.mux.Handle("POST /api/runtime/{id}/pause", s.require("runtime:stop", http.HandlerFunc(s.pauseInstrument)))
	s.mux.Handle("POST /api/runtime/{id}/resume", s.require("runtime:start", http.HandlerFunc(s.resumeInstrument)))
	s.mux.Handle("POST /api/runtime/{id}/emergency-cancel", s.require("runtime:emergency_cancel", http.HandlerFunc(s.emergencyCancel)))
	s.mux.Handle("POST /api/runtime/{id}/reconcile", s.require("runtime:start", http.HandlerFunc(s.reconcileInstrument)))
	s.mux.Handle("POST /api/oracle/pancake-v2/inspect-pair", s.require("config:edit", http.HandlerFunc(s.inspectPancakeV2Pair)))
	s.mux.Handle("GET /api/users", s.require("users:manage", http.HandlerFunc(s.listUsers)))
	s.mux.Handle("POST /api/users", s.require("users:manage", http.HandlerFunc(s.createUser)))
	s.mux.Handle("PUT /api/users/{id}", s.require("users:manage", http.HandlerFunc(s.updateUser)))
	s.mux.Handle("PUT /api/users/{id}/password", s.require("users:manage", http.HandlerFunc(s.resetUserPassword)))
	s.mux.Handle("POST /api/users/{id}/revoke-sessions", s.require("users:manage", http.HandlerFunc(s.revokeUserSessions)))
	s.mux.Handle("GET /api/user-role-options", s.require("users:manage", http.HandlerFunc(s.userRoleOptions)))
	s.mux.Handle("GET /api/roles", s.require("roles:manage", http.HandlerFunc(s.listRoles)))
	s.mux.Handle("PUT /api/roles/{id}", s.require("roles:manage", http.HandlerFunc(s.updateRole)))
	s.mux.Handle("GET /api/permissions", s.require("roles:manage", http.HandlerFunc(s.listPermissions)))
	s.mux.Handle("GET /api/credential-options", s.require("venue:view", http.HandlerFunc(s.credentialOptions)))
	s.mux.Handle("GET /api/venue-types", s.require("venue:view", http.HandlerFunc(s.venueTypes)))
	s.mux.Handle("GET /api/credentials", s.require("secrets:manage", http.HandlerFunc(s.listCredentials)))
	s.mux.Handle("POST /api/credentials", s.require("secrets:manage", http.HandlerFunc(s.createCredential)))
	s.mux.Handle("PUT /api/credentials/{id}", s.require("secrets:manage", http.HandlerFunc(s.updateCredential)))
}

func (s *Server) venueTypes(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, venue.AdapterSpecs())
}

func (s *Server) changeOwnPassword(w http.ResponseWriter, r *http.Request) {
	var body struct {
		CurrentPassword string `json:"current_password"`
		NewPassword     string `json:"new_password"`
	}
	if err := decodeJSON(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	session := sessionFromContext(r.Context())
	if err := s.auth.ChangePassword(r.Context(), session.UserID, body.CurrentPassword, body.NewPassword); errors.Is(err, auth.ErrInvalidCredentials) {
		writeError(w, http.StatusBadRequest, "current password is incorrect")
		return
	} else if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	s.auditResource(r.Context(), session.UserID, "user.password.change", "user", strconv.FormatInt(session.UserID, 10), map[string]any{})
	w.WriteHeader(http.StatusNoContent)
}

type configPlanResponse struct {
	FromVersion int64           `json:"from_version"`
	ToVersion   int64           `json:"to_version"`
	Plan        configdiff.Plan `json:"plan"`
}

func (s *Server) configPlan(w http.ResponseWriter, r *http.Request) {
	session := sessionFromContext(r.Context())
	draft, err := s.configs.GetDraft(r.Context())
	if err != nil {
		writeError(w, http.StatusBadRequest, "draft configuration not available")
		return
	}
	if err := enforceInstrumentScope(session, draft); err != nil {
		writeError(w, http.StatusForbidden, err.Error())
		return
	}
	if err := draft.Validate(); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	active, err := s.configs.LoadActive(r.Context())
	if errors.Is(err, configstore.ErrNoActive) {
		writeJSON(w, http.StatusOK, configPlanResponse{FromVersion: 0, ToVersion: 1, Plan: configdiff.Build(nil, draft)})
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "load active configuration failed")
		return
	}
	writeJSON(w, http.StatusOK, configPlanResponse{FromVersion: active.Version, ToVersion: active.Version + 1, Plan: configdiff.Build(&active.Config, draft)})
}

type inspectPairResponse struct {
	pancakev2.PairInfo
	BaseToken  string `json:"base_token"`
	QuoteToken string `json:"quote_token"`
}

func (s *Server) inspectPancakeV2Pair(w http.ResponseWriter, r *http.Request) {
	session := sessionFromContext(r.Context())
	if !session.Has("instrument:edit") {
		writeError(w, http.StatusForbidden, "instrument:edit permission is required")
		return
	}
	var body struct {
		PairAddress     string `json:"pair_address"`
		InputToken      string `json:"input_token"`
		ExpectedFactory string `json:"expected_factory"`
	}
	if err := decodeJSON(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if !isEVMAddress(body.PairAddress) || !isEVMAddress(body.InputToken) {
		writeError(w, http.StatusBadRequest, "valid pair_address and input_token are required")
		return
	}
	if body.ExpectedFactory != "" && !isEVMAddress(body.ExpectedFactory) {
		writeError(w, http.StatusBadRequest, "expected_factory is invalid")
		return
	}
	cfg, err := s.configs.GetDraft(r.Context())
	if err != nil || len(cfg.RPC.URLs) == 0 {
		writeError(w, http.StatusBadRequest, "configure and save a BNB Chain RPC first")
		return
	}
	timeout := cfg.RequestTimeout()
	if timeout <= 0 || timeout > 15*time.Second {
		timeout = 5 * time.Second
	}
	ctx, cancel := context.WithTimeout(r.Context(), timeout)
	defer cancel()
	rpc := pancakev2.NewRPCClient(cfg.RPC.URLs, timeout)
	chainID, err := rpc.ChainID(ctx)
	if err != nil {
		writeError(w, http.StatusBadGateway, "query BNB Chain RPC failed: "+err.Error())
		return
	}
	if chainID != cfg.RPC.ChainID {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("RPC chain id %d does not match configured %d", chainID, cfg.RPC.ChainID))
		return
	}
	info, err := rpc.InspectPair(ctx, body.PairAddress)
	if err != nil {
		writeError(w, http.StatusBadGateway, "inspect Pair failed: "+err.Error())
		return
	}
	if body.ExpectedFactory != "" && !strings.EqualFold(body.ExpectedFactory, info.Factory) {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("Pair factory %s does not match expected %s", info.Factory, body.ExpectedFactory))
		return
	}
	baseToken, quoteToken, err := orientPairTokens(info, body.InputToken)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, inspectPairResponse{PairInfo: info, BaseToken: baseToken, QuoteToken: quoteToken})
}

func orientPairTokens(info pancakev2.PairInfo, inputToken string) (string, string, error) {
	if strings.EqualFold(inputToken, info.Token0) {
		return info.Token0, info.Token1, nil
	}
	if strings.EqualFold(inputToken, info.Token1) {
		return info.Token1, info.Token0, nil
	}
	return "", "", fmt.Errorf("Pair does not contain path input token %s", inputToken)
}

func isEVMAddress(value string) bool {
	value = strings.TrimSpace(value)
	if len(value) != 42 || !strings.HasPrefix(strings.ToLower(value), "0x") {
		return false
	}
	for _, char := range value[2:] {
		if !((char >= '0' && char <= '9') || (char >= 'a' && char <= 'f') || (char >= 'A' && char <= 'F')) {
			return false
		}
	}
	return true
}

type runtimeResponse struct {
	Engine      runtimeops.EngineStatus         `json:"engine"`
	Instruments []runtimeops.InstrumentSnapshot `json:"instruments"`
	Monitoring  MonitoringSummary               `json:"monitoring"`
}

func (s *Server) runtimeList(w http.ResponseWriter, r *http.Request) {
	session := sessionFromContext(r.Context())
	cfg, err := s.runtimeConfig(r.Context())
	if err != nil {
		engine := filterEngineRuleChanges(s.runtime.EngineStatus(r.Context()), &session)
		writeJSON(w, http.StatusOK, runtimeResponse{Engine: engine, Instruments: []runtimeops.InstrumentSnapshot{}, Monitoring: BuildMonitoringSummary(config.Config{}, engine, nil, time.Now().UTC())})
		return
	}
	paused, _ := s.runtime.Paused(r.Context())
	result := runtimeResponse{Engine: filterEngineRuleChanges(s.runtime.EngineStatus(r.Context()), &session), Instruments: make([]runtimeops.InstrumentSnapshot, 0, len(cfg.Instruments))}
	for _, instrument := range cfg.Instruments {
		if !session.CanAccessInstrument(instrument.ID) {
			continue
		}
		snapshot, snapshotErr := s.runtime.Get(r.Context(), instrument.ID)
		if snapshotErr != nil {
			snapshot = runtimeops.InstrumentSnapshot{InstrumentID: instrument.ID, BaseSymbol: instrument.Base.Symbol, QuoteSymbol: instrument.Quote.Symbol, Mode: cfg.Mode, Status: "waiting", TargetInventory: instrument.Strategy.TargetBase, MaxBaseDeviation: instrument.Strategy.MaxBaseDeviation, Venues: []runtimeops.VenueSnapshot{}}
		}
		pause, pauseRequested := paused[instrument.ID]
		mergeRuntimeControlState(&snapshot, pause, pauseRequested)
		redactRuntimeSnapshot(session, &snapshot)
		result.Instruments = append(result.Instruments, snapshot)
	}
	result.Monitoring = BuildMonitoringSummary(cfg, result.Engine, result.Instruments, time.Now().UTC())
	writeJSON(w, http.StatusOK, result)
}

func (s *Server) runtimeInstrument(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	session := sessionFromContext(r.Context())
	if !session.CanAccessInstrument(id) {
		writeError(w, http.StatusForbidden, "instrument is outside your data scope")
		return
	}
	cfg, err := s.runtimeConfig(r.Context())
	if err != nil {
		writeError(w, http.StatusNotFound, "runtime configuration not available")
		return
	}
	var configured *config.InstrumentConfig
	for index := range cfg.Instruments {
		if cfg.Instruments[index].ID == id {
			configured = &cfg.Instruments[index]
			break
		}
	}
	if configured == nil {
		writeError(w, http.StatusNotFound, "instrument is not in the runtime configuration")
		return
	}
	snapshot, err := s.runtime.Get(r.Context(), id)
	if err != nil {
		snapshot = runtimeops.InstrumentSnapshot{InstrumentID: id, BaseSymbol: configured.Base.Symbol, QuoteSymbol: configured.Quote.Symbol, Mode: cfg.Mode, Status: "waiting", TargetInventory: configured.Strategy.TargetBase, MaxBaseDeviation: configured.Strategy.MaxBaseDeviation, Venues: []runtimeops.VenueSnapshot{}}
	}
	paused, _ := s.runtime.Paused(r.Context())
	pause, pauseRequested := paused[id]
	mergeRuntimeControlState(&snapshot, pause, pauseRequested)
	redactRuntimeSnapshot(session, &snapshot)
	writeJSON(w, http.StatusOK, snapshot)
}

func mergeRuntimeControlState(snapshot *runtimeops.InstrumentSnapshot, pause runtimeops.PauseState, pauseRequested bool) {
	if pauseRequested {
		snapshot.Pause = &pause
		if snapshot.Paused {
			snapshot.Status = "paused"
		} else {
			snapshot.Status = "pausing"
		}
		return
	}
	if snapshot.Paused {
		snapshot.Pause = nil
		snapshot.Status = "resuming"
	}
}

func redactRuntimeSnapshot(session auth.Session, snapshot *runtimeops.InstrumentSnapshot) {
	if !session.Has("fills:view") && snapshot.TradeSimulation != nil {
		snapshot.TradeSimulation.Fills = []domain.Fill{}
	}
	for index := range snapshot.Venues {
		if !session.Has("orders:view") {
			snapshot.Venues[index].OpenOrders = []domain.Order{}
		}
		if !session.Has("fills:view") {
			snapshot.Venues[index].Fills = []domain.Fill{}
		}
	}
}

func (s *Server) pauseInstrument(w http.ResponseWriter, r *http.Request) {
	s.controlPause(w, r, runtimeops.ReasonManualPause, "runtime.pause")
}

func (s *Server) emergencyCancel(w http.ResponseWriter, r *http.Request) {
	s.controlPause(w, r, runtimeops.ReasonEmergencyCancel, "runtime.emergency_cancel")
}

func (s *Server) controlPause(w http.ResponseWriter, r *http.Request, reason, action string) {
	id := r.PathValue("id")
	session := sessionFromContext(r.Context())
	if err := s.authorizeRuntimeInstrument(r.Context(), session, id); err != nil {
		writeError(w, http.StatusForbidden, err.Error())
		return
	}
	state, err := s.runtime.SetPaused(r.Context(), id, reason, session.UserID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "set pause control failed")
		return
	}
	s.auditResource(r.Context(), session.UserID, action, "instrument", id, map[string]any{"reason": reason})
	writeJSON(w, http.StatusAccepted, state)
}

func (s *Server) resumeInstrument(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	session := sessionFromContext(r.Context())
	if err := s.authorizeRuntimeInstrument(r.Context(), session, id); err != nil {
		writeError(w, http.StatusForbidden, err.Error())
		return
	}
	if err := s.runtime.Resume(r.Context(), id); err != nil {
		writeError(w, http.StatusInternalServerError, "resume control failed")
		return
	}
	s.auditResource(r.Context(), session.UserID, "runtime.resume", "instrument", id, map[string]any{})
	writeJSON(w, http.StatusAccepted, map[string]any{"instrument_id": id, "paused": false})
}

func (s *Server) reconcileInstrument(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	session := sessionFromContext(r.Context())
	if err := s.authorizeRuntimeInstrument(r.Context(), session, id); err != nil {
		writeError(w, http.StatusForbidden, err.Error())
		return
	}
	request, err := s.runtime.RequestReconcile(r.Context(), id, session.UserID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "request reconcile failed")
		return
	}
	s.auditResource(r.Context(), session.UserID, "runtime.reconcile", "instrument", id, map[string]any{})
	writeJSON(w, http.StatusAccepted, request)
}

func (s *Server) authorizeRuntimeInstrument(ctx context.Context, session auth.Session, instrumentID string) error {
	if !session.CanAccessInstrument(instrumentID) {
		return fmt.Errorf("instrument is outside your data scope")
	}
	cfg, err := s.configs.LoadActive(ctx)
	if err != nil {
		return fmt.Errorf("no published runtime configuration")
	}
	for _, instrument := range cfg.Config.Instruments {
		if instrument.ID == instrumentID {
			return nil
		}
	}
	return fmt.Errorf("instrument is not in the published configuration")
}

func (s *Server) runtimeConfig(ctx context.Context) (config.Config, error) {
	active, err := s.configs.LoadActive(ctx)
	if err == nil {
		return active.Config, nil
	}
	return s.configs.GetDraft(ctx)
}

type credentialOption struct {
	ID        int64  `json:"id"`
	Name      string `json:"name"`
	VenueType string `json:"venue_type"`
	Enabled   bool   `json:"enabled"`
}

func (s *Server) credentialOptions(w http.ResponseWriter, r *http.Request) {
	items, err := s.secrets.List(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "list credential options failed")
		return
	}
	options := make([]credentialOption, 0, len(items))
	for _, item := range items {
		options = append(options, credentialOption{ID: item.ID, Name: item.Name, VenueType: item.VenueType, Enabled: item.Enabled})
	}
	writeJSON(w, http.StatusOK, options)
}

func (s *Server) listCredentials(w http.ResponseWriter, r *http.Request) {
	items, err := s.secrets.List(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "list credentials failed")
		return
	}
	writeJSON(w, http.StatusOK, items)
}

func (s *Server) createCredential(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name      string `json:"name"`
		VenueType string `json:"venue_type"`
		APIKey    string `json:"api_key"`
		APISecret string `json:"api_secret"`
	}
	if err := decodeJSON(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	session := sessionFromContext(r.Context())
	item, err := s.secrets.Create(r.Context(), body.Name, body.VenueType, body.APIKey, body.APISecret, session.UserID)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	s.audit(r.Context(), session.UserID, "credential.create", map[string]any{"credential_id": item.ID, "name": item.Name, "venue_type": item.VenueType})
	writeJSON(w, http.StatusCreated, item)
}

func (s *Server) updateCredential(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		writeError(w, http.StatusBadRequest, "invalid credential id")
		return
	}
	var body struct {
		Name      string `json:"name"`
		APIKey    string `json:"api_key"`
		APISecret string `json:"api_secret"`
		Enabled   *bool  `json:"enabled,omitempty"`
	}
	if err := decodeJSON(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	session := sessionFromContext(r.Context())
	item, err := s.secrets.Update(r.Context(), id, body.Name, body.APIKey, body.APISecret, body.Enabled, session.UserID)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	s.audit(r.Context(), session.UserID, "credential.update", map[string]any{"credential_id": item.ID, "name": item.Name, "enabled": item.Enabled})
	writeJSON(w, http.StatusOK, item)
}

func (s *Server) audit(ctx context.Context, userID int64, action string, detail map[string]any) {
	s.auditResource(ctx, userID, action, "venue_credential", "", detail)
}

func (s *Server) auditResource(ctx context.Context, userID int64, action, resourceType, resourceID string, detail map[string]any) {
	data, _ := json.Marshal(detail)
	_, _ = s.db.Exec(ctx, `INSERT INTO audit_logs(user_id,action,resource_type,resource_id,details) VALUES($1,$2,$3,$4,$5)`, nullableAdminUser(userID), action, resourceType, resourceID, data)
}

func nullableAdminUser(id int64) any {
	if id <= 0 {
		return nil
	}
	return id
}

func (s *Server) health(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), time.Second)
	defer cancel()
	if err := s.db.Ping(ctx); err != nil {
		writeError(w, http.StatusServiceUnavailable, "postgres unavailable")
		return
	}
	if err := s.redis.Ping(ctx).Err(); err != nil {
		writeError(w, http.StatusServiceUnavailable, "redis unavailable")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) live(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Email    string `json:"email"`
		Password string `json:"password"`
	}
	if err := decodeJSON(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	token, session, err := s.auth.Login(r.Context(), body.Email, body.Password)
	if errors.Is(err, auth.ErrInvalidCredentials) {
		writeError(w, http.StatusUnauthorized, "invalid credentials")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "login failed")
		return
	}
	http.SetCookie(w, &http.Cookie{Name: "fluxmaker_session", Value: token, Path: "/", HttpOnly: true, SameSite: http.SameSiteStrictMode, Secure: r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https", Expires: session.ExpiresAt})
	writeJSON(w, http.StatusOK, map[string]any{"expires_at": session.ExpiresAt, "user": session})
}

func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	_ = s.auth.Logout(r.Context(), bearerToken(r))
	http.SetCookie(w, &http.Cookie{Name: "fluxmaker_session", Value: "", Path: "/", HttpOnly: true, SameSite: http.SameSiteStrictMode, MaxAge: -1})
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) me(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, sessionFromContext(r.Context()))
}

func (s *Server) getDraft(w http.ResponseWriter, r *http.Request) {
	cfg, err := s.configs.GetDraft(r.Context())
	if errors.Is(err, configstore.ErrNoDraft) {
		writeError(w, http.StatusNotFound, "no draft configuration")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "load draft failed")
		return
	}
	writeJSON(w, http.StatusOK, scopeConfig(sessionFromContext(r.Context()), cfg))
}

func (s *Server) putDraft(w http.ResponseWriter, r *http.Request) {
	var cfg config.Config
	if err := decodeJSON(r, &cfg); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	session := sessionFromContext(r.Context())
	if err := enforceInstrumentScope(session, cfg); err != nil {
		writeError(w, http.StatusForbidden, err.Error())
		return
	}
	if !session.AllInstruments {
		existing, err := s.configs.GetDraft(r.Context())
		if err != nil {
			writeError(w, http.StatusBadRequest, "a global draft must exist before scoped editing")
			return
		}
		cfg = mergeScopedConfig(existing, cfg, session)
	}
	// Saving now takes effect immediately: validate credential bindings and
	// atomically activate the edit. The running engine picks it up by content
	// diff on its next reload — no separate publish step.
	if err := s.validateCredentialBindings(r.Context(), cfg); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if _, err := s.configs.SaveActive(r.Context(), cfg, session.UserID); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "saved"})
}

func mergeScopedConfig(existing, incoming config.Config, session auth.Session) config.Config {
	allowed := make(map[string]bool, len(session.Instruments))
	for _, id := range session.Instruments {
		allowed[id] = true
	}
	merged := existing
	if merged.Venues == nil {
		merged.Venues = make(map[string]config.VenueConfig)
	}
	merged.Instruments = nil
	for _, instrument := range existing.Instruments {
		if !allowed[instrument.ID] {
			merged.Instruments = append(merged.Instruments, instrument)
		}
	}
	for _, instrument := range incoming.Instruments {
		if allowed[instrument.ID] {
			merged.Instruments = append(merged.Instruments, instrument)
		}
	}
	for venueName, existingVenue := range existing.Venues {
		venue := existingVenue
		venue.Markets = make(map[string]config.VenueMarketConfig)
		for instrumentID, market := range existingVenue.Markets {
			if !allowed[instrumentID] {
				venue.Markets[instrumentID] = market
			}
		}
		if incomingVenue, ok := incoming.Venues[venueName]; ok {
			for instrumentID, market := range incomingVenue.Markets {
				if allowed[instrumentID] {
					venue.Markets[instrumentID] = market
				}
			}
		}
		merged.Venues[venueName] = venue
	}
	return merged
}

// publish is retained for backward compatibility with clients that still call
// it after saving. Edits now take effect on save (see putDraft), so this is an
// idempotent no-op that simply returns the already-active configuration.
func (s *Server) publish(w http.ResponseWriter, r *http.Request) {
	session := sessionFromContext(r.Context())
	snapshot, err := s.configs.LoadActive(r.Context())
	if errors.Is(err, configstore.ErrNoActive) {
		writeError(w, http.StatusBadRequest, "no configuration has been saved yet")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "load active configuration failed")
		return
	}
	snapshot.Config = scopeConfig(session, snapshot.Config)
	writeJSON(w, http.StatusOK, snapshot)
}

func (s *Server) validateCredentialBindings(ctx context.Context, cfg config.Config) error {
	if cfg.Mode != "live" {
		return nil
	}
	for venueName, venueCfg := range cfg.Venues {
		if !venueCfg.Enabled || !venueCfg.TradingEnabled {
			continue
		}
		for instrumentID, market := range venueCfg.Markets {
			if err := s.secrets.ValidateReference(ctx, market.CredentialID, venueCfg.Type); err != nil {
				return fmt.Errorf("venue %s market %s: %w", venueName, instrumentID, err)
			}
		}
	}
	return nil
}

func (s *Server) active(w http.ResponseWriter, r *http.Request) {
	snapshot, err := s.configs.LoadActive(r.Context())
	if errors.Is(err, configstore.ErrNoActive) {
		writeError(w, http.StatusNotFound, "no published configuration")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "load active configuration failed")
		return
	}
	snapshot.Config = scopeConfig(sessionFromContext(r.Context()), snapshot.Config)
	writeJSON(w, http.StatusOK, snapshot)
}

type userView struct {
	ID                int64      `json:"id"`
	Email             string     `json:"email"`
	Enabled           bool       `json:"enabled"`
	Roles             []string   `json:"roles"`
	CreatedAt         time.Time  `json:"created_at"`
	UpdatedAt         time.Time  `json:"updated_at"`
	LastLoginAt       *time.Time `json:"last_login_at,omitempty"`
	PasswordChangedAt time.Time  `json:"password_changed_at"`
}
type roleView struct {
	ID             int64    `json:"id"`
	Code           string   `json:"code"`
	Name           string   `json:"name"`
	AllInstruments bool     `json:"all_instruments"`
	Permissions    []string `json:"permissions"`
	Instruments    []string `json:"instruments"`
}

type userRoleOption struct {
	Code string `json:"code"`
	Name string `json:"name"`
}

type permissionView struct {
	Code string `json:"code"`
	Name string `json:"name"`
}

func (s *Server) listPermissions(w http.ResponseWriter, r *http.Request) {
	rows, err := s.db.Query(r.Context(), `SELECT code,name FROM permissions ORDER BY code`)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "list permissions failed")
		return
	}
	defer rows.Close()
	permissions := make([]permissionView, 0)
	for rows.Next() {
		var item permissionView
		if err := rows.Scan(&item.Code, &item.Name); err != nil {
			writeError(w, http.StatusInternalServerError, "list permissions failed")
			return
		}
		permissions = append(permissions, item)
	}
	writeJSON(w, http.StatusOK, permissions)
}

func (s *Server) listUsers(w http.ResponseWriter, r *http.Request) {
	rows, err := s.db.Query(r.Context(), `SELECT id,email,enabled,created_at,updated_at,last_login_at,password_changed_at FROM users ORDER BY email`)
	if err != nil {
		writeError(w, 500, "list users failed")
		return
	}
	defer rows.Close()
	users := make([]userView, 0)
	for rows.Next() {
		var user userView
		if rows.Scan(&user.ID, &user.Email, &user.Enabled, &user.CreatedAt, &user.UpdatedAt, &user.LastLoginAt, &user.PasswordChangedAt) != nil {
			writeError(w, 500, "list users failed")
			return
		}
		roleRows, err := s.db.Query(r.Context(), `SELECT r.code FROM user_roles ur JOIN roles r ON r.id=ur.role_id WHERE ur.user_id=$1 ORDER BY r.code`, user.ID)
		if err != nil {
			writeError(w, 500, "list users failed")
			return
		}
		for roleRows.Next() {
			var code string
			_ = roleRows.Scan(&code)
			user.Roles = append(user.Roles, code)
		}
		roleRows.Close()
		users = append(users, user)
	}
	writeJSON(w, 200, users)
}

func (s *Server) userRoleOptions(w http.ResponseWriter, r *http.Request) {
	rows, err := s.db.Query(r.Context(), `SELECT code,name FROM roles ORDER BY id`)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "list user role options failed")
		return
	}
	defer rows.Close()
	result := make([]userRoleOption, 0)
	for rows.Next() {
		var item userRoleOption
		if err := rows.Scan(&item.Code, &item.Name); err != nil {
			writeError(w, http.StatusInternalServerError, "list user role options failed")
			return
		}
		result = append(result, item)
	}
	if err := rows.Err(); err != nil {
		writeError(w, http.StatusInternalServerError, "list user role options failed")
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (s *Server) createUser(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Email    string   `json:"email"`
		Password string   `json:"password"`
		Roles    []string `json:"roles"`
	}
	if err := decodeJSON(r, &body); err != nil {
		writeError(w, 400, err.Error())
		return
	}
	email, err := normalizeUserEmail(body.Email)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	roleCodes, err := normalizeRoleCodes(body.Roles)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	hash, err := auth.HashPassword(body.Password)
	if err != nil {
		writeError(w, 400, err.Error())
		return
	}
	tx, err := s.db.Begin(r.Context())
	if err != nil {
		writeError(w, 500, "create user failed")
		return
	}
	defer tx.Rollback(r.Context())
	session := sessionFromContext(r.Context())
	actorSuperAdmin, err := isSuperAdmin(r.Context(), tx, session.UserID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "create user failed")
		return
	}
	if containsString(roleCodes, "super_admin") && !actorSuperAdmin {
		writeError(w, http.StatusForbidden, "only a super administrator can assign super_admin")
		return
	}
	roleIDs, err := resolveRoleIDs(r.Context(), tx, roleCodes)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	var userID int64
	if err := tx.QueryRow(r.Context(), `INSERT INTO users(email,password_hash) VALUES($1,$2) RETURNING id`, email, hash).Scan(&userID); err != nil {
		writeError(w, 400, "email already exists or is invalid")
		return
	}
	for _, roleID := range roleIDs {
		if _, err := tx.Exec(r.Context(), `INSERT INTO user_roles(user_id,role_id) VALUES($1,$2)`, userID, roleID); err != nil {
			writeError(w, http.StatusInternalServerError, "assign user role failed")
			return
		}
	}
	if err := tx.Commit(r.Context()); err != nil {
		writeError(w, 500, "create user failed")
		return
	}
	s.auditResource(r.Context(), session.UserID, "user.create", "user", strconv.FormatInt(userID, 10), map[string]any{"email": email, "roles": roleCodes})
	writeJSON(w, http.StatusCreated, map[string]any{"id": userID, "email": email, "enabled": true, "roles": roleCodes})
}

func (s *Server) updateUser(w http.ResponseWriter, r *http.Request) {
	userID, err := parseUserID(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	var body struct {
		Email   string   `json:"email"`
		Enabled *bool    `json:"enabled"`
		Roles   []string `json:"roles"`
	}
	if err := decodeJSON(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	email, err := normalizeUserEmail(body.Email)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	roleCodes, err := normalizeRoleCodes(body.Roles)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	tx, err := s.db.Begin(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "update user failed")
		return
	}
	defer tx.Rollback(r.Context())
	var currentEmail string
	var currentEnabled bool
	if err := tx.QueryRow(r.Context(), `SELECT email,enabled FROM users WHERE id=$1 FOR UPDATE`, userID).Scan(&currentEmail, &currentEnabled); errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusNotFound, "user not found")
		return
	} else if err != nil {
		writeError(w, http.StatusInternalServerError, "update user failed")
		return
	}
	session := sessionFromContext(r.Context())
	actorSuperAdmin, err := isSuperAdmin(r.Context(), tx, session.UserID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "update user failed")
		return
	}
	targetSuperAdmin, err := isSuperAdmin(r.Context(), tx, userID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "update user failed")
		return
	}
	desiredSuperAdmin := containsString(roleCodes, "super_admin")
	if (targetSuperAdmin || desiredSuperAdmin) && !actorSuperAdmin {
		writeError(w, http.StatusForbidden, "only a super administrator can manage super_admin accounts")
		return
	}
	enabled := currentEnabled
	if body.Enabled != nil {
		enabled = *body.Enabled
	}
	if userID == session.UserID && !enabled {
		writeError(w, http.StatusBadRequest, "you cannot disable your current account")
		return
	}
	if userID == session.UserID && targetSuperAdmin && !desiredSuperAdmin {
		writeError(w, http.StatusBadRequest, "you cannot remove your own super_admin role")
		return
	}
	if currentEnabled && targetSuperAdmin && (!enabled || !desiredSuperAdmin) {
		remaining, err := enabledSuperAdminCountExcluding(r.Context(), tx, userID)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "update user failed")
			return
		}
		if remaining == 0 {
			writeError(w, http.StatusBadRequest, "at least one enabled super administrator is required")
			return
		}
	}
	roleIDs, err := resolveRoleIDs(r.Context(), tx, roleCodes)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if _, err := tx.Exec(r.Context(), `UPDATE users SET email=$1,enabled=$2,authorization_version=authorization_version+1,updated_at=now() WHERE id=$3`, email, enabled, userID); err != nil {
		writeError(w, http.StatusBadRequest, "email already exists or is invalid")
		return
	}
	if _, err := tx.Exec(r.Context(), `DELETE FROM user_roles WHERE user_id=$1`, userID); err != nil {
		writeError(w, http.StatusInternalServerError, "replace user roles failed")
		return
	}
	for _, roleID := range roleIDs {
		if _, err := tx.Exec(r.Context(), `INSERT INTO user_roles(user_id,role_id) VALUES($1,$2)`, userID, roleID); err != nil {
			writeError(w, http.StatusInternalServerError, "replace user roles failed")
			return
		}
	}
	if err := tx.Commit(r.Context()); err != nil {
		writeError(w, http.StatusInternalServerError, "update user failed")
		return
	}
	s.auditResource(r.Context(), session.UserID, "user.update", "user", strconv.FormatInt(userID, 10), map[string]any{"previous_email": currentEmail, "email": email, "enabled": enabled, "roles": roleCodes})
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) resetUserPassword(w http.ResponseWriter, r *http.Request) {
	userID, err := parseUserID(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	var body struct {
		Password string `json:"password"`
	}
	if err := decodeJSON(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	hash, err := auth.HashPassword(body.Password)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	tx, err := s.db.Begin(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "reset password failed")
		return
	}
	defer tx.Rollback(r.Context())
	var targetEmail string
	if err := tx.QueryRow(r.Context(), `SELECT email FROM users WHERE id=$1 FOR UPDATE`, userID).Scan(&targetEmail); errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusNotFound, "user not found")
		return
	} else if err != nil {
		writeError(w, http.StatusInternalServerError, "reset password failed")
		return
	}
	session := sessionFromContext(r.Context())
	actorSuperAdmin, err := isSuperAdmin(r.Context(), tx, session.UserID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "reset password failed")
		return
	}
	targetSuperAdmin, err := isSuperAdmin(r.Context(), tx, userID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "reset password failed")
		return
	}
	if targetSuperAdmin && !actorSuperAdmin {
		writeError(w, http.StatusForbidden, "only a super administrator can reset this password")
		return
	}
	if _, err := tx.Exec(r.Context(), `UPDATE users SET password_hash=$1,authorization_version=authorization_version+1,password_changed_at=now(),updated_at=now() WHERE id=$2`, hash, userID); err != nil {
		writeError(w, http.StatusInternalServerError, "reset password failed")
		return
	}
	if err := tx.Commit(r.Context()); err != nil {
		writeError(w, http.StatusInternalServerError, "reset password failed")
		return
	}
	s.auditResource(r.Context(), session.UserID, "user.password.reset", "user", strconv.FormatInt(userID, 10), map[string]any{"email": targetEmail})
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) revokeUserSessions(w http.ResponseWriter, r *http.Request) {
	userID, err := parseUserID(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	session := sessionFromContext(r.Context())
	tx, err := s.db.Begin(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "revoke sessions failed")
		return
	}
	defer tx.Rollback(r.Context())
	var targetEmail string
	if err := tx.QueryRow(r.Context(), `SELECT email FROM users WHERE id=$1 FOR UPDATE`, userID).Scan(&targetEmail); errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusNotFound, "user not found")
		return
	} else if err != nil {
		writeError(w, http.StatusInternalServerError, "revoke sessions failed")
		return
	}
	targetSuperAdmin, err := isSuperAdmin(r.Context(), tx, userID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "revoke sessions failed")
		return
	}
	actorSuperAdmin, err := isSuperAdmin(r.Context(), tx, session.UserID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "revoke sessions failed")
		return
	}
	if targetSuperAdmin && !actorSuperAdmin {
		writeError(w, http.StatusForbidden, "only a super administrator can revoke these sessions")
		return
	}
	if _, err := tx.Exec(r.Context(), `UPDATE users SET authorization_version=authorization_version+1,updated_at=now() WHERE id=$1`, userID); err != nil {
		writeError(w, http.StatusInternalServerError, "revoke sessions failed")
		return
	}
	if err := tx.Commit(r.Context()); err != nil {
		writeError(w, http.StatusInternalServerError, "revoke sessions failed")
		return
	}
	s.auditResource(r.Context(), session.UserID, "user.sessions.revoke", "user", strconv.FormatInt(userID, 10), map[string]any{"email": targetEmail})
	w.WriteHeader(http.StatusNoContent)
}

type rowQueryer interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}

func parseUserID(r *http.Request) (int64, error) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		return 0, fmt.Errorf("invalid user id")
	}
	return id, nil
}

func normalizeUserEmail(value string) (string, error) {
	normalized := strings.ToLower(strings.TrimSpace(value))
	parsed, err := mail.ParseAddress(normalized)
	if err != nil || !strings.EqualFold(parsed.Address, normalized) {
		return "", fmt.Errorf("a valid email address is required")
	}
	return normalized, nil
}

func normalizeRoleCodes(values []string) ([]string, error) {
	seen := make(map[string]struct{}, len(values))
	result := make([]string, 0, len(values))
	for _, value := range values {
		code := strings.ToLower(strings.TrimSpace(value))
		if code == "" {
			continue
		}
		if _, exists := seen[code]; exists {
			continue
		}
		seen[code] = struct{}{}
		result = append(result, code)
	}
	if len(result) == 0 {
		return nil, fmt.Errorf("at least one role is required")
	}
	return result, nil
}

func resolveRoleIDs(ctx context.Context, queryer rowQueryer, codes []string) ([]int64, error) {
	result := make([]int64, 0, len(codes))
	for _, code := range codes {
		var id int64
		if err := queryer.QueryRow(ctx, `SELECT id FROM roles WHERE code=$1`, code).Scan(&id); errors.Is(err, pgx.ErrNoRows) {
			return nil, fmt.Errorf("unknown role: %s", code)
		} else if err != nil {
			return nil, err
		}
		result = append(result, id)
	}
	return result, nil
}

func isSuperAdmin(ctx context.Context, queryer rowQueryer, userID int64) (bool, error) {
	var result bool
	err := queryer.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM user_roles ur JOIN roles r ON r.id=ur.role_id WHERE ur.user_id=$1 AND r.code='super_admin')`, userID).Scan(&result)
	return result, err
}

func enabledSuperAdminCountExcluding(ctx context.Context, queryer rowQueryer, userID int64) (int, error) {
	var count int
	err := queryer.QueryRow(ctx, `SELECT COUNT(DISTINCT u.id) FROM users u JOIN user_roles ur ON ur.user_id=u.id JOIN roles r ON r.id=ur.role_id WHERE u.enabled=TRUE AND r.code='super_admin' AND u.id<>$1`, userID).Scan(&count)
	return count, err
}

func containsString(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func (s *Server) listRoles(w http.ResponseWriter, r *http.Request) {
	rows, err := s.db.Query(r.Context(), `SELECT id,code,name,all_instruments FROM roles ORDER BY id`)
	if err != nil {
		writeError(w, 500, "list roles failed")
		return
	}
	defer rows.Close()
	roles := make([]roleView, 0)
	for rows.Next() {
		var role roleView
		if rows.Scan(&role.ID, &role.Code, &role.Name, &role.AllInstruments) != nil {
			writeError(w, 500, "list roles failed")
			return
		}
		role.Permissions, _ = queryStringList(r.Context(), s.db, `SELECT permission_code FROM role_permissions WHERE role_id=$1 ORDER BY permission_code`, role.ID)
		role.Instruments, _ = queryStringList(r.Context(), s.db, `SELECT instrument_id FROM role_instruments WHERE role_id=$1 ORDER BY instrument_id`, role.ID)
		roles = append(roles, role)
	}
	writeJSON(w, 200, roles)
}

func (s *Server) updateRole(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeError(w, 400, "invalid role id")
		return
	}
	var body struct {
		Name           string   `json:"name"`
		AllInstruments bool     `json:"all_instruments"`
		Permissions    []string `json:"permissions"`
		Instruments    []string `json:"instruments"`
	}
	if err := decodeJSON(r, &body); err != nil {
		writeError(w, 400, err.Error())
		return
	}
	tx, err := s.db.Begin(r.Context())
	if err != nil {
		writeError(w, 500, "update role failed")
		return
	}
	defer tx.Rollback(r.Context())
	var code string
	if err := tx.QueryRow(r.Context(), `SELECT code FROM roles WHERE id=$1 FOR UPDATE`, id).Scan(&code); err != nil {
		writeError(w, 404, "role not found")
		return
	}
	if code == "super_admin" {
		writeError(w, 400, "super_admin cannot be modified")
		return
	}
	if _, err := tx.Exec(r.Context(), `UPDATE roles SET name=$1,all_instruments=$2 WHERE id=$3`, body.Name, body.AllInstruments, id); err != nil {
		writeError(w, 500, "update role failed")
		return
	}
	_, _ = tx.Exec(r.Context(), `DELETE FROM role_permissions WHERE role_id=$1`, id)
	_, _ = tx.Exec(r.Context(), `DELETE FROM role_instruments WHERE role_id=$1`, id)
	for _, permission := range body.Permissions {
		tag, err := tx.Exec(r.Context(), `INSERT INTO role_permissions(role_id,permission_code) SELECT $1,code FROM permissions WHERE code=$2`, id, permission)
		if err != nil || tag.RowsAffected() != 1 {
			writeError(w, 400, "unknown permission: "+permission)
			return
		}
	}
	if !body.AllInstruments {
		for _, instrument := range body.Instruments {
			if _, err := tx.Exec(r.Context(), `INSERT INTO role_instruments(role_id,instrument_id) VALUES($1,$2)`, id, instrument); err != nil {
				writeError(w, 400, "invalid instrument scope")
				return
			}
		}
	}
	// Sessions carry a permission snapshot for fast menu and scope checks.
	// Bump every affected user's authorization version in the same transaction
	// so a revoked permission cannot remain usable until the 12-hour TTL.
	if _, err := tx.Exec(r.Context(), `UPDATE users SET authorization_version=authorization_version+1,updated_at=now() WHERE id IN (SELECT user_id FROM user_roles WHERE role_id=$1)`, id); err != nil {
		writeError(w, 500, "invalidate role sessions failed")
		return
	}
	if err := tx.Commit(r.Context()); err != nil {
		writeError(w, 500, "update role failed")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) require(permission string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" && r.Method != "HEAD" {
			if origin := r.Header.Get("Origin"); origin != "" {
				parsed, err := url.Parse(origin)
				if err != nil || !strings.EqualFold(parsed.Host, r.Host) {
					writeError(w, http.StatusForbidden, "cross-origin request blocked")
					return
				}
			}
		}
		session, err := s.auth.Authenticate(r.Context(), bearerToken(r))
		if err != nil {
			writeError(w, http.StatusUnauthorized, "authentication required")
			return
		}
		if permission != "" && !session.Has(permission) {
			writeError(w, http.StatusForbidden, "permission denied")
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), sessionContextKey, session)))
	})
}

func enforceInstrumentScope(session auth.Session, cfg config.Config) error {
	if session.AllInstruments {
		return nil
	}
	for _, instrument := range cfg.Instruments {
		if !session.CanAccessInstrument(instrument.ID) {
			return fmt.Errorf("instrument %s is outside your data scope", instrument.ID)
		}
	}
	return nil
}

func scopeConfig(session auth.Session, cfg config.Config) config.Config {
	if session.AllInstruments {
		return cfg
	}
	allowed := map[string]bool{}
	for _, id := range session.Instruments {
		allowed[id] = true
	}
	filtered := cfg
	filtered.Instruments = nil
	for _, in := range cfg.Instruments {
		if allowed[in.ID] {
			filtered.Instruments = append(filtered.Instruments, in)
		}
	}
	filtered.Venues = map[string]config.VenueConfig{}
	for name, v := range cfg.Venues {
		copyVenue := v
		copyVenue.Markets = map[string]config.VenueMarketConfig{}
		for id, m := range v.Markets {
			if allowed[id] {
				copyVenue.Markets[id] = m
			}
		}
		filtered.Venues[name] = copyVenue
	}
	return filtered
}

func sessionFromContext(ctx context.Context) auth.Session {
	value, _ := ctx.Value(sessionContextKey).(auth.Session)
	return value
}
func bearerToken(r *http.Request) string {
	parts := strings.Fields(r.Header.Get("Authorization"))
	if len(parts) == 2 && strings.EqualFold(parts[0], "Bearer") {
		return parts[1]
	}
	if cookie, err := r.Cookie("fluxmaker_session"); err == nil {
		return cookie.Value
	}
	return ""
}

func decodeJSON(r *http.Request, target any) error {
	decoder := json.NewDecoder(io.LimitReader(r.Body, 2<<20))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return fmt.Errorf("invalid JSON: %w", err)
	}
	return nil
}
func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}
func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Cache-Control", "no-store")
		next.ServeHTTP(w, r)
	})
}

type stringListQueryer interface {
	Query(context.Context, string, ...any) (pgx.Rows, error)
}

func queryStringList(ctx context.Context, q stringListQueryer, query string, args ...any) ([]string, error) {
	rows, err := q.Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var values []string
	for rows.Next() {
		var value string
		if err := rows.Scan(&value); err != nil {
			return nil, err
		}
		values = append(values, value)
	}
	return values, rows.Err()
}
