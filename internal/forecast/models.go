package forecast

import (
	"math"
	"sort"
)

// predictor returns the next h values after the series it was fitted on.
type predictor func(h int) []float64

// model fits a predictor to a training series. minHistory is the shortest
// training series it accepts.
type model struct {
	name       string
	minHistory int
	fit        func(train []float64) predictor
}

// candidates are the models the engine compares, simplest first, so a tie
// in validation error goes to the simpler model. Seasonal models are
// offered only for a period that fits the data's sampling interval.
func candidates(daily, weekly int) []model {
	models := []model{
		{name: "naive", minHistory: 1, fit: fitNaive},
	}
	if daily >= 2 {
		models = append(models,
			model{name: "seasonal_naive_daily", minHistory: 2 * daily, fit: seasonalNaive(daily)},
			model{name: "seasonal_profile_daily", minHistory: 2 * daily, fit: seasonalProfile(daily)},
		)
	}
	if weekly >= 2 {
		models = append(models, model{name: "seasonal_naive_weekly", minHistory: 2 * weekly, fit: seasonalNaive(weekly)})
	}
	models = append(models,
		model{name: "exponential_smoothing", minHistory: 8, fit: fitSES},
		model{name: "damped_trend", minHistory: 12, fit: fitHolt},
	)
	if daily >= 2 {
		models = append(models, model{name: "holt_winters_daily", minHistory: 3 * daily, fit: holtWinters(daily)})
	}
	return models
}

func fitNaive(train []float64) predictor {
	last := train[len(train)-1]
	return func(h int) []float64 { return repeat(last, h) }
}

// seasonalNaive repeats the last full season.
func seasonalNaive(period int) func([]float64) predictor {
	return func(train []float64) predictor {
		season := append([]float64(nil), train[len(train)-period:]...)
		return func(h int) []float64 {
			out := make([]float64, h)
			for index := range out {
				out[index] = season[index%period]
			}
			return out
		}
	}
}

// seasonalProfile forecasts each slot of the season as the median of that
// slot over the last four seasons: a seasonal baseline that one unusual day
// cannot move.
func seasonalProfile(period int) func([]float64) predictor {
	return func(train []float64) predictor {
		cycles := min(4, len(train)/period)
		start := len(train) - cycles*period
		profile := make([]float64, period)
		values := make([]float64, cycles)
		for slot := range profile {
			for cycle := range cycles {
				values[cycle] = train[start+cycle*period+slot]
			}
			profile[slot] = median(values)
		}
		return func(h int) []float64 {
			out := make([]float64, h)
			for index := range out {
				out[index] = profile[index%period]
			}
			return out
		}
	}
}

// fitSES is simple exponential smoothing with the smoothing weight that
// minimizes the in-sample one-step squared error.
func fitSES(train []float64) predictor {
	best, bestLevel := math.Inf(1), train[len(train)-1]
	for _, alpha := range []float64{0.05, 0.1, 0.2, 0.3, 0.5, 0.7, 0.9} {
		level, sse := train[0], 0.0
		for _, value := range train[1:] {
			err := value - level
			sse += err * err
			level += alpha * err
		}
		if sse < best {
			best, bestLevel = sse, level
		}
	}
	return func(h int) []float64 { return repeat(bestLevel, h) }
}

// fitHolt is Holt's linear method with a damped trend, so a recent slope
// flattens out instead of growing without bound over a long horizon.
func fitHolt(train []float64) predictor {
	intercept, slope := linearFit(train[:min(len(train), 24)])
	best := math.Inf(1)
	var level, trend, phi float64
	for _, alpha := range []float64{0.1, 0.3, 0.5, 0.8} {
		for _, beta := range []float64{0.01, 0.05, 0.1, 0.2} {
			for _, damping := range []float64{0.8, 0.9, 0.98} {
				l, b, sse := intercept, slope, 0.0
				for _, value := range train {
					forecast := l + damping*b
					err := value - forecast
					sse += err * err
					previous := l
					l = alpha*value + (1-alpha)*forecast
					b = beta*(l-previous) + (1-beta)*damping*b
				}
				if sse < best {
					best, level, trend, phi = sse, l, b, damping
				}
			}
		}
	}
	return func(h int) []float64 {
		out, damped := make([]float64, h), 0.0
		for index := range out {
			damped += math.Pow(phi, float64(index+1))
			out[index] = level + damped*trend
		}
		return out
	}
}

// holtWinters is additive Holt-Winters with a damped trend: level, trend,
// and one seasonal offset per slot of the period.
func holtWinters(period int) func([]float64) predictor {
	return func(train []float64) predictor {
		first, second := mean(train[:period]), mean(train[period:2*period])
		initialTrend := (second - first) / float64(period)
		initialSeason := make([]float64, period)
		for slot := range initialSeason {
			initialSeason[slot] = train[slot] - first
		}
		best := math.Inf(1)
		var level, trend, phi float64
		var season []float64
		for _, alpha := range []float64{0.1, 0.3, 0.5} {
			for _, beta := range []float64{0.01, 0.05} {
				for _, gamma := range []float64{0.05, 0.1, 0.3} {
					for _, damping := range []float64{0.9, 0.98} {
						l, b := first, initialTrend
						s := append([]float64(nil), initialSeason...)
						sse := 0.0
						for t := period; t < len(train); t++ {
							slot := t % period
							forecast := l + damping*b + s[slot]
							err := train[t] - forecast
							sse += err * err
							previous := l
							l = alpha*(train[t]-s[slot]) + (1-alpha)*(l+damping*b)
							b = beta*(l-previous) + (1-beta)*damping*b
							s[slot] = gamma*(train[t]-l) + (1-gamma)*s[slot]
						}
						if sse < best {
							best, level, trend, phi, season = sse, l, b, damping, s
						}
					}
				}
			}
		}
		n := len(train)
		return func(h int) []float64 {
			out, damped := make([]float64, h), 0.0
			for index := range out {
				damped += math.Pow(phi, float64(index+1))
				out[index] = level + damped*trend + season[(n+index)%period]
			}
			return out
		}
	}
}

func repeat(value float64, h int) []float64 {
	out := make([]float64, h)
	for index := range out {
		out[index] = value
	}
	return out
}

func mean(values []float64) float64 {
	total := 0.0
	for _, value := range values {
		total += value
	}
	return total / float64(len(values))
}

func median(values []float64) float64 {
	return quantile(values, 0.5)
}

// quantile interpolates linearly between order statistics.
func quantile(values []float64, q float64) float64 {
	sorted := append([]float64(nil), values...)
	sort.Float64s(sorted)
	if len(sorted) == 1 {
		return sorted[0]
	}
	position := q * float64(len(sorted)-1)
	lower := int(math.Floor(position))
	upper := min(lower+1, len(sorted)-1)
	return sorted[lower] + (position-float64(lower))*(sorted[upper]-sorted[lower])
}

// linearFit returns the least-squares intercept and slope over the index.
func linearFit(values []float64) (intercept, slope float64) {
	n := float64(len(values))
	if n < 2 {
		return values[0], 0
	}
	var sumX, sumY, sumXY, sumXX float64
	for index, value := range values {
		x := float64(index)
		sumX += x
		sumY += value
		sumXY += x * value
		sumXX += x * x
	}
	denominator := n*sumXX - sumX*sumX
	if denominator == 0 {
		return sumY / n, 0
	}
	slope = (n*sumXY - sumX*sumY) / denominator
	return (sumY - slope*sumX) / n, slope
}
