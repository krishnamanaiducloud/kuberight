package main

import "testing"

func TestMemoryTargetLeavesHeadroomBelowTheContainerLimit(t *testing.T) {
	for raw, want := range map[string]int64{
		"1073741824":   912_680_530, // 1Gi -> 85%
		" 536870912\n": 456_340_265, // 512Mi
	} {
		got, ok := memoryTarget(raw)
		if !ok || got != want {
			t.Fatalf("memoryTarget(%q) = %d, %v; want %d", raw, got, ok, want)
		}
	}
	for _, raw := range []string{"", "abc", "-1", "1048576"} {
		if _, ok := memoryTarget(raw); ok {
			t.Fatalf("memoryTarget(%q) must be ignored", raw)
		}
	}
}
