package metrics

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestQueryRangeReturnsNormalizedFiniteSamples(t *testing.T) {
	start := time.Date(2026, time.October, 21, 12, 0, 0, 0, time.UTC)
	expression := `sum(rate(container_cpu_usage_seconds_total{namespace="shop"}[5m])) by (pod)`
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodGet {
			t.Errorf("method = %s, want GET", request.Method)
		}
		if request.URL.Path != "/prometheus/api/v1/query_range" {
			t.Errorf("path = %q", request.URL.Path)
		}
		if got := request.URL.Query().Get("query"); got != expression {
			t.Errorf("query = %q", got)
		}
		if got := request.URL.Query().Get("start"); got != start.Format(time.RFC3339Nano) {
			t.Errorf("start = %q", got)
		}
		if got := request.URL.Query().Get("step"); got != "60" {
			t.Errorf("step = %q", got)
		}
		if got := request.Header.Get("Accept"); got != "application/json" {
			t.Errorf("Accept = %q", got)
		}
		writer.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(writer, `{"status":"success","data":{"resultType":"matrix","result":[{"metric":{"pod":"api-1","namespace":"shop"},"values":[%s,%s,%s]}]},"warnings":["partial response warning"]}`,
			prometheusValue(start.Add(2*time.Minute), "3.5"),
			prometheusValue(start, "1.5"),
			prometheusValue(start.Add(time.Minute), "2.5"),
		)
	}))
	defer server.Close()

	client := mustPrometheusClient(t, PrometheusOptions{Endpoint: server.URL + "/prometheus/", AllowHTTP: true})
	result, err := client.QueryRange(context.Background(), RangeQuery{
		Expression: expression,
		Start:      start,
		End:        start.Add(2 * time.Minute),
		Step:       time.Minute,
	})
	if err != nil {
		t.Fatalf("QueryRange() error = %v", err)
	}
	if len(result.Series) != 1 {
		t.Fatalf("series count = %d", len(result.Series))
	}
	series := result.Series[0]
	if len(series.Samples) != 3 || series.Samples[0].Value != 1.5 || series.Samples[2].Value != 3.5 {
		t.Fatalf("samples = %#v", series.Samples)
	}
	if series.Labels["pod"] != "api-1" || len(series.Quality.Issues) != 0 {
		t.Fatalf("series = %#v", series)
	}
	if series.Quality.ExpectedSamples != 3 || series.Quality.AcceptedSamples != 3 || series.Quality.Coverage != 1 {
		t.Fatalf("quality = %#v", series.Quality)
	}
	if len(result.Warnings) != 1 {
		t.Fatalf("warnings = %#v", result.Warnings)
	}
}

func TestQueryRangeKeepsMissingDataDistinctFromZero(t *testing.T) {
	start := time.Date(2026, time.October, 21, 12, 0, 0, 0, time.UTC)
	series := queryOneSeries(t, `{"metric":{"pod":"missing"},"values":[]}`, RangeQuery{
		Expression: "up",
		Start:      start,
		End:        start.Add(2 * time.Minute),
		Step:       time.Minute,
	})
	if len(series.Samples) != 0 {
		t.Fatalf("missing series acquired synthetic samples: %#v", series.Samples)
	}
	if !series.Quality.Has(IssueMissing) || series.Quality.Has(IssueSparse) {
		t.Fatalf("issues = %v", series.Quality.Issues)
	}
	if series.Quality.MissingSamples != 3 || series.Quality.Coverage != 0 || series.Quality.LastAcceptedSample != nil {
		t.Fatalf("quality = %#v", series.Quality)
	}
}

func TestQueryRangeMarksAQueryWithNoMatchingSeriesAsNoData(t *testing.T) {
	client, closeServer := clientReturning(t, `{"status":"success","data":{"resultType":"matrix","result":[]}}`, PrometheusOptions{})
	defer closeServer()
	result, err := client.QueryRange(context.Background(), validRangeQuery())
	if err != nil {
		t.Fatalf("QueryRange() error = %v", err)
	}
	if !result.NoData || len(result.Series) != 0 {
		t.Fatalf("result = %#v", result)
	}
}

func TestQueryRangeMarksStaleMarkersAndTrailingGap(t *testing.T) {
	start := time.Date(2026, time.October, 21, 12, 0, 0, 0, time.UTC)
	raw := fmt.Sprintf(`{"metric":{"pod":"stale"},"values":[%s,%s,%s]}`,
		prometheusValue(start, "1"),
		prometheusValue(start.Add(time.Minute), "2"),
		prometheusValue(start.Add(2*time.Minute), "NaN"),
	)
	series := queryOneSeries(t, raw, RangeQuery{
		Expression: "up",
		Start:      start,
		End:        start.Add(4 * time.Minute),
		Step:       time.Minute,
		StaleAfter: 2 * time.Minute,
	})
	if !series.Quality.Has(IssueStale) || !series.Quality.Has(IssueSparse) {
		t.Fatalf("issues = %v", series.Quality.Issues)
	}
	if series.Quality.StaleSamples != 1 || series.Quality.AcceptedSamples != 2 || series.Quality.MissingSamples != 3 {
		t.Fatalf("quality = %#v", series.Quality)
	}
	if len(series.Samples) != 2 {
		t.Fatalf("stale marker was exposed as a usable sample: %#v", series.Samples)
	}
}

func TestQueryRangeMarksSparseSeriesWithoutCallingItMissing(t *testing.T) {
	start := time.Date(2026, time.October, 21, 12, 0, 0, 0, time.UTC)
	raw := fmt.Sprintf(`{"metric":{"pod":"sparse"},"values":[%s,%s,%s]}`,
		prometheusValue(start, "0"),
		prometheusValue(start.Add(2*time.Minute), "2"),
		prometheusValue(start.Add(4*time.Minute), "4"),
	)
	series := queryOneSeries(t, raw, RangeQuery{
		Expression:      "up",
		Start:           start,
		End:             start.Add(4 * time.Minute),
		Step:            time.Minute,
		MinimumCoverage: 0.8,
		StaleAfter:      10 * time.Minute,
	})
	if !series.Quality.Has(IssueSparse) || series.Quality.Has(IssueMissing) || series.Quality.Has(IssueStale) {
		t.Fatalf("issues = %v", series.Quality.Issues)
	}
	if series.Quality.Coverage != 0.6 || series.Quality.MissingSamples != 2 {
		t.Fatalf("quality = %#v", series.Quality)
	}
	if series.Samples[0].Value != 0 {
		t.Fatal("a real measured zero must remain a valid sample")
	}
}

func TestQueryRangeDeduplicatesTimestamps(t *testing.T) {
	start := time.Date(2026, time.October, 21, 12, 0, 0, 0, time.UTC)
	raw := fmt.Sprintf(`{"metric":{"pod":"duplicate"},"values":[%s,%s,%s]}`,
		prometheusValue(start, "1"),
		prometheusValue(start, "1"),
		prometheusValue(start.Add(time.Minute), "2"),
	)
	series := queryOneSeries(t, raw, RangeQuery{
		Expression: "up",
		Start:      start,
		End:        start.Add(time.Minute),
		Step:       time.Minute,
	})
	if len(series.Samples) != 2 || !series.Quality.Has(IssueDuplicate) || series.Quality.Has(IssueSparse) {
		t.Fatalf("series = %#v", series)
	}
	if series.Quality.DuplicateSamples != 1 || series.Quality.Coverage != 1 {
		t.Fatalf("quality = %#v", series.Quality)
	}
}

func TestQueryRangeRejectsAnomalousValuesWithoutLosingValidData(t *testing.T) {
	start := time.Date(2026, time.October, 21, 12, 0, 0, 0, time.UTC)
	raw := fmt.Sprintf(`{"metric":{"pod":"anomalous"},"values":[%s,%s,%s,%s,%s,[123]]}`,
		prometheusValue(start, "1"),
		prometheusValue(start, "9"),
		prometheusValue(start.Add(time.Minute), "+Inf"),
		prometheusValue(start.Add(2*time.Minute), "not-a-number"),
		prometheusValue(start.Add(3*time.Minute), "3"),
	)
	series := queryOneSeries(t, raw, RangeQuery{
		Expression: "up",
		Start:      start,
		End:        start.Add(2 * time.Minute),
		Step:       time.Minute,
		StaleAfter: 10 * time.Minute,
	})
	if len(series.Samples) != 1 || series.Samples[0].Value != 1 {
		t.Fatalf("samples = %#v", series.Samples)
	}
	if !series.Quality.Has(IssueAnomalous) || !series.Quality.Has(IssueDuplicate) || !series.Quality.Has(IssueSparse) {
		t.Fatalf("issues = %v", series.Quality.Issues)
	}
	if series.Quality.AnomalousSamples != 5 || series.Quality.DuplicateSamples != 1 {
		t.Fatalf("quality = %#v", series.Quality)
	}
}

func TestNewPrometheusClientRejectsUnsafeEndpoints(t *testing.T) {
	tests := []string{
		"",
		" https://prometheus.example",
		"prometheus.example",
		"file:///etc/passwd",
		"javascript:alert(1)",
		"https://user:password@prometheus.example",
		"https://prometheus.example?target=https://169.254.169.254",
		"https://prometheus.example#fragment",
		"https://prometheus.example/a/../admin",
		"https://prometheus.example/%2e%2e/admin",
		"https://prometheus.example/a\\b",
		"http://prometheus.monitoring.svc:9090",
	}
	for _, endpoint := range tests {
		t.Run(url.PathEscape(endpoint), func(t *testing.T) {
			_, err := NewPrometheusClient(PrometheusOptions{Endpoint: endpoint})
			if !errors.Is(err, ErrInvalidEndpoint) {
				t.Fatalf("NewPrometheusClient(%q) error = %v", endpoint, err)
			}
		})
	}

	if _, err := NewPrometheusClient(PrometheusOptions{
		Endpoint:  "http://prometheus.monitoring.svc:9090/base",
		AllowHTTP: true,
	}); err != nil {
		t.Fatalf("explicitly allowed in-cluster HTTP endpoint rejected: %v", err)
	}
}

func TestQueryCannotOverrideConfiguredOriginOrEndpoint(t *testing.T) {
	start := time.Date(2026, time.October, 21, 12, 0, 0, 0, time.UTC)
	expression := `up{url="https://169.254.169.254/latest/meta-data?x=1&start=0"}`
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Host != strings.TrimPrefix(server.URL, "http://") {
			t.Errorf("unexpected Host %q", request.Host)
		}
		if request.URL.Path != "/tenant/api/v1/query_range" {
			t.Errorf("path = %q", request.URL.Path)
		}
		if got := request.URL.Query().Get("query"); got != expression {
			t.Errorf("query was not encoded as one parameter: %q", got)
		}
		if got := request.URL.Query().Get("start"); got != start.Format(time.RFC3339Nano) {
			t.Errorf("start was overridden: %q", got)
		}
		fmt.Fprint(writer, `{"status":"success","data":{"resultType":"matrix","result":[]}}`)
	}))
	defer server.Close()

	client := mustPrometheusClient(t, PrometheusOptions{Endpoint: server.URL + "/tenant", AllowHTTP: true})
	_, err := client.QueryRange(context.Background(), RangeQuery{Expression: expression, Start: start, End: start, Step: time.Minute})
	if err != nil {
		t.Fatalf("QueryRange() error = %v", err)
	}
}

func TestPrometheusClientRefusesRedirects(t *testing.T) {
	var redirectedRequests atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		redirectedRequests.Add(1)
	}))
	defer target.Close()
	source := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		http.Redirect(writer, request, target.URL+"/private", http.StatusTemporaryRedirect)
	}))
	defer source.Close()

	client := mustPrometheusClient(t, PrometheusOptions{Endpoint: source.URL, AllowHTTP: true})
	_, err := client.QueryRange(context.Background(), validRangeQuery())
	if !errors.Is(err, ErrUpstream) {
		t.Fatalf("QueryRange() error = %v", err)
	}
	if redirectedRequests.Load() != 0 {
		t.Fatal("client followed a cross-origin redirect")
	}
}

func TestQueryRangeEnforcesResourceBounds(t *testing.T) {
	t.Run("range", func(t *testing.T) {
		client := mustPrometheusClient(t, PrometheusOptions{Endpoint: "https://prometheus.example", MaxRange: time.Minute})
		query := validRangeQuery()
		query.End = query.Start.Add(2 * time.Minute)
		if _, err := client.QueryRange(context.Background(), query); !errors.Is(err, ErrInvalidQuery) {
			t.Fatalf("error = %v", err)
		}
	})

	t.Run("points", func(t *testing.T) {
		client := mustPrometheusClient(t, PrometheusOptions{Endpoint: "https://prometheus.example", MaxPoints: 2})
		query := validRangeQuery()
		query.End = query.Start.Add(2 * time.Minute)
		if _, err := client.QueryRange(context.Background(), query); !errors.Is(err, ErrInvalidQuery) {
			t.Fatalf("error = %v", err)
		}
	})

	t.Run("response bytes", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
			fmt.Fprint(writer, strings.Repeat("x", 65))
		}))
		defer server.Close()
		client := mustPrometheusClient(t, PrometheusOptions{Endpoint: server.URL, AllowHTTP: true, MaxResponseBytes: 64})
		if _, err := client.QueryRange(context.Background(), validRangeQuery()); !errors.Is(err, ErrResponseTooLarge) {
			t.Fatalf("error = %v", err)
		}
	})

	t.Run("series", func(t *testing.T) {
		body := `{"status":"success","data":{"resultType":"matrix","result":[{"metric":{},"values":[]},{"metric":{},"values":[]}]}}`
		client, closeServer := clientReturning(t, body, PrometheusOptions{MaxSeries: 1})
		defer closeServer()
		if _, err := client.QueryRange(context.Background(), validRangeQuery()); !errors.Is(err, ErrProtocol) {
			t.Fatalf("error = %v", err)
		}
	})

	t.Run("samples", func(t *testing.T) {
		query := validRangeQuery()
		body := fmt.Sprintf(`{"status":"success","data":{"resultType":"matrix","result":[{"metric":{},"values":[%s,%s]}]}}`,
			prometheusValue(query.Start, "1"), prometheusValue(query.End, "2"))
		client, closeServer := clientReturning(t, body, PrometheusOptions{MaxSamples: 1})
		defer closeServer()
		if _, err := client.QueryRange(context.Background(), query); !errors.Is(err, ErrProtocol) {
			t.Fatalf("error = %v", err)
		}
	})
}

func TestQueryRangeReturnsSanitizedUpstreamErrors(t *testing.T) {
	const secret = "token=do-not-log-this"
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.WriteHeader(http.StatusBadGateway)
		fmt.Fprint(writer, `{"status":"error","error":"`+secret+`"}`)
	}))
	defer server.Close()
	client := mustPrometheusClient(t, PrometheusOptions{Endpoint: server.URL, AllowHTTP: true})

	query := validRangeQuery()
	query.Expression = `up{secret="` + secret + `"}`
	_, err := client.QueryRange(context.Background(), query)
	if !errors.Is(err, ErrUpstream) {
		t.Fatalf("error = %v", err)
	}
	if strings.Contains(err.Error(), secret) || strings.Contains(err.Error(), query.Expression) {
		t.Fatalf("error leaked request data: %q", err)
	}
}

func TestQueryRangePreservesCancellationWithoutLeakingTheRequest(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(writer, `{"status":"success","data":{"resultType":"matrix","result":[]}}`)
	}))
	defer server.Close()
	client := mustPrometheusClient(t, PrometheusOptions{Endpoint: server.URL, AllowHTTP: true})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	query := validRangeQuery()
	query.Expression = `up{credential="do-not-log"}`
	_, err := client.QueryRange(ctx, query)
	if !errors.Is(err, context.Canceled) || !errors.Is(err, ErrUpstream) {
		t.Fatalf("error = %v", err)
	}
	if strings.Contains(err.Error(), query.Expression) {
		t.Fatalf("error leaked expression: %q", err)
	}
}

func TestQueryRangeRejectsMalformedPayloads(t *testing.T) {
	tests := []string{
		`not-json`,
		`{"status":"success","data":{"resultType":"vector","result":[]}}`,
		`{"status":"error","errorType":"bad_data","error":"sensitive detail"}`,
		`{"status":"success","data":{"resultType":"matrix","result":[]}} {}`,
	}
	for _, body := range tests {
		client, closeServer := clientReturning(t, body, PrometheusOptions{})
		_, err := client.QueryRange(context.Background(), validRangeQuery())
		closeServer()
		if err == nil {
			t.Fatalf("body %q was accepted", body)
		}
	}
}

func queryOneSeries(t *testing.T, rawSeries string, query RangeQuery) Series {
	t.Helper()
	body := `{"status":"success","data":{"resultType":"matrix","result":[` + rawSeries + `]}}`
	client, closeServer := clientReturning(t, body, PrometheusOptions{})
	defer closeServer()
	result, err := client.QueryRange(context.Background(), query)
	if err != nil {
		t.Fatalf("QueryRange() error = %v", err)
	}
	if len(result.Series) != 1 {
		t.Fatalf("series count = %d", len(result.Series))
	}
	return result.Series[0]
}

func clientReturning(t *testing.T, body string, options PrometheusOptions) (*PrometheusClient, func()) {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		fmt.Fprint(writer, body)
	}))
	options.Endpoint = server.URL
	options.AllowHTTP = true
	return mustPrometheusClient(t, options), server.Close
}

func mustPrometheusClient(t *testing.T, options PrometheusOptions) *PrometheusClient {
	t.Helper()
	client, err := NewPrometheusClient(options)
	if err != nil {
		t.Fatalf("NewPrometheusClient() error = %v", err)
	}
	return client
}

func validRangeQuery() RangeQuery {
	start := time.Date(2026, time.October, 21, 12, 0, 0, 0, time.UTC)
	return RangeQuery{Expression: "up", Start: start, End: start.Add(time.Minute), Step: time.Minute}
}

func prometheusValue(timestamp time.Time, value string) string {
	return fmt.Sprintf(`[%d,%q]`, timestamp.Unix(), value)
}
