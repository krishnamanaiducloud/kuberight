package usage

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kuberight/kuberight/internal/metrics"
)

func attributedIdentity() WorkloadIdentity {
	created := time.Date(2026, 9, 30, 0, 0, 0, 0, time.UTC)
	return WorkloadIdentity{
		UID: "deployment-current", Generation: 7, CreatedAt: created, ExpectedReplicas: 2,
		Pods: []PodIdentity{
			{Name: "web-api-new-aaaaa", UID: "pod-uid-a", CreatedAt: created.Add(time.Hour), Phase: "Running", Containers: []string{"app"}},
			{Name: "web-api-new-bbbbb", UID: "pod-uid-b", CreatedAt: created.Add(2 * time.Hour), Phase: "Running", Containers: []string{"app"}},
		},
	}
}

func TestOwnershipFollowsKubeStateMetricsOwnersBoundedByWorkloadCreation(t *testing.T) {
	identity := attributedIdentity()
	cpu, err := AttributedCPUExpression(subject, identity, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	created := fmt.Sprint(identity.CreatedAt.Unix())
	for _, required := range []string{
		`kube_pod_info{namespace="shop",created_by_kind="ReplicaSet",created_by_name=~"web\\.api-[a-z0-9]{1,10}"}`,
		`kube_replicaset_owner{namespace="shop",replicaset=~"web\\.api-[a-z0-9]{1,10}",owner_kind="Deployment",owner_name="web.api",owner_is_controller="true"}`,
		`kube_replicaset_created{namespace="shop",replicaset=~"web\\.api-[a-z0-9]{1,10}"} >= ` + created,
		`kube_pod_created{namespace="shop",pod=~"web\\.api-[a-z0-9]{1,10}-[a-z0-9]{5}"} >= ` + created,
		`unless on (namespace, pod) (count by (namespace, pod) (`,
		`group_left (uid)`,
	} {
		if !strings.Contains(cpu, required) {
			t.Fatalf("CPU attribution does not contain %q:\n%s", required, cpu)
		}
	}
	if strings.Contains(cpu, "pod-uid-a") {
		t.Fatalf("historical attribution must not be limited to live pod UIDs: %s", cpu)
	}
	identity.LivePodsOnly = true
	live, err := AttributedMemoryExpression(subject, identity, time.Minute)
	if err != nil || !strings.Contains(live, `uid=~"pod-uid-a|pod-uid-b"`) {
		t.Fatalf("live-pods attribution must bind UIDs: %q err=%v", live, err)
	}
}

func TestOwnershipForDirectAndCronJobOwners(t *testing.T) {
	identity := attributedIdentity()
	for kind, required := range map[string][]string{
		"StatefulSet": {`created_by_kind="StatefulSet",created_by_name="web.api"`},
		"DaemonSet":   {`created_by_kind="DaemonSet",created_by_name="web.api"`},
		"Job":         {`created_by_kind="Job",created_by_name="web.api"`},
		"CronJob":     {`created_by_kind="Job",created_by_name=~"web\\.api-[0-9]+"`, `kube_job_owner{namespace="shop",job_name=~"web\\.api-[0-9]+",owner_kind="CronJob",owner_name="web.api"`, `kube_job_created{`},
	} {
		value := subject
		value.Kind = kind
		expression, err := AttributedCPUExpression(value, identity, time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		for _, text := range required {
			if !strings.Contains(expression, text) {
				t.Fatalf("%s ownership lacks %q: %s", kind, text, expression)
			}
		}
	}
}

func TestAttributedIdentityFailsClosed(t *testing.T) {
	valid := attributedIdentity()
	cases := map[string]WorkloadIdentity{
		"missing workload uid": func() WorkloadIdentity { value := valid; value.UID = ""; return value }(),
		"invalid workload uid": func() WorkloadIdentity { value := valid; value.UID = `bad"uid`; return value }(),
		"missing creation":     func() WorkloadIdentity { value := valid; value.CreatedAt = time.Time{}; return value }(),
		"resolution issue": func() WorkloadIdentity {
			value := valid
			value.ResolutionIssues = []string{"owner UID unavailable"}
			return value
		}(),
		"duplicate uid": func() WorkloadIdentity {
			value := valid
			value.Pods = append([]PodIdentity(nil), valid.Pods...)
			value.Pods[1].UID = value.Pods[0].UID
			return value
		}(),
		"live pods only without pods": func() WorkloadIdentity {
			value := valid
			value.Pods, value.LivePodsOnly = nil, true
			return value
		}(),
		"negative replicas": func() WorkloadIdentity { value := valid; value.ExpectedReplicas = -1; return value }(),
	}
	for name, identity := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := AttributedCPUExpression(subject, identity, time.Minute); !errors.Is(err, ErrAttributionIncomplete) {
				t.Fatalf("got %v, want ErrAttributionIncomplete", err)
			}
		})
	}
	// Scaled-to-zero workloads and empty live populations are allowed: the
	// history, not the current inventory, decides whether evidence exists.
	idle := valid
	idle.ExpectedReplicas, idle.Pods = 0, nil
	if _, err := AttributedCPUExpression(subject, idle, time.Minute); err != nil {
		t.Fatalf("scaled-to-zero identity: %v", err)
	}
}

func TestPlanRejectsWindowsBeforeTheWorkloadExisted(t *testing.T) {
	identity := attributedIdentity()
	_, err := newAttributionPlan(subject, identity, Window{Duration: time.Hour, Step: time.Minute, End: identity.CreatedAt.Add(-time.Second)})
	if !errors.Is(err, ErrAttributionIncomplete) {
		t.Fatalf("window before creation: %v", err)
	}
	plan, err := newAttributionPlan(subject, identity, Window{Duration: time.Hour, Step: 5 * time.Minute, End: identity.CreatedAt.Add(22 * time.Minute)})
	if err != nil {
		t.Fatal(err)
	}
	// The effective start is the first step after creation on the window grid.
	if want := identity.CreatedAt.Add(2 * time.Minute); !plan.effectiveStart.Equal(want) {
		t.Fatalf("effective start = %v, want %v", plan.effectiveStart, want)
	}
}

// scriptedPrometheus answers the exact requests of one plan.
type scriptedPrometheus struct {
	mu        sync.Mutex
	responses map[string]metrics.RangeResult
	failures  map[string]error
	seen      []string
}

func (fake *scriptedPrometheus) QueryRange(_ context.Context, query metrics.RangeQuery) (metrics.RangeResult, error) {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	fake.seen = append(fake.seen, query.Expression)
	if err := fake.failures[query.Expression]; err != nil {
		return metrics.RangeResult{}, err
	}
	if result, ok := fake.responses[query.Expression]; ok {
		return result, nil
	}
	return metrics.RangeResult{NoData: true}, nil
}

// steps renders a per-step workload series. A negative value is a gap.
func steps(plan attributionPlan, value func(index int) float64) metrics.RangeResult {
	series := metrics.Series{Labels: map[string]string{}}
	index := 0
	for at := plan.effectiveStart; !at.After(plan.end); at = at.Add(plan.step) {
		if current := value(index); current >= 0 {
			series.Samples = append(series.Samples, metrics.Sample{Timestamp: at, Value: current})
		}
		index++
	}
	return metrics.RangeResult{Series: []metrics.Series{series}}
}

func perPod(at time.Time, values map[replicaKey]float64) metrics.RangeResult {
	result := metrics.RangeResult{}
	for key, value := range values {
		result.Series = append(result.Series, metrics.Series{Labels: map[string]string{"pod": key.pod, "uid": key.uid}, Samples: []metrics.Sample{{Timestamp: at, Value: value}}})
	}
	return result
}

type scenario struct {
	desired, running, cpu, memory, ambiguous, unattributed func(int) float64
	cpuValue, memoryValue                                  func(int) float64
	podRunning, podCPU, podMemory, ownedNow                map[replicaKey]float64
}

func (s scenario) prometheus(plan attributionPlan) *scriptedPrometheus {
	requests := plan.historyRequests()
	zero := func(int) float64 { return -1 }
	or := func(value, fallback func(int) float64) func(int) float64 {
		if value == nil {
			return fallback
		}
		return value
	}
	fake := &scriptedPrometheus{responses: map[string]metrics.RangeResult{
		requests.cpuMax.expression:       steps(plan, or(s.cpuValue, func(int) float64 { return 100 })),
		requests.memoryMax.expression:    steps(plan, or(s.memoryValue, func(int) float64 { return 1 << 20 })),
		requests.cpuCount.expression:     steps(plan, s.cpu),
		requests.memoryCount.expression:  steps(plan, s.memory),
		requests.runningCount.expression: steps(plan, s.running),
		requests.ambiguous.expression:    steps(plan, or(s.ambiguous, zero)),
		requests.unattributed.expression: steps(plan, or(s.unattributed, zero)),
		requests.podRunning.expression:   perPod(plan.end, s.podRunning),
		requests.podCPU.expression:       perPod(plan.end, s.podCPU),
		requests.podMemory.expression:    perPod(plan.end, s.podMemory),
		requests.ownedNow.expression:     perPod(plan.end, s.ownedNow),
	}}
	if requests.desired != nil && s.desired != nil {
		fake.responses[requests.desired.expression] = steps(plan, s.desired)
	}
	return fake
}

var (
	unitEnd    = time.Date(2026, 10, 1, 1, 0, 0, 0, time.UTC)
	unitWindow = Window{Duration: time.Hour, Step: 5 * time.Minute, End: unitEnd}
)

func unitPlan(t *testing.T, identity WorkloadIdentity) attributionPlan {
	t.Helper()
	plan, err := newAttributionPlan(subject, identity, unitWindow)
	if err != nil {
		t.Fatal(err)
	}
	return plan
}

func constantSteps(value float64) func(int) float64 { return func(int) float64 { return value } }

func livePods(identity WorkloadIdentity, value float64) map[replicaKey]float64 {
	values := map[replicaKey]float64{}
	for _, pod := range identity.Pods {
		values[replicaKey{pod: pod.Name, uid: pod.UID}] = value
	}
	return values
}

func runScenario(t *testing.T, identity WorkloadIdentity, s scenario) (History, error) {
	t.Helper()
	plan := unitPlan(t, identity)
	return NewSource(s.prometheus(plan)).HistoryForIdentity(context.Background(), subject, identity, unitWindow)
}

func completeScenario(identity WorkloadIdentity) scenario {
	replicas := float64(identity.ExpectedReplicas)
	return scenario{
		desired: constantSteps(replicas), running: constantSteps(replicas), cpu: constantSteps(replicas), memory: constantSteps(replicas),
		podRunning: livePods(identity, 13), podCPU: livePods(identity, 13), podMemory: livePods(identity, 13), ownedNow: livePods(identity, 1),
	}
}

func issuesOf(history History) string { return strings.Join(history.ReplicaCoverage.Issues, "; ") }

func TestHistoryCompleteWhenEveryExpectedReplicaReportsEveryStep(t *testing.T) {
	identity := attributedIdentity()
	history, err := runScenario(t, identity, completeScenario(identity))
	if err != nil {
		t.Fatal(err)
	}
	coverage := history.ReplicaCoverage
	if !coverage.Complete || coverage.Attribution != AttributionHistorical || coverage.CPUReplicaCoverage != 1 || coverage.IdentityCoverage != 1 || coverage.ActiveSteps != 13 || len(coverage.Timeline) != 13 {
		t.Fatalf("coverage = %+v", coverage)
	}
	if len(history.CPU.Points) != 13 || history.CPU.Quality.Coverage != 1 {
		t.Fatalf("busiest-replica CPU = %+v", history.CPU)
	}
}

func TestHistoryThreeExpectedOneReportingIsIncomplete(t *testing.T) {
	identity := attributedIdentity()
	identity.ExpectedReplicas = 3
	identity.Pods = append(identity.Pods, PodIdentity{Name: "web-api-new-ccccc", UID: "pod-uid-c", CreatedAt: identity.CreatedAt.Add(time.Hour), Phase: "Running", Containers: []string{"app"}})
	s := completeScenario(identity)
	s.cpu, s.memory = constantSteps(1), constantSteps(1)
	s.podCPU = map[replicaKey]float64{{pod: "web-api-new-aaaaa", uid: "pod-uid-a"}: 13}
	s.podMemory = s.podCPU
	history, err := runScenario(t, identity, s)
	if err != nil {
		t.Fatal(err)
	}
	coverage := history.ReplicaCoverage
	if coverage.Complete || coverage.MissingReplicas != 2 || coverage.CPUTemporalCoverage != 1 {
		t.Fatalf("coverage = %+v", coverage)
	}
	for _, required := range []string{"CPU metrics cover 33% of the expected replica-time", "memory metrics cover 33%", "2 attributed replicas ran without both CPU and memory metrics"} {
		if !strings.Contains(issuesOf(history), required) {
			t.Fatalf("issues lack %q: %s", required, issuesOf(history))
		}
	}
}

func TestHistoryDisjointReplicasAreNotCompleteFromTimestampUnion(t *testing.T) {
	identity := attributedIdentity()
	s := completeScenario(identity)
	// Each replica reports for half the window: every step has one sample.
	s.cpu, s.memory = constantSteps(1), constantSteps(1)
	s.podCPU, s.podMemory = livePods(identity, 6.5), livePods(identity, 6.5)
	history, err := runScenario(t, identity, s)
	if err != nil {
		t.Fatal(err)
	}
	coverage := history.ReplicaCoverage
	if coverage.Complete || coverage.CPUTemporalCoverage != 1 || coverage.CPUReplicaCoverage != 0.5 || !strings.Contains(issuesOf(history), "active replicas have incomplete per-replica history") {
		t.Fatalf("coverage = %+v", coverage)
	}
}

func TestHistoryScalingFromTwoToSixUsesPerStepExpectedPopulation(t *testing.T) {
	identity := attributedIdentity()
	identity.ExpectedReplicas = 6
	for index := 2; index < 6; index++ {
		identity.Pods = append(identity.Pods, PodIdentity{Name: fmt.Sprintf("web-api-new-%05d", index), UID: fmt.Sprintf("pod-uid-%d", index), CreatedAt: unitEnd.Add(-30 * time.Minute), Phase: "Running", Containers: []string{"app"}})
	}
	s := completeScenario(identity)
	population := func(index int) float64 {
		if index < 7 {
			return 2
		}
		return 6
	}
	s.desired, s.running, s.cpu, s.memory = population, population, population, population
	for index := 2; index < 6; index++ {
		key := replicaKey{pod: identity.Pods[index].Name, uid: identity.Pods[index].UID}
		s.podRunning[key], s.podCPU[key], s.podMemory[key] = 6, 6, 6
	}
	history, err := runScenario(t, identity, s)
	if err != nil {
		t.Fatal(err)
	}
	coverage := history.ReplicaCoverage
	if !coverage.Complete || coverage.PeakExpectedReplicas != 6 || coverage.MinimumExpectedReplicas != 2 || coverage.ScalingEvents != 1 {
		t.Fatalf("autoscaled coverage = %+v issues=%v", coverage, coverage.Issues)
	}
}

func TestHistoryRolloutSurgeCountsEveryRunningReplica(t *testing.T) {
	identity := attributedIdentity()
	s := completeScenario(identity)
	// During the surge three pods run against a desired count of two; only
	// two report, so the surge step is short by one replica.
	s.running = func(index int) float64 {
		if index == 6 {
			return 3
		}
		return 2
	}
	history, err := runScenario(t, identity, s)
	if err != nil {
		t.Fatal(err)
	}
	coverage := history.ReplicaCoverage
	if !coverage.Complete || coverage.CPUReplicaCoverage >= 1 || coverage.CPUReplicaCoverage < 0.96 {
		t.Fatalf("surge coverage = %+v", coverage)
	}
}

func TestHistoryMissingDesiredReplicaHistoryIsIncomplete(t *testing.T) {
	identity := attributedIdentity()
	s := completeScenario(identity)
	s.desired = func(index int) float64 {
		if index < 3 {
			return 2
		}
		return -1
	}
	history, err := runScenario(t, identity, s)
	if err != nil {
		t.Fatal(err)
	}
	if history.ReplicaCoverage.Complete || !strings.Contains(issuesOf(history), "desired replica count is recorded for 23% of the observed steps") {
		t.Fatalf("issues = %s", issuesOf(history))
	}
}

func TestHistoryScaledToZeroStepsAreExcluded(t *testing.T) {
	identity := attributedIdentity()
	identity.ExpectedReplicas = 1
	identity.Pods = identity.Pods[:1]
	s := completeScenario(identity)
	idle := func(value float64) func(int) float64 {
		return func(index int) float64 {
			if index >= 4 && index < 8 {
				if value == 0 {
					return 0
				}
				return -1
			}
			return value
		}
	}
	s.desired, s.running, s.cpu, s.memory = idle(0), idle(1), idle(1), idle(1)
	s.desired = func(index int) float64 {
		if index >= 4 && index < 8 {
			return 0
		}
		return 1
	}
	s.cpuValue, s.memoryValue = idle(100), idle(1<<20)
	s.podRunning, s.podCPU, s.podMemory = livePods(identity, 9), livePods(identity, 9), livePods(identity, 9)
	history, err := runScenario(t, identity, s)
	if err != nil {
		t.Fatal(err)
	}
	coverage := history.ReplicaCoverage
	if !coverage.Complete || coverage.ScaledToZeroSteps != 4 || coverage.ActiveSteps != 9 || history.CPU.Quality.ExpectedSamples != 9 || history.CPU.Quality.Coverage != 1 {
		t.Fatalf("coverage = %+v quality = %+v", coverage, history.CPU.Quality)
	}
}

func TestHistoryPrometheusOutageGapsAreIncomplete(t *testing.T) {
	identity := attributedIdentity()
	s := completeScenario(identity)
	outage := func(value float64) func(int) float64 {
		return func(index int) float64 {
			if index >= 3 && index < 9 {
				return -1
			}
			return value
		}
	}
	// Everything is missing during the outage, including kube-state-metrics.
	s.running, s.cpu, s.memory, s.cpuValue, s.memoryValue = outage(2), outage(2), outage(2), outage(100), outage(1<<20)
	s.podCPU, s.podMemory = livePods(identity, 7), livePods(identity, 7)
	history, err := runScenario(t, identity, s)
	if err != nil {
		t.Fatal(err)
	}
	for _, required := range []string{"exact CPU history covers 53% of the requested window", "attributed running pods cover 53%"} {
		if !strings.Contains(issuesOf(history), required) {
			t.Fatalf("issues lack %q: %s", required, issuesOf(history))
		}
	}
}

func TestHistoryCPUOnlyAndMemoryOnlyObservations(t *testing.T) {
	identity := attributedIdentity()
	s := completeScenario(identity)
	s.memory = constantSteps(0)
	s.podMemory = map[replicaKey]float64{}
	history, err := runScenario(t, identity, s)
	if err != nil {
		t.Fatal(err)
	}
	if history.ReplicaCoverage.Complete || !strings.Contains(issuesOf(history), "memory metrics cover 0% of the expected replica-time") || strings.Contains(issuesOf(history), "CPU metrics cover") {
		t.Fatalf("issues = %s", issuesOf(history))
	}
}

func TestHistoryLiveCrossChecksFailClosed(t *testing.T) {
	t.Run("live pod not attributed by kube-state-metrics", func(t *testing.T) {
		identity := attributedIdentity()
		s := completeScenario(identity)
		s.ownedNow = map[replicaKey]float64{{pod: "web-api-new-aaaaa", uid: "pod-uid-a"}: 1}
		history, err := runScenario(t, identity, s)
		if err != nil || !strings.Contains(issuesOf(history), "does not attribute 1 running live replicas") {
			t.Fatalf("err=%v issues=%s", err, issuesOf(history))
		}
	})
	t.Run("stale UID for a live pod name", func(t *testing.T) {
		identity := attributedIdentity()
		s := completeScenario(identity)
		s.ownedNow[replicaKey{pod: "web-api-new-aaaaa", uid: "old-uid"}] = 1
		history, err := runScenario(t, identity, s)
		if err != nil || !strings.Contains(issuesOf(history), "different UID than Kubernetes") {
			t.Fatalf("err=%v issues=%s", err, issuesOf(history))
		}
	})
	t.Run("adopted pod predates the workload object", func(t *testing.T) {
		identity := attributedIdentity()
		identity.Pods[0].CreatedAt = identity.CreatedAt.Add(-time.Hour)
		history, err := runScenario(t, identity, completeScenario(identity))
		if err != nil || !strings.Contains(issuesOf(history), "predate the current workload object") {
			t.Fatalf("err=%v issues=%s", err, issuesOf(history))
		}
	})
	t.Run("ambiguous and unattributed steps", func(t *testing.T) {
		identity := attributedIdentity()
		s := completeScenario(identity)
		s.ambiguous = func(index int) float64 { return map[bool]float64{true: 1, false: -1}[index == 2] }
		s.unattributed = func(index int) float64 { return map[bool]float64{true: 1, false: -1}[index < 3] }
		history, err := runScenario(t, identity, s)
		if err != nil {
			t.Fatal(err)
		}
		// One of 13 steps exceeds the 5% bound; the replay's StatefulSet case
		// covers the warning-only path (1 of 73 steps).
		if !strings.Contains(issuesOf(history), "7% of steps map one pod name to several UIDs") {
			t.Fatalf("ambiguous share above the bound should block: issues=%s notes=%v", issuesOf(history), history.Notes)
		}
		if !strings.Contains(issuesOf(history), "23% of steps contain usage from pods matching the workload's names") {
			t.Fatalf("unattributed usage should block: %s", issuesOf(history))
		}
	})
}

func TestHistoryUnattributableUsageIsAttributionIncompleteNotNoData(t *testing.T) {
	identity := attributedIdentity()
	plan := unitPlan(t, identity)
	requests := plan.historyRequests()
	fake := &scriptedPrometheus{responses: map[string]metrics.RangeResult{
		requests.unattributed.expression: steps(plan, constantSteps(2)),
		requests.desired.expression:      steps(plan, constantSteps(2)),
	}}
	_, err := NewSource(fake).HistoryForIdentity(context.Background(), subject, identity, unitWindow)
	if !errors.Is(err, ErrAttributionIncomplete) {
		t.Fatalf("err = %v", err)
	}
	empty := &scriptedPrometheus{responses: map[string]metrics.RangeResult{}}
	if _, err := NewSource(empty).HistoryForIdentity(context.Background(), subject, identity, unitWindow); !errors.Is(err, ErrNoData) {
		t.Fatalf("no usage at all should be ErrNoData, got %v", err)
	}
}

func TestHistoryPropagatesPrometheusFailure(t *testing.T) {
	identity := attributedIdentity()
	plan := unitPlan(t, identity)
	fake := completeScenario(identity).prometheus(plan)
	fake.failures = map[string]error{plan.historyRequests().runningCount.expression: metrics.ErrUpstream}
	if _, err := NewSource(fake).HistoryForIdentity(context.Background(), subject, identity, unitWindow); !errors.Is(err, metrics.ErrUpstream) {
		t.Fatalf("err = %v", err)
	}
}

func TestShortLivedWorkloadFailsTemporalCoverage(t *testing.T) {
	identity := attributedIdentity()
	identity.CreatedAt = unitEnd.Add(-20 * time.Minute)
	for index := range identity.Pods {
		identity.Pods[index].CreatedAt = identity.CreatedAt.Add(time.Minute)
	}
	plan := unitPlan(t, identity)
	s := completeScenario(identity)
	s.podRunning, s.podCPU, s.podMemory = livePods(identity, 5), livePods(identity, 5), livePods(identity, 5)
	history, err := NewSource(s.prometheus(plan)).HistoryForIdentity(context.Background(), subject, identity, unitWindow)
	if err != nil {
		t.Fatal(err)
	}
	if history.ReplicaCoverage.Complete || !strings.Contains(issuesOf(history), "exact CPU history covers 38% of the requested window") {
		t.Fatalf("issues = %s", issuesOf(history))
	}
}

func TestEventsIncludeReplacedPodsAndUseCreationAwareRestartBaselines(t *testing.T) {
	identity := attributedIdentity()
	plan := unitPlan(t, identity)
	requests := plan.eventRequests()
	live := replicaKey{pod: "web-api-new-aaaaa", uid: "pod-uid-a"}
	replaced := replicaKey{pod: "web-api-old-zzzzz", uid: "pod-uid-old"}
	fresh := replicaKey{pod: "web-api-new-bbbbb", uid: "pod-uid-b"}
	fake := &scriptedPrometheus{responses: map[string]metrics.RangeResult{
		requests.restartsMax.expression:  perPod(unitEnd, map[replicaKey]float64{live: 5, replaced: 3, fresh: 2}),
		requests.restartsMin.expression:  perPod(unitEnd, map[replicaKey]float64{live: 4, replaced: 3, fresh: 1}),
		requests.created.expression:      perPod(unitEnd, map[replicaKey]float64{live: float64(identity.CreatedAt.Unix()), fresh: float64(unitEnd.Add(-10 * time.Minute).Unix())}),
		requests.oom.expression:          perPod(unitEnd, map[replicaKey]float64{replaced: 1}),
		requests.throttledSum.expression: perPod(unitEnd, map[replicaKey]float64{live: 10, replaced: 30}),
		requests.periodsSum.expression:   perPod(unitEnd, map[replicaKey]float64{live: 100, replaced: 100}),
		requests.peak.expression:         perPod(unitEnd, map[replicaKey]float64{live: 0.2, replaced: 0.6}),
	}}
	events, err := NewSource(fake).EventsForIdentity(context.Background(), subject, identity, unitWindow)
	if err != nil {
		t.Fatal(err)
	}
	// live: 5-4; replaced: 0; fresh was created in the window, so all 2 count.
	if !events.Available || events.Restarts != 3 || events.OOMKills != 1 || events.ThrottlingRatio != 0.3 || events.ThrottlingPeakRatio != 0.6 || len(events.Replicas) != 3 {
		t.Fatalf("events = %+v", events)
	}
	for _, replica := range events.Replicas {
		if (replica.UID == "pod-uid-old") == replica.Live {
			t.Fatalf("live flag is wrong for %+v", replica)
		}
	}
}

func TestAttributedFingerprintChangesWithPodUIDModeAndCreation(t *testing.T) {
	identity := attributedIdentity()
	window := Window{Duration: time.Hour, Step: time.Minute, End: time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)}
	base, err := AttributedRecommendationQueryFingerprint(subject, identity, window)
	if err != nil || len(base) != 64 {
		t.Fatalf("fingerprint=%q err=%v", base, err)
	}
	variants := map[string]func(WorkloadIdentity) WorkloadIdentity{
		"pod uid": func(value WorkloadIdentity) WorkloadIdentity {
			value.Pods = append([]PodIdentity(nil), value.Pods...)
			value.Pods[0].UID = "replacement-pod-uid"
			return value
		},
		"mode": func(value WorkloadIdentity) WorkloadIdentity { value.LivePodsOnly = true; return value },
		"creation": func(value WorkloadIdentity) WorkloadIdentity {
			value.CreatedAt = value.CreatedAt.Add(time.Second)
			return value
		},
	}
	for name, change := range variants {
		other, err := AttributedRecommendationQueryFingerprint(subject, change(identity), window)
		if err != nil || other == base {
			t.Fatalf("%s must change the fingerprint: %q err=%v", name, other, err)
		}
	}
}

func TestForContainerFailsClosedWhenActivePodLacksContainer(t *testing.T) {
	identity := attributedIdentity()
	identity.Pods[1].Containers = []string{"sidecar"}
	filtered := identity.ForContainer("app")
	if len(filtered.Pods) != 1 || len(filtered.ResolutionIssues) != 1 || !strings.Contains(filtered.ResolutionIssues[0], "1 active") || len(identity.ResolutionIssues) != 0 {
		t.Fatalf("filtered identity = %#v", filtered)
	}
}

func TestAttributionQueriesStayWithinPrometheusExpressionBound(t *testing.T) {
	long := Subject{Namespace: "shop", Kind: "Deployment", Workload: strings.Repeat("a", 60) + "." + strings.Repeat("b", 60) + "." + strings.Repeat("c", 60) + "." + strings.Repeat("d", 60), Container: "app"}
	identity := attributedIdentity()
	identity.Pods = nil
	for _, value := range []Subject{subject, long} {
		fake := &scriptedPrometheus{responses: map[string]metrics.RangeResult{}}
		source := NewSource(fake)
		_, historyErr := source.HistoryForIdentity(context.Background(), value, identity, unitWindow)
		_, eventsErr := source.EventsForIdentity(context.Background(), value, identity, unitWindow)
		for _, err := range []error{historyErr, eventsErr} {
			if err != nil && !errors.Is(err, ErrAttributionIncomplete) && !errors.Is(err, ErrNoData) {
				t.Fatalf("%s: unexpected error %v", value.Workload, err)
			}
		}
		for _, expression := range fake.seen {
			if len(expression) > maximumExpressionBytes {
				t.Fatalf("an expression of %d bytes was sent", len(expression))
			}
		}
		if value == subject && (historyErr == nil || !errors.Is(historyErr, ErrNoData) || eventsErr != nil || len(fake.seen) == 0) {
			t.Fatalf("ordinary names must be queried: history=%v events=%v", historyErr, eventsErr)
		}
	}
}
