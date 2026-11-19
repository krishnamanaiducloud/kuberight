package metrics

import (
	"context"
	"encoding/pem"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// recordingPrometheus answers every range query with one empty matrix and
// records the headers and query parameters it was sent.
type recordingPrometheus struct {
	mu        sync.Mutex
	tokens    []string
	namespace []string
}

func (recorder *recordingPrometheus) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		recorder.mu.Lock()
		recorder.tokens = append(recorder.tokens, r.Header.Get("Authorization"))
		recorder.namespace = append(recorder.namespace, r.URL.Query().Get("namespace"))
		recorder.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"matrix","result":[]}}`))
	})
}

func writeFile(t *testing.T, dir, name, content string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// serverCA writes the TLS test server's certificate as a PEM bundle.
func serverCA(t *testing.T, server *httptest.Server, dir string) string {
	t.Helper()
	block := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw})
	return writeFile(t, dir, "ca.crt", string(block))
}

func emptyQuery() RangeQuery {
	end := time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)
	return RangeQuery{Expression: "vector(1)", Start: end.Add(-time.Hour), End: end, Step: time.Minute}
}

func TestPrometheusClientTrustsTheCABundleAndSendsTheBearerToken(t *testing.T) {
	recorder := &recordingPrometheus{}
	server := httptest.NewTLSServer(recorder.handler())
	defer server.Close()
	dir := t.TempDir()
	tokenPath := writeFile(t, dir, "token", "first-token\n")

	// Without the bundle the server's certificate is not trusted.
	untrusted, err := NewPrometheusClient(PrometheusOptions{Endpoint: server.URL, BearerTokenFile: tokenPath})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := untrusted.QueryRange(context.Background(), emptyQuery()); !errors.Is(err, ErrUpstream) {
		t.Fatalf("an untrusted certificate was accepted: %v", err)
	}

	client, err := NewPrometheusClient(PrometheusOptions{Endpoint: server.URL, BearerTokenFile: tokenPath, CAFile: serverCA(t, server, dir)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.QueryRange(context.Background(), emptyQuery()); err != nil {
		t.Fatal(err)
	}
	if got := recorder.tokens[len(recorder.tokens)-1]; got != "Bearer first-token" {
		t.Fatalf("authorization header = %q", got)
	}
	if got := recorder.namespace[len(recorder.namespace)-1]; got != "" {
		t.Fatalf("a namespace parameter was sent without a tenant: %q", got)
	}
}

func TestTokenFileFollowsRotation(t *testing.T) {
	dir := t.TempDir()
	path := writeFile(t, dir, "token", "first")
	now := time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)
	token, err := newTokenFile(path, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, dir, "token", "second")
	if value, _ := token.get(); value != "first" {
		t.Fatalf("the token was re-read before the refresh interval: %q", value)
	}
	now = now.Add(tokenRefresh)
	if value, _ := token.get(); value != "second" {
		t.Fatalf("a rotated token was not picked up: %q", value)
	}
	// A failed re-read keeps the last token instead of dropping auth.
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	now = now.Add(tokenRefresh)
	if value, err := token.get(); err != nil || value != "second" {
		t.Fatalf("value=%q err=%v", value, err)
	}
}

func TestPrometheusClientRejectsUnsafeAccessSettings(t *testing.T) {
	dir := t.TempDir()
	token := writeFile(t, dir, "token", "secret-token")
	for name, test := range map[string]struct {
		options PrometheusOptions
		want    string
	}{
		"token over HTTP":         {PrometheusOptions{Endpoint: "http://prometheus.monitoring.svc:9090", AllowHTTP: true, BearerTokenFile: token}, "cleartext HTTP"},
		"missing token file":      {PrometheusOptions{Endpoint: "https://prometheus.example", BearerTokenFile: filepath.Join(dir, "absent")}, "could not be read"},
		"empty token":             {PrometheusOptions{Endpoint: "https://prometheus.example", BearerTokenFile: writeFile(t, dir, "empty", "\n")}, "empty or malformed"},
		"token with inner spaces": {PrometheusOptions{Endpoint: "https://prometheus.example", BearerTokenFile: writeFile(t, dir, "spaced", "a b")}, "empty or malformed"},
		"missing CA file":         {PrometheusOptions{Endpoint: "https://prometheus.example", CAFile: filepath.Join(dir, "absent.crt")}, "CA bundle could not be read"},
		"CA file without PEM":     {PrometheusOptions{Endpoint: "https://prometheus.example", CAFile: writeFile(t, dir, "bad.crt", "not a certificate")}, "no PEM certificate"},
		"invalid tenant":          {PrometheusOptions{Endpoint: "https://prometheus.example", TenantNamespace: "Shop_Prod"}, "namespace name"},
	} {
		_, err := NewPrometheusClient(test.options)
		if !errors.Is(err, ErrInvalidOptions) || !strings.Contains(err.Error(), test.want) {
			t.Fatalf("%s: error = %v", name, err)
		}
		if err != nil && strings.Contains(err.Error(), "secret-token") {
			t.Fatalf("%s: the token leaked into an error", name)
		}
	}
}

func TestPrometheusClientSendsTheTenantNamespace(t *testing.T) {
	recorder := &recordingPrometheus{}
	server := httptest.NewServer(recorder.handler())
	defer server.Close()
	client, err := NewPrometheusClient(PrometheusOptions{Endpoint: server.URL, AllowHTTP: true, TenantNamespace: "shop"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.QueryRange(context.Background(), emptyQuery()); err != nil {
		t.Fatal(err)
	}
	if recorder.namespace[0] != "shop" || recorder.tokens[0] != "" {
		t.Fatalf("namespace=%q authorization=%q", recorder.namespace[0], recorder.tokens[0])
	}
}

func TestDiscoverPrometheusFindsOpenShiftMonitoringOnly(t *testing.T) {
	dir := t.TempDir()
	// A plain Kubernetes ServiceAccount mount has a token but no service CA.
	writeFile(t, dir, "token", "token")
	if found, ok := DiscoverPrometheus(dir, ""); ok {
		t.Fatalf("discovered on a cluster without OpenShift monitoring: %+v", found)
	}
	writeFile(t, dir, "service-ca.crt", "pem")
	found, ok := DiscoverPrometheus(dir, "")
	if !ok || found.Endpoint != openShiftThanosQuerier || found.TenantNamespace != "" ||
		found.BearerTokenFile != filepath.Join(dir, "token") || found.CAFile != filepath.Join(dir, "service-ca.crt") {
		t.Fatalf("cluster-wide discovery = %+v ok=%v", found, ok)
	}
	// A namespace-scoped inventory uses the tenancy port for that namespace.
	found, ok = DiscoverPrometheus(dir, "shop")
	if !ok || found.Endpoint != openShiftThanosTenancy || found.TenantNamespace != "shop" || !strings.Contains(found.Source, "namespace shop") {
		t.Fatalf("namespace-scoped discovery = %+v ok=%v", found, ok)
	}
	// A directory in place of the CA file is not mistaken for one.
	other := t.TempDir()
	writeFile(t, other, "token", "token")
	if err := os.Mkdir(filepath.Join(other, "service-ca.crt"), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, ok := DiscoverPrometheus(other, ""); ok {
		t.Fatal("a directory was taken for the service CA")
	}
}
