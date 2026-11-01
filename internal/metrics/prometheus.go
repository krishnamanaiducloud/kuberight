// Package metrics provides bounded, read-only metric-source clients and
// normalization primitives. It deliberately keeps missing data distinct from
// a measured zero so callers cannot manufacture healthy-looking usage data.
package metrics

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	defaultTimeout          = 10 * time.Second
	defaultMaxResponseBytes = int64(16 << 20)
	defaultMaxSeries        = 1_000
	defaultMaxSamples       = 100_000
	defaultMaxRange         = 30 * 24 * time.Hour
	defaultMaxPoints        = 11_000
	defaultMinimumCoverage  = 0.8
	maximumExpressionBytes  = 8 << 10
)

var (
	// ErrInvalidEndpoint indicates that the configured Prometheus base URL is
	// unsafe or cannot be used as an HTTP(S) endpoint.
	ErrInvalidEndpoint = errors.New("invalid Prometheus endpoint")
	// ErrInvalidOptions indicates an invalid constructor resource bound.
	ErrInvalidOptions = errors.New("invalid Prometheus client options")
	// ErrInvalidQuery indicates that a range query exceeds a client bound or
	// has internally inconsistent timestamps.
	ErrInvalidQuery = errors.New("invalid Prometheus range query")
	// ErrUpstream indicates a network failure, redirect, non-2xx response, or
	// error status reported by Prometheus.
	ErrUpstream = errors.New("Prometheus upstream unavailable")
	// ErrProtocol indicates that Prometheus returned an unusable API payload.
	ErrProtocol = errors.New("invalid Prometheus API response")
	// ErrResponseTooLarge indicates that the configured response-size bound was
	// reached before a complete payload could be read.
	ErrResponseTooLarge = errors.New("Prometheus response exceeds configured limit")
)

// QualityIssue names an independently observable problem with a time series.
// A series can carry more than one issue (for example, sparse and stale).
type QualityIssue string

const (
	IssueMissing   QualityIssue = "missing"
	IssueStale     QualityIssue = "stale"
	IssueSparse    QualityIssue = "sparse"
	IssueDuplicate QualityIssue = "duplicate"
	IssueAnomalous QualityIssue = "anomalous"
)

// Sample is a finite, in-window observation. Stale markers, non-finite values,
// malformed values, out-of-window values, and duplicates are never exposed as
// usable samples.
type Sample struct {
	Timestamp time.Time
	Value     float64
}

// DataQuality explains exactly how raw Prometheus values became normalized
// samples. MissingSamples is an estimate based on the inclusive query window;
// it is never filled with synthetic zero-valued observations.
type DataQuality struct {
	Issues             []QualityIssue `json:"issues"`
	ExpectedSamples    int            `json:"expectedSamples"`
	ReceivedSamples    int            `json:"receivedSamples"`
	AcceptedSamples    int            `json:"acceptedSamples"`
	MissingSamples     int            `json:"missingSamples"`
	StaleSamples       int            `json:"staleSamples"`
	DuplicateSamples   int            `json:"duplicateSamples"`
	AnomalousSamples   int            `json:"anomalousSamples"`
	Coverage           float64        `json:"coverage"`
	LastAcceptedSample *time.Time     `json:"lastAcceptedSample,omitempty"`
}

// Has reports whether the quality report contains issue.
func (quality DataQuality) Has(issue QualityIssue) bool {
	for _, candidate := range quality.Issues {
		if candidate == issue {
			return true
		}
	}
	return false
}

// Series is a normalized Prometheus matrix series.
type Series struct {
	Labels  map[string]string
	Samples []Sample
	Quality DataQuality
}

// RangeResult contains all normalized series and any warnings emitted by
// Prometheus. An empty Series slice means Prometheus returned no matching
// series; it must not be interpreted as a zero-valued series.
type RangeResult struct {
	Series   []Series
	Warnings []string
	// NoData is explicit because an empty successful result means no metric was
	// observed, not that the requested metric had a value of zero.
	NoData bool
}

// RangeQuery describes one bounded call to Prometheus's query_range endpoint.
type RangeQuery struct {
	Expression      string
	Start           time.Time
	End             time.Time
	Step            time.Duration
	MinimumCoverage float64
	StaleAfter      time.Duration
}

// PrometheusOptions configures the immutable upstream and resource bounds.
// The endpoint is operator-owned configuration; callers of QueryRange cannot
// override its scheme, authority, or path. HTTP is rejected unless explicitly
// enabled for an in-cluster endpoint.
type PrometheusOptions struct {
	Endpoint         string
	AllowHTTP        bool
	Timeout          time.Duration
	MaxResponseBytes int64
	MaxSeries        int
	MaxSamples       int
	MaxRange         time.Duration
	MaxPoints        int
	HTTPClient       *http.Client
}

// PrometheusClient is safe for concurrent use after construction.
type PrometheusClient struct {
	queryRangeURL    url.URL
	httpClient       *http.Client
	maxResponseBytes int64
	maxSeries        int
	maxSamples       int
	maxRange         time.Duration
	maxPoints        int
}

// NewPrometheusClient validates and freezes the configured endpoint. Redirects
// are always refused so an upstream cannot pivot a request to another host.
func NewPrometheusClient(options PrometheusOptions) (*PrometheusClient, error) {
	endpoint, err := parsePrometheusEndpoint(options.Endpoint, options.AllowHTTP)
	if err != nil {
		return nil, err
	}
	timeout, err := positiveDurationOrDefault("timeout", options.Timeout, defaultTimeout)
	if err != nil {
		return nil, err
	}
	maxResponseBytes, err := positiveInt64OrDefault("maximum response bytes", options.MaxResponseBytes, defaultMaxResponseBytes)
	if err != nil {
		return nil, err
	}
	maxSeries, err := positiveIntOrDefault("maximum series", options.MaxSeries, defaultMaxSeries)
	if err != nil {
		return nil, err
	}
	maxSamples, err := positiveIntOrDefault("maximum samples", options.MaxSamples, defaultMaxSamples)
	if err != nil {
		return nil, err
	}
	maxRange, err := positiveDurationOrDefault("maximum range", options.MaxRange, defaultMaxRange)
	if err != nil {
		return nil, err
	}
	maxPoints, err := positiveIntOrDefault("maximum points", options.MaxPoints, defaultMaxPoints)
	if err != nil {
		return nil, err
	}

	httpClient := &http.Client{}
	if options.HTTPClient != nil {
		clone := *options.HTTPClient
		httpClient = &clone
	}
	httpClient.Timeout = timeout
	httpClient.CheckRedirect = func(_ *http.Request, _ []*http.Request) error {
		return http.ErrUseLastResponse
	}

	endpoint.Path = strings.TrimRight(endpoint.Path, "/") + "/api/v1/query_range"
	endpoint.RawPath = ""
	return &PrometheusClient{
		queryRangeURL:    endpoint,
		httpClient:       httpClient,
		maxResponseBytes: maxResponseBytes,
		maxSeries:        maxSeries,
		maxSamples:       maxSamples,
		maxRange:         maxRange,
		maxPoints:        maxPoints,
	}, nil
}

// QueryRange executes a single matrix query and normalizes every returned
// series under the same explicit data-quality policy.
func (client *PrometheusClient) QueryRange(ctx context.Context, query RangeQuery) (RangeResult, error) {
	if ctx == nil {
		return RangeResult{}, fmt.Errorf("%w: context must not be nil", ErrInvalidQuery)
	}
	expectedSamples, minimumCoverage, staleAfter, err := client.validateRangeQuery(query)
	if err != nil {
		return RangeResult{}, err
	}

	requestURL := client.queryRangeURL
	parameters := requestURL.Query()
	parameters.Set("query", query.Expression)
	parameters.Set("start", query.Start.UTC().Format(time.RFC3339Nano))
	parameters.Set("end", query.End.UTC().Format(time.RFC3339Nano))
	parameters.Set("step", strconv.FormatFloat(query.Step.Seconds(), 'f', -1, 64))
	requestURL.RawQuery = parameters.Encode()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, requestURL.String(), nil)
	if err != nil {
		return RangeResult{}, fmt.Errorf("%w: could not construct request", ErrInvalidQuery)
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("User-Agent", "KubeRight/Prometheus")

	response, err := client.httpClient.Do(request)
	if err != nil {
		return RangeResult{}, upstreamRequestError{cause: err}
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return RangeResult{}, fmt.Errorf("%w: HTTP status %d", ErrUpstream, response.StatusCode)
	}
	payload, err := io.ReadAll(io.LimitReader(response.Body, client.maxResponseBytes+1))
	if err != nil {
		return RangeResult{}, upstreamRequestError{cause: err}
	}
	if int64(len(payload)) > client.maxResponseBytes {
		return RangeResult{}, ErrResponseTooLarge
	}

	envelope, err := decodePrometheusEnvelope(payload)
	if err != nil {
		return RangeResult{}, err
	}
	switch envelope.Status {
	case "success":
	case "error":
		return RangeResult{}, ErrUpstream
	default:
		return RangeResult{}, fmt.Errorf("%w: unrecognized API status", ErrProtocol)
	}
	if envelope.Data.ResultType != "matrix" {
		return RangeResult{}, fmt.Errorf("%w: expected matrix result", ErrProtocol)
	}
	if len(envelope.Data.Result) > client.maxSeries {
		return RangeResult{}, fmt.Errorf("%w: series limit exceeded", ErrProtocol)
	}

	totalSamples := 0
	result := RangeResult{
		Series:   make([]Series, 0, len(envelope.Data.Result)),
		Warnings: append([]string(nil), envelope.Warnings...),
		NoData:   len(envelope.Data.Result) == 0,
	}
	for _, rawSeries := range envelope.Data.Result {
		if len(rawSeries.Values) > client.maxSamples-totalSamples {
			return RangeResult{}, fmt.Errorf("%w: sample limit exceeded", ErrProtocol)
		}
		totalSamples += len(rawSeries.Values)
		result.Series = append(result.Series, normalizeSeries(
			rawSeries,
			query.Start,
			query.End,
			expectedSamples,
			minimumCoverage,
			staleAfter,
		))
	}
	return result, nil
}

func (client *PrometheusClient) validateRangeQuery(query RangeQuery) (int, float64, time.Duration, error) {
	expression := strings.TrimSpace(query.Expression)
	if expression == "" || expression != query.Expression || len(expression) > maximumExpressionBytes || !utf8.ValidString(expression) {
		return 0, 0, 0, fmt.Errorf("%w: expression is empty, malformed, or too long", ErrInvalidQuery)
	}
	if query.Start.IsZero() || query.End.IsZero() || query.Start.Year() < 1 || query.Start.Year() > 9999 || query.End.Year() < 1 || query.End.Year() > 9999 {
		return 0, 0, 0, fmt.Errorf("%w: start and end must be representable timestamps", ErrInvalidQuery)
	}
	if query.End.Before(query.Start) {
		return 0, 0, 0, fmt.Errorf("%w: end precedes start", ErrInvalidQuery)
	}
	queryRange := query.End.Sub(query.Start)
	if queryRange > client.maxRange {
		return 0, 0, 0, fmt.Errorf("%w: range exceeds configured limit", ErrInvalidQuery)
	}
	if query.Step <= 0 {
		return 0, 0, 0, fmt.Errorf("%w: step must be positive", ErrInvalidQuery)
	}
	expectedSamples64 := int64(queryRange/query.Step) + 1
	if expectedSamples64 > int64(client.maxPoints) {
		return 0, 0, 0, fmt.Errorf("%w: point count exceeds configured limit", ErrInvalidQuery)
	}

	minimumCoverage := query.MinimumCoverage
	if minimumCoverage == 0 {
		minimumCoverage = defaultMinimumCoverage
	}
	if math.IsNaN(minimumCoverage) || math.IsInf(minimumCoverage, 0) || minimumCoverage <= 0 || minimumCoverage > 1 {
		return 0, 0, 0, fmt.Errorf("%w: minimum coverage must be greater than zero and at most one", ErrInvalidQuery)
	}
	staleAfter := query.StaleAfter
	if staleAfter < 0 {
		return 0, 0, 0, fmt.Errorf("%w: stale-after duration must not be negative", ErrInvalidQuery)
	}
	if staleAfter == 0 {
		if query.Step > time.Duration(math.MaxInt64/2) {
			staleAfter = query.Step
		} else {
			staleAfter = 2 * query.Step
		}
	}
	return int(expectedSamples64), minimumCoverage, staleAfter, nil
}

type prometheusEnvelope struct {
	Status string `json:"status"`
	Data   struct {
		ResultType string             `json:"resultType"`
		Result     []prometheusSeries `json:"result"`
	} `json:"data"`
	Warnings []string `json:"warnings"`
}

type prometheusSeries struct {
	Metric map[string]string `json:"metric"`
	Values []json.RawMessage `json:"values"`
}

func decodePrometheusEnvelope(payload []byte) (prometheusEnvelope, error) {
	var envelope prometheusEnvelope
	decoder := json.NewDecoder(bytes.NewReader(payload))
	if err := decoder.Decode(&envelope); err != nil {
		return prometheusEnvelope{}, fmt.Errorf("%w: malformed JSON", ErrProtocol)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return prometheusEnvelope{}, fmt.Errorf("%w: multiple JSON values", ErrProtocol)
	}
	return envelope, nil
}

func normalizeSeries(raw prometheusSeries, start, end time.Time, expectedSamples int, minimumCoverage float64, staleAfter time.Duration) Series {
	series := Series{
		Labels:  cloneLabels(raw.Metric),
		Samples: make([]Sample, 0, min(len(raw.Values), expectedSamples)),
		Quality: DataQuality{
			Issues:          make([]QualityIssue, 0, 5),
			ExpectedSamples: expectedSamples,
			ReceivedSamples: len(raw.Values),
		},
	}
	seen := make(map[time.Time]float64, min(len(raw.Values), expectedSamples))
	for _, encoded := range raw.Values {
		timestamp, valueText, ok := decodePrometheusValue(encoded)
		if !ok {
			series.Quality.AnomalousSamples++
			continue
		}
		pointTime, ok := prometheusTimestamp(timestamp)
		if !ok || pointTime.Before(start) || pointTime.After(end) {
			series.Quality.AnomalousSamples++
			continue
		}
		value, err := strconv.ParseFloat(valueText, 64)
		if err != nil {
			series.Quality.AnomalousSamples++
			continue
		}
		if math.IsNaN(value) {
			series.Quality.StaleSamples++
			continue
		}
		if math.IsInf(value, 0) {
			series.Quality.AnomalousSamples++
			continue
		}
		if previous, exists := seen[pointTime]; exists {
			series.Quality.DuplicateSamples++
			if previous != value {
				series.Quality.AnomalousSamples++
			}
			continue
		}
		seen[pointTime] = value
		series.Samples = append(series.Samples, Sample{Timestamp: pointTime, Value: value})
	}
	sort.Slice(series.Samples, func(left, right int) bool {
		return series.Samples[left].Timestamp.Before(series.Samples[right].Timestamp)
	})

	series.Quality.AcceptedSamples = len(series.Samples)
	series.Quality.MissingSamples = max(expectedSamples-len(series.Samples), 0)
	series.Quality.Coverage = math.Min(float64(len(series.Samples))/float64(expectedSamples), 1)
	if len(series.Samples) == 0 {
		series.Quality.Issues = append(series.Quality.Issues, IssueMissing)
	} else {
		last := series.Samples[len(series.Samples)-1].Timestamp
		series.Quality.LastAcceptedSample = &last
	}
	if series.Quality.StaleSamples > 0 || (series.Quality.LastAcceptedSample != nil && series.Quality.LastAcceptedSample.Before(end) && end.Sub(*series.Quality.LastAcceptedSample) >= staleAfter) {
		series.Quality.Issues = append(series.Quality.Issues, IssueStale)
	}
	if len(series.Samples) > 0 && series.Quality.Coverage < minimumCoverage {
		series.Quality.Issues = append(series.Quality.Issues, IssueSparse)
	}
	if series.Quality.DuplicateSamples > 0 {
		series.Quality.Issues = append(series.Quality.Issues, IssueDuplicate)
	}
	if series.Quality.AnomalousSamples > 0 {
		series.Quality.Issues = append(series.Quality.Issues, IssueAnomalous)
	}
	return series
}

func decodePrometheusValue(encoded json.RawMessage) (float64, string, bool) {
	var pair []json.RawMessage
	if err := json.Unmarshal(encoded, &pair); err != nil || len(pair) != 2 {
		return 0, "", false
	}
	var timestamp float64
	if err := json.Unmarshal(pair[0], &timestamp); err != nil || math.IsNaN(timestamp) || math.IsInf(timestamp, 0) {
		return 0, "", false
	}
	var value string
	if err := json.Unmarshal(pair[1], &value); err != nil {
		return 0, "", false
	}
	return timestamp, value, true
}

func prometheusTimestamp(value float64) (time.Time, bool) {
	// Restrict values to the range formatted by validateRangeQuery. This also
	// prevents implementation-defined float-to-int conversion at the extremes.
	const (
		minimumUnixSecond = -62135596800.0
		maximumUnixSecond = 253402300799.999999999
	)
	if value < minimumUnixSecond || value > maximumUnixSecond {
		return time.Time{}, false
	}
	seconds := math.Floor(value)
	nanoseconds := math.Round((value - seconds) * float64(time.Second))
	if nanoseconds == float64(time.Second) {
		seconds++
		nanoseconds = 0
	}
	return time.Unix(int64(seconds), int64(nanoseconds)).UTC(), true
}

func parsePrometheusEndpoint(raw string, allowHTTP bool) (url.URL, error) {
	if raw == "" || raw != strings.TrimSpace(raw) {
		return url.URL{}, fmt.Errorf("%w: endpoint must be non-empty and contain no surrounding whitespace", ErrInvalidEndpoint)
	}
	endpoint, err := url.Parse(raw)
	if err != nil || !endpoint.IsAbs() || endpoint.Opaque != "" || endpoint.Host == "" || endpoint.Hostname() == "" {
		return url.URL{}, fmt.Errorf("%w: endpoint must be an absolute hierarchical URL", ErrInvalidEndpoint)
	}
	endpoint.Scheme = strings.ToLower(endpoint.Scheme)
	if endpoint.Scheme != "https" && endpoint.Scheme != "http" {
		return url.URL{}, fmt.Errorf("%w: endpoint scheme must be HTTP or HTTPS", ErrInvalidEndpoint)
	}
	if endpoint.Scheme == "http" && !allowHTTP {
		return url.URL{}, fmt.Errorf("%w: cleartext HTTP was not explicitly enabled", ErrInvalidEndpoint)
	}
	if endpoint.User != nil || endpoint.RawQuery != "" || endpoint.ForceQuery || endpoint.Fragment != "" || endpoint.RawFragment != "" {
		return url.URL{}, fmt.Errorf("%w: credentials, query strings, and fragments are not permitted", ErrInvalidEndpoint)
	}
	if endpoint.RawPath != "" || strings.Contains(endpoint.Path, "\\") {
		return url.URL{}, fmt.Errorf("%w: encoded or backslash path is not permitted", ErrInvalidEndpoint)
	}
	for _, segment := range strings.Split(endpoint.Path, "/") {
		if segment == "." || segment == ".." {
			return url.URL{}, fmt.Errorf("%w: dot path segment is not permitted", ErrInvalidEndpoint)
		}
	}
	return *endpoint, nil
}

func cloneLabels(labels map[string]string) map[string]string {
	cloned := make(map[string]string, len(labels))
	for key, value := range labels {
		cloned[key] = value
	}
	return cloned
}

func positiveIntOrDefault(name string, value, fallback int) (int, error) {
	if value == 0 {
		return fallback, nil
	}
	if value < 0 {
		return 0, fmt.Errorf("%w: %s must be positive", ErrInvalidOptions, name)
	}
	return value, nil
}

func positiveInt64OrDefault(name string, value, fallback int64) (int64, error) {
	if value == 0 {
		return fallback, nil
	}
	if value < 0 {
		return 0, fmt.Errorf("%w: %s must be positive", ErrInvalidOptions, name)
	}
	if value == math.MaxInt64 {
		return 0, fmt.Errorf("%w: %s is too large", ErrInvalidOptions, name)
	}
	return value, nil
}

func positiveDurationOrDefault(name string, value, fallback time.Duration) (time.Duration, error) {
	if value == 0 {
		return fallback, nil
	}
	if value < 0 {
		return 0, fmt.Errorf("%w: %s must be positive", ErrInvalidOptions, name)
	}
	return value, nil
}

type upstreamRequestError struct {
	cause error
}

func (err upstreamRequestError) Error() string {
	return "Prometheus request failed"
}

func (err upstreamRequestError) Unwrap() []error {
	return []error{ErrUpstream, err.cause}
}
