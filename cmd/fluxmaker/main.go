package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"fluxmaker/internal/app"
	"fluxmaker/internal/config"
	"fluxmaker/internal/configdiff"
	"fluxmaker/internal/configstore"
	"fluxmaker/internal/credentials"
	"fluxmaker/internal/database"
	"fluxmaker/internal/domain"
	"fluxmaker/internal/heartbeat"
	projectlogging "fluxmaker/internal/logging"
	"fluxmaker/internal/runtimeops"
)

func main() {
	once := flag.Bool("once", false, "run one shadow tick and exit")
	flag.Parse()
	logger := projectlogging.New("fluxmaker")
	if err := run(*once, logger); err != nil {
		logger.Error("fluxmaker stopped", "error", err)
		os.Exit(1)
	}
}

func run(once bool, logger *slog.Logger) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	connectCtx, cancelConnect := context.WithTimeout(ctx, 15*time.Second)
	connections, err := database.OpenFromEnv(connectCtx, os.Getenv)
	cancelConnect()
	if err != nil {
		return err
	}
	defer connections.Close()
	store := configstore.New(connections.Postgres, connections.Redis)
	runtimeStore := runtimeops.New(connections.Redis)
	credentialService, err := credentials.NewService(connections.Postgres, os.Getenv("CREDENTIAL_MASTER_KEY"))
	if err != nil {
		return fmt.Errorf("initialize credential encryption: %w", err)
	}
	if once {
		snapshot, err := store.LoadActive(ctx)
		if err != nil {
			return err
		}
		if snapshot.Config.Mode == domain.ModeLive {
			return fmt.Errorf("-once is disabled in live mode")
		}
		buildCtx, cancel := context.WithTimeout(ctx, snapshot.Config.RequestTimeout())
		defer cancel()
		runtime, err := app.BuildRuntime(buildCtx, snapshot.Config, credentialService, runtimeStore, logger)
		if err != nil {
			return err
		}
		err = runtime.Engine.RunOnce(ctx)
		_ = heartbeat.Touch(runtime.Config.HeartbeatPath)
		return err
	}

	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	var runtime *app.Runtime
	var activeVersion int64
	var desiredVersion int64
	// activeRawConfig is the last applied configuration as published, before any
	// exchange rule sync. Structural classification compares raw-to-raw so that
	// runtime-synced venue rules do not masquerade as user changes.
	var activeRawConfig config.Config
	var pendingRuntime *app.Runtime
	var pendingVersion int64
	var pendingPlan configdiff.Plan
	var runtimeError = "waiting for a published configuration"
	var nextRun, nextReload, nextControl, nextRulesRefresh, nextBlockedRetry time.Time
	heartbeatPublisher := heartbeat.NewPublisher(runtimeStore)
	updateHeartbeat := func() {
		path := ""
		if runtime != nil {
			path = runtime.Config.HeartbeatPath
		}
		heartbeatPublisher.Update(heartbeat.PublisherState{Path: path, Version: activeVersion, DesiredVersion: desiredVersion, Ready: runtime != nil, Error: runtimeError})
	}
	updateHeartbeat()
	go heartbeatPublisher.Run(ctx, 2*time.Second, func(err error) {
		logger.Error("runtime heartbeat failed", "error", err)
	})
	for {
		now := time.Now()
		if !now.Before(nextReload) {
			nextReload = now.Add(2 * time.Second)
			snapshot, loadErr := store.LoadActive(ctx)
			if loadErr != nil {
				if !errors.Is(loadErr, configstore.ErrNoActive) {
					logger.Error("load active configuration failed", "error", loadErr)
					runtimeError = "load active configuration: " + loadErr.Error()
				} else if runtime == nil {
					runtimeError = "waiting for a published configuration"
				}
				if runtime == nil {
					logger.Info("waiting for a published configuration")
				}
			} else if snapshot.Version != activeVersion {
				desiredVersion = snapshot.Version
				updateHeartbeat()
				hotPlan := configdiff.Build(&activeRawConfig, snapshot.Config)
				if runtime != nil && !hotPlan.Structural {
					// Only strategy/simulation/scalar tuning changed: hot-apply it
					// in place. No rebuild, no rule re-sync, no preflight, so nothing
					// unrelated (a warming TWAP, a slow venue) can block it.
					runtime.Engine.ApplyParameters(snapshot.Config)
					runtime.Config = runtime.Engine.EffectiveConfig()
					activeVersion = snapshot.Version
					activeRawConfig = snapshot.Config
					pendingRuntime = nil
					pendingVersion = 0
					runtimeError = ""
					nextRun = time.Time{}
					if err := runtimeStore.SetAppliedVersion(ctx, activeVersion); err != nil {
						logger.Error("persist applied runtime version failed", "version", activeVersion, "error", err)
					}
					updateHeartbeat()
					logger.Info("parameters applied in place without rebuild", "version", snapshot.Version, "affected_instruments", hotPlan.AffectedInstruments)
				} else {
					logger.Info("applying published configuration", "version", snapshot.Version)
					if pendingVersion != snapshot.Version || pendingRuntime == nil {
						buildCtx, cancel := context.WithTimeout(ctx, maxDuration(snapshot.Config.RequestTimeout()*2, 15*time.Second))
						newRuntime, buildErr := app.BuildRuntimeCandidate(buildCtx, snapshot.Config, credentialService, runtimeStore, logger, runtime)
						cancel()
						if buildErr != nil {
							pendingRuntime = nil
							pendingVersion = 0
							logger.Error("published configuration rejected before switch", "version", snapshot.Version, "error", buildErr)
							runtimeError = "version v" + fmt.Sprint(snapshot.Version) + " preparation failed: " + buildErr.Error()
						} else {
							pendingRuntime = newRuntime
							pendingVersion = snapshot.Version
							if runtime == nil {
								pendingPlan = configdiff.Build(nil, pendingRuntime.Config)
							} else {
								pendingPlan = configdiff.Build(&runtime.Config, pendingRuntime.Config)
							}
						}
					}
					if pendingRuntime != nil {
						prepareCtx, cancel := context.WithTimeout(ctx, maxDuration(snapshot.Config.RequestTimeout()*4, 15*time.Second))
						prepareErr := pendingRuntime.Prepare(prepareCtx)
						cancel()
						if prepareErr != nil {
							runtimeError = "version v" + fmt.Sprint(snapshot.Version) + " preflight pending: " + prepareErr.Error()
							logger.Warn("candidate configuration not ready; current version remains active", "version", snapshot.Version, "error", prepareErr)
							if !strings.Contains(strings.ToLower(prepareErr.Error()), "twap warming") {
								pendingRuntime = nil
								pendingVersion = 0
							}
						} else {
							cleanupCtx, cleanupCancel := context.WithTimeout(ctx, 20*time.Second)
							cleanupErr := runtime.ApplyCleanup(cleanupCtx, pendingPlan)
							cleanupCancel()
							if cleanupErr != nil {
								runtimeError = "version v" + fmt.Sprint(snapshot.Version) + " targeted cleanup failed: " + cleanupErr.Error()
								logger.Error("candidate cleanup failed; current version remains active", "version", snapshot.Version, "error", cleanupErr)
							} else {
								runtime = pendingRuntime
								activeVersion = snapshot.Version
								activeRawConfig = snapshot.Config
								if err := runtimeStore.SetAppliedVersion(ctx, activeVersion); err != nil {
									logger.Error("persist applied runtime version failed", "version", activeVersion, "error", err)
								}
								pendingRuntime = nil
								pendingVersion = 0
								runtimeError = ""
								nextRun = time.Time{}
								nextRulesRefresh = time.Now().Add(time.Duration(runtime.Config.RulesRefreshSeconds) * time.Second)
								nextBlockedRetry = time.Now().Add(30 * time.Second)
								logger.Info("published configuration applied incrementally", "version", snapshot.Version, "affected_instruments", pendingPlan.AffectedInstruments, "cancel_targets", len(pendingPlan.CancelTargets), "cancel_all", pendingPlan.CancelAll, "blocked_instruments", runtime.Engine.BlockedInstruments())
							}
						}
					}
				}
			} else {
				desiredVersion = activeVersion
			}
		}
		if runtime != nil && !now.Before(nextControl) {
			controlCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
			if err := runtime.Engine.ApplyControls(controlCtx); err != nil {
				logger.Error("apply runtime controls failed", "error", err)
			}
			cancel()
			nextControl = now.Add(500 * time.Millisecond)
		}
		updateHeartbeat()
		if runtime != nil && !nextRulesRefresh.IsZero() && !now.Before(nextRulesRefresh) {
			refreshCtx, cancel := context.WithTimeout(ctx, maxDuration(runtime.Config.RequestTimeout()*4, 15*time.Second))
			changes, refreshErr := runtime.RefreshMarketRules(refreshCtx)
			cancel()
			if refreshErr != nil {
				logger.Error("periodic trading rule refresh failed", "error", refreshErr, "changes_applied", changes)
			} else if changes > 0 {
				logger.Warn("periodic trading rules changed", "changes_applied", changes)
			}
			nextRulesRefresh = now.Add(time.Duration(runtime.Config.RulesRefreshSeconds) * time.Second)
		}
		if runtime != nil && !nextBlockedRetry.IsZero() && !now.Before(nextBlockedRetry) {
			retryCtx, cancel := context.WithTimeout(ctx, maxDuration(runtime.Config.RequestTimeout()*4, 15*time.Second))
			recovered, retryErr := runtime.RetryBlocked(retryCtx)
			cancel()
			if retryErr != nil {
				logger.Warn("blocked instruments remain degraded", "error", retryErr, "recovered", recovered, "blocked_instruments", runtime.Engine.BlockedInstruments())
			} else if recovered > 0 {
				logger.Info("blocked instruments recovered", "recovered", recovered)
			}
			nextBlockedRetry = now.Add(30 * time.Second)
		}
		if runtime != nil && !now.Before(nextRun) {
			if err := runtime.Engine.RunOnce(ctx); err != nil {
				logger.Debug("tick failed", "error", err)
			}
			nextRun = now.Add(runtime.Config.PollInterval())
		}
		select {
		case <-ctx.Done():
			if runtime == nil {
				return nil
			}
			shutdownCtx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			return runtime.Engine.Shutdown(shutdownCtx)
		case <-ticker.C:
		}
	}
}

func maxDuration(a, b time.Duration) time.Duration {
	if a > b {
		return a
	}
	return b
}
