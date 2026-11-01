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
	return &Client{forecastURL: root + "/api/v1/forecast", readyURL: root + "/readyz", apiKey: options.APIKey, http: client}, nil
}

// Forecast submits a series and validates the shape of the response.
func (client *Client) Forecast(ctx context.Context, request Request) (Response, error) {
	body, err := json.Marshal(request)
	if err != nil {
		return Response{}, fmt.Errorf("%w: could not encode request", ErrRejected)
	}
	httpRequest, err := http.NewRequestWithContext(ctx, http.MethodPost, client.forecastURL, bytes.NewReader(body))
	if err != nil {
		return Response{}, fmt.Errorf("%w: could not construct request", ErrUnavailable)
	}
	httpRequest.Header.Set("Content-Type", "application/json")
	httpRequest.Header.Set("Accept", "application/json")
	httpRequest.Header.Set("User-Agent", "KubeRight/forecast-client")
	if client.apiKey != "" {
		httpRequest.Header.Set("X-API-Key", client.apiKey)
	}
	response, err := client.http.Do(httpRequest)
	if err != nil {
		return Response{}, fmt.Errorf("%w: %w", ErrUnavailable, err)
	}
	defer response.Body.Close()
	payload, err := io.ReadAll(io.LimitReader(response.Body, maxResponseBytes+1))
	if err != nil {
		return Response{}, fmt.Errorf("%w: %w", ErrUnavailable, err)
	}
	if len(payload) > maxResponseBytes {
		return Response{}, fmt.Errorf("%w: response exceeds %d bytes", ErrProtocol, maxResponseBytes)
	}
	switch {
	case response.StatusCode == http.StatusTooManyRequests:
		return Response{}, ErrBusy
	case response.StatusCode == http.StatusUnauthorized || response.StatusCode == http.StatusForbidden:
		// A credential mismatch is an operator configuration fault, not a
		// caller error, so it is reported as unavailability.
		return Response{}, fmt.Errorf("%w: authentication with the forecasting service failed", ErrUnavailable)
	case response.StatusCode >= 400 && response.StatusCode < 500:
		return Response{}, fmt.Errorf("%w: %s", ErrRejected, serviceDetail(payload))
	case response.StatusCode != http.StatusOK:
		return Response{}, fmt.Errorf("%w: HTTP status %d", ErrUnavailable, response.StatusCode)
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
