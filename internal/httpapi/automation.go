package httpapi

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
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

// InPlaceResizer resizes the running pods of a workload whose update
// strategy leaves them alone after a template change. It is only used when
// automation.inPlaceResize is enabled, where the chart grants pods/resize.
type InPlaceResizer interface {
	ResizeInPlace(ctx context.Context, change automation.Change) (automation.ResizeResult, error)
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
	// InPlace reports the in-place resize of running pods, when enabled.
	InPlace *automation.ResizeResult `json:"inPlace,omitempty"`
	change  *automation.Change
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

// automationLockName is the repository lock that makes an applying pass a
// singleton across API replicas. Plans (dry runs) never take it.
const automationLockName = "automation"

// errAutomationBusy means another replica or request holds the pass lock.
var errAutomationBusy = errors.New("another automation pass is running")

// automationMetrics are process-wide, because the background automator and
// the HTTP server are separate server instances in one process. Every action
// and outcome pair starts at zero: a counter series that first appears at 1
// hides that change from increase(), and the failure and rollback alerts
// would miss the first event.
var automationMetrics = struct {
	sync.Mutex
	changes  map[string]uint64
	lastPass time.Time
	eligible int
	busy     uint64
}{changes: map[string]uint64{
	AutomationApply + "\x00" + AutomationApplied:       0,
	AutomationApply + "\x00" + AutomationDeferred:      0,
	AutomationApply + "\x00" + AutomationFailed:        0,
	AutomationRollback + "\x00" + AutomationRolledBack: 0,
	AutomationRollback + "\x00" + AutomationDeferred:   0,
	AutomationRollback + "\x00" + AutomationFailed:     0,
}}

func recordAutomationPass(report AutomationReport) {
	automationMetrics.Lock()
	defer automationMetrics.Unlock()
	automationMetrics.lastPass, automationMetrics.eligible = report.EvaluatedAt, report.Eligible
	for _, decision := range report.Decisions {
		if decision.Action != AutomationSkip && decision.Outcome != "" && decision.Outcome != AutomationPlanned {
			automationMetrics.changes[decision.Action+"\x00"+decision.Outcome]++
		}
	}
}

// renderAutomationMetrics writes the automation metrics. interval is the
// configured pass interval in apply mode and zero otherwise; dashboards divide
// the time since the last pass by it.
func renderAutomationMetrics(w http.ResponseWriter, interval time.Duration) {
	if interval > 0 {
		_, _ = fmt.Fprintln(w, "# HELP kuberight_automation_interval_seconds Configured interval between applying passes.")
		_, _ = fmt.Fprintln(w, "# TYPE kuberight_automation_interval_seconds gauge")
		_, _ = fmt.Fprintf(w, "kuberight_automation_interval_seconds %s\n", strconv.FormatFloat(interval.Seconds(), 'f', -1, 64))
	}
	automationMetrics.Lock()
	defer automationMetrics.Unlock()
	_, _ = fmt.Fprintln(w, "# HELP kuberight_automation_changes_total Automated changes attempted by this process, by action and outcome.")
	_, _ = fmt.Fprintln(w, "# TYPE kuberight_automation_changes_total counter")
	keys := make([]string, 0, len(automationMetrics.changes))
	for key := range automationMetrics.changes {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		action, outcome, _ := strings.Cut(key, "\x00")
		_, _ = fmt.Fprintf(w, "kuberight_automation_changes_total{action=%q,outcome=%q} %d\n", action, outcome, automationMetrics.changes[key])
	}
	_, _ = fmt.Fprintln(w, "# HELP kuberight_automation_eligible_changes Eligible changes found by the last applying pass in this process.")
	_, _ = fmt.Fprintln(w, "# TYPE kuberight_automation_eligible_changes gauge")
	_, _ = fmt.Fprintf(w, "kuberight_automation_eligible_changes %d\n", automationMetrics.eligible)
	_, _ = fmt.Fprintln(w, "# HELP kuberight_automation_last_pass_timestamp_seconds Completion time of the last applying pass in this process; 0 before the first.")
	_, _ = fmt.Fprintln(w, "# TYPE kuberight_automation_last_pass_timestamp_seconds gauge")
	last := int64(0)
	if !automationMetrics.lastPass.IsZero() {
		last = automationMetrics.lastPass.Unix()
	}
	_, _ = fmt.Fprintf(w, "kuberight_automation_last_pass_timestamp_seconds %d\n", last)
	_, _ = fmt.Fprintln(w, "# HELP kuberight_automation_busy_total Applying passes skipped because another replica held the automation lock.")
	_, _ = fmt.Fprintln(w, "# TYPE kuberight_automation_busy_total counter")
	_, _ = fmt.Fprintf(w, "kuberight_automation_busy_total %d\n", automationMetrics.busy)
}

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
	return server.automationRun(ctx, dryRun, false)
}

// regressedRecommendations lists approved recommendations whose latest
// verification regressed, the only candidates of a rollback pass.
func (server *apiServer) regressedRecommendations(ctx context.Context) ([]repository.RecommendationRecord, error) {
	records := []repository.RecommendationRecord{}
	for page := 1; len(records) < maxAutomationCandidates; page++ {
		result, err := server.repository.ListVerificationOutcomes(ctx, page, 100)
		if err != nil {
			return nil, err
		}
		for _, outcome := range result.Items {
			if outcome.Status != VerificationRegressed || len(records) >= maxAutomationCandidates {
				continue
			}
			if record, getErr := server.repository.GetRecommendation(ctx, outcome.RecommendationID); getErr == nil && record.ReviewStatus == repository.ReviewApproved {
				records = append(records, record)
			}
		}
		if page*100 >= result.Total {
			break
		}
	}
	return records, nil
}

// automationRun evaluates approved recommendations and executes the
// eligible decisions. With rollbacksOnly it evaluates only regressed ones and
// executes only rollbacks.
func (server *apiServer) automationRun(ctx context.Context, dryRun, rollbacksOnly bool) (AutomationReport, error) {
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
	// Applying passes are singletons across replicas: the lock lives in the
	// shared database (or the process, for the in-memory store).
	if !report.DryRun {
		locker, ok := server.repository.(repository.Locker)
		if !ok {
			return report, errors.New("the recommendation store cannot coordinate automation across replicas")
		}
		release, acquired, lockErr := locker.TryLock(ctx, automationLockName)
		if lockErr != nil {
			return report, lockErr
		}
		if !acquired {
			automationMetrics.Lock()
			automationMetrics.busy++
			automationMetrics.Unlock()
			return report, errAutomationBusy
		}
		defer release()
		defer func() { recordAutomationPass(report) }()
	}
	patcher, _ := server.provider.(ResourcePatcher)
	if rollbacksOnly {
		records, listErr := server.regressedRecommendations(ctx)
		if listErr != nil {
			return report, listErr
		}
		for _, record := range records {
			if ctx.Err() != nil {
				break
			}
			report.Evaluated++
			if decision := server.evaluateAutomation(ctx, record, patcher); decision.Action == AutomationRollback {
				report.Decisions = append(report.Decisions, decision)
			}
		}
	}
	for page := 1; !rollbacksOnly && report.Evaluated < maxAutomationCandidates; page++ {
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
		case decision.Action == AutomationRollback:
			// A rollback restores the exact values that ran before a
			// detected regression, so neither the change window nor the
			// per-run limit defers it.
			server.executeAutomation(ctx, decision, patcher, &report)
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
		server.resizeInPlace(ctx, decision, patcher)
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

// resizeInPlace follows a successful template patch. A resize that fails or
// does not apply never fails the change: the template already carries the
// new resources, and the pods take them when their owner recreates them.
func (server *apiServer) resizeInPlace(ctx context.Context, decision *AutomationDecision, patcher ResourcePatcher) {
	resizer, ok := patcher.(InPlaceResizer)
	if !ok || !server.config.AutomationInPlaceResize {
		return
	}
	result, err := resizer.ResizeInPlace(ctx, *decision.change)
	if err != nil {
		result = automation.ResizeResult{Failed: 1, Error: err.Error()}
	}
	decision.InPlace = &result
	switch {
	case result.Failed > 0:
		decision.Reasons = append(decision.Reasons, fmt.Sprintf("in-place resize reached %d of %d running pods; the rest take the new resources when recreated", result.Resized, result.Pods))
		server.logger.Warn("in-place resize incomplete", "recommendation_id", decision.RecommendationID, "pods", result.Pods, "resized", result.Resized, "error", result.Error)
	case result.Attempted:
		decision.Reasons = append(decision.Reasons, fmt.Sprintf("resized %d running pods in place", result.Resized))
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
	if errors.Is(err, errAutomationBusy) {
		respondError(w, r, http.StatusConflict, "automation_busy", "another automation pass is running; retry after it completes")
		return
	}
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
	cfg := automator.server.config.Normalized()
	ticker := time.NewTicker(cfg.AutomationInterval)
	defer ticker.Stop()
	// Regressions found by early verification are rolled back on the early
	// cadence instead of waiting for the next full pass.
	var rollbacks <-chan time.Time
	if every := cfg.VerificationEarlyInterval; every > 0 && every < cfg.AutomationInterval {
		early := time.NewTicker(every)
		defer early.Stop()
		rollbacks = early.C
	}
	run := func(rollbacksOnly bool) {
		report, err := automator.server.automationRun(ctx, false, rollbacksOnly)
		if errors.Is(err, errAutomationBusy) {
			automator.server.logger.Debug("automation pass skipped; another replica holds the lock")
		} else if err != nil {
			automator.server.logger.Warn("automation pass failed", "error", err)
		} else if report.Applied+report.RolledBack+report.Failed > 0 {
			automator.server.logger.Info("automation pass completed", "applied", report.Applied, "rolled_back", report.RolledBack, "failed", report.Failed, "deferred", report.Deferred, "rollbacks_only", rollbacksOnly)
		}
	}
	run(false)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			run(false)
		case <-rollbacks:
			run(true)
		}
	}
}
