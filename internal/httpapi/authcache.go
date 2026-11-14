package httpapi

import (
	"container/list"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"
	authzv1 "k8s.io/api/authorization/v1"
)

// Cache lifetimes follow the Kubernetes API server's webhook token
// authenticator (2 minutes) and keep authorization decisions shorter, so a
// revoked RoleBinding takes effect within 30 seconds. Failures to reach the
// API server are never cached.
const (
	authenticationTTL       = 2 * time.Minute
	authenticationDeniedTTL = 10 * time.Second
	authorizationAllowedTTL = 30 * time.Second
	authorizationDeniedTTL  = 10 * time.Second
	accessCacheEntries      = 8192
)

// cachedAccess caches TokenReview and SubjectAccessReview outcomes. Every
// request otherwise costs at least two API server calls, and client-go's
// rate limiter then caps the whole API at a few requests per second.
type cachedAccess struct {
	next   AccessController
	now    func() time.Time
	tokens *ttlCache[Principal]
	grants *ttlCache[struct{}]
	flight singleflight.Group
}

func newCachedAccess(next AccessController, now func() time.Time) *cachedAccess {
	if now == nil {
		now = time.Now
	}
	return &cachedAccess{next: next, now: now, tokens: newTTLCache[Principal](accessCacheEntries), grants: newTTLCache[struct{}](accessCacheEntries)}
}

func (access *cachedAccess) Authenticate(ctx context.Context, bearer string) (Principal, error) {
	digest := sha256.Sum256([]byte(bearer))
	key := "token\x00" + string(digest[:])
	now := access.now()
	if principal, err, ok := access.tokens.get(key, now); ok {
		return clonePrincipal(principal), err
	}
	value, err, _ := access.flight.Do(key, func() (any, error) {
		// A flight that finished since the lookup above has stored its result.
		if principal, err, ok := access.tokens.get(key, access.now()); ok {
			return principal, err
		}
		principal, err := access.next.Authenticate(context.WithoutCancel(ctx), bearer)
		switch {
		case err == nil:
			ttl := authenticationTTL
			if expiry, ok := tokenExpiry(bearer); ok {
				ttl = min(ttl, expiry.Sub(access.now()))
			}
			if ttl > 0 {
				access.tokens.put(key, principal, nil, access.now().Add(ttl))
			}
		case deniedAccess(err):
			access.tokens.put(key, Principal{}, err, access.now().Add(authenticationDeniedTTL))
		}
		return principal, err
	})
	principal, _ := value.(Principal)
	return clonePrincipal(principal), err
}

// clonePrincipal copies the slices and maps a cached identity shares.
func clonePrincipal(principal Principal) Principal {
	principal.Groups = append([]string(nil), principal.Groups...)
	if principal.Extra != nil {
		extra := make(map[string]authzv1.ExtraValue, len(principal.Extra))
		for key, values := range principal.Extra {
			extra[key] = append(authzv1.ExtraValue(nil), values...)
		}
		principal.Extra = extra
	}
	return principal
}

func (access *cachedAccess) Authorize(ctx context.Context, principal Principal, attributes AccessAttributes) error {
	key := authorizationKey(principal, attributes)
	now := access.now()
	if _, err, ok := access.grants.get(key, now); ok {
		return err
	}
	_, err, _ := access.flight.Do(key, func() (any, error) {
		if _, err, ok := access.grants.get(key, access.now()); ok {
			return nil, err
		}
		err := access.next.Authorize(context.WithoutCancel(ctx), principal, attributes)
		switch {
		case err == nil:
			access.grants.put(key, struct{}{}, nil, access.now().Add(authorizationAllowedTTL))
		case deniedAccess(err):
			access.grants.put(key, struct{}{}, err, access.now().Add(authorizationDeniedTTL))
		}
		return nil, err
	})
	return err
}

func (access *cachedAccess) Ready(ctx context.Context) error { return access.next.Ready(ctx) }

// deniedAccess reports a definitive rejection, as opposed to the API server
// being unavailable.
func deniedAccess(err error) bool {
	var typed *accessError
	return errors.As(err, &typed) && (typed.status == http.StatusUnauthorized || typed.status == http.StatusForbidden)
}

// tokenExpiry reads the exp claim of a JWT without trusting it: it only
// shortens how long an already verified identity is cached.
func tokenExpiry(token string) (time.Time, bool) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return time.Time{}, false
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return time.Time{}, false
	}
	var claims struct {
		Exp json.Number `json:"exp"`
	}
	if json.Unmarshal(payload, &claims) != nil || claims.Exp == "" {
		return time.Time{}, false
	}
	seconds, err := claims.Exp.Int64()
	if err != nil {
		return time.Time{}, false
	}
	return time.Unix(seconds, 0), true
}

// authorizationKey identifies a decision by the complete identity and the
// complete request attributes, with unambiguous length-prefixed fields.
func authorizationKey(principal Principal, attributes AccessAttributes) string {
	hash := sha256.New()
	write := func(value string) {
		var length [8]byte
		binary.BigEndian.PutUint64(length[:], uint64(len(value)))
		hash.Write(length[:])
		hash.Write([]byte(value))
	}
	write(principal.Username)
	write(principal.UID)
	groups := append([]string(nil), principal.Groups...)
	sort.Strings(groups)
	write(strings.Join(groups, "\x00"))
	keys := make([]string, 0, len(principal.Extra))
	for key := range principal.Extra {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		write(key)
		write(strings.Join(principal.Extra[key], "\x00"))
	}
	for _, value := range []string{attributes.Verb, attributes.APIGroup, attributes.Resource, attributes.Subresource, attributes.Namespace, attributes.Name} {
		write(value)
	}
	return "grant\x00" + string(hash.Sum(nil))
}

// ttlCache is a bounded least-recently-used cache whose entries expire.
type ttlCache[T any] struct {
	mu       sync.Mutex
	capacity int
	order    *list.List
	entries  map[string]*list.Element
}

type ttlEntry[T any] struct {
	key     string
	value   T
	err     error
	expires time.Time
}

func newTTLCache[T any](capacity int) *ttlCache[T] {
	return &ttlCache[T]{capacity: capacity, order: list.New(), entries: map[string]*list.Element{}}
}

func (cache *ttlCache[T]) get(key string, now time.Time) (T, error, bool) {
	cache.mu.Lock()
	defer cache.mu.Unlock()
	element, ok := cache.entries[key]
	if !ok {
		var zero T
		return zero, nil, false
	}
	entry := element.Value.(*ttlEntry[T])
	if !now.Before(entry.expires) {
		cache.order.Remove(element)
		delete(cache.entries, key)
		var zero T
		return zero, nil, false
	}
	cache.order.MoveToFront(element)
	return entry.value, entry.err, true
}

func (cache *ttlCache[T]) put(key string, value T, err error, expires time.Time) {
	cache.mu.Lock()
	defer cache.mu.Unlock()
	if element, ok := cache.entries[key]; ok {
		element.Value = &ttlEntry[T]{key: key, value: value, err: err, expires: expires}
		cache.order.MoveToFront(element)
		return
	}
	cache.entries[key] = cache.order.PushFront(&ttlEntry[T]{key: key, value: value, err: err, expires: expires})
	for cache.order.Len() > cache.capacity {
		oldest := cache.order.Back()
		cache.order.Remove(oldest)
		delete(cache.entries, oldest.Value.(*ttlEntry[T]).key)
	}
}
