package optimizer

import (
	"math"
	"strings"
	"testing"
)

func completeReplicaCoverage() *ReplicaCoverage {
	return &ReplicaCoverage{
		ExpectedReplicas: 2, DiscoveredReplicas: 2, ObservedReplicas: 2, ActiveReplicas: 2,
		CPUObservedReplicas: 2, MemoryObservedReplicas: 2,
		CPUReplicaCoverage: 1, MemoryReplicaCoverage: 1, CPUTemporalCoverage: 0.95, MemoryTemporalCoverage: 0.9,
		CPUImbalanceRatio: 1.4, MemoryImbalanceRatio: 1.2, Complete: true,
		Replicas: []ReplicaQuality{
			{Pod: "demo-a", UID: "pod-a", Phase: "Running", Active: true, CPUCoverage: 1, MemoryCoverage: 0.9},
			{Pod: "demo-b", UID: "pod-b", Phase: "Running", Active: true, CPUCoverage: 0.9, MemoryCoverage: 0.9},
		},
	}
}

func TestRecommendationRetainsReplicaCoverage(t *testing.T) {
	input := baseInput()
	input.Replicas = 2
	input.ReplicaCoverage = completeReplicaCoverage()
	recommendation, err := Recommend(input)
	if err != nil {
		t.Fatal(err)
	}
	coverage := recommendation.DataQuality.ReplicaCoverage
	if coverage == nil || !coverage.Complete || coverage.ExpectedReplicas != 2 || len(coverage.Replicas) != 2 || coverage.Replicas[1].UID != "pod-b" {
		t.Fatalf("replica coverage was not retained: %#v", coverage)
	}
}

func TestIncompleteReplicaCoverageBlocksReductions(t *testing.T) {
	input := baseInput()
	input.Replicas = 2
	input.ReplicaCoverage = completeReplicaCoverage()
	input.ReplicaCoverage.Complete = false
	input.ReplicaCoverage.MissingReplicas = 1
	input.ReplicaCoverage.ObservedReplicas = 1
	input.ReplicaCoverage.CPUObservedReplicas = 1
	input.ReplicaCoverage.CPUReplicaCoverage = 0.5
	recommendation, err := Recommend(input)
	if err != nil {
		t.Fatal(err)
	}
	if recommendation.DataQuality.Sufficient || recommendation.CPURequestMilli < input.CPURequestsMilli || recommendation.MemoryRequestBytes < input.MemoryRequestsBytes {
		t.Fatalf("incomplete replicas caused a reduction: %#v", recommendation)
	}
	if !strings.Contains(strings.Join(recommendation.DataQuality.Issues, "; "), "per-replica") {
		t.Fatalf("missing coverage issue: %v", recommendation.DataQuality.Issues)
	}
}

func TestReplicaCoverageValidation(t *testing.T) {
	cases := map[string]func(*Input){
		"count mismatch": func(input *Input) { input.ReplicaCoverage.ExpectedReplicas = 3 },
		"invalid ratio":  func(input *Input) { input.ReplicaCoverage.CPUReplicaCoverage = math.NaN() },
		"bad pod":        func(input *Input) { input.ReplicaCoverage.Replicas[0].UID = "" },
		"false complete": func(input *Input) { input.ReplicaCoverage.MissingReplicas = 1 },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			input := baseInput()
			input.Replicas = 2
			input.ReplicaCoverage = completeReplicaCoverage()
			mutate(&input)
			if _, err := Recommend(input); err == nil {
				t.Fatal("invalid replica coverage was accepted")
			}
		})
	}
}
