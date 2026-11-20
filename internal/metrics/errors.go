package metrics

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"syscall"
	"time"
)

// FailureKind says why a Prometheus request failed, in terms an operator
// can act on. The values are stable: they appear in logs and in the
// integration status.
type FailureKind string

const (
	FailureInvalidQuery   FailureKind = "invalid_query"           // HTTP 400 or 422, or errorType bad_data
	FailureAuthentication FailureKind = "authentication_failed"   // HTTP 401
	FailureAuthorization  FailureKind = "authorization_denied"    // HTTP 403
	FailureEndpoint       FailureKind = "endpoint_not_found"      // HTTP 404: wrong path or port
	FailureRateLimited    FailureKind = "rate_limited"            // HTTP 429
	FailureUpstream       FailureKind = "upstream_error"          // HTTP 5xx, or another status
	FailureTLS            FailureKind = "tls_error"               // certificate or handshake
	FailureDNS            FailureKind = "dns_error"               // the host name does not resolve
	FailureConnection     FailureKind = "connection_failed"       // refused, reset, or unreachable
	FailureProxy          FailureKind = "proxy_error"             // the HTTP proxy could not be reached
	FailureTimeout        FailureKind = "timeout"                 // the request ran out of time
	FailureCanceled       FailureKind = "canceled"                // the caller went away
	FailureCredentials    FailureKind = "credentials_unavailable" // the token file could not be read
	FailureProtocol       FailureKind = "invalid_response"        // HTTP 200 with an unusable payload
)

// errCredentials marks a bearer token that could not be read; the token
// itself never appears in an error.
var errCredentials = errors.New("the bearer token file could not be read")

// QueryError describes one failed Prometheus request. It never holds a
// header, token, query string, or upstream response text. Error() leaves out
// the host too, because API responses can carry it; the host is a field for
// logs and the integration status.
type QueryError struct {
	Kind       FailureKind
	Query      string // the caller's name for the query, such as "hotspot.throttled_ratio"
	Host       string // host and port of the endpoint
	Method     string
	StatusCode int    // 0 when no response arrived
	ErrorType  string // Prometheus errorType, when it is a documented value
	Message    string // a fixed description, never upstream text
	Duration   time.Duration
	ViaProxy   bool
	cause      error
}

func (err *QueryError) Error() string {
	var text strings.Builder
	text.WriteString("Prometheus ")
	text.WriteString(string(err.Kind))
	if err.StatusCode != 0 {
		fmt.Fprintf(&text, " (HTTP %d", err.StatusCode)
		if err.ErrorType != "" {
			text.WriteString(", " + err.ErrorType)
		}
		text.WriteString(")")
	}
	if err.ViaProxy {
		text.WriteString(" through the HTTP proxy")
	}
	if err.Message != "" {
		text.WriteString(": " + err.Message)
	}
	return text.String()
}

// Unwrap keeps errors.Is working for the package sentinels and for context
// cancellation and deadlines.
func (err *QueryError) Unwrap() []error {
	errs := []error{ErrUpstream}
	if err.Kind == FailureProtocol {
		errs = []error{ErrProtocol}
	}
	switch {
	case errors.Is(err.cause, context.Canceled):
		errs = append(errs, context.Canceled)
	case errors.Is(err.cause, context.DeadlineExceeded):
		errs = append(errs, context.DeadlineExceeded)
	}
	return errs
}

// AuthFailure reports whether the request was refused for its identity.
func (err *QueryError) AuthFailure() bool {
	return err.Kind == FailureAuthentication || err.Kind == FailureAuthorization || err.Kind == FailureCredentials
}

// AsQueryError returns the QueryError in err's chain, if any.
func AsQueryError(err error) (*QueryError, bool) {
	var queryError *QueryError
	ok := errors.As(err, &queryError)
	return queryError, ok
}

// FailureOf classifies any error returned by QueryRange.
func FailureOf(err error) FailureKind {
	if queryError, ok := AsQueryError(err); ok {
		return queryError.Kind
	}
	switch {
	case err == nil:
		return ""
	case errors.Is(err, ErrInvalidQuery):
		return FailureInvalidQuery
	case errors.Is(err, ErrProtocol), errors.Is(err, ErrResponseTooLarge):
		return FailureProtocol
	case errors.Is(err, context.Canceled):
		return FailureCanceled
	case errors.Is(err, context.DeadlineExceeded):
		return FailureTimeout
	default:
		return FailureUpstream
	}
}

// statusFailure maps a non-200 response, or an error envelope, to its kind.
func statusFailure(status int, errorType string) FailureKind {
	switch {
	case status == http.StatusBadRequest || status == http.StatusUnprocessableEntity || errorType == "bad_data":
		return FailureInvalidQuery
	case status == http.StatusUnauthorized:
		return FailureAuthentication
	case status == http.StatusForbidden:
		return FailureAuthorization
	case status == http.StatusNotFound:
		return FailureEndpoint
	case status == http.StatusTooManyRequests:
		return FailureRateLimited
	case errorType == "timeout":
		return FailureTimeout
	default:
		return FailureUpstream
	}
}

var knownErrorTypes = map[string]bool{
	"bad_data": true, "timeout": true, "canceled": true, "execution": true, "unavailable": true,
	"internal": true, "not_found": true, "not_acceptable": true,
}

// apiErrorType returns Prometheus's errorType when it is one of the
// documented values. Upstream error text is never kept: it can echo the
// query, or anything a proxy put in its body.
func apiErrorType(body []byte) string {
	var payload struct {
		ErrorType string `json:"errorType"`
	}
	if json.Unmarshal(body, &payload) == nil && knownErrorTypes[payload.ErrorType] {
		return payload.ErrorType
	}
	return ""
}

// statusMessage describes a failed status without quoting the body.
func statusMessage(kind FailureKind, status int) string {
	switch kind {
	case FailureInvalidQuery:
		return "Prometheus rejected the PromQL"
	case FailureAuthentication:
		return "the bearer token was missing or not accepted"
	case FailureAuthorization:
		return "the ServiceAccount is not allowed to query this endpoint"
	case FailureEndpoint:
		return "no Prometheus API at this URL; check the path and port"
	case FailureRateLimited:
		return "Prometheus is rate limiting KubeRight"
	case FailureTimeout:
		return "the query timed out in Prometheus"
	}
	if text := http.StatusText(status); text != "" {
		return "Prometheus answered " + text
	}
	return "Prometheus answered with an error"
}

// transportFailure classifies an error from http.Client.Do, where no
// response arrived, into a kind and a fixed description. The raw error is
// not quoted: it can carry the request URL with its query string.
func transportFailure(ctx context.Context, err error) (FailureKind, string) {
	var (
		dnsError         *net.DNSError
		opError          *net.OpError
		unknownAuthority x509.UnknownAuthorityError
		hostnameError    x509.HostnameError
		invalidCert      x509.CertificateInvalidError
		verification     *tls.CertificateVerificationError
		recordHeader     tls.RecordHeaderError
		netError         net.Error
	)
	switch {
	case errors.Is(err, errCredentials):
		return FailureCredentials, "the ServiceAccount token file could not be read"
	case ctx.Err() == context.Canceled || errors.Is(err, context.Canceled):
		return FailureCanceled, "the caller canceled the request"
	case errors.As(err, &opError) && opError.Op == "proxyconnect":
		return FailureProxy, "the HTTP proxy could not be reached"
	case errors.As(err, &unknownAuthority):
		return FailureTLS, "the server certificate is signed by an unknown authority; trust its CA with prometheus.caFile, or the service CA on OpenShift"
	case errors.As(err, &hostnameError):
		return FailureTLS, "the server certificate is not valid for this host name"
	case errors.As(err, &invalidCert):
		return FailureTLS, "the server certificate is invalid: " + certificateReason(invalidCert.Reason)
	case errors.As(err, &verification):
		return FailureTLS, "the server certificate could not be verified"
	case errors.As(err, &recordHeader):
		return FailureTLS, "the server did not answer with TLS; check the URL scheme and port"
	case errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &netError) && netError.Timeout()):
		return FailureTimeout, "no response within the Prometheus timeout"
	case errors.As(err, &dnsError):
		return FailureDNS, "the host name does not resolve"
	case errors.Is(err, syscall.ECONNREFUSED):
		return FailureConnection, "the connection was refused"
	case errors.Is(err, syscall.ECONNRESET):
		return FailureConnection, "the connection was reset"
	case errors.Is(err, syscall.EHOSTUNREACH), errors.Is(err, syscall.ENETUNREACH):
		return FailureConnection, "the host is unreachable"
	case errors.As(err, &opError) && strings.HasPrefix(opError.Op, "remote error"):
		return FailureTLS, "the TLS handshake failed"
	default:
		return FailureConnection, "the request failed before a response arrived"
	}
}

func certificateReason(reason x509.InvalidReason) string {
	switch reason {
	case x509.Expired:
		return "expired or not yet valid"
	case x509.NotAuthorizedToSign:
		return "signed by a certificate that is not a CA"
	case x509.IncompatibleUsage:
		return "not valid for server authentication"
	default:
		return "rejected"
	}
}
