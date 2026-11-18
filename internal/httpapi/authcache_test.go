package httpapi

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	authzv1 "k8s.io/api/authorization/v1"

	"github.com/kuberight/kuberight/internal/config"
)

// countingAccess stands in for the Kubernetes API server.
type countingAccess struct {
	authentications atomic.Int64
	authorizations  atomic.Int64
	delay           time.Duration
	authenticate    func(string) (Principal, error)
	authorize       func(Principal, AccessAttributes) error
}

func (access *countingAccess) Authenticate(_ context.Context, bearer string) (Principal, error) {
	access.authentications.Add(1)
	time.Sleep(access.delay)
	return access.authenticate(bearer)
}

func (access *countingAccess) Authorize(_ context.Context, principal Principal, attributes AccessAttributes) error {
	access.authorizations.Add(1)
	time.Sleep(access.delay)
	return access.authorize(principal, attributes)
}

func (*countingAccess) Ready(context.Context) error { return nil }

type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func (clock *fakeClock) Now() time.Time {
	clock.mu.Lock()
	defer clock.mu.Unlock()
	return clock.now
}

func (clock *fakeClock) Advance(duration time.Duration) {
	clock.mu.Lock()
	defer clock.mu.Unlock()
	clock.now = clock.now.Add(duration)
}

func jwtExpiring(at time.Time) string {
	encode := func(value string) string { return base64.RawURLEncoding.EncodeToString([]byte(value)) }
	return encode(`{"alg":"RS256"}`) + "." + encode(fmt.Sprintf(`{"sub":"alice","exp":%d}`, at.Unix())) + ".signature"
}

func TestCachedAccessReusesVerifiedIdentitiesWithinTheirLifetime(t *testing.T) {
	clock := &fakeClock{now: time.Date(2026, 11, 14, 6, 0, 0, 0, time.UTC)}
	upstream := &countingAccess{authenticate: func(token string) (Principal, error) {
		switch token {
		case "unavailable":
			return Principal{}, newAccessError(http.StatusServiceUnavailable, "authentication_unavailable", "identity verification is unavailable")
		case "rejected":
			return Principal{}, newAccessError(http.StatusUnauthorized, "unauthorized", "credentials were not accepted")
		}
		return Principal{Username: "alice", Groups: []string{"team"}, Extra: map[string]authzv1.ExtraValue{"scopes": {"read"}}}, nil
	}}
	access := newCachedAccess(upstream, clock.Now)
	ctx := context.Background()

	first, err := access.Authenticate(ctx, "opaque-token")
	if err != nil || first.Username != "alice" {
		t.Fatalf("first=%+v err=%v", first, err)
	}
	// A caller that changes its copy cannot change the cached identity.
	first.Groups[0], first.Extra["scopes"][0] = "admins", "write"
	again, _ := access.Authenticate(ctx, "opaque-token")
	if upstream.authentications.Load() != 1 || again.Groups[0] != "team" || again.Extra["scopes"][0] != "read" {
		t.Fatalf("calls=%d again=%+v", upstream.authentications.Load(), again)
	}
	clock.Advance(authenticationTTL)
	if _, err := access.Authenticate(ctx, "opaque-token"); err != nil || upstream.authentications.Load() != 2 {
		t.Fatalf("an expired entry must be verified again: calls=%d err=%v", upstream.authentications.Load(), err)
	}

	// A token that expires sooner than the cache lifetime is cached only until then.
	shortLived := jwtExpiring(clock.Now().Add(20 * time.Second))
	_, _ = access.Authenticate(ctx, shortLived)
	clock.Advance(21 * time.Second)
	_, _ = access.Authenticate(ctx, shortLived)
	if upstream.authentications.Load() != 4 {
		t.Fatalf("a token past its exp claim was served from the cache: calls=%d", upstream.authentications.Load())
	}

	// Rejections are cached briefly; an unavailable API server never is.
	for range 3 {
		if _, err := access.Authenticate(ctx, "rejected"); !deniedAccess(err) {
			t.Fatalf("rejected token: %v", err)
		}
		if _, err := access.Authenticate(ctx, "unavailable"); deniedAccess(err) || err == nil {
			t.Fatalf("unavailable: %v", err)
		}
	}
	if upstream.authentications.Load() != 4+1+3 {
		t.Fatalf("calls=%d, want one for the rejection and three for the outage", upstream.authentications.Load())
	}
	clock.Advance(authenticationDeniedTTL)
	_, _ = access.Authenticate(ctx, "rejected")
	if upstream.authentications.Load() != 9 {
		t.Fatalf("an expired rejection must be checked again: calls=%d", upstream.authentications.Load())
	}
}

func TestCachedAccessKeysDecisionsByIdentityAndAttributes(t *testing.T) {
	clock := &fakeClock{now: time.Date(2026, 11, 14, 6, 0, 0, 0, time.UTC)}
	upstream := &countingAccess{authorize: func(principal Principal, attributes AccessAttributes) error {
		if attributes.Verb == "update" && principal.Username != "bob" {
			return newAccessError(http.StatusForbidden, "forbidden", "permission was denied")
		}
		return nil
	}}
	access := newCachedAccess(upstream, clock.Now)
	ctx := context.Background()
	alice := Principal{Username: "alice", Groups: []string{"b", "a"}}
	read := AccessAttributes{Verb: "list", APIGroup: apiGroup, Resource: "recommendations", Namespace: "shop"}
	update := AccessAttributes{Verb: "update", APIGroup: apiGroup, Resource: "recommendations", Subresource: "review", Namespace: "shop"}

	for range 5 {
		if err := access.Authorize(ctx, alice, read); err != nil {
			t.Fatal(err)
		}
	}
	// Group order does not matter; group membership does.
	if err := access.Authorize(ctx, Principal{Username: "alice", Groups: []string{"a", "b"}}, read); err != nil || upstream.authorizations.Load() != 1 {
		t.Fatalf("calls=%d err=%v", upstream.authorizations.Load(), err)
	}
	_ = access.Authorize(ctx, Principal{Username: "alice", Groups: []string{"a"}}, read)
	other := read
	other.Namespace = "payments"
	_ = access.Authorize(ctx, alice, other)
	if upstream.authorizations.Load() != 3 {
		t.Fatalf("different groups or namespaces must be authorized separately: calls=%d", upstream.authorizations.Load())
	}
	if err := access.Authorize(ctx, alice, update); !deniedAccess(err) {
		t.Fatalf("alice update: %v", err)
	}
	if err := access.Authorize(ctx, Principal{Username: "bob"}, update); err != nil {
		t.Fatalf("bob update: %v", err)
	}
	// A revoked grant takes effect once the allowed entry expires.
	clock.Advance(authorizationAllowedTTL)
	before := upstream.authorizations.Load()
	_ = access.Authorize(ctx, alice, read)
	if upstream.authorizations.Load() != before+1 {
		t.Fatal("an expired grant was served from the cache")
	}
}

func TestCachedAccessCollapsesConcurrentLookups(t *testing.T) {
	upstream := &countingAccess{delay: 50 * time.Millisecond,
		authenticate: func(string) (Principal, error) { return Principal{Username: "alice"}, nil },
		authorize:    func(Principal, AccessAttributes) error { return nil }}
	access := newCachedAccess(upstream, nil)
	var group sync.WaitGroup
	for range 50 {
		group.Add(1)
		go func() {
			defer group.Done()
			principal, err := access.Authenticate(context.Background(), "token")
			if err == nil {
				err = access.Authorize(context.Background(), principal, AccessAttributes{Verb: "get", Resource: "overview"})
			}
			if err != nil {
				t.Error(err)
			}
		}()
	}
	group.Wait()
	if upstream.authentications.Load() != 1 || upstream.authorizations.Load() != 1 {
		t.Fatalf("authentications=%d authorizations=%d, want one each", upstream.authentications.Load(), upstream.authorizations.Load())
	}
}

func TestTTLCacheEvictsTheLeastRecentlyUsedEntry(t *testing.T) {
	cache := newTTLCache[int](2)
	now := time.Date(2026, 11, 14, 6, 0, 0, 0, time.UTC)
	cache.put("a", 1, nil, now.Add(time.Minute))
	cache.put("b", 2, nil, now.Add(time.Minute))
	if _, _, ok := cache.get("a", now); !ok {
		t.Fatal("a missing")
	}
	cache.put("c", 3, nil, now.Add(time.Minute))
	if _, _, ok := cache.get("b", now); ok {
		t.Fatal("b should have been evicted as least recently used")
	}
	if value, _, ok := cache.get("a", now); !ok || value != 1 {
		t.Fatal("a should remain")
	}
}

func TestProxyPrincipalPrefixesTheAssertedIdentity(t *testing.T) {
	header := http.Header{}
	header.Set("X-Forwarded-User", "CiQwOGE4Njg0Yi1kYjg4")
	header.Set("X-Forwarded-Email", "alice@example.com")
	header.Add("X-Forwarded-Groups", "platform, sre")
	header.Add("X-Forwarded-Groups", "admins")
	principal, ok := proxyPrincipal(header, "oidc:", "oidc:")
	if !ok || principal.Username != "oidc:alice@example.com" {
		t.Fatalf("principal=%+v ok=%v", principal, ok)
	}
	if want := []string{"system:authenticated", "oidc:platform", "oidc:sre", "oidc:admins"}; strings.Join(principal.Groups, ",") != strings.Join(want, ",") {
		t.Fatalf("groups=%v", principal.Groups)
	}
	for name, mutate := range map[string]func(http.Header){
		"no identity":      func(h http.Header) { h.Del("X-Forwarded-Email"); h.Del("X-Forwarded-User") },
		"header splitting": func(h http.Header) { h.Set("X-Forwarded-Email", "alice\r\nX-Admin: true") },
	} {
		copied := header.Clone()
		mutate(copied)
		if _, ok := proxyPrincipal(copied, "oidc:", "oidc:"); ok {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestOIDCModeTrustsOnlyProxyHeadersAndStillAuthorizes(t *testing.T) {
	api := newTestAPI(t, func(cfg *config.Config) { cfg.AuthMode = "oidc"; cfg.ListenAddress = "127.0.0.1:8080" }, Dependencies{})
	request := httptest.NewRequest(http.MethodGet, "/api/v1/recommendations", nil)
	request.Header.Set("Authorization", "Bearer forged")
	response := httptest.NewRecorder()
	api.handler.ServeHTTP(response, request)
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("a bearer token without proxy headers: %d", response.Code)
	}
	request = httptest.NewRequest(http.MethodGet, "/api/v1/recommendations", nil)
	request.Header.Set("X-Forwarded-Email", "alice@example.com")
	response = httptest.NewRecorder()
	api.handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK || len(api.access.calls) == 0 || api.access.calls[len(api.access.calls)-1].Resource != "recommendations" {
		t.Fatalf("proxy identity: %d calls=%v", response.Code, api.access.calls)
	}
}
