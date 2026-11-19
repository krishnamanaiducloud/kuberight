package metrics

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net/http"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"
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
		return "", fmt.Errorf("%w: the bearer token file could not be read", ErrInvalidOptions)
	case value == "" || strings.ContainsAny(value, "\r\n\t "):
		return "", fmt.Errorf("%w: the bearer token file is empty or malformed", ErrInvalidOptions)
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
	if options.CAFile != "" {
		config, err := tlsConfig(options.CAFile)
		if err != nil {
			return err
		}
		transport, ok := base.(*http.Transport)
		switch {
		case base == nil:
			transport = http.DefaultTransport.(*http.Transport).Clone()
		case ok:
			transport = transport.Clone()
		default:
			return errors.New("a CA bundle cannot be combined with a custom HTTP transport")
		}
		transport.TLSClientConfig = config
		base = transport
	}
	if options.BearerTokenFile != "" {
		token, err := newTokenFile(options.BearerTokenFile, time.Now)
		if err != nil {
			return err
		}
		if base == nil {
			base = http.DefaultTransport
		}
		base = bearerTransport{base: base, token: token}
	}
	client.Transport = base
	return nil
}
