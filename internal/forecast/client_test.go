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

func TestBacktestSendsContractAndValidatesResponse(t *testing.T) {
	body := `{"method":"rolling-origin-no-future-observations","horizon":2,"nominalCoverage":0.9,"origins":[{"originIndex":96,"model":"m","calibrated":true,"score":{"samples":2,"mae":0.1,"rmse":0.1,"smape":0.05,"mape":null,"coverage":1,"meanIntervalWidth":0.2,"baselineMAE":0.2,"skill":0.5}}],"overall":{"samples":2,"mae":0.1,"rmse":0.1,"smape":0.05,"mape":0.04,"coverage":1,"meanIntervalWidth":0.2,"baselineMAE":0.2,"skill":0.5},"points":[{"index":96,"actual":1,"predicted":1.1,"lower":0.9,"upper":1.3,"baseline":1},{"index":97,"actual":2,"predicted":1.9,"lower":1.5,"upper":2.2,"baseline":1}],"generatedAt":"2026-10-01T00:00:00Z","warning":"w"}`
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		var request map[string]any
		if r.URL.Path != "/api/v1/backtest" || json.NewDecoder(r.Body).Decode(&request) != nil || request["origins"].(float64) != 1 {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		_, _ = w.Write([]byte(body))
	})
	response, err := client.Backtest(context.Background(), BacktestRequest{Samples: make([]float64, 98), Horizon: 2, Origins: 1, IntervalSeconds: 3600, Resource: "cpu", ConfidenceLevel: 0.9})
	if err != nil {
		t.Fatal(err)
	}
	if response.Overall.Skill == nil || *response.Overall.Skill != 0.5 || response.Origins[0].Score.MAPE != nil || len(response.Points) != 2 {
		t.Fatalf("response %#v", response)
	}

	for name, payload := range map[string]string{
		"inverted":     strings.Replace(body, `"lower":0.9,"upper":1.3`, `"lower":1.3,"upper":0.9`, 1),
		"out of range": strings.Replace(body, `"coverage":1,"meanIntervalWidth":0.2,"baselineMAE":0.2,"skill":0.5},"points"`, `"coverage":2,"meanIntervalWidth":0.2,"baselineMAE":0.2,"skill":0.5},"points"`, 1),
		"malformed":    "{",
	} {
		bad := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(payload)) })
		if _, err := bad.Backtest(context.Background(), BacktestRequest{Horizon: 2, Origins: 1}); !errors.Is(err, ErrProtocol) {
			t.Fatalf("%s: err=%v", name, err)
		}
	}
	shape := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(body)) })
	if _, err := shape.Backtest(context.Background(), BacktestRequest{Horizon: 3, Origins: 1}); !errors.Is(err, ErrProtocol) {
		t.Fatalf("wrong shape err=%v", err)
	}
	busy := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusTooManyRequests) })
	if _, err := busy.Backtest(context.Background(), BacktestRequest{Horizon: 2, Origins: 1}); !errors.Is(err, ErrBusy) {
		t.Fatalf("busy err=%v", err)
	}
}
