package httpapi

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"net/http"
	"sort"
	"time"

	"github.com/kuberight/kuberight/internal/config"
	"github.com/kuberight/kuberight/internal/notify"
	"github.com/kuberight/kuberight/internal/repository"
	"github.com/kuberight/kuberight/internal/usage"
)

// Verification statuses are aliases of the durable repository contract.
const (
	VerificationUnavailable  = repository.VerificationUnavailable
	VerificationInconclusive = repository.VerificationInconclusive
	VerificationNotApplied   = repository.VerificationNotApplied
	VerificationPending      = repository.VerificationPending
	VerificationVerified     = repository.VerificationVerified
	VerificationRegressed    = repository.VerificationRegressed
)

const (
	minimumPostChange       = time.Hour
	maximumPostChange       = 7 * 24 * time.Hour
	appliedTolerance        = 0.02
	maxVerificationsPerPass = 200
)

// VerificationCheck and Verification keep the public API tied to exactly the
// same evidence shape that is persisted. There is no process-local verdict.
type VerificationCheck = repository.VerificationCheck
type Verification = repository.VerificationOutcome

func (server *apiServer) recommendationForVerification(w http.ResponseWriter, r *http.Request, verb string) (repository.RecommendationRecord, bool) {
	id := r.PathValue("id")
	if !validRecommendationID(id) {
		respondError(w, r, http.StatusBadRequest, "invalid_id", "recommendation identifier is invalid")
		return repository.RecommendationRecord{}, false
	}
	record, err := server.repository.GetRecommendation(r.Context(), id)
	if errors.Is(err, repository.ErrNotFound) {
		respondError(w, r, http.StatusNotFound, "not_found", "recommendation was not found")
		return repository.RecommendationRecord{}, false
	}
	if err != nil {
		server.repositoryFailure(w, r, "get recommendation", err)
		return repository.RecommendationRecord{}, false
	}
	if !server.authorize(w, r, AccessAttributes{Verb: verb, Resource: "recommendations", Namespace: record.Namespace, Name: id}) {
		return repository.RecommendationRecord{}, false
	}
	if record.ReviewStatus != repository.ReviewApproved {
		respondError(w, r, http.StatusConflict, "not_approved", "only approved recommendations are verified after the change")
		return repository.RecommendationRecord{}, false
	}
	return record, true
}

// recommendationVerification is deliberately read-only. A missing outcome
// means no durable verification has run yet; GET never queries dependencies,
// appends evidence, or emits a webhook.
func (server *apiServer) recommendationVerification(w http.ResponseWriter, r *http.Request) {
	record, ok := server.recommendationForVerification(w, r, "get")
	if !ok {
		return
	}
	result, err := server.repository.GetVerificationOutcome(r.Context(), record.ID)
	if errors.Is(err, repository.ErrNotFound) {
		respondError(w, r, http.StatusNotFound, "verification_not_checked", "no durable verification outcome exists; request an explicit refresh")
		return
	}
	if err != nil {
		server.repositoryFailure(w, r, "get recommendation verification", err)
		return
	}
	respondJSON(w, http.StatusOK, result)
}

// refreshRecommendationVerification performs and durably records one
// verification attempt. Notifications occur only after the outcome is stored
// and its regression notification is atomically claimed.
func (server *apiServer) refreshRecommendationVerification(w http.ResponseWriter, r *http.Request) {
	record, ok := server.recommendationForVerification(w, r, "update")
	if !ok {
		return
	}
	result, err := server.refreshVerification(r.Context(), record)
	if err != nil {
		server.repositoryFailure(w, r, "refresh recommendation verification", err)
		return
	}
	respondJSON(w, http.StatusOK, result)
}

func (server *apiServer) refreshVerification(ctx context.Context, record repository.RecommendationRecord) (repository.VerificationOutcome, error) {
	var previous *repository.VerificationOutcome
	stored, err := server.repository.GetVerificationOutcome(ctx, record.ID)
	if err == nil {
		previous = &stored
	} else if !errors.Is(err, repository.ErrNotFound) {
		return repository.VerificationOutcome{}, err
	}
	run := server.verify(ctx, record, previous)
	result, err := server.repository.RecordVerification(ctx, run)
	if err != nil {
		return repository.VerificationOutcome{}, err
	}
	if result.Status == VerificationRegressed {
		claimed, claimErr := server.repository.ClaimRegressionNotification(ctx, record.ID, result.CheckedAt, server.now().UTC())
		if claimErr != nil {
			server.logger.Error("verification regression notification claim failed", "recommendation_id", record.ID, "error", claimErr)
		} else if claimed {
			server.announceRegression(record, result)
		}
	}
	return result, nil
}

func (server *apiServer) announceRegression(record repository.RecommendationRecord, result repository.VerificationOutcome) {
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

func validSHA256(value string) bool {
	if len(value) != 64 {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func preserveFirstObservation(run *repository.VerificationRun, previous *repository.VerificationOutcome) {
	if previous == nil || previous.FirstObservedAt == nil {
		return
	}
	first := previous.FirstObservedAt.UTC()
	run.FirstObservedAt = &first
	run.ObservedGeneration = previous.ObservedGeneration
	run.ObservedSpecFingerprint = previous.ObservedSpecFingerprint
}

func (server *apiServer) inconclusive(run *repository.VerificationRun, reason string) repository.VerificationRun {
	run.Status = VerificationInconclusive
	run.Reasons = append(run.Reasons, reason)
	return *run
}

// verify computes one fail-closed run. A verified/regressed verdict requires
// immutable workload provenance, an unchanged first-observed target spec, and
// complete Prometheus evidence attributed to exact pod UIDs.
func (server *apiServer) verify(ctx context.Context, record repository.RecommendationRecord, previous *repository.VerificationOutcome) repository.VerificationRun {
	now := server.now().UTC().Truncate(time.Second)
	run := repository.VerificationRun{
		RecommendationID: record.ID,
		Status:           VerificationUnavailable,
		Attribution:      repository.VerificationAttributionNone,
		Checks:           []repository.VerificationCheck{},
		Reasons:          []string{},
		CheckedAt:        now,
	}
	preserveFirstObservation(&run, previous)

	if record.Source != repository.SourcePrometheus {
		return server.inconclusive(&run, "only Prometheus-backed recommendations have independently verifiable evidence")
	}
	provenance := record.Provenance
	if provenance.ExpiresAt == nil || provenance.WorkloadUID == "" || provenance.WorkloadGeneration <= 0 ||
		!validSHA256(provenance.SourceSpecFingerprint) || !validSHA256(provenance.QueryFingerprint) ||
		provenance.ObservationStart == nil || provenance.ObservationEnd == nil {
		return server.inconclusive(&run, "the recommendation lacks complete immutable workload and observation provenance")
	}
	if record.DataQuality.ReplicaCoverage == nil || !record.DataQuality.ReplicaCoverage.Complete {
		return server.inconclusive(&run, "the recommendation was not calculated from complete exact-replica evidence")
	}
	resolver, ok := server.provider.(WorkloadResolver)
	exactSource, exact := server.usage.(attributedUsageSource)
	if server.provider == nil || !ok || server.usage == nil || !exact {
		run.Status = VerificationUnavailable
		run.Reasons = append(run.Reasons, "verification requires Kubernetes inventory and exact pod-UID Prometheus attribution")
		return run
	}
	spec, err := resolver.Workload(ctx, record.Namespace, record.Kind, record.Workload)
	if err != nil {
		run.Status = VerificationUnavailable
		run.Reasons = append(run.Reasons, "the live workload could not be read")
		return run
	}
	if spec.Cluster != record.Cluster || spec.UID != provenance.WorkloadUID {
		return server.inconclusive(&run, "the workload was recreated or moved; its immutable identity no longer matches the recommendation")
	}
	if spec.Generation < provenance.WorkloadGeneration || !validSHA256(spec.SourceSpecFingerprint) {
		return server.inconclusive(&run, "the live workload generation or spec fingerprint is invalid")
	}
	if spec.Generation == provenance.WorkloadGeneration && spec.SourceSpecFingerprint != provenance.SourceSpecFingerprint {
		return server.inconclusive(&run, "the live spec changed without a matching Kubernetes generation")
	}
	live, found := spec.Container(record.Container)
	if !found {
		return server.inconclusive(&run, "the container no longer exists in the workload")
	}
	if !matches(live.CPURequestMilli, record.CPURequestMilli) || !matches(live.MemoryRequestBytes, record.MemoryRequestBytes) ||
		!matches(live.CPULimitMilli, record.CPULimitMilli) || !matches(live.MemoryLimitBytes, record.MemoryLimitBytes) {
		run.Status = VerificationNotApplied
		run.Reasons = append(run.Reasons, "the live requests and limits do not match the approved target values")
		return run
	}

	targetChanged := record.CPURequestMilli != record.CurrentCPURequestMilli || record.MemoryRequestBytes != record.CurrentMemoryRequestBytes ||
		record.CPULimitMilli != record.CurrentCPULimitMilli || record.MemoryLimitBytes != record.CurrentMemoryLimitBytes
	if targetChanged && (spec.Generation <= provenance.WorkloadGeneration || spec.SourceSpecFingerprint == provenance.SourceSpecFingerprint) {
		return server.inconclusive(&run, "the target values are visible but Kubernetes provenance does not prove a new workload spec")
	}
	if previous != nil && previous.FirstObservedAt != nil &&
		(spec.Generation != previous.ObservedGeneration || spec.SourceSpecFingerprint != previous.ObservedSpecFingerprint) {
		return server.inconclusive(&run, "the workload spec changed after the target configuration was first observed")
	}
	if spec.MetricIdentity == nil || spec.MetricIdentity.UID != spec.UID || spec.MetricIdentity.Generation != spec.Generation {
		return server.inconclusive(&run, "the live workload has no exact metric identity for its current UID and generation")
	}
	identity := spec.MetricIdentity.ForContainer(record.Container)
	if identity.ExpectedReplicas != spec.Replicas {
		return server.inconclusive(&run, "the exact metric identity does not cover the live desired replica count")
	}

	run.ConfigurationObserved = true
	run.Attribution = repository.VerificationAttributionExactPodUID
	if run.FirstObservedAt == nil {
		first := now
		run.FirstObservedAt = &first
		run.ObservedGeneration = spec.Generation
		run.ObservedSpecFingerprint = spec.SourceSpecFingerprint
	}
	postChange := now.Sub(*run.FirstObservedAt)
	if postChange < 0 {
		return server.inconclusive(&run, "the persisted first observation is later than this verification attempt")
	}
	run.PostChangeWindowSeconds = int64(postChange / time.Second)
	if postChange < minimumPostChange {
		run.Status = VerificationPending
		run.Reasons = append(run.Reasons, fmt.Sprintf("only %s of exact post-change observation exists; %s is required", postChange.Truncate(time.Minute), minimumPostChange))
		return run
	}

	windowDuration := min(postChange, maximumPostChange)
	queryWindow := usage.Window{Duration: windowDuration, Step: chartStep(windowDuration, server.config.RecommendationStep), End: now}
	subject := usage.Subject{Namespace: record.Namespace, Kind: record.Kind, Workload: record.Workload, Container: record.Container}
	history, err := exactSource.HistoryForIdentity(ctx, subject, identity, queryWindow)
	if err != nil {
		return server.inconclusive(&run, "exact post-change CPU and memory history could not be read")
	}
	events, err := exactSource.EventsForIdentity(ctx, subject, identity, queryWindow)
	if err != nil {
		return server.inconclusive(&run, "exact post-change restart, OOM, and throttling evidence could not be read")
	}
	if reasons := incompleteVerificationEvidence(history, events, identity, spec, record); len(reasons) > 0 {
		run.Status = VerificationInconclusive
		run.Reasons = append(run.Reasons, reasons...)
		return run
	}

	run.EvidenceComplete = true
	run.Checks = postChangeChecks(record, history, events, windowDuration)
	for _, check := range run.Checks {
		if check.Regressed {
			run.Reasons = append(run.Reasons, check.Detail)
		}
	}
	if len(run.Reasons) > 0 {
		run.Status = VerificationRegressed
	} else {
		run.Status = VerificationVerified
		run.Reasons = append(run.Reasons, "no safety regression was detected in the complete exact post-change evidence")
	}
	return run
}

func incompleteVerificationEvidence(history usage.History, events usage.Events, identity usage.WorkloadIdentity, spec WorkloadSpec, record repository.RecommendationRecord) []string {
	reasons := []string{}
	if len(history.Warnings) > 0 {
		reasons = append(reasons, "Prometheus returned warnings for the exact post-change history")
	}
	for _, series := range []usage.Series{history.CPU, history.Memory} {
		blocking, _ := series.QualityIssues()
		reasons = append(reasons, blocking...)
		if len(series.Points) == 0 {
			reasons = append(reasons, series.Resource+" post-change history is empty")
		}
	}
	coverage := history.ReplicaCoverage
	if coverage == nil {
		reasons = append(reasons, "exact replica coverage is missing")
	} else {
		if !coverage.Complete || len(coverage.Issues) > 0 || coverage.MissingReplicas > 0 {
			reasons = append(reasons, "exact replica coverage is incomplete")
		}
		if coverage.ExpectedReplicas != spec.Replicas || coverage.ObservedReplicas < int(spec.Replicas) ||
			coverage.CPUObservedReplicas < int(spec.Replicas) || coverage.MemoryObservedReplicas < int(spec.Replicas) {
			reasons = append(reasons, "CPU and memory do not cover every desired replica")
		}
	}
	if !events.Available {
		reasons = append(reasons, "restart and OOM evidence is unavailable")
	}
	if record.CPULimitMilli > 0 && !events.ThrottlingAvailable {
		reasons = append(reasons, "CPU throttling evidence is unavailable for the limited container")
	}
	expected := make(map[string]string, len(identity.Pods))
	for _, pod := range identity.Pods {
		expected[pod.Name] = pod.UID
	}
	if len(events.Replicas) != len(expected) {
		reasons = append(reasons, "event evidence does not cover every exact pod UID")
	} else {
		for _, replica := range events.Replicas {
			if expected[replica.Pod] != replica.UID {
				reasons = append(reasons, "event evidence contains a pod outside the exact workload identity")
				break
			}
		}
	}
	return sortedUnique(reasons)
}

func sortedUnique(values []string) []string {
	sort.Strings(values)
	result := values[:0]
	for _, value := range values {
		if len(result) == 0 || result[len(result)-1] != value {
			result = append(result, value)
		}
	}
	return result
}

func matches(live, approved int64) bool {
	if approved == 0 || live == 0 {
		return approved == live
	}
	return math.Abs(float64(live-approved)) <= float64(approved)*appliedTolerance
}

func ratioPointer(value float64) *float64 {
	rounded := math.Round(value*10_000) / 10_000
	return &rounded
}

func maximum(values []float64) float64 {
	result := 0.0
	for _, value := range values {
		result = math.Max(result, value)
	}
	return result
}

// postChangeChecks use absolute safety signals plus the recommendation's
// durable pre-change throttling baseline. Missing signals never reach here.
func postChangeChecks(record repository.RecommendationRecord, history usage.History, events usage.Events, window time.Duration) []repository.VerificationCheck {
	hours := math.Max(window.Hours(), 1)
	oomAfter := float64(events.OOMKills)
	restartsAfter := float64(events.Restarts) / hours
	checks := []repository.VerificationCheck{
		{Name: "OOM kills", After: ratioPointer(oomAfter), Regressed: events.OOMKills > 0, Detail: fmt.Sprintf("%d OOM kill(s) occurred after the target configuration was observed", events.OOMKills)},
		{Name: "Restarts per hour", After: ratioPointer(restartsAfter), Regressed: events.Restarts > 0, Detail: fmt.Sprintf("%.2f restart(s) per hour occurred after the target configuration was observed", restartsAfter)},
	}
	if events.ThrottlingAvailable {
		before := record.CPUThrottlingRatio
		after := events.ThrottlingRatio
		checks = append(checks, repository.VerificationCheck{
			Name: "CPU throttling", Before: ratioPointer(before), After: ratioPointer(after),
			Regressed: after >= 0.05 && after-before >= 0.05,
			Detail:    fmt.Sprintf("throttled periods changed from %.1f%% before analysis to %.1f%% after the target was observed", before*100, after*100),
		})
	}
	peakMemory := maximum(history.Memory.Values())
	requestRatio := 0.0
	if record.MemoryRequestBytes > 0 {
		requestRatio = peakMemory / float64(record.MemoryRequestBytes)
	}
	checks = append(checks, repository.VerificationCheck{
		Name: "Peak memory vs request", After: ratioPointer(requestRatio),
		Detail: fmt.Sprintf("peak memory reached %.0f%% of the approved request", requestRatio*100),
	})
	if record.MemoryLimitBytes > 0 {
		limitRatio := peakMemory / float64(record.MemoryLimitBytes)
		checks = append(checks, repository.VerificationCheck{
			Name: "Peak memory vs limit", After: ratioPointer(limitRatio), Regressed: limitRatio >= 0.95,
			Detail: fmt.Sprintf("peak memory reached %.0f%% of the approved limit", limitRatio*100),
		})
	}
	return checks
}

// renderVerificationMetrics derives gauges from durable outcomes, so restarts
// and multiple API replicas cannot lose or disagree about latest verdicts.
func (server *apiServer) renderVerificationMetrics(ctx context.Context, w http.ResponseWriter) {
	counts := map[string]int{}
	result, err := server.repository.ListVerificationOutcomes(ctx, 1, 10_000)
	if err != nil {
		server.logger.Error("verification metrics could not list outcomes", "error", err)
	} else {
		for _, outcome := range result.Items {
			counts[outcome.Status]++
		}
	}
	_, _ = fmt.Fprintln(w, "# HELP kuberight_recommendation_verifications Approved recommendations by durable post-change verification status.")
	_, _ = fmt.Fprintln(w, "# TYPE kuberight_recommendation_verifications gauge")
	for _, status := range []string{VerificationVerified, VerificationRegressed, VerificationPending, VerificationNotApplied, VerificationInconclusive, VerificationUnavailable} {
		_, _ = fmt.Fprintf(w, "kuberight_recommendation_verifications{status=%q} %d\n", status, counts[status])
	}
	_, _ = fmt.Fprintln(w, "# HELP kuberight_recommendation_regressions Applied recommendations followed by a durable exact-evidence regression.")
	_, _ = fmt.Fprintln(w, "# TYPE kuberight_recommendation_regressions gauge")
	_, _ = fmt.Fprintf(w, "kuberight_recommendation_regressions %d\n", counts[VerificationRegressed])
}

// Verifier periodically refreshes approved recommendations. Every attempt is
// append-only and the latest outcome is durable.
type Verifier struct{ server *apiServer }

func NewVerifier(cfg config.Config, dependencies Dependencies) (*Verifier, error) {
	server, err := newAPIServer(cfg, dependencies)
	if err != nil {
		return nil, err
	}
	return &Verifier{server: server}, nil
}

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
			if _, err := server.refreshVerification(ctx, record); err != nil {
				server.logger.Warn("verification refresh failed", "recommendation_id", record.ID, "error", err)
			}
			checked++
		}
		if len(result.Items) < 100 {
			return checked
		}
	}
	return checked
}
