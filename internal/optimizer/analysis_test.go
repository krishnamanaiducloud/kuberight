package optimizer

import (
	"math"
	"strings"
	"testing"
)

// hourly returns n hourly samples starting at base and growing by perDay.
func hourly(n int, base, perDay float64) []float64 {
	values := make([]float64, n)
	for index := range values {
		values[index] = base + perDay*float64(index)/24 + float64(index%3)
	}
	return values
}

func TestTrendDetectsSustainedGrowthOnly(t *testing.T) {
	growing := Trend(hourly(7*24, 1000, 100), 3600)
	if !growing.Significant || math.Abs(growing.SlopePerDay-100) > 2 || growing.RSquared < 0.95 {
		t.Fatalf("growing trend = %#v", growing)
	}
	flat := Trend(hourly(7*24, 1000, 0), 3600)
	if flat.Significant {
		t.Fatalf("flat usage reported a trend: %#v", flat)
	}
	// A daily cycle with no net growth is seasonality, not a trend.
	cycle := make([]float64, 7*24)
	for index := range cycle {
		cycle[index] = 1000 + 400*math.Sin(float64(index)/24*2*math.Pi)
	}
	if Trend(cycle, 3600).Significant {
		t.Fatal("a daily cycle was reported as a trend")
	}
	// Strong growth inside a few hours is warm-up, not a trend.
	if Trend(hourly(12, 1000, 2400), 3600).Significant {
		t.Fatal("less than a day of data produced a trend")
	}
	if Trend(hourly(100, 1000, 100), 0).Significant {
		t.Fatal("unknown sample spacing produced a trend")
	}
}

func TestMemoryGrowthIsSizedAndBlocksReductions(t *testing.T) {
	input := baseInput()
	input.SampleIntervalSeconds = 3600
	input.CPUSamplesMilli = hourly(7*24, 100, 0)
	input.MemorySamplesBytes = hourly(7*24, 300<<20, 20<<20)
	recommendation, err := Recommend(input)
	if err != nil {
		t.Fatal(err)
	}
	if !recommendation.Trends.Memory.Significant || recommendation.Trends.Memory.ProjectedUplift <= 0 {
		t.Fatalf("memory trend = %#v", recommendation.Trends.Memory)
	}
	if recommendation.MemoryRequestBytes < input.MemoryRequestsBytes {
		t.Fatalf("growing memory was reduced: %d", recommendation.MemoryRequestBytes)
	}
	if !strings.Contains(strings.Join(recommendation.Warnings, ";"), "possible leak") {
		t.Fatalf("missing leak warning: %v", recommendation.Warnings)
	}
	// The uplift is capped at half the sizing statistic.
	if recommendation.Trends.Memory.ProjectedUplift > recommendation.Memory.P99*0.5+1 {
		t.Fatalf("uplift %v exceeds cap", recommendation.Trends.Memory.ProjectedUplift)
	}
}

func TestMemoryLimitNeverFallsBelowObservedPeak(t *testing.T) {
	input := baseInput()
	input.MemoryRequestsBytes = 256 << 20
	input.MemoryLimitBytes = 512 << 20
	input.MemorySamplesBytes = samples(1000, 100<<20)
	// One rare spike above the P99 statistic.
	input.MemorySamplesBytes[500] = 900 << 20
	recommendation, err := Recommend(input)
	if err != nil {
		t.Fatal(err)
	}
	if float64(recommendation.MemoryLimitBytes) < 900*1.1*(1<<20) {
		t.Fatalf("memory limit %d is below the observed peak plus headroom", recommendation.MemoryLimitBytes)
	}
	if over := recommendation.Backtest.MemoryRecommended.OverLimit; over == nil || *over != 0 {
		t.Fatalf("recommended memory limit would have OOM-killed the container: %v", over)
	}
}

func TestBacktestComparesCurrentAndRecommended(t *testing.T) {
	input := baseInput()
	input.CPURequestsMilli = 100
	input.CPULimitMilli = 0
	input.CPUSamplesMilli = make([]float64, 100)
	for index := range input.CPUSamplesMilli {
		input.CPUSamplesMilli[index] = float64(50 + index*2)
	}
	recommendation, err := Recommend(input)
	if err != nil {
		t.Fatal(err)
	}
	current := recommendation.Backtest.CPUCurrent.OverRequest
	next := recommendation.Backtest.CPURecommended.OverRequest
	if current == nil || next == nil || *current != 0.74 || *next >= *current {
		t.Fatalf("current=%v recommended=%v", current, next)
	}
	if recommendation.Backtest.CPUCurrent.OverLimit != nil || recommendation.Backtest.CPURecommended.OverLimit != nil {
		t.Fatal("exceedance of an unset limit must be undefined, not zero")
	}
	input.CPURequestsMilli = 0
	unset, _ := Recommend(input)
	if unset.Backtest.CPUCurrent.OverRequest != nil {
		t.Fatal("exceedance of an unset request must be undefined")
	}
}

func TestTrendHorizonIsValidated(t *testing.T) {
	input := baseInput()
	policy := DefaultPolicy()
	policy.TrendHorizonDays = 31
	input.Policy = &policy
	if _, err := Recommend(input); err == nil {
		t.Fatal("a 31-day trend horizon was accepted")
	}
}
