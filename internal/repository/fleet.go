package repository

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"
)

var (
	ErrInvalidFleetSchedule = errors.New("invalid fleet schedule")
	ErrFleetConflict        = errors.New("fleet record version conflict")
	ErrFleetScanConflict    = errors.New("fleet scan state conflict")
)

const (
	FleetScanQueued    = "queued"
	FleetScanRunning   = "running"
	FleetScanCompleted = "completed"
	FleetScanFailed    = "failed"
	FleetScanCanceled  = "canceled"

	// FleetScanLeaseDuration is shared by workers and repository heartbeat
	// updates so a heartbeat never silently changes the claim lease policy.
	FleetScanLeaseDuration = 5 * time.Minute
	// FleetScanRetryDelay is deliberately bounded and deterministic so a
	// transient executor failure is retried without creating a hot queue loop.
	FleetScanRetryDelay = 30 * time.Second
	// MaxFleetFailureDetails bounds persisted per-target diagnostics. Aggregate
	// counters remain exact even when a very large scan has more failures.
	MaxFleetFailureDetails = 25
)

// FleetScope restricts discovery. Empty selectors mean every visible object.
type FleetScope struct {
	Cluster    string   `json:"cluster,omitempty"`
	Namespaces []string `json:"namespaces,omitempty"`
	Kinds      []string `json:"kinds,omitempty"`
	Filter     string   `json:"filter,omitempty"`
}

func (scope FleetScope) Validate() error {
	if len(scope.Cluster) > 253 || len(scope.Filter) > 512 || !utf8.ValidString(scope.Cluster) || !utf8.ValidString(scope.Filter) {
		return fmt.Errorf("%w: scope selectors are too long or invalid UTF-8", ErrInvalidFleetSchedule)
	}
	if len(scope.Namespaces) > 1000 || len(scope.Kinds) > 32 {
		return fmt.Errorf("%w: scope contains too many selectors", ErrInvalidFleetSchedule)
	}
	for _, values := range [][]string{scope.Namespaces, scope.Kinds} {
		seen := map[string]struct{}{}
		for _, value := range values {
			if strings.TrimSpace(value) == "" || len(value) > 253 || !utf8.ValidString(value) {
				return fmt.Errorf("%w: scope selectors must be non-empty valid UTF-8", ErrInvalidFleetSchedule)
			}
			key := strings.ToLower(value)
			if _, duplicate := seen[key]; duplicate {
				return fmt.Errorf("%w: scope selectors must not contain duplicates", ErrInvalidFleetSchedule)
			}
			seen[key] = struct{}{}
		}
	}
	return nil
}

type FleetSchedule struct {
	ID              string     `json:"id"`
	Name            string     `json:"name"`
	Enabled         bool       `json:"enabled"`
	IntervalSeconds int64      `json:"intervalSeconds"`
	Scope           FleetScope `json:"scope"`
	PolicyID        string     `json:"policyId,omitempty"`
	WindowSeconds   int64      `json:"windowSeconds"`
	NextRunAt       time.Time  `json:"nextRunAt"`
	LastRunAt       *time.Time `json:"lastRunAt,omitempty"`
	Version         int64      `json:"version"`
	CreatedAt       time.Time  `json:"createdAt"`
	CreatedBy       string     `json:"createdBy"`
	UpdatedAt       time.Time  `json:"updatedAt"`
	UpdatedBy       string     `json:"updatedBy"`
}

type FleetScheduleMutation struct {
	Name            string
	Enabled         bool
	IntervalSeconds int64
	Scope           FleetScope
	PolicyID        string
	WindowSeconds   int64
	NextRunAt       time.Time
	Actor           string
}

func (mutation FleetScheduleMutation) Validate() error {
	if strings.TrimSpace(mutation.Name) == "" || len(mutation.Name) > 128 || !utf8.ValidString(mutation.Name) {
		return fmt.Errorf("%w: name must contain 1 to 128 bytes of valid UTF-8", ErrInvalidFleetSchedule)
	}
	if mutation.IntervalSeconds < 300 || mutation.IntervalSeconds > int64(30*24*time.Hour/time.Second) {
		return fmt.Errorf("%w: intervalSeconds must be between 300 and 2592000", ErrInvalidFleetSchedule)
	}
	if mutation.WindowSeconds < 3600 || mutation.WindowSeconds > int64(30*24*time.Hour/time.Second) {
		return fmt.Errorf("%w: windowSeconds must be between 3600 and 2592000", ErrInvalidFleetSchedule)
	}
	if mutation.NextRunAt.IsZero() || len(mutation.Actor) == 0 || len(mutation.Actor) > maxActorLength {
		return fmt.Errorf("%w: nextRunAt and actor are required", ErrInvalidFleetSchedule)
	}
	if mutation.PolicyID != "" && (len(mutation.PolicyID) > 64 || !strings.HasPrefix(mutation.PolicyID, "pol_")) {
		return fmt.Errorf("%w: policyId is invalid", ErrInvalidFleetSchedule)
	}
	return mutation.Scope.Validate()
}

type FleetScan struct {
	ID                 string               `json:"id"`
	ScheduleID         string               `json:"scheduleId,omitempty"`
	ScheduledFor       time.Time            `json:"scheduledFor"`
	AvailableAt        time.Time            `json:"availableAt"`
	Scope              FleetScope           `json:"scope"`
	PolicyID           string               `json:"policyId,omitempty"`
	WindowSeconds      int64                `json:"windowSeconds"`
	Status             string               `json:"status"`
	Expected           int                  `json:"expected"`
	Processed          int                  `json:"processed"`
	Succeeded          int                  `json:"succeeded"`
	Failed             int                  `json:"failed"`
	Attempt            int                  `json:"attempt"`
	MaxAttempts        int                  `json:"maxAttempts"`
	LeaseOwner         string               `json:"leaseOwner,omitempty"`
	LeaseExpiresAt     *time.Time           `json:"leaseExpiresAt,omitempty"`
	LastError          string               `json:"lastError,omitempty"`
	CreatedAt          time.Time            `json:"createdAt"`
	CreatedBy          string               `json:"createdBy"`
	StartedAt          *time.Time           `json:"startedAt,omitempty"`
	CompletedAt        *time.Time           `json:"completedAt,omitempty"`
	CancellationAt     *time.Time           `json:"cancellationAt,omitempty"`
	FailureDetails     []FleetTargetFailure `json:"failureDetails,omitempty"`
	IdempotencyHash    string               `json:"-"`
	RequestFingerprint string               `json:"-"`
}

type FleetScanRequest struct {
	Scope          FleetScope
	PolicyID       string
	WindowSeconds  int64
	ScheduledFor   time.Time
	CreatedBy      string
	IdempotencyKey string
	MaxAttempts    int
}

func (request FleetScanRequest) Validate() error {
	if err := request.Scope.Validate(); err != nil {
		return err
	}
	if request.WindowSeconds < 3600 || request.WindowSeconds > int64(30*24*time.Hour/time.Second) || request.ScheduledFor.IsZero() {
		return fmt.Errorf("%w: scan window or scheduled time is invalid", ErrInvalidFleetSchedule)
	}
	if request.CreatedBy == "" || len(request.CreatedBy) > maxActorLength || request.MaxAttempts < 1 || request.MaxAttempts > 10 {
		return fmt.Errorf("%w: scan actor or retry limit is invalid", ErrInvalidFleetSchedule)
	}
	if request.IdempotencyKey != "" && !validIdempotencyKey(request.IdempotencyKey) {
		return fmt.Errorf("%w: idempotency key is invalid", ErrInvalidFleetSchedule)
	}
	if request.PolicyID != "" && (len(request.PolicyID) > 64 || !strings.HasPrefix(request.PolicyID, "pol_")) {
		return fmt.Errorf("%w: policyId is invalid", ErrInvalidFleetSchedule)
	}
	return nil
}

func FleetIdempotencyHash(actor, key string) string {
	if key == "" {
		return ""
	}
	digest := sha256.Sum256([]byte(actor + "\x00" + key))
	return hex.EncodeToString(digest[:])
}

func FleetRequestFingerprint(request FleetScanRequest) string {
	// ScheduledFor is assigned by the server, not supplied by the caller. It
	// must not participate in HTTP idempotency or a network retry a few
	// milliseconds later would conflict with the original request.
	payload, _ := json.Marshal(struct {
		Scope         FleetScope `json:"scope"`
		PolicyID      string     `json:"policyId"`
		WindowSeconds int64      `json:"windowSeconds"`
		MaxAttempts   int        `json:"maxAttempts"`
	}{request.Scope, request.PolicyID, request.WindowSeconds, request.MaxAttempts})
	digest := sha256.Sum256(payload)
	return hex.EncodeToString(digest[:])
}

type FleetListOptions struct {
	Page       int
	PageSize   int
	Status     string
	ScheduleID string
}

type FleetScheduleListResult struct {
	Items                 []FleetSchedule
	Page, PageSize, Total int
}
type FleetScanListResult struct {
	Items                 []FleetScan
	Page, PageSize, Total int
}

type FleetScanProgress struct {
	Expected       int
	Processed      int
	Succeeded      int
	Failed         int
	Status         string
	Error          string
	FailureDetails []FleetTargetFailure
}

// FleetTargetFailure is a bounded, operator-visible diagnostic for a failed
// workload discovery or container analysis. It intentionally excludes metric
// payloads and credentials.
type FleetTargetFailure struct {
	Cluster   string `json:"cluster,omitempty"`
	Namespace string `json:"namespace,omitempty"`
	Kind      string `json:"kind,omitempty"`
	Workload  string `json:"workload,omitempty"`
	Container string `json:"container,omitempty"`
	Stage     string `json:"stage"`
	Error     string `json:"error"`
}

func ValidateFleetScanProgress(progress FleetScanProgress) error {
	if progress.Expected < 0 || progress.Processed < 0 || progress.Succeeded < 0 || progress.Failed < 0 ||
		progress.Processed > progress.Expected || progress.Succeeded+progress.Failed > progress.Processed {
		return ErrFleetScanConflict
	}
	if progress.Status != "" && progress.Status != FleetScanCompleted && progress.Status != FleetScanFailed {
		return ErrFleetScanConflict
	}
	if progress.Status == FleetScanCompleted && (progress.Processed != progress.Expected || progress.Succeeded+progress.Failed != progress.Processed) {
		return ErrFleetScanConflict
	}
	if progress.Status == FleetScanCompleted && progress.Error != "" {
		return ErrFleetScanConflict
	}
	if progress.Status == FleetScanFailed && strings.TrimSpace(progress.Error) == "" {
		return ErrFleetScanConflict
	}
	if len(progress.Error) > 4096 || !utf8.ValidString(progress.Error) || len(progress.FailureDetails) > MaxFleetFailureDetails {
		return ErrFleetScanConflict
	}
	for _, detail := range progress.FailureDetails {
		fields := []string{detail.Cluster, detail.Namespace, detail.Kind, detail.Workload, detail.Container}
		for _, field := range fields {
			if len(field) > 253 || !utf8.ValidString(field) {
				return ErrFleetScanConflict
			}
		}
		if strings.TrimSpace(detail.Stage) == "" || len(detail.Stage) > 64 || !utf8.ValidString(detail.Stage) ||
			strings.TrimSpace(detail.Error) == "" || len(detail.Error) > 1024 || !utf8.ValidString(detail.Error) {
			return ErrFleetScanConflict
		}
	}
	return nil
}

type FleetRepository interface {
	CreateFleetSchedule(context.Context, FleetScheduleMutation) (FleetSchedule, error)
	GetFleetSchedule(context.Context, string) (FleetSchedule, error)
	ListFleetSchedules(context.Context, FleetListOptions) (FleetScheduleListResult, error)
	UpdateFleetSchedule(context.Context, string, int64, FleetScheduleMutation) (FleetSchedule, error)
	DeleteFleetSchedule(context.Context, string, int64) error
	CreateFleetScan(context.Context, FleetScanRequest) (FleetScan, bool, error)
	GetFleetScan(context.Context, string) (FleetScan, error)
	ListFleetScans(context.Context, FleetListOptions) (FleetScanListResult, error)
	CancelFleetScan(context.Context, string, time.Time) (FleetScan, error)
	ResumeFleetScan(context.Context, string, time.Time) (FleetScan, error)
	EnqueueDueFleetScans(context.Context, time.Time, int) ([]FleetScan, error)
	ClaimFleetScan(context.Context, string, time.Time, time.Duration) (FleetScan, error)
	UpdateFleetScan(context.Context, string, string, time.Time, FleetScanProgress) (FleetScan, error)
}
