package httpapi

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/kuberight/kuberight/internal/config"
	"github.com/kuberight/kuberight/internal/usage"
)

func rangeRequest(query string) *http.Request {
	return httptest.NewRequest(http.MethodGet, "/api/v1/metrics/signals?"+query, nil)
}

func TestChartRangeBoundsAndSteps(t *testing.T) {
	step := config.DefaultRecommendationStep
	for _, test := range []struct {
		query           string
		duration        time.Duration
		minStep, maxStp time.Duration
	}{
		{"window=1m", time.Minute, 15 * time.Second, 15 * time.Second},
		{"window=1h", time.Hour, 15 * time.Second, 15 * time.Second},
		{"window=6h", 6 * time.Hour, 36 * time.Second, 36 * time.Second},
		{"window=24h", 24 * time.Hour, 5 * time.Minute, 5 * time.Minute},
		{"window=8760h", 365 * 24 * time.Hour, 14 * time.Hour, 15 * time.Hour},
		{"", 24 * time.Hour, 5 * time.Minute, 5 * time.Minute},
	} {
		span, err := parseChartRange(rangeRequest(test.query), 24*time.Hour, step, fixedNow)
		if err != nil {
			t.Fatalf("%q: %v", test.query, err)
		}
		if span.duration != test.duration || span.step < test.minStep || span.step > test.maxStp || !span.end.Equal(fixedNow) || span.custom {
			t.Fatalf("%q: duration=%s step=%s end=%s", test.query, span.duration, span.step, span.end)
		}
		if points := int(span.duration/span.step) + 1; points > historyChartPoints+1 || points > config.MaximumQueryPoints {
			t.Fatalf("%q: %d points exceed the chart bound", test.query, points)
		}
	}
	for _, query := range []string{"window=59s", "window=8761h", "window=soon"} {
		if _, err := parseChartRange(rangeRequest(query), 24*time.Hour, step, fixedNow); err == nil {
			t.Fatalf("%q was accepted", query)
		}
	}
}

func TestChartRangeCustomStartAndEndAreUTC(t *testing.T) {
	step := config.DefaultRecommendationStep
	// Browser-local offsets are converted to UTC.
	span, err := parseChartRange(rangeRequest("start=2026-10-14T06:00:00-06:00&end=2026-10-14T18:30:00%2B05:30"), 24*time.Hour, step, fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	if !span.custom || !span.start.Equal(time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)) || !span.end.Equal(time.Date(2026, 10, 14, 13, 0, 0, 0, time.UTC)) || span.duration != time.Hour || span.end.Location() != time.UTC {
		t.Fatalf("span=%+v", span)
	}
	if window := span.window(); !window.End.Equal(span.end) || window.Duration != time.Hour {
		t.Fatalf("window=%+v", window)
	}
	// A clock a few seconds ahead of the server is clamped, not rejected.
	if span, err := parseChartRange(rangeRequest("start=2026-10-15T11:00:00Z&end=2026-10-15T12:00:20Z"), 24*time.Hour, step, fixedNow); err != nil || !span.end.Equal(fixedNow) {
		t.Fatalf("clamp: span=%+v err=%v", span, err)
	}
	for query, message := range map[string]string{
		"start=2026-10-14T00:00:00Z":                                    "together",
		"start=2026-10-14T00:00:00Z&end=2026-10-14T00:00:30Z":           "between",
		"start=2026-10-14T01:00:00Z&end=2026-10-14T00:00:00Z":           "after start",
		"start=2026-10-15T11:00:00Z&end=2026-10-15T14:00:00Z":           "future",
		"start=2024-10-14T00:00:00Z&end=2026-10-14T00:00:00Z":           "365 days",
		"start=yesterday&end=2026-10-14T00:00:00Z":                      "RFC 3339",
		"start=2026-10-14T00:00:00Z&end=2026-10-15T00:00:00Z&window=1h": "either",
	} {
		if _, err := parseChartRange(rangeRequest(query), 24*time.Hour, step, fixedNow); err == nil || !strings.Contains(err.Error(), message) {
			t.Fatalf("%q: err=%v, want %q", query, err, message)
		}
	}
}

func TestDataAvailabilityReportsRetentionInsteadOfClaimingHistory(t *testing.T) {
	span, err := parseChartRange(rangeRequest("window=8760h"), 24*time.Hour, config.DefaultRecommendationStep, fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	firstSample := fixedNow.Add(-15 * 24 * time.Hour)
	series := usage.Series{Points: []usage.Point{{Timestamp: firstSample, Value: 1}, {Timestamp: fixedNow, Value: 1}}}
	availability := dataAvailability(span, series)
	if availability["dataStart"] != firstSample || !strings.Contains(availability["note"].(string), "retention") {
		t.Fatalf("availability=%v", availability)
	}
	full, _ := parseChartRange(rangeRequest("window=24h"), 24*time.Hour, config.DefaultRecommendationStep, fixedNow)
	complete := usage.Series{Points: []usage.Point{{Timestamp: full.start, Value: 1}}}
	if _, noted := dataAvailability(full, complete)["note"]; noted {
		t.Fatal("a range fully covered by samples must carry no note")
	}
	if dataAvailability(full)["note"] == nil {
		t.Fatal("an empty range must say no samples were returned")
	}
}

func TestSignalsAcceptCustomRange(t *testing.T) {
	spec := checkoutSpec()
	source := healthyUsage(60)
	source.signals = &usage.Signals{CPU: source.history.CPU, Memory: source.history.Memory}
	api := newTestAPI(t, nil, Dependencies{Provider: fakeInventory{spec: spec}, Usage: source})
	response, body := api.do(t, http.MethodGet, "/api/v1/metrics/signals?namespace=shop&kind=Deployment&workload=checkout&container=api&start=2026-10-15T07:00:00Z&end=2026-10-15T11:00:00Z", "alice", "")
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	if body["range"].(map[string]any)["requestedStart"] != "2026-10-15T07:00:00Z" || body["step"] != "24s" {
		t.Fatalf("range=%v step=%v", body["range"], body["step"])
	}
	response, _ = api.do(t, http.MethodGet, "/api/v1/metrics/history?namespace=shop&kind=Deployment&workload=checkout&container=api&window=8760h", "alice", "")
	if response.Code != http.StatusOK {
		t.Fatalf("365-day history: status=%d body=%s", response.Code, response.Body.String())
	}
}
