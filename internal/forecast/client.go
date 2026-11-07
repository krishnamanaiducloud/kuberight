// Package forecast is a bounded HTTP client for the KubeRight forecasting
// service. The forecasting service is advisory: its output never changes a
// recommendation or a Kubernetes object.
package forecast

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
	"strings"
	"time"
)

const maxResponseBytes = 4 << 20

var (
	// ErrUnavailable indicates a network failure or server error.
	ErrUnavailable = errors.New("forecasting service is unavailable")
	// ErrBusy indicates that the service rejected the request for capacity.
	ErrBusy = errors.New("forecasting service is busy")
	// ErrRejected indicates that the service rejected the request content.
	ErrRejected = errors.New("forecasting service rejected the request")
	// ErrProtocol indicates an unusable response.
	ErrProtocol = errors.New("invalid forecasting service response")
)

// Request mirrors the forecasting service's ForecastRequest contract.
type Request struct {
	Samples         []float64   `json:"samples"`
	Timestamps      []time.Time `json:"timestamps,omitempty"`
	Horizon         int         `json:"horizon"`
	IntervalSeconds int         `json:"interval_seconds"`
	Resource        string      `json:"resource"`
	ConfidenceLevel float64     `json:"confidence_level"`
	// SeriesID is an opaque, stable workload-UID/container/resource identity.
	// Persist asks the service to retain and safely reuse model state for that
	// exact identity. Callers must leave both unset when workload UID evidence
	// is unavailable so recreated same-name workloads cannot share a model.
	SeriesID string `json:"series_id,omitempty"`
	Persist  bool   `json:"persist,omitempty"`
}

// Interval is a forecast confidence interval.
type Interval struct {
	Lower float64 `json:"lower"`
	Upper float64 `json:"upper"`
}

// Response contains the fields KubeRight relies on; diagnostic sections are
// passed through unchanged for display.
type Response struct {
	Predictions         []float64       `json:"predictions,omitempty"`
	ConfidenceIntervals []Interval      `json:"confidenceIntervals,omitempty"`
	Model               string          `json:"model"`
	ModelVersion        string          `json:"modelVersion"`
	FallbackUsed        bool            `json:"fallbackUsed"`
	FallbackReasons     []string        `json:"fallbackReasons"`
	Confidence          float64         `json:"confidence"`
	GeneratedAt         time.Time       `json:"generatedAt"`
	Experimental        bool            `json:"experimental"`
	Warning             string          `json:"warning"`
	Validation          json.RawMessage `json:"validation,omitempty"`
	Seasonality         json.RawMessage `json:"seasonality,omitempty"`
	Anomalies           json.RawMessage `json:"anomalies,omitempty"`
	Drift               json.RawMessage `json:"drift,omitempty"`
	DataQuality         json.RawMessage `json:"dataQuality,omitempty"`
	Retraining          json.RawMessage `json:"retraining,omitempty"`
	Calibration         *Calibration    `json:"calibration,omitempty"`
	ModelMetadata       *ModelMetadata  `json:"modelMetadata,omitempty"`
}

// ModelMetadata describes the versioned model artifact selected by the
// forecasting service.
type ModelMetadata struct {
	Name            string    `json:"name"`
	Version         string    `json:"version"`
	FeatureSchema   string    `json:"featureSchema"`
	TrainedAt       time.Time `json:"trainedAt"`
	TrainingSamples int       `json:"trainingSamples"`
	Persisted       bool      `json:"persisted"`
	Reused          bool      `json:"reused,omitempty"`
}

// Calibration is interval coverage measured on held-out data. Intervals are
// widened by Factor when the held-out coverage fell short of the nominal level.
type Calibration struct {
	Method             string   `json:"method"`
	NominalCoverage    float64  `json:"nominalCoverage"`
	HeldOutCoverage    *float64 `json:"heldOutCoverage"`
	CalibratedCoverage *float64 `json:"calibratedCoverage"`
	Factor             float64  `json:"factor"`
	EvaluationSamples  int      `json:"evaluationSamples"`
	Calibrated         bool     `json:"calibrated"`
}

// BacktestRequest replays a history from past origins.
type BacktestRequest struct {
	Samples         []float64 `json:"samples"`
	Horizon         int       `json:"horizon"`
	Origins         int       `json:"origins"`
	IntervalSeconds int       `json:"interval_seconds"`
	Resource        string    `json:"resource"`
	ConfidenceLevel float64   `json:"confidence_level"`
}

// Score summarizes forecast errors against what actually happened.
type Score struct {
	Samples           int      `json:"samples"`
	MAE               float64  `json:"mae"`
	RMSE              float64  `json:"rmse"`
	SMAPE             float64  `json:"smape"`
	MAPE              *float64 `json:"mape"`
	Coverage          float64  `json:"coverage"`
	MeanIntervalWidth float64  `json:"meanIntervalWidth"`
	BaselineMAE       float64  `json:"baselineMAE"`
	Skill             *float64 `json:"skill"`
}

// BacktestPoint is one forecast step and the observation it predicted.
type BacktestPoint struct {
	Index     int     `json:"index"`
	Actual    float64 `json:"actual"`
	Predicted float64 `json:"predicted"`
	Lower     float64 `json:"lower"`
	Upper     float64 `json:"upper"`
	Baseline  float64 `json:"baseline"`
}

// BacktestOrigin scores one forecast origin.
type BacktestOrigin struct {
	OriginIndex int    `json:"originIndex"`
	Model       string `json:"model"`
	Calibrated  bool   `json:"calibrated"`
	Score       Score  `json:"score"`
}

// BacktestResponse is the rolling-origin evaluation.
type BacktestResponse struct {
	Method          string           `json:"method"`
	Horizon         int              `json:"horizon"`
	NominalCoverage float64          `json:"nominalCoverage"`
	Origins         []BacktestOrigin `json:"origins"`
	Overall         Score            `json:"overall"`
	Points          []BacktestPoint  `json:"points"`
	GeneratedAt     time.Time        `json:"generatedAt"`
	Warning         string           `json:"warning"`
}

// Options configures the client.
type Options struct {
	BaseURL    string
	APIKey     string
	Timeout    time.Duration
	AllowHTTP  bool
	HTTPClient *http.Client
}

// Client is safe for concurrent use.
type Client struct {
	forecastURL string
	backtestURL string
	readyURL    string
	apiKey      string
	http        *http.Client
}

// NewClient validates the endpoint and refuses redirects.
func NewClient(options Options) (*Client, error) {
	base, err := url.Parse(options.BaseURL)
	if err != nil || !base.IsAbs() || base.Host == "" || base.User != nil || base.RawQuery != "" || base.Fragment != "" {
		return nil, errors.New("forecasting service URL must be an absolute URL without credentials, query, or fragment")
	}
	if base.Scheme != "https" && (base.Scheme != "http" || !options.AllowHTTP) {
		return nil, errors.New("forecasting service URL must use HTTPS unless HTTP is explicitly allowed")
	}
	timeout := options.Timeout
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	client := &http.Client{}
	if options.HTTPClient != nil {
		clone := *options.HTTPClient
		client = &clone
	}
	client.Timeout = timeout
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	root := strings.TrimRight(base.String(), "/")
	return &Client{forecastURL: root + "/api/v1/forecast", backtestURL: root + "/api/v1/backtest", readyURL: root + "/readyz", apiKey: options.APIKey, http: client}, nil
}

// Forecast submits a series and validates the shape of the response.
func (client *Client) Forecast(ctx context.Context, request Request) (Response, error) {
	payload, err := client.post(ctx, client.forecastURL, request)
	if err != nil {
		return Response{}, err
	}
	var decoded Response
	if err := json.Unmarshal(payload, &decoded); err != nil {
		return Response{}, fmt.Errorf("%w: malformed JSON", ErrProtocol)
	}
	if len(decoded.Predictions) != request.Horizon || len(decoded.ConfidenceIntervals) != request.Horizon {
		return Response{}, fmt.Errorf("%w: expected %d predictions and intervals", ErrProtocol, request.Horizon)
	}
	for index, prediction := range decoded.Predictions {
		interval := decoded.ConfidenceIntervals[index]
		if !finite(prediction) || !finite(interval.Lower) || !finite(interval.Upper) || interval.Lower > interval.Upper {
			return Response{}, fmt.Errorf("%w: prediction %d is not finite or its interval is inverted", ErrProtocol, index)
		}
	}
	return decoded, nil
}

// Backtest replays a history from past origins and validates the shape of
// the scores.
func (client *Client) Backtest(ctx context.Context, request BacktestRequest) (BacktestResponse, error) {
	payload, err := client.post(ctx, client.backtestURL, request)
	if err != nil {
		return BacktestResponse{}, err
	}
	var decoded BacktestResponse
	if err := json.Unmarshal(payload, &decoded); err != nil {
		return BacktestResponse{}, fmt.Errorf("%w: malformed JSON", ErrProtocol)
	}
	if len(decoded.Origins) != request.Origins || len(decoded.Points) != request.Horizon {
		return BacktestResponse{}, fmt.Errorf("%w: expected %d origins and %d points", ErrProtocol, request.Origins, request.Horizon)
	}
	for index, point := range decoded.Points {
		if !finite(point.Actual) || !finite(point.Predicted) || !finite(point.Lower) || !finite(point.Upper) || !finite(point.Baseline) || point.Lower > point.Upper {
			return BacktestResponse{}, fmt.Errorf("%w: backtest point %d is not finite or its interval is inverted", ErrProtocol, index)
		}
	}
	if !finite(decoded.Overall.MAE) || decoded.Overall.Coverage < 0 || decoded.Overall.Coverage > 1 {
		return BacktestResponse{}, fmt.Errorf("%w: backtest scores are out of range", ErrProtocol)
	}
	return decoded, nil
}

// post sends a JSON request and returns the bounded body of a successful
// response, mapping failures onto the client's error kinds.
func (client *Client) post(ctx context.Context, endpoint string, request any) ([]byte, error) {
	body, err := json.Marshal(request)
	if err != nil {
		return nil, fmt.Errorf("%w: could not encode request", ErrRejected)
	}
	httpRequest, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("%w: could not construct request", ErrUnavailable)
	}
	httpRequest.Header.Set("Content-Type", "application/json")
	httpRequest.Header.Set("Accept", "application/json")
	httpRequest.Header.Set("User-Agent", "KubeRight/forecast-client")
	if client.apiKey != "" {
		httpRequest.Header.Set("X-API-Key", client.apiKey)
	}
	response, err := client.http.Do(httpRequest)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrUnavailable, err)
	}
	defer response.Body.Close()
	payload, err := io.ReadAll(io.LimitReader(response.Body, maxResponseBytes+1))
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrUnavailable, err)
	}
	if len(payload) > maxResponseBytes {
		return nil, fmt.Errorf("%w: response exceeds %d bytes", ErrProtocol, maxResponseBytes)
	}
	switch {
	case response.StatusCode == http.StatusTooManyRequests:
		return nil, ErrBusy
	case response.StatusCode == http.StatusUnauthorized || response.StatusCode == http.StatusForbidden:
		// A credential mismatch is an operator configuration fault, not a
		// caller error, so it is reported as unavailability.
		return nil, fmt.Errorf("%w: authentication with the forecasting service failed", ErrUnavailable)
	case response.StatusCode >= 400 && response.StatusCode < 500:
		return nil, fmt.Errorf("%w: %s", ErrRejected, serviceDetail(payload))
	case response.StatusCode != http.StatusOK:
		return nil, fmt.Errorf("%w: HTTP status %d", ErrUnavailable, response.StatusCode)
	}
	return payload, nil
}

// Ready calls the service's readiness endpoint.
func (client *Client) Ready(ctx context.Context) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, client.readyURL, nil)
	if err != nil {
		return err
	}
	response, err := client.http.Do(request)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrUnavailable, err)
	}
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 64<<10))
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("%w: readiness returned HTTP %d", ErrUnavailable, response.StatusCode)
	}
	return nil
}

func finite(value float64) bool { return !math.IsNaN(value) && !math.IsInf(value, 0) }

// serviceDetail extracts a bounded, printable validation message from the
// service's error envelope.
func serviceDetail(payload []byte) string {
	var envelope struct {
		Detail any `json:"detail"`
		Error  any `json:"error"`
	}
	message := "the request was not accepted"
	if json.Unmarshal(payload, &envelope) == nil {
		for _, candidate := range []any{envelope.Detail, envelope.Error} {
			switch value := candidate.(type) {
			case string:
				message = value
			case map[string]any:
				if text, ok := value["message"].(string); ok {
					message = text
				}
				if details, ok := value["details"].([]any); ok && len(details) > 0 {
					if first, ok := details[0].(map[string]any); ok {
						if text, ok := first["message"].(string); ok {
							message += ": " + text
						}
					}
				}
			}
		}
	}
	message = strings.Map(func(character rune) rune {
		if character < 0x20 || character == 0x7f {
			return ' '
		}
		return character
	}, message)
	if len(message) > 300 {
		message = message[:300]
	}
	return message
}
