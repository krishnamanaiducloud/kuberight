package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/kuberight/kuberight/internal/config"
	"github.com/kuberight/kuberight/internal/forecast"
	"github.com/kuberight/kuberight/internal/httpapi"
	"github.com/kuberight/kuberight/internal/metrics"
	"github.com/kuberight/kuberight/internal/provider"
	"github.com/kuberight/kuberight/internal/repository/postgres"
	"github.com/kuberight/kuberight/internal/usage"
)

func main() {
	if len(os.Args) == 2 && os.Args[1] == "healthcheck" {
		if err := healthcheck(); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	}

	logger := httpapi.JSONLogger(slog.LevelInfo)
	slog.SetDefault(logger)

	cfg, err := config.Load()
	if err != nil {
		logger.Error("configuration is invalid", "error", err)
		os.Exit(1)
	}
	dependencies := httpapi.Dependencies{Logger: logger}
	store, err := openStore(cfg, logger)
	if err != nil {
		logger.Error("recommendation store initialization failed", "error", err)
		os.Exit(1)
	}
	if store != nil {
		defer store.Close()
		dependencies.Repository = store
	}
	if cfg.PrometheusURL != "" {
		client, clientErr := metrics.NewPrometheusClient(metrics.PrometheusOptions{
			Endpoint: cfg.PrometheusURL, AllowHTTP: cfg.AllowInsecureUpstreams, Timeout: cfg.PrometheusTimeout,
		})
		if clientErr != nil {
			logger.Error("Prometheus client initialization failed", "error", clientErr)
			os.Exit(1)
		}
		dependencies.Usage = usage.NewSource(client)
	}
	if cfg.MLURL != "" {
		client, clientErr := forecast.NewClient(forecast.Options{
			BaseURL: cfg.MLURL, APIKey: cfg.MLAPIKey, Timeout: cfg.MLTimeout, AllowHTTP: cfg.MLAllowHTTP || cfg.AllowInsecureUpstreams,
		})
		if clientErr != nil {
			logger.Error("forecasting client initialization failed", "error", clientErr)
			os.Exit(1)
		}
		dependencies.Forecaster = client
	}
	if cfg.InventoryEnabled {
		inventory, inventoryErr := provider.NewKubernetesProvider(cfg)
		if inventoryErr != nil {
			logger.Error("Kubernetes inventory initialization failed", "error", inventoryErr)
			os.Exit(1)
		}
		dependencies.Provider = inventory
	}
	handler, err := httpapi.NewHandler(cfg, dependencies)
	if err != nil {
		logger.Error("API initialization failed", "error", err)
		os.Exit(1)
	}
	server := httpapi.NewHTTPServer(cfg, handler)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	shutdownComplete := make(chan struct{})
	go func() {
		<-ctx.Done()
		shutdownContext, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
		defer cancel()
		if err := server.Shutdown(shutdownContext); err != nil {
			logger.Error("graceful shutdown failed", "error", err)
		}
		close(shutdownComplete)
	}()

	logger.Info("starting KubeRight API", "address", cfg.ListenAddress, "version", httpapi.Version, "commit", httpapi.Commit, "mode", cfg.OperationMode,
		"store", storeKind(store), "prometheus", cfg.PrometheusURL != "", "forecasting", cfg.MLURL != "", "costModel", cfg.CostConfigured())
	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		logger.Error("API server failed", "error", err)
		os.Exit(1)
	}
	if ctx.Err() != nil {
		<-shutdownComplete
	}
	logger.Info("KubeRight API stopped")
}

func healthcheck() error {
	endpoint := os.Getenv("KUBERIGHT_HEALTHCHECK_URL")
	if endpoint == "" {
		endpoint = "http://127.0.0.1:8080/healthz"
	}
	client := &http.Client{Timeout: 3 * time.Second}
	response, err := client.Get(endpoint) // #nosec G107 -- endpoint is an operator-owned container setting.
	if err != nil {
		return fmt.Errorf("health request failed: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("health endpoint returned %s", response.Status)
	}
	return nil
}

// openStore connects to PostgreSQL when configured. It returns nil, nil to
// select the in-process store. Migrations run only when explicitly enabled;
// otherwise the schema must already match this binary.
func openStore(cfg config.Config, logger *slog.Logger) (*postgres.Store, error) {
	if cfg.DatabaseURL == "" {
		logger.Warn("KUBERIGHT_DATABASE_URL is not set; recommendations and audit events are kept in memory and lost on restart")
		return nil, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	store, err := postgres.Open(ctx, cfg.DatabaseURL, postgres.Options{MaxConns: int32(cfg.DatabaseMaxConns)})
	if err != nil {
		return nil, err
	}
	if cfg.DatabaseMigrate {
		if err := postgres.Migrate(ctx, store.Pool()); err != nil {
			store.Close()
			return nil, fmt.Errorf("apply database migrations: %w", err)
		}
		logger.Info("database schema is current", "version", postgres.LatestSchemaVersion)
	} else if err := postgres.VerifySchema(ctx, store.Pool()); err != nil {
		store.Close()
		return nil, err
	}
	return store, nil
}

func storeKind(store *postgres.Store) string {
	if store == nil {
		return "memory"
	}
	return store.Kind()
}
