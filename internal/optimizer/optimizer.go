// Package optimizer implements deterministic, explainable resource recommendations.
// It never mutates Kubernetes resources.
package optimizer

import (
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
)

const (
	maxSamples     = 100_000
	maxCPUMilli    = int64(1_000_000_000)
	maxMemoryBytes = int64(math.MaxInt64 / 4)
)

type AutoscalingContext struct {
	HPAUtilizationBased bool `json:"hpaUtilizationBased,omitempty"`
	VPAControlsCPU      bool `json:"vpaControlsCpu,omitempty"`
	VPAControlsMemory   bool `json:"vpaControlsMemory,omitempty"`
	KEDAEnabled         bool `json:"kedaEnabled,omitempty"`
}

type Policy struct {
	CPUSafetyMargin       float64 `json:"cpuSafetyMargin"`
	MemorySafetyMargin    float64 `json:"memorySafetyMargin"`
	LimitSafetyMargin     float64 `json:"limitSafetyMargin"`
	MinimumCPUMilli       int64   `json:"minimumCpuMilli"`
	MinimumMemoryBytes    int64   `json:"minimumMemoryBytes"`
	MaximumCPUMilli       int64   `json:"maximumCpuMilli"`
	MaximumMemoryBytes    int64   `json:"maximumMemoryBytes"`
	MaximumReduction      float64 `json:"maximumReduction"`
	MinimumSamples        int     `json:"minimumSamples"`
	HighConfidenceSamples int     `json:"highConfidenceSamples"`
	// CPUPercentile and MemoryPercentile select the usage statistic that
	// requests are sized from: 0.9, 0.95, 0.99, or 1 for the observed
	// maximum. Zero selects the defaults, 0.95 and 0.99, so policies stored
	// before these fields existed keep their original behavior.
	CPUPercentile    float64 `json:"cpuPercentile,omitempty"`
	MemoryPercentile float64 `json:"memoryPercentile,omitempty"`
	// TrendHorizonDays is how far a significant rising trend is projected when
	// sizing, from 1 to 30 days. Zero selects 7.
	TrendHorizonDays int `json:"trendHorizonDays,omitempty"`
}

const (
	defaultCPUPercentile    = 0.95
	defaultMemoryPercentile = 0.99
	// throttlingThreshold is the share of CFS periods in which a container
	// was throttled above which CPU is treated as constrained, not idle.
	throttlingThreshold       = 0.05
	severeThrottlingThreshold = 0.25
)

func DefaultPolicy() Policy {
	return Policy{
		CPUSafetyMargin:       0.30,
		MemorySafetyMargin:    0.25,
		LimitSafetyMargin:     0.50,
		MinimumCPUMilli:       50,
		MinimumMemoryBytes:    64 * 1024 * 1024,
		MaximumCPUMilli:       maxCPUMilli,
		MaximumMemoryBytes:    maxMemoryBytes,
		MaximumReduction:      0.20,
		MinimumSamples:        30,
		HighConfidenceSamples: 500,
		CPUPercentile:         defaultCPUPercentile,
		MemoryPercentile:      defaultMemoryPercentile,
		TrendHorizonDays:      defaultTrendHorizonDays,
	}
}

// Profile is a named, built-in policy.
type Profile struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Policy      Policy `json:"policy"`
}

// Profiles returns the built-in policies, most cautious first.
func Profiles() []Profile {
	conservative := DefaultPolicy()
	conservative.CPUPercentile, conservative.MemoryPercentile = 0.99, 1
	conservative.CPUSafetyMargin, conservative.MemorySafetyMargin, conservative.LimitSafetyMargin = 0.40, 0.30, 0.75
	conservative.MaximumReduction = 0.10
	conservative.MinimumSamples, conservative.HighConfidenceSamples = 60, 1000

	aggressive := DefaultPolicy()
	aggressive.CPUPercentile, aggressive.MemoryPercentile = 0.90, 0.99
	aggressive.CPUSafetyMargin, aggressive.MemorySafetyMargin, aggressive.LimitSafetyMargin = 0.15, 0.15, 0.30
	aggressive.MaximumReduction = 0.40

	return []Profile{
		{Name: "conservative", Description: "P99 CPU and peak memory with wide margins; at most 10% reduction per change. For latency-critical or stateful services.", Policy: conservative},
		{Name: "balanced", Description: "P95 CPU and P99 memory with moderate margins; at most 20% reduction per change. The default.", Policy: DefaultPolicy()},
		{Name: "aggressive", Description: "P90 CPU and P99 memory with narrow margins; up to 40% reduction per change. For batch, development, and cost-sensitive workloads.", Policy: aggressive},
	}
}

// ProfileByName returns a built-in policy.
func ProfileByName(name string) (Policy, bool) {
	for _, profile := range Profiles() {
		if profile.Name == name {
			return profile.Policy, true
		}
	}
	return Policy{}, false
}

func normalizePolicy(policy Policy) Policy {
	if policy.CPUPercentile == 0 {
		policy.CPUPercentile = defaultCPUPercentile
	}
	if policy.MemoryPercentile == 0 {
		policy.MemoryPercentile = defaultMemoryPercentile
	}
	if policy.TrendHorizonDays == 0 {
		policy.TrendHorizonDays = defaultTrendHorizonDays
	}
	return policy
}

func validPercentile(value float64) bool {
	return value == 0.9 || value == 0.95 || value == 0.99 || value == 1
}

// at returns the statistic for a supported percentile.
func (stats Percentiles) at(q float64) float64 {
	switch q {
	case 0.9:
		return stats.P90
	case 0.95:
		return stats.P95
	case 0.99:
		return stats.P99
	default:
		return stats.Max
	}
}

func percentileLabel(q float64) string {
	if q == 1 {
		return "peak"
	}
	return fmt.Sprintf("P%g", q*100)
}

type Input struct {
	Cluster                   string    `json:"cluster,omitempty"`
	Namespace                 string    `json:"namespace"`
	Kind                      string    `json:"kind,omitempty"`
	Workload                  string    `json:"workload"`
	Container                 string    `json:"container"`
	Criticality               string    `json:"criticality,omitempty"`
	CPURequestsMilli          int64     `json:"cpuRequestMilli"`
	MemoryRequestsBytes       int64     `json:"memoryRequestBytes"`
	CPULimitMilli             int64     `json:"cpuLimitMilli,omitempty"`
	MemoryLimitBytes          int64     `json:"memoryLimitBytes,omitempty"`
	CPUSamplesMilli           []float64 `json:"cpuSamplesMilli"`
	MemorySamplesBytes        []float64 `json:"memorySamplesBytes"`
	SampleIntervalSeconds     int64     `json:"sampleIntervalSeconds,omitempty"`
	ObservedContainerRestarts int64     `json:"observedContainerRestarts,omitempty"`
	ObservedOOMKills          int64     `json:"observedOomKills,omitempty"`
	// ObservedCPUThrottlingRatio is the share of CFS scheduling periods in
	// which the container was throttled, from 0 to 1. Usage samples average
	// away sub-second bursts; throttling exposes them.
	ObservedCPUThrottlingRatio float64 `json:"observedCpuThrottlingRatio,omitempty"`
	// Replicas is the desired replica count, used only for cost estimates.
	Replicas int32 `json:"replicas,omitempty"`
	// DataQualityIssues are problems found by the metric source. Any issue
	// blocks reductions, exactly as insufficient sample coverage does.
	DataQualityIssues []string           `json:"dataQualityIssues,omitempty"`
	Autoscaling       AutoscalingContext `json:"autoscaling,omitempty"`
	Policy            *Policy            `json:"policy,omitempty"`
}

type Percentiles struct {
	P50 float64 `json:"p50"`
	P90 float64 `json:"p90"`
	P95 float64 `json:"p95"`
	P99 float64 `json:"p99"`
	Max float64 `json:"max"`
}

type DataQuality struct {
	CPUSampleCount    int      `json:"cpuSampleCount"`
	MemorySampleCount int      `json:"memorySampleCount"`
	MinimumRequired   int      `json:"minimumRequired"`
	Sufficient        bool     `json:"sufficient"`
	Issues            []string `json:"issues"`
}

type Recommendation struct {
	ID                        string      `json:"id,omitempty"`
	Cluster                   string      `json:"cluster,omitempty"`
	Namespace                 string      `json:"namespace"`
	Kind                      string      `json:"kind,omitempty"`
	Workload                  string      `json:"workload"`
	Container                 string      `json:"container"`
	Replicas                  int32       `json:"replicas,omitempty"`
	CPURequestMilli           int64       `json:"cpuRequestMilli"`
	MemoryRequestBytes        int64       `json:"memoryRequestBytes"`
	CPULimitMilli             int64       `json:"cpuLimitMilli"`
	MemoryLimitBytes          int64       `json:"memoryLimitBytes"`
	CurrentCPURequestMilli    int64       `json:"currentCpuRequestMilli"`
	CurrentMemoryRequestBytes int64       `json:"currentMemoryRequestBytes"`
	CurrentCPULimitMilli      int64       `json:"currentCpuLimitMilli"`
	CurrentMemoryLimitBytes   int64       `json:"currentMemoryLimitBytes"`
	CPU                       Percentiles `json:"cpuStatisticsMilli"`
	Memory                    Percentiles `json:"memoryStatisticsBytes"`
	Confidence                string      `json:"confidence"`
	ConfidenceScore           float64     `json:"confidenceScore"`
	Risk                      string      `json:"risk"`
	Burstable                 bool        `json:"burstable"`
	CPUThrottlingRatio        float64     `json:"cpuThrottlingRatio,omitempty"`
	Trends                    Trends      `json:"trends"`
	Backtest                  Backtest    `json:"backtest"`
	DataQuality               DataQuality `json:"dataQuality"`
	EstimatedCPUChangePercent float64     `json:"estimatedCpuChangePercent,omitempty"`
	EstimatedMemoryChangePct  float64     `json:"estimatedMemoryChangePercent,omitempty"`
	Reason                    string      `json:"reason"`
	Warnings                  []string    `json:"warnings"`
	Applied                   bool        `json:"applied"`
	Policy                    Policy      `json:"effectivePolicy"`
}

func percentile(sortedValues []float64, q float64) float64 {
	position := q * float64(len(sortedValues)-1)
	lower := int(math.Floor(position))
	upper := int(math.Ceil(position))
	return sortedValues[lower] + (sortedValues[upper]-sortedValues[lower])*(position-float64(lower))
}

func statistics(values []float64, label string, maximum int64) (Percentiles, error) {
	if len(values) == 0 {
		return Percentiles{}, fmt.Errorf("%s samples are required", label)
	}
	if len(values) > maxSamples {
		return Percentiles{}, fmt.Errorf("%s samples exceed the maximum of %d", label, maxSamples)
	}
	clean := make([]float64, len(values))
	for index, value := range values {
		if math.IsNaN(value) || math.IsInf(value, 0) || value < 0 || value > float64(maximum) {
			return Percentiles{}, fmt.Errorf("%s sample at index %d is outside the supported range", label, index)
		}
		clean[index] = value
	}
	sort.Float64s(clean)
	return Percentiles{
		P50: percentile(clean, 0.50),
		P90: percentile(clean, 0.90),
		P95: percentile(clean, 0.95),
		P99: percentile(clean, 0.99),
		Max: clean[len(clean)-1],
	}, nil
}

func validatePolicy(policy Policy) error {
	if policy.CPUSafetyMargin < 0 || policy.CPUSafetyMargin > 10 || policy.MemorySafetyMargin < 0 || policy.MemorySafetyMargin > 10 || policy.LimitSafetyMargin < 0 || policy.LimitSafetyMargin > 10 {
		return errors.New("safety margins must be between 0 and 10")
	}
	if policy.MinimumCPUMilli <= 0 || policy.MaximumCPUMilli < policy.MinimumCPUMilli || policy.MaximumCPUMilli > maxCPUMilli {
		return errors.New("CPU policy bounds are invalid")
	}
	if policy.MinimumMemoryBytes <= 0 || policy.MaximumMemoryBytes < policy.MinimumMemoryBytes || policy.MaximumMemoryBytes > maxMemoryBytes {
		return errors.New("memory policy bounds are invalid")
	}
	if policy.MaximumReduction < 0 || policy.MaximumReduction > 0.50 {
		return errors.New("maximum reduction must be between 0 and 0.5")
	}
	if policy.MinimumSamples < 2 || policy.MinimumSamples > maxSamples || policy.HighConfidenceSamples < policy.MinimumSamples || policy.HighConfidenceSamples > maxSamples {
		return errors.New("sample thresholds are invalid")
	}
	if !validPercentile(policy.CPUPercentile) || !validPercentile(policy.MemoryPercentile) {
		return errors.New("CPU and memory percentiles must be 0.9, 0.95, 0.99, or 1")
	}
	if policy.TrendHorizonDays < 1 || policy.TrendHorizonDays > 30 {
		return errors.New("trend horizon must be between 1 and 30 days")
	}
	return nil
}

func validateInput(input Input, policy Policy) error {
	for field, value := range map[string]string{"namespace": input.Namespace, "workload": input.Workload, "container": input.Container} {
		if strings.TrimSpace(value) == "" || len(value) > 253 {
			return fmt.Errorf("%s must contain 1 to 253 characters", field)
		}
	}
	if len(input.Cluster) > 253 || len(input.Kind) > 128 {
		return errors.New("cluster or workload kind is too long")
	}
	if input.CPURequestsMilli < 0 || input.CPURequestsMilli > maxCPUMilli || input.CPULimitMilli < 0 || input.CPULimitMilli > maxCPUMilli {
		return errors.New("CPU requests and limits are outside the supported range")
	}
	if input.MemoryRequestsBytes < 0 || input.MemoryRequestsBytes > maxMemoryBytes || input.MemoryLimitBytes < 0 || input.MemoryLimitBytes > maxMemoryBytes {
		return errors.New("memory requests and limits are outside the supported range")
	}
	if input.CPULimitMilli > 0 && input.CPULimitMilli < input.CPURequestsMilli {
		return errors.New("CPU limit cannot be lower than CPU request")
	}
	if input.MemoryLimitBytes > 0 && input.MemoryLimitBytes < input.MemoryRequestsBytes {
		return errors.New("memory limit cannot be lower than memory request")
	}
	if input.CPURequestsMilli > policy.MaximumCPUMilli || input.MemoryRequestsBytes > policy.MaximumMemoryBytes {
		return errors.New("current requests exceed the configured policy maximum")
	}
	if input.Criticality != "" && input.Criticality != "low" && input.Criticality != "medium" && input.Criticality != "high" {
		return errors.New("criticality must be low, medium, or high")
	}
	if input.SampleIntervalSeconds < 0 || input.ObservedContainerRestarts < 0 || input.ObservedOOMKills < 0 {
		return errors.New("sample interval and event counts cannot be negative")
	}
	if math.IsNaN(input.ObservedCPUThrottlingRatio) || input.ObservedCPUThrottlingRatio < 0 || input.ObservedCPUThrottlingRatio > 1 {
		return errors.New("CPU throttling ratio must be between 0 and 1")
	}
	if input.Replicas < 0 || input.Replicas > 100_000 {
		return errors.New("replicas must be between 0 and 100000")
	}
	if len(input.DataQualityIssues) > 32 {
		return errors.New("at most 32 data-quality issues may be supplied")
	}
	for _, issue := range input.DataQualityIssues {
		if strings.TrimSpace(issue) == "" || len(issue) > 256 {
			return errors.New("data-quality issues must contain 1 to 256 characters")
		}
	}
	return nil
}

func clampInt64(value, minimum, maximum int64) int64 {
	if value < minimum {
		return minimum
	}
	if value > maximum {
		return maximum
	}
	return value
}

func roundedRecommendation(value float64, margin float64, minimum, maximum int64) int64 {
	if value > float64(maximum)/(1+margin) {
		return maximum
	}
	return clampInt64(int64(math.Ceil(value*(1+margin))), minimum, maximum)
}

func reductionFloor(current int64, maximumReduction float64) int64 {
	return int64(math.Ceil(float64(current) * (1 - maximumReduction)))
}

func changePercent(current, recommended int64) float64 {
	if current == 0 {
		return 0
	}
	return math.Round(((float64(recommended-current)/float64(current))*100)*100) / 100
}

// Recommend calculates a recommendation from observed samples. Low-quality
// datasets may result in increases, but never in reductions.
func Recommend(input Input) (Recommendation, error) {
	policy := DefaultPolicy()
	if input.Policy != nil {
		policy = normalizePolicy(*input.Policy)
	}
	if err := validatePolicy(policy); err != nil {
		return Recommendation{}, err
	}
	if err := validateInput(input, policy); err != nil {
		return Recommendation{}, err
	}

	cpuStats, err := statistics(input.CPUSamplesMilli, "CPU", maxCPUMilli)
	if err != nil {
		return Recommendation{}, err
	}
	memoryStats, err := statistics(input.MemorySamplesBytes, "memory", maxMemoryBytes)
	if err != nil {
		return Recommendation{}, err
	}

	sampleCount := min(len(input.CPUSamplesMilli), len(input.MemorySamplesBytes))
	quality := DataQuality{
		CPUSampleCount:    len(input.CPUSamplesMilli),
		MemorySampleCount: len(input.MemorySamplesBytes),
		MinimumRequired:   policy.MinimumSamples,
		Sufficient:        sampleCount >= policy.MinimumSamples && input.SampleIntervalSeconds > 0,
		Issues:            []string{},
	}
	warnings := []string{}
	if sampleCount < policy.MinimumSamples {
		quality.Issues = append(quality.Issues, "insufficient samples for a safe reduction")
	}
	if input.SampleIntervalSeconds == 0 {
		quality.Issues = append(quality.Issues, "sample interval was not supplied; temporal coverage is unknown")
	}
	if len(input.DataQualityIssues) > 0 {
		quality.Sufficient = false
		quality.Issues = append(quality.Issues, input.DataQualityIssues...)
	}
	if !quality.Sufficient {
		warnings = append(warnings, "requests were not reduced because sample coverage, temporal coverage, or data quality is insufficient")
	}
	if len(input.CPUSamplesMilli) != len(input.MemorySamplesBytes) {
		quality.Issues = append(quality.Issues, "CPU and memory sample counts differ")
	}

	confidence := "low"
	confidenceScore := math.Min(0.49, float64(sampleCount)/float64(policy.MinimumSamples)*0.49)
	if quality.Sufficient {
		confidence = "medium"
		confidenceScore = 0.50 + math.Min(0.29, float64(sampleCount-policy.MinimumSamples)/float64(max(1, policy.HighConfidenceSamples-policy.MinimumSamples))*0.29)
	}
	if quality.Sufficient && sampleCount >= policy.HighConfidenceSamples {
		confidence = "high"
		confidenceScore = math.Min(0.99, 0.80+float64(sampleCount-policy.HighConfidenceSamples)/float64(policy.HighConfidenceSamples)*0.19)
	}
	confidenceScore = math.Round(confidenceScore*100) / 100

	maximumReduction := policy.MaximumReduction
	if strings.EqualFold(input.Kind, "StatefulSet") || input.Criticality == "high" {
		maximumReduction = math.Min(maximumReduction, 0.10)
		warnings = append(warnings, "stateful or high-criticality safeguards limit reductions to 10 percent")
	}
	if !quality.Sufficient {
		maximumReduction = 0
	}
	memoryMaximumReduction := maximumReduction
	if input.ObservedOOMKills > 0 {
		memoryMaximumReduction = 0
		warnings = append(warnings, "observed OOM kills prevent memory request and limit reductions")
	}

	// Size for where usage is heading, not only where it has been.
	trends := Trends{CPU: Trend(input.CPUSamplesMilli, input.SampleIntervalSeconds), Memory: Trend(input.MemorySamplesBytes, input.SampleIntervalSeconds)}
	if trends.Memory.Significant && trends.Memory.SlopePerDay > 0 {
		memoryMaximumReduction = 0
		warnings = append(warnings, fmt.Sprintf("memory grows %.1f%% per day (R² %.2f); memory was not reduced and is sized for %d days of growth; investigate a possible leak", trends.Memory.GrowthPerDayPercent, trends.Memory.RSquared, policy.TrendHorizonDays))
	}
	if trends.CPU.Significant && trends.CPU.SlopePerDay > 0 {
		warnings = append(warnings, fmt.Sprintf("CPU usage grows %.1f%% per day (R² %.2f); CPU is sized for %d days of growth", trends.CPU.GrowthPerDayPercent, trends.CPU.RSquared, policy.TrendHorizonDays))
	}

	cpuRequest := roundedRecommendation(trends.CPU.withUplift(cpuStats.at(policy.CPUPercentile), policy.TrendHorizonDays), policy.CPUSafetyMargin, policy.MinimumCPUMilli, policy.MaximumCPUMilli)
	memoryRequest := roundedRecommendation(trends.Memory.withUplift(memoryStats.at(policy.MemoryPercentile), policy.TrendHorizonDays), policy.MemorySafetyMargin, policy.MinimumMemoryBytes, policy.MaximumMemoryBytes)
	// A throttled container is CPU-constrained even when its averaged usage
	// looks low, so its CPU must not be reduced.
	throttled := input.ObservedCPUThrottlingRatio >= throttlingThreshold
	cpuMaximumReduction := maximumReduction
	if throttled {
		cpuMaximumReduction = 0
		warnings = append(warnings, fmt.Sprintf("CPU was throttled in %.1f%% of scheduling periods; CPU was not reduced and the CPU limit was raised", input.ObservedCPUThrottlingRatio*100))
	}
	if input.CPURequestsMilli > 0 {
		cpuRequest = max(cpuRequest, reductionFloor(input.CPURequestsMilli, cpuMaximumReduction))
	}
	if input.MemoryRequestsBytes > 0 {
		memoryRequest = max(memoryRequest, reductionFloor(input.MemoryRequestsBytes, memoryMaximumReduction))
	}

	// Include the observed maximum so a narrow but material spike at the edge
	// of the P99 interpolation window is not incorrectly classified as steady.
	burstable := cpuStats.P50 > 0 && (cpuStats.P99/cpuStats.P50 >= 3 || cpuStats.Max/cpuStats.P50 >= 3)
	if burstable {
		warnings = append(warnings, "CPU usage is bursty; validate throttling and latency before applying")
	}
	if input.ObservedContainerRestarts > 0 {
		warnings = append(warnings, "container restarts were observed during the sample window")
	}
	if input.Autoscaling.HPAUtilizationBased {
		warnings = append(warnings, "changing CPU requests can change HPA utilization and replica behavior")
	}
	if input.Autoscaling.VPAControlsCPU {
		cpuRequest = input.CPURequestsMilli
		warnings = append(warnings, "CPU request is VPA-managed and was left unchanged")
	}
	if input.Autoscaling.VPAControlsMemory {
		memoryRequest = input.MemoryRequestsBytes
		warnings = append(warnings, "memory request is VPA-managed and was left unchanged")
	}
	if input.Autoscaling.KEDAEnabled {
		warnings = append(warnings, "KEDA is present; validate scaling triggers against the proposed requests")
	}

	// An absent limit means unlimited. Introducing a new limit can create CPU
	// throttling or OOM kills, so it is intentionally not done automatically.
	cpuLimit := int64(0)
	if input.CPULimitMilli > 0 {
		cpuLimit = roundedRecommendation(cpuStats.at(math.Max(0.99, policy.CPUPercentile)), policy.LimitSafetyMargin, cpuRequest, policy.MaximumCPUMilli)
		cpuLimit = max(cpuLimit, reductionFloor(input.CPULimitMilli, cpuMaximumReduction))
		if throttled {
			raise := 1.25
			if input.ObservedCPUThrottlingRatio >= severeThrottlingThreshold {
				raise = 1.5
			}
			cpuLimit = max(cpuLimit, min(int64(math.Ceil(float64(input.CPULimitMilli)*raise)), policy.MaximumCPUMilli))
		}
		cpuLimit = max(cpuLimit, cpuRequest)
	} else {
		warnings = append(warnings, "CPU limit is absent and no new limit was introduced")
	}
	memoryLimit := int64(0)
	if input.MemoryLimitBytes > 0 {
		memoryLimit = roundedRecommendation(memoryStats.at(math.Max(0.99, policy.MemoryPercentile)), policy.LimitSafetyMargin, memoryRequest, policy.MaximumMemoryBytes)
		memoryLimit = max(memoryLimit, reductionFloor(input.MemoryLimitBytes, memoryMaximumReduction))
		memoryLimit = max(memoryLimit, memoryRequest)
		// A memory limit below an observed peak would have OOM-killed the
		// container, so the limit always keeps headroom above the peak.
		if peakFloor := int64(math.Ceil(memoryStats.Max * (1 + memoryLimitPeakHeadroom))); memoryLimit < peakFloor {
			memoryLimit = min(peakFloor, policy.MaximumMemoryBytes)
			warnings = append(warnings, "memory limit was raised to keep 10% headroom above the observed peak")
		}
	} else {
		warnings = append(warnings, "memory limit is absent and no new limit was introduced")
	}

	risk := "low"
	if confidence != "high" || burstable || throttled || input.Autoscaling.HPAUtilizationBased {
		risk = "medium"
	}
	if confidence == "low" || input.ObservedOOMKills > 0 || strings.EqualFold(input.Kind, "StatefulSet") || input.Criticality == "high" || input.Autoscaling.VPAControlsCPU || input.Autoscaling.VPAControlsMemory {
		risk = "high"
	}

	return Recommendation{
		Cluster:                   input.Cluster,
		Namespace:                 input.Namespace,
		Kind:                      input.Kind,
		Workload:                  input.Workload,
		Container:                 input.Container,
		Replicas:                  input.Replicas,
		CPURequestMilli:           cpuRequest,
		MemoryRequestBytes:        memoryRequest,
		CPULimitMilli:             cpuLimit,
		MemoryLimitBytes:          memoryLimit,
		CurrentCPURequestMilli:    input.CPURequestsMilli,
		CurrentMemoryRequestBytes: input.MemoryRequestsBytes,
		CurrentCPULimitMilli:      input.CPULimitMilli,
		CurrentMemoryLimitBytes:   input.MemoryLimitBytes,
		CPU:                       cpuStats,
		Memory:                    memoryStats,
		Confidence:                confidence,
		ConfidenceScore:           confidenceScore,
		Risk:                      risk,
		Burstable:                 burstable,
		CPUThrottlingRatio:        math.Round(input.ObservedCPUThrottlingRatio*10_000) / 10_000,
		DataQuality:               quality,
		EstimatedCPUChangePercent: changePercent(input.CPURequestsMilli, cpuRequest),
		EstimatedMemoryChangePct:  changePercent(input.MemoryRequestsBytes, memoryRequest),
		Reason:                    fmt.Sprintf("%s CPU and %s memory with policy safety margins, resource floors, and bounded reductions", percentileLabel(policy.CPUPercentile), percentileLabel(policy.MemoryPercentile)),
		Warnings:                  warnings,
		Applied:                   false,
		Policy:                    policy,
		Trends:                    trends,
		Backtest:                  backtest(input, cpuRequest, cpuLimit, memoryRequest, memoryLimit),
	}, nil
}
