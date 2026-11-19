package forecast

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"time"
)

// EngineVersion identifies the built-in models and selection rules. It
// changes whenever their output would.
const EngineVersion = "builtin-1"

const (
	// minimumTraining is the shortest series any validation fold fits on.
	minimumTraining = 24
	// minimumHistory is the shortest series the engine forecasts from.
	minimumHistory = 48
	// maximumHistory bounds memory and time per request.
	maximumHistory = 10_080
	maximumHorizon = 2_160
	// maximumOrigins bounds the rolling-origin validation and backtests.
	validationOrigins = 3
	// intervalOrigins is how many past origins, at most, supply the errors
	// that intervals are built from. More errors make the tail quantiles
	// trustworthy; selection still uses the newest validationOrigins.
	intervalOrigins = 14
	maximumOrigins  = 10
	// winsorizeMADs is how far from the median, in robust standard
	// deviations, a training value may lie before it is clipped for fitting.
	winsorizeMADs = 8.0
	// interpolateSteps is the longest gap filled by interpolation.
	interpolateSteps = 3
)

// Engine is KubeRight's built-in forecaster. It runs inside the API
// process, keeps no state between requests, and bounds how many forecasts
// run at once so forecasting cannot starve the API. It is safe for
// concurrent use.
type Engine struct {
	slots chan struct{}
	now   func() time.Time
}

// NewEngine returns an engine that runs at most concurrency forecasts at
// once.
func NewEngine(concurrency int) *Engine {
	return &Engine{slots: make(chan struct{}, max(1, concurrency)), now: time.Now}
}

// Ready reports that the engine can serve: it has no dependencies.
func (engine *Engine) Ready(context.Context) error { return nil }

func (engine *Engine) acquire(ctx context.Context) (func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrBusy, err)
	}
	select {
	case engine.slots <- struct{}{}:
		return func() { <-engine.slots }, nil
	case <-ctx.Done():
		return nil, fmt.Errorf("%w: %v", ErrBusy, ctx.Err())
	}
}

func rejected(format string, values ...any) error {
	return fmt.Errorf("%w: "+format, append([]any{ErrRejected}, values...)...)
}

// run is one forecast: the chosen model's predictions and intervals and the
// evidence for that choice.
type run struct {
	model        string
	baseline     string
	predictions  []float64
	lower, upper []float64
	origins      int
	scores       []candidateScore
	calibration  *Calibration
	skill        *float64
	fallback     []string
	daily        int
	weekly       int
	clipped      int
}

type candidateScore struct {
	Model string   `json:"model"`
	MAE   float64  `json:"mae"`
	RMSE  float64  `json:"rmse"`
	WAPE  *float64 `json:"wape"`
}

// Forecast predicts the next request.Horizon values of the series.
func (engine *Engine) Forecast(ctx context.Context, request Request) (Response, error) {
	release, err := engine.acquire(ctx)
	if err != nil {
		return Response{}, err
	}
	defer release()
	level, interval, err := validateCommon(request.Horizon, request.IntervalSeconds, request.Resource, request.ConfidenceLevel)
	if err != nil {
		return Response{}, err
	}
	values, interpolated, dropped, err := prepare(request.Samples, request.Timestamps, interval)
	if err != nil {
		return Response{}, err
	}
	if request.Horizon > len(values) {
		return Response{}, rejected("the horizon of %d steps is longer than the %d steps of usable history; KubeRight does not forecast further ahead than it has observed", request.Horizon, len(values))
	}
	result := forecastSeries(values, request.Horizon, interval, level)
	now := engine.now().UTC()
	response := Response{
		Predictions: result.predictions, ConfidenceIntervals: make([]Interval, request.Horizon),
		Model: result.model, ModelVersion: EngineVersion, FallbackUsed: len(result.fallback) > 0, FallbackReasons: result.fallback,
		Confidence: confidence(result, level), GeneratedAt: now, Calibration: result.calibration,
		Warning:       "Forecasts are advisory; the model and its accuracy were chosen and measured on this series' own history.",
		ModelMetadata: &ModelMetadata{Name: result.model, Version: EngineVersion, FeatureSchema: "univariate", TrainedAt: now, TrainingSamples: len(values)},
	}
	if response.FallbackReasons == nil {
		response.FallbackReasons = []string{}
	}
	for index := range response.ConfidenceIntervals {
		response.ConfidenceIntervals[index] = Interval{Lower: result.lower[index], Upper: result.upper[index]}
	}
	response.Validation = mustJSON(map[string]any{
		"method": "rolling-origin", "origins": result.origins, "horizon": request.Horizon,
		"selectedModel": result.model, "baselineModel": result.baseline, "skill": result.skill, "candidates": result.scores,
	})
	response.Seasonality = mustJSON(map[string]any{"dailyPeriod": result.daily, "weeklyPeriod": result.weekly})
	response.Anomalies = mustJSON(map[string]any{"method": fmt.Sprintf("training values beyond the median ± %.0f robust deviations are clipped for fitting; scores use the raw values", winsorizeMADs), "clipped": result.clipped})
	response.DataQuality = mustJSON(map[string]any{"samples": len(values), "interpolated": interpolated, "dropped": dropped})
	return response, nil
}

// Backtest replays the series from past origins: each forecast is made from
// the history before its origin and scored against what followed, next to
// a seasonal-naive baseline.
func (engine *Engine) Backtest(ctx context.Context, request BacktestRequest) (BacktestResponse, error) {
	release, err := engine.acquire(ctx)
	if err != nil {
		return BacktestResponse{}, err
	}
	defer release()
	level, interval, err := validateCommon(request.Horizon, request.IntervalSeconds, request.Resource, request.ConfidenceLevel)
	if err != nil {
		return BacktestResponse{}, err
	}
	if request.Origins < 1 || request.Origins > maximumOrigins {
		return BacktestResponse{}, rejected("origins must be between 1 and %d", maximumOrigins)
	}
	values, _, _, err := prepare(request.Samples, nil, interval)
	if err != nil {
		return BacktestResponse{}, err
	}
	if len(values)-request.Origins*request.Horizon < minimumHistory {
		return BacktestResponse{}, rejected("a backtest of %d origins of %d steps needs at least %d samples", request.Origins, request.Horizon, minimumHistory+request.Origins*request.Horizon)
	}
	response := BacktestResponse{Method: "rolling-origin", Horizon: request.Horizon, NominalCoverage: level, GeneratedAt: engine.now().UTC(),
		Warning: "Each origin is forecast only from the history before it, with the same model selection as live forecasts."}
	var actual, predicted, lower, upper, baseline []float64
	daily, weekly := period(24*time.Hour, interval), period(7*24*time.Hour, interval)
	for origin := request.Origins; origin >= 1; origin-- {
		if err := ctx.Err(); err != nil {
			return BacktestResponse{}, fmt.Errorf("%w: %v", ErrBusy, err)
		}
		cut := len(values) - origin*request.Horizon
		train, future := values[:cut], values[cut:cut+request.Horizon]
		result := forecastSeries(train, request.Horizon, interval, level)
		reference := baselineFor(train, daily, weekly)(request.Horizon)
		response.Origins = append(response.Origins, BacktestOrigin{
			OriginIndex: cut, Model: result.model, Calibrated: result.calibration != nil && result.calibration.Calibrated,
			Score: score(future, result.predictions, result.lower, result.upper, reference),
		})
		actual, predicted = append(actual, future...), append(predicted, result.predictions...)
		lower, upper, baseline = append(lower, result.lower...), append(upper, result.upper...), append(baseline, reference...)
		if origin == 1 {
			for index := range future {
				response.Points = append(response.Points, BacktestPoint{
					Index: cut + index, Actual: future[index], Predicted: result.predictions[index],
					Lower: result.lower[index], Upper: result.upper[index], Baseline: reference[index],
				})
			}
		}
	}
	response.Overall = score(actual, predicted, lower, upper, baseline)
	return response, nil
}

func validateCommon(horizon, intervalSeconds int, resource string, level float64) (float64, time.Duration, error) {
	switch {
	case horizon < 1 || horizon > maximumHorizon:
		return 0, 0, rejected("horizon must be between 1 and %d steps", maximumHorizon)
	case intervalSeconds < 1:
		return 0, 0, rejected("the sampling interval must be at least one second")
	case resource != "cpu" && resource != "memory":
		return 0, 0, rejected("resource must be cpu or memory")
	case !(level > 0.5 && level < 1):
		return 0, 0, rejected("the confidence level must be between 0.5 and 1")
	}
	return level, time.Duration(intervalSeconds) * time.Second, nil
}

// prepare puts the samples on a regular grid. Gaps of up to
// interpolateSteps are interpolated; before a longer gap, history is
// dropped, because a forecast must not run across missing data it would
// have to invent.
func prepare(samples []float64, timestamps []time.Time, interval time.Duration) (values []float64, interpolated, dropped int, err error) {
	if len(samples) < minimumHistory {
		return nil, 0, 0, rejected("at least %d samples are required, got %d", minimumHistory, len(samples))
	}
	if len(samples) > maximumHistory {
		return nil, 0, 0, rejected("at most %d samples are accepted", maximumHistory)
	}
	for _, value := range samples {
		if math.IsNaN(value) || math.IsInf(value, 0) || value < 0 {
			return nil, 0, 0, rejected("samples must be finite and not negative")
		}
	}
	if len(timestamps) == 0 {
		return append([]float64(nil), samples...), 0, 0, nil
	}
	if len(timestamps) != len(samples) {
		return nil, 0, 0, rejected("timestamps and samples differ in length")
	}
	first := timestamps[0]
	span := timestamps[len(timestamps)-1].Sub(first)
	if span < 0 || span/interval > 2*maximumHistory {
		return nil, 0, 0, rejected("timestamps must be ascending and span at most %d intervals", 2*maximumHistory)
	}
	grid := make([]float64, int(math.Round(float64(span)/float64(interval)))+1)
	for index := range grid {
		grid[index] = math.NaN()
	}
	for index, at := range timestamps {
		if index > 0 && !at.After(timestamps[index-1]) {
			return nil, 0, 0, rejected("timestamps must be strictly ascending")
		}
		slot := int(math.Round(float64(at.Sub(first)) / float64(interval)))
		// Two samples in one slot keep the larger: these series are peaks.
		if math.IsNaN(grid[slot]) || samples[index] > grid[slot] {
			grid[slot] = samples[index]
		}
	}
	start := 0
	for index := 0; index < len(grid); {
		if !math.IsNaN(grid[index]) {
			index++
			continue
		}
		end := index
		for end < len(grid) && math.IsNaN(grid[end]) {
			end++
		}
		if end-index > interpolateSteps {
			start = end
		} else {
			before, after := grid[index-1], grid[end]
			for slot := index; slot < end; slot++ {
				fraction := float64(slot-index+1) / float64(end-index+1)
				grid[slot] = before + fraction*(after-before)
				interpolated++
			}
		}
		index = end
	}
	values = grid[start:]
	if len(values) < minimumHistory {
		return nil, 0, 0, rejected("only %d contiguous samples remain after the last gap longer than %d intervals; at least %d are required", len(values), interpolateSteps, minimumHistory)
	}
	return values, interpolated, start, nil
}

// period is the number of samples in one season, or 0 when the season is
// not a whole number of at least two intervals.
func period(season, interval time.Duration) int {
	if interval <= 0 || season%interval != 0 || season/interval < 2 {
		return 0
	}
	return int(season / interval)
}

// season is the shortest seasonal cycle the sampling interval can show: a
// day for sub-daily samples, a week for daily ones, or none.
func season(daily, weekly int) (name string, length int) {
	switch {
	case daily >= 2:
		return "daily", daily
	case weekly >= 2:
		return "weekly", weekly
	}
	return "", 0
}

// baselineName is the seasonal-naive baseline every model must beat, or
// naive when the history holds fewer than two seasons.
func baselineName(n, daily, weekly int) string {
	if name, length := season(daily, weekly); length >= 2 && n >= 2*length {
		return "seasonal_naive_" + name
	}
	return "naive"
}

func baselineFor(train []float64, daily, weekly int) predictor {
	if _, length := season(daily, weekly); baselineName(len(train), daily, weekly) != "naive" {
		return seasonalNaive(length)(train)
	}
	return fitNaive(train)
}

// winsorize clips values far from the median so one spike or dropout does
// not dominate a fit. It returns the clipped copy and how many changed.
func winsorize(values []float64) ([]float64, int) {
	center := median(values)
	deviations := make([]float64, len(values))
	for index, value := range values {
		deviations[index] = math.Abs(value - center)
	}
	spread := 1.4826 * median(deviations)
	out := append([]float64(nil), values...)
	if spread == 0 {
		return out, 0
	}
	low, high, clipped := center-winsorizeMADs*spread, center+winsorizeMADs*spread, 0
	for index, value := range out {
		if value < low || value > high {
			out[index] = math.Min(math.Max(value, low), high)
			clipped++
		}
	}
	return out, clipped
}

// forecastSeries selects a model by rolling-origin validation and forecasts
// with it. Validation scores always use the raw values.
func forecastSeries(values []float64, h int, interval time.Duration, level float64) run {
	result := run{daily: period(24*time.Hour, interval), weekly: period(7*24*time.Hour, interval)}
	fitValues, clipped := winsorize(values)
	result.clipped = clipped
	n := len(values)
	result.origins = min(validationOrigins, (n-minimumTraining)/h)
	if result.origins < 1 {
		return fallbackForecast(result, values, fitValues, h, level)
	}
	oldest := n - result.origins*h
	result.baseline = baselineName(oldest, result.daily, result.weekly)
	type evaluated struct {
		model  model
		errors [][]float64
		score  candidateScore
	}
	var best, baseline *evaluated
	for _, candidate := range candidates(result.daily, result.weekly) {
		if candidate.minHistory > oldest {
			continue
		}
		item := &evaluated{model: candidate}
		var actual, predicted []float64
		for origin := 1; origin <= result.origins; origin++ {
			cut := n - origin*h
			prediction := candidate.fit(fitValues[:cut])(h)
			errs := make([]float64, h)
			for step := range errs {
				errs[step] = values[cut+step] - prediction[step]
			}
			item.errors = append(item.errors, errs)
			actual, predicted = append(actual, values[cut:cut+h]...), append(predicted, prediction...)
		}
		s := score(actual, predicted, predicted, predicted, predicted)
		item.score = candidateScore{Model: candidate.name, MAE: s.MAE, RMSE: s.RMSE, WAPE: s.WAPE}
		result.scores = append(result.scores, item.score)
		if candidate.name == result.baseline {
			baseline = item
		}
		if best == nil || item.score.MAE < best.score.MAE {
			best = item
		}
	}
	result.model = best.model.name
	result.predictions = clampZero(best.model.fit(fitValues)(h))
	if baseline != nil && baseline.score.MAE > 0 {
		skill := round(1-best.score.MAE/baseline.score.MAE, 6)
		result.skill = &skill
	}
	// The chosen model is replayed from older origins as well, so the
	// interval quantiles rest on more errors than selection needed.
	errorsByOrigin := best.errors
	for origin := result.origins + 1; origin <= intervalOrigins; origin++ {
		cut := n - origin*h
		if cut < max(minimumTraining, best.model.minHistory) {
			break
		}
		prediction := best.model.fit(fitValues[:cut])(h)
		errs := make([]float64, h)
		for step := range errs {
			errs[step] = values[cut+step] - prediction[step]
		}
		errorsByOrigin = append(errorsByOrigin, errs)
	}
	low, high := intervalOffsets(errorsByOrigin, h, level)
	result.lower, result.upper = applyOffsets(result.predictions, low, high)
	result.calibration = &Calibration{Method: fmt.Sprintf("conformal quantiles of the chosen model's errors from %d past origins", len(errorsByOrigin)), NominalCoverage: level, Factor: 1, EvaluationSamples: h}
	if len(errorsByOrigin) >= 2 {
		// Intervals from the older origins, scored on the newest one, which
		// they never saw.
		heldLow, heldHigh := intervalOffsets(errorsByOrigin[1:], h, level)
		cut := n - h
		prediction := clampZero(best.model.fit(fitValues[:cut])(h))
		lower, upper := applyOffsets(prediction, heldLow, heldHigh)
		coverage := round(score(values[cut:], prediction, lower, upper, prediction).Coverage, 6)
		result.calibration.HeldOutCoverage, result.calibration.CalibratedCoverage = &coverage, &coverage
		result.calibration.Calibrated = true
	}
	return result
}

// fallbackForecast serves a series too short to validate: a naive model
// with intervals from its own one-step errors, reported as uncalibrated.
func fallbackForecast(result run, values, fitValues []float64, h int, level float64) run {
	n := len(values)
	result.baseline = baselineName(n, result.daily, result.weekly)
	result.model = result.baseline
	result.fallback = []string{fmt.Sprintf("%d samples are too few to validate any model against a %d-step horizon; a %s forecast with unmeasured accuracy is shown", n, h, result.model)}
	result.predictions = clampZero(baselineFor(fitValues, result.daily, result.weekly)(h))
	lag := 1
	if _, length := season(result.daily, result.weekly); result.model != "naive" {
		lag = length
	}
	differences := make([]float64, 0, n-lag)
	for index := lag; index < n; index++ {
		differences = append(differences, math.Abs(values[index]-values[index-lag]))
	}
	spread := quantile(differences, level)
	result.lower, result.upper = make([]float64, h), make([]float64, h)
	for step := range h {
		width := spread * math.Sqrt(float64(step/lag+1))
		result.lower[step] = math.Max(0, result.predictions[step]-width)
		result.upper[step] = result.predictions[step] + width
	}
	result.calibration = &Calibration{Method: "one-step differences, not validated", NominalCoverage: level, Factor: 1}
	return result
}

// intervalOffsets turns past forecast errors into lower and upper offsets
// for each step. Steps are grouped in up to four ranges so each quantile
// rests on several errors, and the interval never narrows further ahead.
// The quantiles are conformal: order statistics at ranks that, for
// exchangeable errors, cover at least the nominal share, instead of
// interpolated quantiles that cover less when errors are few.
func intervalOffsets(errorsByOrigin [][]float64, h int, level float64) (low, high []float64) {
	buckets := min(4, h)
	low, high = make([]float64, h), make([]float64, h)
	widestLow, widestHigh := 0.0, 0.0
	for bucket := range buckets {
		from, to := bucket*h/buckets, (bucket+1)*h/buckets
		var errs []float64
		for _, origin := range errorsByOrigin {
			errs = append(errs, origin[from:to]...)
		}
		lowTail, highTail := conformalBounds(errs, level)
		widestLow = math.Min(widestLow, math.Min(0, lowTail))
		widestHigh = math.Max(widestHigh, math.Max(0, highTail))
		for step := from; step < to; step++ {
			low[step], high[step] = widestLow, widestHigh
		}
	}
	return low, high
}

func applyOffsets(predictions, low, high []float64) (lower, upper []float64) {
	lower, upper = make([]float64, len(predictions)), make([]float64, len(predictions))
	for index, value := range predictions {
		lower[index] = math.Max(0, value+low[index])
		upper[index] = value + high[index]
	}
	return lower, upper
}

func clampZero(values []float64) []float64 {
	for index, value := range values {
		values[index] = math.Max(0, value)
	}
	return values
}

// confidence summarizes, from 0 to 1, how much the forecast has earned:
// half for existing, the rest for beating the seasonal-naive baseline,
// scaled by how well its intervals covered held-out data.
func confidence(result run, level float64) float64 {
	if len(result.fallback) > 0 {
		return 0.2
	}
	skill := 0.0
	if result.skill != nil {
		skill = math.Max(0, math.Min(1, *result.skill))
	}
	value := 0.5 + 0.5*skill
	if calibration := result.calibration; calibration != nil && calibration.HeldOutCoverage != nil {
		value *= math.Min(1, *calibration.HeldOutCoverage/level)
	} else {
		value *= 0.6
	}
	return round(value, 2)
}

// score compares a forecast with what happened. Like the dashboard's
// backtest view expects, sMAPE, MAPE, WAPE, and coverage are fractions, and
// the interval width is relative to the mean actual value.
func score(actual, predicted, lower, upper, baseline []float64) Score {
	n := float64(len(actual))
	var absolute, squared, smape, absoluteActual, baselineAbsolute, width, inside float64
	var mape float64
	nonzero := 0
	for index, value := range actual {
		err := value - predicted[index]
		absolute += math.Abs(err)
		squared += err * err
		if denominator := math.Abs(value) + math.Abs(predicted[index]); denominator > 0 {
			smape += 2 * math.Abs(err) / denominator
		}
		if math.Abs(value) > 1e-12 {
			mape += math.Abs(err) / math.Abs(value)
			nonzero++
		}
		absoluteActual += math.Abs(value)
		baselineAbsolute += math.Abs(value - baseline[index])
		width += upper[index] - lower[index]
		if lower[index] <= value && value <= upper[index] {
			inside++
		}
	}
	result := Score{
		Samples: len(actual), MAE: round(absolute/n, 9), RMSE: round(math.Sqrt(squared/n), 9), SMAPE: round(smape/n, 6),
		Coverage: round(inside/n, 6), MeanIntervalWidth: round(width/n/math.Max(absoluteActual/n, 1e-12), 6), BaselineMAE: round(baselineAbsolute/n, 9),
	}
	if nonzero > 0 {
		value := round(mape/float64(nonzero), 6)
		result.MAPE = &value
	}
	if absoluteActual > 0 {
		value := round(absolute/absoluteActual, 6)
		result.WAPE = &value
	}
	if baselineAbsolute/n > 1e-12 {
		value := round(1-absolute/baselineAbsolute, 6)
		result.Skill = &value
	}
	return result
}

func round(value float64, digits int) float64 {
	scale := math.Pow(10, float64(digits))
	return math.Round(value*scale) / scale
}

func mustJSON(value any) json.RawMessage {
	encoded, err := json.Marshal(value)
	if err != nil {
		return nil
	}
	return encoded
}

// conformalBounds returns the order statistics that bound a two-sided
// interval at level: with n errors, the ceil((n+1)(1+level)/2)-th smallest
// above and the matching rank below. With too few errors for that rank it
// returns the extremes.
func conformalBounds(errs []float64, level float64) (low, high float64) {
	sorted := append([]float64(nil), errs...)
	sort.Float64s(sorted)
	n := len(sorted)
	rank := int(math.Ceil(float64(n+1) * (1 + level) / 2))
	if rank > n {
		return sorted[0], sorted[n-1]
	}
	return sorted[n-rank], sorted[rank-1]
}
