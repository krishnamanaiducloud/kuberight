package httpapi

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/kuberight/kuberight/internal/config"
	"github.com/kuberight/kuberight/internal/notify"
	"github.com/kuberight/kuberight/internal/optimizer"
	"github.com/kuberight/kuberight/internal/repository"
	"github.com/kuberight/kuberight/internal/usage"
)

const (
	maxSourceWarnings      = 5
	maxSourceWarningLength = 256
	// rolloutShiftThreshold is the relative CPU change around a rollout that
	// makes pre-rollout usage unrepresentative.
	rolloutShiftThreshold = 0.25
	// Live advice is deliberately short-lived. Approval rechecks workload
	// identity and spec as well, so this bounds metric staleness independently
	// of Kubernetes configuration changes.
	recommendationValidity = 24 * time.Hour
)

// attributedUsageSource is implemented by the Prometheus source. It binds
// every sample to pod UIDs resolved from immutable Kubernetes owner chains.
type attributedUsageSource interface {
	HistoryForIdentity(context.Context, usage.Subject, usage.WorkloadIdentity, usage.Window) (usage.History, error)
	EventsForIdentity(context.Context, usage.Subject, usage.WorkloadIdentity, usage.Window) (usage.Events, error)
}

func recommendationReplicaCoverage(coverage *usage.ReplicaCoverage) *optimizer.ReplicaCoverage {
	if coverage == nil {
		return nil
	}
	result := &optimizer.ReplicaCoverage{
		ExpectedReplicas: coverage.ExpectedReplicas, DiscoveredReplicas: coverage.DiscoveredReplicas,
		ObservedReplicas: coverage.ObservedReplicas, MissingReplicas: coverage.MissingReplicas,
		ActiveReplicas: coverage.ActiveReplicas, TerminatedReplicas: coverage.TerminatedReplicas,
		CPUObservedReplicas: coverage.CPUObservedReplicas, MemoryObservedReplicas: coverage.MemoryObservedReplicas,
		CPUReplicaCoverage: coverage.CPUReplicaCoverage, MemoryReplicaCoverage: coverage.MemoryReplicaCoverage,
		CPUTemporalCoverage: coverage.CPUTemporalCoverage, MemoryTemporalCoverage: coverage.MemoryTemporalCoverage,
		CPUImbalanceRatio: coverage.CPUImbalanceRatio, MemoryImbalanceRatio: coverage.MemoryImbalanceRatio,
		Complete: coverage.Complete, Replicas: make([]optimizer.ReplicaQuality, 0, len(coverage.Observations)),
	}
	for _, replica := range coverage.Observations {
		result.Replicas = append(result.Replicas, optimizer.ReplicaQuality{
			Pod: replica.Pod, UID: replica.UID, Phase: replica.Phase, Active: replica.Active,
			CPUCoverage: replica.CPU.Quality.Coverage, MemoryCoverage: replica.Memory.Quality.Coverage,
		})
	}
	return result
}

func (server *apiServer) repositoryFailure(w http.ResponseWriter, r *http.Request, operation string, err error) {
	server.logger.Error("repository operation failed", "request_id", requestIDFromContext(r.Context()), "operation", operation, "error", err)
	if errors.Is(err, context.DeadlineExceeded) {
		respondError(w, r, http.StatusGatewayTimeout, "repository_timeout", "the recommendation store did not respond in time")
		return
	}
	respondError(w, r, http.StatusServiceUnavailable, "repository_unavailable", "the recommendation store is unavailable")
}

func validRecommendationID(id string) bool {
	return len(id) >= 5 && len(id) <= 64 && strings.HasPrefix(id, "rec_")
}

func (server *apiServer) listRecommendations(w http.ResponseWriter, r *http.Request) {
	server.recommendationList(w, r, "recommendations", r.URL.Query().Get("reviewStatus"))
}

// approvals lists recommendations awaiting review by default. Approval
// records a decision only; it never applies a change to a workload.
func (server *apiServer) approvals(w http.ResponseWriter, r *http.Request) {
	status := r.URL.Query().Get("reviewStatus")
	switch status {
	case "":
		status = repository.ReviewPending
	case "all":
		status = ""
	}
	server.recommendationList(w, r, "approvals", status)
}

func (server *apiServer) recommendationList(w http.ResponseWriter, r *http.Request, resource, reviewStatus string) {
	page, pageSize, err := parsePagination(r)
	if err != nil {
		respondError(w, r, http.StatusBadRequest, "invalid_pagination", err.Error())
		return
	}
	query := r.URL.Query()
	options := repository.ListOptions{
		Page: page, PageSize: pageSize, Namespace: query.Get("namespace"), Workload: query.Get("workload"),
		Confidence: query.Get("confidence"), Risk: query.Get("risk"), ReviewStatus: reviewStatus,
		Filter: query.Get("filter"), Sort: query.Get("sort"), Order: query.Get("order"),
		LatestOnly: query.Get("latest") == "true",
	}
	if options.Sort == "" {
		options.Sort = "createdAt"
	}
	if options.Order == "" {
		options.Order = "desc"
	}
	if !contains([]string{"createdAt", "namespace", "workload", "confidence", "risk"}, options.Sort) || !contains([]string{"asc", "desc"}, options.Order) {
		respondError(w, r, http.StatusBadRequest, "invalid_sort", "sort or order is not supported")
		return
	}
	if options.Confidence != "" && !contains([]string{"low", "medium", "high"}, options.Confidence) || options.Risk != "" && !contains([]string{"low", "medium", "high"}, options.Risk) {
		respondError(w, r, http.StatusBadRequest, "invalid_filter", "confidence and risk filters must be low, medium, or high")
		return
	}
	if options.ReviewStatus != "" && !contains([]string{repository.ReviewPending, repository.ReviewApproved, repository.ReviewRejected}, options.ReviewStatus) {
		respondError(w, r, http.StatusBadRequest, "invalid_filter", "reviewStatus must be pending, approved, or rejected")
		return
	}
	if latest := query.Get("latest"); latest != "" && latest != "true" && latest != "false" {
		respondError(w, r, http.StatusBadRequest, "invalid_filter", "latest must be true or false")
		return
	}
	if !server.authorize(w, r, AccessAttributes{Verb: "list", Resource: resource, Namespace: options.Namespace}) {
		return
	}
	result, err := server.repository.ListRecommendations(r.Context(), options)
	if err != nil {
		server.repositoryFailure(w, r, "list recommendations", err)
		return
	}
	respondJSON(w, http.StatusOK, map[string]any{
		"items":      result.Items,
		"pagination": map[string]int{"page": result.Page, "pageSize": result.PageSize, "total": result.Total},
		"status":     Availability{Available: true, Source: server.repository.Kind()},
	})
}

func (server *apiServer) getRecommendation(w http.ResponseWriter, r *http.Request) {
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
	respondJSON(w, http.StatusOK, record)
}

func (server *apiServer) calculateRecommendation(w http.ResponseWriter, r *http.Request) {
	if server.config.OperationMode == config.ModeObserve {
		respondError(w, r, http.StatusForbidden, "operation_disabled", "recommendation generation is disabled in observe mode")
		return
	}
	var input optimizer.Input
	if err := decodeJSON(w, r, &input); err != nil {
		writeDecodeError(w, r, err)
		return
	}
	if !server.authorize(w, r, AccessAttributes{Verb: "create", Resource: "recommendations", Namespace: input.Namespace}) {
		return
	}
	recommendation, err := optimizer.Recommend(input)
	if err != nil {
		respondError(w, r, http.StatusUnprocessableEntity, "invalid_recommendation_input", err.Error())
		return
	}
	server.storeRecommendation(w, r, recommendation, input, repository.SourceRequest)
}

// generateRequest asks KubeRight to read a workload's live configuration and
// Prometheus usage, then calculate a recommendation server-side.
type generateRequest struct {
	Namespace   string `json:"namespace"`
	Kind        string `json:"kind"`
	Workload    string `json:"workload"`
	Container   string `json:"container"`
	Criticality string `json:"criticality,omitempty"`
	Window      string `json:"window,omitempty"`
	// Profile names a built-in policy. An explicit Policy takes precedence.
	Profile string            `json:"profile,omitempty"`
	Policy  *optimizer.Policy `json:"policy,omitempty"`
	// DryRun calculates and returns the recommendation without storing or
	// auditing it, so callers can tune a policy interactively.
	DryRun bool `json:"dryRun,omitempty"`
}

// parseDuration parses an optional lookback between minimum and 30 days.
func parseDuration(raw string, fallback, minimum time.Duration) (time.Duration, error) {
	window := fallback
	if raw != "" {
		parsed, err := time.ParseDuration(raw)
		if err != nil {
			return 0, errors.New("window must be a duration such as 168h")
		}
		window = parsed
	}
	if window < minimum || window > 30*24*time.Hour {
		return 0, fmt.Errorf("window must be between %s and 720h", minimum)
	}
	return window, nil
}

// parseWindow validates a caller-supplied lookback against the step and the
// Prometheus point bound. An empty value selects the configured default.
func parseWindow(raw string, fallback, step, minimum time.Duration) (time.Duration, error) {
	window, err := parseDuration(raw, fallback, minimum)
	if err != nil {
		return 0, err
	}
	if window <= step || window/step+1 > config.MaximumQueryPoints {
		return 0, fmt.Errorf("window must contain between 2 and %d steps of %s", config.MaximumQueryPoints, step)
	}
	return window, nil
}

func (server *apiServer) resolveWorkload(w http.ResponseWriter, r *http.Request, subject usage.Subject) (WorkloadSpec, ContainerSpec, bool) {
	resolver, ok := server.provider.(WorkloadResolver)
	if server.provider == nil || !ok {
		respondError(w, r, http.StatusServiceUnavailable, "integration_not_configured", "Kubernetes inventory is not configured, so current workload resources cannot be read")
		return WorkloadSpec{}, ContainerSpec{}, false
	}
	spec, err := resolver.Workload(r.Context(), subject.Namespace, subject.Kind, subject.Workload)
	switch {
	case errors.Is(err, ErrWorkloadNotFound):
		respondError(w, r, http.StatusNotFound, "workload_not_found", "the workload was not found")
		return WorkloadSpec{}, ContainerSpec{}, false
	case errors.Is(err, ErrWorkloadOutOfScope):
		respondError(w, r, http.StatusUnprocessableEntity, "workload_out_of_scope", err.Error())
		return WorkloadSpec{}, ContainerSpec{}, false
	case err != nil:
		server.logger.Error("workload lookup failed", "request_id", requestIDFromContext(r.Context()), "error", err)
		respondError(w, r, http.StatusServiceUnavailable, "dependency_unavailable", "the Kubernetes inventory is unavailable")
		return WorkloadSpec{}, ContainerSpec{}, false
	}
	container, found := spec.Container(subject.Container)
	if !found && spec.IsInitContainer(subject.Container) {
		respondError(w, r, http.StatusUnprocessableEntity, "unsupported_container", "unsupported: init containers run to completion before the application starts, so their brief usage history cannot be sized safely; native sidecars (restartPolicy Always) are supported")
		return WorkloadSpec{}, ContainerSpec{}, false
	}
	if !found {
		respondError(w, r, http.StatusNotFound, "container_not_found", "the container was not found in the workload")
		return WorkloadSpec{}, ContainerSpec{}, false
	}
	return spec, container, true
}

func (server *apiServer) usageFailure(w http.ResponseWriter, r *http.Request, err error) {
	if errors.Is(err, usage.ErrAttributionIncomplete) {
		respondError(w, r, http.StatusUnprocessableEntity, "metric_attribution_incomplete", err.Error())
		return
	}
	if errors.Is(err, usage.ErrNoData) {
		respondError(w, r, http.StatusUnprocessableEntity, "no_usage_data", "Prometheus has no CPU or memory usage for this container in the requested window")
		return
	}
	server.logger.Error("usage query failed", "request_id", requestIDFromContext(r.Context()), "error", err)
	if errors.Is(err, context.DeadlineExceeded) {
		respondError(w, r, http.StatusGatewayTimeout, "dependency_timeout", "Prometheus did not respond in time")
		return
	}
	respondError(w, r, http.StatusServiceUnavailable, "dependency_unavailable", "Prometheus is unavailable")
}

func (server *apiServer) generateRecommendation(w http.ResponseWriter, r *http.Request) {
	var request generateRequest
	if err := decodeJSON(w, r, &request); err != nil {
		writeDecodeError(w, r, err)
		return
	}
	// A dry run stores nothing, so it remains available in observe mode.
	if server.config.OperationMode == config.ModeObserve && !request.DryRun {
		respondError(w, r, http.StatusForbidden, "operation_disabled", "recommendation generation is disabled in observe mode; use dryRun to preview")
		return
	}
	policy := request.Policy
	if policy == nil && request.Profile != "" {
		profile, found := optimizer.ProfileByName(request.Profile)
		if !found {
			respondError(w, r, http.StatusBadRequest, "invalid_request", "profile must be conservative, balanced, or aggressive")
			return
		}
		policy = &profile
	}
	subject := usage.Subject{Namespace: request.Namespace, Kind: request.Kind, Workload: request.Workload, Container: request.Container}
	if err := subject.Validate(); err != nil {
		respondError(w, r, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	step := server.config.RecommendationStep
	window, err := parseWindow(request.Window, server.config.RecommendationWindow, step, time.Hour)
	if err != nil {
		respondError(w, r, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	if !server.authorize(w, r, AccessAttributes{Verb: "create", Resource: "recommendations", Namespace: request.Namespace}) {
		return
	}
	if server.usage == nil {
		respondError(w, r, http.StatusServiceUnavailable, "integration_not_configured", "Prometheus is not configured; set KUBERIGHT_PROMETHEUS_URL or submit samples to /api/v1/recommendations:calculate")
		return
	}
	spec, container, ok := server.resolveWorkload(w, r, subject)
	if !ok {
		return
	}
	matchedPolicy := ""
	if policy == nil {
		record, policyErr := server.repository.ResolvePolicy(r.Context(), repository.PolicyScope{
			Cluster: spec.Cluster, Namespace: spec.Namespace, Kind: spec.Kind, Workload: spec.Name, Container: container.Name,
		})
		switch {
		case policyErr == nil:
			resolved := record.Policy
			policy = &resolved
			matchedPolicy = record.Name + " (" + record.ID + ")"
		case errors.Is(policyErr, repository.ErrNotFound):
			// No persisted match means the optimizer's balanced default.
		default:
			server.repositoryFailure(w, r, "resolve recommendation policy", policyErr)
			return
		}
	}
	// Use a second-aligned endpoint everywhere: Prometheus range parameters,
	// provenance timestamps, expiry, and the query fingerprint must describe
	// exactly the same observation window.
	queryWindow := usage.Window{Duration: window, Step: step, End: server.now().UTC().Truncate(time.Second)}
	var (
		history          usage.History
		events           usage.Events
		metricIdentity   *usage.WorkloadIdentity
		exactSource      attributedUsageSource
		exactAttribution bool
	)
	if spec.MetricIdentity != nil {
		identity := spec.MetricIdentity.ForContainer(container.Name)
		metricIdentity = &identity
		exactSource, exactAttribution = server.usage.(attributedUsageSource)
		if !exactAttribution {
			server.usageFailure(w, r, fmt.Errorf("%w: the configured metric source cannot query exact pod UIDs", usage.ErrAttributionIncomplete))
			return
		}
		history, err = exactSource.HistoryForIdentity(r.Context(), subject, identity, queryWindow)
	} else {
		// Non-Kubernetes test and extension providers can retain the legacy
		// interface. The built-in Kubernetes provider always supplies identity.
		history, err = server.usage.History(r.Context(), subject, queryWindow)
	}
	if err != nil {
		server.usageFailure(w, r, err)
		return
	}

	warnings := []string{}
	if matchedPolicy != "" {
		warnings = append(warnings, "effective persisted policy: "+matchedPolicy)
	}
	eventQualityIssues := []string{}
	if exactAttribution {
		events, err = exactSource.EventsForIdentity(r.Context(), subject, *metricIdentity, queryWindow)
	} else {
		events, err = server.usage.Events(r.Context(), subject, queryWindow)
	}
	if err != nil {
		server.logger.Warn("restart and OOM query failed", "request_id", requestIDFromContext(r.Context()), "error", err)
		warnings = append(warnings, "restart and OOM history could not be read; review container events before applying")
		if exactAttribution {
			eventQualityIssues = append(eventQualityIssues, "per-replica restart and OOM history could not be verified")
		}
	} else if !events.Available {
		warnings = append(warnings, "restart and OOM history is unavailable because kube-state-metrics series were not found")
		if exactAttribution {
			eventQualityIssues = append(eventQualityIssues, "per-replica restart and OOM history is unavailable")
		}
	}
	if events.ThrottlingAvailable && events.ThrottlingRatio == 0 {
		warnings = append(warnings, "no CPU throttling was observed in the window")
	}
	// Usage recorded before a rollout that materially changed it describes
	// code that no longer runs, so size from post-rollout samples instead.
	rolloutIssues := []string{}
	recentRollout := false
	if rollouts, rolloutErr := server.usage.Rollouts(r.Context(), subject, history.Start, history.End); rolloutErr == nil {
		if at, change, ok := rolloutShift(history.CPU, rollouts); ok && math.Abs(change) >= rolloutShiftThreshold {
			minimumSamples := optimizer.DefaultPolicy().MinimumSamples
			if policy != nil && policy.MinimumSamples > 0 {
				minimumSamples = policy.MinimumSamples
			}
			cpuAfter, memoryAfter := history.CPU.Since(at), history.Memory.Since(at)
			if len(cpuAfter.Points) >= minimumSamples && len(memoryAfter.Points) >= minimumSamples {
				history.CPU, history.Memory = cpuAfter, memoryAfter
				recentRollout = true
				warnings = append(warnings, fmt.Sprintf("CPU changed %+.0f%% after the rollout at %s; only the %d post-rollout samples were used", change*100, at.Format(time.RFC3339), len(cpuAfter.Points)))
			} else {
				rolloutIssues = append(rolloutIssues, fmt.Sprintf("CPU changed %+.0f%% after the rollout at %s and too few post-rollout samples exist", change*100, at.Format(time.RFC3339)))
			}
		}
	}
	blockingCPU, informationalCPU := history.CPU.QualityIssues()
	blockingCPU = append(blockingCPU, rolloutIssues...)
	blockingMemory, informationalMemory := history.Memory.QualityIssues()
	replicaIssues := []string{}
	if history.ReplicaCoverage != nil {
		replicaIssues = append(replicaIssues, history.ReplicaCoverage.Issues...)
	}
	input := optimizer.Input{
		Cluster: spec.Cluster, Namespace: spec.Namespace, Kind: spec.Kind, Workload: spec.Name, Container: container.Name,
		Criticality:      request.Criticality,
		CPURequestsMilli: container.CPURequestMilli, MemoryRequestsBytes: container.MemoryRequestBytes,
		CPULimitMilli: container.CPULimitMilli, MemoryLimitBytes: container.MemoryLimitBytes,
		CPUSamplesMilli: history.CPU.Values(), MemorySamplesBytes: history.Memory.Values(),
		SampleIntervalSeconds:          int64(step / time.Second),
		ObservedContainerRestarts:      events.Restarts,
		ObservedOOMKills:               events.OOMKills,
		ObservedCPUThrottlingRatio:     events.ThrottlingRatio,
		ObservedCPUThrottlingPeakRatio: events.ThrottlingPeakRatio,
		SamplesStartTime:               firstSampleTime(history.CPU),
		Replicas:                       spec.Replicas,
		DataQualityIssues:              append(append(append(blockingCPU, blockingMemory...), replicaIssues...), eventQualityIssues...),
		ReplicaCoverage:                recommendationReplicaCoverage(history.ReplicaCoverage),
		Autoscaling: optimizer.AutoscalingContext{
			HPAUtilizationBased: spec.HPAUtilizationBased,
			VPAControlsCPU:      container.VPAControlsCPU,
			VPAControlsMemory:   container.VPAControlsMemory,
			KEDAEnabled:         spec.KEDAEnabled,
		},
		Policy: policy,
		Provenance: optimizer.Provenance{
			WorkloadUID:             spec.UID,
			WorkloadGeneration:      spec.Generation,
			WorkloadResourceVersion: spec.ResourceVersion,
			SourceSpecFingerprint:   spec.SourceSpecFingerprint,
			ObservationStart:        observationStart(history, queryWindow),
			ObservationEnd:          timePointer(queryWindow.End),
			QueryFingerprint:        usage.RecommendationQueryFingerprint(subject, queryWindow),
			ExpiresAt:               timePointer(queryWindow.End.Add(recommendationValidity)),
		},
		RecentRollout: recentRollout,
	}
	if exactAttribution {
		fingerprint, fingerprintErr := usage.AttributedRecommendationQueryFingerprint(subject, *metricIdentity, queryWindow)
		if fingerprintErr != nil {
			server.usageFailure(w, r, fingerprintErr)
			return
		}
		input.Provenance.QueryFingerprint = fingerprint
	}
	warnings = append(warnings, server.assessmentContext(r.Context(), &input, spec, history)...)
	recommendation, err := optimizer.Recommend(input)
	if err != nil {
		respondError(w, r, http.StatusUnprocessableEntity, "invalid_recommendation_input", err.Error())
		return
	}
	// Volatile or changing usage gets a shorter validity than the default.
	if assessment := recommendation.Assessment; assessment != nil && assessment.ValidForSeconds > 0 {
		expires := queryWindow.End.Add(time.Duration(assessment.ValidForSeconds) * time.Second)
		recommendation.Provenance.ExpiresAt = &expires
	}
	warnings = append(warnings, informationalCPU...)
	warnings = append(warnings, informationalMemory...)
	warnings = append(warnings, spec.Warnings...)
	for index, warning := range history.Warnings {
		if index == maxSourceWarnings {
			break
		}
		warnings = append(warnings, "Prometheus: "+truncate(warning, maxSourceWarningLength))
	}
	recommendation.Warnings = append(recommendation.Warnings, warnings...)
	if request.DryRun {
		respondJSON(w, http.StatusOK, map[string]any{
			"dryRun": true, "recommendation": recommendation, "source": repository.SourcePrometheus,
			"window": window.String(), "step": step.String(), "events": events, "replicaCoverage": history.ReplicaCoverage,
		})
		return
	}
	server.storeRecommendation(w, r, recommendation, request, repository.SourcePrometheus)
}

func truncate(value string, maximum int) string {
	if len(value) <= maximum {
		return value
	}
	cut := maximum
	for cut > 0 && !utf8.RuneStart(value[cut]) {
		cut--
	}
	return value[:cut]
}

// storeRecommendation persists a recommendation and its audit event in one
// transaction. The idempotency fingerprint covers the caller's request so a
// replay returns the original record even when live data has since changed.
func (server *apiServer) storeRecommendation(w http.ResponseWriter, r *http.Request, recommendation optimizer.Recommendation, request any, source string) {
	payload, err := json.Marshal(request)
	if err != nil {
		respondError(w, r, http.StatusInternalServerError, "internal_error", "could not fingerprint the recommendation request")
		return
	}
	fingerprintDigest := sha256.Sum256(append([]byte(source+"\x00"), payload...))
	principal, ok := principalFromContext(r.Context())
	if !ok {
		respondError(w, r, http.StatusUnauthorized, "unauthorized", "an authenticated identity is required")
		return
	}
	var (
		record  repository.RecommendationRecord
		created bool
	)
	err = server.repository.InTransaction(r.Context(), func(ctx context.Context, tx repository.Transaction) error {
		var createErr error
		record, created, createErr = tx.CreateRecommendation(ctx, recommendation, repository.CreateOptions{
			IdempotencyKey: strings.TrimSpace(r.Header.Get("Idempotency-Key")),
			Scope:          principal.Username + "\x00" + principal.UID + "\x00" + recommendation.Namespace,
			Fingerprint:    hex.EncodeToString(fingerprintDigest[:]),
			CreatedBy:      principal.Username,
			Source:         source,
		})
		if createErr != nil {
			return createErr
		}
		action := "replay"
		if created {
			action = "create"
		}
		return tx.AppendAuditEvent(ctx, server.auditEvent(r, principal, action, "recommendations/"+record.ID, record.Namespace))
	})
	switch {
	case errors.Is(err, repository.ErrIdempotencyConflict):
		respondError(w, r, http.StatusConflict, "idempotency_conflict", err.Error())
		return
	case errors.Is(err, repository.ErrInvalidIdempotency):
		respondError(w, r, http.StatusBadRequest, "invalid_idempotency_key", err.Error())
		return
	case err != nil:
		server.repositoryFailure(w, r, "create recommendation", err)
		return
	}
	status := http.StatusOK
	if created {
		server.metrics.observeRecommendation(record.Confidence, record.Risk)
		server.notify(notify.RecommendationCreated, record, principal.Username, "")
		status = http.StatusCreated
	}
	w.Header().Set("Location", "/api/v1/recommendations/"+record.ID)
	respondJSON(w, status, record)
}

func (server *apiServer) auditEvent(r *http.Request, principal Principal, action, resource, namespace string) repository.AuditEvent {
	actor := principal.Username
	if actor == "" {
		actor = "unknown"
	}
	requestID := requestIDFromContext(r.Context())
	if requestID == "" {
		requestID = "unknown"
	}
	return repository.AuditEvent{
		Timestamp: server.now().UTC(), Actor: truncate(actor, 512), Action: action, Resource: resource,
		Namespace: namespace, Outcome: "success", RequestID: requestID,
	}
}

type reviewRequest struct {
	Decision string `json:"decision"`
	Comment  string `json:"comment,omitempty"`
}

func (server *apiServer) reviewRecommendation(w http.ResponseWriter, r *http.Request) {
	if server.config.OperationMode == config.ModeObserve {
		respondError(w, r, http.StatusForbidden, "operation_disabled", "recommendation reviews are disabled in observe mode")
		return
	}
	id := r.PathValue("id")
	if !validRecommendationID(id) {
		respondError(w, r, http.StatusBadRequest, "invalid_id", "recommendation identifier is invalid")
		return
	}
	var request reviewRequest
	if err := decodeJSON(w, r, &request); err != nil {
		writeDecodeError(w, r, err)
		return
	}
	status := map[string]string{"approve": repository.ReviewApproved, "reject": repository.ReviewRejected}[request.Decision]
	if status == "" {
		respondError(w, r, http.StatusBadRequest, "invalid_request", "decision must be approve or reject")
		return
	}
	request.Comment = strings.TrimSpace(request.Comment)
	if status == repository.ReviewRejected && request.Comment == "" {
		respondError(w, r, http.StatusBadRequest, "invalid_request", "a comment explaining the rejection is required")
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
	// Reviews are authorized like the Kubernetes CSR approval subresource.
	if !server.authorize(w, r, AccessAttributes{Verb: "update", Resource: "recommendations", Subresource: "approval", Namespace: record.Namespace, Name: id}) {
		return
	}
	principal, _ := principalFromContext(r.Context())
	if server.config.RequireDistinctApprover && record.CreatedBy != "" && record.CreatedBy == principal.Username {
		respondError(w, r, http.StatusForbidden, "separation_of_duties", "a recommendation must be reviewed by an identity other than the one that created it")
		return
	}
	if status == repository.ReviewApproved {
		// Hold, insufficient-data, and unsupported advice cannot be approved;
		// it can still be rejected with a comment.
		if approvable, reason := record.Approvable(); !approvable {
			respondError(w, r, http.StatusConflict, "recommendation_not_approvable", reason)
			return
		}
		staleReason, freshnessErr := server.recommendationStaleness(r.Context(), record)
		if freshnessErr != nil {
			server.logger.Warn("recommendation freshness check failed", "request_id", requestIDFromContext(r.Context()), "recommendation_id", id, "error", freshnessErr)
			respondError(w, r, http.StatusServiceUnavailable, "freshness_check_unavailable", "the live workload could not be verified; retry before approving")
			return
		}
		if staleReason != "" {
			respondError(w, r, http.StatusConflict, "recommendation_stale", staleReason+"; generate a new recommendation")
			return
		}
	}
	var reviewed repository.RecommendationRecord
	err = server.repository.InTransaction(r.Context(), func(ctx context.Context, tx repository.Transaction) error {
		var reviewErr error
		reviewed, reviewErr = tx.ReviewRecommendation(ctx, id, repository.ReviewDecision{
			Status: status, Reviewer: truncate(principal.Username, 512), Comment: request.Comment, At: server.now(),
		})
		if reviewErr != nil {
			return reviewErr
		}
		return tx.AppendAuditEvent(ctx, server.auditEvent(r, principal, request.Decision, "recommendations/"+id, reviewed.Namespace))
	})
	switch {
	case errors.Is(err, repository.ErrNotFound):
		respondError(w, r, http.StatusNotFound, "not_found", "recommendation was not found")
	case errors.Is(err, repository.ErrReviewConflict):
		respondError(w, r, http.StatusConflict, "review_conflict", "the recommendation has already been reviewed")
	case errors.Is(err, repository.ErrInvalidReview):
		respondError(w, r, http.StatusBadRequest, "invalid_request", err.Error())
	case err != nil:
		server.repositoryFailure(w, r, "review recommendation", err)
	default:
		eventType := notify.RecommendationApproved
		if status == repository.ReviewRejected {
			eventType = notify.RecommendationRejected
		}
		server.notify(eventType, reviewed, principal.Username, request.Comment)
		respondJSON(w, http.StatusOK, reviewed)
	}
}

// recommendationStaleness validates live, Prometheus-backed advice immediately
// before approval. Caller-supplied calculations remain manually reviewable,
// but cannot silently claim that KubeRight verified their workload identity.
func (server *apiServer) recommendationStaleness(ctx context.Context, record repository.RecommendationRecord) (string, error) {
	if record.Source != repository.SourcePrometheus {
		return "", nil
	}
	provenance := record.Provenance
	if provenance.ExpiresAt == nil || provenance.WorkloadUID == "" || provenance.WorkloadGeneration <= 0 || provenance.SourceSpecFingerprint == "" {
		return "recommendation lacks verifiable live-workload provenance", nil
	}
	if record.Expired(server.now().UTC()) {
		return "recommendation observation window has expired", nil
	}
	resolver, ok := server.provider.(WorkloadResolver)
	if server.provider == nil || !ok {
		return "", errors.New("Kubernetes workload resolver is not configured")
	}
	spec, err := resolver.Workload(ctx, record.Namespace, record.Kind, record.Workload)
	if errors.Is(err, ErrWorkloadNotFound) || errors.Is(err, ErrWorkloadOutOfScope) {
		return "workload no longer resolves to the observed object", nil
	}
	if err != nil {
		return "", err
	}
	if spec.Cluster != record.Cluster || spec.UID != provenance.WorkloadUID {
		return "workload was recreated or moved to a different cluster", nil
	}
	if spec.Generation != provenance.WorkloadGeneration || spec.SourceSpecFingerprint != provenance.SourceSpecFingerprint {
		return "workload specification changed after analysis", nil
	}
	container, found := spec.Container(record.Container)
	if !found {
		return "container no longer exists in the workload", nil
	}
	if container.CPURequestMilli != record.CurrentCPURequestMilli || container.MemoryRequestBytes != record.CurrentMemoryRequestBytes ||
		container.CPULimitMilli != record.CurrentCPULimitMilli || container.MemoryLimitBytes != record.CurrentMemoryLimitBytes {
		return "container resources changed after analysis", nil
	}
	return "", nil
}

// notify forwards a lifecycle event to the configured integration, if any.
// Delivery is asynchronous and never fails the request.
func (server *apiServer) notify(eventType string, record repository.RecommendationRecord, actor, comment string) {
	if server.notifier == nil {
		return
	}
	server.notifier.Notify(notify.Event{
		Type: eventType, Timestamp: server.now().UTC(), Actor: actor, RecommendationID: record.ID,
		Cluster: record.Cluster, Namespace: record.Namespace, Kind: record.Kind, Workload: record.Workload, Container: record.Container,
		CurrentCPURequestMilli: record.CurrentCPURequestMilli, CPURequestMilli: record.CPURequestMilli,
		CurrentMemoryRequestBytes: record.CurrentMemoryRequestBytes, MemoryRequestBytes: record.MemoryRequestBytes,
		Risk: record.Risk, Confidence: record.Confidence, Comment: comment,
	})
}

// firstSampleTime anchors samples in time so recurring spikes can be
// reported as an hour of day.
func firstSampleTime(series usage.Series) *time.Time {
	if len(series.Points) == 0 {
		return nil
	}
	start := series.Points[0].Timestamp
	return &start
}

func timePointer(value time.Time) *time.Time {
	utc := value.UTC()
	return &utc
}

func observationStart(history usage.History, window usage.Window) *time.Time {
	start := window.End.Add(-window.Duration)
	for _, series := range []usage.Series{history.CPU, history.Memory} {
		if len(series.Points) > 0 && series.Points[0].Timestamp.After(start) {
			start = series.Points[0].Timestamp
		}
	}
	return timePointer(start)
}
