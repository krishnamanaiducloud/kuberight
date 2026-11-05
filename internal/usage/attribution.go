package usage

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/kuberight/kuberight/internal/metrics"
)

const (
	// minimumReplicaCoverage is the share of the requested window, and of each
	// live replica's own running time, that must carry samples.
	minimumReplicaCoverage = 0.80
	// minimumReplicaTimeCoverage is the share of expected replica-time, summed
	// over every observation step, that attributed samples must represent. A
	// workload with three expected replicas and one reporting replica has 33%.
	minimumReplicaTimeCoverage = 0.90
	// minimumPopulationHistoryCoverage is the share of active steps for which
	// kube-state-metrics recorded the desired replica count.
	minimumPopulationHistoryCoverage = 0.90
	// Steps in which a pod name maps to more than one UID, or usage matches the
	// workload's pod names but no attributable owner, are excluded. Small
	// shares are reported; larger shares make the evidence incomplete.
	maximumAmbiguousShare    = 0.05
	maximumUnattributedShare = 0.05
	maximumLivePods          = 10_000
	// LivePodsOnly queries embed every live UID, so they keep the previous
	// exact-query population bound.
	maximumLivePodsOnly    = 256
	maximumTimelineBuckets = 96
	maximumExpressionBytes = 8 << 10
)

// Attribution modes reported in replica coverage.
const (
	// AttributionHistorical reconstructs the workload's pod population for
	// every step from kube-state-metrics ownership and creation series, so
	// pods deleted during the window (old ReplicaSets, scale-downs, replaced
	// StatefulSet pods, finished Jobs) are included.
	AttributionHistorical = "historical"
	// AttributionLivePods restricts evidence to the pods that exist now. It is
	// used for post-change verification, where earlier pods ran a different
	// configuration.
	AttributionLivePods = "live-pods"
)

var (
	// ErrAttributionIncomplete means metric samples cannot be proven to belong
	// to the current workload UID. Callers must not fall back to pod-name
	// inference because a recreated same-name workload could then be mixed in.
	ErrAttributionIncomplete = errors.New("workload metric attribution is incomplete")
	uidToken                 = regexp.MustCompile(`^[A-Za-z0-9_.:-]+$`)
)

// PodIdentity is immutable Kubernetes identity and lifecycle information for
// one live pod controlled by a workload.
type PodIdentity struct {
	Name       string     `json:"name"`
	UID        string     `json:"uid"`
	CreatedAt  time.Time  `json:"createdAt"`
	DeletedAt  *time.Time `json:"deletedAt,omitempty"`
	Phase      string     `json:"phase,omitempty"`
	Containers []string   `json:"containers,omitempty"`
}

// Active reports whether Kubernetes still considers the pod a possible
// workload replica. Pending pods count as active but will be reported as
// missing usage until they begin exporting metrics.
func (pod PodIdentity) Active() bool {
	return pod.DeletedAt == nil && pod.Phase != "Succeeded" && pod.Phase != "Failed"
}

func (pod PodIdentity) hasContainer(name string) bool {
	for _, container := range pod.Containers {
		if container == name {
			return true
		}
	}
	return false
}

// WorkloadIdentity is the exact, live Kubernetes identity of a workload.
// The workload UID and creation time bound historical attribution: only pods
// created by owners of this name after the current object was created can
// belong to it. Live pods cross-check the historical population.
// ResolutionIssues are fail-closed.
type WorkloadIdentity struct {
	UID              string        `json:"workloadUid"`
	Generation       int64         `json:"workloadGeneration,omitempty"`
	CreatedAt        time.Time     `json:"workloadCreatedAt"`
	ExpectedReplicas int32         `json:"expectedReplicas"`
	Pods             []PodIdentity `json:"pods"`
	ResolutionIssues []string      `json:"resolutionIssues,omitempty"`
	// LivePodsOnly restricts attribution to the UIDs in Pods.
	LivePodsOnly bool `json:"livePodsOnly,omitempty"`
}

// ForContainer retains only pods whose spec contains the requested
// container. Missing containers remain an explicit resolution issue.
func (identity WorkloadIdentity) ForContainer(container string) WorkloadIdentity {
	filtered := identity
	filtered.ResolutionIssues = append([]string(nil), identity.ResolutionIssues...)
	filtered.Pods = make([]PodIdentity, 0, len(identity.Pods))
	missing := 0
	for _, pod := range identity.Pods {
		if pod.hasContainer(container) {
			filtered.Pods = append(filtered.Pods, pod)
		} else if pod.Active() {
			missing++
		}
	}
	if missing > 0 {
		filtered.ResolutionIssues = append(filtered.ResolutionIssues,
			fmt.Sprintf("%d active workload replicas do not contain container %q", missing, container))
	}
	return filtered
}

// ReplicaObservation summarizes one attributed pod UID over the window.
// Full-resolution per-pod series stay in Prometheus; the workload series is
// the busiest replica at each step.
type ReplicaObservation struct {
	Pod   string `json:"pod"`
	UID   string `json:"uid"`
	Phase string `json:"phase,omitempty"`
	// Revision is the owning ReplicaSet or Job name, when it has one.
	Revision  string     `json:"revision,omitempty"`
	Live      bool       `json:"live"`
	Active    bool       `json:"active"`
	CreatedAt time.Time  `json:"createdAt,omitzero"`
	DeletedAt *time.Time `json:"deletedAt,omitempty"`
	// RunningSamples counts steps in which kube-state-metrics reported the pod
	// Running; CPU and memory coverage are relative to that lifetime.
	RunningSamples int     `json:"runningSamples"`
	CPUSamples     int     `json:"cpuSamples"`
	MemorySamples  int     `json:"memorySamples"`
	CPUCoverage    float64 `json:"cpuCoverage"`
	MemoryCoverage float64 `json:"memoryCoverage"`
	CPUP95Milli    float64 `json:"cpuP95Milli,omitempty"`
	MemoryP95Bytes float64 `json:"memoryP95Bytes,omitempty"`
}

// CoverageBucket is one slice of the coverage timeline. Replica counts are
// averages over the bucket's active steps.
type CoverageBucket struct {
	Start                  time.Time `json:"start"`
	End                    time.Time `json:"end"`
	ExpectedReplicas       float64   `json:"expectedReplicas"`
	CPUObservedReplicas    float64   `json:"cpuObservedReplicas"`
	MemoryObservedReplicas float64   `json:"memoryObservedReplicas"`
	// PopulationKnown is the share of active steps whose desired replica count
	// was recorded.
	PopulationKnown float64 `json:"populationKnown"`
	ActiveSteps     int     `json:"activeSteps"`
}

// ReplicaCoverage explains whether the workload's replica population is
// complete enough to justify a reduction. Coverage is measured per step: at
// each step the expected population is the larger of the recorded desired
// replicas and the attributed running pods, and is compared with the pods
// that reported samples in that step.
type ReplicaCoverage struct {
	Attribution    string     `json:"attribution"`
	EffectiveStart *time.Time `json:"effectiveStart,omitempty"`
	// ExpectedReplicas is the live desired replica count.
	ExpectedReplicas        int32 `json:"expectedReplicas"`
	PeakExpectedReplicas    int32 `json:"peakExpectedReplicas"`
	MinimumExpectedReplicas int32 `json:"minimumExpectedReplicas"`
	// DiscoveredReplicas counts every attributed pod UID in the window,
	// including pods that no longer exist.
	DiscoveredReplicas     int `json:"discoveredReplicas"`
	ObservedReplicas       int `json:"observedReplicas"`
	MissingReplicas        int `json:"missingReplicas"`
	ActiveReplicas         int `json:"activeReplicas"`
	TerminatedReplicas     int `json:"terminatedReplicas"`
	CPUObservedReplicas    int `json:"cpuObservedReplicas"`
	MemoryObservedReplicas int `json:"memoryObservedReplicas"`
	// Revisions counts distinct owning ReplicaSets or Jobs.
	Revisions     int `json:"revisions"`
	ScalingEvents int `json:"scalingEvents"`
	// CPUReplicaCoverage and MemoryReplicaCoverage are replica-time weighted:
	// the sum over steps of min(reporting, expected) / the sum of expected.
	CPUReplicaCoverage    float64 `json:"cpuReplicaCoverage"`
	MemoryReplicaCoverage float64 `json:"memoryReplicaCoverage"`
	// CPUTemporalCoverage and MemoryTemporalCoverage are the share of the
	// requested window, excluding scaled-to-zero steps, with any sample.
	CPUTemporalCoverage    float64 `json:"cpuTemporalCoverage"`
	MemoryTemporalCoverage float64 `json:"memoryTemporalCoverage"`
	// ExpectedReplicaHistoryCoverage is the share of active steps for which
	// the desired replica count is known.
	ExpectedReplicaHistoryCoverage float64 `json:"expectedReplicaHistoryCoverage"`
	// IdentityCoverage is the share of expected replica-time backed by a
	// running pod whose ownership was established.
	IdentityCoverage     float64              `json:"identityCoverage"`
	ActiveSteps          int                  `json:"activeSteps"`
	ScaledToZeroSteps    int                  `json:"scaledToZeroSteps"`
	AmbiguousSteps       int                  `json:"ambiguousSteps"`
	UnattributedSteps    int                  `json:"unattributedSteps"`
	CPUImbalanceRatio    float64              `json:"cpuImbalanceRatio,omitempty"`
	MemoryImbalanceRatio float64              `json:"memoryImbalanceRatio,omitempty"`
	Complete             bool                 `json:"complete"`
	Issues               []string             `json:"issues"`
	Timeline             []CoverageBucket     `json:"timeline,omitempty"`
	Observations         []ReplicaObservation `json:"observations,omitempty"`
}

// ReplicaEvents preserves restart, OOM, and throttling evidence by pod UID.
type ReplicaEvents struct {
	Pod                 string  `json:"pod"`
	UID                 string  `json:"uid"`
	Live                bool    `json:"live"`
	Restarts            int64   `json:"restarts"`
	OOMKills            int64   `json:"oomKills"`
	ThrottlingAvailable bool    `json:"throttlingAvailable"`
	ThrottlingRatio     float64 `json:"throttlingRatio"`
	ThrottlingPeakRatio float64 `json:"throttlingPeakRatio"`
}

func validateIdentity(identity WorkloadIdentity) error {
	if identity.UID == "" || len(identity.UID) > 128 || !uidToken.MatchString(identity.UID) {
		return fmt.Errorf("%w: the workload UID is missing or invalid", ErrAttributionIncomplete)
	}
	if identity.CreatedAt.IsZero() {
		return fmt.Errorf("%w: the workload creation timestamp is missing", ErrAttributionIncomplete)
	}
	if identity.ExpectedReplicas < 0 || identity.ExpectedReplicas > 100_000 {
		return fmt.Errorf("%w: expected replicas are outside the supported range", ErrAttributionIncomplete)
	}
	if len(identity.ResolutionIssues) > 0 {
		return fmt.Errorf("%w: %s", ErrAttributionIncomplete, strings.Join(identity.ResolutionIssues, "; "))
	}
	if len(identity.Pods) > maximumLivePods {
		return fmt.Errorf("%w: %d live pods exceed the supported population of %d", ErrAttributionIncomplete, len(identity.Pods), maximumLivePods)
	}
	if identity.LivePodsOnly {
		if len(identity.Pods) == 0 {
			return fmt.Errorf("%w: no live pod UIDs were resolved", ErrAttributionIncomplete)
		}
		if len(identity.Pods) > maximumLivePodsOnly {
			return fmt.Errorf("%w: %d pods exceed the live-pod query limit of %d", ErrAttributionIncomplete, len(identity.Pods), maximumLivePodsOnly)
		}
	}
	seenNames, seenUIDs := map[string]bool{}, map[string]bool{}
	for _, pod := range identity.Pods {
		if len(pod.Name) > 253 || !dnsSubdomain.MatchString(pod.Name) || pod.UID == "" || len(pod.UID) > 128 || !uidToken.MatchString(pod.UID) || pod.CreatedAt.IsZero() {
			return fmt.Errorf("%w: pod identity is missing a valid name, UID, or creation timestamp", ErrAttributionIncomplete)
		}
		if seenNames[pod.Name] || seenUIDs[pod.UID] {
			return fmt.Errorf("%w: duplicate pod name or UID", ErrAttributionIncomplete)
		}
		seenNames[pod.Name], seenUIDs[pod.UID] = true, true
	}
	return nil
}

// desiredReplicaMetrics are the kube-state-metrics series that record each
// controller's desired replica count over time. Jobs have no stable desired
// population; their running pods are the expected population.
var desiredReplicaMetrics = map[string][2]string{
	"Deployment":  {"kube_deployment_spec_replicas", "deployment"},
	"StatefulSet": {"kube_statefulset_replicas", "statefulset"},
	"DaemonSet":   {"kube_daemonset_status_desired_number_scheduled", "daemonset"},
	"ReplicaSet":  {"kube_replicaset_spec_replicas", "replicaset"},
}

func batchKind(kind string) bool { return kind == "Job" || kind == "CronJob" }

// attributionQueries are the PromQL building blocks for one identity. They
// depend on the step but not on the window position.
type attributionQueries struct {
	// owned has one series per (namespace, pod, uid, created_by_name) for each
	// step in which kube-state-metrics attributes the pod to the workload.
	owned string
	// unambiguous drops steps in which one pod name maps to two identities,
	// for example the overlap of a replaced StatefulSet pod.
	unambiguous string
	running     string
	selector    string
	cpuPerPod   string
	memoryPod   string
	desired     string
}

// ownershipExpression follows controller ownership recorded by
// kube-state-metrics rather than the live API, so deleted pods remain
// attributable. A same-name workload that existed earlier is excluded because
// every pod and intermediate owner must have been created at or after the
// current workload's creation.
func ownershipExpression(subject Subject, identity WorkloadIdentity) string {
	quote := strconv.Quote
	namespace := quote(subject.Namespace)
	created := strconv.FormatInt(identity.CreatedAt.Unix(), 10)
	pods := quote(podPattern(subject))
	info := "namespace=" + namespace
	if identity.LivePodsOnly {
		uids := make([]string, 0, len(identity.Pods))
		for _, pod := range identity.Pods {
			uids = append(uids, regexp.QuoteMeta(pod.UID))
		}
		sort.Strings(uids)
		info += ",uid=~" + quote(strings.Join(uids, "|"))
	}
	var owned string
	switch subject.Kind {
	case "Deployment", "CronJob":
		parentKind, parentLabel, ownerMetric, createdMetric := "ReplicaSet", "replicaset", "kube_replicaset_owner", "kube_replicaset_created"
		parents := quote(regexp.QuoteMeta(subject.Workload) + "-[a-z0-9]{1,10}")
		if subject.Kind == "CronJob" {
			parentKind, parentLabel, ownerMetric, createdMetric = "Job", "job_name", "kube_job_owner", "kube_job_created"
			parents = quote(regexp.QuoteMeta(subject.Workload) + "-[0-9]+")
		}
		owners := "label_replace(" + ownerMetric + "{namespace=" + namespace + "," + parentLabel + "=~" + parents +
			",owner_kind=" + quote(subject.Kind) + ",owner_name=" + quote(subject.Workload) + `,owner_is_controller="true"}` +
			" and on (namespace, " + parentLabel + ") (" + createdMetric + "{namespace=" + namespace + "," + parentLabel + "=~" + parents + "} >= " + created + ")" +
			`, "created_by_name", "$1", "` + parentLabel + `", "(.+)")`
		owned = "kube_pod_info{" + info + ",created_by_kind=" + quote(parentKind) + ",created_by_name=~" + parents + "}" +
			" * on (namespace, created_by_name) group_left () max by (namespace, created_by_name) (" + owners + ")"
	default:
		owned = "kube_pod_info{" + info + ",created_by_kind=" + quote(subject.Kind) + ",created_by_name=" + quote(subject.Workload) + "}"
	}
	return "max by (namespace, pod, uid, created_by_name) ((" + owned + ") and on (namespace, pod) (kube_pod_created{namespace=" + namespace + ",pod=~" + pods + "} >= " + created + "))"
}

func buildAttributionQueries(subject Subject, identity WorkloadIdentity, step time.Duration) (attributionQueries, error) {
	if err := subject.Validate(); err != nil {
		return attributionQueries{}, err
	}
	if err := validateIdentity(identity); err != nil {
		return attributionQueries{}, err
	}
	if step <= 0 {
		return attributionQueries{}, fmt.Errorf("%w: the observation step must be positive", ErrInvalidSubject)
	}
	quote := strconv.Quote
	queries := attributionQueries{owned: ownershipExpression(subject, identity), selector: subject.selector()}
	queries.unambiguous = "(" + queries.owned + " unless on (namespace, pod) (count by (namespace, pod) (" + queries.owned + ") > 1))"
	queries.running = "(" + queries.owned + " and on (namespace, pod) (kube_pod_status_phase{namespace=" + quote(subject.Namespace) + ",pod=~" + quote(podPattern(subject)) + `,phase="Running"} == 1))`
	queries.cpuPerPod = "max by (pod, uid) (max by (namespace, pod) (rate(container_cpu_usage_seconds_total{" + queries.selector + "}[" + rateWindow(step) + "])) * on (namespace, pod) group_left (uid) " + queries.unambiguous + ") * 1000"
	queries.memoryPod = "max by (pod, uid) (max by (namespace, pod) (max_over_time(container_memory_working_set_bytes{" + queries.selector + "}[" + promDuration(step) + "])) * on (namespace, pod) group_left (uid) " + queries.unambiguous + ")"
	if metric, ok := desiredReplicaMetrics[subject.Kind]; ok {
		queries.desired = "max(" + metric[0] + "{namespace=" + quote(subject.Namespace) + "," + metric[1] + "=" + quote(subject.Workload) + "})"
	}
	return queries, nil
}

// AttributedCPUExpression returns one CPU series per attributed pod UID, in
// millicores. Pods that no longer exist are included for the steps in which
// kube-state-metrics attributed them to the workload.
func AttributedCPUExpression(subject Subject, identity WorkloadIdentity, step time.Duration) (string, error) {
	queries, err := buildAttributionQueries(subject, identity, step)
	if err != nil {
		return "", err
	}
	return queries.cpuPerPod, checkExpressions(queries.cpuPerPod)
}

// AttributedMemoryExpression returns one working-set series per attributed
// pod UID.
func AttributedMemoryExpression(subject Subject, identity WorkloadIdentity, step time.Duration) (string, error) {
	queries, err := buildAttributionQueries(subject, identity, step)
	if err != nil {
		return "", err
	}
	return queries.memoryPod, checkExpressions(queries.memoryPod)
}

func checkExpressions(expressions ...string) error {
	for _, expression := range expressions {
		if len(expression) > maximumExpressionBytes {
			return fmt.Errorf("%w: the workload name is too long for a bounded attribution query", ErrAttributionIncomplete)
		}
	}
	return nil
}

// attributionPlan positions the queries in a window. Steps before the current
// workload existed are part of the requested window but cannot contain its
// evidence.
type attributionPlan struct {
	attributionQueries
	subject        Subject
	identity       WorkloadIdentity
	start, end     time.Time
	effectiveStart time.Time
	step           time.Duration
}

func newAttributionPlan(subject Subject, identity WorkloadIdentity, window Window) (attributionPlan, error) {
	if window.Step <= 0 || window.Duration <= 0 || window.End.IsZero() {
		return attributionPlan{}, fmt.Errorf("%w: the observation window is invalid", ErrInvalidSubject)
	}
	queries, err := buildAttributionQueries(subject, identity, window.Step)
	if err != nil {
		return attributionPlan{}, err
	}
	end := window.End.UTC().Truncate(time.Second)
	plan := attributionPlan{attributionQueries: queries, subject: subject, identity: identity, start: end.Add(-window.Duration), end: end, step: window.Step}
	if !end.After(identity.CreatedAt) {
		return attributionPlan{}, fmt.Errorf("%w: observation window ends before the current workload was created", ErrAttributionIncomplete)
	}
	plan.effectiveStart = plan.start
	if identity.CreatedAt.After(plan.start) {
		steps := int64(math.Ceil(float64(identity.CreatedAt.Sub(plan.start)) / float64(window.Step)))
		plan.effectiveStart = plan.start.Add(time.Duration(steps) * window.Step)
	}
	if plan.effectiveStart.After(end) {
		return attributionPlan{}, fmt.Errorf("%w: the current workload has existed for less than one observation step", ErrAttributionIncomplete)
	}
	return plan, nil
}

// subqueryRange covers the effective part of the window.
func (plan attributionPlan) subqueryRange() string {
	return "[" + promDuration(max(plan.end.Sub(plan.effectiveStart), plan.step)) + ":" + promDuration(plan.step) + "]"
}

func (plan attributionPlan) requestedSteps() int { return int(plan.end.Sub(plan.start)/plan.step) + 1 }

// AttributedRecommendationQueryFingerprint binds recommendation provenance to
// the workload UID and creation, the live pod population, the attribution
// mode, the query window, and the attributed expressions.
func AttributedRecommendationQueryFingerprint(subject Subject, identity WorkloadIdentity, window Window) (string, error) {
	plan, err := newAttributionPlan(subject, identity, window)
	if err != nil {
		return "", err
	}
	if err := checkExpressions(plan.cpuPerPod, plan.memoryPod); err != nil {
		return "", err
	}
	pairs := make([]string, 0, len(identity.Pods))
	for _, pod := range identity.Pods {
		pairs = append(pairs, pod.Name+"="+pod.UID)
	}
	sort.Strings(pairs)
	mode := AttributionHistorical
	if identity.LivePodsOnly {
		mode = AttributionLivePods
	}
	parts := []string{
		"kuberight-recommendation-queries/attributed-v3", mode, identity.UID, identity.CreatedAt.UTC().Format(time.RFC3339Nano),
		strings.Join(pairs, ","),
		strconv.FormatInt(plan.start.Unix(), 10), strconv.FormatInt(plan.end.Unix(), 10), strconv.FormatInt(int64(plan.step), 10),
		plan.cpuPerPod, plan.memoryPod, plan.desired,
	}
	digest := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return hex.EncodeToString(digest[:]), nil
}

type replicaKey struct{ pod, uid string }

// promRequest is one bounded Prometheus call; Start equal to End is an
// instant evaluation.
type promRequest struct {
	expression string
	start, end time.Time
	step       time.Duration
	result     metrics.RangeResult
	err        error
}

func (source *Source) runRequests(ctx context.Context, requests []*promRequest) error {
	for _, request := range requests {
		if err := checkExpressions(request.expression); err != nil {
			return err
		}
	}
	var wait sync.WaitGroup
	for _, request := range requests {
		wait.Add(1)
		go func(request *promRequest) {
			defer wait.Done()
			request.result, request.err = source.client.QueryRange(ctx, metrics.RangeQuery{Expression: request.expression, Start: request.start, End: request.end, Step: request.step})
		}(request)
	}
	wait.Wait()
	failures := make([]error, 0, len(requests))
	for _, request := range requests {
		failures = append(failures, request.err)
	}
	return errors.Join(failures...)
}

func (plan attributionPlan) rangeRequest(expression string) *promRequest {
	return &promRequest{expression: expression, start: plan.effectiveStart, end: plan.end, step: plan.step}
}

func (plan attributionPlan) instantRequest(expression string) *promRequest {
	return &promRequest{expression: expression, start: plan.end, end: plan.end, step: time.Minute}
}

// historyRequests are the Prometheus calls behind HistoryForIdentity. Range
// requests return one workload-level series each; instant requests return
// one value per attributed pod UID.
type historyRequests struct {
	cpuMax, memoryMax, cpuCount, memoryCount, runningCount, ambiguous, unattributed, desired *promRequest
	podRunning, podCPU, podMemory, podCPUP95, podMemoryP95, podCreated, ownedNow             *promRequest
}

func (requests historyRequests) all() []*promRequest {
	all := []*promRequest{requests.cpuMax, requests.memoryMax, requests.cpuCount, requests.memoryCount, requests.runningCount, requests.ambiguous, requests.unattributed,
		requests.podRunning, requests.podCPU, requests.podMemory, requests.podCPUP95, requests.podMemoryP95, requests.podCreated, requests.ownedNow}
	if requests.desired != nil {
		all = append(all, requests.desired)
	}
	return all
}

func (plan attributionPlan) podCreatedExpression() string {
	return "max by (pod, uid) (last_over_time((max by (namespace, pod) (kube_pod_created{namespace=" + strconv.Quote(plan.subject.Namespace) + ",pod=~" + strconv.Quote(podPattern(plan.subject)) + "}) * on (namespace, pod) group_left (uid) " + plan.unambiguous + ")" + plan.subqueryRange() + "))"
}

func (plan attributionPlan) historyRequests() historyRequests {
	rng := plan.subqueryRange()
	requests := historyRequests{
		cpuMax:       plan.rangeRequest("max(" + plan.cpuPerPod + ")"),
		memoryMax:    plan.rangeRequest("max(" + plan.memoryPod + ")"),
		cpuCount:     plan.rangeRequest("count(" + plan.cpuPerPod + ")"),
		memoryCount:  plan.rangeRequest("count(" + plan.memoryPod + ")"),
		runningCount: plan.rangeRequest("count(" + plan.running + ")"),
		ambiguous:    plan.rangeRequest("count(count by (namespace, pod) (" + plan.owned + ") > 1)"),
		unattributed: plan.rangeRequest("count(max by (namespace, pod) (container_memory_working_set_bytes{" + plan.selector + "}) unless on (namespace, pod) " + plan.owned + ")"),
		podRunning:   plan.instantRequest("count_over_time((max by (pod, uid, created_by_name) (" + plan.running + "))" + rng + ")"),
		podCPU:       plan.instantRequest("count_over_time((" + plan.cpuPerPod + ")" + rng + ")"),
		podMemory:    plan.instantRequest("count_over_time((" + plan.memoryPod + ")" + rng + ")"),
		podCPUP95:    plan.instantRequest("quantile_over_time(0.95, (" + plan.cpuPerPod + ")" + rng + ")"),
		podMemoryP95: plan.instantRequest("quantile_over_time(0.95, (" + plan.memoryPod + ")" + rng + ")"),
		podCreated:   plan.instantRequest(plan.podCreatedExpression()),
		ownedNow:     plan.instantRequest("max by (pod, uid) (" + plan.owned + ")"),
	}
	if plan.desired != "" {
		requests.desired = plan.rangeRequest(plan.desired)
	}
	return requests
}

// eventRequests are the per-pod restart, OOM, and throttling evaluations.
type eventRequests struct {
	restartsMax, restartsMin, oom, throttledSum, periodsSum, peak, created *promRequest
}

func (requests eventRequests) all() []*promRequest {
	return []*promRequest{requests.restartsMax, requests.restartsMin, requests.oom, requests.throttledSum, requests.periodsSum, requests.peak, requests.created}
}

func (plan attributionPlan) eventRequests() eventRequests {
	rng := plan.subqueryRange()
	join := " * on (namespace, pod) group_left (uid) " + plan.unambiguous
	restarts := "max by (namespace, pod) (kube_pod_container_status_restarts_total{" + plan.selector + "})" + join
	throttled := "sum by (pod, uid) (sum by (namespace, pod) (rate(container_cpu_cfs_throttled_periods_total{" + plan.selector + "}[" + rateWindow(plan.step) + "]))" + join + ")"
	periods := "sum by (pod, uid) (sum by (namespace, pod) (rate(container_cpu_cfs_periods_total{" + plan.selector + "}[" + rateWindow(plan.step) + "]))" + join + ")"
	return eventRequests{
		restartsMax:  plan.instantRequest("max by (pod, uid) (max_over_time((" + restarts + ")" + rng + "))"),
		restartsMin:  plan.instantRequest("min by (pod, uid) (min_over_time((" + restarts + ")" + rng + "))"),
		oom:          plan.instantRequest("max by (pod, uid) (max_over_time((max by (namespace, pod) (kube_pod_container_status_last_terminated_reason{" + plan.selector + `,reason="OOMKilled"})` + join + ")" + rng + "))"),
		throttledSum: plan.instantRequest("sum_over_time((" + throttled + ")" + rng + ")"),
		periodsSum:   plan.instantRequest("sum_over_time((" + periods + ")" + rng + ")"),
		peak:         plan.instantRequest("quantile_over_time(0.95, (" + throttled + " / (" + periods + " > 0))" + rng + ")"),
		created:      plan.instantRequest(plan.podCreatedExpression()),
	}
}

// stepValues indexes a single-series range result by Unix second. If an
// upstream returns several series, the largest value wins.
func stepValues(result metrics.RangeResult) map[int64]float64 {
	values := map[int64]float64{}
	for _, series := range result.Series {
		for _, sample := range series.Samples {
			key := sample.Timestamp.Unix()
			if current, ok := values[key]; !ok || sample.Value > current {
				values[key] = sample.Value
			}
		}
	}
	return values
}

func resultQuality(result metrics.RangeResult) (duplicate, anomalous int) {
	for _, series := range result.Series {
		duplicate += series.Quality.DuplicateSamples
		anomalous += series.Quality.AnomalousSamples
	}
	return duplicate, anomalous
}

// replicaValues indexes an instant per-pod result by pod and UID, and records
// any revision label.
func replicaValues(result metrics.RangeResult, revisions map[replicaKey]string) map[replicaKey]float64 {
	values := map[replicaKey]float64{}
	for _, series := range result.Series {
		if len(series.Samples) == 0 {
			continue
		}
		key := replicaKey{pod: series.Labels["pod"], uid: series.Labels["uid"]}
		if key.pod == "" || key.uid == "" || !dnsSubdomain.MatchString(key.pod) || !uidToken.MatchString(key.uid) {
			continue
		}
		value := series.Samples[len(series.Samples)-1].Value
		if math.IsNaN(value) || math.IsInf(value, 0) {
			continue
		}
		values[key] += value
		if revision := series.Labels["created_by_name"]; revisions != nil && revision != "" {
			revisions[key] = revision
		}
	}
	return values
}

func seriesPoints(result metrics.RangeResult) []Point {
	values := stepValues(result)
	points := make([]Point, 0, len(values))
	for at, value := range values {
		points = append(points, Point{Timestamp: time.Unix(at, 0).UTC(), Value: value})
	}
	sort.Slice(points, func(left, right int) bool { return points[left].Timestamp.Before(points[right].Timestamp) })
	return points
}

// workloadQuality describes the busiest-replica series against the steps in
// which the workload should have produced samples.
func workloadQuality(points []Point, expected int, end time.Time, step time.Duration, stale bool, duplicate, anomalous int) metrics.DataQuality {
	quality := metrics.DataQuality{Issues: []metrics.QualityIssue{}, ExpectedSamples: expected, ReceivedSamples: len(points) + duplicate + anomalous, AcceptedSamples: len(points), DuplicateSamples: duplicate, AnomalousSamples: anomalous}
	quality.MissingSamples = max(0, expected-len(points))
	if expected > 0 {
		quality.Coverage = math.Min(1, float64(len(points))/float64(expected))
	}
	if len(points) == 0 {
		quality.Issues = append(quality.Issues, metrics.IssueMissing)
	} else {
		last := points[len(points)-1].Timestamp
		quality.LastAcceptedSample = &last
		if quality.Coverage < minimumReplicaCoverage {
			quality.Issues = append(quality.Issues, metrics.IssueSparse)
		}
		if stale && last.Before(end.Add(-max(5*time.Minute, 2*step))) {
			quality.Issues = append(quality.Issues, metrics.IssueStale)
		}
	}
	if duplicate > 0 {
		quality.Issues = append(quality.Issues, metrics.IssueDuplicate)
	}
	if anomalous > 0 {
		quality.Issues = append(quality.Issues, metrics.IssueAnomalous)
	}
	return quality
}

// stepState is the population evidence for one observation step.
type stepState struct {
	at                      time.Time
	expected, cpu, memory   int
	active, known, zero     bool
	running                 int
	ambiguous, unattributed bool
}

type populationSummary struct {
	steps                                      []stepState
	active, known, zero, ambiguous, unattrib   int
	expectedTime, cpuTime, memoryTime, runTime float64
	cpuSteps, memorySteps                      int
	peakDesired, minimumDesired                int32
	desiredSeen                                bool
	scalingEvents                              int
	finalZero                                  bool
}

func summarizePopulation(plan attributionPlan, desired, running, cpu, memory, ambiguous, unattributed map[int64]float64) populationSummary {
	summary := populationSummary{minimumDesired: math.MaxInt32}
	batch := batchKind(plan.subject.Kind)
	previous, havePrevious := 0.0, false
	for at := plan.effectiveStart; !at.After(plan.end); at = at.Add(plan.step) {
		key := at.Unix()
		state := stepState{at: at, running: int(running[key]), cpu: int(cpu[key]), memory: int(memory[key]), ambiguous: ambiguous[key] > 0, unattributed: unattributed[key] > 0}
		wanted, hasDesired := desired[key]
		if hasDesired && !batch {
			summary.desiredSeen = true
			summary.peakDesired = max(summary.peakDesired, int32(wanted))
			summary.minimumDesired = min(summary.minimumDesired, int32(wanted))
			if havePrevious && wanted != previous {
				summary.scalingEvents++
			}
			previous, havePrevious = wanted, true
		}
		switch {
		case batch && state.running == 0 && state.cpu == 0 && state.memory == 0:
			state.zero = true
		case !batch && hasDesired && wanted == 0 && state.running == 0:
			state.zero = true
		case batch:
			state.expected, state.known = max(state.running, state.cpu, state.memory), true
		case hasDesired:
			state.expected, state.known = max(int(wanted), state.running), true
		default:
			// Without a desired count the population is unknown; at least one
			// replica is assumed so the step counts against coverage.
			state.expected = max(state.running, 1)
		}
		if state.zero {
			summary.zero++
		} else {
			state.active = true
			summary.active++
			if state.known {
				summary.known++
			}
			expected := float64(state.expected)
			summary.expectedTime += expected
			summary.runTime += math.Min(float64(state.running), expected)
			summary.cpuTime += math.Min(float64(state.cpu), expected)
			summary.memoryTime += math.Min(float64(state.memory), expected)
			if state.cpu > 0 {
				summary.cpuSteps++
			}
			if state.memory > 0 {
				summary.memorySteps++
			}
			if state.ambiguous {
				summary.ambiguous++
			}
			if state.unattributed {
				summary.unattrib++
			}
		}
		summary.steps = append(summary.steps, state)
	}
	if !summary.desiredSeen {
		summary.minimumDesired = 0
	}
	summary.finalZero = len(summary.steps) > 0 && summary.steps[len(summary.steps)-1].zero
	return summary
}

func ratio(numerator, denominator float64) float64 {
	if denominator <= 0 {
		return 0
	}
	return math.Min(1, numerator/denominator)
}

func timeline(steps []stepState, step time.Duration) []CoverageBucket {
	if len(steps) == 0 {
		return nil
	}
	size := (len(steps) + maximumTimelineBuckets - 1) / maximumTimelineBuckets
	buckets := make([]CoverageBucket, 0, (len(steps)+size-1)/size)
	for begin := 0; begin < len(steps); begin += size {
		finish := min(begin+size, len(steps))
		bucket := CoverageBucket{Start: steps[begin].at, End: steps[finish-1].at.Add(step)}
		known := 0
		for _, state := range steps[begin:finish] {
			if !state.active {
				continue
			}
			bucket.ActiveSteps++
			bucket.ExpectedReplicas += float64(state.expected)
			bucket.CPUObservedReplicas += math.Min(float64(state.cpu), float64(state.expected))
			bucket.MemoryObservedReplicas += math.Min(float64(state.memory), float64(state.expected))
			if state.known {
				known++
			}
		}
		if bucket.ActiveSteps > 0 {
			count := float64(bucket.ActiveSteps)
			bucket.ExpectedReplicas = math.Round(bucket.ExpectedReplicas/count*100) / 100
			bucket.CPUObservedReplicas = math.Round(bucket.CPUObservedReplicas/count*100) / 100
			bucket.MemoryObservedReplicas = math.Round(bucket.MemoryObservedReplicas/count*100) / 100
			bucket.PopulationKnown = math.Round(float64(known)/count*100) / 100
		}
		buckets = append(buckets, bucket)
	}
	return buckets
}

func imbalance(observations []ReplicaObservation, value func(ReplicaObservation) float64) float64 {
	minimum, maximum, counted := math.Inf(1), 0.0, 0
	live := 0
	for _, observation := range observations {
		if observation.Live && value(observation) > 0 {
			live++
		}
	}
	for _, observation := range observations {
		current := value(observation)
		if current <= 0 || (live >= 2 && !observation.Live) {
			continue
		}
		counted++
		minimum, maximum = math.Min(minimum, current), math.Max(maximum, current)
	}
	if counted < 2 || minimum == 0 {
		return 0
	}
	return math.Round(maximum/minimum*100) / 100
}

type replicaEvidence struct {
	running, cpuSamples, memorySamples, cpuP95, memoryP95, created, ownedNow map[replicaKey]float64
	revisions                                                                map[replicaKey]string
}

func percent(value float64) string {
	return strconv.FormatFloat(math.Floor(value*100), 'f', 0, 64) + "%"
}

func coverageFor(plan attributionPlan, summary populationSummary, evidence replicaEvidence) (ReplicaCoverage, []string) {
	identity := plan.identity
	effective := plan.effectiveStart
	result := ReplicaCoverage{
		Attribution: AttributionHistorical, EffectiveStart: &effective, ExpectedReplicas: identity.ExpectedReplicas,
		PeakExpectedReplicas: summary.peakDesired, MinimumExpectedReplicas: summary.minimumDesired, ScalingEvents: summary.scalingEvents,
		ActiveSteps: summary.active, ScaledToZeroSteps: summary.zero, AmbiguousSteps: summary.ambiguous, UnattributedSteps: summary.unattrib,
		Issues: []string{}, Observations: []ReplicaObservation{},
	}
	if identity.LivePodsOnly {
		result.Attribution = AttributionLivePods
	}
	warnings := []string{}
	batch := batchKind(plan.subject.Kind)

	// Temporal coverage is measured against the requested window. Steps before
	// the workload existed count as missing for long-running controllers.
	denominator := plan.requestedSteps() - summary.zero
	if batch || identity.LivePodsOnly {
		denominator = summary.active
	}
	result.CPUTemporalCoverage = ratio(float64(summary.cpuSteps), float64(denominator))
	result.MemoryTemporalCoverage = ratio(float64(summary.memorySteps), float64(denominator))
	result.CPUReplicaCoverage = ratio(summary.cpuTime, summary.expectedTime)
	result.MemoryReplicaCoverage = ratio(summary.memoryTime, summary.expectedTime)
	result.IdentityCoverage = ratio(summary.runTime, summary.expectedTime)
	result.ExpectedReplicaHistoryCoverage = ratio(float64(summary.known), float64(summary.active))
	result.Timeline = timeline(summary.steps, plan.step)

	live := map[replicaKey]PodIdentity{}
	liveNames := map[string]PodIdentity{}
	for _, pod := range identity.Pods {
		live[replicaKey{pod: pod.Name, uid: pod.UID}] = pod
		liveNames[pod.Name] = pod
	}
	universe := map[replicaKey]bool{}
	for _, values := range []map[replicaKey]float64{evidence.running, evidence.cpuSamples, evidence.memorySamples} {
		for key := range values {
			universe[key] = true
		}
	}
	for key, pod := range live {
		if pod.Active() || universe[key] {
			universe[key] = true
		}
	}
	keys := make([]replicaKey, 0, len(universe))
	for key := range universe {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(left, right int) bool {
		if keys[left].pod != keys[right].pod {
			return keys[left].pod < keys[right].pod
		}
		return keys[left].uid < keys[right].uid
	})
	revisions := map[string]bool{}
	grace := max(5*time.Minute, 2*plan.step)
	notAttributed, staleUIDs, predates, incompleteLive := 0, 0, 0, 0
	for _, key := range keys {
		pod, isLive := live[key]
		observation := ReplicaObservation{
			Pod: key.pod, UID: key.uid, Live: isLive, Revision: evidence.revisions[key],
			RunningSamples: int(evidence.running[key]), CPUSamples: int(evidence.cpuSamples[key]), MemorySamples: int(evidence.memorySamples[key]),
			CPUP95Milli: math.Round(evidence.cpuP95[key]*100) / 100, MemoryP95Bytes: math.Round(evidence.memoryP95[key]),
		}
		if isLive {
			observation.Phase, observation.Active, observation.CreatedAt, observation.DeletedAt = pod.Phase, pod.Active(), pod.CreatedAt, pod.DeletedAt
		} else if created, ok := evidence.created[key]; ok && created > 0 {
			observation.CreatedAt = time.Unix(int64(created), 0).UTC()
		}
		if observation.Revision != "" {
			revisions[observation.Revision] = true
		}
		lifetime := float64(max(observation.RunningSamples, observation.CPUSamples, observation.MemorySamples))
		observation.CPUCoverage = ratio(float64(observation.CPUSamples), lifetime)
		observation.MemoryCoverage = ratio(float64(observation.MemorySamples), lifetime)
		if observation.CPUSamples > 0 {
			result.CPUObservedReplicas++
		}
		if observation.MemorySamples > 0 {
			result.MemoryObservedReplicas++
		}
		if observation.CPUSamples > 0 || observation.MemorySamples > 0 {
			result.ObservedReplicas++
		}
		if observation.Active {
			result.ActiveReplicas++
		} else {
			result.TerminatedReplicas++
		}
		established := isLive && pod.Active() && pod.Phase == "Running" && !pod.CreatedAt.After(plan.end.Add(-grace))
		switch {
		case observation.RunningSamples >= 2 && (observation.CPUSamples == 0 || observation.MemorySamples == 0):
			result.MissingReplicas++
		case established && (observation.CPUSamples == 0 || observation.MemorySamples == 0):
			result.MissingReplicas++
		}
		if established {
			if pod.CreatedAt.Before(identity.CreatedAt) {
				predates++
			} else if _, attributed := evidence.ownedNow[key]; !attributed {
				notAttributed++
			}
			if observation.CPUCoverage < minimumReplicaCoverage || observation.MemoryCoverage < minimumReplicaCoverage {
				incompleteLive++
			}
		}
		result.Observations = append(result.Observations, observation)
	}
	for key := range evidence.ownedNow {
		if pod, ok := liveNames[key.pod]; ok && pod.UID != key.uid {
			staleUIDs++
		}
	}
	result.DiscoveredReplicas = len(keys)
	result.Revisions = len(revisions)
	result.CPUImbalanceRatio = imbalance(result.Observations, func(item ReplicaObservation) float64 { return item.CPUP95Milli })
	result.MemoryImbalanceRatio = imbalance(result.Observations, func(item ReplicaObservation) float64 { return item.MemoryP95Bytes })

	issue := func(format string, values ...any) {
		result.Issues = append(result.Issues, fmt.Sprintf(format, values...))
	}
	if summary.active == 0 {
		issue("the workload had no running replicas in the observation window")
	}
	if summary.active > 0 && summary.runTime == 0 {
		issue("kube-state-metrics ownership series are unavailable, so no usage can be attributed to the current workload UID")
	}
	if !batch && !identity.LivePodsOnly && summary.active > 0 && result.ExpectedReplicaHistoryCoverage < minimumPopulationHistoryCoverage {
		issue("the desired replica count is recorded for %s of the observed steps", percent(result.ExpectedReplicaHistoryCoverage))
	}
	if summary.expectedTime > 0 && result.IdentityCoverage < minimumReplicaTimeCoverage {
		issue("attributed running pods cover %s of the expected replica-time", percent(result.IdentityCoverage))
	}
	if summary.expectedTime > 0 && result.CPUReplicaCoverage < minimumReplicaTimeCoverage {
		issue("CPU metrics cover %s of the expected replica-time", percent(result.CPUReplicaCoverage))
	}
	if summary.expectedTime > 0 && result.MemoryReplicaCoverage < minimumReplicaTimeCoverage {
		issue("memory metrics cover %s of the expected replica-time", percent(result.MemoryReplicaCoverage))
	}
	if result.CPUTemporalCoverage < minimumReplicaCoverage {
		issue("exact CPU history covers %s of the requested window", percent(result.CPUTemporalCoverage))
	}
	if result.MemoryTemporalCoverage < minimumReplicaCoverage {
		issue("exact memory history covers %s of the requested window", percent(result.MemoryTemporalCoverage))
	}
	if result.MissingReplicas > 0 {
		issue("%d attributed replicas ran without both CPU and memory metrics", result.MissingReplicas)
	}
	if notAttributed > 0 {
		issue("kube-state-metrics does not attribute %d running live replicas to the current workload UID", notAttributed)
	}
	if predates > 0 {
		issue("%d live replicas predate the current workload object (adopted pods), so their history cannot be attributed", predates)
	}
	if staleUIDs > 0 {
		issue("kube-state-metrics reports %d live pod names with a different UID than Kubernetes", staleUIDs)
	}
	if incompleteLive > 0 {
		issue("one or more active replicas have incomplete per-replica history")
	}
	activeShare := float64(max(1, summary.active))
	switch share := float64(summary.ambiguous) / activeShare; {
	case share > maximumAmbiguousShare:
		issue("%s of steps map one pod name to several UIDs and were excluded", percent(share))
	case summary.ambiguous > 0:
		warnings = append(warnings, fmt.Sprintf("%d steps mapped one pod name to several UIDs (pod replacement overlap) and were excluded", summary.ambiguous))
	}
	switch share := float64(summary.unattrib) / activeShare; {
	case share > maximumUnattributedShare:
		issue("%s of steps contain usage from pods matching the workload's names that cannot be attributed to its UID", percent(share))
	case summary.unattrib > 0:
		warnings = append(warnings, fmt.Sprintf("%d steps contained usage from same-name pods that could not be attributed and were excluded", summary.unattrib))
	}
	if result.Revisions > 1 && plan.subject.Kind == "Deployment" {
		warnings = append(warnings, fmt.Sprintf("the window spans %d ReplicaSet revisions; usage from replaced replicas is included", result.Revisions))
	}
	if summary.zero > 0 && !batch {
		warnings = append(warnings, fmt.Sprintf("the workload was scaled to zero for %d steps, which are excluded from coverage", summary.zero))
	}
	result.Complete = len(result.Issues) == 0
	return result, warnings
}

// HistoryForIdentity reconstructs the workload's replica population for
// every step, derives the conservative busiest-replica series, and records
// time-aware completeness.
func (source *Source) HistoryForIdentity(ctx context.Context, subject Subject, identity WorkloadIdentity, window Window) (History, error) {
	if window.End.IsZero() {
		window.End = source.now()
	}
	plan, err := newAttributionPlan(subject, identity, window)
	if err != nil {
		return History{}, err
	}
	requests := plan.historyRequests()
	if err := source.runRequests(ctx, requests.all()); err != nil {
		return History{}, err
	}
	cpuMax, memoryMax, cpuCount, memoryCount, runningCount, ambiguous, unattributed := requests.cpuMax, requests.memoryMax, requests.cpuCount, requests.memoryCount, requests.runningCount, requests.ambiguous, requests.unattributed
	podRunning, podCPU, podMemory, podCPUP95, podMemoryP95, podCreated, ownedNow := requests.podRunning, requests.podCPU, requests.podMemory, requests.podCPUP95, requests.podMemoryP95, requests.podCreated, requests.ownedNow
	desiredValues := map[int64]float64{}
	if requests.desired != nil {
		desiredValues = stepValues(requests.desired.result)
	}
	summary := summarizePopulation(plan, desiredValues, stepValues(runningCount.result), stepValues(cpuCount.result), stepValues(memoryCount.result), stepValues(ambiguous.result), stepValues(unattributed.result))
	evidence := replicaEvidence{revisions: map[replicaKey]string{}}
	evidence.running = replicaValues(podRunning.result, evidence.revisions)
	evidence.cpuSamples = replicaValues(podCPU.result, nil)
	evidence.memorySamples = replicaValues(podMemory.result, nil)
	evidence.cpuP95 = replicaValues(podCPUP95.result, nil)
	evidence.memoryP95 = replicaValues(podMemoryP95.result, nil)
	evidence.created = replicaValues(podCreated.result, nil)
	evidence.ownedNow = replicaValues(ownedNow.result, nil)
	replicaCoverage, warnings := coverageFor(plan, summary, evidence)

	expected := plan.requestedSteps() - summary.zero
	if batchKind(subject.Kind) || identity.LivePodsOnly {
		expected = summary.active
	}
	cpuPoints, memoryPoints := seriesPoints(cpuMax.result), seriesPoints(memoryMax.result)
	cpuDuplicate, cpuAnomalous := resultQuality(cpuMax.result)
	memoryDuplicate, memoryAnomalous := resultQuality(memoryMax.result)
	checkStale := !summary.finalZero
	history := History{
		Subject: subject, Start: plan.start, End: plan.end, Step: plan.step.String(), Warnings: []string{}, Notes: warnings,
		CPU:             Series{Resource: "cpu", Unit: "millicores", Points: cpuPoints, Quality: workloadQuality(cpuPoints, expected, plan.end, plan.step, checkStale, cpuDuplicate, cpuAnomalous)},
		Memory:          Series{Resource: "memory", Unit: "bytes", Points: memoryPoints, Quality: workloadQuality(memoryPoints, expected, plan.end, plan.step, checkStale, memoryDuplicate, memoryAnomalous)},
		ReplicaCoverage: &replicaCoverage,
	}
	for _, request := range requests.all() {
		history.Warnings = append(history.Warnings, request.result.Warnings...)
	}
	if len(history.CPU.Points) == 0 && len(history.Memory.Points) == 0 {
		if summary.unattrib > 0 {
			return history, fmt.Errorf("%w: usage exists for pods matching the workload's names, but kube-state-metrics attributes none of them to the current workload UID", ErrAttributionIncomplete)
		}
		return history, ErrNoData
	}
	return history, nil
}

// EventsForIdentity reads restart, OOM, and throttling evidence for every
// attributed pod UID in the window, including replaced pods, so an OOM kill
// before a rollout still blocks a memory reduction. Workload-level values use
// the worst replica.
func (source *Source) EventsForIdentity(ctx context.Context, subject Subject, identity WorkloadIdentity, window Window) (Events, error) {
	if window.End.IsZero() {
		window.End = source.now()
	}
	plan, err := newAttributionPlan(subject, identity, window)
	if err != nil {
		return Events{}, err
	}
	requests := plan.eventRequests()
	if err := source.runRequests(ctx, requests.all()); err != nil {
		return Events{}, err
	}
	restartsMax, restartsMin, oom, throttledSum, periodsSum, peak, created := requests.restartsMax, requests.restartsMin, requests.oom, requests.throttledSum, requests.periodsSum, requests.peak, requests.created
	maxima, minima := replicaValues(restartsMax.result, nil), replicaValues(restartsMin.result, nil)
	oomValues, throttledValues, periodValues, peakValues := replicaValues(oom.result, nil), replicaValues(throttledSum.result, nil), replicaValues(periodsSum.result, nil), replicaValues(peak.result, nil)
	createdValues := replicaValues(created.result, nil)

	live := map[replicaKey]bool{}
	keys := map[replicaKey]bool{}
	for _, pod := range identity.Pods {
		key := replicaKey{pod: pod.Name, uid: pod.UID}
		live[key] = true
		keys[key] = true
	}
	for _, values := range []map[replicaKey]float64{maxima, oomValues, periodValues} {
		for key := range values {
			keys[key] = true
		}
	}
	ordered := make([]replicaKey, 0, len(keys))
	for key := range keys {
		ordered = append(ordered, key)
	}
	sort.Slice(ordered, func(left, right int) bool {
		if ordered[left].pod != ordered[right].pod {
			return ordered[left].pod < ordered[right].pod
		}
		return ordered[left].uid < ordered[right].uid
	})
	events := Events{Available: len(maxima) > 0, Replicas: make([]ReplicaEvents, 0, len(ordered))}
	for _, key := range ordered {
		replica := ReplicaEvents{Pod: key.pod, UID: key.uid, Live: live[key]}
		if value, ok := maxima[key]; ok {
			// A pod created inside the window started its counter at zero.
			baseline := minima[key]
			if at, known := createdValues[key]; known && !time.Unix(int64(at), 0).Before(plan.effectiveStart) {
				baseline = 0
			}
			replica.Restarts = int64(math.Round(math.Max(0, value-baseline)))
			events.Restarts += replica.Restarts
		}
		if value, ok := oomValues[key]; ok && value > 0 {
			replica.OOMKills = int64(math.Round(math.Max(1, value)))
			events.OOMKills += replica.OOMKills
		}
		if denominator, ok := periodValues[key]; ok && denominator > 0 {
			replica.ThrottlingAvailable = true
			replica.ThrottlingRatio = math.Min(1, math.Max(0, throttledValues[key]/denominator))
			events.ThrottlingAvailable = true
			events.ThrottlingRatio = math.Max(events.ThrottlingRatio, replica.ThrottlingRatio)
		}
		if value, ok := peakValues[key]; ok {
			replica.ThrottlingPeakRatio = math.Min(1, math.Max(0, value))
			events.ThrottlingPeakRatio = math.Max(events.ThrottlingPeakRatio, replica.ThrottlingPeakRatio)
		}
		events.Replicas = append(events.Replicas, replica)
	}
	return events, nil
}

// ResourceChangedAt returns when the container's requests or limits last
// changed among the workload's attributed pods in the window, or nil when no
// change was observed. During a rollout old and new pods coexist, so the
// change is dated when the per-step minimum or maximum moves, which is when
// the new configuration replaced the old one. Limits that are added to or
// removed from a container without a value change are not detected.
func (source *Source) ResourceChangedAt(ctx context.Context, subject Subject, identity WorkloadIdentity, window Window) (*time.Time, error) {
	if window.End.IsZero() {
		window.End = source.now()
	}
	plan, err := newAttributionPlan(subject, identity, window)
	if err != nil {
		return nil, err
	}
	join := " * on (namespace, pod) group_left () max by (namespace, pod) (" + plan.unambiguous + ")"
	selector := plan.selector + `,resource=~"cpu|memory"`
	requests := make([]*promRequest, 0, 4)
	for _, metric := range []string{"kube_pod_container_resource_requests", "kube_pod_container_resource_limits"} {
		for _, operation := range []string{"max", "min"} {
			requests = append(requests, plan.rangeRequest(operation+" by (resource) ("+metric+"{"+selector+"}"+join+")"))
		}
	}
	if err := source.runRequests(ctx, requests); err != nil {
		return nil, err
	}
	var latest time.Time
	for _, request := range requests {
		for _, series := range request.result.Series {
			for index := 1; index < len(series.Samples); index++ {
				if series.Samples[index].Value != series.Samples[index-1].Value && series.Samples[index].Timestamp.After(latest) {
					latest = series.Samples[index].Timestamp
				}
			}
		}
	}
	if latest.IsZero() {
		return nil, nil
	}
	latest = latest.UTC()
	return &latest, nil
}
