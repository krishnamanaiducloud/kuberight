package benchmark

import (
	"context"
	"fmt"
	"math"
	"math/rand/v2"
	"runtime"
	"sort"
	"sync"

	"github.com/kuberight/kuberight/internal/forecast"
)

// ForecastTraceConfig controls the forecast backtest of a trace.
type ForecastTraceConfig struct {
	// Limit is how many workloads, drawn at random with Seed, are scored.
	Limit            int
	Horizon, Origins int
	Level            float64
	BootstrapSamples int
	Seed             uint64
}

// DefaultForecastTraceConfig scores 200 workloads with three 24-hour
// origins and 90% intervals.
func DefaultForecastTraceConfig() ForecastTraceConfig {
	return ForecastTraceConfig{Limit: 200, Horizon: 24, Origins: 3, Level: 0.9, BootstrapSamples: 2000, Seed: 20261119}
}

// ForecastSummary is the built-in engine's accuracy on one resource.
type ForecastSummary struct {
	Resource string `json:"resource"`
	// Workloads were scored; Skipped had gaps the engine would not bridge.
	Workloads int `json:"workloads"`
	Skipped   int `json:"skipped"`
	// Coverage is the mean share of held-out hours inside the interval.
	Coverage   float64  `json:"coverage"`
	CoverageCI Interval `json:"coverageCI"`
	// WinRate is the share of workloads where the engine's MAE is lower than
	// seasonal naive's; workloads where both are exact are left out.
	WinRate   float64  `json:"winRate"`
	WinRateCI Interval `json:"winRateCI"`
	// MedianSkill is 1 - MAE/baseline MAE, the median over workloads.
	MedianSkill float64 `json:"medianSkill"`
	// MedianWAPE and MedianBaselineWAPE compare total absolute error with
	// total usage, for the engine and for seasonal naive.
	MedianWAPE         float64 `json:"medianWape"`
	MedianBaselineWAPE float64 `json:"medianBaselineWape"`
	// Models counts which model the engine chose at each origin.
	Models map[string]int `json:"models"`
}

// ForecastTraceReport is the forecast trace benchmark output.
type ForecastTraceReport struct {
	Methodology map[string]any    `json:"methodology"`
	Summaries   []ForecastSummary `json:"summaries"`
}

// hourly averages the trace's five-minute samples into hours. An hour with
// fewer than half its samples is missing; gaps of up to three hours are
// interpolated, and a workload with a longer gap is skipped.
func hourly(values []float64) ([]float64, bool) {
	per := 3600 / stepSeconds
	out := make([]float64, len(values)/per)
	for hour := range out {
		total, count := 0.0, 0
		for _, value := range values[hour*per : (hour+1)*per] {
			if !math.IsNaN(value) {
				total += value
				count++
			}
		}
		out[hour] = math.NaN()
		if count*2 >= per {
			out[hour] = total / float64(count)
		}
	}
	for index := 0; index < len(out); {
		if !math.IsNaN(out[index]) {
			index++
			continue
		}
		end := index
		for end < len(out) && math.IsNaN(out[end]) {
			end++
		}
		if index == 0 || end == len(out) || end-index > 3 {
			return nil, false
		}
		for slot := index; slot < end; slot++ {
			fraction := float64(slot-index+1) / float64(end-index+1)
			out[slot] = out[index-1] + fraction*(out[end]-out[index-1])
		}
		index = end
	}
	return out, true
}

type forecastOutcome struct {
	ok                    bool
	coverage              float64
	skill, wape, baseline *float64
	models                []string
}

// RunForecastTraces backtests the built-in engine on each sampled workload's
// hourly CPU and memory, exactly as live forecasts select and calibrate.
func RunForecastTraces(series []TraceSeries, config ForecastTraceConfig) ForecastTraceReport {
	random := rand.New(rand.NewPCG(config.Seed, config.Seed^0x9e3779b97f4a7c15))
	order := random.Perm(len(series))
	if len(order) > config.Limit {
		order = order[:config.Limit]
	}
	report := ForecastTraceReport{Methodology: map[string]any{
		"engine":     forecast.EngineVersion,
		"workloads":  fmt.Sprintf("%d drawn at random (seed %d) from %d", len(order), config.Seed, len(series)),
		"series":     "hourly means of the five-minute samples; gaps up to 3 hours interpolated, longer gaps skip the workload",
		"backtest":   fmt.Sprintf("%d rolling origins of %d hours; each forecast sees only earlier hours and selects its model the same way live forecasts do", config.Origins, config.Horizon),
		"interval":   fmt.Sprintf("%.0f%% prediction interval", config.Level*100),
		"baseline":   "seasonal naive: the same hour one day earlier",
		"confidence": fmt.Sprintf("95%% percentile bootstrap over workloads, %d resamples", config.BootstrapSamples),
	}}
	engine := forecast.NewEngine(runtime.GOMAXPROCS(0))
	for _, resource := range []string{"cpu", "memory"} {
		outcomes := make([]forecastOutcome, len(order))
		var group sync.WaitGroup
		jobs := make(chan int)
		for range runtime.GOMAXPROCS(0) {
			group.Add(1)
			go func() {
				defer group.Done()
				for position := range jobs {
					item := series[order[position]]
					raw := item.CPUMilli
					if resource == "memory" {
						raw = item.MemoryBytes
					}
					values, ok := hourly(unpack(raw))
					if !ok {
						continue
					}
					response, err := engine.Backtest(context.Background(), forecast.BacktestRequest{
						Samples: values, Horizon: config.Horizon, Origins: config.Origins, IntervalSeconds: 3600, Resource: resource, ConfidenceLevel: config.Level,
					})
					if err != nil {
						continue
					}
					outcome := forecastOutcome{ok: true, coverage: response.Overall.Coverage, skill: response.Overall.Skill, wape: response.Overall.WAPE}
					if overall := response.Overall; overall.WAPE != nil && overall.MAE > 0 {
						baseline := *overall.WAPE * overall.BaselineMAE / overall.MAE
						outcome.baseline = &baseline
					}
					for _, origin := range response.Origins {
						outcome.models = append(outcome.models, origin.Model)
					}
					outcomes[position] = outcome
				}
			}()
		}
		for position := range order {
			jobs <- position
		}
		close(jobs)
		group.Wait()
		report.Summaries = append(report.Summaries, summarizeForecasts(resource, outcomes, config))
	}
	return report
}

func summarizeForecasts(resource string, outcomes []forecastOutcome, config ForecastTraceConfig) ForecastSummary {
	summary := ForecastSummary{Resource: resource, Models: map[string]int{}}
	var scored []forecastOutcome
	var skills, wapes, baselines []float64
	for _, outcome := range outcomes {
		if !outcome.ok {
			summary.Skipped++
			continue
		}
		scored = append(scored, outcome)
		for _, model := range outcome.models {
			summary.Models[model]++
		}
		if outcome.skill != nil {
			skills = append(skills, *outcome.skill)
		}
		if outcome.wape != nil && outcome.baseline != nil {
			wapes, baselines = append(wapes, *outcome.wape), append(baselines, *outcome.baseline)
		}
	}
	summary.Workloads = len(scored)
	if len(scored) == 0 {
		return summary
	}
	coverage := func(selection []forecastOutcome) float64 {
		total := 0.0
		for _, outcome := range selection {
			total += outcome.coverage
		}
		return total / float64(len(selection))
	}
	winRate := func(selection []forecastOutcome) float64 {
		wins, counted := 0, 0
		for _, outcome := range selection {
			if outcome.skill != nil {
				counted++
				if *outcome.skill > 0 {
					wins++
				}
			}
		}
		if counted == 0 {
			return 0
		}
		return float64(wins) / float64(counted)
	}
	random := rand.New(rand.NewPCG(config.Seed+1, config.Seed^0x9e3779b97f4a7c15))
	interval := func(measure func([]forecastOutcome) float64) Interval {
		estimates := make([]float64, config.BootstrapSamples)
		sample := make([]forecastOutcome, len(scored))
		for resample := range estimates {
			for index := range sample {
				sample[index] = scored[random.IntN(len(scored))]
			}
			estimates[resample] = measure(sample)
		}
		sort.Float64s(estimates)
		at := func(quantile float64) float64 {
			return round(estimates[int(math.Round(quantile*float64(len(estimates)-1)))])
		}
		return Interval{Low: at(0.025), High: at(0.975)}
	}
	summary.Coverage, summary.CoverageCI = round(coverage(scored)), interval(coverage)
	summary.WinRate, summary.WinRateCI = round(winRate(scored)), interval(winRate)
	summary.MedianSkill = round(medianOf(skills))
	summary.MedianWAPE, summary.MedianBaselineWAPE = round(medianOf(wapes)), round(medianOf(baselines))
	return summary
}

func medianOf(values []float64) float64 {
	if len(values) == 0 {
		return 0
	}
	sorted := append([]float64(nil), values...)
	sort.Float64s(sorted)
	middle := len(sorted) / 2
	if len(sorted)%2 == 1 {
		return sorted[middle]
	}
	return (sorted[middle-1] + sorted[middle]) / 2
}
