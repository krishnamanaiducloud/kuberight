package benchmark

import (
	"encoding/json"
	"math"
	"reflect"
	"testing"

	"github.com/kuberight/kuberight/internal/optimizer"
)

func summary(t *testing.T, report Report, strategy string) Summary {
	t.Helper()
	for _, candidate := range report.Summaries {
		if candidate.Strategy == strategy {
			return candidate
		}
	}
	t.Fatalf("strategy %s missing", strategy)
	return Summary{}
}

func TestBenchmarkIsReproducible(t *testing.T) {
	first, second := Run(DefaultConfig()), Run(DefaultConfig())
	if !reflect.DeepEqual(first, second) {
		t.Fatal("the same seed must produce identical results")
	}
	other := DefaultConfig()
	other.Seed++
	if reflect.DeepEqual(first.Results, Run(other).Results) {
		t.Fatal("a different seed must produce different data")
	}
	if _, err := json.Marshal(first); err != nil {
		t.Fatal(err)
	}
	if first.Methodology["datasets"] != 11 || len(first.Results) != 11*len(Strategies()) {
		t.Fatalf("methodology %v, %d results", first.Methodology, len(first.Results))
	}
}

// TestKubeRightIsSaferThanBaselines states the acceptance criteria: on the
// unseen week KubeRight never exposes a container to an OOM kill, never
// regresses it, and never makes a reduction the week contradicts, while the
// naive percentile baseline shows exactly those risks. Savings are bounded
// per change by the policy, so KubeRight saves less per step than one-shot
// baselines; the README reports that trade-off with these numbers.
func TestKubeRightIsSaferThanBaselines(t *testing.T) {
	report := Run(DefaultConfig())
	for _, result := range report.Results {
		if result.Strategy == "kuberight" {
			t.Logf("%-20s %-18s changed=%-5v cpu>req=%.3f mem>lim=%.3f fp=%-5v regression=%-5v demandErr=%.3f cpuSave=%+.2f memSave=%+.2f",
				result.Dataset, result.Status, result.Changed, result.CPUOverRequest, result.MemoryOverLimit, result.FalsePositive, result.Regression, result.DemandError, result.CPUSavings, result.MemorySavings)
		}
	}
	for _, item := range report.Summaries {
		t.Logf("%-14s changes=%2d oom=%d fp=%d regressions=%d cpu>req=%.3f demandErr=%.3f cpuSave=%+.3f memSave=%+.3f",
			item.Strategy, item.Changes, item.OOMExposures, item.FalsePositives, item.Regressions, item.MeanCPUOverRequest, item.MeanDemandError, item.MeanCPUSavings, item.MeanMemorySavings)
	}
	ours := summary(t, report, "kuberight")
	if ours.OOMExposures != 0 || ours.Regressions != 0 || ours.FalsePositives != 0 {
		t.Fatalf("KubeRight must not expose, regress, or wrongly reduce: %+v", ours)
	}
	if naive := summary(t, report, "p95-no-margin"); naive.OOMExposures == 0 || naive.Regressions == 0 {
		t.Fatalf("the margin-free baseline should show the risks the datasets contain: %+v", naive)
	}
	if ours.Changes == 0 || ours.MeanCPUSavings <= 0 {
		t.Fatalf("safety must not come from never recommending anything: %+v", ours)
	}
	if keep := summary(t, report, "keep-current"); keep.Changes != 0 || keep.Regressions != 0 {
		t.Fatalf("keep-current must never change anything: %+v", keep)
	}
}

func TestStatusesForHardDatasets(t *testing.T) {
	statuses := map[string]string{}
	for _, result := range Run(DefaultConfig()).Results {
		if result.Strategy == "kuberight" {
			statuses[result.Dataset] = result.Status
		}
	}
	if statuses["incomplete-metrics"] != optimizer.StatusInsufficientData {
		t.Fatalf("a third of samples missing must be insufficient data, got %s", statuses["incomplete-metrics"])
	}
	if statuses["stable"] != optimizer.StatusRecommend {
		t.Fatalf("a stable, over-provisioned service must get a recommendation, got %s", statuses["stable"])
	}
}

func TestEvaluateDefinitions(t *testing.T) {
	dataset := Dataset{
		Name:       "unit",
		TestCPU:    []float64{100, 100, 400, math.NaN()},
		TestMemory: []float64{100, 100, 300, 100},
		Current:    optimizer.ContainerResources{CPURequestMilli: 500, CPULimitMilli: 1000, MemoryRequestBytes: 500, MemoryLimitBytes: 1000},
	}
	result := Evaluate(dataset, "x", Sizing{Status: "recommend", CPURequest: 200, CPULimit: 300, MemoryRequest: 150, MemoryLimit: 200, PredictedCPU: 100})
	if !result.Changed || !result.OOMExposure || !result.FalsePositive || !result.Regression {
		t.Fatalf("a reduction below observed peaks is a false positive and a regression: %+v", result)
	}
	if result.CPUOverRequest != 0.3333 || result.MemoryOverLimit != 0.25 || result.CPUSavings != 0.6 {
		t.Fatalf("shares %+v", result)
	}
	unchanged := Evaluate(dataset, "x", keep(dataset, "keep"))
	if unchanged.Changed || unchanged.FalsePositive || unchanged.Regression {
		t.Fatalf("keeping current settings is never a regression: %+v", unchanged)
	}
	if share(nil, 1) != 0 || share([]float64{1}, 0) != 0 || limitFor(0, 10, 5) != 0 || limitFor(100, 200, 50) != 100 {
		t.Fatal("helpers")
	}
}
