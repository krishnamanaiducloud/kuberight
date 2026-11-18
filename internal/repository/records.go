package repository

import (
	"errors"
	"strings"
	"time"

	"github.com/kuberight/kuberight/internal/optimizer"
)

const maxIdempotencyKeyLength = 128

// ErrIdempotencyConflict is returned when a caller reuses an idempotency key
// in the same scope for a semantically different request.
var ErrIdempotencyConflict = errors.New("idempotency key was already used for a different request")

// RecommendationRecord is a stored recommendation with its provenance and
// review state. The embedded Recommendation is the optimizer's immutable
// output; review fields are the only mutable part of a record.
type RecommendationRecord struct {
	optimizer.Recommendation
	CreatedAt     time.Time  `json:"createdAt"`
	CreatedBy     string     `json:"createdBy,omitempty"`
	Source        string     `json:"source"`
	ReviewStatus  string     `json:"reviewStatus"`
	ReviewedBy    string     `json:"reviewedBy,omitempty"`
	ReviewedAt    *time.Time `json:"reviewedAt,omitempty"`
	ReviewComment string     `json:"reviewComment,omitempty"`
}

type AuditEvent struct {
	ID        string    `json:"id"`
	Timestamp time.Time `json:"timestamp"`
	Actor     string    `json:"actor"`
	Action    string    `json:"action"`
	Resource  string    `json:"resource"`
	Namespace string    `json:"namespace,omitempty"`
	Outcome   string    `json:"outcome"`
	RequestID string    `json:"requestId"`
}

type ListOptions struct {
	Page         int
	PageSize     int
	Namespace    string
	Workload     string
	Confidence   string
	Risk         string
	ReviewStatus string
	Filter       string
	Sort         string
	Order        string
	// LatestOnly keeps only the newest record for each cluster, namespace,
	// workload kind, workload, and container before filters are applied.
	LatestOnly bool
}

type CreateOptions struct {
	IdempotencyKey string
	Scope          string
	Fingerprint    string
	CreatedBy      string
	Source         string
}

type ListResult struct {
	Items    []RecommendationRecord
	Page     int
	PageSize int
	Total    int
}

func validIdempotencyKey(key string) bool {
	if len(key) == 0 || len(key) > maxIdempotencyKeyLength {
		return false
	}
	for _, character := range key {
		if (character >= 'a' && character <= 'z') || (character >= 'A' && character <= 'Z') || (character >= '0' && character <= '9') || strings.ContainsRune("._:-", character) {
			continue
		}
		return false
	}
	return true
}

// NormalizeSource validates a recommendation source and applies the default.
func NormalizeSource(source string) (string, error) {
	switch source {
	case "":
		return SourceRequest, nil
	case SourceRequest, SourcePrometheus:
		return source, nil
	default:
		return "", errors.New("recommendation source is not supported")
	}
}

func levelRank(level string) int {
	switch strings.ToLower(level) {
	case "low":
		return 1
	case "medium":
		return 2
	case "high":
		return 3
	default:
		return 0
	}
}

// latestKey identifies the subject of a recommendation for LatestOnly lists.
func latestKey(record RecommendationRecord) string {
	return strings.Join([]string{record.Cluster, record.Namespace, record.Kind, record.Workload, record.Container}, "\x00")
}
