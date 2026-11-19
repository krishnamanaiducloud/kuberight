package benchmark

import (
	"math"
	"testing"
)

// cyclicTrace is one workload with a daily cycle over 21 days of
// five-minute samples.
func cyclicTrace(id string, phase float64, gapAt int) TraceSeries {
	steps := 21 * stepsPerDay
	cpu, memory := make([]*float64, steps), make([]*float64, steps)
	for index := range cpu {
		if gapAt >= 0 && index >= gapAt && index < gapAt+12*5 {
			continue // five missing hours
		}
		hour := float64(index) / float64(3600/stepSeconds)
		c := 500 + 200*math.Sin(2*math.Pi*hour/24+phase) + 20*math.Sin(float64(index)*1.7)
		m := 1e9 + 1e8*math.Sin(2*math.Pi*hour/24+phase) + 1e7*math.Sin(float64(index)*0.9)
		cpu[index], memory[index] = &c, &m
	}
	return TraceSeries{ID: id, StepSeconds: stepSeconds, CPUMilli: cpu, MemoryBytes: memory, CPUCapacityMilli: 2000}
}

func TestRunForecastTracesScoresEachResource(t *testing.T) {
	series := []TraceSeries{cyclicTrace("a", 0, -1), cyclicTrace("b", 1, -1), cyclicTrace("gappy", 2, 400)}
	config := DefaultForecastTraceConfig()
	config.BootstrapSamples = 200
	report := RunForecastTraces(series, config)
	if len(report.Summaries) != 2 || report.Summaries[0].Resource != "cpu" || report.Summaries[1].Resource != "memory" {
		t.Fatalf("summaries = %+v", report.Summaries)
	}
	for _, summary := range report.Summaries {
		if summary.Workloads != 2 || summary.Skipped != 1 {
			t.Fatalf("%s: workloads=%d skipped=%d", summary.Resource, summary.Workloads, summary.Skipped)
		}
		if summary.Coverage <= 0 || summary.Coverage > 1 || summary.CoverageCI.Low > summary.Coverage || summary.CoverageCI.High < summary.Coverage {
			t.Fatalf("%s: coverage %v outside its interval %+v", summary.Resource, summary.Coverage, summary.CoverageCI)
		}
		models := 0
		for _, count := range summary.Models {
			models += count
		}
		if models != 2*config.Origins || summary.MedianWAPE <= 0 || summary.MedianBaselineWAPE <= 0 {
			t.Fatalf("%s: models=%v wape=%v baseline=%v", summary.Resource, summary.Models, summary.MedianWAPE, summary.MedianBaselineWAPE)
		}
	}
	if report.Methodology["engine"] == "" {
		t.Fatal("the report must name the engine version")
	}
}

func TestHourlyAveragesAndBridgesShortGaps(t *testing.T) {
	per := 3600 / stepSeconds
	values := make([]float64, 6*per)
	for index := range values {
		values[index] = float64(index / per)
	}
	// Hour 2 loses all its samples; hour 3 loses fewer than half.
	for index := 2 * per; index < 3*per; index++ {
		values[index] = math.NaN()
	}
	values[3*per] = math.NaN()
	out, ok := hourly(values)
	if !ok || len(out) != 6 || out[2] != 2 || out[3] != 3 {
		t.Fatalf("out=%v ok=%v", out, ok)
	}
	// A leading gap cannot be interpolated.
	values[0] = math.NaN()
	for index := 0; index < per; index++ {
		values[index] = math.NaN()
	}
	if _, ok := hourly(values); ok {
		t.Fatal("a leading gap was bridged")
	}
}
