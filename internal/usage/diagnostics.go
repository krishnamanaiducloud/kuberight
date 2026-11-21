package usage

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/kuberight/kuberight/internal/metrics"
)

// Attribution outcomes, from the most to the least useful. They distinguish a
// workload KubeRight could not analyze from one it analyzed and found in good
// shape, so a skipped analysis is never reported as a failure of the
// workload, and a missing dependency is never reported as missing history.
const (
	OutcomeAttributed            = "attributed"
	OutcomeIncompleteHistory     = "insufficient_history"
	OutcomeMissingMetrics        = "missing_metrics"
	OutcomeIncompleteAttribution = "incomplete_attribution"
	OutcomeProviderUnavailable   = "provider_unavailable"
)

// maximumDiagnosticPods bounds the per-pod rows in one report.
const maximumDiagnosticPods = 200

// FamilyStatus reports whether one metric family has series for the
// workload's pods in the window.
type FamilyStatus struct {
	Name    string `json:"name"`
	Purpose string `json:"purpose"`
	// Required families block attribution or coverage when absent, unless
	// an Alternative is present.
	Required    bool   `json:"required"`
	Alternative string `json:"alternative,omitempty"`
	Series      int    `json:"series"`
}

// OwnerAttribution is one ReplicaSet or Job that can link pods to the
// workload, and the evidence for that link.
type OwnerAttribution struct {
	Kind string `json:"kind"`
	Name string `json:"name"`
	UID  string `json:"uid,omitempty"`
	// Verified means the Kubernetes API reports the workload UID in the
	// owner's controller reference. Recorded means kube-state-metrics
	// recorded the owner as the workload's in the window.
	Verified  bool       `json:"verified"`
	Recorded  bool       `json:"recorded"`
	CreatedAt *time.Time `json:"createdAt,omitempty"`
	Replicas  *int32     `json:"replicas,omitempty"`
}

// PodAttribution explains why one pod identity's usage was or was not
// attributed to the workload.
type PodAttribution struct {
	Pod string `json:"pod"`
	// UID is empty when kube-state-metrics never reported the pod.
	UID       string     `json:"uid,omitempty"`
	OwnerKind string     `json:"ownerKind,omitempty"`
	Owner     string     `json:"owner,omitempty"`
	Live      bool       `json:"live"`
	CreatedAt *time.Time `json:"createdAt,omitempty"`
	// CreatedFrom names the series that dated the pod.
	CreatedFrom string `json:"createdFrom,omitempty"`
	HasUsage    bool   `json:"hasUsage"`
	Attributed  bool   `json:"attributed"`
	// Excluded means the pod belongs to a same-name predecessor (or was
	// adopted) and is left out by design; it is not an attribution gap.
	Excluded bool   `json:"excluded,omitempty"`
	Reason   string `json:"reason"`
}

// AttributionReport is read-only evidence for how usage was attributed to a
// workload over a window. It never contains credentials.
type AttributionReport struct {
	Subject           Subject            `json:"subject"`
	Start             time.Time          `json:"start"`
	End               time.Time          `json:"end"`
	Step              string             `json:"step"`
	WorkloadUID       string             `json:"workloadUid"`
	WorkloadCreatedAt time.Time          `json:"workloadCreatedAt"`
	ExpectedReplicas  int32              `json:"expectedReplicas"`
	Owners            []OwnerAttribution `json:"owners"`
	Pods              []PodAttribution   `json:"pods"`
	// PodsTruncated is set when more pod identities existed than are listed.
	PodsTruncated    bool `json:"podsTruncated,omitempty"`
	UsagePods        int  `json:"usagePods"`
	AttributedPods   int  `json:"attributedPods"`
	UnattributedPods int  `json:"unattributedPods"`
	// ExcludedPods were created before the current workload object.
	ExcludedPods    int              `json:"excludedPods"`
	Families        []FamilyStatus   `json:"families"`
	MissingFamilies []string         `json:"missingFamilies"`
	Coverage        *ReplicaCoverage `json:"coverage,omitempty"`
	// CoveragePercent is the replica-time share backed by both CPU and
	// memory samples from attributed pods.
	CoveragePercent float64 `json:"coveragePercent"`
	// Eligible means attributed CPU and memory history exists, so a
	// recommendation can be calculated; Complete coverage is still required
	// before the optimizer allows reductions.
	Eligible bool   `json:"eligible"`
	Outcome  string `json:"outcome"`
	Reason   string `json:"reason"`
}

type familyProbe struct {
	status   FamilyStatus
	selector string
}

func (plan attributionPlan) familyProbes() []familyProbe {
	quote := strconv.Quote
	subject := plan.subject
	namespace := "namespace=" + quote(subject.Namespace)
	pods := namespace + ",pod=~" + quote(podPattern(subject))
	containers := plan.selector
	probes := []familyProbe{
		{FamilyStatus{Name: "container_cpu_usage_seconds_total", Purpose: "CPU usage (cAdvisor)", Required: true}, containers},
		{FamilyStatus{Name: "container_memory_working_set_bytes", Purpose: "memory working set (cAdvisor)", Required: true}, containers},
		{FamilyStatus{Name: "kube_pod_owner", Purpose: "links each pod UID to its controller", Required: true}, pods},
		{FamilyStatus{Name: "kube_pod_created", Purpose: "pod creation time, separates a recreated workload from its same-name predecessor", Required: true, Alternative: "kube_pod_start_time"}, pods},
		{FamilyStatus{Name: "kube_pod_start_time", Purpose: "pod start time, used when kube_pod_created is not exported (OpenShift)", Required: true, Alternative: "kube_pod_created"}, pods},
		{FamilyStatus{Name: "kube_pod_status_phase", Purpose: "running replica population", Required: true}, pods},
		{FamilyStatus{Name: "kube_pod_info", Purpose: "pod inventory (informational)"}, pods},
		{FamilyStatus{Name: "kube_pod_container_resource_requests", Purpose: "historical requests"}, containers},
		{FamilyStatus{Name: "kube_pod_container_resource_limits", Purpose: "historical limits"}, containers},
		{FamilyStatus{Name: "kube_pod_container_status_restarts_total", Purpose: "restart history"}, containers},
		{FamilyStatus{Name: "kube_pod_container_status_last_terminated_reason", Purpose: "OOMKilled evidence"}, containers},
		{FamilyStatus{Name: "container_cpu_cfs_throttled_periods_total", Purpose: "CPU throttling (exported only for containers with a CPU limit)"}, containers},
	}
	if parent, ok := intermediateOwnerOf(subject); ok {
		alternative := ""
		if len(verifiedOwnerNames(plan.identity, parent)) > 0 {
			alternative = "Kubernetes API owner references"
		}
		probes = append(probes, familyProbe{FamilyStatus{Name: parent.metric, Purpose: "links each " + parent.kind + " to the " + subject.Kind + " (needed for revisions no longer in the API)", Required: true, Alternative: alternative},
			namespace + ",owner_kind=" + quote(subject.Kind) + ",owner_name=" + quote(subject.Workload)})
	}
	if metric, ok := desiredReplicaMetrics[subject.Kind]; ok {
		probes = append(probes, familyProbe{FamilyStatus{Name: metric[0], Purpose: "desired replica history for population coverage", Required: true}, namespace + "," + metric[1] + "=" + quote(subject.Workload)})
	}
	return probes
}

// missingFamilies lists required families that are absent with no present
// alternative.
func missingFamilies(families []FamilyStatus) []string {
	present := map[string]bool{}
	for _, family := range families {
		if family.Series > 0 {
			present[family.Name] = true
		}
	}
	missing, reported := []string{}, map[string]bool{}
	for _, family := range families {
		if !family.Required || present[family.Name] {
			continue
		}
		switch {
		case family.Alternative == "":
			missing = append(missing, family.Name)
		case strings.HasPrefix(family.Alternative, "kube_"):
			// Interchangeable families are reported once, as a pair.
			if present[family.Alternative] || reported[family.Alternative] {
				continue
			}
			reported[family.Name] = true
			missing = append(missing, family.Name+" or "+family.Alternative)
		}
		// Any other alternative, such as API owner references, is
		// already known to be available.
	}
	return missing
}

// probeFamilies counts series for each family over the window.
func (source *Source) probeFamilies(ctx context.Context, plan attributionPlan, lookback time.Duration) ([]FamilyStatus, error) {
	probes := plan.familyProbes()
	requests := make([]*promRequest, len(probes))
	for index, probe := range probes {
		requests[index] = plan.instantRequest("count(last_over_time(" + probe.status.Name + "{" + probe.selector + "}[" + promDuration(lookback) + "]))")
	}
	if err := source.runRequests(ctx, requests); err != nil {
		return nil, err
	}
	families := make([]FamilyStatus, len(probes))
	for index, probe := range probes {
		families[index] = probe.status
		for _, series := range requests[index].result.Series {
			if len(series.Samples) > 0 {
				families[index].Series = int(series.Samples[len(series.Samples)-1].Value)
			}
		}
	}
	return families, nil
}

// ownershipGaps explains, for an error message, why usage that matches the
// workload's pod names could not be attributed.
func (source *Source) ownershipGaps(ctx context.Context, plan attributionPlan) string {
	families, err := source.probeFamilies(ctx, plan, plan.end.Sub(plan.start))
	if err != nil {
		return ""
	}
	if missing := missingFamilies(families); len(missing) > 0 {
		return "; Prometheus has no " + strings.Join(missing, ", ") + " series for these pods"
	}
	return "; their kube_pod_owner series name controllers that are not linked to this workload, or the pods were created before the current workload object (" + plan.identity.CreatedAt.UTC().Format(time.RFC3339) + ")"
}

type ownerRecord struct {
	pod, uid, kind, name string
}

func lastValue(series metrics.Series) (float64, bool) {
	if len(series.Samples) == 0 {
		return 0, false
	}
	value := series.Samples[len(series.Samples)-1].Value
	return value, !math.IsNaN(value) && !math.IsInf(value, 0)
}

// AttributionReport reconstructs how usage was attributed to the workload in
// the window: which ReplicaSets or Jobs link pods to it, every pod identity
// with usage or ownership series and why it was or was not attributed, the
// metric families present, and the resulting coverage and eligibility. It
// issues only read queries.
func (source *Source) AttributionReport(ctx context.Context, subject Subject, identity WorkloadIdentity, window Window) (AttributionReport, error) {
	if window.End.IsZero() {
		window.End = source.now()
	}
	plan, err := newAttributionPlan(subject, identity, window)
	if err != nil {
		return AttributionReport{}, err
	}
	report := AttributionReport{
		Subject: subject, Start: plan.start, End: plan.end, Step: plan.step.String(),
		WorkloadUID: identity.UID, WorkloadCreatedAt: identity.CreatedAt.UTC(), ExpectedReplicas: identity.ExpectedReplicas,
		Owners: []OwnerAttribution{}, Pods: []PodAttribution{}, MissingFamilies: []string{},
	}
	lookback := promDuration(plan.end.Sub(plan.start))
	quote := strconv.Quote
	pods := "namespace=" + quote(subject.Namespace) + ",pod=~" + quote(podPattern(subject))
	owners := plan.instantRequest(`max by (pod, uid, owner_kind, owner_name) (last_over_time(kube_pod_owner{` + pods + `,owner_is_controller="true"}[` + lookback + "]))")
	created := plan.instantRequest("max by (pod, uid) (last_over_time(kube_pod_created{" + pods + "}[" + lookback + "]))")
	started := plan.instantRequest("max by (pod, uid) (last_over_time(kube_pod_start_time{" + pods + "}[" + lookback + "]))")
	usagePods := plan.instantRequest("max by (pod) (last_over_time(container_memory_working_set_bytes{" + plan.selector + "}[" + lookback + "])) or max by (pod) (last_over_time(container_cpu_usage_seconds_total{" + plan.selector + "}[" + lookback + "]))")
	requests := []*promRequest{owners, created, started, usagePods}
	parent, intermediate := intermediateOwnerOf(subject)
	var recorded *promRequest
	if intermediate {
		recorded = plan.instantRequest("max by (" + parent.label + ") (last_over_time(" + parent.metric + "{namespace=" + quote(subject.Namespace) + "," + parent.label + "=~" + quote(parent.pattern) +
			",owner_kind=" + quote(subject.Kind) + ",owner_name=" + quote(subject.Workload) + `,owner_is_controller="true"}[` + lookback + "]))")
		requests = append(requests, recorded)
	}
	if err := source.runRequests(ctx, requests); err != nil {
		report.Outcome, report.Reason = OutcomeProviderUnavailable, "Prometheus could not answer the attribution queries"
		return report, err
	}
	if report.Families, err = source.probeFamilies(ctx, plan, plan.end.Sub(plan.start)); err != nil {
		report.Outcome, report.Reason = OutcomeProviderUnavailable, "Prometheus could not answer the metric family queries"
		return report, err
	}
	report.MissingFamilies = missingFamilies(report.Families)

	// Owners: verified through the API, recorded by kube-state-metrics, or both.
	ownerIndex := map[string]*OwnerAttribution{}
	if intermediate {
		for _, owner := range identity.Owners {
			if owner.Kind != parent.kind {
				continue
			}
			createdAt, replicas := owner.CreatedAt.UTC(), owner.Replicas
			report.Owners = append(report.Owners, OwnerAttribution{Kind: owner.Kind, Name: owner.Name, UID: owner.UID, Verified: true, CreatedAt: &createdAt, Replicas: &replicas})
		}
		for _, series := range recorded.result.Series {
			name := series.Labels[parent.label]
			if name == "" {
				continue
			}
			found := false
			for index := range report.Owners {
				if report.Owners[index].Name == name {
					report.Owners[index].Recorded, found = true, true
				}
			}
			if !found {
				report.Owners = append(report.Owners, OwnerAttribution{Kind: parent.kind, Name: name, Recorded: true})
			}
		}
		sort.SliceStable(report.Owners, func(left, right int) bool { return report.Owners[left].Name < report.Owners[right].Name })
		for index := range report.Owners {
			ownerIndex[report.Owners[index].Name] = &report.Owners[index]
		}
	}

	// The attribution itself; its failure is part of the report.
	history, historyErr := source.HistoryForIdentity(ctx, subject, identity, window)
	attributed := map[replicaKey]bool{}
	if history.ReplicaCoverage != nil {
		report.Coverage = history.ReplicaCoverage
		report.CoveragePercent = math.Round(math.Min(history.ReplicaCoverage.CPUReplicaCoverage, history.ReplicaCoverage.MemoryReplicaCoverage)*1000) / 10
		for _, observation := range history.ReplicaCoverage.Observations {
			if observation.RunningSamples > 0 || observation.CPUSamples > 0 || observation.MemorySamples > 0 {
				attributed[replicaKey{pod: observation.Pod, uid: observation.UID}] = true
			}
		}
	}

	live := map[replicaKey]bool{}
	for _, pod := range identity.Pods {
		live[replicaKey{pod: pod.Name, uid: pod.UID}] = true
	}
	withUsage := map[string]bool{}
	for _, series := range usagePods.result.Series {
		if name := series.Labels["pod"]; name != "" {
			withUsage[name] = true
		}
	}
	report.UsagePods = len(withUsage)
	createdAt := func(request *promRequest) map[replicaKey]time.Time {
		values := map[replicaKey]time.Time{}
		for _, series := range request.result.Series {
			if value, ok := lastValue(series); ok && value > 0 {
				values[replicaKey{pod: series.Labels["pod"], uid: series.Labels["uid"]}] = time.Unix(int64(value), 0).UTC()
			}
		}
		return values
	}
	createdValues, startedValues := createdAt(created), createdAt(started)
	records := []ownerRecord{}
	recordedPods := map[string]bool{}
	for _, series := range owners.result.Series {
		record := ownerRecord{pod: series.Labels["pod"], uid: series.Labels["uid"], kind: series.Labels["owner_kind"], name: series.Labels["owner_name"]}
		if record.pod == "" || record.uid == "" {
			continue
		}
		records = append(records, record)
		recordedPods[record.pod] = true
	}
	sort.Slice(records, func(left, right int) bool {
		if records[left].pod != records[right].pod {
			return records[left].pod < records[right].pod
		}
		return records[left].uid < records[right].uid
	})
	workloadCreated := identity.CreatedAt.UTC()
	for _, record := range records {
		key := replicaKey{pod: record.pod, uid: record.uid}
		row := PodAttribution{Pod: record.pod, UID: record.uid, OwnerKind: record.kind, Owner: record.name, Live: live[key], HasUsage: withUsage[record.pod], Attributed: attributed[key]}
		if at, ok := createdValues[key]; ok {
			row.CreatedAt, row.CreatedFrom = &at, "kube_pod_created"
		} else if at, ok := startedValues[key]; ok {
			row.CreatedAt, row.CreatedFrom = &at, "kube_pod_start_time"
		}
		ownerLinked := false
		if intermediate {
			owner := ownerIndex[record.name]
			ownerLinked = record.kind == parent.kind && owner != nil && (owner.Verified || owner.Recorded)
		} else {
			ownerLinked = record.kind == subject.Kind && record.name == subject.Workload
		}
		switch {
		case row.Attributed:
			row.Reason = "attributed"
		case !ownerLinked && intermediate && record.kind == parent.kind:
			row.Reason = fmt.Sprintf("owned by %s %s, which neither the Kubernetes API nor %s links to this %s", record.kind, record.name, parent.metric, subject.Kind)
		case !ownerLinked:
			row.Reason = fmt.Sprintf("controlled by %s %s, not by this workload", record.kind, record.name)
		case row.CreatedAt == nil:
			row.Reason = "neither kube_pod_created nor kube_pod_start_time reports when this pod was created"
		case row.CreatedAt.Before(workloadCreated):
			row.Reason = "created at " + row.CreatedAt.Format(time.RFC3339) + ", before the current workload object (" + workloadCreated.Format(time.RFC3339) + "): a same-name predecessor's or an adopted pod, excluded by design"
			row.Excluded = true
		case !row.HasUsage:
			row.Reason = "owned by the workload but exported no usage in the window"
		default:
			row.Reason = "owned by the workload, but no running, CPU, or memory sample overlapped its ownership series (stale or delayed kube-state-metrics)"
		}
		report.Pods = append(report.Pods, row)
	}
	names := make([]string, 0, len(withUsage))
	for name := range withUsage {
		if !recordedPods[name] {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	for _, name := range names {
		report.Pods = append(report.Pods, PodAttribution{Pod: name, HasUsage: true, Reason: "kube-state-metrics has no kube_pod_owner series for this pod, so its usage cannot be tied to a pod UID"})
	}
	unattributedNames := map[string]bool{}
	attributedNames := map[string]bool{}
	for _, row := range report.Pods {
		if row.Attributed {
			report.AttributedPods++
			attributedNames[row.Pod] = true
		}
	}
	for _, row := range report.Pods {
		switch {
		case row.Excluded:
			report.ExcludedPods++
		case row.HasUsage && !row.Attributed && !attributedNames[row.Pod]:
			unattributedNames[row.Pod] = true
		}
	}
	report.UnattributedPods = len(unattributedNames)
	if len(report.Pods) > maximumDiagnosticPods {
		report.Pods, report.PodsTruncated = report.Pods[:maximumDiagnosticPods], true
	}

	report.Eligible = historyErr == nil
	switch {
	case historyErr == nil && report.Coverage != nil && report.Coverage.Complete:
		report.Outcome, report.Reason = OutcomeAttributed, "every expected replica is attributed to the workload UID with CPU and memory history"
	case len(report.MissingFamilies) > 0:
		report.Outcome, report.Reason = OutcomeMissingMetrics, "Prometheus has no "+strings.Join(report.MissingFamilies, ", ")+" series for this workload"
	case errors.Is(historyErr, ErrAttributionIncomplete) || report.UnattributedPods > 0:
		report.Outcome = OutcomeIncompleteAttribution
		if historyErr != nil {
			report.Reason = historyErr.Error()
		} else {
			report.Reason = fmt.Sprintf("%d pods with usage matching the workload's names could not be attributed to its UID", report.UnattributedPods)
		}
	case historyErr != nil && !errors.Is(historyErr, ErrNoData):
		report.Outcome, report.Reason = OutcomeProviderUnavailable, "Prometheus could not answer the attribution queries"
	case historyErr != nil:
		report.Outcome, report.Reason = OutcomeIncompleteHistory, "no attributed CPU or memory samples exist in the window"
	default:
		report.Outcome = OutcomeIncompleteHistory
		report.Reason = strings.Join(report.Coverage.Issues, "; ")
	}
	return report, nil
}
