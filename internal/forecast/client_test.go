package forecast

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Construct test credentials at runtime so secret scanners do not mistake a
// committed fixture for a real API key.
var testAPIKey = strings.Repeat("x", 16)

func newTestClient(t *testing.T, handler http.HandlerFunc) *Client {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	client, err := NewClient(Options{BaseURL: server.URL + "/", APIKey: testAPIKey, AllowHTTP: true})
	if err != nil {
		t.Fatal(err)
	}
	return client
}

func TestForecastSendsContractAndValidatesResponse(t *testing.T) {
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/forecast" || r.Header.Get("X-API-Key") != testAPIKey {
			http.Error(w, "bad request target", http.StatusBadRequest)
			return
		}
		var request map[string]any
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil || request["interval_seconds"].(float64) != 3600 || request["resource"] != "cpu" {
			http.Error(w, "bad body", http.StatusBadRequest)
			return
		}
		_, _ = w.Write([]byte(`{"predictions":[1,2],"confidenceIntervals":[{"lower":0.5,"upper":1.5},{"lower":1,"upper":3}],"model":"m","modelVersion":"v","fallbackUsed":false,"fallbackReasons":[],"confidence":0.8,"generatedAt":"2026-10-01T00:00:00Z","experimental":true,"warning":"w","drift":{"detected":false}}`))
	})
	response, err := client.Forecast(context.Background(), Request{Samples: []float64{1, 2, 3}, Horizon: 2, IntervalSeconds: 3600, Resource: "cpu", ConfidenceLevel: 0.9})
	if err != nil {
		t.Fatal(err)
	}
	if len(response.Predictions) != 2 || response.Model != "m" || string(response.Drift) != `{"detected":false}` {
		t.Fatalf("unexpected response %#v", response)
	}
}

func TestForecastClassifiesFailures(t *testing.T) {
	cases := map[string]struct {
		status int
		body   string
		want   error
		detail string
	}{
		"busy":         {http.StatusTooManyRequests, `{}`, ErrBusy, ""},
		"validation":   {http.StatusUnprocessableEntity, `{"error":{"message":"request validation failed","details":[{"message":"too few samples"}]}}`, ErrRejected, "too few samples"},
		"unauthorized": {http.StatusUnauthorized, `{}`, ErrUnavailable, "authentication"},
		"server":       {http.StatusInternalServerError, `{}`, ErrUnavailable, "500"},
		"wrong length": {http.StatusOK, `{"predictions":[1],"confidenceIntervals":[{"lower":0,"upper":1}]}`, ErrProtocol, ""},
		"inverted":     {http.StatusOK, `{"predictions":[1,2],"confidenceIntervals":[{"lower":2,"upper":1},{"lower":0,"upper":3}]}`, ErrProtocol, ""},
		"malformed":    {http.StatusOK, `{`, ErrProtocol, ""},
	}
	for name, test := range cases {
		t.Run(name, func(t *testing.T) {
			client := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(test.status)
				_, _ = w.Write([]byte(test.body))
			})
			_, err := client.Forecast(context.Background(), Request{Samples: []float64{1}, Horizon: 2, IntervalSeconds: 60, Resource: "cpu", ConfidenceLevel: 0.9})
			if !errors.Is(err, test.want) || !strings.Contains(err.Error(), test.detail) {
				t.Fatalf("got %v, want %v containing %q", err, test.want, test.detail)
			}
		})
	}
}

func TestForecastRefusesRedirects(t *testing.T) {
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "https://example.invalid/", http.StatusFound)
	})
	if _, err := client.Forecast(context.Background(), Request{Horizon: 1}); !errors.Is(err, ErrRejected) && !errors.Is(err, ErrUnavailable) {
		t.Fatalf("redirect was followed or misclassified: %v", err)
	}
}

func TestReady(t *testing.T) {
	healthy := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/readyz" {
			w.WriteHeader(http.StatusNotFound)
		}
	})
	if err := healthy.Ready(context.Background()); err != nil {
		t.Fatal(err)
	}
	unhealthy := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusServiceUnavailable) })
	if err := unhealthy.Ready(context.Background()); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("got %v, want ErrUnavailable", err)
	}
}

func TestNewClientRejectsUnsafeEndpoints(t *testing.T) {
	for _, endpoint := range []string{"", "ml:8081", "http://ml:8081", "https://user:pass@ml", "https://ml/?x=1", "ftp://ml"} {
		if _, err := NewClient(Options{BaseURL: endpoint}); err == nil {
			t.Fatalf("accepted %q", endpoint)
		}
	}
}
