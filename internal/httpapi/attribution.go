package httpapi

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/kuberight/kuberight/internal/usage"
)

// attributionReporter is implemented by the Prometheus usage source.
type attributionReporter interface {
	AttributionReport(context.Context, usage.Subject, usage.WorkloadIdentity, usage.Window) (usage.AttributionReport, error)
}

// attributionIdentity is the Kubernetes side of the ownership chain: the
// workload, the ReplicaSets or Jobs whose controller reference carries its
// UID, and the live pods those own.
type attributionIdentity struct {
	Kind             string                `json:"kind"`
	Name             string                `json:"name"`
	Namespace        string                `json:"namespace"`
	UID              string                `json:"uid"`
	CreatedAt        time.Time             `json:"createdAt"`
	ExpectedReplicas int32                 `json:"expectedReplicas"`
	Owners           []usage.OwnerIdentity `json:"owners"`
	Pods             []usage.PodIdentity   `json:"pods"`
	ResolutionIssues []string              `json:"resolutionIssues"`
}

// attributionDiagnostics explains how Prometheus usage is attributed to one
// workload container: the UID chain from the Kubernetes API, the matching
// kube-state-metrics and cAdvisor series, every pod identity with the reason
// it was or was not attributed, metric family availability, coverage, and
// whether a recommendation can be calculated. It is read-only.
func (server *apiServer) attributionDiagnostics(w http.ResponseWriter, r *http.Request) {
	subject := subjectFromQuery(r)
	if err := subject.Validate(); err != nil {
		respondError(w, r, http.StatusBadRequest, "invalid_request", err.Error()+"; namespace, kind, workload, and container are required")
		return
	}
	step := server.config.RecommendationStep
	window, err := parseWindow(r.URL.Query().Get("window"), server.config.RecommendationWindow, step, time.Hour)
	if err != nil {
		respondError(w, r, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	if !server.authorize(w, r, AccessAttributes{Verb: "get", Resource: "metrics-history", Namespace: subject.Namespace}) {
		return
	}
	reporter, ok := server.usage.(attributionReporter)
	if server.usage == nil || !ok {
		respondError(w, r, http.StatusServiceUnavailable, "integration_not_configured", "Prometheus is not configured, so usage attribution cannot be inspected")
		return
	}
	spec, container, ok := server.resolveWorkload(w, r, subject)
	if !ok {
		return
	}
	if spec.MetricIdentity == nil {
		respondError(w, r, http.StatusServiceUnavailable, "integration_not_configured", "the Kubernetes inventory did not supply the workload's UID chain")
		return
	}
	identity := spec.MetricIdentity.ForContainer(container.Name)
	chain := attributionIdentity{
		Kind: spec.Kind, Name: spec.Name, Namespace: spec.Namespace, UID: identity.UID, CreatedAt: identity.CreatedAt,
		ExpectedReplicas: identity.ExpectedReplicas, Owners: identity.Owners, Pods: identity.Pods, ResolutionIssues: identity.ResolutionIssues,
	}
	if chain.Owners == nil {
		chain.Owners = []usage.OwnerIdentity{}
	}
	if chain.ResolutionIssues == nil {
		chain.ResolutionIssues = []string{}
	}
	end := server.now().UTC().Truncate(time.Second)
	report, err := reporter.AttributionReport(r.Context(), subject, identity, usage.Window{Duration: window, Step: step, End: end})
	switch {
	case err == nil:
	case errors.Is(err, usage.ErrInsufficientHistory):
		report = usage.AttributionReport{Subject: subject, Start: end.Add(-window), End: end, Step: step.String(), WorkloadUID: identity.UID, WorkloadCreatedAt: identity.CreatedAt,
			ExpectedReplicas: identity.ExpectedReplicas, Owners: []usage.OwnerAttribution{}, Pods: []usage.PodAttribution{}, Families: []usage.FamilyStatus{}, MissingFamilies: []string{},
			Outcome: usage.OutcomeIncompleteHistory, Reason: err.Error()}
	case errors.Is(err, usage.ErrAttributionIncomplete), errors.Is(err, usage.ErrInvalidSubject):
		// The identity itself is unusable (for example a pod without a UID);
		// that is the diagnosis.
		report = usage.AttributionReport{Subject: subject, Start: end.Add(-window), End: end, Step: step.String(), WorkloadUID: identity.UID, WorkloadCreatedAt: identity.CreatedAt,
			ExpectedReplicas: identity.ExpectedReplicas, Owners: []usage.OwnerAttribution{}, Pods: []usage.PodAttribution{}, Families: []usage.FamilyStatus{}, MissingFamilies: []string{},
			Outcome: usage.OutcomeIncompleteAttribution, Reason: err.Error()}
	default:
		server.usageFailure(w, r, err)
		return
	}
	respondJSON(w, http.StatusOK, map[string]any{"identity": chain, "report": report, "window": window.String(), "step": step.String()})
}
