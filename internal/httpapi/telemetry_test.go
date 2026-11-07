package httpapi

import (
	"net/http"
	"net/url"
	"testing"
)

func TestTelemetryRouteBoundsCardinality(t *testing.T) {
	tests := []struct {
		method string
		path   string
		want   string
	}{
		{http.MethodGet, "/healthz", "GET /healthz"},
		{http.MethodGet, "/api/v1/recommendations/rec_12345", "GET /api/v1/recommendations/{id}"},
		{http.MethodGet, "/api/v1/recommendations/rec_12345/extra", "unmatched"},
		{http.MethodGet, "/api/v1/not-real", "unmatched"},
		{http.MethodGet, "/assets/app.js", "web"},
	}
	for _, test := range tests {
		request := &http.Request{Method: test.method, URL: &url.URL{Path: test.path}}
		if got := telemetryRoute(request); got != test.want {
			t.Errorf("telemetryRoute(%s %s) = %q, want %q", test.method, test.path, got, test.want)
		}
	}
}

func TestBoundedHTTPMethodRejectsUnboundedLabels(t *testing.T) {
	if got := boundedHTTPMethod(http.MethodGet); got != http.MethodGet {
		t.Fatalf("GET normalized to %q", got)
	}
	if got := boundedHTTPMethod("ATTACK-UNIQUE-123"); got != "OTHER" {
		t.Fatalf("custom method normalized to %q, want OTHER", got)
	}
}
