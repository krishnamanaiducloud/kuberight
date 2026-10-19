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
	"github.com/kuberight/kuberight/internal/httpapi"
	"github.com/kuberight/kuberight/internal/provider"
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

	logger.Info("starting KubeRight API", "address", cfg.ListenAddress, "version", httpapi.Version, "commit", httpapi.Commit, "mode", cfg.OperationMode)
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
