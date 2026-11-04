package repository

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode/utf8"
)

// Verification statuses are deliberately narrower than recommendation review
// states. A review is a human decision; a verification is observed evidence
// about the live target configuration and its post-change behavior.
const (
	VerificationUnavailable  = "unavailable"
	VerificationInconclusive = "inconclusive"
	VerificationNotApplied   = "not_applied"
	VerificationPending      = "pending"
	VerificationVerified     = "verified"
	VerificationRegressed    = "regressed"
)

const (
	VerificationAttributionNone        = "none"
	VerificationAttributionExactPodUID = "exact_pod_uid"

	maxVerificationChecks  = 64
	maxVerificationReasons = 64
	maxVerificationText    = 2048
)

var verificationRecommendationID = regexp.MustCompile(`^rec_[0-9a-f]{32}$`)

// VerificationCheck is one durable, machine-readable before/after or absolute
// safety check. Missing values stay nil and must never be interpreted as zero.
type VerificationCheck struct {
	Name      string   `json:"name"`
	Before    *float64 `json:"before,omitempty"`
	After     *float64 `json:"after,omitempty"`
	Regressed bool     `json:"regressed"`
	Detail    string   `json:"detail"`
}

// VerificationRun is append-only evidence from one verification attempt.
// FirstObservedAt is the earliest exact observation of the target resources;
// it is not an assertion about when an external change system applied them.
type VerificationRun struct {
	ID                      string              `json:"id"`
	RecommendationID        string              `json:"recommendationId"`
	Status                  string              `json:"status"`
	ConfigurationObserved   bool                `json:"configurationObserved"`
	FirstObservedAt         *time.Time          `json:"firstObservedAt,omitempty"`
	ObservedGeneration      int64               `json:"observedGeneration,omitempty"`
	ObservedSpecFingerprint string              `json:"observedSpecFingerprint,omitempty"`
	PostChangeWindowSeconds int64               `json:"postChangeWindowSeconds,omitempty"`
	EvidenceComplete        bool                `json:"evidenceComplete"`
	Attribution             string              `json:"attribution"`
	Checks                  []VerificationCheck `json:"checks"`
	Reasons                 []string            `json:"reasons"`
	CheckedAt               time.Time           `json:"checkedAt"`
}

// VerificationOutcome is the latest durable outcome for a recommendation.
// RegressionNotifiedAt is persisted so several API replicas cannot emit the
// same recommendation.regressed lifecycle event after a restart.
type VerificationOutcome struct {
	VerificationRun
	RegressionNotifiedAt *time.Time `json:"-"`
}

type VerificationRunListResult struct {
	Items    []VerificationRun
	Page     int
	PageSize int
	Total    int
}

type VerificationOutcomeListResult struct {
	Items    []VerificationOutcome
	Page     int
	PageSize int
	Total    int
}

// VerificationRepository atomically appends evidence and replaces the latest
// outcome. Implementations must preserve the earliest FirstObservedAt and the
// immutable generation/spec fingerprint captured with that first observation.
type VerificationRepository interface {
	RecordVerification(ctx context.Context, run VerificationRun) (VerificationOutcome, error)
	GetVerificationOutcome(ctx context.Context, recommendationID string) (VerificationOutcome, error)
	ListVerificationOutcomes(ctx context.Context, page, pageSize int) (VerificationOutcomeListResult, error)
	ListVerificationRuns(ctx context.Context, recommendationID string, page, pageSize int) (VerificationRunListResult, error)
	ClaimRegressionNotification(ctx context.Context, recommendationID string, checkedAt, notifiedAt time.Time) (bool, error)
}

// NewVerificationRunID returns a random identifier in the durable vfy_ format.
func NewVerificationRunID() (string, error) { return newID("vfy_") }

func validVerificationStatus(status string) bool {
	switch status {
	case VerificationUnavailable, VerificationInconclusive, VerificationNotApplied, VerificationPending, VerificationVerified, VerificationRegressed:
		return true
	default:
		return false
	}
}

// ValidateVerificationRun rejects evidence that would make the durable state
// ambiguous or unsafe to aggregate.
func ValidateVerificationRun(run VerificationRun) error {
	if !verificationRecommendationID.MatchString(run.RecommendationID) {
		return errors.New("verification recommendation ID is invalid")
	}
	if run.ID != "" && !regexp.MustCompile(`^vfy_[0-9a-f]{32}$`).MatchString(run.ID) {
		return errors.New("verification run ID is invalid")
	}
	if !validVerificationStatus(run.Status) {
		return errors.New("verification status is invalid")
	}
	if run.CheckedAt.IsZero() {
		return errors.New("verification checkedAt is required")
	}
	if run.PostChangeWindowSeconds < 0 {
		return errors.New("verification post-change window cannot be negative")
	}
	if run.Attribution != VerificationAttributionNone && run.Attribution != VerificationAttributionExactPodUID {
		return errors.New("verification attribution is invalid")
	}
	if len(run.Checks) > maxVerificationChecks || len(run.Reasons) > maxVerificationReasons {
		return errors.New("verification evidence exceeds the supported item count")
	}
	for _, check := range run.Checks {
		if strings.TrimSpace(check.Name) == "" || len(check.Name) > 256 || !utf8.ValidString(check.Name) || len(check.Detail) > maxVerificationText || !utf8.ValidString(check.Detail) {
			return errors.New("verification check text is invalid")
		}
	}
	for _, reason := range run.Reasons {
		if strings.TrimSpace(reason) == "" || len(reason) > maxVerificationText || !utf8.ValidString(reason) {
			return errors.New("verification reason is invalid")
		}
	}
	if run.FirstObservedAt != nil && run.FirstObservedAt.IsZero() {
		return errors.New("verification first observation is invalid")
	}
	if run.ConfigurationObserved && run.FirstObservedAt == nil {
		return errors.New("observed target configuration requires firstObservedAt")
	}
	if run.FirstObservedAt != nil {
		if run.ObservedGeneration <= 0 || len(run.ObservedSpecFingerprint) != 64 {
			return errors.New("first observation requires generation and a SHA-256 spec fingerprint")
		}
	}
	if run.Status == VerificationVerified || run.Status == VerificationRegressed {
		if !run.ConfigurationObserved || run.FirstObservedAt == nil || !run.EvidenceComplete || run.Attribution != VerificationAttributionExactPodUID {
			return fmt.Errorf("%s verification requires exact, complete evidence for an observed target configuration", run.Status)
		}
	}
	return nil
}

func cloneVerificationChecks(checks []VerificationCheck) []VerificationCheck {
	cloned := slices.Clone(checks)
	for index := range cloned {
		if cloned[index].Before != nil {
			value := *cloned[index].Before
			cloned[index].Before = &value
		}
		if cloned[index].After != nil {
			value := *cloned[index].After
			cloned[index].After = &value
		}
	}
	return cloned
}

func cloneVerificationRun(run VerificationRun) VerificationRun {
	run.Checks = cloneVerificationChecks(run.Checks)
	run.Reasons = slices.Clone(run.Reasons)
	if run.FirstObservedAt != nil {
		value := run.FirstObservedAt.UTC()
		run.FirstObservedAt = &value
	}
	run.CheckedAt = run.CheckedAt.UTC()
	return run
}

func cloneVerificationOutcome(outcome VerificationOutcome) VerificationOutcome {
	outcome.VerificationRun = cloneVerificationRun(outcome.VerificationRun)
	if outcome.RegressionNotifiedAt != nil {
		value := outcome.RegressionNotifiedAt.UTC()
		outcome.RegressionNotifiedAt = &value
	}
	return outcome
}

