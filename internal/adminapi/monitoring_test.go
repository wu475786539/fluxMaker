package adminapi

import (
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"fluxmaker/internal/auth"
	"fluxmaker/internal/config"
	"fluxmaker/internal/domain"
	"fluxmaker/internal/runtimeops"
)

func TestMonitoringSummaryHealthyAndCriticalStates(t *testing.T) {
	now := time.Now().UTC()
	cfg := config.Config{Mode: domain.ModeShadow, PollIntervalMS: 2000, TradingProgressTimeoutSeconds: 120}
	engine := runtimeops.EngineStatus{Online: true, Ready: true, LastHeartbeat: now, LastTradingProgress: now, Performance: &runtimeops.CyclePerformance{StartedAt: now, DurationMS: 100, Instruments: 1, Succeeded: 1}}
	snapshots := []runtimeops.InstrumentSnapshot{{InstrumentID: "token_usdt", Status: "running", UpdatedAt: now, Venues: []runtimeops.VenueSnapshot{{Name: "binance", MarketConnected: true}}}}
	summary := BuildMonitoringSummary(cfg, engine, snapshots, now)
	if summary.Status != "healthy" || len(summary.Alerts) != 0 {
		t.Fatalf("unexpected healthy summary: %+v", summary)
	}

	cfg.Mode = domain.ModeLive
	snapshots[0].Venues[0].TradingEnabled = true
	snapshots[0].Venues[0].AccountConnected = false
	summary = BuildMonitoringSummary(cfg, engine, snapshots, now)
	if summary.Status != "critical" || summary.Critical == 0 {
		t.Fatalf("expected critical account alert: %+v", summary)
	}
}

func TestPrometheusRenderingIncludesEngineAndVenueMetrics(t *testing.T) {
	now := time.Now().UTC()
	engine := runtimeops.EngineStatus{Online: true, Ready: true, Version: 9, LastHeartbeat: now, LastTradingProgress: now, Metrics: &runtimeops.MetricsSnapshot{CyclesTotal: 12, OMSPlacedTotal: 7, RuleChangesTotal: 2, LeaseFenceRejectsTotal: 1}, RuleChanges: []runtimeops.RuleChange{{InstrumentID: "token_usdt", Venue: "binance", DetectedAt: now}}}
	instruments := []runtimeops.InstrumentSnapshot{{InstrumentID: `token_"usdt`, Status: "running", TickDurationMS: 250, Venues: []runtimeops.VenueSnapshot{{Name: "binance", MarketConnected: true, AccountConnected: true, OpenOrders: make([]domain.Order, 3)}}}}
	output := RenderPrometheus(engine, instruments, MonitoringSummary{Status: "healthy"}, now)
	for _, expected := range []string{"fluxmaker_engine_up 1", "fluxmaker_cycles_total 12", "fluxmaker_oms_placed_orders_total 7", "fluxmaker_rule_changes_total 2", "fluxmaker_lease_fence_rejections_total 1", "fluxmaker_recent_rule_changes 1", "fluxmaker_venue_open_orders", `instrument="token_\"usdt"`} {
		if !strings.Contains(output, expected) {
			t.Fatalf("missing %q in metrics:\n%s", expected, output)
		}
	}
}

func TestRecentTradingRuleChangeCreatesWarning(t *testing.T) {
	now := time.Now().UTC()
	engine := runtimeops.EngineStatus{Online: true, Ready: true, LastHeartbeat: now, LastTradingProgress: now, RuleChanges: []runtimeops.RuleChange{{InstrumentID: "token_usdt", Venue: "binance", DetectedAt: now.Add(-time.Minute)}}}
	summary := BuildMonitoringSummary(config.Config{Mode: domain.ModeShadow}, engine, nil, now)
	if summary.Status != "degraded" || summary.Warnings != 1 || summary.Alerts[0].Code != "trading_rules_changed" {
		t.Fatalf("unexpected rule change alert: %+v", summary)
	}
}

func TestShadowModeIgnoresStaleWatchdogStatus(t *testing.T) {
	now := time.Now().UTC()
	engine := runtimeops.EngineStatus{Online: true, Ready: true, LastHeartbeat: now, LastTradingProgress: now, Watchdog: &runtimeops.WatchdogStatus{Healthy: true, LastCheckAt: now.Add(-time.Hour)}}
	summary := BuildMonitoringSummary(config.Config{Mode: domain.ModeShadow}, engine, nil, now)
	if summary.Status != "healthy" {
		t.Fatalf("shadow mode treated inactive watchdog as unhealthy: %+v", summary)
	}
}

func TestRuleChangesRespectInstrumentScope(t *testing.T) {
	engine := runtimeops.EngineStatus{RuleChanges: []runtimeops.RuleChange{{InstrumentID: "allowed"}, {InstrumentID: "hidden"}}}
	session := auth.Session{Instruments: []string{"allowed"}}
	filtered := filterEngineRuleChanges(engine, &session)
	if len(filtered.RuleChanges) != 1 || filtered.RuleChanges[0].InstrumentID != "allowed" {
		t.Fatalf("rule changes escaped scope: %+v", filtered.RuleChanges)
	}
}

func TestMetricsTokenIsRequiredAndSupportsBearer(t *testing.T) {
	request := httptest.NewRequest("GET", "/metrics", nil)
	if validMetricsToken(request, "secret") {
		t.Fatal("missing token was accepted")
	}
	request.Header.Set("Authorization", "Bearer secret")
	if !validMetricsToken(request, "secret") {
		t.Fatal("valid bearer token was rejected")
	}
	if validMetricsToken(request, "") {
		t.Fatal("empty configured token must disable metrics")
	}
}
