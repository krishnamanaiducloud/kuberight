package httpapi

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/http"
	"sort"
	"sync"
	"time"

	"github.com/kuberight/kuberight/internal/config"
	"github.com/kuberight/kuberight/internal/notify"
	"github.com/kuberight/kuberight/internal/repository"
	"github.com/kuberight/kuberight/internal/stats"
	"github.com/kuberight/kuberight/internal/usage"
)

// Verification statuses.
const (
	VerificationUnavailable = "unavailable"
	VerificationNotApplied  = "not_applied"
	VerificationPending     = "pending"
	VerificationVerified    = "verified"
	VerificationRegressed   = "regressed"
)

const (
	// minimumPostChange is how much post-change history a verdict needs.
	minimumPostChange = time.Hour
	// appliedTolerance accepts live values within 2% of the approved ones,
	// because quantities are often rewritten in a different unit.
	appliedTolerance        = 0.02
	maxVerificationsPerPass = 200
)

// VerificationCheck compares one signal before and after the change.
type VerificationCheck struct {
	Name      string   `json:"name"`
	Before    *float64 `json:"before,omitempty"`
	After     *float64 `json:"after,omitempty"`
	Regressed bool     `json:"regressed"`
	Detail    string   `json:"detail"`
}

// Verification is the post-change verdict for an approved recommendation.
type Verification struct {
	RecommendationID string              `json:"recommendationId"`
	Status           string              `json:"status"`
	Applied          bool                `json:"applied"`
	AppliedAt        *time.Time          `json:"appliedAt,omitempty"`
	AppliedAtSource  string              `json:"appliedAtSource,omitempty"`
	PostChangeWindow string              `json:"postChangeWindow,omitempty"`
	Checks           []VerificationCheck `json:"checks"`
	Reasons          []string            `json:"reasons"`
	CheckedAt        time.Time           `json:"checkedAt"`
}

// VerificationState holds the latest verdict per recommendation for /metrics
// and remembers which regressions were already announced.
type VerificationState struct {
	mu        sync.Mutex
	latest    map[string]Verification
	announced map[string]bool
}

// NewVerificationState returns an empty state.
func NewVerificationState() *VerificationState {
	return &VerificationState{latest: map[string]Verification{}, announced: map[string]bool{}}
}

// record stores a verdict and reports whether it is a regression that has
// not been announced yet.
func (state *VerificationState) record(result Verification) bool {
	state.mu.Lock()
	defer state.mu.Unlock()
	state.latest[result.RecommendationID] = result
	if result.Status != VerificationRegressed {
		return false
	}
	if state.announced[result.RecommendationID] {
		return false
	}
	state.announced[result.RecommendationID] = true
	return true
}

func (state *VerificationState) render(w http.ResponseWriter) {
	state.mu.Lock()
	counts := map[string]int{}
	for _, result := range state.latest {
		counts[result.Status]++
	}
	state.mu.Unlock()
	_, _ = fmt.Fprintln(w, "# HELP kuberight_recommendation_verifications Approved recommendations by post-change verification status.")
	_, _ = fmt.Fprintln(w, "# TYPE kuberight_recommendation_verifications gauge")
	for _, status := range []string{VerificationVerified, VerificationRegressed, VerificationPending, VerificationNotApplied, VerificationUnavailable} {
		_, _ = fmt.Fprintf(w, "kuberight_recommendation_verifications{status=%q} %d\n", status, counts[status])
	}
	_, _ = fmt.Fprintln(w, "# HELP kuberight_recommendation_regressions Applied recommendations followed by a regression.")
	_, _ = fmt.Fprintln(w, "# TYPE kuberight_recommendation_regressions gauge")
	_, _ = fmt.Fprintf(w, "kuberight_recommendation_regressions %d\n", counts[VerificationRegressed])
}

func (server *apiServer) recommendationVerification(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !validRecommendationID(id) {
		respondError(w, r, http.StatusBadRequest, "invalid_id", "recommendation identifier is invalid")
		return
	}
	record, err := server.repository.GetRecommendation(r.Context(), id)
	if errors.Is(err, repository.ErrNotFound) {
		respondError(w, r, http.StatusNotFound, "not_found", "recommendation was not found")
		return
	}
	if err != nil {
		server.repositoryFailure(w, r, "get recommendation", err)
		return
	}
	if !server.authorize(w, r, AccessAttributes{Verb: "get", Resource: "recommendations", Namespace: record.Namespace, Name: id}) {
		return
	}
	if record.ReviewStatus != repository.ReviewApproved {
		respondError(w, r, http.StatusConflict, "not_approved", "only approved recommendations are verified after the change")
		return
	}
	result := server.verify(r.Context(), record)
	if server.verified.record(result) {
		server.announceRegression(record, result)
	}
	respondJSON(w, http.StatusOK, result)
}

func (server *apiServer) announceRegression(record repository.RecommendationRecord, result Verification) {
	server.logger.Warn("applied recommendation regressed", "recommendation_id", record.ID, "namespace", record.Namespace, "workload", record.Workload, "container", record.Container, "reasons", result.Reasons)
	server.notify(notify.RecommendationRegressed, record, "kuberight", joinLimited(result.Reasons, 3))
}

func joinLimited(values []string, limit int) string {
	if len(values) > limit {
		values = append(values[:limit:limit], fmt.Sprintf("%d more", len(values)-limit))
	}
	text := ""
	for index, value := range values {
		if index > 0 {
			text += "; "
		}
		text += value
	}
	return text
}

// verify decides whether the approved values are live and, once enough
// post-change history exists, compares it with the history the
// recommendation was based on.
func (server *apiServer) verify(ctx context.Context, record repository.RecommendationRecord) Verification {
	result := Verification{RecommendationID: record.ID, Checks: []VerificationCheck{}, Reasons: []string{}, CheckedAt: server.now().UTC()}
	resolver, ok := server.provider.(WorkloadResolver)
	if server.usage == nil || server.provider == nil || !ok {
		result.Status = VerificationUnavailable
		result.Reasons = append(result.Reasons, "verification needs both Prometheus and the Kubernetes inventory")
		return result
	}
	spec, err := resolver.Workload(ctx, record.Namespace, record.Kind, record.Workload)
	if err != nil {
		result.Status = VerificationUnavailable
		result.Reasons = append(result.Reasons, "the live workload could not be read")
		return result
	}
	live, found := spec.Container(record.Container)
	if !found {
		result.Status = VerificationUnavailable
		result.Reasons = append(result.Reasons, "the container no longer exists in the workload")
		return result
	}
	if !matches(live.CPURequestMilli, record.CPURequestMilli) || !matches(live.MemoryRequestBytes, record.MemoryRequestBytes) ||
		!matches(live.CPULimitMilli, record.CPULimitMilli) || !matches(live.MemoryLimitBytes, record.MemoryLimitBytes) {
		result.Status = VerificationNotApplied
		result.Reasons = append(result.Reasons, "the live requests and limits do not match the approved values yet")
		return result
	}
	result.Applied = true

	appliedAt, source := server.appliedAt(ctx, record)
	result.AppliedAt, result.AppliedAtSource = &appliedAt, source
	now := server.now().UTC().Truncate(time.Second)
	post := now.Sub(appliedAt)
	result.PostChangeWindow = post.Truncate(time.Minute).String()
	if post < minimumPostChange {
		result.Status = VerificationPending
		result.Reasons = append(result.Reasons, fmt.Sprintf("only %s of post-change history exists; %s is needed", post.Truncate(time.Minute), minimumPostChange))
		return result
	}
	subject := usage.Subject{Namespace: record.Namespace, Kind: record.Kind, Workload: record.Workload, Container: record.Container}
	if err := subject.Validate(); err != nil {
		result.Status = VerificationUnavailable
		result.Reasons = append(result.Reasons, "the recommendation's workload identity is invalid")
		return result
	}
	window := min(post, 7*24*time.Hour)
	step := chartStep(window, server.config.RecommendationStep)
	before := usage.Window{Duration: window, Step: step, End: appliedAt}
	if end := record.Provenance.ObservationEnd; end != nil && end.Before(appliedAt) {
		before.End = *end
	}
	after := usage.Window{Duration: window, Step: step, End: now}
	afterSignals, err := server.usage.Signals(ctx, subject, after, usage.LatencyTemplate(server.config.LatencyQuery))
	if err != nil {
		result.Status = VerificationUnavailable
		result.Reasons = append(result.Reasons, "post-change usage could not be read")
		return result
	}
	beforeSignals, beforeErr := server.usage.Signals(ctx, subject, before, usage.LatencyTemplate(server.config.LatencyQuery))
	if beforeErr != nil {
		beforeSignals = usage.Signals{}
	}
	result.Checks = compareSignals(record, beforeSignals, afterSignals, window)
	for _, check := range result.Checks {
		if check.Regressed {
			result.Reasons = append(result.Reasons, check.Detail)
		}
	}
	if len(result.Reasons) > 0 {
		result.Status = VerificationRegressed
	} else {
		result.Status = VerificationVerified
	}
	return result
}

func matches(live, approved int64) bool {
	if approved == 0 || live == 0 {
		return approved == live
	}
	return math.Abs(float64(live-approved)) <= float64(approved)*appliedTolerance
}

// appliedAt estimates when the approved values went live: the first rollout
// after the review for Deployments, otherwise the review time itself, which
// is the earliest the change can have been made.
func (server *apiServer) appliedAt(ctx context.Context, record repository.RecommendationRecord) (time.Time, string) {
	reviewed := record.CreatedAt
	if record.ReviewedAt != nil {
		reviewed = *record.ReviewedAt
	}
	reviewed = reviewed.UTC().Truncate(time.Second)
	subject := usage.Subject{Namespace: record.Namespace, Kind: record.Kind, Workload: record.Workload, Container: record.Container}
	if record.Kind == "Deployment" && subject.Validate() == nil {
		if rollouts, err := server.usage.Rollouts(ctx, subject, reviewed, server.now()); err == nil && len(rollouts) > 0 {
			sort.Slice(rollouts, func(left, right int) bool { return rollouts[left].Timestamp.Before(rollouts[right].Timestamp) })
			return rollouts[0].Timestamp.UTC(), "first rollout after the approval"
		}
	}
	return reviewed, "approval time (the change cannot be older)"
}

func ratioPointer(value float64) *float64 {
	rounded := math.Round(value*10_000) / 10_000
	return &rounded
}

func eventCount(signals usage.Signals, kind string) int64 {
	total := int64(0)
	for _, event := range signals.Events {
		if event.Kind == kind {
			total += max(1, event.Count)
		}
	}
	return total
}

// compareSignals applies the regression rules: any OOM kill after a memory
// change, throttling that rose by five points or more, more restarts per
// hour, and latency 25% higher.
func compareSignals(record repository.RecommendationRecord, before, after usage.Signals, window time.Duration) []VerificationCheck {
	checks := []VerificationCheck{}
	hours := math.Max(window.Hours(), 1)

	oomBefore, oomAfter := eventCount(before, usage.EventOOMKill), eventCount(after, usage.EventOOMKill)
	checks = append(checks, VerificationCheck{
		Name: "OOM kills", Before: ratioPointer(float64(oomBefore)), After: ratioPointer(float64(oomAfter)),
		Regressed: oomAfter > 0 && oomAfter >= oomBefore,
		Detail:    fmt.Sprintf("%d OOM kill(s) after the change, %d before", oomAfter, oomBefore),
	})

	restartsBefore, restartsAfter := float64(eventCount(before, usage.EventRestart))/hours, float64(eventCount(after, usage.EventRestart))/hours
	checks = append(checks, VerificationCheck{
		Name: "Restarts per hour", Before: ratioPointer(restartsBefore), After: ratioPointer(restartsAfter),
		Regressed: restartsAfter > 0 && restartsAfter >= 2*math.Max(restartsBefore, 1/hours),
		Detail:    fmt.Sprintf("restarts per hour went from %.2f to %.2f", restartsBefore, restartsAfter),
	})

	if after.Throttling != nil {
		throttlingAfter, _ := stats.Mean(after.Throttling.Values())
		throttlingBefore := 0.0
		if before.Throttling != nil {
			throttlingBefore, _ = stats.Mean(before.Throttling.Values())
		}
		checks = append(checks, VerificationCheck{
			Name: "CPU throttling", Before: ratioPointer(throttlingBefore), After: ratioPointer(throttlingAfter),
			Regressed: throttlingAfter >= 0.05 && throttlingAfter-throttlingBefore >= 0.05,
			Detail:    fmt.Sprintf("throttled periods went from %.1f%% to %.1f%%", throttlingBefore*100, throttlingAfter*100),
		})
	}

	if after.Latency != nil && before.Latency != nil {
		latencyBefore, beforeCount := stats.Mean(before.Latency.Values())
		latencyAfter, afterCount := stats.Mean(after.Latency.Values())
		if beforeCount > 0 && afterCount > 0 && latencyBefore > 0 {
			change := (latencyAfter - latencyBefore) / latencyBefore
			checks = append(checks, VerificationCheck{
				Name: "Latency", Before: ratioPointer(latencyBefore), After: ratioPointer(latencyAfter),
				Regressed: change >= 0.25,
				Detail:    fmt.Sprintf("mean latency changed %+.0f%%", change*100),
			})
		}
	}

	if record.MemoryRequestBytes > 0 {
		share := stats.Quantile(after.Memory.Values(), 1)
		checks = append(checks, VerificationCheck{
			Name: "Peak memory vs request", After: ratioPointer(share / float64(record.MemoryRequestBytes)),
			Detail: fmt.Sprintf("peak memory reached %.0f%% of the new request", share/float64(record.MemoryRequestBytes)*100),
		})
	}
	return checks
}

// Verifier periodically verifies approved recommendations, updates the
// shared state on /metrics, and announces each regression once.
type Verifier struct {
	server *apiServer
}

// NewVerifier builds a verifier from the API's dependencies. Share
// dependencies.Verification with the handler so results reach /metrics.
func NewVerifier(cfg config.Config, dependencies Dependencies) (*Verifier, error) {
	server, err := newAPIServer(cfg, dependencies)
	if err != nil {
		return nil, err
	}
	return &Verifier{server: server}, nil
}

// Run verifies every interval until the context ends.
func (verifier *Verifier) Run(ctx context.Context) {
	interval := verifier.server.config.VerificationInterval
	if interval <= 0 {
		return
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		verifier.Pass(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// Pass verifies the newest approved recommendation of each container once.
func (verifier *Verifier) Pass(ctx context.Context) int {
	server := verifier.server
	checked := 0
	for page := 1; checked < maxVerificationsPerPass; page++ {
		result, err := server.repository.ListRecommendations(ctx, repository.ListOptions{
			Page: page, PageSize: 100, ReviewStatus: repository.ReviewApproved, LatestOnly: true, Sort: "createdAt", Order: "desc",
		})
		if err != nil {
			server.logger.Warn("verification could not list approved recommendations", "error", err)
			return checked
		}
		for _, record := range result.Items {
			if ctx.Err() != nil || checked >= maxVerificationsPerPass {
				return checked
			}
			verdict := server.verify(ctx, record)
			if server.verified.record(verdict) {
				server.announceRegression(record, verdict)
			}
			checked++
		}
		if len(result.Items) < 100 {
			return checked
		}
	}
	return checked
}
