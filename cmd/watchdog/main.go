package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"fluxmaker/internal/app"
	"fluxmaker/internal/config"
	"fluxmaker/internal/configstore"
	"fluxmaker/internal/credentials"
	"fluxmaker/internal/database"
	"fluxmaker/internal/domain"
	"fluxmaker/internal/heartbeat"
	projectlogging "fluxmaker/internal/logging"
	"fluxmaker/internal/oms"
	"fluxmaker/internal/runtimeops"
	"fluxmaker/internal/venue"
)

func main() {
	once := flag.Bool("once", false, "check once and exit")
	flag.Parse()
	logger := projectlogging.New("watchdog")
	if err := run(*once, logger); err != nil {
		logger.Error("watchdog stopped", "error", err)
		os.Exit(1)
	}
}

func run(once bool, logger *slog.Logger) error {
	ctx := context.Background()
	connectCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	connections, err := database.OpenFromEnv(connectCtx, os.Getenv)
	cancel()
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
	episodeActive := false
	for {
		appliedVersion := runtimeStore.AppliedVersion(ctx)
		var snapshot configstore.Snapshot
		if appliedVersion > 0 {
			snapshot, err = store.LoadVersion(ctx, appliedVersion)
		} else {
			// Compatibility fallback for installations upgraded from versions that
			// did not persist the applied runtime version yet.
			snapshot, err = store.LoadActive(ctx)
		}
		if errors.Is(err, configstore.ErrNoActive) {
			if once {
				return err
			}
			logger.Info("waiting for a published configuration")
			time.Sleep(5 * time.Second)
			continue
		}
		if err != nil {
			if once {
				return err
			}
			logger.Error("load configuration failed", "error", err)
			time.Sleep(5 * time.Second)
			continue
		}
		cfg := snapshot.Config
		if cfg.Mode != domain.ModeLive {
			_ = runtimeStore.ReportWatchdog(ctx, true, false, "", "")
			if once {
				return nil
			}
			time.Sleep(5 * time.Second)
			continue
		}
		if err := cfg.ValidateRuntime(); err != nil {
			if once {
				return err
			}
			logger.Error("runtime configuration invalid", "error", err)
			time.Sleep(5 * time.Second)
			continue
		}
		age, ageErr := heartbeat.Age(cfg.HeartbeatPath)
		progressTimeout := cfg.TradingProgressTimeoutSeconds
		if progressTimeout <= 0 {
			progressTimeout = 120
		}
		progressAt := runtimeStore.TradingProgress(ctx)
		progressStale := !progressAt.IsZero() && time.Since(progressAt) > time.Duration(progressTimeout)*time.Second
		stale := ageErr != nil || age > time.Duration(cfg.WatchdogTimeoutSeconds)*time.Second || progressStale
		if stale && !episodeActive {
			reason := watchdogReason(age, ageErr, progressStale)
			logger.Error("liveness stale; canceling managed orders", "reason", reason, "process_age", age, "heartbeat_error", ageErr, "last_trading_progress", progressAt, "trading_progress_stale", progressStale)
			clients, err := app.BuildVenues(ctx, cfg, credentialService)
			if err != nil {
				_ = runtimeStore.ReportWatchdog(ctx, false, true, reason, err.Error())
				return err
			}
			if err := cancelAll(cfg, clients); err != nil {
				_ = runtimeStore.ReportWatchdog(ctx, false, true, reason, err.Error())
				return err
			}
			episodeActive = true
			_ = runtimeStore.ReportWatchdog(ctx, false, true, reason, "")
		} else if stale {
			_ = runtimeStore.ReportWatchdog(ctx, false, false, watchdogReason(age, ageErr, progressStale), "")
		} else {
			if episodeActive {
				logger.Info("liveness recovered", "process_age", age, "last_trading_progress", progressAt)
			}
			episodeActive = false
			_ = runtimeStore.ReportWatchdog(ctx, true, false, "", "")
		}
		if once {
			return nil
		}
		interval := time.Duration(cfg.WatchdogTimeoutSeconds) * time.Second / 3
		if interval < time.Second {
			interval = time.Second
		}
		time.Sleep(interval)
	}
}

func watchdogReason(age time.Duration, ageErr error, progressStale bool) string {
	if ageErr != nil {
		return "process heartbeat unavailable: " + ageErr.Error()
	}
	if progressStale {
		return "trading progress timeout"
	}
	return "process heartbeat stale: " + age.Round(time.Second).String()
}

func cancelAll(cfg config.Config, clients map[string]venue.Client) error {
	var failures []string
	reconciler := oms.New()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	for _, in := range cfg.Instruments {
		for venueName, venueCfg := range cfg.Venues {
			if !venueCfg.Enabled || !venueCfg.TradingEnabled {
				continue
			}
			market, ok := venueCfg.Markets[in.ID]
			if !ok {
				continue
			}
			client := clients[venue.ClientKey(venueName, in.ID)]
			if client == nil {
				failures = append(failures, venueName+"/"+in.ID+": client missing")
				continue
			}
			if err := reconciler.CancelManaged(ctx, client, in.ID, market.Symbol); err != nil {
				failures = append(failures, venueName+"/"+in.ID+": "+err.Error())
			}
		}
	}
	if len(failures) > 0 {
		return fmt.Errorf("watchdog cancel failures: %s", strings.Join(failures, "; "))
	}
	return nil
}
