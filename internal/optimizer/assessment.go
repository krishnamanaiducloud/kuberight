package optimizer

import (
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/kuberight/kuberight/internal/stats"
)

// Recommendation statuses. Only "recommend" may be approved; the others
// explain why no safe change can be offered yet.
const (
	StatusRecommend        = "recommend"
	StatusHold             = "hold"
	StatusInsufficientData = "insufficient_data"
	StatusUnsupported      = "unsupported"
)

// Check outcomes.
const (
	CheckPass          = "pass"
	CheckFail          = "fail"
	CheckAdjusted      = "adjusted"
	CheckUnknown       = "unknown"
	CheckNotApplicable = "not_applicable"
)

// QoS classes.
const (
	QoSGuaranteed = "Guaranteed"
	QoSBurstable  = "Burstable"
	QoSBestEffort = "BestEffort"
)

const (
	maxValidity     = 24 * time.Hour
	reducedValidity = 12 * time.Hour
	shortValidity   = 6 * time.Hour
)

// MarginBreakdown explains a dynamic safety margin: the policy's base plus
// risk-driven additions, all as fractions of the sizing statistic.
type MarginBreakdown struct {
	Base        float64 `json:"base"`
	Variability float64 `json:"variability"`
	Burst       float64 `json:"burst"`
	Uncertainty float64 `json:"uncertainty"`
	Criticality float64 `json:"criticality"`
	Total       float64 `json:"total"`
}

// SafetyMargins are the margins applied to requests.
type SafetyMargins struct {
	CPU    MarginBreakdown `json:"cpu"`
	Memory MarginBreakdown `json:"memory"`
}

// WindowCheck replays recent sub-windows against the recommendation, so a
// recent rise that the full-window statistics dilute is caught.
type WindowCheck struct {
	Window            string   `json:"window"`
	Samples           int      `json:"samples"`
	CPUOverRequest    *float64 `json:"cpuOverRequest,omitempty"`
	MemoryOverRequest *float64 `json:"memoryOverRequest,omitempty"`
	MemoryOverLimit   *float64 `json:"memoryOverLimit,omitempty"`
	Passed            bool     `json:"passed"`
	Detail            string   `json:"detail,omitempty"`
}

// OutOfSample sizes from the earlier part of the history and measures the
// result against the later part it never saw.
type OutOfSample struct {
	Performed         bool     `json:"performed"`
	TrainSamples      int      `json:"trainSamples"`
	TestSamples       int      `json:"testSamples"`
	CPUOverRequest    *float64 `json:"cpuOverRequest,omitempty"`
	CPUOverLimit      *float64 `json:"cpuOverLimit,omitempty"`
	MemoryOverRequest *float64 `json:"memoryOverRequest,omitempty"`
	MemoryOverLimit   *float64 `json:"memoryOverLimit,omitempty"`
	Passed            bool     `json:"passed"`
	Detail            string   `json:"detail"`
}

// QoS reports the pod QoS class before and after the change.
type QoS struct {
	Current     string `json:"current"`
	Recommended string `json:"recommended"`
	Protected   bool   `json:"protected"`
	Approximate bool   `json:"approximate,omitempty"`
}

// Check is one compatibility check.
type Check struct {
	Name   string `json:"name"`
	Status string `json:"status"`
	Detail string `json:"detail"`
}

// HPAImpact projects how a CPU-utilization HPA reacts to the new request.
type HPAImpact struct {
	TargetUtilization      int32   `json:"targetUtilization"`
	CurrentUtilization     float64 `json:"currentUtilization"`
	ProjectedUtilization   float64 `json:"projectedUtilization"`
	CurrentReplicas        int32   `json:"currentReplicas"`
	CurrentDesiredReplicas int32   `json:"currentDesiredReplicas"`
	ProjectedReplicas      int32   `json:"projectedReplicas"`
	UnboundedReplicas      int32   `json:"unboundedReplicas"`
	MinReplicas            int32   `json:"minReplicas"`
	MaxReplicas            int32   `json:"maxReplicas"`
	Approximate            bool    `json:"approximate,omitempty"`
	Detail                 string  `json:"detail"`
}

// AutoscalingImpact summarizes every autoscaler's interaction.
type AutoscalingImpact struct {
	HPA  *HPAImpact `json:"hpa,omitempty"`
	KEDA string     `json:"keda,omitempty"`
	VPA  string     `json:"vpa,omitempty"`
}

// WorkloadTotals aggregate effective pod requests (init containers by their
// maximum, sidecars and application containers summed, plus overhead) and
// multiply by replicas, so nothing is counted twice.
type WorkloadTotals struct {
	Replicas                    int32 `json:"replicas"`
	PodCurrentCPUMilli          int64 `json:"podCurrentCpuMilli"`
	PodRecommendedCPUMilli      int64 `json:"podRecommendedCpuMilli"`
	PodCurrentMemoryBytes       int64 `json:"podCurrentMemoryBytes"`
	PodRecommendedMemoryBytes   int64 `json:"podRecommendedMemoryBytes"`
	TotalCurrentCPUMilli        int64 `json:"totalCurrentCpuMilli"`
	TotalRecommendedCPUMilli    int64 `json:"totalRecommendedCpuMilli"`
	TotalCurrentMemoryBytes     int64 `json:"totalCurrentMemoryBytes"`
	TotalRecommendedMemoryBytes int64 `json:"totalRecommendedMemoryBytes"`
	Approximate                 bool  `json:"approximate,omitempty"`
}

// DemandForecast is the predicted demand over the trend horizon.
type DemandForecast struct {
	Source            string   `json:"source"` // ml or trend
	Horizon           string   `json:"horizon"`
	CPUMilli          float64  `json:"cpuMilli"`
	CPUUpperMilli     float64  `json:"cpuUpperMilli,omitempty"`
	MemoryBytes       float64  `json:"memoryBytes"`
	MemoryUpperBytes  float64  `json:"memoryUpperBytes,omitempty"`
	IntervalLevel     float64  `json:"intervalLevel,omitempty"`
	EmpiricalCoverage *float64 `json:"empiricalCoverage,omitempty"`
	Calibrated        bool     `json:"calibrated"`
	UsedForSizing     bool     `json:"usedForSizing"`
}

// Savings is current minus recommended requests across all replicas;
// negative values are increases.
type Savings struct {
	CPUMilli    int64 `json:"cpuMilli"`
	MemoryBytes int64 `json:"memoryBytes"`
}

// Assessment is the evidence and verdict for a recommendation.
type Assessment struct {
	Status          string            `json:"status"`
	StatusReasons   []string          `json:"statusReasons"`
	RiskScore       float64           `json:"riskScore"`
	Current         Quantities        `json:"current"`
	Recommended     Quantities        `json:"recommended"`
	SafetyMargins   SafetyMargins     `json:"safetyMargins"`
	Seasonality     stats.Seasonality `json:"seasonality"`
	Validation      []WindowCheck     `json:"validation"`
	OutOfSample     OutOfSample       `json:"outOfSample"`
	QoS             QoS               `json:"qos"`
	Checks          []Check           `json:"checks"`
	Autoscaling     AutoscalingImpact `json:"autoscaling"`
	Workload        *WorkloadTotals   `json:"workload,omitempty"`
	Forecast        *DemandForecast   `json:"forecast,omitempty"`
	ExpectedSavings Savings           `json:"expectedSavings"`
	ValidFor        string            `json:"validFor"`
	ValidForSeconds int64             `json:"validForSeconds"`
	Explanation     []string          `json:"explanation"`
}

// dynamicMargin adds risk-driven headroom to the policy margin: more for
// variable usage, regular bursts, forecast uncertainty, and critical or
// stateful workloads. The additions are capped so one factor cannot dominate.
func dynamicMargin(base float64, samples []float64, burst bool, forecastWidth float64, input Input) MarginBreakdown {
	breakdown := MarginBreakdown{Base: base}
	if cv := stats.CoefficientOfVariation(samples); cv > 0.3 {
		breakdown.Variability = math.Min(0.25, (cv-0.3)*0.25)
	}
	if burst {
		breakdown.Burst = 0.10
	}
	if forecastWidth > 0 {
		breakdown.Uncertainty = math.Min(0.20, forecastWidth*0.5)
	}
	if input.Criticality == "high" {
		breakdown.Criticality += 0.10
	}
	if strings.EqualFold(input.Kind, "StatefulSet") {
		breakdown.Criticality += 0.05
	}
	for _, value := range []*float64{&breakdown.Variability, &breakdown.Burst, &breakdown.Uncertainty, &breakdown.Criticality} {
		*value = math.Round(*value*1000) / 1000
	}
	breakdown.Total = math.Round((base+breakdown.Variability+breakdown.Burst+breakdown.Uncertainty+breakdown.Criticality)*1000) / 1000
	return breakdown
}

// forecastWidth is the relative half-width of a forecast's interval.
func forecastWidth(point, upper float64) float64 {
	if point <= 0 || upper <= point {
		return 0
	}
	return (upper - point) / point
}

// calibrated reports whether the forecast's intervals covered at least the
// nominal level minus 10 points on held-out data.
func (forecast *ForecastContext) calibrated() bool {
	return forecast != nil && forecast.EmpiricalCoverage != nil && forecast.IntervalLevel > 0 && *forecast.EmpiricalCoverage >= forecast.IntervalLevel-0.10
}

// podQoS classifies a pod from all of its containers.
func podQoS(containers []ContainerResources) string {
	guaranteed, any := true, false
	for _, container := range containers {
		if container.CPURequestMilli > 0 || container.CPULimitMilli > 0 || container.MemoryRequestBytes > 0 || container.MemoryLimitBytes > 0 {
			any = true
		}
		cpuRequest := container.CPURequestMilli
		if cpuRequest == 0 {
			cpuRequest = container.CPULimitMilli
		}
		memoryRequest := container.MemoryRequestBytes
		if memoryRequest == 0 {
			memoryRequest = container.MemoryLimitBytes
		}
		if container.CPULimitMilli == 0 || container.MemoryLimitBytes == 0 || cpuRequest != container.CPULimitMilli || memoryRequest != container.MemoryLimitBytes {
			guaranteed = false
		}
	}
	switch {
	case !any:
		return QoSBestEffort
	case guaranteed:
		return QoSGuaranteed
	default:
		return QoSBurstable
	}
}

// podContainers returns every container that counts for QoS, with the
// recommended container's values replaced when a replacement is given.
func podContainers(input Input, replacement *ContainerResources) ([]ContainerResources, bool) {
	self := ContainerResources{Name: input.Container, CPURequestMilli: input.CPURequestsMilli, CPULimitMilli: input.CPULimitMilli, MemoryRequestBytes: input.MemoryRequestsBytes, MemoryLimitBytes: input.MemoryLimitBytes}
	if replacement != nil {
		self = *replacement
	}
	if input.Pod == nil {
		return []ContainerResources{self}, true
	}
	result := []ContainerResources{}
	found := false
	for _, container := range input.Pod.Containers {
		if container.Name == input.Container {
			result = append(result, self)
			found = true
			continue
		}
		result = append(result, container)
	}
	for _, init := range input.Pod.InitContainers {
		if init.Name == input.Container {
			result = append(result, self)
			found = true
			continue
		}
		result = append(result, init.ContainerResources)
	}
	if !found {
		result = append(result, self)
	}
	return result, false
}

// effectivePodRequests applies the Kubernetes rule: the pod requests the
// larger of its long-running containers (application containers plus native
// sidecars) and its peak init phase (each init container plus the sidecars
// started before it), plus RuntimeClass overhead.
func effectivePodRequests(input Input, replacement ContainerResources) (cpu, memory int64) {
	pick := func(container ContainerResources) ContainerResources {
		if container.Name == input.Container {
			return replacement
		}
		return container
	}
	if input.Pod == nil {
		return replacement.CPURequestMilli, replacement.MemoryRequestBytes
	}
	var runningCPU, runningMemory, sidecarCPU, sidecarMemory, initCPU, initMemory int64
	for _, container := range input.Pod.Containers {
		container = pick(container)
		runningCPU += container.CPURequestMilli
		runningMemory += container.MemoryRequestBytes
	}
	for _, init := range input.Pod.InitContainers {
		container := pick(init.ContainerResources)
		if init.Sidecar {
			sidecarCPU += container.CPURequestMilli
			sidecarMemory += container.MemoryRequestBytes
			continue
		}
		initCPU = max(initCPU, container.CPURequestMilli+sidecarCPU)
		initMemory = max(initMemory, container.MemoryRequestBytes+sidecarMemory)
	}
	cpu = max(runningCPU+sidecarCPU, initCPU) + input.Pod.OverheadCPUMilli
	memory = max(runningMemory+sidecarMemory, initMemory) + input.Pod.OverheadMemoryBytes
	return cpu, memory
}

type sizing struct {
	cpuRequest, cpuLimit, memoryRequest, memoryLimit int64
}

func (values sizing) resources(name string) ContainerResources {
	return ContainerResources{Name: name, CPURequestMilli: values.cpuRequest, CPULimitMilli: values.cpuLimit, MemoryRequestBytes: values.memoryRequest, MemoryLimitBytes: values.memoryLimit}
}

// protectQoS keeps a Guaranteed pod Guaranteed by setting each resource's
// request and limit to the larger recommended value. A policy that removes
// CPU limits opts out, because that change is meant to leave Guaranteed.
func protectQoS(input Input, values sizing, removeCPULimit bool) (sizing, QoS) {
	currentContainers, approximate := podContainers(input, nil)
	qos := QoS{Current: podQoS(currentContainers), Approximate: approximate}
	if qos.Current == QoSGuaranteed && !removeCPULimit {
		cpu := max(values.cpuRequest, values.cpuLimit)
		memory := max(values.memoryRequest, values.memoryLimit)
		if values.cpuRequest != cpu || values.cpuLimit != cpu || values.memoryRequest != memory || values.memoryLimit != memory {
			qos.Protected = true
		}
		values = sizing{cpuRequest: cpu, cpuLimit: cpu, memoryRequest: memory, memoryLimit: memory}
	}
	replacement := values.resources(input.Container)
	recommended, _ := podContainers(input, &replacement)
	qos.Recommended = podQoS(recommended)
	return values, qos
}

// applyLimitRanges raises values below LimitRange minimums and tightens
// limit-to-request ratios; violations that cannot be corrected safely fail.
func applyLimitRanges(input Input, values sizing) (sizing, []Check) {
	if input.NamespacePolicy == nil {
		return values, []Check{{Name: "LimitRange", Status: CheckUnknown, Detail: "namespace LimitRanges were not available"}}
	}
	if len(input.NamespacePolicy.LimitRanges) == 0 {
		return values, []Check{{Name: "LimitRange", Status: CheckNotApplicable, Detail: "the namespace has no LimitRange"}}
	}
	checks := []Check{}
	for _, limits := range input.NamespacePolicy.LimitRanges {
		name := "LimitRange " + limits.Name
		switch limits.Type {
		case "Container":
			adjusted, problems := []string{}, []string{}
			if limits.MinCPUMilli > 0 && values.cpuRequest < limits.MinCPUMilli {
				values.cpuRequest = limits.MinCPUMilli
				adjusted = append(adjusted, "CPU request raised to the minimum "+CPUQuantity(limits.MinCPUMilli))
			}
			if limits.MinMemoryBytes > 0 && values.memoryRequest < limits.MinMemoryBytes {
				values.memoryRequest = limits.MinMemoryBytes
				adjusted = append(adjusted, "memory request raised to the minimum "+MemoryQuantity(limits.MinMemoryBytes))
			}
			values.cpuLimit = max(values.cpuLimit, valueIf(values.cpuLimit > 0, values.cpuRequest))
			values.memoryLimit = max(values.memoryLimit, valueIf(values.memoryLimit > 0, values.memoryRequest))
			if limits.MaxCPUMilli > 0 && (values.cpuRequest > limits.MaxCPUMilli || values.cpuLimit > limits.MaxCPUMilli) {
				problems = append(problems, "CPU needs more than the maximum "+CPUQuantity(limits.MaxCPUMilli))
			}
			if limits.MaxMemoryBytes > 0 && (values.memoryRequest > limits.MaxMemoryBytes || values.memoryLimit > limits.MaxMemoryBytes) {
				problems = append(problems, "memory needs more than the maximum "+MemoryQuantity(limits.MaxMemoryBytes))
			}
			if ratio := limits.MaxCPULimitRequestRatio; ratio > 0 && values.cpuLimit > 0 && float64(values.cpuLimit) > float64(values.cpuRequest)*ratio {
				values.cpuRequest = int64(math.Ceil(float64(values.cpuLimit) / ratio))
				adjusted = append(adjusted, fmt.Sprintf("CPU request raised to keep the limit within %.2gx the request", ratio))
			}
			if ratio := limits.MaxMemoryLimitRequestRatio; ratio > 0 && values.memoryLimit > 0 && float64(values.memoryLimit) > float64(values.memoryRequest)*ratio {
				values.memoryRequest = int64(math.Ceil(float64(values.memoryLimit) / ratio))
				adjusted = append(adjusted, fmt.Sprintf("memory request raised to keep the limit within %.2gx the request", ratio))
			}
			switch {
			case len(problems) > 0:
				checks = append(checks, Check{name, CheckFail, strings.Join(problems, "; ")})
			case len(adjusted) > 0:
				checks = append(checks, Check{name, CheckAdjusted, strings.Join(adjusted, "; ")})
			default:
				checks = append(checks, Check{name, CheckPass, "within the container bounds"})
			}
		case "Pod":
			cpu, memory := effectivePodRequests(input, values.resources(input.Container))
			problems := []string{}
			if limits.MaxCPUMilli > 0 && cpu > limits.MaxCPUMilli {
				problems = append(problems, "pod CPU requests exceed "+CPUQuantity(limits.MaxCPUMilli))
			}
			if limits.MaxMemoryBytes > 0 && memory > limits.MaxMemoryBytes {
				problems = append(problems, "pod memory requests exceed "+MemoryQuantity(limits.MaxMemoryBytes))
			}
			if len(problems) > 0 {
				checks = append(checks, Check{name, CheckFail, strings.Join(problems, "; ")})
			} else {
				checks = append(checks, Check{name, CheckPass, "within the pod bounds"})
			}
		}
	}
	return values, checks
}

func valueIf(condition bool, value int64) int64 {
	if condition {
		return value
	}
	return 0
}

// quotaChecks fails when the added requests or limits across all replicas
// would exceed a ResourceQuota's remaining capacity.
func quotaChecks(input Input, values sizing) []Check {
	if input.NamespacePolicy == nil {
		return []Check{{Name: "ResourceQuota", Status: CheckUnknown, Detail: "namespace ResourceQuotas were not available"}}
	}
	if len(input.NamespacePolicy.Quotas) == 0 {
		return []Check{{Name: "ResourceQuota", Status: CheckNotApplicable, Detail: "the namespace has no ResourceQuota"}}
	}
	replicas := int64(max(1, input.Replicas))
	deltas := QuotaAmounts{
		RequestsCPUMilli:    (values.cpuRequest - input.CPURequestsMilli) * replicas,
		RequestsMemoryBytes: (values.memoryRequest - input.MemoryRequestsBytes) * replicas,
		LimitsCPUMilli:      (values.cpuLimit - input.CPULimitMilli) * replicas,
		LimitsMemoryBytes:   (values.memoryLimit - input.MemoryLimitBytes) * replicas,
	}
	checks := []Check{}
	for _, quota := range input.NamespacePolicy.Quotas {
		problems := []string{}
		for _, item := range []struct {
			label            string
			hard, used, need int64
			render           func(int64) string
		}{
			{"requests.cpu", quota.Hard.RequestsCPUMilli, quota.Used.RequestsCPUMilli, deltas.RequestsCPUMilli, CPUQuantity},
			{"requests.memory", quota.Hard.RequestsMemoryBytes, quota.Used.RequestsMemoryBytes, deltas.RequestsMemoryBytes, MemoryQuantity},
			{"limits.cpu", quota.Hard.LimitsCPUMilli, quota.Used.LimitsCPUMilli, deltas.LimitsCPUMilli, CPUQuantity},
			{"limits.memory", quota.Hard.LimitsMemoryBytes, quota.Used.LimitsMemoryBytes, deltas.LimitsMemoryBytes, MemoryQuantity},
		} {
			if item.hard >= 0 && item.need > 0 && item.used+item.need > item.hard {
				problems = append(problems, fmt.Sprintf("%s needs %s more but only %s remains", item.label, item.render(item.need), item.render(max(0, item.hard-item.used))))
			}
		}
		if len(problems) > 0 {
			checks = append(checks, Check{"ResourceQuota " + quota.Name, CheckFail, strings.Join(problems, "; ")})
		} else {
			checks = append(checks, Check{"ResourceQuota " + quota.Name, CheckPass, "the change fits the remaining quota"})
		}
	}
	return checks
}

// schedulingCheck requires at least one schedulable node to fit the pod's
// effective requests.
func schedulingCheck(input Input, values sizing) Check {
	if input.Nodes == nil {
		return Check{"Scheduling", CheckUnknown, "node capacity was not available"}
	}
	cpu, memory := effectivePodRequests(input, values.resources(input.Container))
	largestCPU, largestMemory := int64(0), int64(0)
	for _, node := range input.Nodes {
		if cpu <= node.CPUMilli && memory <= node.MemoryBytes {
			return Check{"Scheduling", CheckPass, fmt.Sprintf("the pod (%s CPU, %s memory) fits on node %s", CPUQuantity(cpu), MemoryQuantity(memory), node.Name)}
		}
		largestCPU, largestMemory = max(largestCPU, node.CPUMilli), max(largestMemory, node.MemoryBytes)
	}
	return Check{"Scheduling", CheckFail, fmt.Sprintf("no schedulable node fits the pod (%s CPU, %s memory); the largest node offers %s CPU and %s memory", CPUQuantity(cpu), MemoryQuantity(memory), CPUQuantity(largestCPU), MemoryQuantity(largestMemory))}
}

// hpaImpact projects the replica count a CPU-utilization HPA would choose
// with the new request, using mean usage as the utilization numerator.
func hpaImpact(input Input, meanCPU float64, recommendedRequest int64) *HPAImpact {
	hpa := input.HPA
	if hpa == nil || hpa.TargetCPUUtilization <= 0 || input.CPURequestsMilli <= 0 || recommendedRequest <= 0 {
		return nil
	}
	current := max(hpa.CurrentReplicas, 1)
	currentUtilization := meanCPU / float64(input.CPURequestsMilli) * 100
	projectedUtilization := meanCPU / float64(recommendedRequest) * 100
	desired := func(utilization float64) int32 {
		return int32(math.Ceil(float64(current) * utilization / float64(hpa.TargetCPUUtilization)))
	}
	unbounded := desired(projectedUtilization)
	projected := unbounded
	if hpa.MinReplicas > 0 {
		projected = max(projected, hpa.MinReplicas)
	}
	if hpa.MaxReplicas > 0 {
		projected = min(projected, hpa.MaxReplicas)
	}
	impact := &HPAImpact{
		TargetUtilization: hpa.TargetCPUUtilization, CurrentUtilization: math.Round(currentUtilization*10) / 10,
		ProjectedUtilization: math.Round(projectedUtilization*10) / 10, CurrentReplicas: current,
		CurrentDesiredReplicas: max(desired(currentUtilization), hpa.MinReplicas), ProjectedReplicas: projected, UnboundedReplicas: unbounded,
		MinReplicas: hpa.MinReplicas, MaxReplicas: hpa.MaxReplicas,
		Approximate: !hpa.ContainerMetric && input.Pod != nil && len(input.Pod.Containers) > 1,
	}
	impact.Detail = fmt.Sprintf("average utilization moves from %.0f%% to %.0f%% of the request against a %d%% target; the HPA would run about %d replicas instead of %d", impact.CurrentUtilization, impact.ProjectedUtilization, hpa.TargetCPUUtilization, projected, impact.CurrentDesiredReplicas)
	if hpa.MaxReplicas > 0 && unbounded > hpa.MaxReplicas {
		impact.Detail += fmt.Sprintf("; it would need %d, above maxReplicas %d, so capacity could run out", unbounded, hpa.MaxReplicas)
	}
	if impact.Approximate {
		impact.Detail += "; the HPA measures the whole pod, so this projection assumes the other containers' utilization is unchanged"
	}
	return impact
}

// windowChecks replays the most recent quarter, half, and the whole history.
func windowChecks(input Input, values sizing, policy Policy) []WindowCheck {
	n := min(len(input.CPUSamplesMilli), len(input.MemorySamplesBytes))
	checks := []WindowCheck{}
	cpuTolerance := (1 - policy.CPUPercentile) + 0.05
	memoryTolerance := (1 - policy.MemoryPercentile) + 0.02
	for _, share := range []float64{0.25, 0.5, 1} {
		count := int(math.Round(float64(n) * share))
		if count < 12 {
			continue
		}
		cpu, memory := input.CPUSamplesMilli[len(input.CPUSamplesMilli)-count:], input.MemorySamplesBytes[len(input.MemorySamplesBytes)-count:]
		check := WindowCheck{
			Window: windowLabel(count, input.SampleIntervalSeconds, share), Samples: count,
			CPUOverRequest: exceedance(cpu, values.cpuRequest), MemoryOverRequest: exceedance(memory, values.memoryRequest),
			MemoryOverLimit: exceedance(memory, values.memoryLimit), Passed: true,
		}
		problems := []string{}
		if check.CPUOverRequest != nil && *check.CPUOverRequest > cpuTolerance {
			problems = append(problems, fmt.Sprintf("CPU exceeded the request in %.0f%% of samples", *check.CPUOverRequest*100))
		}
		if check.MemoryOverRequest != nil && *check.MemoryOverRequest > memoryTolerance {
			problems = append(problems, fmt.Sprintf("memory exceeded the request in %.0f%% of samples", *check.MemoryOverRequest*100))
		}
		if check.MemoryOverLimit != nil && *check.MemoryOverLimit > 0 {
			problems = append(problems, "memory would have exceeded the limit")
		}
		if len(problems) > 0 {
			check.Passed, check.Detail = false, strings.Join(problems, "; ")
		}
		checks = append(checks, check)
	}
	return checks
}

func windowLabel(count int, intervalSeconds int64, share float64) string {
	if intervalSeconds <= 0 {
		if share == 1 {
			return "all samples"
		}
		return fmt.Sprintf("last %.0f%% of samples", share*100)
	}
	duration := time.Duration(int64(count)*intervalSeconds) * time.Second
	if share == 1 {
		return "full window (" + roundDuration(duration) + ")"
	}
	return "last " + roundDuration(duration)
}

func roundDuration(duration time.Duration) string {
	switch {
	case duration >= 48*time.Hour:
		return fmt.Sprintf("%.1fd", duration.Hours()/24)
	case duration >= time.Hour:
		return fmt.Sprintf("%.1fh", duration.Hours())
	default:
		return duration.Round(time.Minute).String()
	}
}

// outOfSample sizes from the first 70% of the samples and measures the
// result on the remaining 30%.
func outOfSample(input Input) OutOfSample {
	n := min(len(input.CPUSamplesMilli), len(input.MemorySamplesBytes))
	policy := DefaultPolicy()
	if input.Policy != nil {
		policy = normalizePolicy(*input.Policy)
	}
	if n < 2*policy.MinimumSamples {
		return OutOfSample{Detail: fmt.Sprintf("needs at least %d samples to hold some back; %d were available", 2*policy.MinimumSamples, n)}
	}
	split := int(float64(n) * 0.7)
	train := input
	train.CPUSamplesMilli, train.MemorySamplesBytes = input.CPUSamplesMilli[:split], input.MemorySamplesBytes[:split]
	train.skipAssessment = true
	recommendation, err := Recommend(train)
	if err != nil {
		return OutOfSample{Detail: "the held-back test could not be sized: " + err.Error()}
	}
	cpu, memory := input.CPUSamplesMilli[split:n], input.MemorySamplesBytes[split:n]
	result := OutOfSample{
		Performed: true, TrainSamples: split, TestSamples: n - split,
		CPUOverRequest: exceedance(cpu, recommendation.CPURequestMilli), CPUOverLimit: exceedance(cpu, recommendation.CPULimitMilli),
		MemoryOverRequest: exceedance(memory, recommendation.MemoryRequestBytes), MemoryOverLimit: exceedance(memory, recommendation.MemoryLimitBytes),
		Passed: true,
	}
	problems := []string{}
	if result.MemoryOverLimit != nil && *result.MemoryOverLimit > 0 {
		problems = append(problems, "memory sized from earlier data would have exceeded the limit later")
	}
	if result.MemoryOverRequest != nil && *result.MemoryOverRequest > 0.05 {
		problems = append(problems, fmt.Sprintf("memory sized from earlier data exceeded the request in %.0f%% of later samples", *result.MemoryOverRequest*100))
	}
	if result.CPUOverRequest != nil && *result.CPUOverRequest > 0.10 {
		problems = append(problems, fmt.Sprintf("CPU sized from earlier data exceeded the request in %.0f%% of later samples", *result.CPUOverRequest*100))
	}
	if len(problems) > 0 {
		result.Passed, result.Detail = false, strings.Join(problems, "; ")+"; usage is changing faster than the history shows"
	} else {
		result.Detail = fmt.Sprintf("sized from the first %d samples, the recommendation held on the %d later samples", split, n-split)
	}
	return result
}

// validity shortens the advice's lifetime for volatile or changing usage.
func validity(input Input, trends Trends, regularSpikes bool, cv float64) time.Duration {
	switch {
	case input.RecentRollout:
		return shortValidity
	case trends.CPU.Significant || trends.Memory.Significant || regularSpikes || cv > 1:
		return reducedValidity
	default:
		return maxValidity
	}
}

// facts are the intermediate results of Recommend that the assessment uses.
type facts struct {
	policy                                      Policy
	quality                                     DataQuality
	trends                                      Trends
	cpuStats, memoryStats                       Percentiles
	cpuStatistic                                float64
	regularSpikes, burstable, throttled, pinned bool
	cpuMargin, memoryMargin                     MarginBreakdown
	qos                                         QoS
	limitRangeChecks                            []Check
	forecastUsed                                bool
	confidenceScore                             float64
	risk                                        string
}

func assess(input Input, values sizing, fact facts) *Assessment {
	assessment := &Assessment{
		Current:       quantities(input.CPURequestsMilli, input.CPULimitMilli, input.MemoryRequestsBytes, input.MemoryLimitBytes),
		Recommended:   quantities(values.cpuRequest, values.cpuLimit, values.memoryRequest, values.memoryLimit),
		SafetyMargins: SafetyMargins{CPU: fact.cpuMargin, Memory: fact.memoryMargin},
		QoS:           fact.qos,
		Checks:        append(append([]Check{}, fact.limitRangeChecks...), quotaChecks(input, values)...),
		StatusReasons: []string{},
		Explanation:   []string{},
	}
	assessment.Checks = append(assessment.Checks, schedulingCheck(input, values))
	if input.SampleIntervalSeconds > 0 {
		step := time.Duration(input.SampleIntervalSeconds) * time.Second
		assessment.Seasonality = stats.DetectSeasonality(input.CPUSamplesMilli, []int{int(24 * time.Hour / step), int(7 * 24 * time.Hour / step)}, 0.5)
	}
	assessment.Validation = windowChecks(input, values, fact.policy)
	assessment.OutOfSample = outOfSample(input)

	meanCPU, _ := stats.Mean(input.CPUSamplesMilli)
	assessment.Autoscaling.HPA = hpaImpact(input, meanCPU, values.cpuRequest)
	switch {
	case input.Autoscaling.KEDAEnabled && input.Autoscaling.HPAUtilizationBased:
		assessment.Autoscaling.KEDA = "KEDA scales on CPU utilization here, so the request change moves its scaling decisions like an HPA"
	case input.Autoscaling.KEDAEnabled:
		assessment.Autoscaling.KEDA = "KEDA scales on event or external metrics, which a request change does not alter; check that new replicas fit the recommended requests"
	}
	switch {
	case input.Autoscaling.VPAControlsCPU && input.Autoscaling.VPAControlsMemory:
		assessment.Autoscaling.VPA = "VPA manages both CPU and memory; KubeRight leaves them unchanged to avoid fighting it"
	case input.Autoscaling.VPAControlsCPU:
		assessment.Autoscaling.VPA = "VPA manages CPU, which is left unchanged"
	case input.Autoscaling.VPAControlsMemory:
		assessment.Autoscaling.VPA = "VPA manages memory, which is left unchanged"
	}

	replicas := int64(max(1, input.Replicas))
	assessment.ExpectedSavings = Savings{
		CPUMilli:    (input.CPURequestsMilli - values.cpuRequest) * replicas,
		MemoryBytes: (input.MemoryRequestsBytes - values.memoryRequest) * replicas,
	}
	current := ContainerResources{Name: input.Container, CPURequestMilli: input.CPURequestsMilli, CPULimitMilli: input.CPULimitMilli, MemoryRequestBytes: input.MemoryRequestsBytes, MemoryLimitBytes: input.MemoryLimitBytes}
	podCPU, podMemory := effectivePodRequests(input, current)
	podRecommendedCPU, podRecommendedMemory := effectivePodRequests(input, values.resources(input.Container))
	assessment.Workload = &WorkloadTotals{
		Replicas: int32(replicas), PodCurrentCPUMilli: podCPU, PodRecommendedCPUMilli: podRecommendedCPU,
		PodCurrentMemoryBytes: podMemory, PodRecommendedMemoryBytes: podRecommendedMemory,
		TotalCurrentCPUMilli: podCPU * replicas, TotalRecommendedCPUMilli: podRecommendedCPU * replicas,
		TotalCurrentMemoryBytes: podMemory * replicas, TotalRecommendedMemoryBytes: podRecommendedMemory * replicas,
		Approximate: input.Pod == nil,
	}

	horizon := time.Duration(fact.policy.TrendHorizonDays) * 24 * time.Hour
	switch forecast := input.Forecast; {
	case forecast != nil:
		assessment.Forecast = &DemandForecast{
			Source: "ml", Horizon: roundDuration(forecast.Horizon), CPUMilli: forecast.CPUPeakMilli, CPUUpperMilli: forecast.CPUUpperMilli,
			MemoryBytes: forecast.MemoryPeakBytes, MemoryUpperBytes: forecast.MemoryUpperBytes, IntervalLevel: forecast.IntervalLevel,
			EmpiricalCoverage: forecast.EmpiricalCoverage, Calibrated: forecast.calibrated(), UsedForSizing: fact.forecastUsed,
		}
	case fact.trends.CPU.Significant || fact.trends.Memory.Significant:
		assessment.Forecast = &DemandForecast{
			Source: "trend", Horizon: roundDuration(horizon),
			CPUMilli:    fact.cpuStatistic + fact.trends.CPU.ProjectedUplift,
			MemoryBytes: fact.memoryStats.at(fact.policy.MemoryPercentile) + fact.trends.Memory.ProjectedUplift,
			Calibrated:  false, UsedForSizing: true,
		}
	}

	cv := stats.CoefficientOfVariation(input.CPUSamplesMilli)
	valid := validity(input, fact.trends, fact.regularSpikes, cv)
	assessment.ValidFor, assessment.ValidForSeconds = valid.String(), int64(valid/time.Second)

	// Verdict.
	failed := 0
	for _, check := range assessment.Checks {
		if check.Status == CheckFail {
			failed++
			assessment.StatusReasons = append(assessment.StatusReasons, check.Name+": "+check.Detail)
		}
	}
	for _, window := range assessment.Validation {
		if !window.Passed {
			failed++
			assessment.StatusReasons = append(assessment.StatusReasons, window.Window+": "+window.Detail)
		}
	}
	if assessment.OutOfSample.Performed && !assessment.OutOfSample.Passed {
		failed++
		assessment.StatusReasons = append(assessment.StatusReasons, "held-back test: "+assessment.OutOfSample.Detail)
	}
	if hpa := assessment.Autoscaling.HPA; hpa != nil && hpa.MaxReplicas > 0 && hpa.UnboundedReplicas > hpa.MaxReplicas {
		failed++
		assessment.StatusReasons = append(assessment.StatusReasons, "HPA: "+hpa.Detail)
	}
	switch {
	case input.Autoscaling.VPAControlsCPU && input.Autoscaling.VPAControlsMemory:
		assessment.Status = StatusUnsupported
		assessment.StatusReasons = []string{assessment.Autoscaling.VPA}
	case !fact.quality.Sufficient:
		assessment.Status = StatusInsufficientData
		assessment.StatusReasons = append([]string{}, fact.quality.Issues...)
	case failed > 0:
		assessment.Status = StatusHold
	default:
		assessment.Status = StatusRecommend
	}

	factors := 0
	for _, present := range []bool{fact.burstable, fact.throttled || fact.pinned, input.ObservedOOMKills > 0, input.Autoscaling.HPAUtilizationBased, fact.regularSpikes, fact.trends.CPU.Significant || fact.trends.Memory.Significant, cv > 1} {
		if present {
			factors++
		}
	}
	assessment.RiskScore = riskScore(fact.risk, fact.confidenceScore, factors, failed)

	explain := func(format string, arguments ...any) {
		assessment.Explanation = append(assessment.Explanation, fmt.Sprintf(format, arguments...))
	}
	explain("CPU is sized from %s usage (%s) plus a %.0f%% safety margin; memory from %s (%s) plus %.0f%%.",
		percentileLabel(fact.policy.CPUPercentile), CPUQuantity(int64(math.Ceil(fact.cpuStatistic))), fact.cpuMargin.Total*100,
		percentileLabel(fact.policy.MemoryPercentile), MemoryQuantity(int64(math.Ceil(fact.memoryStats.at(fact.policy.MemoryPercentile)))), fact.memoryMargin.Total*100)
	if added := fact.cpuMargin.Total - fact.cpuMargin.Base; added > 0 {
		explain("The CPU margin is %.0f points above the policy base because of usage variability, bursts, forecast uncertainty, or criticality.", added*100)
	}
	if fact.forecastUsed {
		explain("The calibrated forecast predicts higher demand than history, so requests cover the forecast peak.")
	}
	if fact.throttled || fact.pinned {
		explain("CPU was throttled or pinned at its limit, so measured CPU understates demand and CPU is not reduced.")
	}
	if input.ObservedOOMKills > 0 {
		explain("OOM kills were observed, so memory is not reduced.")
	}
	if fact.qos.Protected {
		explain("The pod is Guaranteed; requests equal limits so it stays Guaranteed.")
	} else if fact.qos.Current != fact.qos.Recommended {
		explain("The pod QoS class changes from %s to %s.", fact.qos.Current, fact.qos.Recommended)
	}
	switch assessment.Status {
	case StatusRecommend:
		explain("Every validation passed; the change can be reviewed. The advice is valid for %s.", assessment.ValidFor)
	case StatusHold:
		explain("Hold: keep the current settings until the failed checks are resolved.")
	case StatusInsufficientData:
		explain("Not enough reliable data for a safe change; reductions are blocked until coverage improves.")
	case StatusUnsupported:
		explain("KubeRight does not size resources that another controller manages.")
	}
	return assessment
}

// riskScore places the categorical risk on a 0-1 scale and orders
// recommendations within a category by the factors that drive it.
func riskScore(category string, confidenceScore float64, factors int, failedChecks int) float64 {
	low, high := 0.0, 0.33
	switch category {
	case "medium":
		low, high = 0.34, 0.66
	case "high":
		low, high = 0.67, 1.0
	}
	within := math.Min(1, (1-confidenceScore)*0.5+float64(factors)*0.1+float64(failedChecks)*0.2)
	return math.Round((low+(high-low)*within)*100) / 100
}
