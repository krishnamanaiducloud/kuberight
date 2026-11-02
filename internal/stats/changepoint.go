package stats

import (
	"math"
	"sort"
)

// changePenaltyFactor scales the BIC penalty for one extra split (two
// parameters, so 2·σ²·ln n) by four, because usage samples are serially
// correlated and independent-noise BIC would accept spurious splits.
const changePenaltyFactor = 8

// ChangePoint is a sustained shift in the mean level of a series.
type ChangePoint struct {
	// Index is the first position of the new segment.
	Index      int     `json:"index"`
	MeanBefore float64 `json:"meanBefore"`
	MeanAfter  float64 `json:"meanAfter"`
	// RelativeChange is (after - before) / before, or 0 when before is 0.
	RelativeChange float64 `json:"relativeChange"`
	// Score is the shift in units of the series' noise level.
	Score float64 `json:"score"`
}

// ChangePoints finds sustained mean shifts by binary segmentation. A split is
// kept only when it lowers the squared error by more than a BIC-style penalty
// scaled by a robust noise estimate, both segments hold at least minSegment
// finite points, and the shift exceeds minRelative of the overall level.
// Short spikes therefore do not register; deployments and configuration
// changes that move the level do.
func ChangePoints(values []float64, minSegment int, minRelative float64) []ChangePoint {
	positions, raw := finitePositions(values)
	n := len(raw)
	if minSegment < 2 {
		minSegment = 2
	}
	if n < 2*minSegment {
		return nil
	}
	// Segment a median-filtered copy: the filter removes bursts shorter than
	// half its window but keeps step edges in place. Reported means use the
	// raw values.
	series := medianFilter(raw, max(3, minSegment/2|1))
	noise := noiseScale(series)
	level, _ := Mean(series)
	if noise <= 0 {
		noise = math.Max(math.Abs(level)*1e-3, 1e-9)
	}
	penalty := changePenaltyFactor * noise * noise * math.Log(float64(n))
	prefix, prefixSquares := cumulative(series)
	rawPrefix, _ := cumulative(raw)

	type segment struct{ start, end int }
	pending := []segment{{0, n}}
	splits := []int{}
	for len(pending) > 0 && len(splits) < 16 {
		current := pending[len(pending)-1]
		pending = pending[:len(pending)-1]
		bestIndex, bestGain := -1, 0.0
		whole := segmentCost(prefix, prefixSquares, current.start, current.end)
		for split := current.start + minSegment; split <= current.end-minSegment; split++ {
			gain := whole - segmentCost(prefix, prefixSquares, current.start, split) - segmentCost(prefix, prefixSquares, split, current.end)
			if gain > bestGain {
				bestIndex, bestGain = split, gain
			}
		}
		if bestIndex < 0 || bestGain <= penalty {
			continue
		}
		before := segmentMean(prefix, current.start, bestIndex)
		after := segmentMean(prefix, bestIndex, current.end)
		if math.Abs(after-before) < minRelative*math.Max(math.Abs(level), 1e-9) {
			continue
		}
		splits = append(splits, bestIndex)
		pending = append(pending, segment{current.start, bestIndex}, segment{bestIndex, current.end})
	}
	sort.Ints(splits)

	result := make([]ChangePoint, 0, len(splits))
	for index, split := range splits {
		from, to := 0, n
		if index > 0 {
			from = splits[index-1]
		}
		if index+1 < len(splits) {
			to = splits[index+1]
		}
		shift := math.Abs(segmentMean(prefix, split, to) - segmentMean(prefix, from, split))
		before, after := segmentMean(rawPrefix, from, split), segmentMean(rawPrefix, split, to)
		relative := 0.0
		if before != 0 {
			relative = (after - before) / math.Abs(before)
		}
		result = append(result, ChangePoint{
			Index:          positions[split],
			MeanBefore:     before,
			MeanAfter:      after,
			RelativeChange: relative,
			Score:          shift / noise,
		})
	}
	return result
}

// medianFilter replaces each value with the median of the window centered on
// it, shrinking the window at the edges.
func medianFilter(values []float64, window int) []float64 {
	half := window / 2
	result := make([]float64, len(values))
	buffer := make([]float64, 0, window)
	for index := range values {
		buffer = buffer[:0]
		for position := max(0, index-half); position <= min(len(values)-1, index+half); position++ {
			buffer = append(buffer, values[position])
		}
		sort.Float64s(buffer)
		result[index] = quantileSorted(buffer, 0.5)
	}
	return result
}

// noiseScale estimates the noise standard deviation from first differences
// with the MAD, so level shifts and spikes barely affect it.
func noiseScale(values []float64) float64 {
	if len(values) < 3 {
		return StdDev(values)
	}
	differences := make([]float64, len(values)-1)
	for index := 1; index < len(values); index++ {
		differences[index-1] = values[index] - values[index-1]
	}
	return MedianAbsoluteDeviation(differences) / 0.6745 / math.Sqrt2
}

func finitePositions(values []float64) ([]int, []float64) {
	positions := make([]int, 0, len(values))
	series := make([]float64, 0, len(values))
	for index, value := range values {
		if isFinite(value) {
			positions = append(positions, index)
			series = append(series, value)
		}
	}
	return positions, series
}

func cumulative(values []float64) ([]float64, []float64) {
	prefix := make([]float64, len(values)+1)
	squares := make([]float64, len(values)+1)
	for index, value := range values {
		prefix[index+1] = prefix[index] + value
		squares[index+1] = squares[index] + value*value
	}
	return prefix, squares
}

func segmentMean(prefix []float64, start, end int) float64 {
	if end <= start {
		return 0
	}
	return (prefix[end] - prefix[start]) / float64(end-start)
}

// segmentCost is the squared error of a segment around its own mean.
func segmentCost(prefix, squares []float64, start, end int) float64 {
	count := float64(end - start)
	if count <= 0 {
		return 0
	}
	sum := prefix[end] - prefix[start]
	return math.Max(0, (squares[end]-squares[start])-sum*sum/count)
}

// Seasonality is the strength of a repeating cycle of a given period.
type Seasonality struct {
	PeriodSteps int     `json:"periodSteps"`
	Strength    float64 `json:"strength"`
	Detected    bool    `json:"detected"`
}

// DetectSeasonality checks the candidate periods, in steps, and returns the
// strongest. A cycle counts when its autocorrelation is at least threshold
// and at least two full cycles were observed.
func DetectSeasonality(values []float64, candidates []int, threshold float64) Seasonality {
	filled := Interpolate(values, 6)
	best := Seasonality{}
	for _, period := range candidates {
		if period < 2 || len(filled) < 2*period {
			continue
		}
		strength := Autocorrelation(filled, period)
		if strength > best.Strength {
			best = Seasonality{PeriodSteps: period, Strength: strength}
		}
	}
	best.Detected = best.Strength >= threshold
	best.Strength = math.Round(best.Strength*1000) / 1000
	return best
}
