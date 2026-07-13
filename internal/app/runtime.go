package app

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"sort"
	"strings"
	"sync"

	"fluxmaker/internal/audit"
	"fluxmaker/internal/config"
	"fluxmaker/internal/configdiff"
	"fluxmaker/internal/credentials"
	"fluxmaker/internal/domain"
	"fluxmaker/internal/engine"
	"fluxmaker/internal/oracle/pancakev2"
	"fluxmaker/internal/runtimeops"
	"fluxmaker/internal/venue"
)

type Runtime struct {
	Config config.Config
	Engine *engine.Engine
	Oracle *pancakev2.Oracle
}

func BuildRuntime(ctx context.Context, cfg config.Config, credentialService *credentials.Service, runtimeStore *runtimeops.Store, logger *slog.Logger) (*Runtime, error) {
	return BuildRuntimeCandidate(ctx, cfg, credentialService, runtimeStore, logger, nil)
}

func BuildRuntimeCandidate(ctx context.Context, cfg config.Config, credentialService *credentials.Service, runtimeStore *runtimeops.Store, logger *slog.Logger, previous *Runtime) (*Runtime, error) {
	applyRuntimeSafetyDefaults(&cfg)
	if err := cfg.ValidateRuntime(); err != nil {
		return nil, err
	}
	if cfg.Mode == domain.ModeLive && os.Getenv("FLUXMAKER_ENABLE_LIVE_TRADING") != "I_UNDERSTAND" {
		return nil, fmt.Errorf("live mode requires FLUXMAKER_ENABLE_LIVE_TRADING=I_UNDERSTAND")
	}
	clients, startupFailures, err := BuildVenuesIsolated(ctx, cfg, credentialService)
	if err != nil {
		return nil, err
	}
	mergeInstrumentFailures(startupFailures, syncMarketRulesIsolated(ctx, &cfg, clients))
	if err := cfg.ValidateRuntime(); err != nil {
		return nil, fmt.Errorf("validate synchronized trading rules: %w", err)
	}
	var oracle *pancakev2.Oracle
	if previous != nil && previous.Oracle != nil && equalConfigValue(previous.Config.RPC, cfg.RPC) {
		oracle = previous.Oracle
	} else {
		rpc := pancakev2.NewRPCClient(cfg.RPC.URLs, cfg.RequestTimeout())
		chainID, err := rpc.ChainID(ctx)
		if err != nil {
			return nil, fmt.Errorf("check rpc chain id: %w", err)
		}
		if chainID != cfg.RPC.ChainID {
			return nil, fmt.Errorf("rpc chain id %d does not match configured %d", chainID, cfg.RPC.ChainID)
		}
		oracle = pancakev2.New(rpc)
	}
	ownerID := ""
	if previous != nil && previous.Engine != nil {
		ownerID = previous.Engine.OwnerID()
	}
	engineRuntime := engine.NewWithOwner(cfg, oracle, clients, audit.NewWithRotation(cfg.AuditPath, cfg.AuditMaxBytes, cfg.AuditBackups), runtimeStore, logger, ownerID)
	engineRuntime.SetStartupFailures(flattenInstrumentFailures(startupFailures))
	if previous != nil {
		engineRuntime.InheritMetricsFrom(previous.Engine)
	}
	return &Runtime{Config: cfg, Engine: engineRuntime, Oracle: oracle}, nil
}

func applyRuntimeSafetyDefaults(cfg *config.Config) {
	if cfg.MarketFailureThreshold == 0 {
		cfg.MarketFailureThreshold = 3
	}
	if cfg.MarketRecoveryThreshold == 0 {
		cfg.MarketRecoveryThreshold = 3
	}
	if cfg.MarketErrorGraceSeconds == 0 {
		cfg.MarketErrorGraceSeconds = 15
	}
	if cfg.TradingProgressTimeoutSeconds == 0 {
		cfg.TradingProgressTimeoutSeconds = 120
	}
	if cfg.MaxConcurrentInstruments == 0 {
		cfg.MaxConcurrentInstruments = 4
	}
	if cfg.AuditMaxBytes == 0 {
		cfg.AuditMaxBytes = 100 * 1024 * 1024
	}
	if cfg.AuditBackups == 0 {
		cfg.AuditBackups = 7
	}
	if cfg.RulesRefreshSeconds == 0 {
		cfg.RulesRefreshSeconds = 300
	}
	for i := range cfg.Instruments {
		if cfg.Instruments[i].Strategy.MaxVenueReferenceDeviationBPS == 0 {
			cfg.Instruments[i].Strategy.MaxVenueReferenceDeviationBPS = 500
		}
		if cfg.Instruments[i].Strategy.MaxVenueSpreadBPS == 0 {
			cfg.Instruments[i].Strategy.MaxVenueSpreadBPS = 1000
		}
	}
}

type marketRuleResult struct {
	venueName    string
	instrumentID string
	market       config.VenueMarketConfig
	err          error
}

func syncMarketRulesIsolated(ctx context.Context, cfg *config.Config, clients map[string]venue.Client) map[string][]string {
	resultCount := 0
	for _, venueCfg := range cfg.Venues {
		if venueCfg.Enabled {
			resultCount += len(venueCfg.Markets)
		}
	}
	results := make(chan marketRuleResult, resultCount)
	var workers sync.WaitGroup
	for venueName, venueCfg := range cfg.Venues {
		if !venueCfg.Enabled {
			continue
		}
		for instrumentID, market := range venueCfg.Markets {
			venueName, instrumentID, market := venueName, instrumentID, market
			client := clients[venue.ClientKey(venueName, instrumentID)]
			if client == nil {
				results <- marketRuleResult{venueName: venueName, instrumentID: instrumentID, market: market, err: fmt.Errorf("client unavailable")}
				continue
			}
			workers.Add(1)
			go func() {
				defer workers.Done()
				reader, ok := client.(venue.RuleReader)
				if !ok {
					results <- marketRuleResult{venueName: venueName, instrumentID: instrumentID, market: market, err: fmt.Errorf("trading rules unavailable")}
					return
				}
				requestCtx, cancel := context.WithTimeout(ctx, cfg.RequestTimeout())
				defer cancel()
				rules, err := reader.MarketRules(requestCtx, market.Symbol)
				if err != nil {
					results <- marketRuleResult{venueName: venueName, instrumentID: instrumentID, market: market, err: fmt.Errorf("load trading rules: %w", err)}
					return
				}
				if rules.BaseAsset != "" && !strings.EqualFold(rules.BaseAsset, market.BaseAsset) {
					results <- marketRuleResult{venueName: venueName, instrumentID: instrumentID, market: market, err: fmt.Errorf("base asset %s does not match exchange %s", market.BaseAsset, rules.BaseAsset)}
					return
				}
				if rules.QuoteAsset != "" && !strings.EqualFold(rules.QuoteAsset, market.QuoteAsset) {
					results <- marketRuleResult{venueName: venueName, instrumentID: instrumentID, market: market, err: fmt.Errorf("quote asset %s does not match exchange %s", market.QuoteAsset, rules.QuoteAsset)}
					return
				}
				if rules.PriceTick.IsPositive() {
					market.PriceTick = rules.PriceTick
				}
				if rules.QuantityStep.IsPositive() {
					market.QuantityStep = rules.QuantityStep
				}
				if rules.MinNotional.IsPositive() {
					market.MinNotional = rules.MinNotional
				}
				market.MinQuantity = rules.MinQuantity
				market.MaxQuantity = rules.MaxQuantity
				market.MaxNotional = rules.MaxNotional
				market.MinPrice = rules.MinPrice
				market.MaxPrice = rules.MaxPrice
				market.MaxOpenOrders = rules.MaxOpenOrders
				results <- marketRuleResult{venueName: venueName, instrumentID: instrumentID, market: market}
			}()
		}
	}
	workers.Wait()
	close(results)

	failures := make(map[string][]string)
	for result := range results {
		if result.err != nil {
			failures[result.instrumentID] = append(failures[result.instrumentID], result.venueName+": "+result.err.Error())
			continue
		}
		venueCfg := cfg.Venues[result.venueName]
		venueCfg.Markets[result.instrumentID] = result.market
		cfg.Venues[result.venueName] = venueCfg
	}
	return failures
}

func mergeInstrumentFailures(target, source map[string][]string) {
	for instrumentID, failures := range source {
		target[instrumentID] = append(target[instrumentID], failures...)
	}
}

func flattenInstrumentFailures(values map[string][]string) map[string]string {
	result := make(map[string]string, len(values))
	for instrumentID, failures := range values {
		sort.Strings(failures)
		result[instrumentID] = strings.Join(failures, "; ")
	}
	return result
}

func (r *Runtime) Prepare(ctx context.Context) error {
	if r == nil || r.Engine == nil {
		return fmt.Errorf("runtime is unavailable")
	}
	return r.Engine.Prepare(ctx)
}

func (r *Runtime) RefreshMarketRules(ctx context.Context) (int, error) {
	if r == nil || r.Engine == nil {
		return 0, fmt.Errorf("runtime is unavailable")
	}
	changes, err := r.Engine.RefreshMarketRules(ctx)
	// Preserve every successfully applied per-market change even when another
	// venue failed to refresh during the same pass.
	r.Config = r.Engine.EffectiveConfig()
	return changes, err
}

func (r *Runtime) RetryBlocked(ctx context.Context) (int, error) {
	if r == nil || r.Engine == nil {
		return 0, fmt.Errorf("runtime is unavailable")
	}
	recovered, err := r.Engine.RetryBlocked(ctx)
	r.Config = r.Engine.EffectiveConfig()
	return recovered, err
}

func (r *Runtime) ApplyCleanup(ctx context.Context, plan configdiff.Plan) error {
	if r == nil || r.Engine == nil {
		return nil
	}
	if plan.CancelAll {
		return r.Engine.Shutdown(ctx)
	}
	var failures []string
	for _, target := range plan.CancelTargets {
		if err := r.Engine.CancelMarket(ctx, target.InstrumentID, target.Venue); err != nil {
			failures = append(failures, target.Venue+"/"+target.InstrumentID+": "+err.Error())
		}
	}
	if len(failures) > 0 {
		return fmt.Errorf("targeted cleanup failed: %s", strings.Join(failures, "; "))
	}
	return nil
}

func equalConfigValue(a, b any) bool {
	left, _ := json.Marshal(a)
	right, _ := json.Marshal(b)
	return string(left) == string(right)
}
