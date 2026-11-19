package forecast

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"math/rand"
	"strings"
	"testing"
	"time"
)

const hour = 3600

// dailySeries is hourly CPU in cores: a daily cycle, an optional trend per
// hour, and Gaussian noise.
func dailySeries(seed int64, hours int, trend, noise float64) []float64 {
	random := rand.New(rand.NewSource(seed))
	values := make([]float64, hours)
	for index := range values {
		cycle := 0.4 * math.Sin(2*math.Pi*float64(index%24)/24)
		values[index] = math.Max(0, 1+cycle+trend*float64(index)+random.NormFloat64()*noise)
	}
	return values
}

func forecastOf(t *testing.T, samples []float64, horizon int) Response {
	t.Helper()
	response, err := NewEngine(2).Forecast(context.Background(), Request{Samples: samples, Horizon: horizon, IntervalSeconds: hour, Resource: "cpu", ConfidenceLevel: 0.9})
	if err != nil {
		t.Fatal(err)
	}
	if len(response.Predictions) != horizon || len(response.ConfidenceIntervals) != horizon || response.ModelVersion != EngineVersion {
		t.Fatalf("response shape: %d predictions, %d intervals, version %q", len(response.Predictions), len(response.ConfidenceIntervals), response.ModelVersion)
	}
	for index, prediction := range response.Predictions {
		interval := response.ConfidenceIntervals[index]
		if prediction < 0 || interval.Lower < 0 || interval.Lower > prediction || prediction > interval.Upper {
			t.Fatalf("step %d: prediction %v outside [%v, %v]", index, prediction, interval.Lower, interval.Upper)
		}
		if index > 0 && interval.Upper-prediction < response.ConfidenceIntervals[index-1].Upper-response.Predictions[index-1]-1e-9 {
			t.Fatalf("step %d: the upper interval narrowed further ahead", index)
		}
	}
	return response
}

func TestEngineFollowsADailyCycle(t *testing.T) {
	samples := dailySeries(1, 14*24, 0, 0.05)
	response := forecastOf(t, samples, 24)
	if !strings.Contains(response.Model, "seasonal") && !strings.Contains(response.Model, "holt_winters") {
		t.Fatalf("a daily cycle was forecast with %s", response.Model)
	}
	// The forecast keeps the shape of the cycle it was shown.
	for step, prediction := range response.Predictions {
		want := 1 + 0.4*math.Sin(2*math.Pi*float64((len(samples)+step)%24)/24)
		if math.Abs(prediction-want) > 0.15 {
			t.Fatalf("step %d: predicted %.3f, the cycle is at %.3f", step, prediction, want)
		}
	}
	calibration := response.Calibration
	if calibration == nil || !calibration.Calibrated || calibration.HeldOutCoverage == nil || *calibration.HeldOutCoverage < 0.6 {
		t.Fatalf("calibration = %+v", calibration)
	}
	var validation struct {
		Origins       int              `json:"origins"`
		SelectedModel string           `json:"selectedModel"`
		BaselineModel string           `json:"baselineModel"`
		Skill         *float64         `json:"skill"`
		Candidates    []candidateScore `json:"candidates"`
	}
	if err := json.Unmarshal(response.Validation, &validation); err != nil {
		t.Fatal(err)
	}
	if validation.Origins != 3 || validation.SelectedModel != response.Model || validation.BaselineModel != "seasonal_naive_daily" || validation.Skill == nil || len(validation.Candidates) < 5 {
		t.Fatalf("validation = %+v", validation)
	}
	if response.FallbackUsed || response.Confidence < 0.4 {
		t.Fatalf("fallback=%v confidence=%v", response.FallbackUsed, response.Confidence)
	}
}

func TestEngineFollowsATrend(t *testing.T) {
	random := rand.New(rand.NewSource(2))
	samples := make([]float64, 200)
	for index := range samples {
		samples[index] = 1 + 0.01*float64(index) + random.NormFloat64()*0.01
	}
	response := forecastOf(t, samples, 24)
	if response.Model != "damped_trend" && response.Model != "holt_winters_daily" {
		t.Fatalf("a steady trend was forecast with %s", response.Model)
	}
	if response.Predictions[23] <= samples[len(samples)-1] {
		t.Fatalf("the forecast does not continue the rise: last %.3f, step 24 %.3f", samples[len(samples)-1], response.Predictions[23])
	}
}

func TestEngineIsNotDrivenByASingleSpike(t *testing.T) {
	random := rand.New(rand.NewSource(3))
	samples := make([]float64, 120)
	for index := range samples {
		samples[index] = 1 + random.NormFloat64()*0.02
	}
	samples[100] = 100
	response := forecastOf(t, samples, 12)
	for step, prediction := range response.Predictions {
		if prediction > 1.5 {
			t.Fatalf("step %d: predicted %.2f after one spike", step, prediction)
		}
	}
	var anomalies struct {
		Clipped int `json:"clipped"`
	}
	if err := json.Unmarshal(response.Anomalies, &anomalies); err != nil || anomalies.Clipped < 1 {
		t.Fatalf("anomalies = %s err=%v", response.Anomalies, err)
	}
}

func TestEngineSaysWhenItCannotMeasureItsAccuracy(t *testing.T) {
	samples := dailySeries(4, 48, 0, 0.05)
	// One validation origin: validated against the baseline, but there is
	// no older origin to calibrate intervals that the newest one tests.
	response := forecastOf(t, samples, 24)
	if response.FallbackUsed || response.Calibration == nil || response.Calibration.Calibrated || response.Calibration.HeldOutCoverage != nil {
		t.Fatalf("one origin: fallback=%v calibration=%+v", response.FallbackUsed, response.Calibration)
	}
	// No origin at all: a naive forecast, labelled as such.
	response = forecastOf(t, samples, 30)
	if !response.FallbackUsed || len(response.FallbackReasons) != 1 || !strings.Contains(response.FallbackReasons[0], "unmeasured accuracy") || response.Confidence != 0.2 {
		t.Fatalf("fallback = %v %v confidence=%v", response.FallbackUsed, response.FallbackReasons, response.Confidence)
	}
	if response.Calibration == nil || response.Calibration.Calibrated {
		t.Fatalf("a fallback was reported as calibrated: %+v", response.Calibration)
	}
}

func TestEngineRejectsUnusableRequests(t *testing.T) {
	good := dailySeries(5, 96, 0, 0.05)
	nan := append([]float64(nil), good...)
	nan[10] = math.NaN()
	negative := append([]float64(nil), good...)
	negative[10] = -1
	start := time.Date(2026, 11, 19, 0, 0, 0, 0, time.UTC)
	stamps := func(n int) []time.Time {
		out := make([]time.Time, n)
		for index := range out {
			out[index] = start.Add(time.Duration(index) * time.Hour)
		}
		return out
	}
	descending := stamps(len(good))
	descending[5], descending[6] = descending[6], descending[5]
	for name, request := range map[string]Request{
		"too short":            {Samples: good[:47], Horizon: 6},
		"not a number":         {Samples: nan, Horizon: 6},
		"negative":             {Samples: negative, Horizon: 6},
		"horizon over history": {Samples: good, Horizon: 97},
		"horizon zero":         {Samples: good, Horizon: 0},
		"timestamps differ":    {Samples: good, Timestamps: stamps(10), Horizon: 6},
		"timestamps unordered": {Samples: good, Timestamps: descending, Horizon: 6},
	} {
		request.IntervalSeconds, request.Resource, request.ConfidenceLevel = hour, "cpu", 0.9
		if _, err := NewEngine(1).Forecast(context.Background(), request); !errors.Is(err, ErrRejected) {
			t.Fatalf("%s: error = %v", name, err)
		}
	}
	for name, request := range map[string]Request{
		"resource": {Samples: good, Horizon: 6, IntervalSeconds: hour, Resource: "disk", ConfidenceLevel: 0.9},
		"level":    {Samples: good, Horizon: 6, IntervalSeconds: hour, Resource: "cpu", ConfidenceLevel: 1},
		"interval": {Samples: good, Horizon: 6, IntervalSeconds: 0, Resource: "cpu", ConfidenceLevel: 0.9},
	} {
		if _, err := NewEngine(1).Forecast(context.Background(), request); !errors.Is(err, ErrRejected) {
			t.Fatalf("%s: error = %v", name, err)
		}
	}
}

func TestEngineFillsShortGapsAndDropsHistoryBeforeLongOnes(t *testing.T) {
	start := time.Date(2026, 11, 19, 0, 0, 0, 0, time.UTC)
	var samples []float64
	var stamps []time.Time
	for index := 0; index < 140; index++ {
		// A two-hour gap at 120-121 and an eight-hour gap at 20-27.
		if (index >= 20 && index < 28) || index == 120 || index == 121 {
			continue
		}
		samples = append(samples, 1)
		stamps = append(stamps, start.Add(time.Duration(index)*time.Hour))
	}
	values, interpolated, dropped, err := prepare(samples, stamps, time.Hour)
	if err != nil || interpolated != 2 || dropped != 28 || len(values) != 112 {
		t.Fatalf("values=%d interpolated=%d dropped=%d err=%v", len(values), interpolated, dropped, err)
	}
	// Too little remains after a long gap near the end.
	if _, _, _, err := prepare(samples[:len(samples)-100], stamps[:len(stamps)-100], time.Hour); !errors.Is(err, ErrRejected) {
		t.Fatalf("short remainder: %v", err)
	}
}

func TestEngineBoundsConcurrentForecasts(t *testing.T) {
	engine := NewEngine(1)
	release, err := engine.acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	_, err = engine.Forecast(ctx, Request{Samples: dailySeries(6, 96, 0, 0.05), Horizon: 6, IntervalSeconds: hour, Resource: "cpu", ConfidenceLevel: 0.9})
	if !errors.Is(err, ErrBusy) {
		t.Fatalf("a full engine accepted a forecast: %v", err)
	}
	release()
	if err := engine.Ready(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestBacktestScoresEachOriginAgainstWhatFollowed(t *testing.T) {
	samples := dailySeries(7, 14*24, 0, 0.05)
	response, err := NewEngine(1).Backtest(context.Background(), BacktestRequest{Samples: samples, Horizon: 24, Origins: 3, IntervalSeconds: hour, Resource: "cpu", ConfidenceLevel: 0.9})
	if err != nil {
		t.Fatal(err)
	}
	if len(response.Origins) != 3 || response.Overall.Samples != 72 || len(response.Points) != 24 || response.NominalCoverage != 0.9 {
		t.Fatalf("origins=%d samples=%d points=%d", len(response.Origins), response.Overall.Samples, len(response.Points))
	}
	for index, origin := range response.Origins {
		if want := len(samples) - (3-index)*24; origin.OriginIndex != want {
			t.Fatalf("origin %d starts at %d, want %d", index, origin.OriginIndex, want)
		}
	}
	for index, point := range response.Points {
		if point.Index != len(samples)-24+index || point.Actual != samples[point.Index] {
			t.Fatalf("point %d = %+v", index, point)
		}
	}
	overall := response.Overall
	if overall.Skill == nil || overall.WAPE == nil || overall.MAPE == nil || overall.Coverage < 0.5 || overall.MAE <= 0 {
		t.Fatalf("overall = %+v", overall)
	}
	if _, err := NewEngine(1).Backtest(context.Background(), BacktestRequest{Samples: samples[:100], Horizon: 24, Origins: 3, IntervalSeconds: hour, Resource: "cpu", ConfidenceLevel: 0.9}); !errors.Is(err, ErrRejected) {
		t.Fatalf("a backtest without enough history was accepted: %v", err)
	}
}

func TestScoreOfAPerfectForecast(t *testing.T) {
	actual := []float64{1, 2, 3, 4}
	result := score(actual, actual, actual, actual, []float64{2, 2, 2, 2})
	if result.MAE != 0 || result.RMSE != 0 || result.Coverage != 1 || *result.WAPE != 0 || *result.Skill != 1 || *result.MAPE != 0 {
		t.Fatalf("score = %+v", result)
	}
	zeros := []float64{0, 0}
	if result := score(zeros, zeros, zeros, zeros, zeros); result.WAPE != nil || result.MAPE != nil || result.Skill != nil {
		t.Fatalf("an all-zero series needs no ratio: %+v", result)
	}
}

// TestEngineBeatsSeasonalNaiveOutOfSample is the engine's accuracy evidence
// on synthetic data: over several noisy daily-cycle series, backtested
// forecasts have a lower error than the seasonal-naive baseline. Real
// clusters can differ, which is why every forecast reports its own
// validation.
func TestEngineBeatsSeasonalNaiveOutOfSample(t *testing.T) {
	var skills []float64
	for seed := int64(10); seed < 18; seed++ {
		samples := dailySeries(seed, 21*24, 0.0005, 0.08)
		response, err := NewEngine(1).Backtest(context.Background(), BacktestRequest{Samples: samples, Horizon: 24, Origins: 5, IntervalSeconds: hour, Resource: "cpu", ConfidenceLevel: 0.9})
		if err != nil {
			t.Fatal(err)
		}
		skills = append(skills, *response.Overall.Skill)
	}
	t.Logf("skill over seasonal naive per series: %v; mean %.3f", skills, mean(skills))
	if average := mean(skills); average <= 0.1 {
		t.Fatalf("mean skill over seasonal naive = %.3f (%v)", average, skills)
	}
}

func TestConformalBoundsUseCoveringRanks(t *testing.T) {
	ascending := func(n int) []float64 {
		out := make([]float64, n)
		for index := range out {
			out[index] = float64(index)
		}
		return out
	}
	// 99 errors at 90%: rank ceil(100 * 0.95) = 95, so the 5th and 95th
	// smallest, not interpolated values between them.
	if low, high := conformalBounds(ascending(99), 0.9); low != 4 || high != 94 {
		t.Fatalf("99 errors: [%v, %v]", low, high)
	}
	// Too few errors for that rank: the extremes.
	if low, high := conformalBounds(ascending(10), 0.9); low != 0 || high != 9 {
		t.Fatalf("10 errors: [%v, %v]", low, high)
	}
}

// TestIntervalsCoverHeavyTailedNoiseOutOfSample guards the defect the
// Bitbrains backtest found: quantiles from a few dozen errors made 90%
// intervals cover about 77% of held-out hours.
func TestIntervalsCoverHeavyTailedNoiseOutOfSample(t *testing.T) {
	var coverages []float64
	for seed := int64(30); seed < 40; seed++ {
		random := rand.New(rand.NewSource(seed))
		samples := make([]float64, 28*24)
		for index := range samples {
			// Laplace noise: heavier tails than the Gaussian.
			u := random.Float64() - 0.5
			noise := -0.15 * math.Copysign(math.Log(1-2*math.Abs(u)), u)
			samples[index] = math.Max(0, 1+0.4*math.Sin(2*math.Pi*float64(index%24)/24)+noise)
		}
		response, err := NewEngine(1).Backtest(context.Background(), BacktestRequest{Samples: samples, Horizon: 24, Origins: 5, IntervalSeconds: hour, Resource: "cpu", ConfidenceLevel: 0.9})
		if err != nil {
			t.Fatal(err)
		}
		coverages = append(coverages, response.Overall.Coverage)
	}
	t.Logf("held-out coverage of 90%% intervals per series: %v; mean %.3f", coverages, mean(coverages))
	if average := mean(coverages); average < 0.89 {
		t.Fatalf("mean coverage %.3f is well below the nominal 0.9", average)
	}
}

func TestEngineUsesTheWeekForDailySamples(t *testing.T) {
	random := rand.New(rand.NewSource(50))
	samples := make([]float64, 140)
	for index := range samples {
		weekday := 1.0
		if index%7 >= 5 {
			weekday = 0.4 // weekends are quiet
		}
		samples[index] = weekday + 0.003*float64(index) + random.NormFloat64()*0.03
	}
	response, err := NewEngine(1).Forecast(context.Background(), Request{Samples: samples, Horizon: 30, IntervalSeconds: 24 * hour, Resource: "cpu", ConfidenceLevel: 0.9})
	if err != nil {
		t.Fatal(err)
	}
	var validation struct {
		BaselineModel string `json:"baselineModel"`
	}
	if err := json.Unmarshal(response.Validation, &validation); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(response.Model, "weekly") || validation.BaselineModel != "seasonal_naive_weekly" {
		t.Fatalf("daily samples: model=%s baseline=%s", response.Model, validation.BaselineModel)
	}
	// The forecast keeps the weekly shape: weekends stay below weekdays.
	weekend, weekday := math.Inf(-1), math.Inf(1)
	for step, prediction := range response.Predictions {
		if (len(samples)+step)%7 >= 5 {
			weekend = math.Max(weekend, prediction)
		} else {
			weekday = math.Min(weekday, prediction)
		}
	}
	if weekend >= weekday {
		t.Fatalf("weekends (max %.2f) are not below weekdays (min %.2f)", weekend, weekday)
	}
	// Ninety days ahead needs more history than this.
	if _, err := NewEngine(1).Forecast(context.Background(), Request{Samples: samples[:80], Horizon: 90, IntervalSeconds: 24 * hour, Resource: "cpu", ConfidenceLevel: 0.9}); !errors.Is(err, ErrRejected) {
		t.Fatalf("a 90-day horizon on 80 days of history was accepted: %v", err)
	}
}
