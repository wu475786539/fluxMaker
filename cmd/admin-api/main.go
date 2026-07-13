package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"fluxmaker/internal/adminapi"
	"fluxmaker/internal/auth"
	"fluxmaker/internal/configstore"
	"fluxmaker/internal/credentials"
	"fluxmaker/internal/database"
	projectlogging "fluxmaker/internal/logging"
	"fluxmaker/internal/runtimeops"
)

func main() {
	logger := projectlogging.New("admin-api")
	if err := run(logger); err != nil {
		logger.Error("admin api stopped", "error", err)
		os.Exit(1)
	}
}

func run(logger *slog.Logger) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	connectCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	connections, err := database.OpenFromEnv(connectCtx, os.Getenv)
	if err != nil {
		return err
	}
	defer connections.Close()
	if err := database.Migrate(connectCtx, connections.Postgres); err != nil {
		return fmt.Errorf("migrate: %w", err)
	}
	credentialService, err := credentials.NewService(connections.Postgres, os.Getenv("CREDENTIAL_MASTER_KEY"))
	if err != nil {
		return fmt.Errorf("initialize credential encryption: %w", err)
	}
	authService := auth.NewService(connections.Postgres, connections.Redis)
	if err := authService.BootstrapAdmin(connectCtx, os.Getenv("ADMIN_EMAIL"), os.Getenv("ADMIN_PASSWORD")); err != nil {
		return fmt.Errorf("bootstrap admin: %w", err)
	}
	server := &http.Server{Addr: envDefault("ADMIN_ADDR", ":8080"), Handler: adminapi.New(connections.Postgres, connections.Redis, authService, configstore.New(connections.Postgres, connections.Redis), credentialService, runtimeops.New(connections.Redis), adminapi.WithLogger(logger), adminapi.WithMetricsToken(os.Getenv("METRICS_TOKEN"))).Handler(), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 15 * time.Second, WriteTimeout: 30 * time.Second, IdleTimeout: 60 * time.Second}
	errCh := make(chan error, 1)
	go func() { logger.Info("admin api listening", "addr", server.Addr); errCh <- server.ListenAndServe() }()
	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return server.Shutdown(shutdownCtx)
	case err := <-errCh:
		if err == http.ErrServerClosed {
			return nil
		}
		return err
	}
}

func envDefault(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}
