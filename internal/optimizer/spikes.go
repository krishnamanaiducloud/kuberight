package optimizer

import (
	"math"
	"sort"
	"time"
)

// Spike classifications.
const (
	SpikesNone      = "none"
	SpikesRare      = "rare"
	SpikesFrequent  = "frequent"
	SpikesRecurring = "recurring"
)

const (
	// A sample is a spike when it exceeds the median by more than four
	// scaled median absolute deviations, 1.5x the median, and a quarter of
	// the peak. MAD is robust: the spikes themselves cannot inflate it.
	spikeMADMultiplier      = 4 * 1.4826
	spikeMedianMultiplier   = 1.5
	spikePeakFraction       = 0.25
	minimumSpikeSamples     = 30
	frequentSpikesPerDay    = 4
	recurringMinimumSpikes  = 3
	recurringMinimumDays    = 2
	recurringShareThreshold = 0.7
	recurringWindowSeconds  = 2 * 3600
)

// SpikeProfile describes short bursts above normal usage.
type SpikeProfile struct {
	Classification        string  `json:"classification"`
	Count                 int     `json:"count"`
	SpikesPerDay          float64 `json:"spikesPerDay"`
	MedianDurationMinutes float64 `json:"medianDurationMinutes"`
	// PeakToMedian is the largest spike divided by median usage.
	PeakToMedian float64 `json:"peakToMedian"`
	Threshold    float64 `json:"threshold"`
	// RecurringHourUTC is the hour of day at which recurring spikes start,
	// when the sample start time is known.
	RecurringHourUTC *int `json:"recurringHourUtc,omitempty"`
}

func median(sorted []float64) float64 {
	middle := len(sorted) / 2
	if len(sorted)%2 == 1 {
		return sorted[middle]
	}
	return (sorted[middle-1] + sorted[middle]) / 2
}

// SpikeAnalysis finds runs of samples far above typical usage and classifies
// how often, how long, and how regularly they occur.
func SpikeAnalysis(values []float64, intervalSeconds int64, start *time.Time) SpikeProfile {
	profile := SpikeProfile{Classification: SpikesNone}
	if len(values) < minimumSpikeSamples || intervalSeconds <= 0 {
		return profile
	}
	sorted := append([]float64(nil), values...)
	sort.Float64s(sorted)
	middle := median(sorted)
	deviations := make([]float64, len(values))
	for index, value := range values {
		deviations[index] = math.Abs(value - middle)
	}
	sort.Float64s(deviations)
	mad := median(deviations)
	peak := sorted[len(sorted)-1]
	threshold := math.Max(middle+spikeMADMultiplier*mad, math.Max(middle*spikeMedianMultiplier, peak*spikePeakFraction))
	profile.Threshold = math.Round(threshold*100) / 100
	if peak <= threshold || peak == 0 {
		return profile
	}

	var starts []int
	var durations []float64
	runStart := -1
	for index := 0; index <= len(values); index++ {
		above := index < len(values) && values[index] > threshold
		switch {
		case above && runStart < 0:
			runStart = index
		case !above && runStart >= 0:
			starts = append(starts, runStart)
			durations = append(durations, float64((index-runStart)*int(intervalSeconds))/60)
			runStart = -1
		}
	}
	profile.Count = len(starts)
	sort.Float64s(durations)
	profile.MedianDurationMinutes = math.Round(median(durations)*10) / 10
	if middle > 0 {
		profile.PeakToMedian = math.Round(peak/middle*100) / 100
	}
	spanDays := float64(len(values)) * float64(intervalSeconds) / secondsPerDay
	profile.SpikesPerDay = math.Round(float64(profile.Count)/math.Max(spanDays, 1)*100) / 100

	profile.Classification = SpikesRare
	if spanDays >= 1 && profile.SpikesPerDay >= frequentSpikesPerDay {
		profile.Classification = SpikesFrequent
	}
	if profile.Count >= recurringMinimumSpikes && spanDays >= recurringMinimumDays {
		if phase, share := dominantPhase(starts, intervalSeconds); share >= recurringShareThreshold {
			profile.Classification = SpikesRecurring
			if start != nil {
				hour := start.UTC().Add(time.Duration(phase) * time.Second).Hour()
				profile.RecurringHourUTC = &hour
			}
		}
	}
	return profile
}

// dominantPhase finds the two-hour window of the day containing the most
// spike starts and returns its start offset and the share of spikes in it.
func dominantPhase(starts []int, intervalSeconds int64) (int64, float64) {
	phases := make([]int64, len(starts))
	for index, start := range starts {
		phases[index] = (int64(start) * intervalSeconds) % secondsPerDay
	}
	bestPhase, bestCount := int64(0), 0
	for _, candidate := range phases {
		count := 0
		for _, phase := range phases {
			// Distance on a circular day so 23:30 and 00:30 are neighbors.
			distance := (phase - candidate + secondsPerDay) % secondsPerDay
			if distance < recurringWindowSeconds {
				count++
			}
		}
		if count > bestCount {
			bestPhase, bestCount = candidate, count
		}
	}
	return bestPhase, float64(bestCount) / float64(len(phases))
}
