package configdiff

import (
	"encoding/json"
	"sort"

	"fluxmaker/internal/config"
	"fluxmaker/internal/domain"
)

type MarketTarget struct {
	InstrumentID string `json:"instrument_id"`
	Venue        string `json:"venue"`
	Symbol       string `json:"symbol"`
	Reason       string `json:"reason"`
}

type InstrumentChange struct {
	InstrumentID string   `json:"instrument_id"`
	Action       string   `json:"action"`
	Reasons      []string `json:"reasons"`
}

type Plan struct {
	FirstPublish bool `json:"first_publish"`
	CancelAll    bool `json:"cancel_all"`
	// Structural is true when applying this change requires rebuilding runtime
	// wiring (venue clients, oracle, audit logger) or canceling orders — i.e. the
	// slow candidate+preflight+swap path. When false the change only touches
	// per-instrument strategy/simulation parameters or live scalar knobs, which
	// the running engine can hot-apply in place between cycles.
	Structural           bool               `json:"structural"`
	HotChanges           []string           `json:"hot_changes"`
	InstrumentChanges    []InstrumentChange `json:"instrument_changes"`
	CancelTargets        []MarketTarget     `json:"cancel_targets"`
	AffectedInstruments  int                `json:"affected_instruments"`
	UnchangedInstruments int                `json:"unchanged_instruments"`
}

func Build(previous *config.Config, next config.Config) Plan {
	plan := Plan{FirstPublish: previous == nil, HotChanges: []string{}, InstrumentChanges: []InstrumentChange{}, CancelTargets: []MarketTarget{}}
	if previous == nil {
		plan.Structural = true
		for _, instrument := range next.Instruments {
			plan.InstrumentChanges = append(plan.InstrumentChanges, InstrumentChange{InstrumentID: instrument.ID, Action: "add", Reasons: []string{"新增币对"}})
		}
		finish(&plan, next)
		return plan
	}

	old := *previous
	changes := map[string]*InstrumentChange{}
	addChange := func(instrumentID, action, reason string) {
		item := changes[instrumentID]
		if item == nil {
			item = &InstrumentChange{InstrumentID: instrumentID, Action: action, Reasons: []string{}}
			changes[instrumentID] = item
		}
		if actionPriority(action) > actionPriority(item.Action) {
			item.Action = action
		}
		if reason != "" && !contains(item.Reasons, reason) {
			item.Reasons = append(item.Reasons, reason)
		}
	}

	if old.PollIntervalMS != next.PollIntervalMS || old.MaxConcurrentInstruments != next.MaxConcurrentInstruments || old.RulesRefreshSeconds != next.RulesRefreshSeconds {
		plan.HotChanges = append(plan.HotChanges, "轮询、币对并发与交易规则刷新")
	}
	if old.MarketFailureThreshold != next.MarketFailureThreshold || old.MarketRecoveryThreshold != next.MarketRecoveryThreshold || old.MarketErrorGraceSeconds != next.MarketErrorGraceSeconds || old.TradingProgressTimeoutSeconds != next.TradingProgressTimeoutSeconds {
		plan.HotChanges = append(plan.HotChanges, "市场故障宽限与恢复阈值")
	}
	if old.AuditPath != next.AuditPath || old.AuditMaxBytes != next.AuditMaxBytes || old.AuditBackups != next.AuditBackups {
		plan.HotChanges = append(plan.HotChanges, "审计文件与轮转")
		plan.Structural = true // audit logger is wired at runtime construction
	}
	if old.HeartbeatPath != next.HeartbeatPath || old.WatchdogTimeoutSeconds != next.WatchdogTimeoutSeconds {
		plan.HotChanges = append(plan.HotChanges, "Watchdog 与心跳参数")
		plan.Structural = true
	}
	if !equalJSON(old.RPC, next.RPC) {
		plan.HotChanges = append(plan.HotChanges, "BNB Chain RPC（验证后热切换）")
		plan.Structural = true // needs a revalidated RPC client / oracle
		for _, instrument := range next.Instruments {
			addChange(instrument.ID, "hot_reload", "价格源连接发生变化")
		}
	}
	if old.Mode != next.Mode {
		plan.HotChanges = append(plan.HotChanges, "运行模式")
		plan.Structural = true
		for _, instrument := range next.Instruments {
			addChange(instrument.ID, "reconfigure", "运行模式发生变化")
		}
		if old.Mode == domain.ModeLive && next.Mode == domain.ModeShadow {
			plan.CancelAll = true
		}
	}

	oldInstruments := instrumentMap(old.Instruments)
	newInstruments := instrumentMap(next.Instruments)
	for id, oldInstrument := range oldInstruments {
		newInstrument, exists := newInstruments[id]
		if !exists {
			addChange(id, "remove", "删除币对")
			addInstrumentMarkets(&plan, old, id, "删除币对")
			continue
		}
		if !equalJSON(oldInstrument.Base, newInstrument.Base) || !equalJSON(oldInstrument.Quote, newInstrument.Quote) {
			addChange(id, "reconfigure", "Token 信息发生变化")
		}
		if !equalJSON(oldInstrument.Reference, newInstrument.Reference) {
			addChange(id, "reconfigure", "Pancake 价格路径发生变化")
		}
		if !equalJSON(oldInstrument.Strategy, newInstrument.Strategy) {
			addChange(id, "reconcile", "策略或库存参数发生变化")
		}
		if !equalJSON(oldInstrument.TradeSimulation, newInstrument.TradeSimulation) {
			addChange(id, "hot_reload", "内部成交模拟参数发生变化")
		}
	}
	for id := range newInstruments {
		if _, exists := oldInstruments[id]; !exists {
			addChange(id, "add", "新增币对")
		}
	}

	diffVenues(&plan, old, next, addChange)
	for _, change := range changes {
		sort.Strings(change.Reasons)
		plan.InstrumentChanges = append(plan.InstrumentChanges, *change)
	}
	finish(&plan, next)
	return plan
}

func diffVenues(plan *Plan, old, next config.Config, addChange func(string, string, string)) {
	for venueName, oldVenue := range old.Venues {
		newVenue, venueExists := next.Venues[venueName]
		connectionChanged := !venueExists || oldVenue.Type != newVenue.Type || oldVenue.Environment != newVenue.Environment || oldVenue.BaseURL != newVenue.BaseURL
		executionChanged := connectionChanged || oldVenue.SelfTradePrevention != newVenue.SelfTradePrevention || oldVenue.DedicatedAccount != newVenue.DedicatedAccount
		stopped := !venueExists || !newVenue.Enabled || !newVenue.TradingEnabled
		for instrumentID, oldMarket := range oldVenue.Markets {
			newMarket, marketExists := newVenue.Markets[instrumentID]
			mustCancel := old.Mode == domain.ModeLive && oldVenue.Enabled && oldVenue.TradingEnabled && (stopped || !marketExists || connectionChanged || oldMarket.Symbol != newMarket.Symbol || oldMarket.CredentialID != newMarket.CredentialID)
			if mustCancel {
				addTarget(plan, MarketTarget{InstrumentID: instrumentID, Venue: venueName, Symbol: oldMarket.Symbol, Reason: marketCancelReason(venueExists, newVenue, marketExists, connectionChanged, oldMarket, newMarket)})
			}
			if !venueExists || !marketExists {
				continue
			}
			if executionChanged || oldVenue.Enabled != newVenue.Enabled || oldVenue.TradingEnabled != newVenue.TradingEnabled || !equalJSON(oldMarket, newMarket) {
				addChange(instrumentID, "reconfigure", "交易市场 "+venueName+" 发生变化")
			}
		}
	}
	for venueName, newVenue := range next.Venues {
		oldVenue, venueExists := old.Venues[venueName]
		for instrumentID := range newVenue.Markets {
			if !venueExists {
				addChange(instrumentID, "reconfigure", "新增交易所 "+venueName)
				continue
			}
			if _, marketExists := oldVenue.Markets[instrumentID]; !marketExists {
				addChange(instrumentID, "reconfigure", "新增交易市场 "+venueName)
			}
		}
	}
}

func marketCancelReason(venueExists bool, newVenue config.VenueConfig, marketExists, connectionChanged bool, oldMarket, newMarket config.VenueMarketConfig) string {
	if !venueExists || !newVenue.Enabled {
		return "交易所被删除或停用"
	}
	if !newVenue.TradingEnabled {
		return "交易所关闭实盘"
	}
	if !marketExists {
		return "交易市场被删除"
	}
	if connectionChanged {
		return "交易所连接发生变化"
	}
	if oldMarket.Symbol != newMarket.Symbol {
		return "交易所 Symbol 发生变化"
	}
	if oldMarket.CredentialID != newMarket.CredentialID {
		return "交易凭证发生变化"
	}
	return "交易市场需要重建"
}

func addInstrumentMarkets(plan *Plan, cfg config.Config, instrumentID, reason string) {
	if cfg.Mode != domain.ModeLive {
		return
	}
	for venueName, venueCfg := range cfg.Venues {
		if !venueCfg.Enabled || !venueCfg.TradingEnabled {
			continue
		}
		if market, ok := venueCfg.Markets[instrumentID]; ok {
			addTarget(plan, MarketTarget{InstrumentID: instrumentID, Venue: venueName, Symbol: market.Symbol, Reason: reason})
		}
	}
}

func addTarget(plan *Plan, target MarketTarget) {
	for _, current := range plan.CancelTargets {
		if current.InstrumentID == target.InstrumentID && current.Venue == target.Venue && current.Symbol == target.Symbol {
			return
		}
	}
	plan.CancelTargets = append(plan.CancelTargets, target)
}

func finish(plan *Plan, next config.Config) {
	// Any order cancellation or a change beyond strategy/simulation parameters
	// (reconfigure/add/remove) requires the full rebuild path.
	if plan.CancelAll || len(plan.CancelTargets) > 0 {
		plan.Structural = true
	}
	for _, change := range plan.InstrumentChanges {
		if change.Action == "reconfigure" || change.Action == "add" || change.Action == "remove" {
			plan.Structural = true
			break
		}
	}
	sort.Strings(plan.HotChanges)
	sort.Slice(plan.InstrumentChanges, func(i, j int) bool {
		return plan.InstrumentChanges[i].InstrumentID < plan.InstrumentChanges[j].InstrumentID
	})
	sort.Slice(plan.CancelTargets, func(i, j int) bool {
		if plan.CancelTargets[i].InstrumentID == plan.CancelTargets[j].InstrumentID {
			return plan.CancelTargets[i].Venue < plan.CancelTargets[j].Venue
		}
		return plan.CancelTargets[i].InstrumentID < plan.CancelTargets[j].InstrumentID
	})
	plan.AffectedInstruments = len(plan.InstrumentChanges)
	affected := make(map[string]bool, len(plan.InstrumentChanges))
	for _, change := range plan.InstrumentChanges {
		affected[change.InstrumentID] = true
	}
	for _, instrument := range next.Instruments {
		if !affected[instrument.ID] {
			plan.UnchangedInstruments++
		}
	}
}

func instrumentMap(values []config.InstrumentConfig) map[string]config.InstrumentConfig {
	result := make(map[string]config.InstrumentConfig, len(values))
	for _, value := range values {
		result[value.ID] = value
	}
	return result
}

func actionPriority(action string) int {
	return map[string]int{"hot_reload": 1, "reconcile": 2, "reconfigure": 3, "add": 4, "remove": 5}[action]
}

func equalJSON(a, b any) bool {
	left, _ := json.Marshal(a)
	right, _ := json.Marshal(b)
	return string(left) == string(right)
}

func contains(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}
