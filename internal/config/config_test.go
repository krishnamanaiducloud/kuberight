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
