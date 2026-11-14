package usage

import (
	"reflect"
	"testing"
	"time"
)

func alignSeries(start time.Time, step time.Duration, values map[int]float64, count int) Series {
	series := Series{}
	for index := 0; index < count; index++ {
		if value, ok := values[index]; ok {
			series.Points = append(series.Points, Point{Timestamp: start.Add(time.Duration(index) * step), Value: value})
		}
	}
	return series
}

func TestAlignPointsFoldsUnpairedSamplesByMaximum(t *testing.T) {
	start := time.Date(2026, 11, 14, 0, 0, 0, 0, time.UTC)
	// Every replica restarted at step 3: memory has a startup peak there,
	// while the CPU rate has no value until step 4. Step 7 is the reverse.
	cpu := alignSeries(start, time.Minute, map[int]float64{0: 10, 1: 12, 2: 11, 4: 30, 5: 14, 6: 13, 7: 50}, 8)
	memory := alignSeries(start, time.Minute, map[int]float64{0: 100, 1: 100, 2: 101, 3: 900, 4: 300, 5: 120, 6: 110}, 8)
	alignedCPU, alignedMemory, folded := AlignPoints(cpu, memory)
	if folded != 2 {
		t.Fatalf("folded = %d, want 2", folded)
	}
	if !reflect.DeepEqual(alignedCPU.Values(), []float64{10, 12, 11, 30, 14, 50}) {
		t.Fatalf("CPU = %v", alignedCPU.Values())
	}
	// The memory peak at the restart moves to the next shared step and is kept.
	if !reflect.DeepEqual(alignedMemory.Values(), []float64{100, 100, 101, 900, 120, 110}) {
		t.Fatalf("memory = %v", alignedMemory.Values())
	}
	for index := range alignedCPU.Points {
		if !alignedCPU.Points[index].Timestamp.Equal(alignedMemory.Points[index].Timestamp) {
			t.Fatalf("point %d is not paired: %v vs %v", index, alignedCPU.Points[index].Timestamp, alignedMemory.Points[index].Timestamp)
		}
	}
}

func TestAlignPointsLeavesPairedOrDisjointSeriesUnchanged(t *testing.T) {
	start := time.Date(2026, 11, 14, 0, 0, 0, 0, time.UTC)
	cpu := alignSeries(start, time.Minute, map[int]float64{0: 1, 1: 2}, 2)
	memory := alignSeries(start, time.Minute, map[int]float64{0: 3, 1: 4}, 2)
	if a, b, folded := AlignPoints(cpu, memory); folded != 0 || !reflect.DeepEqual(a, cpu) || !reflect.DeepEqual(b, memory) {
		t.Fatalf("paired series changed: folded=%d", folded)
	}
	disjoint := alignSeries(start.Add(time.Hour), time.Minute, map[int]float64{0: 5}, 1)
	if a, b, folded := AlignPoints(cpu, disjoint); folded != 0 || !reflect.DeepEqual(a, cpu) || !reflect.DeepEqual(b, disjoint) {
		t.Fatalf("disjoint series changed: folded=%d", folded)
	}
}
