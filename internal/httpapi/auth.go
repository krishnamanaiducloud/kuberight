package httpapi

import (
	"context"
	"errors"
	"net/http"
	"strings"
)

// KubeRight has no sign-in, like Goldilocks: every request is served as one
// anonymous identity and every action is allowed. Who can reach the dashboard
// is decided by the network (port-forward, NetworkPolicy, Route, or Ingress),
// and KubeRight never changes workloads.

type Principal struct {
	Username string
	UID      string
	Groups   []string
}

type AccessAttributes struct {
	Verb        string
	APIGroup    string
	Resource    string
	Subresource string
	Namespace   string
	Name        string
}

// AccessController resolves the identity recorded with each action and
// decides whether the action is allowed. The server uses openAccess; the
// interface remains so embedders and tests can attach identities to requests.
type AccessController interface {
	Authenticate(context.Context, string) (Principal, error)
	Authorize(context.Context, Principal, AccessAttributes) error
	Ready(context.Context) error
}

type accessError struct {
	status  int
	code    string
	message string
}

func (err *accessError) Error() string { return err.message }

func newAccessError(status int, code, message string) error {
	return &accessError{status: status, code: code, message: message}
}

func accessErrorDetails(err error) (int, string, string) {
	var typed *accessError
	if errors.As(err, &typed) {
		return typed.status, typed.code, typed.message
	}
	return http.StatusServiceUnavailable, "unavailable", "the request could not be served"
}

// anonymousPrincipal is the identity every request gets, named as Kubernetes
// names unauthenticated requests.
var anonymousPrincipal = Principal{Username: "system:anonymous", Groups: []string{"system:unauthenticated"}}

// openAccess serves every request without sign-in or permission checks.
type openAccess struct{}

func (openAccess) Authenticate(context.Context, string) (Principal, error) {
	return anonymousPrincipal, nil
}

func (openAccess) Authorize(context.Context, Principal, AccessAttributes) error { return nil }

func (openAccess) Ready(context.Context) error { return nil }

// bearerToken returns the token of an "Authorization: Bearer" header, which
// only an attached AccessController reads; openAccess ignores it.
func bearerToken(header string) string {
	parts := strings.Fields(header)
	if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") || len(parts[1]) > 16_384 {
		return ""
	}
	return parts[1]
}

func contains(values []string, expected string) bool {
	for _, value := range values {
		if value == expected {
			return true
		}
	}
	return false
}
