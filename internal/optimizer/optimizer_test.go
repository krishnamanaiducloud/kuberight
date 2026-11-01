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

func TestSourceDataQualityIssuesBlockReductions(t *testing.T) {
	input := baseInput()
	input.CPUSamplesMilli = samples(1000, 100)
	input.MemorySamplesBytes = samples(1000, 100*1024*1024)
	input.DataQualityIssues = []string{"cpu history covers 40% of the expected samples"}
	recommendation, err := Recommend(input)
	if err != nil {
		t.Fatal(err)
	}
	if recommendation.CPURequestMilli < input.CPURequestsMilli || recommendation.MemoryRequestBytes < input.MemoryRequestsBytes {
		t.Fatalf("quality issues must block reductions: cpu=%d memory=%d", recommendation.CPURequestMilli, recommendation.MemoryRequestBytes)
	}
	if recommendation.Confidence != "low" || recommendation.Risk != "high" || recommendation.DataQuality.Sufficient {
		t.Fatalf("confidence=%s risk=%s sufficient=%v", recommendation.Confidence, recommendation.Risk, recommendation.DataQuality.Sufficient)
	}
	if !strings.Contains(strings.Join(recommendation.DataQuality.Issues, ";"), "40%") {
		t.Fatalf("source issue was not reported: %v", recommendation.DataQuality.Issues)
	}
}

func TestReplicasAreCarriedAndValidated(t *testing.T) {
	input := baseInput()
	input.Replicas = 3
	recommendation, err := Recommend(input)
	if err != nil || recommendation.Replicas != 3 {
		t.Fatalf("replicas=%d err=%v", recommendation.Replicas, err)
	}
	input.Replicas = -1
	if _, err := Recommend(input); err == nil {
		t.Fatal("negative replicas were accepted")
	}
	input.Replicas = 1
	input.DataQualityIssues = []string{" "}
	if _, err := Recommend(input); err == nil {
		t.Fatal("blank data-quality issue was accepted")
	}
}

func TestProfilesAreValidAndOrderedByCaution(t *testing.T) {
	profiles := Profiles()
	if len(profiles) != 3 || profiles[0].Name != "conservative" || profiles[1].Name != "balanced" || profiles[2].Name != "aggressive" {
		t.Fatalf("profiles = %#v", profiles)
	}
	for _, profile := range profiles {
		if err := validatePolicy(profile.Policy); err != nil {
			t.Fatalf("%s policy is invalid: %v", profile.Name, err)
		}
	}
	if balanced, ok := ProfileByName("balanced"); !ok || balanced != DefaultPolicy() {
		t.Fatal("balanced must equal the default policy")
	}
	if _, ok := ProfileByName("reckless"); ok {
		t.Fatal("unknown profile was found")
	}

	input := baseInput()
	input.CPUSamplesMilli = samples(1000, 100)
	input.MemorySamplesBytes = samples(1000, 100*1024*1024)
	var requests []int64
	for _, profile := range profiles {
		policy := profile.Policy
		input.Policy = &policy
		recommendation, err := Recommend(input)
		if err != nil {
			t.Fatal(err)
		}
		requests = append(requests, recommendation.CPURequestMilli)
	}
	if !(requests[0] > requests[1] && requests[1] > requests[2]) {
		t.Fatalf("CPU requests must fall from conservative to aggressive: %v", requests)
	}
}

func TestPercentilesSelectTheSizingStatistic(t *testing.T) {
	input := baseInput()
	input.CPURequestsMilli = 0
	input.CPUSamplesMilli = make([]float64, 100)
	for index := range input.CPUSamplesMilli {
		input.CPUSamplesMilli[index] = float64(100 + index)
	}
	policy := DefaultPolicy()
	policy.CPUSafetyMargin = 0
	policy.CPUPercentile = 1
	input.Policy = &policy
	recommendation, err := Recommend(input)
	if err != nil {
		t.Fatal(err)
	}
	if recommendation.CPURequestMilli != 199 || !strings.HasPrefix(recommendation.Reason, "peak CPU") {
		t.Fatalf("peak sizing cpu=%d reason=%q", recommendation.CPURequestMilli, recommendation.Reason)
	}
	policy.CPUPercentile = 0.5
	if _, err := Recommend(input); err == nil {
		t.Fatal("unsupported percentile was accepted")
	}
}

func TestPolicyWithoutPercentilesKeepsHistoricBehavior(t *testing.T) {
	legacy := DefaultPolicy()
	legacy.CPUPercentile, legacy.MemoryPercentile = 0, 0
	input := baseInput()
	input.Policy = &legacy
	withLegacy, err := Recommend(input)
	if err != nil {
		t.Fatal(err)
	}
	input.Policy = nil
	withDefault, _ := Recommend(input)
	if withLegacy.CPURequestMilli != withDefault.CPURequestMilli || withLegacy.MemoryRequestBytes != withDefault.MemoryRequestBytes || withLegacy.Policy.CPUPercentile != 0.95 {
		t.Fatalf("legacy=%#v default=%#v", withLegacy, withDefault)
	}
	if withDefault.Reason != "P95 CPU and P99 memory with policy safety margins, resource floors, and bounded reductions" {
		t.Fatalf("reason=%q", withDefault.Reason)
	}
}

func TestThrottlingBlocksCPUReductionAndRaisesLimit(t *testing.T) {
	input := baseInput()
	input.CPUSamplesMilli = samples(1000, 100)
	input.MemorySamplesBytes = samples(1000, 100*1024*1024)
	unthrottled, err := Recommend(input)
	if err != nil {
		t.Fatal(err)
	}
	if unthrottled.CPURequestMilli >= input.CPURequestsMilli {
		t.Fatalf("baseline should reduce CPU: %d", unthrottled.CPURequestMilli)
	}
	input.ObservedCPUThrottlingRatio = 0.12
	throttled, err := Recommend(input)
	if err != nil {
		t.Fatal(err)
	}
	if throttled.CPURequestMilli != input.CPURequestsMilli || throttled.CPULimitMilli != 2500 {
		t.Fatalf("throttled cpu request=%d limit=%d", throttled.CPURequestMilli, throttled.CPULimitMilli)
	}
	if throttled.Risk == "low" || throttled.CPUThrottlingRatio != 0.12 || !strings.Contains(strings.Join(throttled.Warnings, ";"), "throttled in 12.0%") {
		t.Fatalf("risk=%s ratio=%v warnings=%v", throttled.Risk, throttled.CPUThrottlingRatio, throttled.Warnings)
	}
	// Memory is unaffected by CPU throttling.
	if throttled.MemoryRequestBytes != unthrottled.MemoryRequestBytes {
		t.Fatalf("memory changed: %d vs %d", throttled.MemoryRequestBytes, unthrottled.MemoryRequestBytes)
	}
	input.ObservedCPUThrottlingRatio = 0.4
	severe, _ := Recommend(input)
	if severe.CPULimitMilli != 3000 {
		t.Fatalf("severe throttling limit=%d, want 3000", severe.CPULimitMilli)
	}
	input.ObservedCPUThrottlingRatio = 1.5
	if _, err := Recommend(input); err == nil {
		t.Fatal("throttling ratio above 1 was accepted")
	}
}
