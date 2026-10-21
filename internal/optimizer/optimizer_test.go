package optimizer

import (
	"math"
	"strings"
	"testing"
)

func samples(count int, value float64) []float64 {
	result := make([]float64, count)
	for i := range result {
		result[i] = value + float64(i%5)
	}
	return result
}

func baseInput() Input {
	return Input{
		Cluster:               "test",
		Namespace:             "default",
		Kind:                  "Deployment",
		Workload:              "demo",
		Container:             "app",
		CPURequestsMilli:      1000,
		MemoryRequestsBytes:   1024 * 1024 * 1024,
		CPULimitMilli:         2000,
		MemoryLimitBytes:      2 * 1024 * 1024 * 1024,
		CPUSamplesMilli:       samples(100, 100),
		MemorySamplesBytes:    samples(100, 100*1024*1024),
		SampleIntervalSeconds: 60,
	}
}

func TestRecommendBoundsReductionAndNeverApplies(t *testing.T) {
	recommendation, err := Recommend(baseInput())
	if err != nil {
		t.Fatal(err)
	}
	if recommendation.Applied {
		t.Fatal("recommendation engine must never apply changes")
	}
	if recommendation.CPURequestMilli < 800 || recommendation.MemoryRequestBytes < int64(math.Ceil(float64(1024*1024*1024)*0.8)) {
		t.Fatalf("unsafe reduction: %#v", recommendation)
	}
	if recommendation.CPULimitMilli < recommendation.CPURequestMilli || recommendation.MemoryLimitBytes < recommendation.MemoryRequestBytes {
		t.Fatal("limits must not be below requests")
	}
}

func TestRecommendDoesNotReduceWithSparseSamples(t *testing.T) {
	input := baseInput()
	input.CPUSamplesMilli = []float64{10, 20}
	input.MemorySamplesBytes = []float64{1024, 2048}
	recommendation, err := Recommend(input)
	if err != nil {
		t.Fatal(err)
	}
	if recommendation.CPURequestMilli < input.CPURequestsMilli || recommendation.MemoryRequestBytes < input.MemoryRequestsBytes {
		t.Fatalf("sparse samples caused a reduction: %#v", recommendation)
	}
	if recommendation.Confidence != "low" || recommendation.DataQuality.Sufficient {
		t.Fatalf("unexpected sparse-data confidence: %#v", recommendation.DataQuality)
	}
}

func TestRecommendDoesNotReduceWithoutTemporalCoverage(t *testing.T) {
	input := baseInput()
	input.SampleIntervalSeconds = 0
	recommendation, err := Recommend(input)
	if err != nil {
		t.Fatal(err)
	}
	if recommendation.CPURequestMilli < input.CPURequestsMilli || recommendation.MemoryRequestBytes < input.MemoryRequestsBytes {
		t.Fatalf("unknown temporal coverage caused a reduction: %#v", recommendation)
	}
	if recommendation.DataQuality.Sufficient || recommendation.Confidence != "low" {
		t.Fatalf("unexpected data quality without sample interval: %#v", recommendation.DataQuality)
	}
}

func TestRecommendPreservesVPAControlledResources(t *testing.T) {
	input := baseInput()
	input.Autoscaling.VPAControlsCPU = true
	input.Autoscaling.VPAControlsMemory = true
	recommendation, err := Recommend(input)
	if err != nil {
		t.Fatal(err)
	}
	if recommendation.CPURequestMilli != input.CPURequestsMilli || recommendation.MemoryRequestBytes != input.MemoryRequestsBytes {
		t.Fatal("VPA-managed requests must be left unchanged")
	}
}

func TestRecommendDoesNotReduceMemoryAfterOOMKill(t *testing.T) {
	input := baseInput()
	input.ObservedOOMKills = 1
	recommendation, err := Recommend(input)
	if err != nil {
		t.Fatal(err)
	}
	if recommendation.MemoryRequestBytes < input.MemoryRequestsBytes {
		t.Fatalf("memory request was reduced after OOM: got %d want at least %d", recommendation.MemoryRequestBytes, input.MemoryRequestsBytes)
	}
	if recommendation.MemoryLimitBytes < input.MemoryLimitBytes {
		t.Fatalf("memory limit was reduced after OOM: got %d want at least %d", recommendation.MemoryLimitBytes, input.MemoryLimitBytes)
	}
}

func TestRecommendRejectsNonFiniteAndOversizedInput(t *testing.T) {
	input := baseInput()
	input.CPUSamplesMilli[0] = math.NaN()
	if _, err := Recommend(input); err == nil || !strings.Contains(err.Error(), "outside") {
		t.Fatalf("expected non-finite sample rejection, got %v", err)
	}

	input = baseInput()
	input.CPUSamplesMilli = make([]float64, maxSamples+1)
	if _, err := Recommend(input); err == nil || !strings.Contains(err.Error(), "maximum") {
		t.Fatalf("expected sample count rejection, got %v", err)
	}
}

func TestRecommendDetectsBurstyWorkload(t *testing.T) {
	input := baseInput()
	input.CPUSamplesMilli = make([]float64, 100)
	for index := range input.CPUSamplesMilli {
		input.CPUSamplesMilli[index] = 10
	}
	input.CPUSamplesMilli[99] = 1000
	recommendation, err := Recommend(input)
	if err != nil {
		t.Fatal(err)
	}
	if !recommendation.Burstable {
		t.Fatal("expected burstable workload detection")
	}
}
