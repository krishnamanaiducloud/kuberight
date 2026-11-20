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
)

// Config contains the security- and reliability-relevant API server settings.
// Secrets are intentionally kept separate from values which are safe to log.
type Config struct {
	ListenAddress string
	OperationMode string
	WebRoot       string
	// BasePath is the URL prefix the dashboard is served under, such as
	// "/kuberight" behind an Ingress or Route path; "" serves it at the root.
	BasePath              string
	MaxBodyBytes          int64
	RequestTimeout        time.Duration
	ReadHeaderTimeout     time.Duration
	ReadTimeout           time.Duration
	WriteTimeout          time.Duration
	IdleTimeout           time.Duration
	ShutdownTimeout       time.Duration
	KubernetesQPS         float32
	KubernetesBurst       int
	InventoryEnabled      bool
	InventoryNamespace    string
	InventoryResync       time.Duration
	ClusterID             string
	PrometheusURL         string
	PrometheusTimeout     time.Duration
	PrometheusMaxInFlight int
	// PrometheusBearerTokenFile and PrometheusCAFile authenticate KubeRight
	// to a protected Prometheus; PrometheusTenantNamespace is sent as the
	// namespace parameter that multi-tenant query proxies require.
	PrometheusBearerTokenFile string
	PrometheusCAFile          string
	PrometheusTenantNamespace string
	// PrometheusDiscovery lets KubeRight find the cluster's monitoring stack
	// when PrometheusURL is empty. PrometheusSource says where the endpoint
	// came from; it is set at startup, not from the environment.
	PrometheusDiscovery    bool
	PrometheusSource       string
	AllowInsecureUpstreams bool

	// DatabaseURL is a secret PostgreSQL connection string. Empty selects
	// the bounded in-process store.
	DatabaseURL      string
	DatabaseMaxConns int
	DatabaseMigrate  bool

	// RemovedSettings lists environment variables that are set but no longer
	// have any effect, so startup can say so.
	RemovedSettings []string

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

	// LatencyQuery is an optional PromQL template, in seconds, correlated with
	// resource signals. It must use $namespace, $workload, or $container.
	LatencyQuery string

	// RequestRateQuery is an optional PromQL template, in requests per
	// second, that correlation analysis compares with usage and latency.
	RequestRateQuery string

	// VerificationInterval is how often approved recommendations are checked
	// for post-change regressions. Zero disables the background check.
	VerificationInterval time.Duration
	// VerificationEarlyInterval is how often changes in their first hour are
	// rechecked for severe regressions. Zero leaves them to the full pass.
	VerificationEarlyInterval time.Duration
	// DefaultScanInterval is the interval of the scan schedule KubeRight
	// creates when none exists, so recommendations appear without anyone
	// queueing a scan. Zero creates none.
	DefaultScanInterval time.Duration

	// WebhookURL receives recommendation lifecycle events. WebhookSecret,
	// when set, signs each body with HMAC-SHA256. WebhookFormat is json or
	// slack.
	WebhookURL string
	// OpenCostURL, when set, prices CPU and memory per namespace from the
	// OpenCost allocation API; configured prices remain the fallback.
	OpenCostURL   string
	WebhookSecret string
	WebhookFormat string
	// GitOps pull requests: provider is "", "github", "gitlab", or "gitea". The
	// repository and base branch are defaults that workload annotations
	// override.
	GitOpsProvider   string
	GitOpsURL        string
	GitOpsToken      string
	GitOpsRepository string
	GitOpsBaseBranch string

	// ExportRecommendationMetrics adds per-container recommendation gauges to
	// /metrics for Grafana and other Prometheus-compatible consumers.
	ExportRecommendationMetrics bool
}

const (
	DefaultRecommendationWindow      = 7 * 24 * time.Hour
	DefaultVerificationInterval      = 15 * time.Minute
	DefaultDefaultScanInterval       = 6 * time.Hour
	DefaultVerificationEarlyInterval = 2 * time.Minute
	DefaultRecommendationStep        = 5 * time.Minute
	DefaultForecastWindow            = 14 * 24 * time.Hour
	DefaultForecastStep              = time.Hour
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
	if c.PrometheusTimeout == 0 {
		c.PrometheusTimeout = 10 * time.Second
	}
	if c.PrometheusMaxInFlight == 0 {
		c.PrometheusMaxInFlight = 8
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
	return (c.CostCPUCoreHour > 0 && c.CostMemoryGiBHour > 0) || c.OpenCostURL != ""
}

// Load reads configuration from the process environment and rejects unsafe or
// internally inconsistent combinations.
func Load() (Config, error) {
	cfg := Config{
		ListenAddress:             envOr("KUBERIGHT_LISTEN_ADDR", ":8080"),
		OperationMode:             strings.ToLower(envOr("KUBERIGHT_OPERATION_MODE", ModeRecommend)),
		WebRoot:                   os.Getenv("KUBERIGHT_WEB_ROOT"),
		BasePath:                  strings.TrimRight(strings.TrimSpace(os.Getenv("KUBERIGHT_BASE_PATH")), "/"),
		InventoryNamespace:        os.Getenv("KUBERIGHT_INVENTORY_NAMESPACE"),
		ClusterID:                 envOr("KUBERIGHT_CLUSTER_ID", "local"),
		PrometheusURL:             os.Getenv("KUBERIGHT_PROMETHEUS_URL"),
		PrometheusBearerTokenFile: strings.TrimSpace(os.Getenv("KUBERIGHT_PROMETHEUS_BEARER_TOKEN_FILE")),
		PrometheusCAFile:          strings.TrimSpace(os.Getenv("KUBERIGHT_PROMETHEUS_CA_FILE")),
		PrometheusTenantNamespace: strings.TrimSpace(os.Getenv("KUBERIGHT_PROMETHEUS_TENANT_NAMESPACE")),
		DatabaseURL:               strings.TrimSpace(os.Getenv("KUBERIGHT_DATABASE_URL")),
		CostCurrency:              strings.ToUpper(envOr("KUBERIGHT_COST_CURRENCY", "USD")),
		LatencyQuery:              strings.TrimSpace(os.Getenv("KUBERIGHT_LATENCY_QUERY")),
		RequestRateQuery:          strings.TrimSpace(os.Getenv("KUBERIGHT_REQUEST_RATE_QUERY")),
		WebhookURL:                strings.TrimSpace(os.Getenv("KUBERIGHT_WEBHOOK_URL")),
		OpenCostURL:               strings.TrimSpace(os.Getenv("KUBERIGHT_OPENCOST_URL")),
		WebhookSecret:             os.Getenv("KUBERIGHT_WEBHOOK_SECRET"),
		WebhookFormat:             strings.ToLower(envOr("KUBERIGHT_WEBHOOK_FORMAT", "json")),
		GitOpsProvider:            strings.ToLower(strings.TrimSpace(os.Getenv("KUBERIGHT_GITOPS_PROVIDER"))),
		GitOpsURL:                 strings.TrimSpace(os.Getenv("KUBERIGHT_GITOPS_URL")),
		GitOpsToken:               os.Getenv("KUBERIGHT_GITOPS_TOKEN"),
		GitOpsRepository:          strings.TrimSpace(os.Getenv("KUBERIGHT_GITOPS_REPOSITORY")),
		GitOpsBaseBranch:          strings.TrimSpace(envOr("KUBERIGHT_GITOPS_BASE_BRANCH", "main")),
	}

	var err error
	if cfg.InventoryEnabled, err = parseBool("KUBERIGHT_INVENTORY_ENABLED", false); err != nil {
		return Config{}, err
	}
	if cfg.PrometheusDiscovery, err = parseBool("KUBERIGHT_PROMETHEUS_DISCOVERY", true); err != nil {
		return Config{}, err
	}
	if cfg.AllowInsecureUpstreams, err = parseBool("KUBERIGHT_ALLOW_INSECURE_UPSTREAMS", false); err != nil {
		return Config{}, err
	}
	// Forecasting is built in, KubeRight no longer changes workloads, and it
	// has no sign-in: these settings are reported at startup and otherwise
	// ignored, so an older manifest still starts.
	for _, name := range []string{
		"KUBERIGHT_ML_URL", "KUBERIGHT_ML_API_KEY", "KUBERIGHT_ML_TIMEOUT", "KUBERIGHT_ML_ALLOW_HTTP",
		"KUBERIGHT_AUTOMATION_INTERVAL", "KUBERIGHT_AUTOMATION_MAX_CHANGES", "KUBERIGHT_AUTOMATION_WINDOW", "KUBERIGHT_AUTOMATION_IN_PLACE_RESIZE",
		"KUBERIGHT_AUTH_MODE", "KUBERIGHT_API_TOKEN", "KUBERIGHT_OIDC_USERNAME_PREFIX", "KUBERIGHT_OIDC_GROUPS_PREFIX",
		"KUBERIGHT_TOKEN_AUDIENCE", "KUBERIGHT_REQUIRE_DISTINCT_APPROVER",
	} {
		if os.Getenv(name) != "" {
			cfg.RemovedSettings = append(cfg.RemovedSettings, name)
		}
	}
	// Apply mode was removed. Running read-only is the safe reading of it.
	if cfg.OperationMode == "apply" {
		cfg.RemovedSettings = append(cfg.RemovedSettings, "KUBERIGHT_OPERATION_MODE=apply")
		cfg.OperationMode = ModeRecommend
	}
	if cfg.DatabaseMigrate, err = parseBool("KUBERIGHT_DATABASE_MIGRATE", false); err != nil {
		return Config{}, err
	}
	if cfg.ExportRecommendationMetrics, err = parseBool("KUBERIGHT_EXPORT_RECOMMENDATION_METRICS", false); err != nil {
		return Config{}, err
	}
	if cfg.DatabaseMaxConns, err = parseInt("KUBERIGHT_DATABASE_MAX_CONNS", 10, 1, 100); err != nil {
		return Config{}, err
	}
	if cfg.PrometheusMaxInFlight, err = parseInt("KUBERIGHT_PROMETHEUS_MAX_IN_FLIGHT", 8, 1, 1024); err != nil {
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
	if cfg.KubernetesBurst, err = parseInt("KUBERIGHT_KUBERNETES_BURST", 100, 1, 1000); err != nil {
		return Config{}, err
	}
	qps, err := parseFloat("KUBERIGHT_KUBERNETES_QPS", 50, 0.1, 1000)
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
		{"KUBERIGHT_RECOMMENDATION_WINDOW", DefaultRecommendationWindow, time.Hour, 30 * 24 * time.Hour, &cfg.RecommendationWindow},
		{"KUBERIGHT_RECOMMENDATION_STEP", DefaultRecommendationStep, 15 * time.Second, time.Hour, &cfg.RecommendationStep},
		{"KUBERIGHT_FORECAST_WINDOW", DefaultForecastWindow, 2 * 24 * time.Hour, 30 * 24 * time.Hour, &cfg.ForecastWindow},
		{"KUBERIGHT_FORECAST_STEP", DefaultForecastStep, time.Minute, 24 * time.Hour, &cfg.ForecastStep},
		{"KUBERIGHT_VERIFICATION_INTERVAL", DefaultVerificationInterval, 0, 24 * time.Hour, &cfg.VerificationInterval},
		{"KUBERIGHT_VERIFICATION_EARLY_INTERVAL", DefaultVerificationEarlyInterval, 0, time.Hour, &cfg.VerificationEarlyInterval},
		{"KUBERIGHT_DEFAULT_SCAN_INTERVAL", DefaultDefaultScanInterval, 0, 30 * 24 * time.Hour, &cfg.DefaultScanInterval},
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
	if err := ValidateBasePath(c.BasePath); err != nil {
		return err
	}
	if c.DefaultScanInterval != 0 && c.DefaultScanInterval < 5*time.Minute {
		return errors.New("KUBERIGHT_DEFAULT_SCAN_INTERVAL must be 0 or at least 5m")
	}
	switch c.OperationMode {
	case ModeObserve, ModeRecommend:
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
	if c.PrometheusMaxInFlight < 0 || c.PrometheusMaxInFlight > 1024 {
		return errors.New("KUBERIGHT_PROMETHEUS_MAX_IN_FLIGHT must be between 1 and 1024 when set")
	}
	if c.PrometheusURL == "" && (c.PrometheusBearerTokenFile != "" || c.PrometheusCAFile != "" || c.PrometheusTenantNamespace != "") {
		return errors.New("KUBERIGHT_PROMETHEUS_BEARER_TOKEN_FILE, KUBERIGHT_PROMETHEUS_CA_FILE and KUBERIGHT_PROMETHEUS_TENANT_NAMESPACE require KUBERIGHT_PROMETHEUS_URL")
	}
	if c.PrometheusBearerTokenFile != "" && strings.HasPrefix(strings.ToLower(c.PrometheusURL), "http://") {
		return errors.New("KUBERIGHT_PROMETHEUS_BEARER_TOKEN_FILE requires an HTTPS KUBERIGHT_PROMETHEUS_URL; a token is never sent in cleartext")
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
	if c.RequestRateQuery != "" {
		if len(c.RequestRateQuery) > 2048 || strings.ContainsAny(c.RequestRateQuery, "\r\n") || !strings.Contains(c.RequestRateQuery, "$") {
			return errors.New("KUBERIGHT_REQUEST_RATE_QUERY must be one line of at most 2048 bytes using $namespace, $workload, or $container")
		}
	}
	if c.VerificationInterval != 0 && (c.VerificationInterval < time.Minute || c.VerificationInterval > 24*time.Hour) {
		return errors.New("KUBERIGHT_VERIFICATION_INTERVAL must be 0 (disabled) or between 1m and 24h")
	}
	if c.VerificationEarlyInterval != 0 && (c.VerificationEarlyInterval < 30*time.Second || c.VerificationEarlyInterval > time.Hour) {
		return errors.New("KUBERIGHT_VERIFICATION_EARLY_INTERVAL must be 0 (disabled) or between 30s and 1h")
	}
	if err := validateUpstream("KUBERIGHT_OPENCOST_URL", c.OpenCostURL, c.AllowInsecureUpstreams, "KUBERIGHT_ALLOW_INSECURE_UPSTREAMS=true"); err != nil {
		return err
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
	return c.validateGitOps()
}

func (c Config) validateGitOps() error {
	switch c.GitOpsProvider {
	case "":
		return nil
	case "github", "gitea", "gitlab":
	default:
		return errors.New("KUBERIGHT_GITOPS_PROVIDER must be github, gitlab, or gitea")
	}
	if c.GitOpsURL == "" {
		if c.GitOpsProvider == "gitea" {
			return errors.New("KUBERIGHT_GITOPS_URL is required for gitea, for example https://git.example.com/api/v1")
		}
	} else if err := validateUpstream("KUBERIGHT_GITOPS_URL", c.GitOpsURL, c.AllowInsecureUpstreams, "KUBERIGHT_ALLOW_INSECURE_UPSTREAMS=true"); err != nil {
		return err
	}
	if len(c.GitOpsToken) < 8 {
		return errors.New("KUBERIGHT_GITOPS_TOKEN is required with KUBERIGHT_GITOPS_PROVIDER")
	}
	if c.GitOpsBaseBranch == "" {
		return errors.New("KUBERIGHT_GITOPS_BASE_BRANCH must not be empty")
	}
	return nil
}

// GitOpsAPIURL is the provider API base, defaulting to github.com.
func (c Config) GitOpsAPIURL() string {
	if c.GitOpsURL == "" && c.GitOpsProvider == "gitlab" {
		return "https://gitlab.com/api/v4"
	}
	if c.GitOpsURL == "" && c.GitOpsProvider == "github" {
		return "https://api.github.com"
	}
	return c.GitOpsURL
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

// ValidateBasePath accepts "" or a prefix such as "/kuberight" or
// "/tools/kuberight": URL-safe segments, no trailing slash, and no "." or ".."
// segments, so the prefix can be written into HTML and matched literally.
func ValidateBasePath(basePath string) error {
	if basePath == "" {
		return nil
	}
	if !strings.HasPrefix(basePath, "/") || strings.HasSuffix(basePath, "/") || len(basePath) > 200 {
		return errors.New("KUBERIGHT_BASE_PATH must start with / and not end with /, for example /kuberight")
	}
	for _, segment := range strings.Split(basePath[1:], "/") {
		if segment == "" || segment == "." || segment == ".." {
			return errors.New("KUBERIGHT_BASE_PATH must not contain empty, . or .. segments")
		}
		for _, r := range segment {
			if !strings.ContainsRune("abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789-_.~", r) {
				return fmt.Errorf("KUBERIGHT_BASE_PATH may contain only letters, digits, and - _ . ~ in each segment, not %q", r)
			}
		}
	}
	return nil
}
