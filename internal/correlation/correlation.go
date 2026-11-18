// Package correlation aligns a container's resource, scaling, traffic, and
// latency series on one time grid and measures how they move together:
// correlation strength and direction with confidence intervals adjusted for
// serial correlation, lead/lag, sustained level shifts, and the change around
// each rollout, scaling, restart, and OOM event.
//
// The results describe statistical association only. They never establish
// that one signal causes another, and every report says so.
package correlation

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/kuberight/kuberight/internal/stats"
	"github.com/kuberight/kuberight/internal/usage"
)

// Series names on the grid.
const (
	CPU           = "cpu"
	CPUPerReplica = "cpuPerReplica"
	Memory        = "memory"
	Throttling    = "throttling"
	Replicas      = "replicas"
	Latency       = "latency"
	RequestRate   = "requestRate"
	NodePressure  = "nodePressure"
)

// Event kinds derived from the replica series.
const (
	EventScaleUp   = "scale_up"
	EventScaleDown = "scale_down"
)

// Causation is attached to every report.
const Causation = "Correlation measures association only. A strong or leading correlation does not show that one signal causes the other; confirm with the change history before acting."

// pairsToCompare lists the relationships worth checking, in display order.
var pairsToCompare = [][2]string{
	{CPU, Throttling},
	{CPU, Replicas},
	{CPUPerReplica, Replicas},
	{RequestRate, CPU},
	{RequestRate, Replicas},
	{CPU, Latency},
	{Throttling, Latency},
	{Memory, Latency},
	{RequestRate, Latency},
	{NodePressure, Throttling},
	{NodePressure, Latency},
	{CPU, Memory},
}

// Input is the data for one container.
type Input struct {
	Start  time.Time
	End    time.Time
	Step   time.Duration
	Series map[string]*usage.Series
	Events []usage.Event
	// MaxLag bounds the lead/lag search. Zero selects one hour.
	MaxLag time.Duration
}

// Grid is every series on shared, evenly spaced timestamps. Missing
// observations are NaN, never zero.
type Grid struct {
	Timestamps []time.Time
	Values     map[string][]float64
}

// Align places each series on the grid [start, end] with the given step.
// Points are assigned to the nearest grid position; when two fall on the
// same position the later one wins.
func Align(start, end time.Time, step time.Duration, series map[string]*usage.Series) Grid {
	if step <= 0 || !end.After(start) {
		return Grid{Values: map[string][]float64{}}
	}
	count := int(end.Sub(start)/step) + 1
	grid := Grid{Timestamps: make([]time.Time, count), Values: map[string][]float64{}}
	for index := range count {
		grid.Timestamps[index] = start.Add(time.Duration(index) * step).UTC()
	}
	for name, values := range series {
		if values == nil {
			continue
		}
		column := make([]float64, count)
		for index := range column {
			column[index] = math.NaN()
		}
		for _, point := range values.Points {
			position := int(math.Round(float64(point.Timestamp.Sub(start)) / float64(step)))
			if position >= 0 && position < count && !math.IsNaN(point.Value) && !math.IsInf(point.Value, 0) {
				column[position] = point.Value
			}
		}
		grid.Values[name] = column
	}
	return grid
}

// Coverage is the share of grid positions with an observation.
func (grid Grid) Coverage(name string) float64 {
	column, ok := grid.Values[name]
	if !ok || len(column) == 0 {
		return 0
	}
	present := 0
	for _, value := range column {
		if !math.IsNaN(value) {
			present++
		}
	}
	return float64(present) / float64(len(column))
}

// Pair is the relationship between two series.
type Pair struct {
	X string `json:"x"`
	Y string `json:"y"`
	// R is the Pearson correlation at lag zero; Spearman is the rank
	// correlation, which is robust to spikes and skew.
	R        float64 `json:"r"`
	Spearman float64 `json:"spearman"`
	N        int     `json:"n"`
	// EffectiveN, Lower, Upper (the 95% confidence interval), PValue,
	// Significant, Strength, and Direction describe the strongest alignment:
	// BestLagR at BestLagSteps, which is lag zero unless a shift is clearly
	// stronger.
	EffectiveN  float64 `json:"effectiveN"`
	Lower       float64 `json:"lower"`
	Upper       float64 `json:"upper"`
	PValue      float64 `json:"pValue"`
	Significant bool    `json:"significant"`
	Strength    string  `json:"strength"`
	Direction   string  `json:"direction"`
	// BestLagSteps is the shift with the largest absolute correlation; a
	// positive lag means X tends to change before Y.
	BestLagSteps   int     `json:"bestLagSteps"`
	BestLagSeconds int64   `json:"bestLagSeconds"`
	BestLagR       float64 `json:"bestLagR"`
	Leads          string  `json:"leads,omitempty"`
	Summary        string  `json:"summary"`
}

// SeriesChange is a sustained level shift, with the nearest event if one
// happened close to it.
type SeriesChange struct {
	Series         string       `json:"series"`
	Timestamp      time.Time    `json:"timestamp"`
	MeanBefore     float64      `json:"meanBefore"`
	MeanAfter      float64      `json:"meanAfter"`
	RelativeChange float64      `json:"relativeChange"`
	Score          float64      `json:"score"`
	NearestEvent   *usage.Event `json:"nearestEvent,omitempty"`
}

// EventImpact compares a series before and after an event.
type EventImpact struct {
	Event          usage.Event `json:"event"`
	Series         string      `json:"series"`
	Before         float64     `json:"before"`
	After          float64     `json:"after"`
	BeforeSamples  int         `json:"beforeSamples"`
	AfterSamples   int         `json:"afterSamples"`
	RelativeChange float64     `json:"relativeChange"`
	Material       bool        `json:"material"`
}

// Finding is a plain-language observation backed by the measurements above.
type Finding struct {
	Severity string `json:"severity"` // info, warning, or critical
	Title    string `json:"title"`
	Detail   string `json:"detail"`
	Evidence string `json:"evidence"`
}

// Report is the full correlation analysis.
type Report struct {
	Start       time.Time `json:"start"`
	End         time.Time `json:"end"`
	StepSeconds int64     `json:"stepSeconds"`
	Points      int       `json:"points"`
	// CorrelationStart is where the pairwise correlations begin: the start of
	// the window, or the latest rollout when enough samples follow it.
	CorrelationStart time.Time                    `json:"correlationStart"`
	Coverage         map[string]float64           `json:"coverage"`
	Pairs            []Pair                       `json:"pairs"`
	ChangePoints     []SeriesChange               `json:"changePoints"`
	Events           []usage.Event                `json:"events"`
	EventImpacts     []EventImpact                `json:"eventImpacts"`
	Seasonality      map[string]stats.Seasonality `json:"seasonality"`
	Findings         []Finding                    `json:"findings"`
	Caveats          []string                     `json:"caveats"`
}

const (
	minimumPairs        = 12
	minimumCoverage     = 0.2
	significanceLevel   = 0.01
	leadMargin          = 0.05
	materialEventChange = 0.25
	changeMinRelative   = 0.2
)

// Analyze correlates every available pair and explains level shifts and
// events. It never fails: series that are missing or too sparse are skipped
// and named in the caveats, so the analysis stays useful when optional
// application metrics are absent.
func Analyze(input Input) Report {
	step := input.Step
	report := Report{
		Start: input.Start.UTC(), End: input.End.UTC(), StepSeconds: int64(step / time.Second),
		Coverage: map[string]float64{}, Pairs: []Pair{}, ChangePoints: []SeriesChange{}, Events: []usage.Event{},
		EventImpacts: []EventImpact{}, Seasonality: map[string]stats.Seasonality{}, Findings: []Finding{},
		Caveats: []string{Causation},
	}
	series := map[string]*usage.Series{}
	for name, values := range input.Series {
		if values != nil {
			series[name] = values
		}
	}
	grid := Align(input.Start, input.End, step, series)
	report.Points = len(grid.Timestamps)
	if report.Points == 0 {
		report.Caveats = append(report.Caveats, "the analysis window is empty")
		return report
	}
	// Short scrape gaps are bridged so lag and autocorrelation stay defined.
	for name, column := range grid.Values {
		grid.Values[name] = stats.Interpolate(column, 2)
	}
	if _, ok := grid.Values[CPUPerReplica]; !ok {
		if perReplica := normalizePerReplica(grid); perReplica != nil {
			grid.Values[CPUPerReplica] = perReplica
		}
	}
	names := make([]string, 0, len(grid.Values))
	for name := range grid.Values {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		coverage := grid.Coverage(name)
		report.Coverage[name] = math.Round(coverage*1000) / 1000
		if coverage < minimumCoverage {
			report.Caveats = append(report.Caveats, fmt.Sprintf("%s covers only %.0f%% of the window and is not correlated", name, coverage*100))
		}
	}
	for _, optional := range []string{Latency, RequestRate, NodePressure} {
		if _, ok := grid.Values[optional]; !ok {
			report.Caveats = append(report.Caveats, optional+" is not available, so relationships involving it are not shown")
		}
	}

	maxLag := input.MaxLag
	if maxLag <= 0 {
		maxLag = time.Hour
	}
	lagSteps := max(1, int(maxLag/step))
	report.Events = append(report.Events, input.Events...)
	report.Events = append(report.Events, scalingEvents(grid)...)
	sort.SliceStable(report.Events, func(left, right int) bool {
		return report.Events[left].Timestamp.Before(report.Events[right].Timestamp)
	})

	regime := grid
	if from := regimeStart(grid, report.Events); from > 0 {
		regime = grid.slice(from)
		report.Caveats = append(report.Caveats, fmt.Sprintf("correlations use the %d samples after the rollout at %s, because a rollout can change how signals relate", len(regime.Timestamps), grid.Timestamps[from].Format(time.RFC3339)))
	}
	report.CorrelationStart = regime.Timestamps[0]
	for _, pair := range pairsToCompare {
		if result, ok := correlate(regime, pair[0], pair[1], lagSteps, step); ok {
			report.Pairs = append(report.Pairs, result)
		}
	}

	report.ChangePoints = changePoints(grid, step, report.Events)
	report.EventImpacts = eventImpacts(grid, step, report.Events)
	for _, name := range []string{CPU, Memory, RequestRate} {
		if column, ok := grid.Values[name]; ok {
			report.Seasonality[name] = stats.DetectSeasonality(column, seasonalPeriods(step), 0.5)
		}
	}
	report.Findings = findings(report)
	return report
}

// normalizePerReplica derives per-replica CPU from the workload total when
// both series exist, so scaling-driven changes in the total are separated
// from changes in per-pod demand.
func normalizePerReplica(grid Grid) []float64 {
	total, okTotal := grid.Values["cpuTotal"]
	replicas, okReplicas := grid.Values[Replicas]
	if !okTotal || !okReplicas {
		return nil
	}
	result := make([]float64, len(total))
	for index := range total {
		if replicas[index] >= 1 && !math.IsNaN(total[index]) {
			result[index] = total[index] / replicas[index]
		} else {
			result[index] = math.NaN()
		}
	}
	return result
}

func correlate(grid Grid, x, y string, lagSteps int, step time.Duration) (Pair, bool) {
	xs, okX := grid.Values[x]
	ys, okY := grid.Values[y]
	if !okX || !okY || grid.Coverage(x) < minimumCoverage || grid.Coverage(y) < minimumCoverage {
		return Pair{}, false
	}
	r, n, ok := stats.Pearson(xs, ys)
	if !ok || n < minimumPairs {
		return Pair{}, false
	}
	rho, _, _ := stats.Spearman(xs, ys)
	pair := Pair{X: x, Y: y, R: round(r, 3), Spearman: round(rho, 3), N: n}
	// The association is judged at the strongest lag, which is lag zero
	// unless a shifted alignment is clearly stronger.
	tested, testedX, testedY := r, xs, ys
	if best, found := stats.BestLag(stats.CrossCorrelation(xs, ys, lagSteps)); found {
		pair.BestLagSteps, pair.BestLagR = best.Lag, round(best.R, 3)
		pair.BestLagSeconds = int64(time.Duration(best.Lag) * step / time.Second)
		if best.Lag != 0 && math.Abs(best.R)-math.Abs(r) >= leadMargin {
			tested = best.R
			testedX, testedY = shifted(xs, ys, best.Lag)
			if best.Lag > 0 {
				pair.Leads = x
			} else {
				pair.Leads = y
			}
		} else {
			pair.BestLagSteps, pair.BestLagSeconds, pair.BestLagR = 0, 0, round(r, 3)
		}
	}
	effective := stats.RegressionEffectiveSampleSize(testedX, testedY)
	lower, upper := stats.FisherInterval(tested, effective)
	pValue := stats.PValue(tested, effective)
	pair.EffectiveN, pair.Lower, pair.Upper, pair.PValue = round(effective, 1), round(lower, 3), round(upper, 3), roundP(pValue)
	pair.Significant = pValue < significanceLevel && (lower > 0 || upper < 0)
	pair.Strength, pair.Direction = strength(tested), direction(tested)
	if !pair.Significant {
		pair.Leads = ""
	}
	pair.Summary = summarize(pair, step)
	return pair, true
}

// shifted aligns x at t with y at t+lag.
func shifted(xs, ys []float64, lag int) ([]float64, []float64) {
	length := min(len(xs), len(ys))
	if lag >= 0 {
		return xs[:length-lag], ys[lag:length]
	}
	return xs[-lag:length], ys[:length+lag]
}

// slice restricts the grid to positions at or after from.
func (grid Grid) slice(from int) Grid {
	result := Grid{Timestamps: grid.Timestamps[from:], Values: map[string][]float64{}}
	for name, column := range grid.Values {
		result.Values[name] = column[from:]
	}
	return result
}

// regimeStart returns the first grid position after the latest rollout when
// enough samples follow it, because a rollout can change how signals relate
// (for example CPU per request). Otherwise the whole window is used.
func regimeStart(grid Grid, events []usage.Event) int {
	minimum := max(48, len(grid.Timestamps)/4)
	start := 0
	for _, event := range events {
		if event.Kind != usage.EventRollout {
			continue
		}
		for index, at := range grid.Timestamps {
			if !at.Before(event.Timestamp) {
				if len(grid.Timestamps)-index >= minimum {
					start = index
				}
				break
			}
		}
	}
	return start
}

func strength(r float64) string {
	switch magnitude := math.Abs(r); {
	case magnitude >= 0.7:
		return "strong"
	case magnitude >= 0.4:
		return "moderate"
	case magnitude >= 0.2:
		return "weak"
	default:
		return "negligible"
	}
}

func direction(r float64) string {
	switch {
	case r >= 0.2:
		return "positive"
	case r <= -0.2:
		return "negative"
	default:
		return "none"
	}
}

func summarize(pair Pair, step time.Duration) string {
	if !pair.Significant {
		return fmt.Sprintf("No reliable association between %s and %s (r=%.2f, 95%% CI %.2f to %.2f).", pair.X, pair.Y, pair.BestLagR, pair.Lower, pair.Upper)
	}
	text := fmt.Sprintf("%s and %s show a %s %s association (r=%.2f, 95%% CI %.2f to %.2f).", label(pair.X), pair.Y, pair.Strength, pair.Direction, pair.BestLagR, pair.Lower, pair.Upper)
	if pair.Leads != "" {
		other := pair.Y
		if pair.Leads == pair.Y {
			other = pair.X
		}
		text += fmt.Sprintf(" Changes in %s tend to come about %s before changes in %s.", pair.Leads, (time.Duration(absInt(pair.BestLagSteps)) * step).String(), other)
	}
	return text
}

func label(name string) string {
	if name == "" {
		return name
	}
	return strings.ToUpper(name[:1]) + name[1:]
}

func absInt(value int) int {
	if value < 0 {
		return -value
	}
	return value
}

// scalingEvents turns sustained replica-count changes into events.
func scalingEvents(grid Grid) []usage.Event {
	replicas, ok := grid.Values[Replicas]
	if !ok {
		return nil
	}
	events := []usage.Event{}
	previous := math.NaN()
	for index, value := range replicas {
		if math.IsNaN(value) {
			continue
		}
		current := math.Round(value)
		if !math.IsNaN(previous) && current != previous {
			kind := EventScaleUp
			if current < previous {
				kind = EventScaleDown
			}
			events = append(events, usage.Event{Timestamp: grid.Timestamps[index], Kind: kind, Count: int64(math.Abs(current - previous)), Detail: fmt.Sprintf("%.0f to %.0f replicas", previous, current)})
		}
		previous = current
	}
	return events
}

func changePoints(grid Grid, step time.Duration, events []usage.Event) []SeriesChange {
	minSegment := max(6, int(time.Hour/step))
	tolerance := max(2*step, 15*time.Minute)
	result := []SeriesChange{}
	for _, name := range []string{CPU, Memory, Latency, RequestRate, Throttling} {
		column, ok := grid.Values[name]
		if !ok || grid.Coverage(name) < minimumCoverage {
			continue
		}
		for _, point := range stats.ChangePoints(column, minSegment, changeMinRelative) {
			at := grid.Timestamps[point.Index]
			change := SeriesChange{
				Series: name, Timestamp: at, MeanBefore: round(point.MeanBefore, 3), MeanAfter: round(point.MeanAfter, 3),
				RelativeChange: round(point.RelativeChange, 3), Score: round(point.Score, 1),
			}
			var nearest *usage.Event
			for index := range events {
				distance := events[index].Timestamp.Sub(at)
				if distance < 0 {
					distance = -distance
				}
				if distance <= tolerance && (nearest == nil || distance < absDuration(nearest.Timestamp.Sub(at))) {
					nearest = &events[index]
				}
			}
			if nearest != nil {
				event := *nearest
				change.NearestEvent = &event
			}
			result = append(result, change)
		}
	}
	sort.SliceStable(result, func(left, right int) bool { return result[left].Timestamp.Before(result[right].Timestamp) })
	return result
}

func absDuration(value time.Duration) time.Duration {
	if value < 0 {
		return -value
	}
	return value
}

// eventImpacts compares the hour before each event with the hour after a
// short settling period.
func eventImpacts(grid Grid, step time.Duration, events []usage.Event) []EventImpact {
	result := []EventImpact{}
	window := max(time.Hour, 3*step)
	settle := max(10*time.Minute, step)
	for _, event := range events {
		for _, name := range []string{CPU, Memory, Latency, Throttling} {
			column, ok := grid.Values[name]
			if !ok {
				continue
			}
			before, beforeCount := meanBetween(grid, column, event.Timestamp.Add(-window), event.Timestamp)
			after, afterCount := meanBetween(grid, column, event.Timestamp.Add(settle), event.Timestamp.Add(settle+window))
			if beforeCount < 3 || afterCount < 3 {
				continue
			}
			relative := 0.0
			if before != 0 {
				relative = (after - before) / math.Abs(before)
			}
			result = append(result, EventImpact{
				Event: event, Series: name, Before: round(before, 3), After: round(after, 3),
				BeforeSamples: beforeCount, AfterSamples: afterCount, RelativeChange: round(relative, 3),
				Material: math.Abs(relative) >= materialEventChange,
			})
		}
	}
	return result
}

func meanBetween(grid Grid, column []float64, from, to time.Time) (float64, int) {
	sum, count := 0.0, 0
	for index, at := range grid.Timestamps {
		if !at.Before(from) && at.Before(to) && !math.IsNaN(column[index]) {
			sum += column[index]
			count++
		}
	}
	if count == 0 {
		return 0, 0
	}
	return sum / float64(count), count
}

// seasonalPeriods are the daily and weekly cycles expressed in steps.
func seasonalPeriods(step time.Duration) []int {
	return []int{int(24 * time.Hour / step), int(7 * 24 * time.Hour / step)}
}

func findings(report Report) []Finding {
	result := []Finding{}
	byName := map[[2]string]Pair{}
	for _, pair := range report.Pairs {
		byName[[2]string{pair.X, pair.Y}] = pair
	}
	if pair, ok := byName[[2]string{Throttling, Latency}]; ok && pair.Significant && pair.R >= 0.4 {
		result = append(result, Finding{"critical", "Latency moves with CPU throttling", "Request latency rises when the container is throttled. Raising or removing the CPU limit is the first change to test.", pair.Summary})
	}
	if pair, ok := byName[[2]string{CPU, Throttling}]; ok && pair.Significant && pair.R >= 0.5 {
		result = append(result, Finding{"warning", "Throttling rises with CPU usage", "The CPU limit is reached at busy times, so measured CPU understates demand. CPU is not reduced while this holds.", pair.Summary})
	}
	if pair, ok := byName[[2]string{CPU, Latency}]; ok && pair.Significant && pair.R >= 0.5 {
		result = append(result, Finding{"warning", "Latency moves with CPU usage", "CPU appears to be on the request path; prefer conservative sizing.", pair.Summary})
	}
	if pair, ok := byName[[2]string{RequestRate, CPU}]; ok && pair.Significant && pair.R >= 0.5 {
		result = append(result, Finding{"info", "CPU follows traffic", "CPU demand tracks request rate, so sizing must cover peak traffic rather than average traffic.", pair.Summary})
	}
	if pair, ok := byName[[2]string{CPUPerReplica, Replicas}]; ok && pair.Significant && pair.R <= -0.4 {
		result = append(result, Finding{"info", "Scaling spreads load", "Per-replica CPU falls as replicas are added. Request changes alter the autoscaler's utilization and therefore the replica count.", pair.Summary})
	}
	if pair, ok := byName[[2]string{Memory, Latency}]; ok && pair.Significant && pair.R >= 0.5 {
		result = append(result, Finding{"info", "Latency moves with memory", "This pattern often accompanies garbage-collection pressure; do not reduce memory without checking it.", pair.Summary})
	}
	for _, change := range report.ChangePoints {
		if change.Series != CPU && change.Series != Memory {
			continue
		}
		detail := fmt.Sprintf("%s level changed %+.0f%% at %s.", change.Series, change.RelativeChange*100, change.Timestamp.Format(time.RFC3339))
		title := "Usage level shifted"
		if change.NearestEvent != nil {
			detail += fmt.Sprintf(" It coincides with a %s at %s.", change.NearestEvent.Kind, change.NearestEvent.Timestamp.Format(time.RFC3339))
			if change.NearestEvent.Kind == usage.EventRollout {
				title = "A rollout coincides with a usage shift"
			}
		}
		detail += " Recommendations size from the most recent level when enough samples remain."
		result = append(result, Finding{"info", title, detail, fmt.Sprintf("mean %.3g to %.3g, %.1f noise units", change.MeanBefore, change.MeanAfter, change.Score)})
	}
	oom := 0
	for _, event := range report.Events {
		if event.Kind == usage.EventOOMKill {
			oom += int(max(1, event.Count))
		}
	}
	if oom > 0 {
		result = append(result, Finding{"critical", "OOM kills in the window", fmt.Sprintf("%d OOM kill(s) occurred. Memory is never reduced while OOM kills are present.", oom), "kube-state-metrics last terminated reason"})
	}
	for name, seasonality := range report.Seasonality {
		if !seasonality.Detected || report.StepSeconds == 0 {
			continue
		}
		period := time.Duration(seasonality.PeriodSteps) * time.Duration(report.StepSeconds) * time.Second
		result = append(result, Finding{"info", fmt.Sprintf("%s repeats every %s", name, period), "Usage follows a regular cycle, so recommendations cover the busy part of the cycle and scheduled scaling may serve the peaks.", fmt.Sprintf("autocorrelation %.2f at the cycle length", seasonality.Strength)})
	}
	sort.SliceStable(result, func(left, right int) bool {
		return severityRank(result[left].Severity) > severityRank(result[right].Severity)
	})
	return result
}

func severityRank(severity string) int {
	switch severity {
	case "critical":
		return 2
	case "warning":
		return 1
	default:
		return 0
	}
}

func round(value float64, decimals int) float64 {
	factor := math.Pow(10, float64(decimals))
	return math.Round(value*factor) / factor
}

// roundP keeps small p-values readable without collapsing them to zero;
// 1e-6 means "at most 1e-6".
func roundP(value float64) float64 {
	if value < 1e-6 {
		return 1e-6
	}
	return round(value, 6)
}
