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

	// DatabaseURL is a secret PostgreSQL connection string. Empty selects
	// the bounded in-process store.
	DatabaseURL      string
	DatabaseMaxConns int
	DatabaseMigrate  bool

	// MLURL is the base URL of the forecasting service. MLAPIKey is secret.
	MLURL       string
	MLAPIKey    string
	MLTimeout   time.Duration
	MLAllowHTTP bool

	// Prometheus usage windows used to generate recommendations and
	// forecasts. Zero values select the defaults.
	RecommendationWindow time.Duration
	RecommendationStep   time.Duration
	ForecastWindow       time.Duration
	ForecastStep         time.Duration

	// Operator-supplied unit prices for savings estimates. Zero disables
	// cost estimation; KubeRight never invents a price.
	CostCPUCoreHour   float64
	CostMemoryGiBHour float64
	CostCurrency      string

	// RequireDistinctApprover rejects reviews by the identity that created
	// the recommendation.
	RequireDistinctApprover bool

	// LatencyQuery is an optional PromQL template, in seconds, correlated with
	// resource signals. It must use $namespace, $workload, or $container.
	LatencyQuery string

	// WebhookURL receives recommendation lifecycle events. WebhookSecret,
	// when set, signs each body with HMAC-SHA256. WebhookFormat is json or
	// slack.
	WebhookURL    string
	WebhookSecret string
	WebhookFormat string

	// ExportRecommendationMetrics adds per-container recommendation gauges to
	// /metrics for Grafana and other Prometheus-compatible consumers.
	ExportRecommendationMetrics bool
}

const (
	DefaultRecommendationWindow = 7 * 24 * time.Hour
	DefaultRecommendationStep   = 5 * time.Minute
	DefaultForecastWindow       = 14 * 24 * time.Hour
	DefaultForecastStep         = time.Hour
	// MaximumQueryPoints matches the Prometheus client's per-query bound.
	MaximumQueryPoints = 11_000
)

// Normalized returns a copy with defaults applied to unset optional values.
func (c Config) Normalized() Config {
	if c.RecommendationWindow == 0 {
		c.RecommendationWindow = DefaultRecommendationWindow
	}
	if c.RecommendationStep == 0 {
		c.RecommendationStep = DefaultRecommendationStep
	}
	if c.ForecastWindow == 0 {
		c.ForecastWindow = DefaultForecastWindow
	}
	if c.ForecastStep == 0 {
		c.ForecastStep = DefaultForecastStep
	}
	if c.MLTimeout == 0 {
		c.MLTimeout = 10 * time.Second
	}
	if c.PrometheusTimeout == 0 {
		c.PrometheusTimeout = 10 * time.Second
	}
	if c.DatabaseMaxConns == 0 {
		c.DatabaseMaxConns = 10
	}
	if c.CostCurrency == "" {
		c.CostCurrency = "USD"
	}
	return c
}

// CostConfigured reports whether savings can be estimated.
func (c Config) CostConfigured() bool {
	return c.CostCPUCoreHour > 0 && c.CostMemoryGiBHour > 0
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
		DatabaseURL:        strings.TrimSpace(os.Getenv("KUBERIGHT_DATABASE_URL")),
		MLURL:              strings.TrimSpace(os.Getenv("KUBERIGHT_ML_URL")),
		MLAPIKey:           os.Getenv("KUBERIGHT_ML_API_KEY"),
		CostCurrency:       strings.ToUpper(envOr("KUBERIGHT_COST_CURRENCY", "USD")),
		LatencyQuery:       strings.TrimSpace(os.Getenv("KUBERIGHT_LATENCY_QUERY")),
		WebhookURL:         strings.TrimSpace(os.Getenv("KUBERIGHT_WEBHOOK_URL")),
		WebhookSecret:      os.Getenv("KUBERIGHT_WEBHOOK_SECRET"),
		WebhookFormat:      strings.ToLower(envOr("KUBERIGHT_WEBHOOK_FORMAT", "json")),
	}

	var err error
	if cfg.InventoryEnabled, err = parseBool("KUBERIGHT_INVENTORY_ENABLED", false); err != nil {
		return Config{}, err
	}
	if cfg.AllowInsecureUpstreams, err = parseBool("KUBERIGHT_ALLOW_INSECURE_UPSTREAMS", false); err != nil {
		return Config{}, err
	}
	if cfg.MLAllowHTTP, err = parseBool("KUBERIGHT_ML_ALLOW_HTTP", false); err != nil {
		return Config{}, err
	}
	if cfg.DatabaseMigrate, err = parseBool("KUBERIGHT_DATABASE_MIGRATE", true); err != nil {
		return Config{}, err
	}
	if cfg.ExportRecommendationMetrics, err = parseBool("KUBERIGHT_EXPORT_RECOMMENDATION_METRICS", false); err != nil {
		return Config{}, err
	}
	// Static mode has a single shared identity, so separation of duties is
	// only meaningful, and therefore only on by default, with Kubernetes auth.
	if cfg.RequireDistinctApprover, err = parseBool("KUBERIGHT_REQUIRE_DISTINCT_APPROVER", cfg.AuthMode == "kubernetes"); err != nil {
		return Config{}, err
	}
	if cfg.DatabaseMaxConns, err = parseInt("KUBERIGHT_DATABASE_MAX_CONNS", 10, 1, 100); err != nil {
		return Config{}, err
	}
	if cfg.CostCPUCoreHour, err = parseFloat("KUBERIGHT_COST_CPU_CORE_HOUR", 0, 0, 1000); err != nil {
		return Config{}, err
	}
	if cfg.CostMemoryGiBHour, err = parseFloat("KUBERIGHT_COST_MEMORY_GIB_HOUR", 0, 0, 1000); err != nil {
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
		{"KUBERIGHT_ML_TIMEOUT", 10 * time.Second, time.Second, time.Minute, &cfg.MLTimeout},
		{"KUBERIGHT_RECOMMENDATION_WINDOW", DefaultRecommendationWindow, time.Hour, 30 * 24 * time.Hour, &cfg.RecommendationWindow},
		{"KUBERIGHT_RECOMMENDATION_STEP", DefaultRecommendationStep, 15 * time.Second, time.Hour, &cfg.RecommendationStep},
		{"KUBERIGHT_FORECAST_WINDOW", DefaultForecastWindow, 2 * 24 * time.Hour, 30 * 24 * time.Hour, &cfg.ForecastWindow},
		{"KUBERIGHT_FORECAST_STEP", DefaultForecastStep, time.Minute, 24 * time.Hour, &cfg.ForecastStep},
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
	if err := validateUpstream("KUBERIGHT_PROMETHEUS_URL", c.PrometheusURL, c.AllowInsecureUpstreams, "KUBERIGHT_ALLOW_INSECURE_UPSTREAMS=true"); err != nil {
		return err
	}
	if err := validateUpstream("KUBERIGHT_ML_URL", c.MLURL, c.AllowInsecureUpstreams || c.MLAllowHTTP, "KUBERIGHT_ML_ALLOW_HTTP=true"); err != nil {
		return err
	}
	if c.MLAPIKey != "" && len(c.MLAPIKey) < 16 {
		return errors.New("KUBERIGHT_ML_API_KEY must contain at least 16 characters")
	}
	if c.DatabaseURL != "" {
		u, err := url.Parse(c.DatabaseURL)
		if err != nil || (u.Scheme != "postgres" && u.Scheme != "postgresql") || u.Host == "" {
			return errors.New("KUBERIGHT_DATABASE_URL must be a postgres:// or postgresql:// URL")
		}
	}
	if c.DatabaseMaxConns < 0 || c.DatabaseMaxConns > 100 {
		return errors.New("KUBERIGHT_DATABASE_MAX_CONNS must be between 1 and 100")
	}
	windows := []struct {
		name         string
		window, step time.Duration
	}{
		{"recommendation", c.RecommendationWindow, c.RecommendationStep},
		{"forecast", c.ForecastWindow, c.ForecastStep},
	}
	for _, item := range windows {
		if item.window < 0 || item.step < 0 {
			return fmt.Errorf("%s window and step must not be negative", item.name)
		}
		if item.window > 0 && item.step > 0 {
			if item.step >= item.window {
				return fmt.Errorf("%s step must be shorter than its window", item.name)
			}
			if item.window/item.step+1 > MaximumQueryPoints {
				return fmt.Errorf("%s window divided by step must not exceed %d points", item.name, MaximumQueryPoints)
			}
		}
	}
	if c.CostCPUCoreHour < 0 || c.CostMemoryGiBHour < 0 || c.CostCPUCoreHour > 1000 || c.CostMemoryGiBHour > 1000 {
		return errors.New("cost unit prices must be between 0 and 1000")
	}
	if (c.CostCPUCoreHour > 0) != (c.CostMemoryGiBHour > 0) {
		return errors.New("KUBERIGHT_COST_CPU_CORE_HOUR and KUBERIGHT_COST_MEMORY_GIB_HOUR must be set together")
	}
	if c.LatencyQuery != "" {
		if len(c.LatencyQuery) > 2048 || strings.ContainsAny(c.LatencyQuery, "\r\n") || !strings.Contains(c.LatencyQuery, "$") {
			return errors.New("KUBERIGHT_LATENCY_QUERY must be one line of at most 2048 bytes using $namespace, $workload, or $container")
		}
	}
	if err := validateUpstream("KUBERIGHT_WEBHOOK_URL", c.WebhookURL, c.AllowInsecureUpstreams, "KUBERIGHT_ALLOW_INSECURE_UPSTREAMS=true"); err != nil {
		return err
	}
	if c.WebhookFormat != "" && c.WebhookFormat != "json" && c.WebhookFormat != "slack" {
		return errors.New("KUBERIGHT_WEBHOOK_FORMAT must be json or slack")
	}
	if c.WebhookSecret != "" && len(c.WebhookSecret) < 16 {
		return errors.New("KUBERIGHT_WEBHOOK_SECRET must contain at least 16 characters")
	}
	if c.CostCurrency != "" && !validCurrency(c.CostCurrency) {
		return errors.New("KUBERIGHT_COST_CURRENCY must be a three-letter ISO 4217 code")
	}
	return nil
}

func validateUpstream(name, raw string, allowHTTP bool, httpOptIn string) error {
	if raw == "" {
		return nil
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return fmt.Errorf("%s must be an absolute HTTP(S) URL without credentials, query, or fragment", name)
	}
	if u.Scheme != "https" && !allowHTTP {
		return fmt.Errorf("HTTP %s values require %s", name, httpOptIn)
	}
	return nil
}

func validCurrency(code string) bool {
	if len(code) != 3 {
		return false
	}
	for _, character := range code {
		if character < 'A' || character > 'Z' {
			return false
		}
	}
	return true
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
