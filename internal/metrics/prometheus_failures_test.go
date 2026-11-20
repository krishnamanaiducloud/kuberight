package metrics

import (
	"bytes"
	"context"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// Realistic Prometheus and Thanos API bodies.
const (
	fixtureCPU = `{"status":"success","data":{"resultType":"matrix","result":[` +
		`{"metric":{"namespace":"shop","pod":"api-7d9f","container":"api"},"values":[[1790000000,"0.25"]]}]}}`
	fixtureEmpty    = `{"status":"success","data":{"resultType":"matrix","result":[]}}`
	fixtureBadQuery = `{"status":"error","errorType":"bad_data","error":"invalid parameter \"query\": 1:5: parse error: unexpected <EOF> in label matching, expected string"}`
	fixtureTimeout  = `{"status":"error","errorType":"timeout","error":"query timed out in expression evaluation"}`
	secretToken     = "sa-token-do-not-log"
)

func instantQuery(name string) RangeQuery {
	at := time.Unix(1790000000, 0).UTC()
	return RangeQuery{Name: name, Expression: `sum(rate(container_cpu_usage_seconds_total{namespace="shop"}[5m]))`, Start: at, End: at, Step: time.Minute}
}

func writeTestFile(t *testing.T, dir, name, content string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// caFileFor writes the test server's certificate as a PEM bundle.
func caFileFor(t *testing.T, server *httptest.Server) string {
	t.Helper()
	certificate := server.Certificate()
	return writeTestFile(t, t.TempDir(), "ca.crt", string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certificate.Raw})))
}

// thanos answers like OpenShift's Thanos Querier behind kube-rbac-proxy:
// only the expected bearer token gets data.
func thanos(t *testing.T, token string, seen *[]string) *httptest.Server {
	t.Helper()
	var mu sync.Mutex
	server := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		mu.Lock()
		*seen = append(*seen, request.Header.Get("Authorization"))
		mu.Unlock()
		if request.Header.Get("Authorization") != "Bearer "+token {
			http.Error(writer, "Unauthorized", http.StatusUnauthorized)
			return
		}
		fmt.Fprint(writer, fixtureCPU)
	}))
	t.Cleanup(server.Close)
	return server
}

func TestServiceAccountBearerTokenOverVerifiedTLS(t *testing.T) {
	var seen []string
	server := thanos(t, secretToken, &seen)
	tokenPath := writeTestFile(t, t.TempDir(), "token", secretToken+"\n")
	client := mustPrometheusClient(t, PrometheusOptions{Endpoint: server.URL, BearerTokenFile: tokenPath, CAFile: caFileFor(t, server)})
	result, err := client.QueryRange(context.Background(), instantQuery("usage.cpu"))
	if err != nil || len(result.Series) != 1 || result.Series[0].Samples[0].Value != 0.25 {
		t.Fatalf("result = %+v, err = %v", result, err)
	}
	if len(seen) != 1 || seen[0] != "Bearer "+secretToken {
		t.Fatalf("Authorization headers = %q", seen)
	}
}

func TestRotatedServiceAccountTokenIsUsedWithoutRestart(t *testing.T) {
	var seen []string
	server := thanos(t, "second-token", &seen)
	dir := t.TempDir()
	tokenPath := writeTestFile(t, dir, "token", "first-token")
	client := mustPrometheusClient(t, PrometheusOptions{Endpoint: server.URL, BearerTokenFile: tokenPath, CAFile: caFileFor(t, server)})
	token := client.httpClient.Transport.(bearerTransport).token
	clock := time.Unix(1790000000, 0)
	token.now = func() time.Time { return clock }
	token.readAt = clock

	if _, err := client.QueryRange(context.Background(), instantQuery("readiness")); FailureOf(err) != FailureAuthentication {
		t.Fatalf("old token: err = %v", err)
	}
	writeTestFile(t, dir, "token", "second-token") // kubelet rotates the projected token in place
	clock = clock.Add(tokenRefresh)
	if _, err := client.QueryRange(context.Background(), instantQuery("readiness")); err != nil {
		t.Fatalf("rotated token was not picked up: %v", err)
	}
	if seen[len(seen)-1] != "Bearer second-token" {
		t.Fatalf("Authorization headers = %q", seen)
	}
}

func TestTLSVerificationStaysOn(t *testing.T) {
	var seen []string
	server := thanos(t, secretToken, &seen)
	tokenPath := writeTestFile(t, t.TempDir(), "token", secretToken)
	// No CA bundle: the self-signed certificate must be rejected, and the
	// token must not be sent over the unverified connection.
	client := mustPrometheusClient(t, PrometheusOptions{Endpoint: server.URL, BearerTokenFile: tokenPath})
	_, err := client.QueryRange(context.Background(), instantQuery("readiness"))
	failure, ok := AsQueryError(err)
	if !ok || failure.Kind != FailureTLS || !strings.Contains(failure.Message, "unknown authority") {
		t.Fatalf("err = %v", err)
	}
	if len(seen) != 0 {
		t.Fatalf("the server received a request over an unverified connection: %q", seen)
	}
	if !errors.Is(err, ErrUpstream) {
		t.Fatalf("errors.Is(ErrUpstream) = false for %v", err)
	}
}

func TestEmptySuccessfulResultIsNotAFailure(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) { fmt.Fprint(writer, fixtureEmpty) }))
	defer server.Close()
	client := mustPrometheusClient(t, PrometheusOptions{Endpoint: server.URL, AllowHTTP: true})
	result, err := client.QueryRange(context.Background(), instantQuery("coverage.container_cpu_cfs_throttled_periods_total"))
	if err != nil || !result.NoData || len(result.Series) != 0 {
		t.Fatalf("result = %+v, err = %v", result, err)
	}
}

func TestHTTPStatusesAreClassified(t *testing.T) {
	cases := []struct {
		status    int
		body      string
		kind      FailureKind
		errorType string
		auth      bool
	}{
		{http.StatusBadRequest, fixtureBadQuery, FailureInvalidQuery, "bad_data", false},
		{http.StatusUnprocessableEntity, fixtureTimeout, FailureInvalidQuery, "timeout", false},
		{http.StatusUnauthorized, "Unauthorized\n", FailureAuthentication, "", true},
		{http.StatusForbidden, "Forbidden (user=system:serviceaccount:kuberight:kuberight, verb=get, resource=namespaces)\n", FailureAuthorization, "", true},
		{http.StatusNotFound, "404 page not found\n", FailureEndpoint, "", false},
		{http.StatusTooManyRequests, "", FailureRateLimited, "", false},
		{http.StatusInternalServerError, `{"status":"error","errorType":"internal","error":"store unavailable"}`, FailureUpstream, "internal", false},
		{http.StatusServiceUnavailable, fixtureTimeout, FailureTimeout, "timeout", false},
		{http.StatusBadGateway, "<html>bad gateway</html>", FailureUpstream, "", false},
	}
	for _, testCase := range cases {
		t.Run(fmt.Sprint(testCase.status), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
				writer.WriteHeader(testCase.status)
				fmt.Fprint(writer, testCase.body)
			}))
			defer server.Close()
			client := mustPrometheusClient(t, PrometheusOptions{Endpoint: server.URL, AllowHTTP: true})
			_, err := client.QueryRange(context.Background(), instantQuery("hotspot.1"))
			failure, ok := AsQueryError(err)
			if !ok || failure.Kind != testCase.kind || failure.StatusCode != testCase.status || failure.ErrorType != testCase.errorType || failure.AuthFailure() != testCase.auth {
				t.Fatalf("failure = %+v", failure)
			}
			if failure.Query != "hotspot.1" || failure.Method != http.MethodGet || failure.Host == "" {
				t.Fatalf("request facts missing: %+v", failure)
			}
			// Upstream text is never echoed: it can contain the query or identities.
			for _, fragment := range []string{"parse error", "system:serviceaccount", "store unavailable", "<html>"} {
				if strings.Contains(err.Error(), fragment) {
					t.Fatalf("error echoed the upstream body: %q", err)
				}
			}
		})
	}
}

func TestErrorEnvelopeInSuccessfulResponseIsClassified(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) { fmt.Fprint(writer, fixtureBadQuery) }))
	defer server.Close()
	client := mustPrometheusClient(t, PrometheusOptions{Endpoint: server.URL, AllowHTTP: true})
	if _, err := client.QueryRange(context.Background(), instantQuery("extra.cpuTotal")); FailureOf(err) != FailureInvalidQuery {
		t.Fatalf("err = %v", err)
	}
}

func TestTimeoutIsClassified(t *testing.T) {
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		select {
		case <-release:
		case <-request.Context().Done():
		}
	}))
	defer server.Close()
	defer close(release)
	client := mustPrometheusClient(t, PrometheusOptions{Endpoint: server.URL, AllowHTTP: true, Timeout: 50 * time.Millisecond})
	_, err := client.QueryRange(context.Background(), instantQuery("hotspot.3"))
	failure, ok := AsQueryError(err)
	if !ok || failure.Kind != FailureTimeout || failure.Duration < 50*time.Millisecond {
		t.Fatalf("err = %v (%+v)", err, failure)
	}
}

func TestCallerCancellationIsNotAnUpstreamFailure(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) { <-request.Context().Done() }))
	defer server.Close()
	var logs bytes.Buffer
	client := mustPrometheusClient(t, PrometheusOptions{Endpoint: server.URL, AllowHTTP: true, Logger: slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelInfo}))})
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(20*time.Millisecond, cancel)
	_, err := client.QueryRange(ctx, instantQuery("hotspot.2"))
	if FailureOf(err) != FailureCanceled || !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
	if logs.Len() != 0 {
		t.Fatalf("a browser navigating away was logged as a Prometheus failure: %s", logs.String())
	}
}

func TestConnectionRefusedIsClassified(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	listener.Close()
	client := mustPrometheusClient(t, PrometheusOptions{Endpoint: "http://" + address, AllowHTTP: true})
	if _, err := client.QueryRange(context.Background(), instantQuery("readiness")); FailureOf(err) != FailureConnection {
		t.Fatalf("err = %v", err)
	}
}

func TestUnreadableTokenIsAClearFailure(t *testing.T) {
	var seen []string
	server := thanos(t, secretToken, &seen)
	dir := t.TempDir()
	tokenPath := writeTestFile(t, dir, "token", secretToken)
	client := mustPrometheusClient(t, PrometheusOptions{Endpoint: server.URL, BearerTokenFile: tokenPath, CAFile: caFileFor(t, server)})
	token := client.httpClient.Transport.(bearerTransport).token
	clock := time.Unix(1790000000, 0)
	token.now, token.readAt = func() time.Time { return clock }, clock
	writeTestFile(t, dir, "token", "")
	clock = clock.Add(tokenRefresh)
	_, err := client.QueryRange(context.Background(), instantQuery("readiness"))
	if failure, ok := AsQueryError(err); !ok || failure.Kind != FailureCredentials || !failure.AuthFailure() {
		t.Fatalf("err = %v", err)
	}
	if len(seen) != 0 {
		t.Fatalf("a request was sent without a usable token: %q", seen)
	}
}

func TestTransportFailureClassification(t *testing.T) {
	wrap := func(inner error) error {
		return &url.Error{Op: "Get", URL: "https://thanos/api/v1/query_range?query=secret", Err: inner}
	}
	cases := map[string]struct {
		err  error
		kind FailureKind
	}{
		"dns":             {wrap(&net.OpError{Op: "dial", Err: &net.DNSError{Err: "no such host", Name: "thanos-querier.openshift-monitoring.svc", IsNotFound: true}}), FailureDNS},
		"proxy":           {wrap(&net.OpError{Op: "proxyconnect", Net: "tcp", Err: syscall.ECONNREFUSED}), FailureProxy},
		"refused":         {wrap(&net.OpError{Op: "dial", Net: "tcp", Err: os.NewSyscallError("connect", syscall.ECONNREFUSED)}), FailureConnection},
		"reset":           {wrap(&net.OpError{Op: "read", Net: "tcp", Err: os.NewSyscallError("read", syscall.ECONNRESET)}), FailureConnection},
		"unknown CA":      {wrap(x509.UnknownAuthorityError{}), FailureTLS},
		"wrong host name": {wrap(x509.HostnameError{Certificate: &x509.Certificate{}, Host: "thanos"}), FailureTLS},
		"expired":         {wrap(x509.CertificateInvalidError{Reason: x509.Expired}), FailureTLS},
		"deadline":        {wrap(context.DeadlineExceeded), FailureTimeout},
		"token":           {wrap(fmt.Errorf("%w: %w", ErrInvalidOptions, errCredentials)), FailureCredentials},
	}
	for name, testCase := range cases {
		kind, message := transportFailure(context.Background(), testCase.err)
		if kind != testCase.kind || message == "" || strings.Contains(message, "secret") {
			t.Errorf("%s: kind %s message %q", name, kind, message)
		}
	}
}

func TestClusterTrafficBypassesTheHTTPProxy(t *testing.T) {
	proxy, _ := url.Parse("http://proxy.corp.example:3128")
	everything := func(*url.URL) (*url.URL, error) { return proxy, nil }
	choose := clusterAwareProxy(everything)
	for host, proxied := range map[string]bool{
		"thanos-querier.openshift-monitoring.svc":               false,
		"thanos-querier.openshift-monitoring.svc.cluster.local": false,
		"prometheus.monitoring.svc.cluster.local.":              false,
		"localhost":              false,
		"127.0.0.1":              false,
		"prometheus.example.com": true,
		"10.0.0.12":              true,
	} {
		got, err := choose(&http.Request{URL: &url.URL{Scheme: "https", Host: host + ":9091"}})
		if err != nil || (got != nil) != proxied {
			t.Errorf("%s: proxy = %v, want proxied=%v", host, got, proxied)
		}
	}
}

func TestExternalPrometheusUsesTheConfiguredProxy(t *testing.T) {
	var proxied []string
	forwardProxy := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		proxied = append(proxied, request.URL.Host) // a forward proxy sees the absolute URL
		fmt.Fprint(writer, fixtureCPU)
	}))
	defer forwardProxy.Close()
	t.Setenv("HTTP_PROXY", forwardProxy.URL)
	t.Setenv("NO_PROXY", "")
	client := mustPrometheusClient(t, PrometheusOptions{Endpoint: "http://prometheus.example.com:9090", AllowHTTP: true})
	if _, viaProxy := client.Endpoint(); !viaProxy {
		t.Fatal("Endpoint() does not report the proxy")
	}
	if _, err := client.QueryRange(context.Background(), instantQuery("usage.cpu")); err != nil {
		t.Fatalf("err = %v", err)
	}
	if len(proxied) != 1 || proxied[0] != "prometheus.example.com:9090" {
		t.Fatalf("proxied = %q", proxied)
	}

	// A proxy that cannot be reached is named as such.
	listener, _ := net.Listen("tcp", "127.0.0.1:0")
	closed := listener.Addr().String()
	listener.Close()
	t.Setenv("HTTP_PROXY", "http://"+closed)
	broken := mustPrometheusClient(t, PrometheusOptions{Endpoint: "http://prometheus.example.com:9090", AllowHTTP: true})
	_, err := broken.QueryRange(context.Background(), instantQuery("usage.cpu"))
	if failure, ok := AsQueryError(err); !ok || failure.Kind != FailureProxy || !failure.ViaProxy {
		t.Fatalf("err = %v", err)
	}
}

func TestFailuresAreLoggedWithoutSecrets(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.WriteHeader(http.StatusForbidden)
		fmt.Fprint(writer, "Forbidden "+secretToken)
	}))
	defer server.Close()
	var logs bytes.Buffer
	type requestKey struct{}
	client := mustPrometheusClient(t, PrometheusOptions{
		Endpoint: server.URL, AllowHTTP: true,
		Logger:    slog.New(slog.NewJSONHandler(&logs, nil)),
		RequestID: func(ctx context.Context) string { value, _ := ctx.Value(requestKey{}).(string); return value },
	})
	ctx := context.WithValue(context.Background(), requestKey{}, "req-123")
	query := instantQuery("hotspot.1")
	_, _ = client.QueryRange(ctx, query)
	line := logs.String()
	for _, field := range []string{`"msg":"Prometheus query failed"`, `"request_id":"req-123"`, `"query":"hotspot.1"`, `"endpoint_host":"127.0.0.1:`,
		`"method":"GET"`, `"status":403`, `"failure":"authorization_denied"`, `"duration_ms":`, `"timeout":false`, `"auth_failure":true`} {
		if !strings.Contains(line, field) {
			t.Errorf("log lacks %s: %s", field, line)
		}
	}
	if strings.Contains(line, secretToken) || strings.Contains(line, "container_cpu_usage_seconds_total") || strings.Contains(line, "Bearer") {
		t.Fatalf("log leaked a secret or the query: %s", line)
	}
}

func TestOpenShiftCredentialsAreOnlyForOpenShiftMonitoring(t *testing.T) {
	dir := t.TempDir()
	writeTestFile(t, dir, "token", "token")
	writeTestFile(t, dir, "service-ca.crt", "ca")
	for endpoint, want := range map[string]bool{
		"https://thanos-querier.openshift-monitoring.svc:9091":               true,
		"https://thanos-querier.openshift-monitoring.svc.cluster.local:9091": true,
		"https://prometheus-k8s.openshift-monitoring.svc:9091":               true,
		"http://thanos-querier.openshift-monitoring.svc:9091":                false, // never over cleartext
		"https://prometheus.example.com":                                     false,
		"https://thanos-querier.openshift-monitoring.svc.evil.example":       false,
	} {
		tokenFile, caFile, ok := OpenShiftMonitoringCredentials(endpoint, dir)
		if ok != want || (ok && (tokenFile != dir+"/token" || caFile != dir+"/service-ca.crt")) {
			t.Errorf("%s: ok=%v token=%q ca=%q", endpoint, ok, tokenFile, caFile)
		}
	}
	if _, _, ok := OpenShiftMonitoringCredentials("https://thanos-querier.openshift-monitoring.svc:9091", t.TempDir()); ok {
		t.Error("credentials were returned without a mounted token and service CA")
	}
}
