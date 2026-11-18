package correlation

import (
	"math"
	"math/rand/v2"
	"strings"
	"testing"
	"time"

	"github.com/kuberight/kuberight/internal/usage"
)

var start = time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)

func series(name string, values []float64, step time.Duration) *usage.Series {
	result := &usage.Series{Resource: name}
	for index, value := range values {
		if math.IsNaN(value) {
			continue
		}
		result.Points = append(result.Points, usage.Point{Timestamp: start.Add(time.Duration(index) * step), Value: value})
	}
	return result
}

// container builds two days of five-minute samples: CPU follows traffic with
// a daily cycle, latency follows throttling two steps later, replicas step up
// once, and a rollout lifts CPU by 60% on the second day.
func container(t *testing.T) Input {
	t.Helper()
	const step = 5 * time.Minute
	n := 2 * 288
	random := rand.New(rand.NewPCG(42, 1))
	traffic, cpu, memory, throttling, latency, replicas := make([]float64, n), make([]float64, n), make([]float64, n), make([]float64, n), make([]float64, n), make([]float64, n)
	rollout := 400
	for index := range n {
		daily := 1 + 0.5*math.Sin(2*math.Pi*float64(index)/288)
		traffic[index] = 100 * daily * (1 + random.NormFloat64()*0.03)
		level := 1.0
		if index >= rollout {
			level = 1.6
		}
		cpu[index] = traffic[index] * 2 * level * (1 + random.NormFloat64()*0.02)
		memory[index] = 512e6 + random.NormFloat64()*2e6
		throttling[index] = math.Max(0, (cpu[index]-260)/600)
		replicas[index] = 3
		if index >= 200 {
			replicas[index] = 4
		}
	}
	for index := range n {
		source := max(0, index-2)
		latency[index] = 0.05 + throttling[source]*0.4 + random.NormFloat64()*0.001
	}
	// A short scrape gap must be bridged, not treated as zero.
	cpu[100], cpu[101] = math.NaN(), math.NaN()
	return Input{
		Start: start, End: start.Add(time.Duration(n-1) * step), Step: step,
		Series: map[string]*usage.Series{
			CPU: series(CPU, cpu, step), Memory: series(Memory, memory, step), Throttling: series(Throttling, throttling, step),
			Latency: series(Latency, latency, step), Replicas: series(Replicas, replicas, step), RequestRate: series(RequestRate, traffic, step),
			"absent": nil,
		},
		Events: []usage.Event{
			{Timestamp: start.Add(time.Duration(rollout) * step), Kind: usage.EventRollout, Detail: "web-7d9f"},
			{Timestamp: start.Add(450 * step), Kind: usage.EventOOMKill, Count: 1},
		},
		MaxLag: 30 * time.Minute,
	}
}

func pair(t *testing.T, report Report, x, y string) Pair {
	t.Helper()
	for _, candidate := range report.Pairs {
		if candidate.X == x && candidate.Y == y {
			return candidate
		}
	}
	t.Fatalf("pair %s/%s missing from %+v", x, y, report.Pairs)
	return Pair{}
}

func TestAnalyzeMeasuresAssociationsLagsAndEvents(t *testing.T) {
	report := Analyze(container(t))
	if report.Points != 576 || report.StepSeconds != 300 {
		t.Fatalf("grid %d points, step %d", report.Points, report.StepSeconds)
	}
	if report.Coverage[CPU] != 1 {
		t.Fatalf("interpolated CPU coverage = %v", report.Coverage[CPU])
	}
	if report.Caveats[0] != Causation {
		t.Fatal("every report must state that correlation is not causation")
	}
	if !strings.Contains(strings.Join(report.Caveats, " "), "nodePressure is not available") {
		t.Fatalf("missing optional series must be named: %v", report.Caveats)
	}

	traffic := pair(t, report, RequestRate, CPU)
	if !traffic.Significant || traffic.Strength != "strong" || traffic.Direction != "positive" {
		t.Fatalf("traffic/CPU %+v", traffic)
	}
	// Correlations use only the regime after the rollout.
	if traffic.N != 176 || !report.CorrelationStart.Equal(start.Add(400*5*time.Minute)) ||
		!strings.Contains(strings.Join(report.Caveats, " "), "176 samples after the rollout") {
		t.Fatalf("post-rollout regime not used: n=%d start=%s caveats=%v", traffic.N, report.CorrelationStart, report.Caveats)
	}
	if traffic.Lower > traffic.R || traffic.Upper < traffic.R {
		t.Fatalf("confidence interval must contain r: %+v", traffic)
	}

	latency := pair(t, report, Throttling, Latency)
	if latency.BestLagSteps != 2 || latency.BestLagSeconds != 600 {
		t.Fatalf("latency should trail throttling by two steps: %+v", latency)
	}
	if latency.Leads != Throttling || !strings.Contains(latency.Summary, "10m0s before") {
		t.Fatalf("lead not reported: %+v", latency)
	}

	// Memory is unrelated noise: present, but not significant.
	memory := pair(t, report, Memory, Latency)
	if memory.Significant || !strings.Contains(memory.Summary, "No reliable association") {
		t.Fatalf("memory/latency %+v", memory)
	}

	// The rollout's level shift is found and tied to the rollout.
	found := false
	for _, change := range report.ChangePoints {
		if change.Series == CPU && change.NearestEvent != nil && change.NearestEvent.Kind == usage.EventRollout {
			found = change.RelativeChange > 0.3
		}
	}
	if !found {
		t.Fatalf("rollout change point missing: %+v", report.ChangePoints)
	}

	// The replica step becomes a scale-up event with its impact measured.
	scaled := false
	for _, event := range report.Events {
		if event.Kind == EventScaleUp && event.Count == 1 && event.Detail == "3 to 4 replicas" {
			scaled = true
		}
	}
	if !scaled {
		t.Fatalf("scale-up event missing: %+v", report.Events)
	}
	material := false
	for _, impact := range report.EventImpacts {
		if impact.Event.Kind == usage.EventRollout && impact.Series == CPU {
			material = impact.Material && impact.RelativeChange > 0.3 && impact.BeforeSamples >= 3
		}
	}
	if !material {
		t.Fatalf("rollout impact missing: %+v", report.EventImpacts)
	}

	if !report.Seasonality[RequestRate].Detected {
		t.Fatalf("daily traffic cycle not detected: %+v", report.Seasonality)
	}
	titles := []string{}
	for _, finding := range report.Findings {
		titles = append(titles, finding.Title)
	}
	joined := strings.Join(titles, "|")
	for _, want := range []string{"Latency moves with CPU throttling", "CPU follows traffic", "A rollout coincides with a usage shift", "OOM kills in the window"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("finding %q missing from %v", want, titles)
		}
	}
	if report.Findings[0].Severity != "critical" {
		t.Fatalf("findings must be ordered by severity: %+v", report.Findings[0])
	}
}

func TestAnalyzeWorksWithOnlyCoreMetrics(t *testing.T) {
	input := container(t)
	input.Series = map[string]*usage.Series{CPU: input.Series[CPU], Memory: input.Series[Memory]}
	input.Events = nil
	report := Analyze(input)
	if len(report.Pairs) != 1 || report.Pairs[0].X != CPU || report.Pairs[0].Y != Memory {
		t.Fatalf("only CPU/memory can be correlated: %+v", report.Pairs)
	}
	caveats := strings.Join(report.Caveats, " ")
	for _, missing := range []string{Latency, RequestRate, NodePressure} {
		if !strings.Contains(caveats, missing+" is not available") {
			t.Fatalf("caveat for %s missing: %v", missing, report.Caveats)
		}
	}
	if len(report.ChangePoints) == 0 {
		t.Fatal("level shifts are still detected from CPU alone")
	}
}

func TestPerReplicaNormalization(t *testing.T) {
	const step = 5 * time.Minute
	n := 120
	total, replicas := make([]float64, n), make([]float64, n)
	for index := range n {
		replicas[index] = float64(2 + index/30)
		total[index] = 300 * (1 + 0.1*math.Sin(float64(index)))
	}
	report := Analyze(Input{
		Start: start, End: start.Add(time.Duration(n-1) * step), Step: step,
		Series: map[string]*usage.Series{"cpuTotal": series("cpuTotal", total, step), Replicas: series(Replicas, replicas, step)},
	})
	perReplica := pair(t, report, CPUPerReplica, Replicas)
	if !perReplica.Significant || perReplica.Direction != "negative" {
		t.Fatalf("fixed total load spread over more replicas must lower per-replica CPU: %+v", perReplica)
	}
	scaleUps := 0
	for _, event := range report.Events {
		if event.Kind == EventScaleUp {
			scaleUps++
		}
	}
	if scaleUps != 3 {
		t.Fatalf("scale-ups = %d", scaleUps)
	}
}

func TestSparseAndEmptyInputs(t *testing.T) {
	if report := Analyze(Input{Start: start, End: start, Step: time.Minute}); report.Points != 0 || !strings.Contains(strings.Join(report.Caveats, " "), "empty") {
		t.Fatalf("empty window: %+v", report)
	}
	const step = time.Minute
	values := make([]float64, 100)
	for index := range values {
		values[index] = math.NaN()
		if index%10 == 0 {
			values[index] = float64(index)
		}
	}
	report := Analyze(Input{Start: start, End: start.Add(99 * step), Step: step, Series: map[string]*usage.Series{CPU: series(CPU, values, step), Throttling: series(Throttling, values, step)}})
	if len(report.Pairs) != 0 || !strings.Contains(strings.Join(report.Caveats, " "), "cpu covers only 10%") {
		t.Fatalf("sparse series must be skipped with a caveat: %+v %v", report.Pairs, report.Caveats)
	}
	if grid := Align(start, start.Add(time.Hour), 0, nil); len(grid.Timestamps) != 0 || grid.Coverage(CPU) != 0 {
		t.Fatal("invalid step yields an empty grid")
	}
}

func TestHelpers(t *testing.T) {
	cases := map[float64][2]string{0.9: {"strong", "positive"}, -0.5: {"moderate", "negative"}, 0.25: {"weak", "positive"}, 0.05: {"negligible", "none"}}
	for r, want := range cases {
		if strength(r) != want[0] || direction(r) != want[1] {
			t.Fatalf("r=%v: %s %s", r, strength(r), direction(r))
		}
	}
	if label("") != "" || label("cpu") != "Cpu" || roundP(1e-9) != 1e-6 || severityRank("info") != 0 {
		t.Fatal("helpers")
	}
	if absDuration(-time.Second) != time.Second || absInt(-2) != 2 {
		t.Fatal("abs helpers")
	}
}
