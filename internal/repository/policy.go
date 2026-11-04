package repository

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/kuberight/kuberight/internal/optimizer"
)

var (
	ErrPolicyConflict = errors.New("policy version conflict")
	ErrInvalidPolicy  = errors.New("invalid policy")
)

const (
	PolicyScopeGlobal    = "global"
	PolicyScopeNamespace = "namespace"
	PolicyScopeWorkload  = "workload"
	PolicyScopeContainer = "container"
)

// PolicyScope identifies where a policy applies. Empty cluster means every
// configured cluster. More-specific scopes inherit by replacement: container,
// workload, namespace, then global. Priority breaks ties at one specificity.
type PolicyScope struct {
	Level     string `json:"level"`
	Cluster   string `json:"cluster,omitempty"`
	Namespace string `json:"namespace,omitempty"`
	Kind      string `json:"kind,omitempty"`
	Workload  string `json:"workload,omitempty"`
	Container string `json:"container,omitempty"`
}

func (scope PolicyScope) Validate() error {
	fields := []string{scope.Cluster, scope.Namespace, scope.Kind, scope.Workload, scope.Container}
	for _, field := range fields {
		if len(field) > 253 || !utf8.ValidString(field) {
			return fmt.Errorf("%w: scope values must be valid UTF-8 of at most 253 bytes", ErrInvalidPolicy)
		}
	}
	switch scope.Level {
	case PolicyScopeGlobal:
		if scope.Namespace != "" || scope.Kind != "" || scope.Workload != "" || scope.Container != "" {
			return fmt.Errorf("%w: global scope cannot select a namespace or workload", ErrInvalidPolicy)
		}
	case PolicyScopeNamespace:
		if scope.Namespace == "" || scope.Kind != "" || scope.Workload != "" || scope.Container != "" {
			return fmt.Errorf("%w: namespace scope requires only namespace", ErrInvalidPolicy)
		}
	case PolicyScopeWorkload:
		if scope.Namespace == "" || scope.Kind == "" || scope.Workload == "" || scope.Container != "" {
			return fmt.Errorf("%w: workload scope requires namespace, kind, and workload", ErrInvalidPolicy)
		}
	case PolicyScopeContainer:
		if scope.Namespace == "" || scope.Kind == "" || scope.Workload == "" || scope.Container == "" {
			return fmt.Errorf("%w: container scope requires namespace, kind, workload, and container", ErrInvalidPolicy)
		}
	default:
		return fmt.Errorf("%w: scope level must be global, namespace, workload, or container", ErrInvalidPolicy)
	}
	return nil
}

func (scope PolicyScope) Specificity() int {
	switch scope.Level {
	case PolicyScopeContainer:
		return 4
	case PolicyScopeWorkload:
		return 3
	case PolicyScopeNamespace:
		return 2
	case PolicyScopeGlobal:
		return 1
	default:
		return 0
	}
}

// Matches reports whether a stored scope applies to the supplied subject.
func (scope PolicyScope) Matches(subject PolicyScope) bool {
	if scope.Cluster != "" && scope.Cluster != subject.Cluster {
		return false
	}
	if scope.Namespace != "" && scope.Namespace != subject.Namespace {
		return false
	}
	if scope.Kind != "" && !strings.EqualFold(scope.Kind, subject.Kind) {
		return false
	}
	if scope.Workload != "" && scope.Workload != subject.Workload {
		return false
	}
	return scope.Container == "" || scope.Container == subject.Container
}

// PolicyRecord is a versioned policy document. Version is used for optimistic
// concurrency so two administrators cannot silently overwrite each other.
type PolicyRecord struct {
	ID          string           `json:"id"`
	Name        string           `json:"name"`
	Description string           `json:"description,omitempty"`
	Scope       PolicyScope      `json:"scope"`
	Priority    int              `json:"priority"`
	Enabled     bool             `json:"enabled"`
	Policy      optimizer.Policy `json:"policy"`
	Version     int64            `json:"version"`
	CreatedAt   time.Time        `json:"createdAt"`
	CreatedBy   string           `json:"createdBy"`
	UpdatedAt   time.Time        `json:"updatedAt"`
	UpdatedBy   string           `json:"updatedBy"`
}

type PolicyMutation struct {
	Name        string
	Description string
	Scope       PolicyScope
	Priority    int
	Enabled     bool
	Policy      optimizer.Policy
	Actor       string
}

type PolicyListOptions struct {
	Page      int
	PageSize  int
	Cluster   string
	Namespace string
	Level     string
	Enabled   *bool
}

type PolicyListResult struct {
	Items    []PolicyRecord
	Page     int
	PageSize int
	Total    int
}

func (mutation PolicyMutation) Validate() error {
	if strings.TrimSpace(mutation.Name) == "" || len(mutation.Name) > 128 || !utf8.ValidString(mutation.Name) {
		return fmt.Errorf("%w: name must contain 1 to 128 bytes of valid UTF-8", ErrInvalidPolicy)
	}
	if len(mutation.Description) > 2048 || !utf8.ValidString(mutation.Description) {
		return fmt.Errorf("%w: description must contain at most 2048 bytes of valid UTF-8", ErrInvalidPolicy)
	}
	if len(mutation.Actor) == 0 || len(mutation.Actor) > maxActorLength {
		return fmt.Errorf("%w: actor is required", ErrInvalidPolicy)
	}
	if mutation.Priority < -1000 || mutation.Priority > 1000 {
		return fmt.Errorf("%w: priority must be between -1000 and 1000", ErrInvalidPolicy)
	}
	if err := mutation.Scope.Validate(); err != nil {
		return err
	}
	return validateOptimizerPolicy(mutation.Policy)
}

func validateOptimizerPolicy(policy optimizer.Policy) error {
	for _, value := range []float64{policy.CPUSafetyMargin, policy.MemorySafetyMargin, policy.LimitSafetyMargin, policy.MaximumReduction} {
		if math.IsNaN(value) || math.IsInf(value, 0) {
			return fmt.Errorf("%w: policy values must be finite", ErrInvalidPolicy)
		}
	}
	if policy.CPUSafetyMargin < 0 || policy.CPUSafetyMargin > 10 || policy.MemorySafetyMargin < 0 || policy.MemorySafetyMargin > 10 || policy.LimitSafetyMargin < 0 || policy.LimitSafetyMargin > 10 {
		return fmt.Errorf("%w: safety margins must be between 0 and 10", ErrInvalidPolicy)
	}
	if policy.MinimumCPUMilli <= 0 || policy.MinimumMemoryBytes <= 0 || policy.MaximumCPUMilli < policy.MinimumCPUMilli || policy.MaximumCPUMilli > 1_000_000_000 || policy.MaximumMemoryBytes < policy.MinimumMemoryBytes || policy.MaximumMemoryBytes > math.MaxInt64/4 {
		return fmt.Errorf("%w: resource bounds must be positive and maximums must not be below minimums", ErrInvalidPolicy)
	}
	if policy.MaximumReduction < 0 || policy.MaximumReduction > .5 {
		return fmt.Errorf("%w: maximumReduction must be between 0 and 0.5", ErrInvalidPolicy)
	}
	if policy.MinimumSamples < 2 || policy.MinimumSamples > 100000 || policy.HighConfidenceSamples < policy.MinimumSamples || policy.HighConfidenceSamples > 100000 {
		return fmt.Errorf("%w: sample thresholds are inconsistent or out of range", ErrInvalidPolicy)
	}
	for _, percentile := range []float64{policy.CPUPercentile, policy.MemoryPercentile} {
		if percentile != 0 && percentile != .9 && percentile != .95 && percentile != .99 && percentile != 1 {
			return fmt.Errorf("%w: percentiles must be 0.9, 0.95, 0.99, or 1", ErrInvalidPolicy)
		}
	}
	if policy.TrendHorizonDays < 0 || policy.TrendHorizonDays > 30 {
		return fmt.Errorf("%w: trendHorizonDays must be between 1 and 30 when set", ErrInvalidPolicy)
	}
	if policy.CPULimitStrategy != "" && policy.CPULimitStrategy != optimizer.CPULimitAdjust && policy.CPULimitStrategy != optimizer.CPULimitRemove {
		return fmt.Errorf("%w: cpuLimitStrategy must be adjust or remove", ErrInvalidPolicy)
	}
	return nil
}

func ValidatePolicyListOptions(options PolicyListOptions) error {
	if options.Page < 1 || options.PageSize < 1 || options.PageSize > 200 {
		return errors.New("page must be positive and pageSize must be between 1 and 200")
	}
	if options.Level != "" {
		switch options.Level {
		case PolicyScopeGlobal, PolicyScopeNamespace, PolicyScopeWorkload, PolicyScopeContainer:
		default:
			return errors.New("level must be global, namespace, workload, or container")
		}
	}
	return nil
}

type PolicyRepository interface {
	CreatePolicy(context.Context, PolicyMutation) (PolicyRecord, error)
	GetPolicy(context.Context, string) (PolicyRecord, error)
	ListPolicies(context.Context, PolicyListOptions) (PolicyListResult, error)
	UpdatePolicy(context.Context, string, int64, PolicyMutation) (PolicyRecord, error)
	DeletePolicy(context.Context, string, int64) error
	ResolvePolicy(context.Context, PolicyScope) (PolicyRecord, error)
}
