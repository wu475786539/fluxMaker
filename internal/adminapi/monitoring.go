package adminapi

import (
	"context"
	"crypto/subtle"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	"fluxmaker/internal/auth"
	"fluxmaker/internal/config"
	"fluxmaker/internal/domain"
	"fluxmaker/internal/runtimeops"
)

type MonitoringAlert struct {
	Severity     string    `json:"severity"`
	Code         string    `json:"code"`
	Message      string    `json:"message"`
	InstrumentID string    `json:"instrument_id,omitempty"`
	Venue        string    `json:"venue,omitempty"`
	Since        time.Time `json:"since,omitempty"`
}

type MonitoringSummary struct {
	Status      string            `json:"status"`
	GeneratedAt time.Time         `json:"generated_at"`
	Critical    int               `json:"critical"`
	Warnings    int               `json:"warnings"`
	Alerts      []MonitoringAlert `json:"alerts"`
}

func BuildMonitoringSummary(cfg config.Config, engine runtimeops.EngineStatus, instruments []runtimeops.InstrumentSnapshot, now time.Time) MonitoringSummary {
	summary := MonitoringSummary{Status: "healthy", GeneratedAt: now.UTC(), Alerts: []MonitoringAlert{}}
	add := func(alert MonitoringAlert) { summary.Alerts = append(summary.Alerts, alert) }
	if !engine.Online {
		add(MonitoringAlert{Severity: "critical", Code: "engine_offline", Message: "交易引擎心跳已离线", Since: engine.LastHeartbeat})
	} else if !engine.Ready {
		add(MonitoringAlert{Severity: "warning", Code: "engine_not_ready", Message: "交易引擎在线但运行配置未就绪", Since: engine.LastHeartbeat})
	}
	if engine.Error != "" {
		add(MonitoringAlert{Severity: "warning", Code: "engine_error", Message: engine.Error, Since: engine.LastHeartbeat})
	}
	progressTimeout := cfg.TradingProgressTimeoutSeconds
	if progressTimeout <= 0 {
		progressTimeout = 120
	}
	progressStale := !engine.LastTradingProgress.IsZero() && now.Sub(engine.LastTradingProgress) > time.Duration(progressTimeout)*time.Second
	if engine.LastTradingProgress.IsZero() && engine.Metrics != nil {
		progressStale = now.Sub(engine.Metrics.StartedAt) > time.Duration(progressTimeout)*time.Second
	}
	if engine.Online && engine.Ready && progressStale {
		add(MonitoringAlert{Severity: "critical", Code: "trading_progress_stale", Message: "交易循环长时间没有完成进度", Since: engine.LastTradingProgress})
	}
	if performance := engine.Performance; performance != nil {
		if performance.Failed > 0 {
			add(MonitoringAlert{Severity: "warning", Code: "cycle_failures", Message: fmt.Sprintf("最近一轮有 %d/%d 个币对失败", performance.Failed, performance.Instruments), Since: performance.StartedAt})
		}
		if cfg.PollIntervalMS > 0 && performance.DurationMS > int64(cfg.PollIntervalMS) {
			add(MonitoringAlert{Severity: "warning", Code: "cycle_slow", Message: fmt.Sprintf("最近一轮耗时 %dms，超过轮询间隔 %dms", performance.DurationMS, cfg.PollIntervalMS), Since: performance.StartedAt})
		}
	}
	if metrics := engine.Metrics; metrics != nil {
		if metrics.AuditPendingEvents > 0 {
			add(MonitoringAlert{Severity: "critical", Code: "audit_write_pending", Message: fmt.Sprintf("审计写入异常 %d 次，待写事件 %d 条", metrics.AuditFlushErrorsTotal, metrics.AuditPendingEvents), Since: metrics.UpdatedAt})
		}
	}
	for _, change := range engine.RuleChanges {
		if change.DetectedAt.IsZero() || now.Sub(change.DetectedAt) > time.Hour {
			continue
		}
		add(MonitoringAlert{Severity: "warning", Code: "trading_rules_changed", Message: "交易所交易规则发生变化，已按新规则热更新", InstrumentID: change.InstrumentID, Venue: change.Venue, Since: change.DetectedAt})
	}
	if watchdog := engine.Watchdog; watchdog != nil && cfg.Mode == domain.ModeLive {
		watchdogTimeout := cfg.WatchdogTimeoutSeconds
		if watchdogTimeout <= 0 {
			watchdogTimeout = 15
		}
		if now.Sub(watchdog.LastCheckAt) > time.Duration(watchdogTimeout*2)*time.Second {
			add(MonitoringAlert{Severity: "critical", Code: "watchdog_offline", Message: "Watchdog 长时间没有更新检查状态", Since: watchdog.LastCheckAt})
		} else if !watchdog.Healthy {
			message := "Watchdog 已触发保护撤单：" + watchdog.Reason
			if watchdog.CancelError != "" {
				message += "；撤单错误：" + watchdog.CancelError
			}
			add(MonitoringAlert{Severity: "critical", Code: "watchdog_triggered", Message: message, Since: watchdog.LastTriggeredAt})
		}
	} else if cfg.Mode == domain.ModeLive && engine.Online {
		add(MonitoringAlert{Severity: "warning", Code: "watchdog_unknown", Message: "尚未收到 Watchdog 检查状态"})
	}
	if engine.Online {
		for _, instrument := range instruments {
			if instrument.Status == "waiting" {
				add(MonitoringAlert{Severity: "warning", Code: "instrument_waiting", Message: "币对尚未产生运行快照", InstrumentID: instrument.InstrumentID})
			}
			if instrument.Status == "degraded" {
				add(MonitoringAlert{Severity: "warning", Code: "instrument_degraded", Message: "币对运行降级", InstrumentID: instrument.InstrumentID, Since: instrument.UpdatedAt})
			}
			for _, venue := range instrument.Venues {
				if !venue.MarketConnected {
					add(MonitoringAlert{Severity: "warning", Code: "market_disconnected", Message: "交易所行情连接异常", InstrumentID: instrument.InstrumentID, Venue: venue.Name, Since: venue.UpdatedAt})
				}
				if cfg.Mode == domain.ModeLive && venue.TradingEnabled && !venue.AccountConnected {
					add(MonitoringAlert{Severity: "critical", Code: "account_disconnected", Message: "实盘账户连接异常", InstrumentID: instrument.InstrumentID, Venue: venue.Name, Since: venue.UpdatedAt})
				}
				if venue.Fault != nil && venue.Fault.Status != "normal" {
					severity := "warning"
					if venue.Fault.Status == "canceling" || venue.Fault.Status == "paused" {
						severity = "critical"
					}
					add(MonitoringAlert{Severity: severity, Code: "venue_fault_" + venue.Fault.Status, Message: "交易市场故障状态：" + venue.Fault.Status, InstrumentID: instrument.InstrumentID, Venue: venue.Name, Since: venue.Fault.Since})
				}
			}
		}
	}
	sort.SliceStable(summary.Alerts, func(i, j int) bool {
		return severityRank(summary.Alerts[i].Severity) > severityRank(summary.Alerts[j].Severity)
	})
	for _, alert := range summary.Alerts {
		switch alert.Severity {
		case "critical":
			summary.Critical++
		case "warning":
			summary.Warnings++
		}
	}
	if summary.Critical > 0 {
		summary.Status = "critical"
	} else if summary.Warnings > 0 {
		summary.Status = "degraded"
	}
	return summary
}

func severityRank(value string) int {
	if value == "critical" {
		return 2
	}
	if value == "warning" {
		return 1
	}
	return 0
}

func (s *Server) monitoring(w http.ResponseWriter, r *http.Request) {
	session := sessionFromContext(r.Context())
	cfg, engine, snapshots := s.monitoringData(r.Context(), &session)
	writeJSON(w, http.StatusOK, BuildMonitoringSummary(cfg, engine, snapshots, time.Now().UTC()))
}

func (s *Server) monitoringData(ctx context.Context, session *auth.Session) (config.Config, runtimeops.EngineStatus, []runtimeops.InstrumentSnapshot) {
	engine := filterEngineRuleChanges(s.runtime.EngineStatus(ctx), session)
	cfg, err := s.runtimeConfig(ctx)
	if err != nil {
		return config.Config{}, engine, []runtimeops.InstrumentSnapshot{}
	}
	snapshots := make([]runtimeops.InstrumentSnapshot, 0, len(cfg.Instruments))
	for _, instrument := range cfg.Instruments {
		if session != nil && !session.CanAccessInstrument(instrument.ID) {
			continue
		}
		snapshot, err := s.runtime.Get(ctx, instrument.ID)
		if err != nil {
			snapshot = runtimeops.InstrumentSnapshot{InstrumentID: instrument.ID, BaseSymbol: instrument.Base.Symbol, QuoteSymbol: instrument.Quote.Symbol, Mode: cfg.Mode, Status: "waiting", Venues: []runtimeops.VenueSnapshot{}}
		}
		snapshots = append(snapshots, snapshot)
	}
	return cfg, engine, snapshots
}

func filterEngineRuleChanges(engine runtimeops.EngineStatus, session *auth.Session) runtimeops.EngineStatus {
	if session == nil || session.AllInstruments {
		return engine
	}
	filtered := make([]runtimeops.RuleChange, 0, len(engine.RuleChanges))
	for _, change := range engine.RuleChanges {
		if session.CanAccessInstrument(change.InstrumentID) {
			filtered = append(filtered, change)
		}
	}
	engine.RuleChanges = filtered
	return engine
}

func (s *Server) prometheusMetrics(w http.ResponseWriter, r *http.Request) {
	if !validMetricsToken(r, s.metricsToken) {
		http.NotFound(w, r)
		return
	}
	cfg, engine, snapshots := s.monitoringData(r.Context(), nil)
	summary := BuildMonitoringSummary(cfg, engine, snapshots, time.Now().UTC())
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	_, _ = w.Write([]byte(RenderPrometheus(engine, snapshots, summary, time.Now().UTC())))
}

func validMetricsToken(r *http.Request, expected string) bool {
	if expected == "" {
		return false
	}
	provided := strings.TrimSpace(r.Header.Get("X-Metrics-Token"))
	if authorization := r.Header.Get("Authorization"); strings.HasPrefix(authorization, "Bearer ") {
		provided = strings.TrimSpace(strings.TrimPrefix(authorization, "Bearer "))
	}
	return len(provided) == len(expected) && subtle.ConstantTimeCompare([]byte(provided), []byte(expected)) == 1
}

func RenderPrometheus(engine runtimeops.EngineStatus, instruments []runtimeops.InstrumentSnapshot, summary MonitoringSummary, now time.Time) string {
	var output strings.Builder
	metric := func(name, help, metricType string, value any) {
		fmt.Fprintf(&output, "# HELP %s %s\n# TYPE %s %s\n%s %v\n", name, help, name, metricType, name, value)
	}
	metric("fluxmaker_engine_up", "Whether the trading engine heartbeat is online.", "gauge", boolFloat(engine.Online))
	metric("fluxmaker_engine_ready", "Whether the trading engine has an applied runtime.", "gauge", boolFloat(engine.Ready))
	metric("fluxmaker_engine_version", "Currently applied configuration version.", "gauge", engine.Version)
	metric("fluxmaker_engine_heartbeat_age_seconds", "Age of the latest engine heartbeat.", "gauge", ageSeconds(now, engine.LastHeartbeat))
	metric("fluxmaker_trading_progress_age_seconds", "Age of the latest completed instrument progress.", "gauge", ageSeconds(now, engine.LastTradingProgress))
	if performance := engine.Performance; performance != nil {
		metric("fluxmaker_cycle_duration_seconds", "Duration of the latest trading cycle.", "gauge", float64(performance.DurationMS)/1000)
		metric("fluxmaker_cycle_failed_instruments", "Failed instruments in the latest trading cycle.", "gauge", performance.Failed)
	}
	if metrics := engine.Metrics; metrics != nil {
		metric("fluxmaker_cycles_total", "Trading cycles since engine start.", "counter", metrics.CyclesTotal)
		metric("fluxmaker_cycle_failures_total", "Trading cycles with at least one failure.", "counter", metrics.CycleFailuresTotal)
		metric("fluxmaker_instrument_runs_total", "Instrument executions since engine start.", "counter", metrics.InstrumentRunsTotal)
		metric("fluxmaker_instrument_failures_total", "Failed instrument executions since engine start.", "counter", metrics.InstrumentFailuresTotal)
		metric("fluxmaker_venue_fault_events_total", "Venue fault observations since engine start.", "counter", metrics.VenueFaultEventsTotal)
		metric("fluxmaker_oms_placed_orders_total", "Orders accepted by OMS since engine start.", "counter", metrics.OMSPlacedTotal)
		metric("fluxmaker_oms_canceled_orders_total", "Orders submitted for cancellation by OMS since engine start.", "counter", metrics.OMSCanceledTotal)
		metric("fluxmaker_simulated_trades_total", "Internal simulated trade events since engine start.", "counter", metrics.SimulatedTradesTotal)
		metric("fluxmaker_audit_flush_errors_total", "Audit flush failures since engine start.", "counter", metrics.AuditFlushErrorsTotal)
		metric("fluxmaker_audit_pending_events", "Audit events waiting for durable flush.", "gauge", metrics.AuditPendingEvents)
		metric("fluxmaker_rule_changes_total", "Trading rule changes detected since engine start.", "counter", metrics.RuleChangesTotal)
		metric("fluxmaker_lease_fence_rejections_total", "Exchange writes rejected because the market lease was stale or unverifiable.", "counter", metrics.LeaseFenceRejectsTotal)
	}
	metric("fluxmaker_recent_rule_changes", "Trading rule changes retained in the runtime alert window.", "gauge", len(engine.RuleChanges))
	metric("fluxmaker_monitoring_critical_alerts", "Current critical monitoring alerts.", "gauge", summary.Critical)
	metric("fluxmaker_monitoring_warning_alerts", "Current warning monitoring alerts.", "gauge", summary.Warnings)
	if watchdog := engine.Watchdog; watchdog != nil {
		metric("fluxmaker_watchdog_healthy", "Whether the independent watchdog reports healthy liveness.", "gauge", boolFloat(watchdog.Healthy))
		metric("fluxmaker_watchdog_last_check_age_seconds", "Age of the latest watchdog check.", "gauge", ageSeconds(now, watchdog.LastCheckAt))
		metric("fluxmaker_watchdog_last_trigger_age_seconds", "Age of the latest watchdog protection action.", "gauge", ageSeconds(now, watchdog.LastTriggeredAt))
	}
	for _, instrument := range instruments {
		labels := `instrument="` + prometheusEscape(instrument.InstrumentID) + `"`
		fmt.Fprintf(&output, "fluxmaker_instrument_running{%s} %v\n", labels, boolFloat(instrument.Status == "running"))
		fmt.Fprintf(&output, "fluxmaker_instrument_tick_duration_seconds{%s} %v\n", labels, float64(instrument.TickDurationMS)/1000)
		fmt.Fprintf(&output, "fluxmaker_instrument_reference_duration_seconds{%s} %v\n", labels, float64(instrument.ReferenceDurationMS)/1000)
		fmt.Fprintf(&output, "fluxmaker_instrument_balance_duration_seconds{%s} %v\n", labels, float64(instrument.BalanceDurationMS)/1000)
		for _, venue := range instrument.Venues {
			venueLabels := labels + `,venue="` + prometheusEscape(venue.Name) + `"`
			fmt.Fprintf(&output, "fluxmaker_venue_market_connected{%s} %v\n", venueLabels, boolFloat(venue.MarketConnected))
			fmt.Fprintf(&output, "fluxmaker_venue_account_connected{%s} %v\n", venueLabels, boolFloat(venue.AccountConnected))
			fmt.Fprintf(&output, "fluxmaker_venue_open_orders{%s} %d\n", venueLabels, len(venue.OpenOrders))
			fmt.Fprintf(&output, "fluxmaker_venue_pending_orders{%s} %d\n", venueLabels, venue.PendingOrders)
			fmt.Fprintf(&output, "fluxmaker_venue_book_duration_seconds{%s} %v\n", venueLabels, float64(venue.BookDurationMS)/1000)
			fmt.Fprintf(&output, "fluxmaker_venue_orders_duration_seconds{%s} %v\n", venueLabels, float64(venue.OrdersDurationMS)/1000)
			fmt.Fprintf(&output, "fluxmaker_venue_fills_duration_seconds{%s} %v\n", venueLabels, float64(venue.FillsDurationMS)/1000)
			fmt.Fprintf(&output, "fluxmaker_venue_oms_duration_seconds{%s} %v\n", venueLabels, float64(venue.OMSDurationMS)/1000)
		}
	}
	return output.String()
}

func boolFloat(value bool) int {
	if value {
		return 1
	}
	return 0
}
func ageSeconds(now, value time.Time) float64 {
	if value.IsZero() {
		return -1
	}
	return max(0, now.Sub(value).Seconds())
}
func prometheusEscape(value string) string {
	return strings.NewReplacer("\\", "\\\\", "\n", "\\n", "\"", "\\\"").Replace(value)
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (w *statusRecorder) WriteHeader(status int) {
	w.status = status
	w.ResponseWriter.WriteHeader(status)
}

func requestLogging(logger *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		startedAt := time.Now()
		requestID := fmt.Sprintf("req-%d-%d", startedAt.UnixMilli(), requestSequence.Add(1))
		w.Header().Set("X-Request-ID", requestID)
		recorder := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(recorder, r)
		duration := time.Since(startedAt)
		attributes := []any{"request_id", requestID, "method", r.Method, "path", r.URL.Path, "status", recorder.status, "duration_ms", duration.Milliseconds(), "remote", r.RemoteAddr}
		if recorder.status >= 500 {
			logger.Error("http request", attributes...)
		} else if recorder.status >= 400 || r.Method != http.MethodGet || duration > time.Second {
			logger.Info("http request", attributes...)
		} else if r.URL.Path != "/healthz" && r.URL.Path != "/readyz" && r.URL.Path != "/livez" {
			logger.Debug("http request", attributes...)
		}
	})
}

var requestSequence atomic.Uint64
