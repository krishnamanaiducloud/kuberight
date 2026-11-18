package optimizer

import (
	"fmt"
	"math"
	"strconv"
	"strings"
)

const (
	mebibyte = int64(1024 * 1024)
	gibibyte = 1024 * mebibyte
)

// CPUQuantity renders millicores the way Kubernetes canonicalizes them:
// whole cores without a suffix ("2"), everything else in millicores
// ("250m", "1500m"). Zero renders as "0".
func CPUQuantity(millicores int64) string {
	if millicores <= 0 {
		return "0"
	}
	if millicores%1000 == 0 {
		return strconv.FormatInt(millicores/1000, 10)
	}
	return strconv.FormatInt(millicores, 10) + "m"
}

// MemoryQuantity renders bytes with the largest binary suffix that
// represents the value exactly ("512Mi", "2Gi"), falling back to plain bytes.
func MemoryQuantity(bytes int64) string {
	if bytes <= 0 {
		return "0"
	}
	for _, unit := range []struct {
		suffix string
		size   int64
	}{{"Ti", 1024 * gibibyte}, {"Gi", gibibyte}, {"Mi", mebibyte}, {"Ki", 1024}} {
		if bytes%unit.size == 0 {
			return strconv.FormatInt(bytes/unit.size, 10) + unit.suffix
		}
	}
	return strconv.FormatInt(bytes, 10)
}

// RoundCPU rounds a CPU recommendation up to a whole millicore and, above one
// core, up to 5 millicores, so patches stay readable without shrinking.
func RoundCPU(millicores int64) int64 {
	if millicores <= 1000 {
		return millicores
	}
	return int64(math.Ceil(float64(millicores)/5) * 5)
}

// RoundMemory rounds a memory recommendation up to a whole mebibyte, the
// granularity the dashboard and patches use.
func RoundMemory(bytes int64) int64 {
	if bytes <= 0 {
		return bytes
	}
	return int64(math.Ceil(float64(bytes)/float64(mebibyte))) * mebibyte
}

// ParseCPUQuantity converts a Kubernetes CPU quantity ("250m", "0.5", "2")
// to millicores, rounding a fractional millicore up as Kubernetes does.
func ParseCPUQuantity(value string) (int64, error) {
	text := strings.TrimSpace(value)
	if text == "" {
		return 0, fmt.Errorf("empty CPU quantity")
	}
	if strings.HasSuffix(text, "m") {
		milli, err := strconv.ParseFloat(strings.TrimSuffix(text, "m"), 64)
		if err != nil || milli < 0 || math.IsInf(milli, 0) {
			return 0, fmt.Errorf("invalid CPU quantity %q", value)
		}
		return int64(math.Ceil(milli)), nil
	}
	cores, err := strconv.ParseFloat(text, 64)
	if err != nil || cores < 0 || math.IsInf(cores, 0) || cores > float64(maxCPUMilli)/1000 {
		return 0, fmt.Errorf("invalid CPU quantity %q", value)
	}
	return int64(math.Ceil(cores * 1000)), nil
}

// ParseMemoryQuantity converts a Kubernetes memory quantity with a binary
// (Ki, Mi, Gi, Ti) or decimal (k, M, G, T) suffix, or plain bytes.
func ParseMemoryQuantity(value string) (int64, error) {
	text := strings.TrimSpace(value)
	multipliers := []struct {
		suffix string
		factor float64
	}{
		{"Ki", 1 << 10}, {"Mi", 1 << 20}, {"Gi", 1 << 30}, {"Ti", 1 << 40},
		{"k", 1e3}, {"M", 1e6}, {"G", 1e9}, {"T", 1e12},
	}
	factor := 1.0
	for _, candidate := range multipliers {
		if strings.HasSuffix(text, candidate.suffix) {
			factor = candidate.factor
			text = strings.TrimSuffix(text, candidate.suffix)
			break
		}
	}
	number, err := strconv.ParseFloat(text, 64)
	if err != nil || number < 0 || math.IsInf(number, 0) || number*factor > float64(maxMemoryBytes) {
		return 0, fmt.Errorf("invalid memory quantity %q", value)
	}
	return int64(math.Ceil(number * factor)), nil
}

// Quantities are requests and limits as Kubernetes quantity strings. An
// empty limit means the container sets none.
type Quantities struct {
	CPURequest    string `json:"cpuRequest"`
	CPULimit      string `json:"cpuLimit,omitempty"`
	MemoryRequest string `json:"memoryRequest"`
	MemoryLimit   string `json:"memoryLimit,omitempty"`
}

func quantities(cpuRequest, cpuLimit, memoryRequest, memoryLimit int64) Quantities {
	result := Quantities{CPURequest: CPUQuantity(cpuRequest), MemoryRequest: MemoryQuantity(memoryRequest)}
	if cpuLimit > 0 {
		result.CPULimit = CPUQuantity(cpuLimit)
	}
	if memoryLimit > 0 {
		result.MemoryLimit = MemoryQuantity(memoryLimit)
	}
	return result
}
