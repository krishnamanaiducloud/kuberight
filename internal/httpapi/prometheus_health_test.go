package httpapi

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/kuberight/kuberight/internal/config"
	"github.com/kuberight/kuberight/internal/metrics"
	"github.com/kuberight/kuberight/internal/usage"
)

// prometheusAnswering builds a real Prometheus client against a test server.
func prometheusAnswering(t *testing.T, timeout time.Duration, handler http.HandlerFunc) UsageSource {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	client, err := metrics.NewPrometheusClient(metrics.PrometheusOptions{Endpoint: server.URL, AllowHTTP: true, Timeout: timeout})
	if err != nil {
		t.Fatal(err)
	}
	return usage.NewSource(client)
}

func integrationStates(t *testing.T, source UsageSource) (map[string]map[string]any, []any) {
	t.Helper()
	api := newTestAPI(t, func(cfg *config.Config) { cfg.PrometheusURL = "https://thanos-querier.openshift-monitoring.svc:9091" },
		Dependencies{Usage: source, Forecaster: &fakeForecaster{}})
	_, body := api.do(t, http.MethodGet, "/api/v1/settings/integrations", "alice", "")
	byName := map[string]map[string]any{}
	for _, item := range body["items"].([]any) {
		dependency := item.(map[string]any)
		byName[dependency["name"].(string)] = dependency
	}
	coverage, _ := body["metricCoverage"].([]any)
	return byName, coverage
}

func TestPrometheusHealthReportsTheActualConnectionState(t *testing.T) {
	cases := []struct {
		name, state, forecasting, detail string
		healthy                          bool
		handler                          http.HandlerFunc
	}{
		{
			name: "authentication failed", state: "authentication_failed", forecasting: "insufficient_data", detail: "HTTP 401",
			handler: func(w http.ResponseWriter, _ *http.Request) { http.Error(w, "Unauthorized", http.StatusUnauthorized) },
		},
		{
			name: "authorization denied", state: "authorization_denied", forecasting: "insufficient_data", detail: "HTTP 403",
			handler: func(w http.ResponseWriter, _ *http.Request) { http.Error(w, "Forbidden", http.StatusForbidden) },
		},
		{
			name: "timeout", state: "timeout", forecasting: "insufficient_data", detail: "timeout",
			handler: func(w http.ResponseWriter, r *http.Request) { <-r.Context().Done() },
		},
		{
			name: "connected without container metrics", state: "connected_metrics_missing", forecasting: "insufficient_data", detail: "container_cpu_usage_seconds_total",
			handler: func(w http.ResponseWriter, r *http.Request) {
				if strings.Contains(r.URL.Query().Get("query"), "count(") {
					fmt.Fprint(w, `{"status":"success","data":{"resultType":"matrix","result":[]}}`)
					return
				}
				fmt.Fprintf(w, `{"status":"success","data":{"resultType":"matrix","result":[{"metric":{},"values":[[%d,"1"]]}]}}`, time.Now().Unix())
			},
		},
		{
			name: "connected", state: "connected", forecasting: "ready", healthy: true,
			handler: func(w http.ResponseWriter, _ *http.Request) {
				fmt.Fprintf(w, `{"status":"success","data":{"resultType":"matrix","result":[{"metric":{},"values":[[%d,"42"]]}]}}`, time.Now().Truncate(time.Second).Unix())
			},
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			statuses, coverage := integrationStates(t, prometheusAnswering(t, 300*time.Millisecond, testCase.handler))
			prometheus, forecasting := statuses["Prometheus"], statuses["Forecasting"]
			if prometheus["state"] != testCase.state || prometheus["healthy"] != testCase.healthy {
				t.Fatalf("Prometheus = %v", prometheus)
			}
			if !strings.Contains(prometheus["detail"].(string), testCase.detail) {
				t.Fatalf("Prometheus detail %q lacks %q", prometheus["detail"], testCase.detail)
			}
			if forecasting["state"] != testCase.forecasting || forecasting["healthy"] != (testCase.forecasting == "ready") {
				t.Fatalf("Forecasting = %v", forecasting)
			}
			if len(coverage) != 8 {
				t.Fatalf("each metric family must be reported: %v", coverage)
			}
			if !testCase.healthy && testCase.state != "connected_metrics_missing" {
				// Unreachable: every family is unknown, with the reason.
				first := coverage[0].(map[string]any)
				if first["state"] != "unknown" || first["failure"] == "" || first["failure"] == nil {
					t.Fatalf("coverage row = %v", first)
				}
			}
		})
	}
}
