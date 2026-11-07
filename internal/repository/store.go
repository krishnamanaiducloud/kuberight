package repository

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

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

// ErrReviewConflict is returned when a recommendation has already left the
// pending state. Reviews are single-assignment so an approval can never be
// silently replaced by a later rejection, or vice versa.
var ErrReviewConflict = errors.New("recommendation has already been reviewed")

// ErrInvalidReview identifies a review decision that cannot be stored.
var ErrInvalidReview = errors.New("invalid recommendation review")

// Review states. A recommendation starts pending. Approval records that a
// reviewer accepted it for manual or GitOps application; KubeRight itself
// never applies it.
const (
	ReviewPending  = "pending"
	ReviewApproved = "approved"
	ReviewRejected = "rejected"
)

// Recommendation provenance values.
const (
	SourceRequest    = "request"
	SourcePrometheus = "prometheus"
)

const (
	maxReviewCommentLength = 1024
	maxActorLength         = 512
)

// ReviewDecision is a terminal review of a pending recommendation.
type ReviewDecision struct {
	Status   string
	Reviewer string
	Comment  string
	At       time.Time
}

// Validate rejects decisions that cannot be persisted. It does not enforce
// separation of duties; that is an API policy decision.
func (decision ReviewDecision) Validate() error {
	if decision.Status != ReviewApproved && decision.Status != ReviewRejected {
		return fmt.Errorf("%w: status must be approved or rejected", ErrInvalidReview)
	}
	if strings.TrimSpace(decision.Reviewer) == "" || len(decision.Reviewer) > maxActorLength {
		return fmt.Errorf("%w: reviewer must contain 1 to %d bytes", ErrInvalidReview, maxActorLength)
	}
	if len(decision.Comment) > maxReviewCommentLength || !utf8.ValidString(decision.Comment) {
		return fmt.Errorf("%w: comment must be valid UTF-8 of at most %d bytes", ErrInvalidReview, maxReviewCommentLength)
	}
	if decision.At.IsZero() {
		return fmt.Errorf("%w: review time is required", ErrInvalidReview)
	}
	return nil
}

func validReviewStatus(status string) bool {
	return status == ReviewPending || status == ReviewApproved || status == ReviewRejected
}

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
//
// ReviewRecommendation moves a pending recommendation to a terminal review
// state. It returns ErrNotFound for an unknown ID and ErrReviewConflict when
// the recommendation is no longer pending.
type RecommendationRepository interface {
	CreateRecommendation(ctx context.Context, recommendation optimizer.Recommendation, options CreateOptions) (record RecommendationRecord, created bool, err error)
	GetRecommendation(ctx context.Context, id string) (RecommendationRecord, error)
	ListRecommendations(ctx context.Context, options ListOptions) (ListResult, error)
	CountRecommendations(ctx context.Context) (int, error)
	ReviewRecommendation(ctx context.Context, id string, decision ReviewDecision) (RecommendationRecord, error)
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
	PolicyRepository
	FleetRepository
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
	VerificationRepository
	InTransaction(ctx context.Context, fn func(context.Context, Transaction) error) error
	// Ready reports whether the store can currently serve requests.
	Ready(ctx context.Context) error
	// Kind names the backing implementation for status reporting.
	Kind() string
}

// ValidateListOptions rejects list options that an implementation would
// otherwise have to interpret. The API validates user input first; this is a
// second line of defense for embedders.
func ValidateListOptions(options ListOptions) error {
	if options.Page < 1 || options.PageSize < 1 || options.PageSize > 10_000 {
		return errors.New("page must be positive and pageSize must be between 1 and 10000")
	}
	if options.ReviewStatus != "" && !validReviewStatus(options.ReviewStatus) {
		return errors.New("review status filter is not supported")
	}
	switch options.Sort {
	case "", "createdAt", "namespace", "workload", "confidence", "risk":
	default:
		return errors.New("sort field is not supported")
	}
	if options.Order != "" && options.Order != "asc" && options.Order != "desc" {
		return errors.New("order must be asc or desc")
	}
	return nil
}
