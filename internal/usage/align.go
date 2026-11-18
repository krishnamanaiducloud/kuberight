package usage

import "time"

// AlignPoints pairs CPU and memory observations on shared timestamps so the
// optimizer can correlate them index by index. CPU is a rate and has no value
// at the first step after a container starts, while memory does, so a
// restart of every replica leaves one series a step longer than the other.
// A point without a counterpart is folded into the nearest later shared
// timestamp (or the last one) by maximum: peaks are kept, which can only make
// sizing more conservative, and no value is invented. folded counts the
// points merged this way. Without a shared timestamp the series are returned
// unchanged.
func AlignPoints(cpu, memory Series) (alignedCPU, alignedMemory Series, folded int) {
	shared := map[time.Time]bool{}
	memoryAt := make(map[time.Time]bool, len(memory.Points))
	for _, point := range memory.Points {
		memoryAt[point.Timestamp] = true
	}
	for _, point := range cpu.Points {
		if memoryAt[point.Timestamp] {
			shared[point.Timestamp] = true
		}
	}
	if len(shared) == 0 || (len(shared) == len(cpu.Points) && len(shared) == len(memory.Points)) {
		return cpu, memory, 0
	}
	alignedCPU, foldedCPU := foldOnto(cpu, shared)
	alignedMemory, foldedMemory := foldOnto(memory, shared)
	return alignedCPU, alignedMemory, foldedCPU + foldedMemory
}

// foldOnto keeps the points at shared timestamps and merges every other
// point into the next kept point, or the last kept point when none follows.
func foldOnto(series Series, shared map[time.Time]bool) (Series, int) {
	result := series
	result.Points = make([]Point, 0, len(shared))
	pending, havePending, folded := 0.0, false, 0
	for _, point := range series.Points {
		if !shared[point.Timestamp] {
			if !havePending || point.Value > pending {
				pending = point.Value
			}
			havePending = true
			folded++
			continue
		}
		kept := point
		if havePending && pending > kept.Value {
			kept.Value = pending
		}
		havePending = false
		result.Points = append(result.Points, kept)
	}
	if havePending && len(result.Points) > 0 {
		last := &result.Points[len(result.Points)-1]
		if pending > last.Value {
			last.Value = pending
		}
	}
	return result, folded
}
