package config

import (
	"strings"
	"testing"
	"time"
)

func validConfig() Config {
	return Config{
		ListenAddress:     ":8080",
		AuthMode:          "static",
		APIToken:          strings.Repeat("x", 32),
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

func TestValidateRejectsWeakStaticToken(t *testing.T) {
	cfg := validConfig()
	cfg.APIToken = "short"
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected a weak token to be rejected")
	}
}

func TestValidateRejectsUnimplementedApplyMode(t *testing.T) {
	cfg := validConfig()
	cfg.OperationMode = ModeApply
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected unimplemented apply mode to fail closed")
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
		"https ml":                    {func(c *Config) { c.MLURL = "https://ml.example:8443" }, true},
		"http ml without opt-in":      {func(c *Config) { c.MLURL = "http://kuberight-ml:8081" }, false},
		"http ml with opt-in":         {func(c *Config) { c.MLURL = "http://kuberight-ml:8081"; c.MLAllowHTTP = true }, true},
		"ml credentials in url":       {func(c *Config) { c.MLURL = "https://user:pw@ml" }, false},
		"short ml key":                {func(c *Config) { c.MLAPIKey = "short" }, false},
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
	t.Setenv("KUBERIGHT_AUTH_MODE", "static")
	t.Setenv("KUBERIGHT_API_TOKEN", strings.Repeat("t", 32))
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.RequireDistinctApprover {
		t.Fatal("static mode has one identity, so distinct approvers must default off")
	}
	if cfg.DatabaseMigrate || cfg.DatabaseMaxConns != 10 || cfg.PrometheusMaxInFlight != 8 || cfg.RecommendationWindow != DefaultRecommendationWindow || cfg.ForecastStep != DefaultForecastStep || cfg.CostCurrency != "USD" || cfg.CostConfigured() {
		t.Fatalf("unexpected defaults: %#v", cfg)
	}
	t.Setenv("KUBERIGHT_AUTH_MODE", "kubernetes")
	if cfg, err = Load(); err != nil || !cfg.RequireDistinctApprover {
		t.Fatalf("kubernetes mode must require distinct approvers: %v %v", cfg.RequireDistinctApprover, err)
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
	if cfg.RecommendationStep != DefaultRecommendationStep || cfg.ForecastWindow != DefaultForecastWindow || cfg.MLTimeout == 0 || cfg.CostCurrency != "USD" {
		t.Fatalf("Normalized() = %#v", cfg)
	}
}
