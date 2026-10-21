// Package config loads and validates KubeRight's process configuration.
package config

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

const (
	ModeObserve   = "observe"
	ModeRecommend = "recommend"
	ModeApply     = "apply"
)

// Config contains the security- and reliability-relevant API server settings.
// Secrets are intentionally kept separate from values which are safe to log.
type Config struct {
	ListenAddress          string
	AuthMode               string
	APIToken               string
	TokenAudience          string
	OperationMode          string
	WebRoot                string
	MaxBodyBytes           int64
	RequestTimeout         time.Duration
	ReadHeaderTimeout      time.Duration
	ReadTimeout            time.Duration
	WriteTimeout           time.Duration
	IdleTimeout            time.Duration
	ShutdownTimeout        time.Duration
	KubernetesQPS          float32
	KubernetesBurst        int
	InventoryEnabled       bool
	InventoryNamespace     string
	InventoryResync        time.Duration
	ClusterID              string
	PrometheusURL          string
	PrometheusTimeout      time.Duration
	AllowInsecureUpstreams bool
}

// Load reads configuration from the process environment and rejects unsafe or
// internally inconsistent combinations.
func Load() (Config, error) {
	cfg := Config{
		ListenAddress:      envOr("KUBERIGHT_LISTEN_ADDR", ":8080"),
		AuthMode:           strings.ToLower(envOr("KUBERIGHT_AUTH_MODE", "kubernetes")),
		APIToken:           os.Getenv("KUBERIGHT_API_TOKEN"),
		TokenAudience:      os.Getenv("KUBERIGHT_TOKEN_AUDIENCE"),
		OperationMode:      strings.ToLower(envOr("KUBERIGHT_OPERATION_MODE", ModeRecommend)),
		WebRoot:            os.Getenv("KUBERIGHT_WEB_ROOT"),
		InventoryNamespace: os.Getenv("KUBERIGHT_INVENTORY_NAMESPACE"),
		ClusterID:          envOr("KUBERIGHT_CLUSTER_ID", "local"),
		PrometheusURL:      os.Getenv("KUBERIGHT_PROMETHEUS_URL"),
	}

	var err error
	if cfg.InventoryEnabled, err = parseBool("KUBERIGHT_INVENTORY_ENABLED", false); err != nil {
		return Config{}, err
	}
	if cfg.AllowInsecureUpstreams, err = parseBool("KUBERIGHT_ALLOW_INSECURE_UPSTREAMS", false); err != nil {
		return Config{}, err
	}
	if cfg.MaxBodyBytes, err = parseInt64("KUBERIGHT_MAX_BODY_BYTES", 1<<20, 1024, 16<<20); err != nil {
		return Config{}, err
	}
	if cfg.KubernetesBurst, err = parseInt("KUBERIGHT_KUBERNETES_BURST", 20, 1, 1000); err != nil {
		return Config{}, err
	}
	qps, err := parseFloat("KUBERIGHT_KUBERNETES_QPS", 10, 0.1, 1000)
	if err != nil {
		return Config{}, err
	}
	cfg.KubernetesQPS = float32(qps)

	durations := []struct {
		name         string
		defaultValue time.Duration
		minimum      time.Duration
		maximum      time.Duration
		destination  *time.Duration
	}{
		{"KUBERIGHT_REQUEST_TIMEOUT", 15 * time.Second, time.Second, 2 * time.Minute, &cfg.RequestTimeout},
		{"KUBERIGHT_READ_HEADER_TIMEOUT", 5 * time.Second, time.Second, 30 * time.Second, &cfg.ReadHeaderTimeout},
		{"KUBERIGHT_READ_TIMEOUT", 15 * time.Second, time.Second, 2 * time.Minute, &cfg.ReadTimeout},
		{"KUBERIGHT_WRITE_TIMEOUT", 30 * time.Second, time.Second, 3 * time.Minute, &cfg.WriteTimeout},
		{"KUBERIGHT_IDLE_TIMEOUT", 60 * time.Second, 5 * time.Second, 10 * time.Minute, &cfg.IdleTimeout},
		{"KUBERIGHT_SHUTDOWN_TIMEOUT", 20 * time.Second, time.Second, 2 * time.Minute, &cfg.ShutdownTimeout},
		{"KUBERIGHT_INVENTORY_RESYNC", 10 * time.Minute, 30 * time.Second, 24 * time.Hour, &cfg.InventoryResync},
		{"KUBERIGHT_PROMETHEUS_TIMEOUT", 10 * time.Second, time.Second, time.Minute, &cfg.PrometheusTimeout},
	}
	for _, item := range durations {
		value, parseErr := parseDuration(item.name, item.defaultValue, item.minimum, item.maximum)
		if parseErr != nil {
			return Config{}, parseErr
		}
		*item.destination = value
	}

	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// Validate checks Config values without reading the environment. It is useful
// for tests and for embedders constructing Config programmatically.
func (c Config) Validate() error {
	if strings.TrimSpace(c.ListenAddress) == "" {
		return errors.New("listen address must not be empty")
	}
	if _, _, err := net.SplitHostPort(c.ListenAddress); err != nil {
		return fmt.Errorf("invalid KUBERIGHT_LISTEN_ADDR: %w", err)
	}
	switch c.AuthMode {
	case "kubernetes":
	case "static":
		if len(c.APIToken) < 32 {
			return errors.New("KUBERIGHT_API_TOKEN must contain at least 32 characters in static auth mode")
		}
	default:
		return errors.New("KUBERIGHT_AUTH_MODE must be either kubernetes or static")
	}
	switch c.OperationMode {
	case ModeObserve, ModeRecommend:
	case ModeApply:
		return errors.New("apply mode is not supported because this release has no mutating provider")
	default:
		return errors.New("KUBERIGHT_OPERATION_MODE must be observe or recommend")
	}
	if c.MaxBodyBytes < 1024 || c.MaxBodyBytes > 16<<20 {
		return errors.New("maximum request body size must be between 1 KiB and 16 MiB")
	}
	if c.RequestTimeout <= 0 || c.ReadHeaderTimeout <= 0 || c.ReadTimeout <= 0 || c.WriteTimeout <= 0 || c.IdleTimeout <= 0 || c.ShutdownTimeout <= 0 {
		return errors.New("HTTP timeouts must be positive")
	}
	if c.RequestTimeout >= c.WriteTimeout {
		return errors.New("request timeout must be shorter than write timeout")
	}
	if len(c.ClusterID) > 253 || strings.TrimSpace(c.ClusterID) == "" {
		return errors.New("KUBERIGHT_CLUSTER_ID must contain 1 to 253 characters")
	}
	if c.PrometheusURL != "" {
		u, err := url.Parse(c.PrometheusURL)
		if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
			return errors.New("KUBERIGHT_PROMETHEUS_URL must be an absolute HTTP(S) URL without credentials, query, or fragment")
		}
		if u.Scheme != "https" && !c.AllowInsecureUpstreams {
			return errors.New("HTTP Prometheus URLs require KUBERIGHT_ALLOW_INSECURE_UPSTREAMS=true")
		}
	}
	return nil
}

func envOr(name, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(name)); value != "" {
		return value
	}
	return fallback
}

func parseBool(name string, fallback bool) (bool, error) {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return fallback, nil
	}
	value, err := strconv.ParseBool(raw)
	if err != nil {
		return false, fmt.Errorf("%s must be a boolean: %w", name, err)
	}
	return value, nil
}

func parseInt(name string, fallback, minimum, maximum int) (int, error) {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return fallback, nil
	}
	value, err := strconv.Atoi(raw)
	if err != nil || value < minimum || value > maximum {
		return 0, fmt.Errorf("%s must be an integer between %d and %d", name, minimum, maximum)
	}
	return value, nil
}

func parseInt64(name string, fallback, minimum, maximum int64) (int64, error) {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return fallback, nil
	}
	value, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || value < minimum || value > maximum {
		return 0, fmt.Errorf("%s must be an integer between %d and %d", name, minimum, maximum)
	}
	return value, nil
}

func parseFloat(name string, fallback, minimum, maximum float64) (float64, error) {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return fallback, nil
	}
	value, err := strconv.ParseFloat(raw, 64)
	if err != nil || value < minimum || value > maximum {
		return 0, fmt.Errorf("%s must be a number between %g and %g", name, minimum, maximum)
	}
	return value, nil
}

func parseDuration(name string, fallback, minimum, maximum time.Duration) (time.Duration, error) {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return fallback, nil
	}
	value, err := time.ParseDuration(raw)
	if err != nil || value < minimum || value > maximum {
		return 0, fmt.Errorf("%s must be a duration between %s and %s", name, minimum, maximum)
	}
	return value, nil
}
