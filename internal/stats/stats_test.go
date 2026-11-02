package stats

import (
	"math"
	"math/rand/v2"
	"testing"
)

func near(t *testing.T, label string, got, want, tolerance float64) {
	t.Helper()
	if math.Abs(got-want) > tolerance {
		t.Fatalf("%s = %.6f, want %.6f ± %g", label, got, want, tolerance)
	}
}

func TestDescriptiveStatisticsIgnoreMissingValues(t *testing.T) {
	values := []float64{1, 2, math.NaN(), 3, 4, math.Inf(1)}
	mean, count := Mean(values)
	near(t, "mean", mean, 2.5, 1e-12)
	if count != 4 {
		t.Fatalf("count = %d", count)
	}
	near(t, "stddev", StdDev(values), math.Sqrt(5.0/3.0), 1e-12)
	near(t, "cv", CoefficientOfVariation(values), math.Sqrt(5.0/3.0)/2.5, 1e-12)
	near(t, "p50", Quantile(values, 0.5), 2.5, 1e-12)
	near(t, "p100", Quantile(values, 1), 4, 1e-12)
	near(t, "mad", MedianAbsoluteDeviation([]float64{1, 1, 2, 2, 4, 6, 9}), 1, 1e-12)
	if Quantile(nil, 0.5) != 0 || MedianAbsoluteDeviation(nil) != 0 || CoefficientOfVariation([]float64{-1, -2}) != 0 {
		t.Fatal("empty or non-positive inputs must return zero")
	}
	if _, count := Mean([]float64{math.NaN()}); count != 0 || StdDev([]float64{1}) != 0 {
		t.Fatal("too few values must return zero")
	}
}

func TestPearsonAndSpearman(t *testing.T) {
	xs := []float64{1, 2, 3, 4, 5, 6, 7, 8}
	linear := []float64{3, 5, 7, 9, 11, 13, 15, 17}
	if r, n, ok := Pearson(xs, linear); !ok || n != 8 {
		t.Fatalf("pearson failed: %v %d", ok, n)
	} else {
		near(t, "r", r, 1, 1e-12)
	}
	// A monotonic but non-linear relationship is perfect for Spearman only.
	exponential := []float64{1, 2, 4, 8, 16, 32, 64, 1024}
	rho, _, _ := Spearman(xs, exponential)
	r, _, _ := Pearson(xs, exponential)
	near(t, "rho", rho, 1, 1e-12)
	if r >= 0.9 {
		t.Fatalf("pearson should be visibly below 1 for an exponential, got %.3f", r)
	}
	// Missing values are dropped pairwise.
	if _, n, _ := Pearson([]float64{1, math.NaN(), 3, 4}, []float64{1, 2, math.NaN(), 4}); n != 2 {
		t.Fatalf("pairwise n = %d", n)
	}
	if _, _, ok := Pearson([]float64{1, 1, 1}, []float64{1, 2, 3}); ok {
		t.Fatal("constant series has no correlation")
	}
	// Ties share average ranks.
	got := ranks([]float64{10, 20, 20, 30})
	want := []float64{1, 2.5, 2.5, 4}
	for index := range want {
		near(t, "rank", got[index], want[index], 1e-12)
	}
}

func TestSignificanceAccountsForSerialCorrelation(t *testing.T) {
	near(t, "independent", EffectiveSampleSize(1000, 0, 0), 1000, 1e-9)
	if effective := EffectiveSampleSize(1000, 0.9, 0.9); effective > 120 {
		t.Fatalf("strong autocorrelation must shrink n, got %.1f", effective)
	}
	if EffectiveSampleSize(1000, 1, 1) < 4 || EffectiveSampleSize(2, 0, 0) != 2 {
		t.Fatal("effective n bounds")
	}
	// Two independent sine waves of the same period correlate strongly, but
	// their regression residuals are smooth, so little evidence remains.
	random := rand.New(rand.NewPCG(9, 9))
	cycleA, cycleB, noisy := make([]float64, 576), make([]float64, 576), make([]float64, 576)
	for index := range cycleA {
		cycleA[index] = math.Sin(2*math.Pi*float64(index)/288) + random.NormFloat64()*0.01
		cycleB[index] = math.Sin(2*math.Pi*float64(index)/288+0.3) + random.NormFloat64()*0.01
		noisy[index] = cycleA[index]*2 + random.NormFloat64()*0.05
	}
	if effective := RegressionEffectiveSampleSize(cycleA, cycleB); effective > 20 {
		t.Fatalf("shared cycles must leave little evidence, effective n = %.1f", effective)
	}
	// A genuine relationship with independent noise keeps most of n.
	if effective := RegressionEffectiveSampleSize(cycleA, noisy); effective < 300 {
		t.Fatalf("genuine relationship lost its evidence, effective n = %.1f", effective)
	}
	if RegressionEffectiveSampleSize([]float64{1, 2}, []float64{1, 2}) != 2 {
		t.Fatal("tiny samples keep their size")
	}
	lower, upper := FisherInterval(0.5, 100)
	if lower >= 0.5 || upper <= 0.5 || lower < 0.3 || upper > 0.65 {
		t.Fatalf("interval [%.3f, %.3f]", lower, upper)
	}
	if lower, upper := FisherInterval(0.5, 3); lower != -1 || upper != 1 {
		t.Fatal("undefined interval must be [-1, 1]")
	}
	if p := PValue(0.5, 100); p > 1e-5 {
		t.Fatalf("p = %g", p)
	}
	if p := PValue(0.05, 30); p < 0.5 {
		t.Fatalf("weak correlation must not be significant, p = %g", p)
	}
	if PValue(0.9, 3) != 1 {
		t.Fatal("too few samples must give p = 1")
	}
}

func TestCrossCorrelationFindsLead(t *testing.T) {
	random := rand.New(rand.NewPCG(1, 2))
	driver := make([]float64, 300)
	for index := range driver {
		driver[index] = random.Float64()
	}
	// The response follows the driver three steps later.
	response := make([]float64, len(driver))
	for index := range response {
		if index >= 3 {
			response[index] = driver[index-3]*2 + random.Float64()*0.05
		}
	}
	best, ok := BestLag(CrossCorrelation(driver, response, 10))
	if !ok || best.Lag != 3 || best.R < 0.95 {
		t.Fatalf("best lag %+v", best)
	}
	reverse, _ := BestLag(CrossCorrelation(response, driver, 10))
	if reverse.Lag != -3 {
		t.Fatalf("reverse lag %+v", reverse)
	}
	if correlations := CrossCorrelation([]float64{1, 2}, []float64{1, 2}, -1); len(correlations) != 0 {
		t.Fatal("too short for any lag")
	}
	if _, ok := BestLag(nil); ok {
		t.Fatal("no lags")
	}
	// Equal correlations prefer the smaller lag.
	tie, _ := BestLag([]LagCorrelation{{Lag: -2, R: 0.8}, {Lag: 0, R: 0.8}, {Lag: 1, R: -0.8}})
	if tie.Lag != 0 {
		t.Fatalf("tie lag %d", tie.Lag)
	}
}

func TestAutocorrelationAndInterpolation(t *testing.T) {
	cycle := make([]float64, 96)
	for index := range cycle {
		cycle[index] = math.Sin(2 * math.Pi * float64(index) / 24)
	}
	near(t, "period ACF", Autocorrelation(cycle, 24), 1, 1e-9)
	if Autocorrelation(cycle, 0) != 0 || Autocorrelation(cycle, 200) != 0 {
		t.Fatal("invalid lags return zero")
	}
	filled := Interpolate([]float64{math.NaN(), 1, math.NaN(), math.NaN(), 4, math.NaN(), math.NaN(), math.NaN(), math.NaN(), 9}, 2)
	near(t, "gap 1", filled[2], 2, 1e-12)
	near(t, "gap 2", filled[3], 3, 1e-12)
	if !math.IsNaN(filled[0]) || !math.IsNaN(filled[6]) {
		t.Fatal("leading and long gaps stay missing")
	}
}

func TestChangePointsFindLevelShiftsNotSpikes(t *testing.T) {
	random := rand.New(rand.NewPCG(7, 11))
	values := make([]float64, 400)
	for index := range values {
		level := 100.0
		if index >= 250 {
			level = 160
		}
		values[index] = level + random.NormFloat64()*5
	}
	// Short spikes and a missing stretch must not create change points.
	for index := 50; index < 53; index++ {
		values[index] = 400
	}
	for index := 120; index < 130; index++ {
		values[index] = math.NaN()
	}
	points := ChangePoints(values, 12, 0.1)
	if len(points) != 1 {
		t.Fatalf("change points %+v", points)
	}
	if point := points[0]; point.Index < 245 || point.Index > 255 || point.RelativeChange < 0.5 || point.RelativeChange > 0.7 || point.Score < 5 {
		t.Fatalf("change point %+v", point)
	}
	if ChangePoints(values[:20], 12, 0.1) != nil {
		t.Fatal("series shorter than two segments has no change points")
	}
	flat := make([]float64, 100)
	for index := range flat {
		flat[index] = 5
	}
	if points := ChangePoints(flat, 10, 0.1); len(points) != 0 {
		t.Fatalf("constant series: %+v", points)
	}
	// A shift smaller than minRelative is ignored even if statistically clear.
	small := make([]float64, 200)
	for index := range small {
		small[index] = 100
		if index >= 100 {
			small[index] = 103
		}
		small[index] += random.NormFloat64() * 0.1
	}
	if points := ChangePoints(small, 10, 0.1); len(points) != 0 {
		t.Fatalf("immaterial shift: %+v", points)
	}
	if points := ChangePoints(small, 1, 0.01); len(points) == 0 {
		t.Fatal("a material threshold of 1% must keep the 3% shift")
	}
	// Stationary noise produces no change points.
	stationary := make([]float64, 600)
	for index := range stationary {
		stationary[index] = 100 + random.NormFloat64()*8
	}
	if points := ChangePoints(stationary, 12, 0.1); len(points) != 0 {
		t.Fatalf("stationary noise: %+v", points)
	}
}

func TestDetectSeasonality(t *testing.T) {
	random := rand.New(rand.NewPCG(3, 5))
	daily := make([]float64, 24*7)
	for index := range daily {
		daily[index] = 100 + 40*math.Sin(2*math.Pi*float64(index)/24) + random.NormFloat64()*3
	}
	found := DetectSeasonality(daily, []int{24, 168}, 0.5)
	if !found.Detected || found.PeriodSteps != 24 || found.Strength < 0.8 {
		t.Fatalf("seasonality %+v", found)
	}
	noise := make([]float64, 24*7)
	for index := range noise {
		noise[index] = random.Float64()
	}
	if found := DetectSeasonality(noise, []int{24}, 0.5); found.Detected {
		t.Fatalf("noise is not seasonal: %+v", found)
	}
	if found := DetectSeasonality(noise[:30], []int{24, 1}, 0.5); found.PeriodSteps != 0 {
		t.Fatal("fewer than two cycles cannot show seasonality")
	}
}
