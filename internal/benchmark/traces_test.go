package benchmark

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"math"
	"strings"
	"testing"
)

func traceLine(t *testing.T, id string, days int, cpu func(int) float64, capacity int64) string {
	t.Helper()
	cpuValues, memoryValues := make([]*float64, days*stepsPerDay), make([]*float64, days*stepsPerDay)
	for index := range cpuValues {
		if index%97 == 13 {
			continue // missing samples stay null
		}
		value, memory := cpu(index), float64(512<<20)+float64(index%stepsPerDay)*1024
		cpuValues[index], memoryValues[index] = &value, &memory
	}
	encoded, err := json.Marshal(TraceSeries{ID: id, StepSeconds: stepSeconds, CPUMilli: cpuValues, MemoryBytes: memoryValues, CPUCapacityMilli: capacity})
	if err != nil {
		t.Fatal(err)
	}
	return string(encoded)
}

func TestReadTracesAcceptsGzipAndPlainJSONLines(t *testing.T) {
	steady := func(int) float64 { return 300 }
	text := traceLine(t, "vm-1", 1, steady, 2000) + "\n\n" + traceLine(t, "vm-2", 1, steady, 4000) + "\n"
	var compressed bytes.Buffer
	writer := gzip.NewWriter(&compressed)
	_, _ = writer.Write([]byte(text))
	_ = writer.Close()
	for name, source := range map[string]*bytes.Reader{"plain": bytes.NewReader([]byte(text)), "gzip": bytes.NewReader(compressed.Bytes())} {
		series, err := ReadTraces(source)
		if err != nil || len(series) != 2 || series[1].CPUCapacityMilli != 4000 || series[0].CPUMilli[13] != nil {
			t.Fatalf("%s: %d series, err=%v", name, len(series), err)
		}
	}
	for name, input := range map[string]string{
		"empty":          "\n",
		"wrong step":     `{"id":"x","stepSeconds":60,"cpuMilli":[1],"memoryBytes":[1]}`,
		"length differs": `{"id":"x","stepSeconds":300,"cpuMilli":[1,2],"memoryBytes":[1]}`,
		"not json":       "{",
	} {
		if _, err := ReadTraces(strings.NewReader(input)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestTraceDatasetsUseRollingOriginFolds(t *testing.T) {
	steady := func(int) float64 { return 300 }
	series, err := ReadTraces(strings.NewReader(traceLine(t, "vm", 30, steady, 2000) + "\n" + traceLine(t, "no-capacity", 30, steady, 0)))
	if err != nil {
		t.Fatal(err)
	}
	datasets := TraceDatasets(series, DefaultTraceConfig())
	if len(datasets) != 3 {
		t.Fatalf("folds = %d, want 3 (the workload without provisioned CPU is skipped)", len(datasets))
	}
	for index, dataset := range datasets {
		if len(dataset.TrainCPU) != 7*stepsPerDay || len(dataset.TestCPU) != 7*stepsPerDay || dataset.Current.CPURequestMilli != 2000 {
			t.Fatalf("fold %d: train %d test %d current %+v", index, len(dataset.TrainCPU), len(dataset.TestCPU), dataset.Current)
		}
		peak := 0.0
		for _, value := range finite(dataset.TrainMemory) {
			peak = math.Max(peak, value)
		}
		if dataset.Current.MemoryLimitBytes != int64(math.Ceil(peak*1.5)) {
			t.Fatalf("fold %d: assumed memory %d for a %.0f peak", index, dataset.Current.MemoryLimitBytes, peak)
		}
	}
	// The test week of one fold is the training week of the next.
	if &datasets[0].TestCPU[0] != &datasets[1].TrainCPU[0] {
		t.Fatal("folds do not advance by one test window")
	}
}

func TestRunTracesReportsBootstrapIntervals(t *testing.T) {
	lines := []string{}
	for index := range 12 {
		level := 200 + 50*float64(index)
		lines = append(lines, traceLine(t, "vm-"+string(rune('a'+index)), 21, func(step int) float64 {
			return level + 40*math.Sin(2*math.Pi*float64(step)/stepsPerDay)
		}, 2000))
	}
	series, err := ReadTraces(strings.NewReader(strings.Join(lines, "\n")))
	if err != nil {
		t.Fatal(err)
	}
	config := DefaultTraceConfig()
	config.BootstrapSamples = 200
	report := RunTraces(TraceDatasets(series, config), config)
	if report.Methodology["workloads"] != 12 || report.Methodology["evaluations"] != 24 {
		t.Fatalf("methodology: %v", report.Methodology)
	}
	if len(report.Summaries) != len(TraceStrategies()) || report.Summaries[0].Strategy != "kuberight" || report.Summaries[1].Strategy != "kuberight-aggressive" {
		t.Fatalf("strategies: %+v", report.Summaries)
	}
	for _, summary := range report.Summaries {
		if summary.MeanCPUSavingsCI.Low > summary.MeanCPUSavings || summary.MeanCPUSavingsCI.High < summary.MeanCPUSavings {
			t.Errorf("%s: savings %.3f outside its interval %+v", summary.Strategy, summary.MeanCPUSavings, summary.MeanCPUSavingsCI)
		}
		if summary.RegressionRateCI.Low > summary.RegressionRate || summary.RegressionRateCI.High < summary.RegressionRate {
			t.Errorf("%s: regression rate %.3f outside its interval %+v", summary.Strategy, summary.RegressionRate, summary.RegressionRateCI)
		}
	}
	if keep := report.Summaries[2]; keep.Strategy != "keep-current" || keep.ChangeRate != 0 || keep.MeanCPUSavings != 0 {
		t.Fatalf("keep-current: %+v", keep)
	}
	// The same seed gives the same report.
	again := RunTraces(TraceDatasets(series, config), config)
	if again.Summaries[0] != report.Summaries[0] {
		t.Fatal("the trace benchmark is not deterministic")
	}
}
