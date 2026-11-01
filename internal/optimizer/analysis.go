package optimizer

import (
	"math"
)

const (
	secondsPerDay = 86_400
	// A trend must explain at least half the variance, grow at least 2% of
	// the mean per day, and span a full day before it influences sizing.
	minimumTrendRSquared       = 0.5
	minimumTrendGrowthPerDay   = 0.02
	minimumTrendSamples        = 20
	maximumTrendUplift         = 0.5
	defaultTrendHorizonDays    = 7
	memoryLimitPeakHeadroom    = 0.10
	exceedanceRoundingDecimals = 10_000
)

// TrendSummary is an ordinary least-squares fit of usage over time.
type TrendSummary struct {
	SlopePerDay         float64 `json:"slopePerDay"`
	GrowthPerDayPercent float64 `json:"growthPerDayPercent"`
	RSquared            float64 `json:"rSquared"`
	Significant         bool    `json:"significant"`
	// ProjectedUplift is the usage added to the sizing statistic to cover
	// growth over the policy's trend horizon. It is zero unless the trend is
	// significant and rising.
	ProjectedUplift float64 `json:"projectedUplift"`
}

// Trends holds the CPU and memory fits.
type Trends struct {
	CPU    TrendSummary `json:"cpu"`
	Memory TrendSummary `json:"memory"`
}

// Trend fits a line to evenly spaced samples. A trend is significant only
// when it is strong, material, and observed over at least a day, so normal
// daily cycles and garbage-collection sawtooth do not register as growth.
func Trend(values []float64, intervalSeconds int64) TrendSummary {
	n := len(values)
	if n < minimumTrendSamples || intervalSeconds <= 0 {
		return TrendSummary{}
	}
	var sumX, sumY, sumXY, sumXX, sumYY float64
	for index, value := range values {
		x := float64(index) * float64(intervalSeconds) / secondsPerDay
		sumX += x
		sumY += value
		sumXY += x * value
		sumXX += x * x
		sumYY += value * value
	}
	count := float64(n)
	denominator := count*sumXX - sumX*sumX
	if denominator == 0 {
		return TrendSummary{}
	}
	slope := (count*sumXY - sumX*sumY) / denominator
	mean := sumY / count
	varianceY := count*sumYY - sumY*sumY
	rSquared := 0.0
	if varianceY > 0 {
		correlation := (count*sumXY - sumX*sumY) / math.Sqrt(denominator*varianceY)
		rSquared = correlation * correlation
	}
	summary := TrendSummary{SlopePerDay: slope, RSquared: math.Round(rSquared*1000) / 1000}
	if mean > 0 {
		summary.GrowthPerDayPercent = math.Round(slope/mean*10_000) / 100
	}
	spanDays := float64(n-1) * float64(intervalSeconds) / secondsPerDay
	summary.Significant = spanDays >= 1 && rSquared >= minimumTrendRSquared && mean > 0 && math.Abs(slope/mean) >= minimumTrendGrowthPerDay
	return summary
}

// withUplift returns the statistic raised by a capped projection of a
// significant rising trend. Falling trends never lower a recommendation.
func (summary *TrendSummary) withUplift(statistic float64, horizonDays int) float64 {
	if !summary.Significant || summary.SlopePerDay <= 0 {
		return statistic
	}
	uplift := math.Min(summary.SlopePerDay*float64(horizonDays), statistic*maximumTrendUplift)
	summary.ProjectedUplift = math.Round(uplift)
	return statistic + uplift
}

// Exceedance is the share of observed samples above a threshold. A nil value
// means the threshold is unset, so exceedance is undefined.
type Exceedance struct {
	OverRequest *float64 `json:"overRequest,omitempty"`
	OverLimit   *float64 `json:"overLimit,omitempty"`
}

// Backtest replays the observed samples against current and recommended
// settings. Memory above a limit would have been OOM-killed; CPU above a
// limit would have been throttled.
type Backtest struct {
	CPUCurrent        Exceedance `json:"cpuCurrent"`
	CPURecommended    Exceedance `json:"cpuRecommended"`
	MemoryCurrent     Exceedance `json:"memoryCurrent"`
	MemoryRecommended Exceedance `json:"memoryRecommended"`
}

func exceedance(samples []float64, threshold int64) *float64 {
	if threshold <= 0 || len(samples) == 0 {
		return nil
	}
	over := 0
	for _, sample := range samples {
		if sample > float64(threshold) {
			over++
		}
	}
	ratio := math.Round(float64(over)/float64(len(samples))*exceedanceRoundingDecimals) / exceedanceRoundingDecimals
	return &ratio
}

func backtest(input Input, cpuRequest, cpuLimit, memoryRequest, memoryLimit int64) Backtest {
	return Backtest{
		CPUCurrent:        Exceedance{OverRequest: exceedance(input.CPUSamplesMilli, input.CPURequestsMilli), OverLimit: exceedance(input.CPUSamplesMilli, input.CPULimitMilli)},
		CPURecommended:    Exceedance{OverRequest: exceedance(input.CPUSamplesMilli, cpuRequest), OverLimit: exceedance(input.CPUSamplesMilli, cpuLimit)},
		MemoryCurrent:     Exceedance{OverRequest: exceedance(input.MemorySamplesBytes, input.MemoryRequestsBytes), OverLimit: exceedance(input.MemorySamplesBytes, input.MemoryLimitBytes)},
		MemoryRecommended: Exceedance{OverRequest: exceedance(input.MemorySamplesBytes, memoryRequest), OverLimit: exceedance(input.MemorySamplesBytes, memoryLimit)},
	}
}
