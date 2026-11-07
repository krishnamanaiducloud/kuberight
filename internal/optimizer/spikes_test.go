package optimizer

import (
	"strings"
	"testing"
	"time"
)

// fiveMinute returns n samples at five-minute spacing with a baseline and
// spikes of the given height for `duration` samples at each start index.
func fiveMinute(n int, baseline, spike float64, duration int, starts ...int) []float64 {
	values := make([]float64, n)
	for index := range values {
		values[index] = baseline + float64(index%4)
	}
	for _, start := range starts {
		for offset := range duration {
			if start+offset < n {
				values[start+offset] = spike
			}
		}
	}
	return values
}

const perDay = 288 // five-minute samples per day

func TestSpikeAnalysisClassifiesPatterns(t *testing.T) {
	steady := SpikeAnalysis(fiveMinute(3*perDay, 200, 0, 0), 300, nil)
	if steady.Classification != SpikesNone || steady.Count != 0 {
		t.Fatalf("steady = %#v", steady)
	}

	rare := SpikeAnalysis(fiveMinute(3*perDay, 200, 900, 2, 400), 300, nil)
	if rare.Classification != SpikesRare || rare.Count != 1 || rare.MedianDurationMinutes != 10 || rare.PeakToMedian < 4 {
		t.Fatalf("rare = %#v", rare)
	}

	// Eight spikes a day at scattered times.
	starts := []int{}
	for day := range 3 {
		for _, offset := range []int{10, 47, 83, 121, 160, 199, 233, 270} {
			starts = append(starts, day*perDay+offset+day*3)
		}
	}
	frequent := SpikeAnalysis(fiveMinute(3*perDay, 200, 800, 2, starts...), 300, nil)
	if frequent.Classification != SpikesFrequent || frequent.SpikesPerDay < 7 {
		t.Fatalf("frequent = %#v", frequent)
	}

	// One spike every day at 09:00 UTC.
	start := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	daily := []int{}
	for day := range 4 {
		daily = append(daily, day*perDay+9*12)
	}
	recurring := SpikeAnalysis(fiveMinute(4*perDay, 200, 900, 3, daily...), 300, &start)
	if recurring.Classification != SpikesRecurring || recurring.RecurringHourUTC == nil || *recurring.RecurringHourUTC != 9 {
		t.Fatalf("recurring = %#v", recurring)
	}

	if SpikeAnalysis(fiveMinute(10, 200, 900, 1, 3), 300, nil).Classification != SpikesNone {
		t.Fatal("too few samples must not classify spikes")
	}
}

func TestDominantPhaseWrapsAroundMidnight(t *testing.T) {
	// Spikes at 23:30 and 00:30 are one hour apart on a circular day.
	starts := []int{23*12 + 6, perDay + 6, 2*perDay + 23*12 + 6, 3*perDay + 6}
	if _, share := dominantPhase(starts, 300); share < 1 {
		t.Fatalf("share = %v, want all spikes in one window", share)
	}
}

func spikyInput() Input {
	input := baseInput()
	input.SampleIntervalSeconds = 300
	starts := []int{}
	for day := range 3 {
		for _, offset := range []int{10, 47, 83, 121, 160, 199, 233, 270} {
			starts = append(starts, day*perDay+offset+day*3)
		}
	}
	input.CPUSamplesMilli = fiveMinute(3*perDay, 200, 900, 2, starts...)
	input.MemorySamplesBytes = samples(3*perDay, 400<<20)
	return input
}

func TestFrequentSpikesSizeRequestsFromP99(t *testing.T) {
	recommendation, err := Recommend(spikyInput())
	if err != nil {
		t.Fatal(err)
	}
	if recommendation.Spikes.Classification != SpikesFrequent || recommendation.Risk == "low" {
		t.Fatalf("spikes=%#v risk=%s", recommendation.Spikes, recommendation.Risk)
	}
	if float64(recommendation.CPURequestMilli) < recommendation.CPU.P99*1.3-1 {
		t.Fatalf("request %d does not cover P99 %v with margin", recommendation.CPURequestMilli, recommendation.CPU.P99)
	}
	if !strings.Contains(strings.Join(recommendation.Warnings, "|"), "spikes") {
		t.Fatalf("warnings=%v", recommendation.Warnings)
	}
}

func TestCPULimitKeepsHeadroomAboveSpikes(t *testing.T) {
	input := spikyInput()
	input.CPULimitMilli = 2000
	recommendation, err := Recommend(input)
	if err != nil {
		t.Fatal(err)
	}
	if recommendation.CPULimitMilli < 990 {
		t.Fatalf("CPU limit %d is below the 900m spike plus headroom", recommendation.CPULimitMilli)
	}
	if near := recommendation.Backtest.CPURecommended.NearLimit; near == nil || *near != 0 {
		t.Fatalf("recommended limit is pressed by spikes: %v", near)
	}
}

func TestPeakThrottlingCountsEvenWhenAverageIsLow(t *testing.T) {
	input := baseInput()
	input.CPUSamplesMilli = samples(1000, 100)
	input.MemorySamplesBytes = samples(1000, 100<<20)
	input.ObservedCPUThrottlingRatio = 0.02
	input.ObservedCPUThrottlingPeakRatio = 0.35
	recommendation, err := Recommend(input)
	if err != nil {
		t.Fatal(err)
	}
	if recommendation.CPURequestMilli < input.CPURequestsMilli || recommendation.CPULimitMilli < 2500 || recommendation.CPUThrottlingPeakRatio != 0.35 {
		t.Fatalf("peak throttling was ignored: request=%d limit=%d", recommendation.CPURequestMilli, recommendation.CPULimitMilli)
	}
	if !strings.Contains(strings.Join(recommendation.Warnings, "|"), "35.0% in peak intervals") {
		t.Fatalf("warnings=%v", recommendation.Warnings)
	}
}

func TestPinnedUsageIsTreatedAsCapped(t *testing.T) {
	input := baseInput()
	input.CPURequestsMilli, input.CPULimitMilli = 1000, 1000
	input.CPUSamplesMilli = samples(1000, 600)
	for index := 0; index < len(input.CPUSamplesMilli); index += 40 {
		input.CPUSamplesMilli[index] = 999
	}
	input.MemorySamplesBytes = samples(1000, 100<<20)
	recommendation, err := Recommend(input)
	if err != nil {
		t.Fatal(err)
	}
	if !recommendation.CPULimitPinned || recommendation.CPURequestMilli < 1000 || recommendation.CPULimitMilli < 1250 {
		t.Fatalf("pinned=%v request=%d limit=%d", recommendation.CPULimitPinned, recommendation.CPURequestMilli, recommendation.CPULimitMilli)
	}
	if near := recommendation.Backtest.CPUCurrent.NearLimit; near == nil || *near == 0 {
		t.Fatalf("current near-limit share must show pressure: %v", near)
	}
}

func TestRemoveLimitStrategy(t *testing.T) {
	input := baseInput()
	input.CPURequestsMilli, input.CPULimitMilli = 1000, 1000
	input.MemoryRequestsBytes, input.MemoryLimitBytes = 1<<30, 1<<30
	policy := DefaultPolicy()
	policy.CPULimitStrategy = CPULimitRemove
	input.Policy = &policy
	recommendation, err := Recommend(input)
	if err != nil {
		t.Fatal(err)
	}
	if recommendation.CPULimitMilli != 0 || !strings.Contains(strings.Join(recommendation.Warnings, "|"), "Guaranteed to Burstable") {
		t.Fatalf("limit=%d warnings=%v", recommendation.CPULimitMilli, recommendation.Warnings)
	}
	if recommendation.Backtest.CPURecommended.NearLimit != nil {
		t.Fatal("a removed limit has no near-limit share")
	}
	policy.CPULimitStrategy = "sometimes"
	if _, err := Recommend(input); err == nil {
		t.Fatal("unknown strategy was accepted")
	}
}
