package metrics

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/http/httpproxy"
)

// ServiceAccountDir is where Kubernetes mounts a pod's ServiceAccount
// credentials. On OpenShift the same projected volume also carries the
// service CA bundle.
const ServiceAccountDir = "/var/run/secrets/kubernetes.io/serviceaccount"

const (
	// OpenShift's monitoring stack serves PromQL through Thanos Querier. Port
	// 9091 answers for every namespace and requires the cluster-monitoring-view
	// role; port 9092 answers for one namespace at a time, named by the
	// namespace query parameter, and requires access to that namespace.
	openShiftThanosQuerier = "https://thanos-querier.openshift-monitoring.svc:9091"
	openShiftThanosTenancy = "https://thanos-querier.openshift-monitoring.svc:9092"
	// tokenRefresh bounds how long a rotated ServiceAccount token can go
	// unnoticed. Kubelet refreshes projected tokens well before they expire.
	tokenRefresh = time.Minute
)

var namespaceName = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)

// Discovered is a Prometheus endpoint KubeRight found on its own, with the
// credentials it needs.
type Discovered struct {
	Endpoint        string
	BearerTokenFile string
	CAFile          string
	TenantNamespace string
	// Source says what was found, for logs and the integration status.
	Source string
}

// DiscoverPrometheus finds the monitoring stack of the cluster KubeRight
// runs in. It recognises OpenShift by the service CA bundle that OpenShift
// mounts next to every ServiceAccount token, and then uses Thanos Querier
// with that token and CA. With a namespace-scoped inventory it uses the
// tenancy port for that namespace, which needs no cluster-wide role.
// Elsewhere there is no endpoint every installation shares, so it reports
// nothing and the operator sets the URL.
func DiscoverPrometheus(serviceAccountDir, inventoryNamespace string) (Discovered, bool) {
	caFile := serviceAccountDir + "/service-ca.crt"
	tokenFile := serviceAccountDir + "/token"
	if !regularFile(caFile) || !regularFile(tokenFile) {
		return Discovered{}, false
	}
	if inventoryNamespace != "" {
		return Discovered{
			Endpoint: openShiftThanosTenancy, BearerTokenFile: tokenFile, CAFile: caFile, TenantNamespace: inventoryNamespace,
			Source: "OpenShift monitoring (Thanos Querier, namespace " + inventoryNamespace + ")",
		}, true
	}
	return Discovered{
		Endpoint: openShiftThanosQuerier, BearerTokenFile: tokenFile, CAFile: caFile,
		Source: "OpenShift monitoring (Thanos Querier)",
	}, true
}

func regularFile(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.Mode().IsRegular()
}

// tlsConfig trusts the system roots plus the PEM bundle in caFile.
func tlsConfig(caFile string) (*tls.Config, error) {
	bundle, err := os.ReadFile(caFile)
	if err != nil {
		return nil, fmt.Errorf("%w: the CA bundle could not be read", ErrInvalidOptions)
	}
	pool, err := x509.SystemCertPool()
	if err != nil || pool == nil {
		pool = x509.NewCertPool()
	}
	if !pool.AppendCertsFromPEM(bundle) {
		return nil, fmt.Errorf("%w: the CA bundle holds no PEM certificate", ErrInvalidOptions)
	}
	return &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: pool}, nil
}

// tokenFile reads a bearer token and re-reads it periodically, because
// projected ServiceAccount tokens are rotated in place. The token is never
// logged or returned in an error.
type tokenFile struct {
	path string
	now  func() time.Time
	mu   sync.Mutex
	// value is the last token read and readAt when it was read.
	value  string
	readAt time.Time
}

func newTokenFile(path string, now func() time.Time) (*tokenFile, error) {
	token := &tokenFile{path: path, now: now}
	if _, err := token.get(); err != nil {
		return nil, err
	}
	return token, nil
}

func (token *tokenFile) get() (string, error) {
	token.mu.Lock()
	defer token.mu.Unlock()
	if token.value != "" && token.now().Sub(token.readAt) < tokenRefresh {
		return token.value, nil
	}
	raw, err := os.ReadFile(token.path)
	value := strings.TrimSpace(string(raw))
	switch {
	case err != nil && token.value != "":
		// A transient read failure keeps the last token; the server decides
		// whether it is still valid.
		return token.value, nil
	case err != nil:
		return "", fmt.Errorf("%w: %w", ErrInvalidOptions, errCredentials)
	case value == "" || strings.ContainsAny(value, "\r\n\t "):
		return "", fmt.Errorf("%w: %w: it is empty or malformed", ErrInvalidOptions, errCredentials)
	}
	token.value, token.readAt = value, token.now()
	return value, nil
}

// bearerTransport adds the token to every request. Redirects are refused by
// the client, so the token only ever reaches the configured host.
type bearerTransport struct {
	base  http.RoundTripper
	token *tokenFile
}

func (transport bearerTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	value, err := transport.token.get()
	if err != nil {
		return nil, err
	}
	authorized := request.Clone(request.Context())
	authorized.Header.Set("Authorization", "Bearer "+value)
	return transport.base.RoundTrip(authorized)
}

// configureAccess applies the CA bundle and bearer token to the client.
func configureAccess(client *http.Client, scheme string, options PrometheusOptions) error {
	if options.TenantNamespace != "" && (len(options.TenantNamespace) > 63 || !namespaceName.MatchString(options.TenantNamespace)) {
		return fmt.Errorf("%w: the tenant namespace must be a Kubernetes namespace name", ErrInvalidOptions)
	}
	if options.BearerTokenFile != "" && scheme != "https" {
		return fmt.Errorf("%w: a bearer token is never sent over cleartext HTTP", ErrInvalidOptions)
	}
	base := client.Transport
	if base == nil {
		transport := http.DefaultTransport.(*http.Transport).Clone()
		transport.Proxy = clusterAwareProxy(httpproxy.FromEnvironment().ProxyFunc())
		base = transport
	}
	if options.CAFile != "" {
		config, err := tlsConfig(options.CAFile)
		if err != nil {
			return err
		}
		transport, ok := base.(*http.Transport)
		if !ok {
			return errors.New("a CA bundle cannot be combined with a custom HTTP transport")
		}
		transport = transport.Clone()
		transport.TLSClientConfig = config
		base = transport
	}
	if options.BearerTokenFile != "" {
		token, err := newTokenFile(options.BearerTokenFile, time.Now)
		if err != nil {
			return err
		}
		base = bearerTransport{base: base, token: token}
	}
	client.Transport = base
	return nil
}

// clusterAwareProxy keeps in-cluster traffic off the HTTP proxy. A
// cluster-wide proxy is set through HTTP_PROXY and HTTPS_PROXY, and a
// Service name missing from NO_PROXY would otherwise send the request,
// with its bearer token, to the proxy, which cannot reach the Service.
// Other hosts follow the environment, including NO_PROXY.
func clusterAwareProxy(fromEnvironment func(*url.URL) (*url.URL, error)) func(*http.Request) (*url.URL, error) {
	return func(request *http.Request) (*url.URL, error) {
		if clusterInternalHost(request.URL.Hostname()) {
			return nil, nil
		}
		return fromEnvironment(request.URL)
	}
}

func clusterInternalHost(host string) bool {
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	if host == "localhost" {
		return true
	}
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback()
	}
	return strings.HasSuffix(host, ".svc") || strings.HasSuffix(host, ".cluster.local")
}

// innermostTransport finds the *http.Transport under the bearer wrapper.
func innermostTransport(roundTripper http.RoundTripper) (*http.Transport, bool) {
	switch typed := roundTripper.(type) {
	case *http.Transport:
		return typed, true
	case bearerTransport:
		return innermostTransport(typed.base)
	default:
		return nil, false
	}
}

// openShiftMonitoringHosts are OpenShift's in-cluster monitoring Services,
// which authenticate the pod's own ServiceAccount token and serve a
// certificate from the service CA.
var openShiftMonitoringHosts = map[string]bool{
	"thanos-querier.openshift-monitoring.svc":               true,
	"thanos-querier.openshift-monitoring.svc.cluster.local": true,
	"prometheus-k8s.openshift-monitoring.svc":               true,
	"prometheus-k8s.openshift-monitoring.svc.cluster.local": true,
}

// OpenShiftMonitoringCredentials returns the ServiceAccount token and
// service CA for a configured URL that names OpenShift's in-cluster
// monitoring, so setting the URL by hand works like discovery. It returns
// nothing for any other host, so the token is never sent elsewhere.
func OpenShiftMonitoringCredentials(endpoint, serviceAccountDir string) (tokenFile, caFile string, ok bool) {
	parsed, err := url.Parse(endpoint)
	if err != nil || !strings.EqualFold(parsed.Scheme, "https") || !openShiftMonitoringHosts[strings.ToLower(parsed.Hostname())] {
		return "", "", false
	}
	tokenFile, caFile = serviceAccountDir+"/token", serviceAccountDir+"/service-ca.crt"
	if !regularFile(tokenFile) || !regularFile(caFile) {
		return "", "", false
	}
	return tokenFile, caFile, true
}
