package config

import (
	"strings"
	"testing"
	"time"
)

func validConfig() Config {
	return Config{
		ListenAddress:     ":8080",
		OperationMode:     ModeRecommend,
		MaxBodyBytes:      1 << 20,
		RequestTimeout:    15 * time.Second,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       time.Minute,
		ShutdownTimeout:   20 * time.Second,
		ClusterID:         "test",
	}
}

func TestValidateRequiresTLSForPrometheusByDefault(t *testing.T) {
	cfg := validConfig()
	cfg.PrometheusURL = "http://prometheus.monitoring.svc:9090"
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected cleartext upstream to be rejected")
	}
	cfg.AllowInsecureUpstreams = true
	if err := cfg.Validate(); err != nil {
		t.Fatalf("expected explicitly permitted cluster-local HTTP endpoint: %v", err)
	}
}

func TestValidateIntegrationSettings(t *testing.T) {
	cases := map[string]struct {
		mutate func(*Config)
		valid  bool
	}{
		"defaults":                    {func(*Config) {}, true},
		"postgres url":                {func(c *Config) { c.DatabaseURL = "postgres://kuberight@db:5432/kuberight?sslmode=verify-full" }, true},
		"non postgres url":            {func(c *Config) { c.DatabaseURL = "mysql://db/kuberight" }, false},
		"too many connections":        {func(c *Config) { c.DatabaseMaxConns = 101 }, false},
		"too much metric parallelism": {func(c *Config) { c.PrometheusMaxInFlight = 1025 }, false},
		"one price only":              {func(c *Config) { c.CostCPUCoreHour = 0.03 }, false},
		"both prices":                 {func(c *Config) { c.CostCPUCoreHour, c.CostMemoryGiBHour = 0.03, 0.004 }, true},
		"lowercase currency":          {func(c *Config) { c.CostCurrency = "usd" }, false},
		"step longer than window":     {func(c *Config) { c.RecommendationWindow, c.RecommendationStep = time.Hour, 2*time.Hour }, false},
		"too many points":             {func(c *Config) { c.RecommendationWindow, c.RecommendationStep = 30*24*time.Hour, 15*time.Second }, false},
		"custom forecast sampling":    {func(c *Config) { c.ForecastWindow, c.ForecastStep = 7*24*time.Hour, 30*time.Minute }, true},
		"early verification off":      {func(c *Config) { c.VerificationEarlyInterval = 0 }, true},
		"early verification 30s":      {func(c *Config) { c.VerificationEarlyInterval = 30 * time.Second }, true},
		"early verification too fast": {func(c *Config) { c.VerificationEarlyInterval = 10 * time.Second }, false},
		"early verification too slow": {func(c *Config) { c.VerificationEarlyInterval = 2 * time.Hour }, false},
	}
	for name, test := range cases {
		t.Run(name, func(t *testing.T) {
			cfg := validConfig()
			test.mutate(&cfg)
			if err := cfg.Validate(); (err == nil) != test.valid {
				t.Fatalf("Validate() = %v, want valid=%v", err, test.valid)
			}
		})
	}
}

func TestLoadAppliesIntegrationDefaults(t *testing.T) {
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.DatabaseMigrate || cfg.DatabaseMaxConns != 10 || cfg.PrometheusMaxInFlight != 8 || cfg.RecommendationWindow != DefaultRecommendationWindow || cfg.ForecastStep != DefaultForecastStep || cfg.VerificationEarlyInterval != DefaultVerificationEarlyInterval || cfg.CostCurrency != "USD" || cfg.CostConfigured() {
		t.Fatalf("unexpected defaults: %#v", cfg)
	}
	t.Setenv("KUBERIGHT_COST_CPU_CORE_HOUR", "0.031")
	t.Setenv("KUBERIGHT_COST_MEMORY_GIB_HOUR", "0.004")
	t.Setenv("KUBERIGHT_COST_CURRENCY", "eur")
	if cfg, err = Load(); err != nil || !cfg.CostConfigured() || cfg.CostCurrency != "EUR" {
		t.Fatalf("cost=%v currency=%s err=%v", cfg.CostConfigured(), cfg.CostCurrency, err)
	}
}

func TestNormalizedFillsUnsetValues(t *testing.T) {
	cfg := validConfig().Normalized()
	if cfg.RecommendationStep != DefaultRecommendationStep || cfg.ForecastWindow != DefaultForecastWindow || cfg.CostCurrency != "USD" {
		t.Fatalf("Normalized() = %#v", cfg)
	}
}

func TestOpenCostURLFollowsUpstreamTransportRules(t *testing.T) {
	cfg := validConfig()
	cfg.OpenCostURL = "http://opencost.opencost.svc:9003"
	if err := cfg.Validate(); err == nil {
		t.Fatal("cleartext OpenCost was accepted without the insecure-upstream opt-in")
	}
	cfg.AllowInsecureUpstreams = true
	if err := cfg.Validate(); err != nil || !cfg.CostConfigured() {
		t.Fatalf("explicit in-cluster OpenCost: err=%v costConfigured=%v", err, cfg.CostConfigured())
	}
}

func TestValidateGitOpsSettings(t *testing.T) {
	cfg := validConfig()
	cfg.GitOpsProvider, cfg.GitOpsToken, cfg.GitOpsBaseBranch = "github", "ghp_0123456789", "main"
	if err := cfg.Validate(); err != nil || cfg.GitOpsAPIURL() != "https://api.github.com" {
		t.Fatalf("github defaults: err=%v url=%s", err, cfg.GitOpsAPIURL())
	}
	for name, mutate := range map[string]func(*Config){
		"unknown provider":  func(c *Config) { c.GitOpsProvider = "svn" },
		"missing token":     func(c *Config) { c.GitOpsToken = "" },
		"gitea without URL": func(c *Config) { c.GitOpsProvider = "gitea" },
		"plain HTTP":        func(c *Config) { c.GitOpsURL = "http://git.example/api/v1" },
	} {
		broken := cfg
		mutate(&broken)
		if err := broken.Validate(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestValidatePrometheusAccessSettings(t *testing.T) {
	for name, test := range map[string]struct {
		mutate func(*Config)
		want   string
	}{
		"token without a URL":  {func(cfg *Config) { cfg.PrometheusBearerTokenFile = "/var/run/secrets/token" }, "require KUBERIGHT_PROMETHEUS_URL"},
		"CA without a URL":     {func(cfg *Config) { cfg.PrometheusCAFile = "/etc/ca.crt" }, "require KUBERIGHT_PROMETHEUS_URL"},
		"tenant without a URL": {func(cfg *Config) { cfg.PrometheusTenantNamespace = "shop" }, "require KUBERIGHT_PROMETHEUS_URL"},
		"token over cleartext": {func(cfg *Config) {
			cfg.PrometheusURL, cfg.AllowInsecureUpstreams, cfg.PrometheusBearerTokenFile = "http://prometheus:9090", true, "/token"
		}, "never sent in cleartext"},
		"token over HTTPS": {func(cfg *Config) {
			cfg.PrometheusURL, cfg.PrometheusBearerTokenFile, cfg.PrometheusCAFile = "https://thanos:9091", "/token", "/ca.crt"
		}, ""},
		"tenant with HTTPS":      {func(cfg *Config) { cfg.PrometheusURL, cfg.PrometheusTenantNamespace = "https://thanos:9092", "shop" }, ""},
		"no Prometheus is valid": {func(*Config) {}, ""},
	} {
		cfg := validConfig()
		test.mutate(&cfg)
		err := cfg.Validate()
		if (test.want == "") != (err == nil) || (err != nil && !strings.Contains(err.Error(), test.want)) {
			t.Fatalf("%s: error = %v", name, err)
		}
	}
}

func TestLoadReadsPrometheusAccessAndDiscovery(t *testing.T) {
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.PrometheusDiscovery || cfg.PrometheusBearerTokenFile != "" {
		t.Fatalf("defaults: discovery=%v token=%q", cfg.PrometheusDiscovery, cfg.PrometheusBearerTokenFile)
	}
	t.Setenv("KUBERIGHT_PROMETHEUS_URL", "https://thanos-querier.openshift-monitoring.svc:9092")
	t.Setenv("KUBERIGHT_PROMETHEUS_BEARER_TOKEN_FILE", " /var/run/secrets/kubernetes.io/serviceaccount/token ")
	t.Setenv("KUBERIGHT_PROMETHEUS_CA_FILE", "/var/run/secrets/kubernetes.io/serviceaccount/service-ca.crt")
	t.Setenv("KUBERIGHT_PROMETHEUS_TENANT_NAMESPACE", "shop")
	t.Setenv("KUBERIGHT_PROMETHEUS_DISCOVERY", "false")
	if cfg, err = Load(); err != nil {
		t.Fatal(err)
	}
	if cfg.PrometheusDiscovery || cfg.PrometheusBearerTokenFile != "/var/run/secrets/kubernetes.io/serviceaccount/token" ||
		cfg.PrometheusCAFile == "" || cfg.PrometheusTenantNamespace != "shop" {
		t.Fatalf("loaded = %+v", cfg)
	}
}

func TestLoadReportsRemovedForecastingSettings(t *testing.T) {
	t.Setenv("KUBERIGHT_ML_URL", "http://kuberight-ml:8081")
	t.Setenv("KUBERIGHT_ML_API_KEY", "an-old-shared-key")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("an old forecasting setting must not stop startup: %v", err)
	}
	if strings.Join(cfg.RemovedSettings, ",") != "KUBERIGHT_ML_URL,KUBERIGHT_ML_API_KEY" {
		t.Fatalf("removed settings = %v", cfg.RemovedSettings)
	}
}

func TestLoadRunsReadOnlyWhenApplyModeIsRequested(t *testing.T) {
	t.Setenv("KUBERIGHT_OPERATION_MODE", "apply")
	t.Setenv("KUBERIGHT_AUTOMATION_IN_PLACE_RESIZE", "true")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("a removed setting must not stop startup: %v", err)
	}
	if cfg.OperationMode != ModeRecommend || strings.Join(cfg.RemovedSettings, ",") != "KUBERIGHT_AUTOMATION_IN_PLACE_RESIZE,KUBERIGHT_OPERATION_MODE=apply" {
		t.Fatalf("mode=%s removed=%v", cfg.OperationMode, cfg.RemovedSettings)
	}
	invalid := validConfig()
	invalid.OperationMode = "apply"
	if err := invalid.Validate(); err == nil {
		t.Fatal("apply mode passed validation")
	}
}

// KubeRight has no sign-in. A manifest from an older release still starts,
// and the sign-in settings it carries are reported rather than rejected.
func TestLoadIgnoresRemovedSignInSettings(t *testing.T) {
	removed := []string{"KUBERIGHT_AUTH_MODE", "KUBERIGHT_API_TOKEN", "KUBERIGHT_OIDC_USERNAME_PREFIX", "KUBERIGHT_OIDC_GROUPS_PREFIX", "KUBERIGHT_TOKEN_AUDIENCE", "KUBERIGHT_REQUIRE_DISTINCT_APPROVER"}
	for _, mode := range []string{"kubernetes", "openshift", "oidc", "static", "nonsense"} {
		t.Setenv("KUBERIGHT_AUTH_MODE", mode)
		t.Setenv("KUBERIGHT_API_TOKEN", "short")
		t.Setenv("KUBERIGHT_OIDC_USERNAME_PREFIX", "oidc:")
		t.Setenv("KUBERIGHT_OIDC_GROUPS_PREFIX", "oidc:")
		t.Setenv("KUBERIGHT_TOKEN_AUDIENCE", "kuberight")
		t.Setenv("KUBERIGHT_REQUIRE_DISTINCT_APPROVER", "not-a-bool")
		cfg, err := Load()
		if err != nil {
			t.Fatalf("KUBERIGHT_AUTH_MODE=%s: %v", mode, err)
		}
		if strings.Join(cfg.RemovedSettings, ",") != strings.Join(removed, ",") {
			t.Fatalf("KUBERIGHT_AUTH_MODE=%s: removed settings %v, want %v", mode, cfg.RemovedSettings, removed)
		}
	}
}
