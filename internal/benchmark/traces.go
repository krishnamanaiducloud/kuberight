package benchmark

import (
	"bufio"
	"compress/gzip"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"math/rand/v2"
	"runtime"
	"sort"
	"strings"
	"sync"

	"github.com/kuberight/kuberight/internal/optimizer"
)

// TraceSeries is one workload's measured usage in KubeRight's trace format:
// gzip or plain JSON Lines, one workload per line, CPU in millicores and
// memory in bytes per step, null for a missing sample.
type TraceSeries struct {
	ID               string     `json:"id"`
	StepSeconds      int        `json:"stepSeconds"`
	CPUMilli         []*float64 `json:"cpuMilli"`
	MemoryBytes      []*float64 `json:"memoryBytes"`
	CPUCapacityMilli int64      `json:"cpuCapacityMilli"`
}

// maxTraceLine bounds one JSON line (a month of five-minute samples is
// about 200 KB).
const maxTraceLine = 16 << 20

// ReadTraces parses a trace file, gzip-compressed or not.
func ReadTraces(source io.Reader) ([]TraceSeries, error) {
	buffered := bufio.NewReader(source)
	if magic, err := buffered.Peek(2); err == nil && magic[0] == 0x1f && magic[1] == 0x8b {
		decompressed, err := gzip.NewReader(buffered)
		if err != nil {
			return nil, fmt.Errorf("open gzip trace: %w", err)
		}
		defer func() { _ = decompressed.Close() }()
		buffered = bufio.NewReader(decompressed)
	}
	scanner := bufio.NewScanner(buffered)
	scanner.Buffer(make([]byte, 1<<20), maxTraceLine)
	series := []TraceSeries{}
	for line := 1; scanner.Scan(); line++ {
		text := strings.TrimSpace(scanner.Text())
		if text == "" {
			continue
		}
		var item TraceSeries
		if err := json.Unmarshal([]byte(text), &item); err != nil {
			return nil, fmt.Errorf("trace line %d: %w", line, err)
		}
		if item.StepSeconds != stepSeconds {
			return nil, fmt.Errorf("trace line %d: step is %d s; the benchmark uses %d s steps", line, item.StepSeconds, stepSeconds)
		}
		if len(item.CPUMilli) != len(item.MemoryBytes) {
			return nil, fmt.Errorf("trace line %d: CPU and memory series differ in length", line)
		}
		series = append(series, item)
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read trace: %w", err)
	}
	if len(series) == 0 {
		return nil, errors.New("the trace contains no workloads")
	}
	return series, nil
}

// TraceConfig controls the rolling-origin evaluation of a trace.
type TraceConfig struct {
	TrainDays, TestDays, Folds int
	// MemoryHeadroom sets the assumed current memory request and limit as a
	// multiple of the training peak, for traces without provisioned memory.
	MemoryHeadroom   float64
	BootstrapSamples int
	Seed             uint64
}

// DefaultTraceConfig evaluates three consecutive weeks of a month.
func DefaultTraceConfig() TraceConfig {
	return TraceConfig{TrainDays: 7, TestDays: 7, Folds: 3, MemoryHeadroom: 1.5, BootstrapSamples: 2000, Seed: 20261114}
}

func unpack(values []*float64) []float64 {
	result := make([]float64, len(values))
	for index, value := range values {
		if value == nil || math.IsNaN(*value) || math.IsInf(*value, 0) || *value < 0 {
			result[index] = math.NaN()
		} else {
			result[index] = *value
		}
	}
	return result
}

// TraceDatasets turns each workload into rolling-origin folds: train on
// TrainDays, score on the following TestDays, then move both forward by
// TestDays. The current CPU request and limit are the provisioned CPU.
func TraceDatasets(series []TraceSeries, config TraceConfig) []Dataset {
	train, test := config.TrainDays*stepsPerDay, config.TestDays*stepsPerDay
	datasets := []Dataset{}
	for _, item := range series {
		if item.CPUCapacityMilli <= 0 {
			continue
		}
		cpu, memory := unpack(item.CPUMilli), unpack(item.MemoryBytes)
		for fold := 0; fold < config.Folds; fold++ {
			start := fold * test
			if start+train+test > len(cpu) {
				break
			}
			dataset := Dataset{
				Name: fmt.Sprintf("%s/fold%d", item.ID, fold+1), Kind: "Deployment", Container: "app", Replicas: 1, RolloutIndex: -1,
				TrainCPU: cpu[start : start+train], TrainMemory: memory[start : start+train],
				TestCPU: cpu[start+train : start+train+test], TestMemory: memory[start+train : start+train+test],
			}
			trainMemory, testCPU := finite(dataset.TrainMemory), finite(dataset.TestCPU)
			if len(trainMemory) == 0 || len(testCPU) < test/2 {
				continue
			}
			memoryPeak := 0.0
			for _, value := range trainMemory {
				memoryPeak = math.Max(memoryPeak, value)
			}
			assumedMemory := int64(math.Ceil(memoryPeak * config.MemoryHeadroom))
			dataset.Current = optimizer.ContainerResources{
				CPURequestMilli: item.CPUCapacityMilli, CPULimitMilli: item.CPUCapacityMilli,
				MemoryRequestBytes: assumedMemory, MemoryLimitBytes: assumedMemory,
			}
			datasets = append(datasets, dataset)
		}
	}
	return datasets
}

// Interval is a two-sided 95% bootstrap confidence interval.
type Interval struct {
	Low  float64 `json:"low"`
	High float64 `json:"high"`
}

// TraceSummary is one strategy's outcome on a trace, with confidence
// intervals from a bootstrap over workloads (all folds of a resampled
// workload are kept together, because they are not independent).
type TraceSummary struct {
	Strategy                string   `json:"strategy"`
	Description             string   `json:"description"`
	Evaluations             int      `json:"evaluations"`
	ChangeRate              float64  `json:"changeRate"`
	FalsePositiveRate       float64  `json:"falsePositiveRate"`
	FalsePositiveRateCI     Interval `json:"falsePositiveRateCI"`
	CPUUnderProvisionedRate float64  `json:"cpuUnderProvisionedRate"`
	OOMExposureRate         float64  `json:"oomExposureRate"`
	OOMExposureRateCI       Interval `json:"oomExposureRateCI"`
	RegressionRate          float64  `json:"regressionRate"`
	RegressionRateCI        Interval `json:"regressionRateCI"`
	MeanCPUOverRequest      float64  `json:"meanCpuOverRequest"`
	MeanCPUSavings          float64  `json:"meanCpuSavings"`
	MeanCPUSavingsCI        Interval `json:"meanCpuSavingsCI"`
}

// TraceReport is the trace benchmark output.
type TraceReport struct {
	Methodology map[string]any `json:"methodology"`
	Summaries   []TraceSummary `json:"summaries"`
}

// TraceStrategies are the synthetic benchmark's strategies plus KubeRight's
// aggressive profile, to show the range its policies span.
func TraceStrategies() []Strategy {
	strategies := Strategies()
	aggressive, _ := optimizer.ProfileByName("aggressive")
	return append(strategies[:1:1], append([]Strategy{{
		Name: "kuberight-aggressive", Description: "KubeRight aggressive profile (larger steps, shorter cooldown); only a \"recommend\" verdict changes settings",
		Size: func(dataset Dataset) Sizing { return kuberightWithPolicy(dataset, &aggressive) },
	}}, strategies[1:]...)...)
}

// cpuUnderProvisionedShare is the share of test samples above the CPU
// request beyond which a workload counts as CPU under-provisioned.
const cpuUnderProvisionedShare = 0.05

// RunTraces evaluates every trace strategy on every fold.
func RunTraces(datasets []Dataset, config TraceConfig) TraceReport {
	strategies := TraceStrategies()
	results := make([][]Result, len(strategies))
	for index := range strategies {
		results[index] = make([]Result, len(datasets))
	}
	type job struct{ strategy, dataset int }
	jobs := make(chan job)
	var group sync.WaitGroup
	for range runtime.GOMAXPROCS(0) {
		group.Add(1)
		go func() {
			defer group.Done()
			for next := range jobs {
				strategy, dataset := strategies[next.strategy], datasets[next.dataset]
				results[next.strategy][next.dataset] = Evaluate(dataset, strategy.Name, strategy.Size(dataset))
			}
		}()
	}
	for strategy := range strategies {
		for dataset := range datasets {
			jobs <- job{strategy, dataset}
		}
	}
	close(jobs)
	group.Wait()

	// Folds of one workload share its name before the "/fold" suffix.
	workloads := map[string][]int{}
	for index, dataset := range datasets {
		workload := dataset.Name[:strings.LastIndex(dataset.Name, "/fold")]
		workloads[workload] = append(workloads[workload], index)
	}
	names := make([]string, 0, len(workloads))
	for name := range workloads {
		names = append(names, name)
	}
	sort.Strings(names)

	report := TraceReport{Methodology: map[string]any{
		"workloads": len(names), "evaluations": len(datasets), "stepSeconds": stepSeconds,
		"trainDays": config.TrainDays, "testDays": config.TestDays, "folds": config.Folds,
		"method":              "rolling origin: each strategy sizes from TrainDays and is scored on the following, unseen TestDays; the origin then moves forward by TestDays",
		"currentCpu":          "the workload's provisioned CPU, as both request and limit",
		"currentMemory":       fmt.Sprintf("assumed: %.2g times the training peak, as request and limit, because the trace has no provisioned memory; memory savings are therefore not reported", config.MemoryHeadroom),
		"falsePositive":       fmt.Sprintf("a reduction followed by CPU above the new request in more than %.0f%% of test samples, or memory above the new limit", falsePositiveCPUShare*100),
		"cpuUnderProvisioned": fmt.Sprintf("CPU above the request in more than %.0f%% of test samples", cpuUnderProvisionedShare*100),
		"regression":          fmt.Sprintf("memory above the new limit, or CPU above the new limit in more than %.0f%% of test samples, when the current settings had neither", regressionThrottleShare*100),
		"confidence":          fmt.Sprintf("95%% percentile bootstrap over workloads, %d resamples, seed %d", config.BootstrapSamples, config.Seed),
	}}
	random := rand.New(rand.NewPCG(config.Seed, config.Seed^0x9e3779b97f4a7c15))
	resamples := make([][]int, config.BootstrapSamples)
	for sample := range resamples {
		resamples[sample] = make([]int, len(names))
		for index := range names {
			resamples[sample][index] = random.IntN(len(names))
		}
	}
	for index, strategy := range strategies {
		outcomes := results[index]
		measure := func(selection []int, value func(Result) float64) float64 {
			total, count := 0.0, 0
			for _, workload := range selection {
				for _, dataset := range workloads[names[workload]] {
					total += value(outcomes[dataset])
					count++
				}
			}
			if count == 0 {
				return 0
			}
			return total / float64(count)
		}
		all := make([]int, len(names))
		for workload := range all {
			all[workload] = workload
		}
		interval := func(value func(Result) float64) Interval {
			estimates := make([]float64, len(resamples))
			for sample, selection := range resamples {
				estimates[sample] = measure(selection, value)
			}
			sort.Float64s(estimates)
			at := func(quantile float64) float64 {
				return round(estimates[int(math.Round(quantile*float64(len(estimates)-1)))])
			}
			return Interval{Low: at(0.025), High: at(0.975)}
		}
		indicator := func(predicate func(Result) bool) func(Result) float64 {
			return func(result Result) float64 {
				if predicate(result) {
					return 1
				}
				return 0
			}
		}
		falsePositive := indicator(func(result Result) bool { return result.FalsePositive })
		oom := indicator(func(result Result) bool { return result.OOMExposure })
		regression := indicator(func(result Result) bool { return result.Regression })
		savings := func(result Result) float64 { return result.CPUSavings }
		report.Summaries = append(report.Summaries, TraceSummary{
			Strategy: strategy.Name, Description: strategy.Description, Evaluations: len(datasets),
			ChangeRate:        round(measure(all, indicator(func(result Result) bool { return result.Changed }))),
			FalsePositiveRate: round(measure(all, falsePositive)), FalsePositiveRateCI: interval(falsePositive),
			CPUUnderProvisionedRate: round(measure(all, indicator(func(result Result) bool { return result.CPUOverRequest > cpuUnderProvisionedShare }))),
			OOMExposureRate:         round(measure(all, oom)), OOMExposureRateCI: interval(oom),
			RegressionRate: round(measure(all, regression)), RegressionRateCI: interval(regression),
			MeanCPUOverRequest: round(measure(all, func(result Result) float64 { return result.CPUOverRequest })),
			MeanCPUSavings:     round(measure(all, savings)), MeanCPUSavingsCI: interval(savings),
		})
	}
	return report
}
