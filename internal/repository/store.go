package repository

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	"github.com/kuberight/kuberight/internal/optimizer"
)

const maxIdempotencyScopeLength = 1024

// ErrNotFound is returned when a durable repository cannot find a requested
// record. Callers should use errors.Is instead of comparing error text.
var ErrNotFound = errors.New("repository record not found")

// ErrInvalidIdempotency identifies an idempotency claim that cannot be stored
// safely. It is deliberately separate from ErrIdempotencyConflict, which
// represents a valid key reused for a different request.
var ErrInvalidIdempotency = errors.New("invalid idempotency claim")

// IdempotencyClaim is the normalized representation persisted by a durable
// repository. ScopeDigest avoids retaining identity-bearing scope data and
// also supports opaque scopes containing bytes, such as NUL separators, that
// PostgreSQL text values cannot represent.
type IdempotencyClaim struct {
	Key                string
	ScopeDigest        [sha256.Size]byte
	RequestFingerprint [sha256.Size]byte
}

// NewIdempotencyClaim validates and normalizes API create options. A missing
// idempotency key means that no claim is required and returns nil.
// Fingerprint must be a lowercase hexadecimal SHA-256 digest.
func NewIdempotencyClaim(options CreateOptions) (*IdempotencyClaim, error) {
	if options.IdempotencyKey == "" {
		return nil, nil
	}
	if !validIdempotencyKey(options.IdempotencyKey) {
		return nil, fmt.Errorf("%w: key contains unsupported characters or is too long", ErrInvalidIdempotency)
	}
	if strings.TrimSpace(options.Scope) == "" || len(options.Scope) > maxIdempotencyScopeLength {
		return nil, fmt.Errorf("%w: scope must contain 1 to %d bytes", ErrInvalidIdempotency, maxIdempotencyScopeLength)
	}
	if len(options.Fingerprint) != sha256.Size*2 || strings.ToLower(options.Fingerprint) != options.Fingerprint {
		return nil, fmt.Errorf("%w: request fingerprint must be a lowercase SHA-256 digest", ErrInvalidIdempotency)
	}
	fingerprint, err := hex.DecodeString(options.Fingerprint)
	if err != nil {
		return nil, fmt.Errorf("%w: request fingerprint must be a lowercase SHA-256 digest", ErrInvalidIdempotency)
	}

	claim := &IdempotencyClaim{
		Key:         options.IdempotencyKey,
		ScopeDigest: sha256.Sum256([]byte(options.Scope)),
	}
	copy(claim.RequestFingerprint[:], fingerprint)
	return claim, nil
}

// RecommendationRepository is the durable recommendation contract. Every
// implementation must propagate the supplied context to database I/O and stop
// new work after cancellation.
//
// CreateRecommendation must write a new recommendation and its optional
// idempotency claim atomically. A matching replay returns the original record
// with created=false. Reusing the same scope and key with a different request
// fingerprint returns ErrIdempotencyConflict and performs no writes.
type RecommendationRepository interface {
	CreateRecommendation(ctx context.Context, recommendation optimizer.Recommendation, options CreateOptions) (record RecommendationRecord, created bool, err error)
	GetRecommendation(ctx context.Context, id string) (RecommendationRecord, error)
	ListRecommendations(ctx context.Context, options ListOptions) (ListResult, error)
	CountRecommendations(ctx context.Context) (int, error)
}

// AuditListResult is a stable page of audit events and the count before
// pagination was applied.
type AuditListResult struct {
	Items    []AuditEvent
	Page     int
	PageSize int
	Total    int
}

// AuditRepository stores append-only security and change events. Implementors
// may delete events only through an explicit retention operation, never as a
// side effect of an update to another entity.
type AuditRepository interface {
	AppendAuditEvent(ctx context.Context, event AuditEvent) error
	ListAuditEvents(ctx context.Context, page, pageSize int) (AuditListResult, error)
}

// Transaction exposes all operations that can participate in one database
// transaction. Implementations must reject use after the callback passed to
// InTransaction returns.
type Transaction interface {
	RecommendationRepository
	AuditRepository
}

// Store is the context-aware durable persistence boundary.
//
// InTransaction commits only when fn returns nil. It passes fn the context that
// must be used for transactional calls. It rolls back when fn returns an error,
// the context is canceled, or fn panics; a panic must be re-raised after
// rollback. This permits recommendation, idempotency, and audit writes to share
// one all-or-nothing boundary.
type Store interface {
	Transaction
	InTransaction(ctx context.Context, fn func(context.Context, Transaction) error) error
}
