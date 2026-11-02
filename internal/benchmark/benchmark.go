// Package benchmark measures recommendation accuracy on reproducible,
// seeded workload datasets. Every strategy sizes from the first part of a
// history and is scored on the later part it never saw, the way a change is
// judged after it ships.
package benchmark

import (
	"fmt"
	"math"
	"math/rand/v2"
	"sort"

	"github.com/kuberight/kuberight/internal/optimizer"
	"github.com/kuberight/kuberight/internal/stats"
)

const (
	stepSeconds = 300
	stepsPerDay = 86_400 / stepSeconds
	mebibyte    = 1024 * 1024
	// A change is a false positive when it reduces a request and the unseen
	// week then exceeds the new CPU request in more than this share of
	// samples, or exceeds the new memory limit at all.
	falsePositiveCPUShare = 0.25
	// A post-change regression is OOM or throttling exposure that the
	// current settings did not have on the same week.
	regressionThrottleShare = 0.05
)

// Dataset is one seeded workload history with its current settings.
type Dataset struct {
	Name        string
	Description string
	Kind        string
	Container   string
	Criticality string
	// Train and Test are CPU millicores and memory bytes per five-minute
	// step; NaN marks a missing sample.
	TrainCPU, TrainMemory []float64
	TestCPU, TestMemory   []float64
	Current               optimizer.ContainerResources
	Replicas              int32
	Autoscaling           optimizer.AutoscalingContext
	HPA                   *optimizer.HPAContext
	Pod                   *optimizer.PodContext
	// RolloutIndex is where a rollout happened in the training data, or -1.
	RolloutIndex int
}

// Config selects the datasets' size and seed.
type Config struct {
	Seed      uint64
	TrainDays int
	TestDays  int
}

// DefaultConfig is the configuration the published results use.
func DefaultConfig() Config { return Config{Seed: 20261101, TrainDays: 7, TestDays: 7} }

type generator struct {
	random *rand.Rand
	n      int
}

func (g generator) noise(scale float64) float64 { return g.random.NormFloat64() * scale }

func (g generator) series(fn func(index int) (cpu, memory float64)) ([]float64, []float64) {
	cpu, memory := make([]float64, g.n), make([]float64, g.n)
	for index := range g.n {
		c, m := fn(index)
		cpu[index], memory[index] = math.Max(1, c), math.Max(mebibyte, m)
	}
	return cpu, memory
}

func daily(index int) float64 { return math.Sin(2 * math.Pi * float64(index) / stepsPerDay) }

// Datasets builds the archetypes the specification calls for.
func Datasets(config Config) []Dataset {
	trainSteps, testSteps := config.TrainDays*stepsPerDay, config.TestDays*stepsPerDay
	type builder struct {
		name, description, kind, criticality, container string
		current                                         optimizer.ContainerResources
		replicas                                        int32
		autoscaling                                     optimizer.AutoscalingContext
		hpa                                             *optimizer.HPAContext
		pod                                             *optimizer.PodContext
		rollout                                         bool
		missing                                         float64
		fn                                              func(g generator, index int) (float64, float64)
	}
	resources := func(cpuRequest, cpuLimit, memoryRequestMi, memoryLimitMi int64) optimizer.ContainerResources {
		return optimizer.ContainerResources{Name: "app", CPURequestMilli: cpuRequest, CPULimitMilli: cpuLimit, MemoryRequestBytes: memoryRequestMi * mebibyte, MemoryLimitBytes: memoryLimitMi * mebibyte}
	}
	builders := []builder{
		{name: "stable", description: "steady service, low noise, over-provisioned", kind: "Deployment", current: resources(1000, 2000, 1024, 2048), replicas: 3,
			fn: func(g generator, _ int) (float64, float64) {
				return 250 + g.noise(10), 300*mebibyte + g.noise(4*mebibyte)
			}},
		{name: "bursty-api", description: "daily traffic cycle with random bursts up to 4x", kind: "Deployment", current: resources(800, 2000, 768, 1024), replicas: 4,
			fn: func(g generator, index int) (float64, float64) {
				cpu := 200 + 100*daily(index) + g.noise(15)
				if g.random.Float64() < 0.03 {
					cpu *= 2 + 2*g.random.Float64()
				}
				return cpu, 400*mebibyte + 30*mebibyte*daily(index) + g.noise(5*mebibyte)
			}},
		{name: "memory-intensive", description: "large, nearly flat working set", kind: "Deployment", current: resources(500, 1000, 8192, 8192), replicas: 2,
			fn: func(g generator, index int) (float64, float64) {
				return 150 + g.noise(10), 6000*mebibyte + 150*mebibyte*daily(index) + g.noise(20*mebibyte)
			}},
		{name: "java", description: "JVM heap sawtooth from garbage collection", kind: "Deployment", current: resources(1000, 2000, 2048, 2048), replicas: 3,
			fn: func(g generator, index int) (float64, float64) {
				heap := 700*mebibyte + 600*mebibyte*float64(index%36)/36
				return 300 + 80*daily(index) + g.noise(20), heap + g.noise(10*mebibyte)
			}},
		{name: "statefulset", description: "database whose memory grows slowly", kind: "StatefulSet", criticality: "high", current: resources(2000, 0, 4096, 6144), replicas: 3,
			fn: func(g generator, index int) (float64, float64) {
				return 600 + 150*daily(index) + g.noise(30), 2500*mebibyte + float64(index)*40_000 + g.noise(15*mebibyte)
			}},
		{name: "hpa-scaled", description: "HPA keeps per-pod CPU near its target", kind: "Deployment", current: resources(500, 1000, 512, 1024), replicas: 6,
			autoscaling: optimizer.AutoscalingContext{HPAUtilizationBased: true},
			hpa:         &optimizer.HPAContext{MinReplicas: 2, MaxReplicas: 20, TargetCPUUtilization: 70, CurrentReplicas: 6, ContainerMetric: true},
			fn: func(g generator, index int) (float64, float64) {
				return 330 + 30*daily(index) + g.noise(20), 350*mebibyte + g.noise(8*mebibyte)
			}},
		{name: "keda-scaled", description: "queue consumer that steps between idle and busy", kind: "Deployment", current: resources(1000, 2000, 1024, 1024), replicas: 2,
			autoscaling: optimizer.AutoscalingContext{KEDAEnabled: true},
			fn: func(g generator, index int) (float64, float64) {
				busy := (index/48)%3 == 0
				cpu := 80.0
				if busy {
					cpu = 600
				}
				return cpu + g.noise(20), 450*mebibyte + g.noise(10*mebibyte)
			}},
		{name: "cronjob", description: "three busy hours a day, idle otherwise", kind: "CronJob", current: resources(2000, 4000, 2048, 4096), replicas: 1,
			fn: func(g generator, index int) (float64, float64) {
				if index%stepsPerDay < 36 {
					return 1200 + g.noise(60), 1500*mebibyte + g.noise(30*mebibyte)
				}
				return 5 + math.Abs(g.noise(2)), 120*mebibyte + g.noise(2*mebibyte)
			}},
		{name: "sidecar", description: "service mesh proxy beside the application", kind: "Deployment", container: "proxy", current: resources(500, 1000, 256, 512), replicas: 4,
			pod: &optimizer.PodContext{
				Containers:     []optimizer.ContainerResources{{Name: "application", CPURequestMilli: 1000, MemoryRequestBytes: 1024 * mebibyte}},
				InitContainers: []optimizer.InitContainer{{ContainerResources: optimizer.ContainerResources{Name: "proxy", CPURequestMilli: 500, CPULimitMilli: 1000, MemoryRequestBytes: 256 * mebibyte, MemoryLimitBytes: 512 * mebibyte}, Sidecar: true}},
			},
			fn: func(g generator, index int) (float64, float64) {
				return 60 + 15*daily(index) + g.noise(5), 90*mebibyte + g.noise(2*mebibyte)
			}},
		{name: "recent-rollout", description: "a rollout raised CPU 60% late in the history", kind: "Deployment", current: resources(1000, 2000, 1024, 2048), replicas: 3, rollout: true,
			fn: func(g generator, index int) (float64, float64) {
				level := 1.0
				if index >= trainSteps*3/4 {
					level = 1.6
				}
				return level * (300 + 60*daily(index) + g.noise(15)), 500*mebibyte + g.noise(8*mebibyte)
			}},
		{name: "incomplete-metrics", description: "a third of the samples are missing", kind: "Deployment", current: resources(1000, 2000, 1024, 2048), replicas: 3, missing: 0.33,
			fn: func(g generator, index int) (float64, float64) {
				return 300 + 50*daily(index) + g.noise(15), 400*mebibyte + g.noise(8*mebibyte)
			}},
	}
	datasets := make([]Dataset, 0, len(builders))
	for position, item := range builders {
		g := generator{random: rand.New(rand.NewPCG(config.Seed, uint64(position+1))), n: trainSteps + testSteps}
		cpu, memory := g.series(func(index int) (float64, float64) { return item.fn(g, index) })
		if item.missing > 0 {
			for index := range trainSteps {
				if g.random.Float64() < item.missing {
					cpu[index], memory[index] = math.NaN(), math.NaN()
				}
			}
		}
		container := item.container
		if container == "" {
			container = "app"
		}
		item.current.Name = container
		dataset := Dataset{
			Name: item.name, Description: item.description, Kind: item.kind, Container: container, Criticality: item.criticality,
			TrainCPU: cpu[:trainSteps], TrainMemory: memory[:trainSteps], TestCPU: cpu[trainSteps:], TestMemory: memory[trainSteps:],
			Current: item.current, Replicas: item.replicas, Autoscaling: item.autoscaling, HPA: item.hpa, Pod: item.pod, RolloutIndex: -1,
		}
		if item.rollout {
			dataset.RolloutIndex = trainSteps * 3 / 4
		}
		datasets = append(datasets, dataset)
	}
	return datasets
}

// Sizing is what a strategy proposes. Status is "recommend" when the
// strategy would change the settings and anything else when it keeps them.
type Sizing struct {
	Status                     string
	CPURequest, CPULimit       int64
	MemoryRequest, MemoryLimit int64
	// PredictedCPU is the demand the strategy sized for, or 0.
	PredictedCPU float64
}

// Strategy sizes a container from its training history.
type Strategy struct {
	Name        string
	Description string
	Size        func(Dataset) Sizing
}

func finite(values []float64) []float64 {
	result := make([]float64, 0, len(values))
	for _, value := range values {
		if !math.IsNaN(value) {
			result = append(result, value)
		}
	}
	return result
}

func keep(dataset Dataset, status string) Sizing {
	current := dataset.Current
	return Sizing{Status: status, CPURequest: current.CPURequestMilli, CPULimit: current.CPULimitMilli, MemoryRequest: current.MemoryRequestBytes, MemoryLimit: current.MemoryLimitBytes}
}

// limitFor keeps the current limit-to-request ratio, as VPA does.
func limitFor(currentRequest, currentLimit, request int64) int64 {
	if currentLimit <= 0 || currentRequest <= 0 {
		return 0
	}
	return int64(math.Ceil(float64(request) * float64(currentLimit) / float64(currentRequest)))
}

// Strategies are KubeRight and the deterministic baselines it is compared
// with.
func Strategies() []Strategy {
	return []Strategy{
		{Name: "kuberight", Description: "KubeRight balanced policy with the full assessment; only a \"recommend\" verdict changes settings", Size: kuberight},
		{Name: "keep-current", Description: "no change", Size: func(dataset Dataset) Sizing { return keep(dataset, "keep") }},
		{Name: "p95-no-margin", Description: "requests at P95, limits at the observed peak, no margin", Size: func(dataset Dataset) Sizing {
			cpu, memory := finite(dataset.TrainCPU), finite(dataset.TrainMemory)
			request, memoryRequest := int64(math.Ceil(stats.Quantile(cpu, 0.95))), int64(math.Ceil(stats.Quantile(memory, 0.95)))
			sizing := Sizing{Status: optimizer.StatusRecommend, CPURequest: request, MemoryRequest: memoryRequest, PredictedCPU: stats.Quantile(cpu, 0.95)}
			if dataset.Current.CPULimitMilli > 0 {
				sizing.CPULimit = max(request, int64(math.Ceil(stats.Quantile(cpu, 1))))
			}
			if dataset.Current.MemoryLimitBytes > 0 {
				sizing.MemoryLimit = max(memoryRequest, int64(math.Ceil(stats.Quantile(memory, 1))))
			}
			return sizing
		}},
		{Name: "vpa-style", Description: "VPA-style target: P90 CPU and P90 memory plus 15%, limits keep the current ratio", Size: func(dataset Dataset) Sizing {
			cpu, memory := finite(dataset.TrainCPU), finite(dataset.TrainMemory)
			request := int64(math.Ceil(stats.Quantile(cpu, 0.9) * 1.15))
			memoryRequest := int64(math.Ceil(stats.Quantile(memory, 0.9) * 1.15))
			return Sizing{
				Status: optimizer.StatusRecommend, CPURequest: request, MemoryRequest: memoryRequest, PredictedCPU: stats.Quantile(cpu, 0.9),
				CPULimit:    limitFor(dataset.Current.CPURequestMilli, dataset.Current.CPULimitMilli, request),
				MemoryLimit: limitFor(dataset.Current.MemoryRequestBytes, dataset.Current.MemoryLimitBytes, memoryRequest),
			}
		}},
		{Name: "max-observed", Description: "requests at the observed peak plus 10%", Size: func(dataset Dataset) Sizing {
			cpu, memory := finite(dataset.TrainCPU), finite(dataset.TrainMemory)
			request := int64(math.Ceil(stats.Quantile(cpu, 1) * 1.1))
			memoryRequest := int64(math.Ceil(stats.Quantile(memory, 1) * 1.1))
			sizing := Sizing{Status: optimizer.StatusRecommend, CPURequest: request, MemoryRequest: memoryRequest, PredictedCPU: stats.Quantile(cpu, 1)}
			if dataset.Current.CPULimitMilli > 0 {
				sizing.CPULimit = request
			}
			if dataset.Current.MemoryLimitBytes > 0 {
				sizing.MemoryLimit = memoryRequest
			}
			return sizing
		}},
	}
}

// kuberight applies the same preparation as live generation: deployment-
// aware segmentation and data-quality gating, then the optimizer.
func kuberight(dataset Dataset) Sizing {
	cpu, memory := dataset.TrainCPU, dataset.TrainMemory
	input := optimizer.Input{
		Namespace: "benchmark", Kind: dataset.Kind, Workload: dataset.Name, Container: dataset.Container, Criticality: dataset.Criticality,
		CPURequestsMilli: dataset.Current.CPURequestMilli, CPULimitMilli: dataset.Current.CPULimitMilli,
		MemoryRequestsBytes: dataset.Current.MemoryRequestBytes, MemoryLimitBytes: dataset.Current.MemoryLimitBytes,
		SampleIntervalSeconds: stepSeconds, Replicas: dataset.Replicas, Autoscaling: dataset.Autoscaling, HPA: dataset.HPA, Pod: dataset.Pod,
	}
	minimum := optimizer.DefaultPolicy().MinimumSamples
	if dataset.RolloutIndex >= 0 && len(cpu)-dataset.RolloutIndex >= minimum {
		cpu, memory = cpu[dataset.RolloutIndex:], memory[dataset.RolloutIndex:]
		input.RecentRollout = true
	}
	present := finite(cpu)
	if coverage := float64(len(present)) / float64(len(cpu)); coverage < 0.9 {
		input.DataQualityIssues = []string{fmt.Sprintf("only %.0f%% of expected samples are present", coverage*100)}
	}
	input.CPUSamplesMilli, input.MemorySamplesBytes = present, finite(memory)
	recommendation, err := optimizer.Recommend(input)
	if err != nil || recommendation.Assessment == nil {
		return keep(dataset, "error")
	}
	if recommendation.Assessment.Status != optimizer.StatusRecommend {
		sizing := keep(dataset, recommendation.Assessment.Status)
		sizing.PredictedCPU = recommendation.CPU.P95
		return sizing
	}
	predicted := recommendation.CPU.P95
	if forecast := recommendation.Assessment.Forecast; forecast != nil && forecast.CPUMilli > 0 {
		predicted = forecast.CPUMilli
	}
	return Sizing{
		Status: optimizer.StatusRecommend, CPURequest: recommendation.CPURequestMilli, CPULimit: recommendation.CPULimitMilli,
		MemoryRequest: recommendation.MemoryRequestBytes, MemoryLimit: recommendation.MemoryLimitBytes, PredictedCPU: predicted,
	}
}

func share(values []float64, threshold int64) float64 {
	if threshold <= 0 || len(values) == 0 {
		return 0
	}
	over := 0
	for _, value := range values {
		if value > float64(threshold) {
			over++
		}
	}
	return float64(over) / float64(len(values))
}

// Result is one strategy's outcome on one dataset's unseen week.
type Result struct {
	Dataset  string `json:"dataset"`
	Strategy string `json:"strategy"`
	Status   string `json:"status"`
	// Changed is true when the strategy would alter the current settings.
	Changed bool `json:"changed"`
	// Exceedance shares are measured on the unseen samples.
	CPUOverRequest    float64 `json:"cpuOverRequest"`
	CPUOverLimit      float64 `json:"cpuOverLimit"`
	MemoryOverRequest float64 `json:"memoryOverRequest"`
	MemoryOverLimit   float64 `json:"memoryOverLimit"`
	// OOMExposure means memory would have passed the limit at least once.
	OOMExposure bool `json:"oomExposure"`
	// FalsePositive is a reduction that the unseen week contradicted.
	FalsePositive bool `json:"falsePositive"`
	// Regression is OOM or throttling exposure that the current settings did
	// not have on the same week.
	Regression bool `json:"regression"`
	// DemandError is |predicted - actual P95| / actual P95 for CPU.
	DemandError float64 `json:"demandError"`
	// CPU and memory request savings across replicas, as a share of current.
	CPUSavings    float64 `json:"cpuSavings"`
	MemorySavings float64 `json:"memorySavings"`
}

// Evaluate scores a sizing on the dataset's unseen week.
func Evaluate(dataset Dataset, strategy string, sizing Sizing) Result {
	testCPU, testMemory := finite(dataset.TestCPU), finite(dataset.TestMemory)
	current := dataset.Current
	changed := sizing.CPURequest != current.CPURequestMilli || sizing.MemoryRequest != current.MemoryRequestBytes ||
		sizing.CPULimit != current.CPULimitMilli || sizing.MemoryLimit != current.MemoryLimitBytes
	result := Result{
		Dataset: dataset.Name, Strategy: strategy, Status: sizing.Status, Changed: changed,
		CPUOverRequest: round(share(testCPU, sizing.CPURequest)), CPUOverLimit: round(share(testCPU, sizing.CPULimit)),
		MemoryOverRequest: round(share(testMemory, sizing.MemoryRequest)), MemoryOverLimit: round(share(testMemory, sizing.MemoryLimit)),
	}
	result.OOMExposure = result.MemoryOverLimit > 0
	reduced := sizing.CPURequest < current.CPURequestMilli || sizing.MemoryRequest < current.MemoryRequestBytes
	result.FalsePositive = changed && reduced && (result.CPUOverRequest > falsePositiveCPUShare || result.OOMExposure)
	currentOOM := share(testMemory, current.MemoryLimitBytes) > 0
	currentThrottle := share(testCPU, current.CPULimitMilli) > regressionThrottleShare
	result.Regression = changed && ((result.OOMExposure && !currentOOM) || (result.CPUOverLimit > regressionThrottleShare && !currentThrottle))
	if actual := stats.Quantile(testCPU, 0.95); actual > 0 && sizing.PredictedCPU > 0 {
		result.DemandError = round(math.Abs(sizing.PredictedCPU-actual) / actual)
	}
	if current.CPURequestMilli > 0 {
		result.CPUSavings = round(float64(current.CPURequestMilli-sizing.CPURequest) / float64(current.CPURequestMilli))
	}
	if current.MemoryRequestBytes > 0 {
		result.MemorySavings = round(float64(current.MemoryRequestBytes-sizing.MemoryRequest) / float64(current.MemoryRequestBytes))
	}
	return result
}

func round(value float64) float64 { return math.Round(value*10_000) / 10_000 }

// Summary aggregates one strategy across all datasets.
type Summary struct {
	Strategy           string  `json:"strategy"`
	Description        string  `json:"description"`
	Datasets           int     `json:"datasets"`
	Changes            int     `json:"changes"`
	OOMExposures       int     `json:"oomExposures"`
	FalsePositives     int     `json:"falsePositives"`
	Regressions        int     `json:"regressions"`
	MeanCPUOverRequest float64 `json:"meanCpuOverRequest"`
	MeanDemandError    float64 `json:"meanDemandError"`
	MeanCPUSavings     float64 `json:"meanCpuSavings"`
	MeanMemorySavings  float64 `json:"meanMemorySavings"`
}

// Report is the full benchmark output.
type Report struct {
	Methodology map[string]any `json:"methodology"`
	Results     []Result       `json:"results"`
	Summaries   []Summary      `json:"summaries"`
}

// Run evaluates every strategy on every dataset.
func Run(config Config) Report {
	datasets := Datasets(config)
	strategies := Strategies()
	report := Report{
		Methodology: map[string]any{
			"seed": config.Seed, "stepSeconds": stepSeconds, "trainDays": config.TrainDays, "testDays": config.TestDays,
			"datasets":          len(datasets),
			"samplesPerDataset": (config.TrainDays + config.TestDays) * stepsPerDay,
			"method":            "each strategy sizes from the training days and is scored on the following, unseen test days",
			"falsePositive":     fmt.Sprintf("a reduction followed by CPU above the new request in more than %.0f%% of test samples, or memory above the new limit", falsePositiveCPUShare*100),
			"regression":        fmt.Sprintf("memory above the new limit or CPU above the new limit in more than %.0f%% of test samples, when the current settings had neither", regressionThrottleShare*100),
			"demandError":       "absolute error of the predicted CPU demand against the test week's P95",
			"unchangedOnHold":   "KubeRight keeps the current settings unless its verdict is recommend",
		},
	}
	for _, strategy := range strategies {
		summary := Summary{Strategy: strategy.Name, Description: strategy.Description, Datasets: len(datasets)}
		demandErrors := []float64{}
		for _, dataset := range datasets {
			result := Evaluate(dataset, strategy.Name, strategy.Size(dataset))
			report.Results = append(report.Results, result)
			if result.Changed {
				summary.Changes++
			}
			if result.OOMExposure {
				summary.OOMExposures++
			}
			if result.FalsePositive {
				summary.FalsePositives++
			}
			if result.Regression {
				summary.Regressions++
			}
			summary.MeanCPUOverRequest += result.CPUOverRequest
			summary.MeanCPUSavings += result.CPUSavings
			summary.MeanMemorySavings += result.MemorySavings
			if result.DemandError > 0 {
				demandErrors = append(demandErrors, result.DemandError)
			}
		}
		count := float64(len(datasets))
		summary.MeanCPUOverRequest = round(summary.MeanCPUOverRequest / count)
		summary.MeanCPUSavings = round(summary.MeanCPUSavings / count)
		summary.MeanMemorySavings = round(summary.MeanMemorySavings / count)
		if mean, n := stats.Mean(demandErrors); n > 0 {
			summary.MeanDemandError = round(mean)
		}
		report.Summaries = append(report.Summaries, summary)
	}
	sort.SliceStable(report.Results, func(left, right int) bool {
		if report.Results[left].Dataset != report.Results[right].Dataset {
			return report.Results[left].Dataset < report.Results[right].Dataset
		}
		return report.Results[left].Strategy < report.Results[right].Strategy
	})
	return report
}
