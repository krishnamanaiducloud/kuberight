package optimizer

import (
	"strings"
	"testing"
)

func TestExplicitHeapKeepsMemoryAboveHeapPlusOverhead(t *testing.T) {
	input := reducibleInput()
	// The working set is low (about 100Mi) but the heap may grow to 900Mi.
	input.Runtime = &RuntimeContext{Kind: RuntimeJVM, Source: "env", MaxHeapBytes: 900 << 20}
	recommendation := stableRecommend(t, input)
	floor := int64(900<<20) * 125 / 100
	if recommendation.MemoryRequestBytes < floor || recommendation.MemoryLimitBytes < floor {
		t.Fatalf("memory request %d / limit %d below heap floor %d", recommendation.MemoryRequestBytes, recommendation.MemoryLimitBytes, floor)
	}
	if !strings.Contains(strings.Join(recommendation.Warnings, "; "), "JVM maximum heap is 900Mi") {
		t.Fatalf("warnings = %v", recommendation.Warnings)
	}
}

func TestJVMWithoutExplicitHeapLimitsMemoryReductions(t *testing.T) {
	input := reducibleInput()
	baseline := stableRecommend(t, input)
	input.Runtime = &RuntimeContext{Kind: RuntimeJVM, Source: "image"}
	jvm := stableRecommend(t, input)
	if jvm.MemoryRequestBytes < input.MemoryRequestsBytes*9/10 || jvm.MemoryRequestBytes <= baseline.MemoryRequestBytes {
		t.Fatalf("JVM memory request %d, baseline %d, current %d", jvm.MemoryRequestBytes, baseline.MemoryRequestBytes, input.MemoryRequestsBytes)
	}
	if jvm.CPURequestMilli != baseline.CPURequestMilli {
		t.Fatalf("JVM rules must not change CPU: %d vs %d", jvm.CPURequestMilli, baseline.CPURequestMilli)
	}
	input.Runtime = &RuntimeContext{Kind: RuntimeJVM, Source: "env", MaxRAMPercentage: 75}
	percentage := stableRecommend(t, input)
	if percentage.MemoryLimitBytes < input.MemoryLimitBytes*9/10 || !strings.Contains(strings.Join(percentage.Warnings, "; "), "75% of the memory limit") {
		t.Fatalf("limit %d warnings %v", percentage.MemoryLimitBytes, percentage.Warnings)
	}
}

func TestRuntimeValidation(t *testing.T) {
	for _, runtime := range []*RuntimeContext{{Kind: "python"}, {Kind: RuntimeJVM, MaxHeapBytes: -1}, {Kind: RuntimeJVM, MaxRAMPercentage: 150}} {
		input := reducibleInput()
		input.Runtime = runtime
		if _, err := Recommend(input); err == nil {
			t.Fatalf("invalid runtime %+v was accepted", runtime)
		}
	}
}
