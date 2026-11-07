package optimizer

import (
	"fmt"
	"math"
)

// RuntimeJVM is the only managed runtime with dedicated memory rules.
const RuntimeJVM = "jvm"

const (
	// jvmNonHeapOverhead covers metaspace, code cache, thread stacks, and
	// direct buffers on top of the configured maximum heap.
	jvmNonHeapOverhead = 0.25
	// jvmMaximumReduction bounds memory reductions when the heap size is not
	// known, because a JVM's working set reflects heap sizing and garbage
	// collection timing, not live data.
	jvmMaximumReduction = 0.10
)

// RuntimeContext describes a managed runtime detected in the container spec.
// Source is "env", "command", or "image"; image-only detection is a
// heuristic and only makes sizing more cautious.
type RuntimeContext struct {
	Kind             string  `json:"kind"`
	Source           string  `json:"source,omitempty"`
	MaxHeapBytes     int64   `json:"maxHeapBytes,omitempty"`
	MaxRAMPercentage float64 `json:"maxRamPercentage,omitempty"`
}

// runtimeMemoryBound limits memory reductions for JVMs whose heap is not
// pinned by -Xmx: the heap either scales with the limit (MaxRAMPercentage)
// or follows JVM defaults, so a lower limit shrinks the heap itself.
func runtimeMemoryBound(runtime *RuntimeContext, bound float64, warnings []string) (float64, []string) {
	if runtime == nil || runtime.Kind != RuntimeJVM || runtime.MaxHeapBytes > 0 {
		return bound, warnings
	}
	if bound > jvmMaximumReduction {
		bound = jvmMaximumReduction
	}
	if runtime.MaxRAMPercentage > 0 {
		return bound, append(warnings, fmt.Sprintf("the JVM heap is %.0f%% of the memory limit, so lowering the limit shrinks the heap; memory reductions are limited to 10%% and JVM heap metrics should confirm garbage-collection headroom", runtime.MaxRAMPercentage))
	}
	return bound, append(warnings, "a JVM was detected without an explicit maximum heap; its working set reflects heap sizing and garbage-collection timing rather than live data, so memory reductions are limited to 10% until JVM heap metrics are reviewed")
}

// runtimeMemoryFloor keeps memory above an explicit -Xmx plus non-heap
// overhead: below it the JVM is OOM-killed when the heap fills, however low
// the observed working set was.
func runtimeMemoryFloor(input Input, request, limit, maximum int64, warnings []string) (int64, int64, []string) {
	runtime := input.Runtime
	if runtime == nil || runtime.Kind != RuntimeJVM || runtime.MaxHeapBytes <= 0 {
		return request, limit, warnings
	}
	floor := min(int64(math.Ceil(float64(runtime.MaxHeapBytes)*(1+jvmNonHeapOverhead))), maximum)
	raised := false
	if request < floor {
		request, raised = floor, true
	}
	if limit > 0 && limit < floor {
		limit, raised = floor, true
	}
	if raised {
		warnings = append(warnings, fmt.Sprintf("the JVM maximum heap is %s, so memory is kept at least %s (heap plus 25%% non-heap overhead); lower -Xmx first to reduce memory", MemoryQuantity(runtime.MaxHeapBytes), MemoryQuantity(floor)))
	}
	return request, limit, warnings
}
