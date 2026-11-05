package httpapi

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"sync"
	"time"

	"github.com/kuberight/kuberight/internal/automation"
	"github.com/kuberight/kuberight/internal/config"
	"github.com/kuberight/kuberight/internal/notify"
	"github.com/kuberight/kuberight/internal/repository"
)

// ResourcePatcher is the write side of the Kubernetes provider. It is only
// used in apply mode, where the chart grants patch permission on Deployments,
// StatefulSets, and DaemonSets.
type ResourcePatcher interface {
	NamespaceAutomationEnabled(ctx context.Context, namespace string) (bool, error)
	PatchResources(ctx context.Context, change automation.Change) error
}

// Automation actions and outcomes.
const (
	AutomationApply    = "apply"
	AutomationRollback = "rollback"
	AutomationSkip     = "skip"

	AutomationPlanned    = "planned"
	AutomationApplied    = "applied"
	AutomationRolledBack = "rolled_back"
	AutomationDeferred   = "deferred"
	AutomationFailed     = "failed"

	automationActor         = "kuberight-automation"
	maxAutomationCandidates = 500
)

// AutomationDecision is the evaluation of one approved recommendation.
type AutomationDecision struct {
	RecommendationID string               `json:"recommendationId"`
	Cluster          string               `json:"cluster,omitempty"`
	Namespace        string               `json:"namespace"`
	Kind             string               `json:"kind"`
	Workload         string               `json:"workload"`
	Container        string               `json:"container"`
	Action           string               `json:"action"`
	Reasons          []string             `json:"reasons"`
	Current          automation.Resources `json:"current"`
	Desired          automation.Resources `json:"desired"`
	Outcome          string               `json:"outcome,omitempty"`
	Error            string               `json:"error,omitempty"`
	change           *automation.Change
}

// AutomationReport summarizes one evaluation pass.
type AutomationReport struct {
	Mode             string               `json:"mode"`
	Enabled          bool                 `json:"enabled"`
	DryRun           bool                 `json:"dryRun"`
	Window           string               `json:"window,omitempty"`
	WindowOpen       bool                 `json:"windowOpen"`
	MaxChangesPerRun int                  `json:"maxChangesPerRun"`
	EvaluatedAt      time.Time            `json:"evaluatedAt"`
	Evaluated        int                  `json:"evaluated"`
	Eligible         int                  `json:"eligible"`
	Applied          int                  `json:"applied"`
	RolledBack       int                  `json:"rolledBack"`
	Deferred         int                  `json:"deferred"`
	Failed           int                  `json:"failed"`
	Decisions        []AutomationDecision `json:"decisions"`
}

// automationMutex serializes passes inside one process. Across replicas the
// resourceVersion precondition makes a second, stale patch fail instead of
// applying twice.
var automationMutex sync.Mutex

func liveResources(container ContainerSpec) automation.Resources {
	return automation.Resources{CPURequestMilli: container.CPURequestMilli, CPULimitMilli: container.CPULimitMilli, MemoryRequestBytes: container.MemoryRequestBytes, MemoryLimitBytes: container.MemoryLimitBytes}
}

func resourcesMatch(live, target automation.Resources) bool {
	return matches(live.CPURequestMilli, target.CPURequestMilli) && matches(live.CPULimitMilli, target.CPULimitMilli) &&
		matches(live.MemoryRequestBytes, target.MemoryRequestBytes) && matches(live.MemoryLimitBytes, target.MemoryLimitBytes)
}

// evaluateAutomation decides whether an approved recommendation may be
// applied now, or an automated change rolled back. Every reason it records
// is a reason not to act.
func (server *apiServer) evaluateAutomation(ctx context.Context, record repository.RecommendationRecord, patcher ResourcePatcher) AutomationDecision {
	decision := AutomationDecision{
		RecommendationID: record.ID, Cluster: record.Cluster, Namespace: record.Namespace, Kind: record.Kind, Workload: record.Workload, Container: record.Container,
		Action: AutomationSkip, Reasons: []string{},
		Desired: automation.Resources{CPURequestMilli: record.CPURequestMilli, CPULimitMilli: record.CPULimitMilli, MemoryRequestBytes: record.MemoryRequestBytes, MemoryLimitBytes: record.MemoryLimitBytes},
	}
	previous := automation.Resources{CPURequestMilli: record.CurrentCPURequestMilli, CPULimitMilli: record.CurrentCPULimitMilli, MemoryRequestBytes: record.CurrentMemoryRequestBytes, MemoryLimitBytes: record.CurrentMemoryLimitBytes}
	reason := func(format string, values ...any) {
		decision.Reasons = append(decision.Reasons, fmt.Sprintf(format, values...))
	}
	if !automation.Supported(record.Kind) {
		reason("%s workloads are advisory only", record.Kind)
		return decision
	}
	resolver, ok := server.provider.(WorkloadResolver)
	if !ok || patcher == nil {
		reason("the Kubernetes provider cannot apply changes")
		return decision
	}
	spec, err := resolver.Workload(ctx, record.Namespace, record.Kind, record.Workload)
	if err != nil {
		reason("the live workload could not be read")
		return decision
	}
	container, found := spec.Container(record.Container)
	if !found {
		reason("the container no longer exists")
		return decision
	}
	decision.Current = liveResources(container)
	enabled, err := patcher.NamespaceAutomationEnabled(ctx, record.Namespace)
	switch {
	case err != nil:
		reason("the namespace opt-in could not be read")
	case !enabled:
		reason("namespace %s is not labeled %s=enabled", record.Namespace, automation.NamespaceLabel)
	}
	if spec.Annotations[automation.WorkloadAnnotation] == "disabled" {
		reason("the workload is annotated %s=disabled", automation.WorkloadAnnotation)
	}
	if manager := automation.GitOpsManager(spec.Labels, spec.Annotations); manager != "" && spec.Annotations[automation.GitOpsAnnotation] != "allow" {
		reason("%s manages the workload and would revert the change; change its source or annotate %s=allow", manager, automation.GitOpsAnnotation)
	}
	if spec.UID != record.Provenance.WorkloadUID || (record.Cluster != "" && spec.Cluster != record.Cluster) {
		reason("the workload was recreated or moved after the recommendation")
	}
	if container.VPAControlsCPU || container.VPAControlsMemory {
		reason("a VerticalPodAutoscaler manages this container")
	}

	// A regressed automated change is rolled back to the exact values it
	// replaced, provided nothing else changed them since.
	outcome, outcomeErr := server.repository.GetVerificationOutcome(ctx, record.ID)
	if outcomeErr == nil && outcome.Status == VerificationRegressed && spec.Annotations[automation.AppliedAnnotation] == record.ID {
		if !resourcesMatch(decision.Current, decision.Desired) {
			reason("the regressed change was already modified by someone else")
			return decision
		}
		if len(decision.Reasons) == 0 {
			decision.Action = AutomationRollback
			decision.Desired = previous
			decision.Reasons = append(decision.Reasons, "post-change verification regressed: "+joinLimited(outcome.Reasons, 2))
			decision.change = &automation.Change{Namespace: record.Namespace, Kind: record.Kind, Workload: record.Workload, Container: record.Container, ResourceVersion: spec.ResourceVersion, RecommendationID: record.ID, Desired: previous, Previous: decision.Current, Rollback: true}
		}
		return decision
	}

	if record.ReviewStatus != repository.ReviewApproved {
		reason("the recommendation is not approved")
	}
	if record.Source != repository.SourcePrometheus {
		reason("only Prometheus-backed recommendations are applied automatically")
	}
	if approvable, why := record.Approvable(); !approvable {
		reason("%s", why)
	}
	now := server.now()
	if record.Provenance.ExpiresAt == nil || record.Expired(now) {
		reason("the recommendation evidence has expired; generate a fresh recommendation")
	}
	if record.DataQuality.ReplicaCoverage == nil || !record.DataQuality.ReplicaCoverage.Complete {
		reason("the evidence did not cover every replica")
	}
	if spec.Generation != record.Provenance.WorkloadGeneration || spec.SourceSpecFingerprint != record.Provenance.SourceSpecFingerprint {
		reason("the workload spec changed after the recommendation was calculated")
	}
	if resourcesMatch(decision.Current, decision.Desired) {
		reason("the recommended values are already live")
	} else if !resourcesMatch(decision.Current, previous) {
		reason("the live requests and limits differ from the values the recommendation was calculated against")
	}
	if len(decision.Reasons) == 0 {
		decision.Action = AutomationApply
		decision.change = &automation.Change{Namespace: record.Namespace, Kind: record.Kind, Workload: record.Workload, Container: record.Container, ResourceVersion: spec.ResourceVersion, RecommendationID: record.ID, Desired: decision.Desired, Previous: decision.Current}
	}
	return decision
}

// automationPass evaluates the latest approved recommendation of every
// container. With dryRun it only plans. Otherwise it applies eligible
// changes, rollbacks first, up to the per-run limit inside the change window.
func (server *apiServer) automationPass(ctx context.Context, dryRun bool) (AutomationReport, error) {
	automationMutex.Lock()
	defer automationMutex.Unlock()
	cfg := server.config.Normalized()
	window, err := automation.ParseWindow(cfg.AutomationWindow)
	if err != nil {
		return AutomationReport{}, err
	}
	now := server.now().UTC()
	report := AutomationReport{
		Mode: cfg.OperationMode, Enabled: cfg.OperationMode == config.ModeApply, DryRun: dryRun || cfg.OperationMode != config.ModeApply,
		Window: window.String(), WindowOpen: window.Open(now), MaxChangesPerRun: cfg.AutomationMaxChanges, EvaluatedAt: now, Decisions: []AutomationDecision{},
	}
	patcher, _ := server.provider.(ResourcePatcher)
	for page := 1; report.Evaluated < maxAutomationCandidates; page++ {
		result, listErr := server.repository.ListRecommendations(ctx, repository.ListOptions{Page: page, PageSize: 100, ReviewStatus: repository.ReviewApproved, LatestOnly: true, Sort: "createdAt", Order: "desc"})
		if listErr != nil {
			return report, listErr
		}
		for _, record := range result.Items {
			if ctx.Err() != nil || report.Evaluated >= maxAutomationCandidates {
				break
			}
			report.Decisions = append(report.Decisions, server.evaluateAutomation(ctx, record, patcher))
			report.Evaluated++
		}
		if len(result.Items) < 100 {
			break
		}
	}
	// Rollbacks restore safety, so they are never starved by new changes.
	sort.SliceStable(report.Decisions, func(left, right int) bool {
		rank := map[string]int{AutomationRollback: 0, AutomationApply: 1, AutomationSkip: 2}
		return rank[report.Decisions[left].Action] < rank[report.Decisions[right].Action]
	})
	changes := 0
	for index := range report.Decisions {
		decision := &report.Decisions[index]
		if decision.Action == AutomationSkip {
			continue
		}
		report.Eligible++
		switch {
		case report.DryRun:
			decision.Outcome = AutomationPlanned
		case !report.WindowOpen:
			decision.Outcome = AutomationDeferred
			decision.Reasons = append(decision.Reasons, "outside the change window "+report.Window)
			report.Deferred++
		case changes >= cfg.AutomationMaxChanges:
			decision.Outcome = AutomationDeferred
			decision.Reasons = append(decision.Reasons, "the per-run change limit was reached")
			report.Deferred++
		default:
			changes++
			server.executeAutomation(ctx, decision, patcher, &report)
		}
	}
	return report, nil
}

func (server *apiServer) executeAutomation(ctx context.Context, decision *AutomationDecision, patcher ResourcePatcher, report *AutomationReport) {
	action, event, outcome := "automation.apply", notify.RecommendationApplied, AutomationApplied
	if decision.Action == AutomationRollback {
		action, event, outcome = "automation.rollback", notify.RecommendationRolledBack, AutomationRolledBack
	}
	err := patcher.PatchResources(ctx, *decision.change)
	audit := repository.AuditEvent{Timestamp: server.now().UTC(), Actor: automationActor, Action: action, Resource: "recommendations/" + decision.RecommendationID, Namespace: decision.Namespace, Outcome: "success", RequestID: "automation/" + decision.RecommendationID}
	if err != nil {
		decision.Outcome, decision.Error = AutomationFailed, err.Error()
		report.Failed++
		audit.Outcome = "failure"
		server.logger.Warn("automated change failed", "recommendation_id", decision.RecommendationID, "action", decision.Action, "error", err)
	} else {
		decision.Outcome = outcome
		if decision.Action == AutomationRollback {
			report.RolledBack++
		} else {
			report.Applied++
		}
		server.logger.Info("automated change applied", "recommendation_id", decision.RecommendationID, "action", decision.Action, "namespace", decision.Namespace, "workload", decision.Workload, "container", decision.Container)
	}
	if auditErr := server.repository.AppendAuditEvent(ctx, audit); auditErr != nil {
		server.logger.Error("automation audit event could not be stored", "recommendation_id", decision.RecommendationID, "error", auditErr)
	}
	if err == nil {
		if record, getErr := server.repository.GetRecommendation(ctx, decision.RecommendationID); getErr == nil {
			server.notify(event, record, automationActor, joinLimited(decision.Reasons, 2))
		}
	}
}

func (server *apiServer) automationStatus(w http.ResponseWriter, r *http.Request) {
	if !server.authorize(w, r, AccessAttributes{Verb: "list", Resource: "automation"}) {
		return
	}
	report, err := server.automationPass(r.Context(), true)
	if err != nil {
		server.repositoryFailure(w, r, "plan automation", err)
		return
	}
	respondJSON(w, http.StatusOK, report)
}

func (server *apiServer) runAutomation(w http.ResponseWriter, r *http.Request) {
	if !server.authorize(w, r, AccessAttributes{Verb: "update", Resource: "automation"}) {
		return
	}
	if server.config.OperationMode != config.ModeApply {
		respondError(w, r, http.StatusConflict, "automation_disabled", "automated changes require operationMode=apply; GET /api/v1/automation shows the plan")
		return
	}
	report, err := server.automationPass(r.Context(), false)
	if err != nil {
		server.repositoryFailure(w, r, "run automation", err)
		return
	}
	respondJSON(w, http.StatusOK, report)
}

// Automator runs automation passes in the background in apply mode.
type Automator struct{ server *apiServer }

// NewAutomator fails unless apply mode and a write-capable provider exist.
func NewAutomator(cfg config.Config, dependencies Dependencies) (*Automator, error) {
	if cfg.OperationMode != config.ModeApply {
		return nil, errors.New("automation requires operationMode=apply")
	}
	server, err := newAPIServer(cfg, dependencies)
	if err != nil {
		return nil, err
	}
	if _, ok := server.provider.(ResourcePatcher); !ok {
		return nil, errors.New("automation requires the Kubernetes provider")
	}
	return &Automator{server: server}, nil
}

// Run evaluates on the configured interval until the context ends.
func (automator *Automator) Run(ctx context.Context) {
	ticker := time.NewTicker(automator.server.config.Normalized().AutomationInterval)
	defer ticker.Stop()
	for {
		report, err := automator.server.automationPass(ctx, false)
		if err != nil {
			automator.server.logger.Warn("automation pass failed", "error", err)
		} else if report.Applied+report.RolledBack+report.Failed > 0 {
			automator.server.logger.Info("automation pass completed", "applied", report.Applied, "rolled_back", report.RolledBack, "failed", report.Failed, "deferred", report.Deferred)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
