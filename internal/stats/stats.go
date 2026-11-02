// Package stats holds the numerical building blocks for correlation analysis
// and recommendation validation. Every function is deterministic, allocates
// only what it returns, and treats NaN as a missing observation.
package stats

import (
	"math"
	"sort"
)

// Mean returns the arithmetic mean of the finite values and how many there
// were.
func Mean(values []float64) (float64, int) {
	sum, count := 0.0, 0
	for _, value := range values {
		if isFinite(value) {
			sum += value
			count++
		}
	}
	if count == 0 {
		return 0, 0
	}
	return sum / float64(count), count
}

// StdDev returns the sample standard deviation of the finite values.
func StdDev(values []float64) float64 {
	mean, count := Mean(values)
	if count < 2 {
		return 0
	}
	sum := 0.0
	for _, value := range values {
		if isFinite(value) {
			sum += (value - mean) * (value - mean)
		}
	}
	return math.Sqrt(sum / float64(count-1))
}

// CoefficientOfVariation is the standard deviation relative to the mean, or
// zero when the mean is not positive.
func CoefficientOfVariation(values []float64) float64 {
	mean, _ := Mean(values)
	if mean <= 0 {
		return 0
	}
	return StdDev(values) / mean
}

// Quantile interpolates the q-quantile (0 to 1) of the finite values.
func Quantile(values []float64, q float64) float64 {
	sorted := finiteSorted(values)
	if len(sorted) == 0 {
		return 0
	}
	return quantileSorted(sorted, q)
}

func quantileSorted(sorted []float64, q float64) float64 {
	q = math.Max(0, math.Min(1, q))
	position := q * float64(len(sorted)-1)
	lower := int(math.Floor(position))
	upper := int(math.Ceil(position))
	return sorted[lower] + (sorted[upper]-sorted[lower])*(position-float64(lower))
}

// MedianAbsoluteDeviation is the median distance from the median, a scale
// estimate that a few spikes cannot inflate.
func MedianAbsoluteDeviation(values []float64) float64 {
	sorted := finiteSorted(values)
	if len(sorted) == 0 {
		return 0
	}
	median := quantileSorted(sorted, 0.5)
	deviations := make([]float64, len(sorted))
	for index, value := range sorted {
		deviations[index] = math.Abs(value - median)
	}
	sort.Float64s(deviations)
	return quantileSorted(deviations, 0.5)
}

func finiteSorted(values []float64) []float64 {
	sorted := make([]float64, 0, len(values))
	for _, value := range values {
		if isFinite(value) {
			sorted = append(sorted, value)
		}
	}
	sort.Float64s(sorted)
	return sorted
}

func isFinite(value float64) bool {
	return !math.IsNaN(value) && !math.IsInf(value, 0)
}

// pairs returns the positions where both series have finite values.
func pairs(xs, ys []float64) ([]float64, []float64) {
	length := min(len(xs), len(ys))
	left, right := make([]float64, 0, length), make([]float64, 0, length)
	for index := range length {
		if isFinite(xs[index]) && isFinite(ys[index]) {
			left = append(left, xs[index])
			right = append(right, ys[index])
		}
	}
	return left, right
}

// Pearson returns the linear correlation of the positions where both series
// are finite. ok is false with fewer than three pairs or no variance.
func Pearson(xs, ys []float64) (r float64, n int, ok bool) {
	left, right := pairs(xs, ys)
	return pearson(left, right)
}

func pearson(xs, ys []float64) (float64, int, bool) {
	n := len(xs)
	if n < 3 {
		return 0, n, false
	}
	meanX, _ := Mean(xs)
	meanY, _ := Mean(ys)
	var covariance, varianceX, varianceY float64
	for index := range xs {
		dx, dy := xs[index]-meanX, ys[index]-meanY
		covariance += dx * dy
		varianceX += dx * dx
		varianceY += dy * dy
	}
	if varianceX <= 0 || varianceY <= 0 {
		return 0, n, false
	}
	r := covariance / math.Sqrt(varianceX*varianceY)
	return math.Max(-1, math.Min(1, r)), n, true
}

// Spearman returns the rank correlation, which measures any monotonic
// relationship and is robust to outliers and skewed usage distributions.
func Spearman(xs, ys []float64) (rho float64, n int, ok bool) {
	left, right := pairs(xs, ys)
	return pearson(ranks(left), ranks(right))
}

// ranks assigns average ranks, so ties share a rank.
func ranks(values []float64) []float64 {
	order := make([]int, len(values))
	for index := range order {
		order[index] = index
	}
	sort.SliceStable(order, func(left, right int) bool { return values[order[left]] < values[order[right]] })
	result := make([]float64, len(values))
	for start := 0; start < len(order); {
		end := start
		for end+1 < len(order) && values[order[end+1]] == values[order[start]] {
			end++
		}
		rank := float64(start+end)/2 + 1
		for position := start; position <= end; position++ {
			result[order[position]] = rank
		}
		start = end + 1
	}
	return result
}

// Autocorrelation returns the lag-k autocorrelation of the finite-valued
// series, treating missing positions as gaps.
func Autocorrelation(values []float64, lag int) float64 {
	if lag <= 0 || lag >= len(values) {
		return 0
	}
	r, _, ok := Pearson(values[:len(values)-lag], values[lag:])
	if !ok {
		return 0
	}
	return r
}

// EffectiveSampleSize discounts n for serial correlation in both series
// (Bartlett's approximation). Usage samples five minutes apart are far from
// independent, so a naive n overstates significance.
func EffectiveSampleSize(n int, lag1X, lag1Y float64) float64 {
	if n < 4 {
		return float64(n)
	}
	product := lag1X * lag1Y
	if product >= 0.999 {
		product = 0.999
	}
	effective := float64(n) * (1 - product) / (1 + product)
	return math.Max(4, math.Min(float64(n), effective))
}

// RegressionEffectiveSampleSize is the effective sample size for the
// correlation of x and y when regression errors follow an AR(1) process:
// n·(1-ρ)/(1+ρ), where ρ is the lag-1 autocorrelation of the residuals of y
// regressed on x. A genuine relationship leaves near-white residuals and keeps
// most of n; two unrelated but smooth series (shared daily cycles, trends)
// leave smooth residuals and collapse it, so they are not called significant.
func RegressionEffectiveSampleSize(xs, ys []float64) float64 {
	left, right := pairs(xs, ys)
	n := len(left)
	if n < 4 {
		return float64(n)
	}
	meanX, _ := Mean(left)
	meanY, _ := Mean(right)
	var covariance, varianceX float64
	for index := range left {
		covariance += (left[index] - meanX) * (right[index] - meanY)
		varianceX += (left[index] - meanX) * (left[index] - meanX)
	}
	slope := 0.0
	if varianceX > 0 {
		slope = covariance / varianceX
	}
	residuals := make([]float64, n)
	for index := range left {
		residuals[index] = right[index] - meanY - slope*(left[index]-meanX)
	}
	rho := math.Max(0, Autocorrelation(residuals, 1))
	rho = math.Min(rho, 0.999)
	return math.Max(4, math.Min(float64(n), float64(n)*(1-rho)/(1+rho)))
}

// FisherInterval is the 95% confidence interval of a correlation with the
// given effective sample size.
func FisherInterval(r, effectiveN float64) (lower, upper float64) {
	if effectiveN <= 3 {
		return -1, 1
	}
	clamped := math.Max(-0.999999, math.Min(0.999999, r))
	z := math.Atanh(clamped)
	margin := 1.959964 / math.Sqrt(effectiveN-3)
	return math.Tanh(z - margin), math.Tanh(z + margin)
}

// PValue is the two-sided p-value for a correlation of zero, using the
// Fisher transformation with the effective sample size.
func PValue(r, effectiveN float64) float64 {
	if effectiveN <= 3 {
		return 1
	}
	clamped := math.Max(-0.999999, math.Min(0.999999, r))
	z := math.Abs(math.Atanh(clamped)) * math.Sqrt(effectiveN-3)
	return math.Erfc(z / math.Sqrt2)
}

// LagCorrelation is the correlation of x at t with y at t+Lag.
type LagCorrelation struct {
	Lag int     `json:"lag"`
	R   float64 `json:"r"`
	N   int     `json:"n"`
}

// CrossCorrelation correlates x with y shifted by -maxLag..maxLag steps. A
// positive best lag means changes in x precede changes in y.
func CrossCorrelation(xs, ys []float64, maxLag int) []LagCorrelation {
	length := min(len(xs), len(ys))
	if maxLag < 0 {
		maxLag = 0
	}
	maxLag = min(maxLag, max(0, length-3))
	result := make([]LagCorrelation, 0, 2*maxLag+1)
	for lag := -maxLag; lag <= maxLag; lag++ {
		var left, right []float64
		if lag >= 0 {
			left, right = xs[:length-lag], ys[lag:length]
		} else {
			left, right = xs[-lag:length], ys[:length+lag]
		}
		if r, n, ok := Pearson(left, right); ok {
			result = append(result, LagCorrelation{Lag: lag, R: r, N: n})
		}
	}
	return result
}

// BestLag returns the lag with the largest absolute correlation. Ties prefer
// the smallest absolute lag, so a simultaneous relationship is not reported
// as a lead.
func BestLag(correlations []LagCorrelation) (LagCorrelation, bool) {
	best, found := LagCorrelation{}, false
	for _, candidate := range correlations {
		if !found || math.Abs(candidate.R) > math.Abs(best.R)+1e-9 ||
			(math.Abs(math.Abs(candidate.R)-math.Abs(best.R)) <= 1e-9 && absInt(candidate.Lag) < absInt(best.Lag)) {
			best, found = candidate, true
		}
	}
	return best, found
}

func absInt(value int) int {
	if value < 0 {
		return -value
	}
	return value
}

// Interpolate fills interior gaps of at most maxGap positions linearly, so a
// short scrape outage does not break lag and autocorrelation analysis.
// Leading, trailing, and longer gaps stay NaN.
func Interpolate(values []float64, maxGap int) []float64 {
	result := append([]float64(nil), values...)
	last := -1
	for index, value := range result {
		if !isFinite(value) {
			continue
		}
		if gap := index - last - 1; last >= 0 && gap > 0 && gap <= maxGap {
			from := result[last]
			for position := last + 1; position < index; position++ {
				fraction := float64(position-last) / float64(index-last)
				result[position] = from + (value-from)*fraction
			}
		}
		last = index
	}
	return result
}
