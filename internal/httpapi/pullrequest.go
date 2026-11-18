package httpapi

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/kuberight/kuberight/internal/gitops"
	"github.com/kuberight/kuberight/internal/repository"
)

// Workload annotations that point KubeRight at the manifest in Git.
const (
	GitOpsPathAnnotation       = "kuberight.io/gitops-path"
	GitOpsRepositoryAnnotation = "kuberight.io/gitops-repository"
	GitOpsBranchAnnotation     = "kuberight.io/gitops-base-branch"
	// GitOpsValuesKeyAnnotation makes the path a Helm values file and names
	// the dotted key of the container's resources in it, such as
	// "api.resources".
	GitOpsValuesKeyAnnotation = "kuberight.io/gitops-values-key"
)

// openPullRequest proposes an approved recommendation as a pull request
// that edits the workload's manifest, for clusters where a GitOps
// controller owns the live object.
func (server *apiServer) openPullRequest(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	record, err := server.repository.GetRecommendation(r.Context(), id)
	if errors.Is(err, repository.ErrNotFound) {
		respondError(w, r, http.StatusNotFound, "not_found", "recommendation was not found")
		return
	}
	if err != nil {
		server.repositoryFailure(w, r, "get recommendation", err)
		return
	}
	if !server.authorize(w, r, AccessAttributes{Verb: "create", Resource: "recommendations", Subresource: "pullrequest", Namespace: record.Namespace, Name: id}) {
		return
	}
	if server.gitops == nil {
		respondError(w, r, http.StatusServiceUnavailable, "gitops_not_configured", "pull requests are not configured; set gitops.provider and a token")
		return
	}
	if record.ReviewStatus != repository.ReviewApproved {
		respondError(w, r, http.StatusConflict, "recommendation_not_approved", "only an approved recommendation can be proposed as a pull request")
		return
	}
	if record.Kind == "" {
		respondError(w, r, http.StatusConflict, "workload_unknown", "the recommendation does not identify a workload kind")
		return
	}
	resolver, ok := server.provider.(WorkloadResolver)
	if !ok {
		respondError(w, r, http.StatusServiceUnavailable, "inventory_unavailable", "the workload cannot be read")
		return
	}
	spec, err := resolver.Workload(r.Context(), record.Namespace, record.Kind, record.Workload)
	if errors.Is(err, ErrWorkloadNotFound) {
		respondError(w, r, http.StatusNotFound, "workload_not_found", "the workload no longer exists")
		return
	}
	if err != nil {
		server.logger.Warn("workload lookup for pull request failed", "request_id", requestIDFromContext(r.Context()), "error", err)
		respondError(w, r, http.StatusServiceUnavailable, "inventory_unavailable", "the workload cannot be read")
		return
	}
	if staleReason, freshnessErr := server.recommendationStaleness(r.Context(), record); freshnessErr != nil {
		respondError(w, r, http.StatusServiceUnavailable, "freshness_check_unavailable", "the live workload could not be verified; retry")
		return
	} else if staleReason != "" {
		respondError(w, r, http.StatusConflict, "recommendation_stale", staleReason+"; generate a new recommendation")
		return
	}
	path := strings.TrimSpace(spec.Annotations[GitOpsPathAnnotation])
	repositoryName := firstNonEmpty(spec.Annotations[GitOpsRepositoryAnnotation], server.config.GitOpsRepository)
	baseBranch := firstNonEmpty(spec.Annotations[GitOpsBranchAnnotation], server.config.GitOpsBaseBranch, "main")
	branch := "kuberight/" + record.ID
	if path == "" || repositoryName == "" {
		respondError(w, r, http.StatusConflict, "gitops_target_missing",
			fmt.Sprintf("annotate the workload with %s (and %s unless a default repository is configured)", GitOpsPathAnnotation, GitOpsRepositoryAnnotation))
		return
	}
	if err := gitops.ValidateTarget(repositoryName, path, baseBranch); err != nil {
		respondError(w, r, http.StatusBadRequest, "invalid_gitops_target", err.Error())
		return
	}
	valuesKey := strings.TrimSpace(spec.Annotations[GitOpsValuesKeyAnnotation])
	if valuesKey != "" {
		if err := gitops.ValidateValuesKey(valuesKey); err != nil {
			respondError(w, r, http.StatusBadRequest, "invalid_gitops_target", err.Error())
			return
		}
	}
	target := gitops.Target{Namespace: record.Namespace, Kind: record.Kind, Workload: record.Workload, Container: record.Container}
	desired := gitops.Resources{
		CPURequestMilli: record.CPURequestMilli, CPULimitMilli: record.CPULimitMilli,
		MemoryRequestBytes: record.MemoryRequestBytes, MemoryLimitBytes: record.MemoryLimitBytes,
		RemoveCPULimit: record.CPULimitMilli == 0 && record.CurrentCPULimitMilli > 0,
	}
	principal, _ := principalFromContext(r.Context())
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	result, err := server.gitops.OpenPullRequest(ctx, gitops.Request{
		Repository: repositoryName, Path: path, BaseBranch: baseBranch, Branch: branch,
		Title:         fmt.Sprintf("Rightsize %s/%s container %s", record.Kind, record.Workload, record.Container),
		Body:          pullRequestBody(record, principal.Username),
		CommitMessage: fmt.Sprintf("Rightsize %s/%s %s\n\nKubeRight recommendation %s, approved by %s.", record.Kind, record.Workload, record.Container, record.ID, record.ReviewedBy),
		Update: func(content []byte) ([]byte, error) {
			if valuesKey != "" {
				return gitops.UpdateValues(content, valuesKey, desired)
			}
			return gitops.UpdateManifest(content, target, desired)
		},
	})
	audit := server.auditEvent(r, principal, "gitops.pull_request", "recommendations/"+id, record.Namespace)
	if err != nil {
		audit.Outcome = "failure"
	}
	if auditErr := server.repository.AppendAuditEvent(r.Context(), audit); auditErr != nil {
		server.logger.Error("pull request audit event could not be stored", "recommendation_id", id, "error", auditErr)
	}
	var providerErr *gitops.ProviderError
	switch {
	case err == nil:
		status := http.StatusCreated
		if result.Existing {
			status = http.StatusOK
		}
		respondJSON(w, status, map[string]any{
			"url": result.URL, "number": result.Number, "branch": result.Branch, "existing": result.Existing,
			"repository": repositoryName, "path": path, "valuesKey": valuesKey, "baseBranch": baseBranch, "provider": server.gitops.Name(),
		})
	case errors.Is(err, gitops.ErrNoChange):
		respondError(w, r, http.StatusConflict, "manifest_current", "the manifest in Git already has the recommended resources")
	case errors.Is(err, gitops.ErrTargetNotFound) && valuesKey != "":
		respondError(w, r, http.StatusUnprocessableEntity, "manifest_target_missing",
			fmt.Sprintf("%s in %s has no mapping for values key %s", path, repositoryName, valuesKey))
	case errors.Is(err, gitops.ErrTargetNotFound):
		respondError(w, r, http.StatusUnprocessableEntity, "manifest_target_missing",
			fmt.Sprintf("%s in %s does not define %s/%s container %s", path, repositoryName, record.Kind, record.Workload, record.Container))
	case errors.Is(err, gitops.ErrUnsupportedLayout):
		respondError(w, r, http.StatusUnprocessableEntity, "manifest_layout_unsupported", err.Error()+"; apply the exported patch instead")
	case errors.As(err, &providerErr):
		server.logger.Warn("Git provider rejected the pull request", "request_id", requestIDFromContext(r.Context()), "operation", providerErr.Operation, "status", providerErr.Status)
		respondError(w, r, http.StatusBadGateway, "gitops_provider_error", truncate(providerErr.Error(), 300))
	default:
		server.logger.Warn("pull request failed", "request_id", requestIDFromContext(r.Context()), "error", err)
		respondError(w, r, http.StatusBadGateway, "gitops_unavailable", "the Git provider could not be reached")
	}
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			return trimmed
		}
	}
	return ""
}

func pullRequestBody(record repository.RecommendationRecord, requester string) string {
	quantity := func(cpu bool, value int64) string {
		if value <= 0 {
			return "unset"
		}
		if cpu {
			return gitops.FormatCPU(value)
		}
		return gitops.FormatMemory(value)
	}
	var body strings.Builder
	fmt.Fprintf(&body, "KubeRight recommendation `%s` for `%s/%s` container `%s` in namespace `%s`.\n\n", record.ID, record.Kind, record.Workload, record.Container, record.Namespace)
	body.WriteString("| Resource | Current | Recommended |\n| --- | ---: | ---: |\n")
	fmt.Fprintf(&body, "| CPU request | %s | %s |\n", quantity(true, record.CurrentCPURequestMilli), quantity(true, record.CPURequestMilli))
	fmt.Fprintf(&body, "| CPU limit | %s | %s |\n", quantity(true, record.CurrentCPULimitMilli), quantity(true, record.CPULimitMilli))
	fmt.Fprintf(&body, "| Memory request | %s | %s |\n", quantity(false, record.CurrentMemoryRequestBytes), quantity(false, record.MemoryRequestBytes))
	fmt.Fprintf(&body, "| Memory limit | %s | %s |\n\n", quantity(false, record.CurrentMemoryLimitBytes), quantity(false, record.MemoryLimitBytes))
	fmt.Fprintf(&body, "Confidence: %s. Risk: %s. Approved by %s; pull request requested by %s.\n", record.Confidence, record.Risk, record.ReviewedBy, requester)
	if record.Assessment != nil && len(record.Assessment.StatusReasons) > 0 {
		fmt.Fprintf(&body, "\nAssessment: %s.\n", strings.Join(record.Assessment.StatusReasons, "; "))
	}
	body.WriteString("\nAfter the change is applied, KubeRight verifies it against OOM kills, restarts, throttling, and latency.\n")
	return body.String()
}

// gitOpsStatus reports how approved recommendations reach Git.
func (server *apiServer) gitOpsStatus() dependencyStatus {
	if server.gitops == nil {
		return dependencyStatus{Name: "GitOps", Configured: true, Healthy: true, Detail: "export approved recommendations as strategic-merge patches for Argo CD, Flux, or kubectl; set gitops.provider to open pull requests"}
	}
	detail := "approved recommendations open " + server.gitops.Name() + " pull requests against the manifest named by " + GitOpsPathAnnotation
	if server.config.GitOpsRepository != "" {
		detail += " (default repository " + server.config.GitOpsRepository + ")"
	}
	return dependencyStatus{Name: "GitOps", Configured: true, Healthy: true, Detail: detail}
}
