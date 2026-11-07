package usage

// Historical attribution replay. The fixture below is synthetic, deterministic
// kube-state-metrics and cAdvisor history for workload lifecycles that the
// live-pod attribution could not represent. scripts/attribution-replay.sh
// writes it, backfills it into a real Prometheus with promtool, and runs
// TestAttributionReplayAgainstPrometheus against that server, so the PromQL
// joins are executed by Prometheus itself rather than asserted as strings.

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/kuberight/kuberight/internal/metrics"
)

const replayNamespace = "replay"

var (
	replayEnd    = time.Date(2026, 10, 1, 6, 0, 0, 0, time.UTC)
	replayStart  = replayEnd.Add(-6 * time.Hour)
	replayMiddle = replayStart.Add(3 * time.Hour)
	replayOrigin = replayStart.Add(-30 * time.Minute)
	replayWindow = Window{Duration: 6 * time.Hour, Step: 5 * time.Minute, End: replayEnd}
)

type replaySeries struct {
	labels  map[string]string
	samples map[int64]float64
}

type replayFixture struct {
	series map[string][]*replaySeries
}

func (fixture *replayFixture) add(metric string, labels map[string]string, from, to time.Time, value func(time.Time) float64) {
	if fixture.series == nil {
		fixture.series = map[string][]*replaySeries{}
	}
	series := &replaySeries{labels: labels, samples: map[int64]float64{}}
	for at := from.Truncate(time.Minute); !at.After(to); at = at.Add(time.Minute) {
		if at.Before(replayOrigin) {
			continue
		}
		series.samples[at.Unix()] = value(at)
	}
	fixture.series[metric] = append(fixture.series[metric], series)
}

func constant(value float64) func(time.Time) float64 { return func(time.Time) float64 { return value } }

// replayPod describes one pod incarnation and the usage of its "app"
// container while it runs.
type replayPod struct {
	name, uid, ownerKind, owner string
	from, to                    time.Time
	cpuMilli                    func(time.Time) float64
	memoryBytes                 float64
	noCAdvisor, noKSM, oom      bool
	reportFrom, reportTo        time.Time
	throttled                   float64
	// cpuRequestCores, when set, emits kube_pod_container_resource_requests.
	cpuRequestCores float64
}

func (fixture *replayFixture) pod(pod replayPod) {
	if !pod.noKSM {
		fixture.add("kube_pod_info", map[string]string{"namespace": replayNamespace, "pod": pod.name, "uid": pod.uid, "created_by_kind": pod.ownerKind, "created_by_name": pod.owner, "node": "node-a"}, pod.from, pod.to, constant(1))
		fixture.add("kube_pod_created", map[string]string{"namespace": replayNamespace, "pod": pod.name, "uid": pod.uid}, pod.from, pod.to, constant(float64(pod.from.Unix())))
		fixture.add("kube_pod_status_phase", map[string]string{"namespace": replayNamespace, "pod": pod.name, "uid": pod.uid, "phase": "Running"}, pod.from, pod.to, constant(1))
		fixture.add("kube_pod_container_status_restarts_total", map[string]string{"namespace": replayNamespace, "pod": pod.name, "uid": pod.uid, "container": "app"}, pod.from, pod.to, constant(0))
		if pod.cpuRequestCores > 0 {
			fixture.add("kube_pod_container_resource_requests", map[string]string{"namespace": replayNamespace, "pod": pod.name, "uid": pod.uid, "container": "app", "resource": "cpu", "unit": "core"}, pod.from, pod.to, constant(pod.cpuRequestCores))
		}
		if pod.oom {
			fixture.add("kube_pod_container_status_last_terminated_reason", map[string]string{"namespace": replayNamespace, "pod": pod.name, "uid": pod.uid, "container": "app", "reason": "OOMKilled"}, pod.from, pod.to, constant(1))
		}
	}
	if pod.noCAdvisor {
		return
	}
	from, to := pod.from, pod.to
	if !pod.reportFrom.IsZero() {
		from = pod.reportFrom
	}
	if !pod.reportTo.IsZero() {
		to = pod.reportTo
	}
	labels := map[string]string{"namespace": replayNamespace, "pod": pod.name, "container": "app"}
	cumulative := 0.0
	fixture.add("container_cpu_usage_seconds_total", labels, from, to, func(at time.Time) float64 {
		cumulative += pod.cpuMilli(at) / 1000 * 60
		return cumulative
	})
	fixture.add("container_memory_working_set_bytes", labels, from, to, constant(pod.memoryBytes))
	periods, throttled := 0.0, 0.0
	fixture.add("container_cpu_cfs_periods_total", labels, from, to, func(time.Time) float64 { periods += 600; return periods })
	fixture.add("container_cpu_cfs_throttled_periods_total", labels, from, to, func(time.Time) float64 { throttled += 600 * pod.throttled; return throttled })
}

func (fixture *replayFixture) replicaSet(deployment, name string, created, from, to time.Time) {
	fixture.add("kube_replicaset_owner", map[string]string{"namespace": replayNamespace, "replicaset": name, "owner_kind": "Deployment", "owner_name": deployment, "owner_is_controller": "true"}, from, to, constant(1))
	fixture.add("kube_replicaset_created", map[string]string{"namespace": replayNamespace, "replicaset": name}, from, to, constant(float64(created.Unix())))
}

func (fixture *replayFixture) desired(kind, label, name string, from, to time.Time, value func(time.Time) float64) {
	metric := map[string]string{"Deployment": "kube_deployment_spec_replicas", "StatefulSet": "kube_statefulset_replicas"}[kind]
	fixture.add(metric, map[string]string{"namespace": replayNamespace, label: name}, from, to, value)
}

// replayCreated is when every long-lived replay workload object was created.
var replayCreated = replayStart.Add(-24 * time.Hour)

func steadyPods(deployment, revision string, count int, from, to time.Time, cpu float64) []replayPod {
	pods := make([]replayPod, 0, count)
	for index := range count {
		pods = append(pods, replayPod{
			name: fmt.Sprintf("%s-%s-p%04d", deployment, revision, index), uid: fmt.Sprintf("%s-%s-%d", deployment, revision, index),
			ownerKind: "ReplicaSet", owner: deployment + "-" + revision, from: from, to: to,
			cpuMilli: constant(cpu), memoryBytes: 256 << 20,
		})
	}
	return pods
}

func buildReplayFixture() *replayFixture {
	fixture := &replayFixture{}
	all := func(pods ...[]replayPod) {
		for _, group := range pods {
			for _, pod := range group {
				fixture.pod(pod)
			}
		}
	}
	// steady: three replicas report for the whole window.
	fixture.replicaSet("steady", "steady-aaaa1", replayCreated, replayOrigin, replayEnd)
	fixture.desired("Deployment", "deployment", "steady", replayOrigin, replayEnd, constant(3))
	all(steadyPods("steady", "aaaa1", 3, replayOrigin, replayEnd, 200))

	// rollout: revision one runs until the middle, revision two afterwards.
	// The first revision used 900m and one of its pods was OOM-killed; both
	// facts would vanish if only the live pods were queried.
	fixture.replicaSet("rollout", "rollout-aaaa1", replayCreated, replayOrigin, replayMiddle.Add(5*time.Minute))
	fixture.replicaSet("rollout", "rollout-bbbb2", replayMiddle.Add(-2*time.Minute), replayMiddle.Add(-2*time.Minute), replayEnd)
	fixture.desired("Deployment", "deployment", "rollout", replayOrigin, replayEnd, constant(2))
	old := steadyPods("rollout", "aaaa1", 2, replayOrigin, replayMiddle.Add(2*time.Minute), 900)
	old[0].oom = true
	all(old, steadyPods("rollout", "bbbb2", 2, replayMiddle.Add(-2*time.Minute), replayEnd, 300))

	// scaledown: six replicas until the middle, then two. A removed replica
	// carried the peak.
	fixture.replicaSet("scaledown", "scaledown-aaaa1", replayCreated, replayOrigin, replayEnd)
	fixture.desired("Deployment", "deployment", "scaledown", replayOrigin, replayEnd, func(at time.Time) float64 {
		if at.Before(replayMiddle) {
			return 6
		}
		return 2
	})
	scaled := steadyPods("scaledown", "aaaa1", 6, replayOrigin, replayEnd, 250)
	for index := 2; index < 6; index++ {
		scaled[index].to = replayMiddle.Add(-time.Minute)
	}
	scaled[5].cpuMilli = constant(1200)
	all(scaled)

	// partial: three running replicas, only one exports cAdvisor metrics.
	fixture.replicaSet("partial", "partial-aaaa1", replayCreated, replayOrigin, replayEnd)
	fixture.desired("Deployment", "deployment", "partial", replayOrigin, replayEnd, constant(3))
	partial := steadyPods("partial", "aaaa1", 3, replayOrigin, replayEnd, 200)
	partial[1].noCAdvisor, partial[2].noCAdvisor = true, true
	all(partial)

	// disjoint: two running replicas, each reporting for only half of the
	// window. The union of timestamps is complete; the population is not.
	fixture.replicaSet("disjoint", "disjoint-aaaa1", replayCreated, replayOrigin, replayEnd)
	fixture.desired("Deployment", "deployment", "disjoint", replayOrigin, replayEnd, constant(2))
	disjoint := steadyPods("disjoint", "aaaa1", 2, replayOrigin, replayEnd, 200)
	disjoint[0].reportTo = replayMiddle
	disjoint[1].reportFrom = replayMiddle
	all(disjoint)

	// recreated: a same-name Deployment was deleted and recreated at the
	// middle. The old object's 5000m usage must never be attributed.
	fixture.replicaSet("recreated", "recreated-aaaa1", replayCreated, replayOrigin, replayMiddle.Add(-time.Minute))
	fixture.replicaSet("recreated", "recreated-bbbb2", replayMiddle, replayMiddle, replayEnd)
	fixture.desired("Deployment", "deployment", "recreated", replayOrigin, replayEnd, constant(2))
	all(steadyPods("recreated", "aaaa1", 2, replayOrigin, replayMiddle.Add(-time.Minute), 5000), steadyPods("recreated", "bbbb2", 2, replayMiddle.Add(time.Minute), replayEnd, 150))

	// sts: pod sts-0 is replaced at the middle with a new UID; the two
	// incarnations overlap for one minute in kube-state-metrics.
	fixture.desired("StatefulSet", "statefulset", "sts", replayOrigin, replayEnd, constant(2))
	all([]replayPod{
		{name: "sts-0", uid: "sts-0-old", ownerKind: "StatefulSet", owner: "sts", from: replayOrigin, to: replayMiddle, cpuMilli: constant(400), memoryBytes: 512 << 20, reportTo: replayMiddle.Add(-time.Minute)},
		{name: "sts-0", uid: "sts-0-new", ownerKind: "StatefulSet", owner: "sts", from: replayMiddle, to: replayEnd, cpuMilli: constant(350), memoryBytes: 512 << 20, noCAdvisor: true},
		{name: "sts-1", uid: "sts-1-only", ownerKind: "StatefulSet", owner: "sts", from: replayOrigin, to: replayEnd, cpuMilli: constant(300), memoryBytes: 512 << 20},
	})
	// The replacement's cAdvisor series continue under the same pod name, as
	// cAdvisor has no UID label; usage restarts from zero.
	fixture.pod(replayPod{name: "sts-0", uid: "unused", noKSM: true, from: replayMiddle.Add(time.Minute), to: replayEnd, cpuMilli: constant(350), memoryBytes: 512 << 20})

	// noksm: usage exists but kube-state-metrics never reported the pods.
	fixture.desired("Deployment", "deployment", "noksm", replayOrigin, replayEnd, constant(2))
	noksm := steadyPods("noksm", "aaaa1", 2, replayOrigin, replayEnd, 200)
	for index := range noksm {
		noksm[index].noKSM = true
	}
	all(noksm)

	// hpa: two replicas, scaled to six for the last quarter. New pods start
	// reporting a minute after they are created.
	fixture.replicaSet("hpa", "hpa-aaaa1", replayCreated, replayOrigin, replayEnd)
	scaleUp := replayEnd.Add(-90 * time.Minute)
	fixture.desired("Deployment", "deployment", "hpa", replayOrigin, replayEnd, func(at time.Time) float64 {
		if at.Before(scaleUp) {
			return 2
		}
		return 6
	})
	hpa := steadyPods("hpa", "aaaa1", 6, replayOrigin, replayEnd, 300)
	for index := 2; index < 6; index++ {
		hpa[index].from, hpa[index].reportFrom = scaleUp, scaleUp.Add(time.Minute)
	}
	all(hpa)

	// zero: scaled to zero for the middle two hours, as KEDA does.
	fixture.replicaSet("zero", "zero-aaaa1", replayCreated, replayOrigin, replayEnd)
	idleFrom, idleTo := replayStart.Add(2*time.Hour), replayStart.Add(4*time.Hour)
	fixture.desired("Deployment", "deployment", "zero", replayOrigin, replayEnd, func(at time.Time) float64 {
		if !at.Before(idleFrom) && at.Before(idleTo) {
			return 0
		}
		return 1
	})
	all([]replayPod{
		{name: "zero-aaaa1-p0000", uid: "zero-first", ownerKind: "ReplicaSet", owner: "zero-aaaa1", from: replayOrigin, to: idleFrom.Add(-time.Minute), cpuMilli: constant(100), memoryBytes: 128 << 20},
		{name: "zero-aaaa1-p0001", uid: "zero-second", ownerKind: "ReplicaSet", owner: "zero-aaaa1", from: idleTo, to: replayEnd, cpuMilli: constant(120), memoryBytes: 128 << 20, reportFrom: idleTo.Add(time.Minute)},
	})

	// resized: the CPU request drops from 1 core to 500m in a rollout at the
	// middle; the old pods are gone five minutes later.
	fixture.replicaSet("resized", "resized-aaaa1", replayCreated, replayOrigin, replayMiddle.Add(5*time.Minute))
	fixture.replicaSet("resized", "resized-bbbb2", replayMiddle.Add(-time.Minute), replayMiddle.Add(-time.Minute), replayEnd)
	fixture.desired("Deployment", "deployment", "resized", replayOrigin, replayEnd, constant(1))
	before, after := steadyPods("resized", "aaaa1", 1, replayOrigin, replayMiddle.Add(3*time.Minute), 300), steadyPods("resized", "bbbb2", 1, replayMiddle.Add(-time.Minute), replayEnd, 300)
	before[0].cpuRequestCores, after[0].cpuRequestCores = 1, 0.5
	all(before, after)

	// cron: two Jobs, each running one pod for twenty minutes.
	for index, at := range []time.Time{replayStart.Add(time.Hour), replayStart.Add(4 * time.Hour)} {
		job := fmt.Sprintf("cron-%d", 29000000+index)
		fixture.add("kube_job_owner", map[string]string{"namespace": replayNamespace, "job_name": job, "owner_kind": "CronJob", "owner_name": "cron", "owner_is_controller": "true"}, at, at.Add(30*time.Minute), constant(1))
		fixture.add("kube_job_created", map[string]string{"namespace": replayNamespace, "job_name": job}, at, at.Add(30*time.Minute), constant(float64(at.Unix())))
		// Pods start between evaluation steps, as real pods do, so every
		// evaluated step has at least two scrapes for rate().
		fixture.pod(replayPod{name: job + "-abcd" + strconv.Itoa(index), uid: "cron-pod-" + strconv.Itoa(index), ownerKind: "Job", owner: job, from: at.Add(2 * time.Minute), to: at.Add(22 * time.Minute), cpuMilli: constant(700), memoryBytes: 64 << 20})
	}
	return fixture
}

// OpenMetrics renders the fixture for promtool tsdb create-blocks-from.
func (fixture *replayFixture) OpenMetrics() string {
	var output strings.Builder
	names := make([]string, 0, len(fixture.series))
	for name := range fixture.series {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		for _, series := range fixture.series[name] {
			keys := make([]string, 0, len(series.labels))
			for key := range series.labels {
				keys = append(keys, key)
			}
			sort.Strings(keys)
			pairs := make([]string, 0, len(keys))
			for _, key := range keys {
				pairs = append(pairs, key+"="+strconv.Quote(series.labels[key]))
			}
			times := make([]int64, 0, len(series.samples))
			for at := range series.samples {
				times = append(times, at)
			}
			sort.Slice(times, func(left, right int) bool { return times[left] < times[right] })
			for _, at := range times {
				fmt.Fprintf(&output, "%s{%s} %s %d\n", name, strings.Join(pairs, ","), strconv.FormatFloat(series.samples[at], 'f', -1, 64), at)
			}
		}
	}
	output.WriteString("# EOF\n")
	return output.String()
}

func TestWriteAttributionReplayFixture(t *testing.T) {
	path := os.Getenv("KUBERIGHT_ATTRIBUTION_FIXTURE")
	if path == "" {
		t.Skip("set KUBERIGHT_ATTRIBUTION_FIXTURE to write the OpenMetrics replay fixture")
	}
	if err := os.WriteFile(path, []byte(buildReplayFixture().OpenMetrics()), 0o600); err != nil {
		t.Fatal(err)
	}
}

func replayIdentity(created time.Time, replicas int32, pods ...PodIdentity) WorkloadIdentity {
	return WorkloadIdentity{UID: "uid-" + strconv.FormatInt(created.Unix(), 10), Generation: 1, CreatedAt: created, ExpectedReplicas: replicas, Pods: pods}
}

func livePod(name, uid string, created time.Time) PodIdentity {
	return PodIdentity{Name: name, UID: uid, CreatedAt: created, Phase: "Running", Containers: []string{"app"}}
}

func replaySubject(kind, workload string) Subject {
	return Subject{Namespace: replayNamespace, Kind: kind, Workload: workload, Container: "app"}
}

func maxValue(series Series) float64 {
	maximum := math.Inf(-1)
	for _, point := range series.Points {
		maximum = math.Max(maximum, point.Value)
	}
	return maximum
}

func TestAttributionReplayAgainstPrometheus(t *testing.T) {
	endpoint := os.Getenv("KUBERIGHT_TEST_PROMETHEUS_URL")
	if endpoint == "" {
		t.Skip("set KUBERIGHT_TEST_PROMETHEUS_URL to a Prometheus loaded with the replay fixture (scripts/attribution-replay.sh)")
	}
	client, err := metrics.NewPrometheusClient(metrics.PrometheusOptions{Endpoint: endpoint, AllowHTTP: true, Timeout: 30 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	source := NewSource(client)
	ctx := context.Background()
	history := func(t *testing.T, kind, workload string, identity WorkloadIdentity) History {
		t.Helper()
		result, err := source.HistoryForIdentity(ctx, replaySubject(kind, workload), identity, replayWindow)
		if err != nil {
			t.Fatalf("history: %v", err)
		}
		t.Logf("%s coverage: %+v", workload, *result.ReplicaCoverage)
		return result
	}
	requireIssue := func(t *testing.T, coverage *ReplicaCoverage, text string) {
		t.Helper()
		if coverage.Complete || !strings.Contains(strings.Join(coverage.Issues, "; "), text) {
			t.Fatalf("expected incomplete coverage mentioning %q, got complete=%v issues=%v", text, coverage.Complete, coverage.Issues)
		}
	}

	t.Run("steady deployment is complete", func(t *testing.T) {
		result := history(t, "Deployment", "steady", replayIdentity(replayCreated, 3,
			livePod("steady-aaaa1-p0000", "steady-aaaa1-0", replayOrigin), livePod("steady-aaaa1-p0001", "steady-aaaa1-1", replayOrigin), livePod("steady-aaaa1-p0002", "steady-aaaa1-2", replayOrigin)))
		coverage := result.ReplicaCoverage
		if !coverage.Complete || coverage.DiscoveredReplicas != 3 || coverage.CPUReplicaCoverage < 0.99 || math.Abs(maxValue(result.CPU)-200) > 1 {
			t.Fatalf("coverage=%+v cpuMax=%v", coverage, maxValue(result.CPU))
		}
	})
	t.Run("rollout keeps replaced revision usage and OOM", func(t *testing.T) {
		identity := replayIdentity(replayCreated, 2, livePod("rollout-bbbb2-p0000", "rollout-bbbb2-0", replayMiddle), livePod("rollout-bbbb2-p0001", "rollout-bbbb2-1", replayMiddle))
		result := history(t, "Deployment", "rollout", identity)
		coverage := result.ReplicaCoverage
		if !coverage.Complete || coverage.Revisions != 2 || coverage.DiscoveredReplicas != 4 || math.Abs(maxValue(result.CPU)-900) > 1 {
			t.Fatalf("coverage=%+v cpuMax=%v", coverage, maxValue(result.CPU))
		}
		events, err := source.EventsForIdentity(ctx, replaySubject("Deployment", "rollout"), identity, replayWindow)
		if err != nil {
			t.Fatal(err)
		}
		if !events.Available || events.OOMKills != 1 || len(events.Replicas) != 4 || !events.ThrottlingAvailable {
			t.Fatalf("events=%+v", events)
		}
	})
	t.Run("scale-down keeps removed replica peak", func(t *testing.T) {
		result := history(t, "Deployment", "scaledown", replayIdentity(replayCreated, 2, livePod("scaledown-aaaa1-p0000", "scaledown-aaaa1-0", replayOrigin), livePod("scaledown-aaaa1-p0001", "scaledown-aaaa1-1", replayOrigin)))
		coverage := result.ReplicaCoverage
		if !coverage.Complete || coverage.PeakExpectedReplicas != 6 || coverage.MinimumExpectedReplicas != 2 || coverage.ScalingEvents != 1 || coverage.DiscoveredReplicas != 6 || math.Abs(maxValue(result.CPU)-1200) > 1 {
			t.Fatalf("coverage=%+v cpuMax=%v", coverage, maxValue(result.CPU))
		}
	})
	t.Run("one of three replicas reporting is incomplete", func(t *testing.T) {
		result := history(t, "Deployment", "partial", replayIdentity(replayCreated, 3, livePod("partial-aaaa1-p0000", "partial-aaaa1-0", replayOrigin), livePod("partial-aaaa1-p0001", "partial-aaaa1-1", replayOrigin), livePod("partial-aaaa1-p0002", "partial-aaaa1-2", replayOrigin)))
		requireIssue(t, result.ReplicaCoverage, "CPU metrics cover 33% of the expected replica-time")
		if result.ReplicaCoverage.MissingReplicas != 2 {
			t.Fatalf("missing=%d", result.ReplicaCoverage.MissingReplicas)
		}
	})
	t.Run("disjoint reporting is not complete coverage", func(t *testing.T) {
		result := history(t, "Deployment", "disjoint", replayIdentity(replayCreated, 2, livePod("disjoint-aaaa1-p0000", "disjoint-aaaa1-0", replayOrigin), livePod("disjoint-aaaa1-p0001", "disjoint-aaaa1-1", replayOrigin)))
		coverage := result.ReplicaCoverage
		if coverage.CPUTemporalCoverage < 0.99 {
			t.Fatalf("the busiest-replica series should be gap-free, temporal=%v", coverage.CPUTemporalCoverage)
		}
		requireIssue(t, coverage, "CPU metrics cover 5")
	})
	t.Run("same-name recreation excludes the previous object", func(t *testing.T) {
		result := history(t, "Deployment", "recreated", replayIdentity(replayMiddle, 2, livePod("recreated-bbbb2-p0000", "recreated-bbbb2-0", replayMiddle.Add(time.Minute)), livePod("recreated-bbbb2-p0001", "recreated-bbbb2-1", replayMiddle.Add(time.Minute))))
		if maxValue(result.CPU) > 200 {
			t.Fatalf("usage from the deleted same-name workload was attributed: %v", maxValue(result.CPU))
		}
		requireIssue(t, result.ReplicaCoverage, "exact CPU history covers")
		if coverage := result.ReplicaCoverage.CPUTemporalCoverage; coverage < 0.4 || coverage > 0.55 {
			t.Fatalf("temporal coverage %v should reflect only the half-window since recreation", coverage)
		}
	})
	t.Run("statefulset replacement keeps both incarnations", func(t *testing.T) {
		result := history(t, "StatefulSet", "sts", replayIdentity(replayCreated, 2, livePod("sts-0", "sts-0-new", replayMiddle), livePod("sts-1", "sts-1-only", replayOrigin)))
		coverage := result.ReplicaCoverage
		if coverage.DiscoveredReplicas != 3 || coverage.AmbiguousSteps == 0 || !coverage.Complete || math.Abs(maxValue(result.CPU)-400) > 1 {
			t.Fatalf("coverage=%+v cpuMax=%v", coverage, maxValue(result.CPU))
		}
	})
	t.Run("missing kube-state-metrics fails closed", func(t *testing.T) {
		_, err := source.HistoryForIdentity(ctx, replaySubject("Deployment", "noksm"), replayIdentity(replayCreated, 2, livePod("noksm-aaaa1-p0000", "noksm-aaaa1-0", replayOrigin)), replayWindow)
		if !errors.Is(err, ErrAttributionIncomplete) || !strings.Contains(err.Error(), "kube-state-metrics") {
			t.Fatalf("err=%v, want attribution incomplete because usage exists but nothing is attributable", err)
		}
	})
	t.Run("autoscaled replicas are measured over their own lifetime", func(t *testing.T) {
		pods := []PodIdentity{}
		for index := range 6 {
			created := replayOrigin
			if index >= 2 {
				created = scaleUpTime()
			}
			pods = append(pods, livePod(fmt.Sprintf("hpa-aaaa1-p%04d", index), fmt.Sprintf("hpa-aaaa1-%d", index), created))
		}
		result := history(t, "Deployment", "hpa", replayIdentity(replayCreated, 6, pods...))
		if !result.ReplicaCoverage.Complete || result.ReplicaCoverage.PeakExpectedReplicas != 6 {
			t.Fatalf("coverage=%+v", result.ReplicaCoverage)
		}
	})
	t.Run("scale to zero is excluded from coverage", func(t *testing.T) {
		result := history(t, "Deployment", "zero", replayIdentity(replayCreated, 1, livePod("zero-aaaa1-p0001", "zero-second", replayStart.Add(4*time.Hour))))
		coverage := result.ReplicaCoverage
		if !coverage.Complete || coverage.ScaledToZeroSteps < 20 || coverage.Revisions != 1 {
			t.Fatalf("coverage=%+v", coverage)
		}
	})
	t.Run("cronjob runs are attributed through their Jobs", func(t *testing.T) {
		result := history(t, "CronJob", "cron", replayIdentity(replayCreated, 0))
		coverage := result.ReplicaCoverage
		if !coverage.Complete || coverage.Revisions != 2 || coverage.DiscoveredReplicas != 2 || math.Abs(maxValue(result.CPU)-700) > 1 {
			t.Fatalf("coverage=%+v cpuMax=%v", coverage, maxValue(result.CPU))
		}
	})
	t.Run("resource change is dated when the old configuration is gone", func(t *testing.T) {
		identity := replayIdentity(replayCreated, 1, livePod("resized-bbbb2-p0000", "resized-bbbb2-0", replayMiddle))
		changed, err := source.ResourceChangedAt(ctx, replaySubject("Deployment", "resized"), identity, replayWindow)
		if err != nil || changed == nil || changed.Before(replayMiddle) || changed.After(replayMiddle.Add(10*time.Minute)) {
			t.Fatalf("changed=%v err=%v, want shortly after %v", changed, err, replayMiddle)
		}
		steady := replayIdentity(replayCreated, 3, livePod("steady-aaaa1-p0000", "steady-aaaa1-0", replayOrigin))
		if unchanged, err := source.ResourceChangedAt(ctx, replaySubject("Deployment", "steady"), steady, replayWindow); err != nil || unchanged != nil {
			t.Fatalf("steady workload reported a change: %v err=%v", unchanged, err)
		}
	})
	t.Run("live-pods mode excludes the replaced revision", func(t *testing.T) {
		identity := replayIdentity(replayCreated, 2, livePod("rollout-bbbb2-p0000", "rollout-bbbb2-0", replayMiddle), livePod("rollout-bbbb2-p0001", "rollout-bbbb2-1", replayMiddle))
		identity.LivePodsOnly = true
		result := history(t, "Deployment", "rollout", identity)
		if result.ReplicaCoverage.Attribution != AttributionLivePods || result.ReplicaCoverage.DiscoveredReplicas != 2 || maxValue(result.CPU) > 301 {
			t.Fatalf("coverage=%+v cpuMax=%v", result.ReplicaCoverage, maxValue(result.CPU))
		}
	})
}

func scaleUpTime() time.Time { return replayEnd.Add(-90 * time.Minute) }
