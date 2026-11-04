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
	maximumAttributedPods  = 256
	minimumReplicaCoverage = 0.80
	maximumSelectorBytes   = 6 << 10
	// Throttling divides two exact expressions, so each chunk's selectors are
	// bounded to half the Prometheus client's expression-size limit.
	maximumChunkSelectorBytes = 3 << 10
)

var (
	// ErrAttributionIncomplete means metric samples cannot be proven to belong
	// to the current workload UID. Callers must not fall back to pod-name
	// inference because a recreated same-name workload could then be mixed in.
	ErrAttributionIncomplete = errors.New("workload metric attribution is incomplete")
	uidToken                 = regexp.MustCompile(`^[A-Za-z0-9_.:-]+$`)
)

// PodIdentity is immutable Kubernetes identity and lifecycle information for
// one pod controlled by a workload. UIDs, rather than generated pod names,
// are the authority used by attributed PromQL queries.
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

// WorkloadIdentity is the exact, live Kubernetes identity used to authorize
// metric attribution. ResolutionIssues are fail-closed: they are returned in
// replica coverage and prevent a resource reduction.
type WorkloadIdentity struct {
	UID              string        `json:"workloadUid"`
	Generation       int64         `json:"workloadGeneration,omitempty"`
	CreatedAt        time.Time     `json:"workloadCreatedAt"`
	ExpectedReplicas int32         `json:"expectedReplicas"`
	Pods             []PodIdentity `json:"pods"`
	ResolutionIssues []string      `json:"resolutionIssues,omitempty"`
}

// ForContainer retains only pods whose spec contains the requested
// container. Missing containers remain an explicit resolution issue.
func (identity WorkloadIdentity) ForContainer(container string) WorkloadIdentity {
	filtered := identity
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

// ReplicaObservation retains resource history independently for every exact
// pod UID. Aggregated recommendation samples are derived from these records,
// never the other way around.
type ReplicaObservation struct {
	Pod       string     `json:"pod"`
	UID       string     `json:"uid"`
	Phase     string     `json:"phase,omitempty"`
	Active    bool       `json:"active"`
	CreatedAt time.Time  `json:"createdAt"`
	DeletedAt *time.Time `json:"deletedAt,omitempty"`
	CPU       Series     `json:"cpu"`
	Memory    Series     `json:"memory"`
}

// ReplicaCoverage explains whether the workload's replica population is
// complete enough to justify a reduction.
type ReplicaCoverage struct {
	ExpectedReplicas       int32                `json:"expectedReplicas"`
	DiscoveredReplicas     int                  `json:"discoveredReplicas"`
	ObservedReplicas       int                  `json:"observedReplicas"`
	MissingReplicas        int                  `json:"missingReplicas"`
	ActiveReplicas         int                  `json:"activeReplicas"`
	TerminatedReplicas     int                  `json:"terminatedReplicas"`
	CPUObservedReplicas    int                  `json:"cpuObservedReplicas"`
	MemoryObservedReplicas int                  `json:"memoryObservedReplicas"`
	CPUReplicaCoverage     float64              `json:"cpuReplicaCoverage"`
	MemoryReplicaCoverage  float64              `json:"memoryReplicaCoverage"`
	CPUTemporalCoverage    float64              `json:"cpuTemporalCoverage"`
	MemoryTemporalCoverage float64              `json:"memoryTemporalCoverage"`
	CPUImbalanceRatio      float64              `json:"cpuImbalanceRatio,omitempty"`
	MemoryImbalanceRatio   float64              `json:"memoryImbalanceRatio,omitempty"`
	Complete               bool                 `json:"complete"`
	Issues                 []string             `json:"issues"`
	Observations           []ReplicaObservation `json:"observations,omitempty"`
}

// ReplicaEvents preserves restart, OOM, and throttling evidence by pod UID.
type ReplicaEvents struct {
	Pod                 string  `json:"pod"`
	UID                 string  `json:"uid"`
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
	if identity.ExpectedReplicas == 0 {
		return fmt.Errorf("%w: the workload is scaled to zero", ErrAttributionIncomplete)
	}
	if len(identity.Pods) == 0 {
		return fmt.Errorf("%w: no exact pod UIDs were resolved", ErrAttributionIncomplete)
	}
	if len(identity.Pods) > maximumAttributedPods {
		return fmt.Errorf("%w: %d pods exceed the exact-query limit of %d", ErrAttributionIncomplete, len(identity.Pods), maximumAttributedPods)
	}
	seenNames, seenUIDs := map[string]bool{}, map[string]bool{}
	for _, pod := range identity.Pods {
		if len(pod.Name) > 253 || !dnsSubdomain.MatchString(pod.Name) || pod.UID == "" || len(pod.UID) > 128 || !uidToken.MatchString(pod.UID) || pod.CreatedAt.IsZero() {
			return fmt.Errorf("%w: pod identity is missing a valid name, UID, or creation timestamp", ErrAttributionIncomplete)
		}
		if pod.CreatedAt.Before(identity.CreatedAt) {
			return fmt.Errorf("%w: pod %q predates the current workload UID", ErrAttributionIncomplete, pod.Name)
		}
		if seenNames[pod.Name] || seenUIDs[pod.UID] {
			return fmt.Errorf("%w: duplicate pod name or UID", ErrAttributionIncomplete)
		}
		seenNames[pod.Name], seenUIDs[pod.UID] = true, true
	}
	return nil
}

func identityMatchers(subject Subject, identity WorkloadIdentity) (containerSelector, podInfo string, err error) {
	if err = subject.Validate(); err != nil {
		return "", "", err
	}
	if err = validateIdentity(identity); err != nil {
		return "", "", err
	}
	pods, uids := make([]string, 0, len(identity.Pods)), make([]string, 0, len(identity.Pods))
	for _, pod := range identity.Pods {
		pods = append(pods, regexp.QuoteMeta(pod.Name))
		uids = append(uids, regexp.QuoteMeta(pod.UID))
	}
	sort.Strings(pods)
	sort.Strings(uids)
	containerSelector = "namespace=" + strconv.Quote(subject.Namespace) + ",pod=~" + strconv.Quote(strings.Join(pods, "|")) + ",container=" + strconv.Quote(subject.Container)
	podInfo = "max by (namespace, pod, uid) (kube_pod_info{namespace=" + strconv.Quote(subject.Namespace) + ",pod=~" + strconv.Quote(strings.Join(pods, "|")) + ",uid=~" + strconv.Quote(strings.Join(uids, "|")) + "})"
	if len(containerSelector)+len(podInfo) > maximumSelectorBytes {
		return "", "", fmt.Errorf("%w: exact pod selectors exceed %d bytes", ErrAttributionIncomplete, maximumSelectorBytes)
	}
	return containerSelector, podInfo, nil
}

func identityChunks(subject Subject, identity WorkloadIdentity) ([]WorkloadIdentity, error) {
	if err := validateIdentity(identity); err != nil {
		return nil, err
	}
	chunks := make([]WorkloadIdentity, 0, 1)
	current := identity
	current.Pods = nil
	for _, pod := range identity.Pods {
		candidate := current
		candidate.Pods = append(append([]PodIdentity(nil), current.Pods...), pod)
		selector, podInfo, err := identityMatchers(subject, candidate)
		if err != nil {
			return nil, err
		}
		if len(selector)+len(podInfo) > maximumChunkSelectorBytes && len(current.Pods) > 0 {
			chunks = append(chunks, current)
			current = identity
			current.Pods = []PodIdentity{pod}
			selector, podInfo, err = identityMatchers(subject, current)
			if err != nil || len(selector)+len(podInfo) > maximumChunkSelectorBytes {
				return nil, fmt.Errorf("%w: pod identity is too large for an exact query", ErrAttributionIncomplete)
			}
			continue
		}
		current = candidate
	}
	if len(current.Pods) > 0 {
		chunks = append(chunks, current)
	}
	return chunks, nil
}

func exactMetric(metric, selector, podInfo, operation string) string {
	return operation + " by (pod, uid) (" + operation + " by (namespace, pod) (" + metric + "{" + selector + "}) * on (namespace, pod) group_left (uid) " + podInfo + ")"
}

// AttributedCPUExpression returns one CPU series per exact pod UID. The
// kube_pod_info join prevents samples from an older same-name pod from being
// attributed to a recreated StatefulSet or other controller.
func AttributedCPUExpression(subject Subject, identity WorkloadIdentity, step time.Duration) (string, error) {
	selector, podInfo, err := identityMatchers(subject, identity)
	if err != nil {
		return "", err
	}
	metric := "rate(container_cpu_usage_seconds_total{" + selector + "}[" + rateWindow(step) + "])"
	return "max by (pod, uid) (max by (namespace, pod) (" + metric + ") * on (namespace, pod) group_left (uid) " + podInfo + ") * 1000", nil
}

// AttributedMemoryExpression returns one working-set series per exact pod UID.
func AttributedMemoryExpression(subject Subject, identity WorkloadIdentity, step time.Duration) (string, error) {
	selector, podInfo, err := identityMatchers(subject, identity)
	if err != nil {
		return "", err
	}
	metric := "max_over_time(container_memory_working_set_bytes{" + selector + "}[" + promDuration(step) + "])"
	return "max by (pod, uid) (max by (namespace, pod) (" + metric + ") * on (namespace, pod) group_left (uid) " + podInfo + ")", nil
}

func attributedCounterExpression(metric string, subject Subject, identity WorkloadIdentity, lookback time.Duration) (string, error) {
	selector, podInfo, err := identityMatchers(subject, identity)
	if err != nil {
		return "", err
	}
	return "sum by (pod, uid) (sum by (namespace, pod) (increase(" + metric + "{" + selector + "}[" + promDuration(lookback) + "])) * on (namespace, pod) group_left (uid) " + podInfo + ")", nil
}

func attributedOOMExpression(subject Subject, identity WorkloadIdentity, lookback time.Duration) (string, error) {
	selector, podInfo, err := identityMatchers(subject, identity)
	if err != nil {
		return "", err
	}
	return "sum by (pod, uid) (max by (namespace, pod) (max_over_time(kube_pod_container_status_last_terminated_reason{" + selector + `,reason="OOMKilled"}[` + promDuration(lookback) + "])) * on (namespace, pod) group_left (uid) " + podInfo + ")", nil
}

func attributedThrottlingExpression(subject Subject, identity WorkloadIdentity, lookback time.Duration) (string, error) {
	numerator, err := attributedCounterExpression("container_cpu_cfs_throttled_periods_total", subject, identity, lookback)
	if err != nil {
		return "", err
	}
	denominator, err := attributedCounterExpression("container_cpu_cfs_periods_total", subject, identity, lookback)
	if err != nil {
		return "", err
	}
	return numerator + " / " + denominator, nil
}

// AttributedRecommendationQueryFingerprint binds recommendation provenance to
// the workload UID and exact pod UID population as well as the query window.
func AttributedRecommendationQueryFingerprint(subject Subject, identity WorkloadIdentity, window Window) (string, error) {
	chunks, err := identityChunks(subject, identity)
	if err != nil {
		return "", err
	}
	pairs := make([]string, 0, len(identity.Pods))
	for _, pod := range identity.Pods {
		pairs = append(pairs, pod.Name+"="+pod.UID)
	}
	sort.Strings(pairs)
	end := window.End.UTC().Truncate(time.Second)
	parts := []string{
		"kuberight-recommendation-queries/exact-v2", identity.UID, identity.CreatedAt.UTC().Format(time.RFC3339Nano),
		strings.Join(pairs, ","),
		strconv.FormatInt(end.Add(-window.Duration).Unix(), 10), strconv.FormatInt(end.Unix(), 10), strconv.FormatInt(int64(window.Step), 10),
	}
	for _, chunk := range chunks {
		cpu, expressionErr := AttributedCPUExpression(subject, chunk, window.Step)
		if expressionErr != nil {
			return "", expressionErr
		}
		memory, expressionErr := AttributedMemoryExpression(subject, chunk, window.Step)
		if expressionErr != nil {
			return "", expressionErr
		}
		parts = append(parts, cpu, memory)
	}
	digest := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return hex.EncodeToString(digest[:]), nil
}

type replicaKey struct{ pod, uid string }

func identityIndex(identity WorkloadIdentity) map[replicaKey]PodIdentity {
	index := make(map[replicaKey]PodIdentity, len(identity.Pods))
	for _, pod := range identity.Pods {
		index[replicaKey{pod: pod.Name, uid: pod.UID}] = pod
	}
	return index
}

func qualityForPoints(points []Point, window Window, duplicate, anomalous int) metrics.DataQuality {
	expected := 0
	if window.Step > 0 && window.Duration >= 0 {
		expected = int(window.Duration/window.Step) + 1
	}
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
		staleAfter := max(5*time.Minute, 2*window.Step)
		if last.Before(window.End.Add(-staleAfter)) {
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

func replicaSeries(resource, unit string, result metrics.RangeResult, identity WorkloadIdentity, window Window) map[replicaKey]Series {
	allowed := identityIndex(identity)
	points := map[replicaKey]map[time.Time]float64{}
	duplicates, anomalies := map[replicaKey]int{}, map[replicaKey]int{}
	for _, raw := range result.Series {
		key := replicaKey{pod: raw.Labels["pod"], uid: raw.Labels["uid"]}
		if _, ok := allowed[key]; !ok {
			continue
		}
		if points[key] == nil {
			points[key] = map[time.Time]float64{}
		}
		duplicates[key] += raw.Quality.DuplicateSamples
		anomalies[key] += raw.Quality.AnomalousSamples
		for _, sample := range raw.Samples {
			if current, exists := points[key][sample.Timestamp]; !exists || sample.Value > current {
				points[key][sample.Timestamp] = sample.Value
			}
		}
	}
	converted := make(map[replicaKey]Series, len(points))
	for key, values := range points {
		series := Series{Resource: resource, Unit: unit, Points: make([]Point, 0, len(values))}
		for at, value := range values {
			series.Points = append(series.Points, Point{Timestamp: at, Value: value})
		}
		sort.Slice(series.Points, func(left, right int) bool {
			return series.Points[left].Timestamp.Before(series.Points[right].Timestamp)
		})
		series.Quality = qualityForPoints(series.Points, window, duplicates[key], anomalies[key])
		converted[key] = series
	}
	return converted
}

func aggregateReplicaSeries(resource, unit string, replicas map[replicaKey]Series, window Window) Series {
	values := map[time.Time]float64{}
	duplicates, anomalies := 0, 0
	for _, replica := range replicas {
		duplicates += replica.Quality.DuplicateSamples
		anomalies += replica.Quality.AnomalousSamples
		for _, point := range replica.Points {
			if current, ok := values[point.Timestamp]; !ok || point.Value > current {
				values[point.Timestamp] = point.Value
			}
		}
	}
	series := Series{Resource: resource, Unit: unit, Points: make([]Point, 0, len(values))}
	for at, value := range values {
		series.Points = append(series.Points, Point{Timestamp: at, Value: value})
	}
	sort.Slice(series.Points, func(left, right int) bool {
		return series.Points[left].Timestamp.Before(series.Points[right].Timestamp)
	})
	series.Quality = qualityForPoints(series.Points, window, duplicates, anomalies)
	return series
}

func p95(points []Point) float64 {
	if len(points) == 0 {
		return 0
	}
	values := make([]float64, len(points))
	for index, point := range points {
		values[index] = point.Value
	}
	sort.Float64s(values)
	position := int(math.Ceil(0.95*float64(len(values)))) - 1
	return values[max(0, position)]
}

func imbalance(observations []ReplicaObservation, selectSeries func(ReplicaObservation) Series) float64 {
	minimum, maximum := math.Inf(1), 0.0
	for _, observation := range observations {
		value := p95(selectSeries(observation).Points)
		if value <= 0 {
			continue
		}
		minimum, maximum = math.Min(minimum, value), math.Max(maximum, value)
	}
	if math.IsInf(minimum, 1) || minimum == 0 {
		return 0
	}
	return math.Round(maximum/minimum*100) / 100
}

func coverage(identity WorkloadIdentity, cpu, memory map[replicaKey]Series, window Window) ReplicaCoverage {
	result := ReplicaCoverage{ExpectedReplicas: identity.ExpectedReplicas, DiscoveredReplicas: len(identity.Pods), Issues: append([]string{}, identity.ResolutionIssues...), Observations: make([]ReplicaObservation, 0, len(identity.Pods))}
	observed := map[replicaKey]bool{}
	for _, pod := range identity.Pods {
		key := replicaKey{pod: pod.Name, uid: pod.UID}
		cpuSeries := cpu[key]
		if cpuSeries.Resource == "" {
			cpuSeries = Series{Resource: "cpu", Unit: "millicores", Points: []Point{}, Quality: qualityForPoints(nil, window, 0, 0)}
		}
		memorySeries := memory[key]
		if memorySeries.Resource == "" {
			memorySeries = Series{Resource: "memory", Unit: "bytes", Points: []Point{}, Quality: qualityForPoints(nil, window, 0, 0)}
		}
		active := pod.Active()
		if active {
			result.ActiveReplicas++
		} else {
			result.TerminatedReplicas++
		}
		if len(cpuSeries.Points) > 0 {
			result.CPUObservedReplicas++
			observed[key] = true
		}
		if len(memorySeries.Points) > 0 {
			result.MemoryObservedReplicas++
			observed[key] = true
		}
		result.Observations = append(result.Observations, ReplicaObservation{Pod: pod.Name, UID: pod.UID, Phase: pod.Phase, Active: active, CreatedAt: pod.CreatedAt, DeletedAt: pod.DeletedAt, CPU: cpuSeries, Memory: memorySeries})
	}
	result.ObservedReplicas = len(observed)
	result.MissingReplicas = max(0, int(identity.ExpectedReplicas)-result.ObservedReplicas)
	if identity.ExpectedReplicas > 0 {
		denominator := float64(identity.ExpectedReplicas)
		result.CPUReplicaCoverage = math.Min(1, float64(result.CPUObservedReplicas)/denominator)
		result.MemoryReplicaCoverage = math.Min(1, float64(result.MemoryObservedReplicas)/denominator)
	}
	cpuAggregate, memoryAggregate := aggregateReplicaSeries("cpu", "millicores", cpu, window), aggregateReplicaSeries("memory", "bytes", memory, window)
	result.CPUTemporalCoverage, result.MemoryTemporalCoverage = cpuAggregate.Quality.Coverage, memoryAggregate.Quality.Coverage
	result.CPUImbalanceRatio = imbalance(result.Observations, func(item ReplicaObservation) Series { return item.CPU })
	result.MemoryImbalanceRatio = imbalance(result.Observations, func(item ReplicaObservation) Series { return item.Memory })
	if result.ActiveReplicas < int(identity.ExpectedReplicas) {
		result.Issues = append(result.Issues, fmt.Sprintf("only %d of %d expected replicas are active", result.ActiveReplicas, identity.ExpectedReplicas))
	}
	if result.CPUObservedReplicas < int(identity.ExpectedReplicas) {
		result.Issues = append(result.Issues, fmt.Sprintf("CPU metrics cover %d of %d expected replicas", result.CPUObservedReplicas, identity.ExpectedReplicas))
	}
	if result.MemoryObservedReplicas < int(identity.ExpectedReplicas) {
		result.Issues = append(result.Issues, fmt.Sprintf("memory metrics cover %d of %d expected replicas", result.MemoryObservedReplicas, identity.ExpectedReplicas))
	}
	if result.CPUTemporalCoverage < minimumReplicaCoverage {
		result.Issues = append(result.Issues, fmt.Sprintf("exact CPU history covers %.0f%% of the requested window", result.CPUTemporalCoverage*100))
	}
	if result.MemoryTemporalCoverage < minimumReplicaCoverage {
		result.Issues = append(result.Issues, fmt.Sprintf("exact memory history covers %.0f%% of the requested window", result.MemoryTemporalCoverage*100))
	}
	for _, observation := range result.Observations {
		if !observation.Active {
			continue
		}
		if observation.CPU.Quality.Coverage < minimumReplicaCoverage || observation.Memory.Quality.Coverage < minimumReplicaCoverage {
			result.Issues = append(result.Issues, "one or more active replicas have incomplete per-replica history")
			break
		}
	}
	result.Complete = len(result.Issues) == 0
	return result
}

// HistoryForIdentity reads CPU and memory as independent per-pod UID series,
// derives the conservative busiest-replica series, and records completeness.
func (source *Source) HistoryForIdentity(ctx context.Context, subject Subject, identity WorkloadIdentity, window Window) (History, error) {
	if window.End.IsZero() {
		window.End = source.now()
	}
	window.End = window.End.UTC().Truncate(time.Second)
	if !window.End.After(identity.CreatedAt) {
		return History{}, fmt.Errorf("%w: observation window ends before the current workload was created", ErrAttributionIncomplete)
	}
	chunks, err := identityChunks(subject, identity)
	if err != nil {
		return History{}, err
	}
	start := window.End.Add(-window.Duration)
	queries := []struct {
		resource   string
		expression string
		result     metrics.RangeResult
		err        error
	}{}
	for _, chunk := range chunks {
		cpuExpression, expressionErr := AttributedCPUExpression(subject, chunk, window.Step)
		if expressionErr != nil {
			return History{}, expressionErr
		}
		memoryExpression, expressionErr := AttributedMemoryExpression(subject, chunk, window.Step)
		if expressionErr != nil {
			return History{}, expressionErr
		}
		queries = append(queries, struct {
			resource   string
			expression string
			result     metrics.RangeResult
			err        error
		}{resource: "cpu", expression: cpuExpression}, struct {
			resource   string
			expression string
			result     metrics.RangeResult
			err        error
		}{resource: "memory", expression: memoryExpression})
	}
	var wait sync.WaitGroup
	for index := range queries {
		wait.Add(1)
		go func(index int) {
			defer wait.Done()
			queries[index].result, queries[index].err = source.client.QueryRange(ctx, metrics.RangeQuery{Expression: queries[index].expression, Start: start, End: window.End, Step: window.Step})
		}(index)
	}
	wait.Wait()
	errorsByQuery := make([]error, 0, len(queries))
	// NoData is true only when every chunk for that resource reported no data.
	// RangeResult's zero value is false, so initialize the conjunction identity
	// explicitly before combining chunk results.
	cpuResult, memoryResult := metrics.RangeResult{NoData: true}, metrics.RangeResult{NoData: true}
	for _, query := range queries {
		errorsByQuery = append(errorsByQuery, query.err)
		destination := &cpuResult
		if query.resource == "memory" {
			destination = &memoryResult
		}
		destination.Series = append(destination.Series, query.result.Series...)
		destination.Warnings = append(destination.Warnings, query.result.Warnings...)
		destination.NoData = destination.NoData && query.result.NoData
	}
	if err := errors.Join(errorsByQuery...); err != nil {
		return History{}, err
	}
	cpu := replicaSeries("cpu", "millicores", cpuResult, identity, window)
	memory := replicaSeries("memory", "bytes", memoryResult, identity, window)
	replicaCoverage := coverage(identity, cpu, memory, window)
	history := History{
		Subject: subject, Start: start, End: window.End, Step: window.Step.String(), Warnings: []string{},
		CPU: aggregateReplicaSeries("cpu", "millicores", cpu, window), Memory: aggregateReplicaSeries("memory", "bytes", memory, window),
		ReplicaCoverage: &replicaCoverage,
	}
	history.Warnings = append(history.Warnings, cpuResult.Warnings...)
	history.Warnings = append(history.Warnings, memoryResult.Warnings...)
	if len(history.CPU.Points) == 0 && len(history.Memory.Points) == 0 {
		return history, ErrNoData
	}
	return history, nil
}

func instantReplicaValues(ctx context.Context, source *Source, expression string, at time.Time) (map[replicaKey]float64, error) {
	result, err := source.client.QueryRange(ctx, metrics.RangeQuery{Expression: expression, Start: at, End: at, Step: time.Minute})
	if err != nil {
		return nil, err
	}
	values := map[replicaKey]float64{}
	for _, series := range result.Series {
		if len(series.Samples) == 0 {
			continue
		}
		key := replicaKey{pod: series.Labels["pod"], uid: series.Labels["uid"]}
		values[key] = series.Samples[len(series.Samples)-1].Value
	}
	return values, nil
}

// EventsForIdentity reads restart, OOM, and throttling evidence per exact pod
// UID. Workload-level values use the worst replica for conservative sizing.
func (source *Source) EventsForIdentity(ctx context.Context, subject Subject, identity WorkloadIdentity, window Window) (Events, error) {
	if window.End.IsZero() {
		window.End = source.now()
	}
	end := window.End.UTC().Truncate(time.Second)
	chunks, err := identityChunks(subject, identity)
	if err != nil {
		return Events{}, err
	}
	type eventQuery struct {
		kind       int
		expression string
		values     map[replicaKey]float64
		err        error
	}
	queries := []eventQuery{}
	for _, chunk := range chunks {
		restartsExpression, expressionErr := attributedCounterExpression("kube_pod_container_status_restarts_total", subject, chunk, window.Duration)
		if expressionErr != nil {
			return Events{}, expressionErr
		}
		oomExpression, expressionErr := attributedOOMExpression(subject, chunk, window.Duration)
		if expressionErr != nil {
			return Events{}, expressionErr
		}
		throttlingExpression, expressionErr := attributedThrottlingExpression(subject, chunk, window.Duration)
		if expressionErr != nil {
			return Events{}, expressionErr
		}
		peakExpression := "quantile_over_time(0.95, (" + throttlingExpression + ")[" + promDuration(window.Duration) + ":" + promDuration(window.Step) + "])"
		for kind, expression := range []string{restartsExpression, oomExpression, throttlingExpression, peakExpression} {
			queries = append(queries, eventQuery{kind: kind, expression: expression})
		}
	}
	var wait sync.WaitGroup
	for index := range queries {
		wait.Add(1)
		go func(index int) {
			defer wait.Done()
			queries[index].values, queries[index].err = instantReplicaValues(ctx, source, queries[index].expression, end)
		}(index)
	}
	wait.Wait()
	results := []map[replicaKey]float64{{}, {}, {}, {}}
	errorsByQuery := make([]error, 0, len(queries))
	for _, query := range queries {
		errorsByQuery = append(errorsByQuery, query.err)
		for key, value := range query.values {
			results[query.kind][key] = value
		}
	}
	if err := errors.Join(errorsByQuery...); err != nil {
		return Events{}, err
	}
	events := Events{Available: len(results[0]) > 0, Replicas: []ReplicaEvents{}}
	for _, pod := range identity.Pods {
		key := replicaKey{pod: pod.Name, uid: pod.UID}
		replica := ReplicaEvents{Pod: pod.Name, UID: pod.UID}
		if value, ok := results[0][key]; ok {
			replica.Restarts = int64(math.Round(math.Max(0, value)))
			events.Restarts += replica.Restarts
		}
		if value, ok := results[1][key]; ok {
			replica.OOMKills = int64(math.Round(math.Max(0, value)))
			events.OOMKills += replica.OOMKills
		}
		if value, ok := results[2][key]; ok && !math.IsNaN(value) && !math.IsInf(value, 0) {
			replica.ThrottlingAvailable = true
			replica.ThrottlingRatio = math.Min(1, math.Max(0, value))
			events.ThrottlingAvailable = true
			events.ThrottlingRatio = math.Max(events.ThrottlingRatio, replica.ThrottlingRatio)
		}
		if value, ok := results[3][key]; ok && !math.IsNaN(value) && !math.IsInf(value, 0) {
			replica.ThrottlingPeakRatio = math.Min(1, math.Max(0, value))
			events.ThrottlingPeakRatio = math.Max(events.ThrottlingPeakRatio, replica.ThrottlingPeakRatio)
		}
		events.Replicas = append(events.Replicas, replica)
	}
	return events, nil
}
