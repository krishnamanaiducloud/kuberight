package main

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"time"

	"github.com/kuberight/kuberight/internal/config"
	"github.com/kuberight/kuberight/internal/metrics"
	"github.com/kuberight/kuberight/internal/usage"
)

// checkResult is one step of prometheus-check. It never holds the token.
type checkResult struct {
	Check      string `json:"check"`
	OK         bool   `json:"ok"`
	Failure    string `json:"failure,omitempty"`
	Status     int    `json:"status,omitempty"`
	ErrorType  string `json:"prometheusErrorType,omitempty"`
	Message    string `json:"message,omitempty"`
	DurationMS int64  `json:"durationMs"`
	Series     int    `json:"series,omitempty"`
	Empty      bool   `json:"emptyResult,omitempty"`
}

type checkReport struct {
	Source          string                 `json:"source"`
	EndpointHost    string                 `json:"endpointHost"`
	BearerToken     bool                   `json:"bearerToken"`
	BearerTokenFile string                 `json:"bearerTokenFile,omitempty"`
	CABundle        string                 `json:"caBundle,omitempty"`
	TenantNamespace string                 `json:"tenantNamespace,omitempty"`
	ViaProxy        bool                   `json:"viaProxy"`
	Timeout         string                 `json:"timeout"`
	Checks          []checkResult          `json:"checks"`
	Coverage        []usage.MetricCoverage `json:"metricCoverage,omitempty"`
	Error           string                 `json:"error,omitempty"`
}

// prometheusCheck resolves Prometheus exactly as the server does, runs the
// readiness query, one real CPU query, and the metric-family probes, and
// prints the outcome as JSON. It returns 0 when Prometheus answered.
func prometheusCheck(out io.Writer) int {
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	report := checkReport{}
	encode := func(code int) int {
		encoder := json.NewEncoder(out)
		encoder.SetIndent("", "  ")
		_ = encoder.Encode(report)
		return code
	}
	cfg, err := config.Load()
	if err != nil {
		report.Error = "configuration is invalid: " + err.Error()
		return encode(1)
	}
	cfg = resolvePrometheus(cfg, logger)
	report.Source, report.BearerToken, report.BearerTokenFile = cfg.PrometheusSource, cfg.PrometheusBearerTokenFile != "", cfg.PrometheusBearerTokenFile
	report.CABundle, report.TenantNamespace, report.Timeout = cfg.PrometheusCAFile, cfg.PrometheusTenantNamespace, cfg.PrometheusTimeout.String()
	if cfg.PrometheusURL == "" {
		report.Error = "Prometheus is not configured and no cluster monitoring stack was found"
		return encode(1)
	}
	client, err := metrics.NewPrometheusClient(metrics.PrometheusOptions{
		Endpoint: cfg.PrometheusURL, AllowHTTP: cfg.AllowInsecureUpstreams, Timeout: cfg.PrometheusTimeout,
		BearerTokenFile: cfg.PrometheusBearerTokenFile, CAFile: cfg.PrometheusCAFile, TenantNamespace: cfg.PrometheusTenantNamespace,
	})
	if err != nil {
		report.Error = "the Prometheus client could not be built: " + err.Error()
		return encode(1)
	}
	report.EndpointHost, report.ViaProxy = client.Endpoint()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	at := time.Now().UTC().Truncate(time.Second)
	run := func(name, expression string) checkResult {
		started := time.Now()
		result, err := client.QueryRange(ctx, metrics.RangeQuery{Name: name, Expression: expression, Start: at, End: at, Step: time.Minute})
		check := checkResult{Check: name, DurationMS: time.Since(started).Milliseconds()}
		if err != nil {
			check.Failure, check.Message = string(metrics.FailureOf(err)), err.Error()
			if failure, ok := metrics.AsQueryError(err); ok {
				check.Status, check.ErrorType, check.Message = failure.StatusCode, failure.ErrorType, failure.Message
			}
			return check
		}
		check.OK, check.Series, check.Empty = true, len(result.Series), result.NoData
		return check
	}
	readiness := run("readiness", "vector(1)")
	report.Checks = append(report.Checks, readiness,
		run("cpu_usage", `topk(5, sum by (namespace) (rate(container_cpu_usage_seconds_total{container!="",container!="POD"}[5m])))`))
	if readiness.OK {
		report.Coverage = usage.NewSource(client).MetricCoverage(ctx)
		return encode(0)
	}
	return encode(1)
}
