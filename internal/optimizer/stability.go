package optimizer

import (
	"fmt"
	"math"
	"sort"
	"time"
)

// Stability defaults. Increases are never held: a container that needs more
// CPU or memory gets it immediately. Only reductions wait.
const (
	defaultMinimumChangeRatio   = 0.05
	defaultCooldownHours        = 24
	defaultRequiredObservations = 1
	// Changes smaller than these absolute amounts are noise regardless of the
	// ratio, so small containers do not flap by a few millicores.
	minimumCPUChangeMilli    = 10
	minimumMemoryChangeBytes = 16 << 20
	// oscillationWindow is how far back prior recommendations are compared.
	oscillationWindow = 14 * 24 * time.Hour
	// oscillationFlips direction changes inside the window mark a container
	// whose advice alternates between increases and reductions.
	oscillationFlips = 2
	// confirmationSpacing keeps two recommendations generated minutes apart
	// from counting as independent observations.
	confirmationSpacing     = time.Hour
	maxPriorRecommendations = 50
)

// PriorRecommendation is an earlier recommendation for the same container.
// Oscillation and confirmation checks compare against it; it never changes
// the sizing itself.
type PriorRecommendation struct {
	CreatedAt                 time.Time `json:"createdAt"`
	CurrentCPURequestMilli    int64     `json:"currentCpuRequestMilli"`
	CPURequestMilli           int64     `json:"cpuRequestMilli"`
	CurrentMemoryRequestBytes int64     `json:"currentMemoryRequestBytes"`
	MemoryRequestBytes        int64     `json:"memoryRequestBytes"`
}

// Stability explains the controls that keep advice from flapping:
// hysteresis, a cooldown after configuration changes, oscillation detection,
// and repeated-observation confirmation.
type Stability struct {
	MinimumChangeRatio float64 `json:"minimumChangeRatio"`
	// SuppressedChanges were below the minimum meaningful change and were
	// left at their current values.
	SuppressedChanges    []string   `json:"suppressedChanges"`
	CooldownHours        int        `json:"cooldownHours"`
	LastResourceChangeAt *time.Time `json:"lastResourceChangeAt,omitempty"`
	CooldownEndsAt       *time.Time `json:"cooldownEndsAt,omitempty"`
	CooldownActive       bool       `json:"cooldownActive"`
	// DirectionChanges counts alternations between increases and reductions
	// across recent recommendations, including this one.
	DirectionChanges       int  `json:"directionChanges"`
	Oscillating            bool `json:"oscillating"`
	RequiredObservations   int  `json:"requiredObservations"`
	ConsistentObservations int  `json:"consistentObservations"`
	// ReductionsHeld explains each reduction that was withheld.
	ReductionsHeld []string `json:"reductionsHeld"`
	// Changed is false when the final values equal the current ones.
	Changed bool `json:"changed"`
}

type direction int

const (
	unchanged direction = iota
	reduce
	increase
)

func changeThreshold(current int64, ratio float64, floor int64) int64 {
	return max(int64(math.Ceil(float64(current)*ratio)), floor)
}

func changeDirection(current, recommended int64, ratio float64, floor int64) direction {
	if current <= 0 || recommended == current {
		if recommended != current {
			return increase
		}
		return unchanged
	}
	delta := recommended - current
	if delta < 0 && -delta >= changeThreshold(current, ratio, floor) {
		return reduce
	}
	if delta > 0 && delta >= changeThreshold(current, ratio, floor) {
		return increase
	}
	return unchanged
}

// resourcePair is one resource's request and limit, current and proposed.
type resourcePair struct {
	name                           string
	currentRequest, currentLimit   int64
	request, limit                 *int64
	floor                          int64
	format                         func(int64) string
	safetySignal                   bool
	priorCurrent, priorRecommended func(PriorRecommendation) int64
}

// applyStability adjusts final values. Each resource's request and limit are
// treated as a pair so a Guaranteed pod's request/limit equality survives.
func applyStability(input Input, policy Policy, values *sizing, safety struct{ cpu, memory bool }) Stability {
	stability := Stability{
		MinimumChangeRatio: policy.MinimumChangeRatio, CooldownHours: policy.CooldownHours, RequiredObservations: policy.RequiredObservations,
		SuppressedChanges: []string{}, ReductionsHeld: []string{}, LastResourceChangeAt: input.LastResourceChangeAt,
	}
	pairs := []resourcePair{
		{
			name: "CPU", currentRequest: input.CPURequestsMilli, currentLimit: input.CPULimitMilli, request: &values.cpuRequest, limit: &values.cpuLimit,
			floor: minimumCPUChangeMilli, format: CPUQuantity, safetySignal: safety.cpu,
			priorCurrent:     func(prior PriorRecommendation) int64 { return prior.CurrentCPURequestMilli },
			priorRecommended: func(prior PriorRecommendation) int64 { return prior.CPURequestMilli },
		},
		{
			name: "memory", currentRequest: input.MemoryRequestsBytes, currentLimit: input.MemoryLimitBytes, request: &values.memoryRequest, limit: &values.memoryLimit,
			floor: minimumMemoryChangeBytes, format: MemoryQuantity, safetySignal: safety.memory,
			priorCurrent:     func(prior PriorRecommendation) int64 { return prior.CurrentMemoryRequestBytes },
			priorRecommended: func(prior PriorRecommendation) int64 { return prior.MemoryRequestBytes },
		},
	}

	end := input.Provenance.ObservationEnd
	if input.LastResourceChangeAt != nil && end != nil {
		cooldownEnds := input.LastResourceChangeAt.Add(time.Duration(policy.CooldownHours) * time.Hour)
		stability.CooldownEndsAt = &cooldownEnds
		stability.CooldownActive = end.Before(cooldownEnds)
	}
	priors := append([]PriorRecommendation(nil), input.PriorRecommendations...)
	sort.Slice(priors, func(left, right int) bool { return priors[left].CreatedAt.After(priors[right].CreatedAt) })
	if end != nil {
		recent := priors[:0]
		for _, prior := range priors {
			if !prior.CreatedAt.After(*end) && end.Sub(prior.CreatedAt) <= oscillationWindow {
				recent = append(recent, prior)
			}
		}
		priors = recent
	}

	hold := func(pair resourcePair, reason string) {
		*pair.request, *pair.limit = pair.currentRequest, pair.currentLimit
		stability.ReductionsHeld = append(stability.ReductionsHeld, pair.name+": "+reason)
	}
	for _, pair := range pairs {
		requestChange := changeDirection(pair.currentRequest, *pair.request, policy.MinimumChangeRatio, pair.floor)
		limitChange := changeDirection(pair.currentLimit, *pair.limit, policy.MinimumChangeRatio, pair.floor)
		if pair.currentLimit == 0 && *pair.limit == 0 {
			limitChange = unchanged
		}
		// Hysteresis: a change below the meaningful threshold is noise. It
		// is kept only when a safety signal (OOM, throttling, a pinned limit)
		// makes even a small increase matter.
		if requestChange == unchanged && limitChange == unchanged && (*pair.request != pair.currentRequest || *pair.limit != pair.currentLimit) {
			if !pair.safetySignal || *pair.request < pair.currentRequest {
				stability.SuppressedChanges = append(stability.SuppressedChanges, fmt.Sprintf("%s request %s to %s is below the %.0f%% minimum meaningful change",
					pair.name, pair.format(pair.currentRequest), pair.format(*pair.request), policy.MinimumChangeRatio*100))
				*pair.request, *pair.limit = pair.currentRequest, pair.currentLimit
			}
			continue
		}
		// A lower request or a lower (but still present) limit is a
		// reduction. Removing a CPU limit relaxes the container and is not.
		if requestChange != reduce && (limitChange != reduce || *pair.limit == 0) {
			continue
		}
		if stability.CooldownActive {
			hold(pair, fmt.Sprintf("requests or limits changed at %s; reductions wait %dh so usage under the new configuration is measured", input.LastResourceChangeAt.UTC().Format(time.RFC3339), policy.CooldownHours))
			continue
		}
		// Direction history, oldest first, ending with this recommendation.
		directions := make([]direction, 0, len(priors)+1)
		for index := len(priors) - 1; index >= 0; index-- {
			if value := changeDirection(pair.priorCurrent(priors[index]), pair.priorRecommended(priors[index]), policy.MinimumChangeRatio, pair.floor); value != unchanged {
				directions = append(directions, value)
			}
		}
		directions = append(directions, reduce)
		flips := 0
		for index := 1; index < len(directions); index++ {
			if directions[index] != directions[index-1] {
				flips++
			}
		}
		stability.DirectionChanges = max(stability.DirectionChanges, flips)
		if flips >= oscillationFlips {
			stability.Oscillating = true
			hold(pair, fmt.Sprintf("recent recommendations alternated between increases and reductions %d times in %s; the reduction is held to prevent oscillation", flips, oscillationWindow))
			continue
		}
		// Confirmation: consecutive earlier recommendations against the same
		// current value that also recommended a reduction.
		consistent, last := 1, time.Time{}
		if end != nil {
			last = *end
		}
		for _, prior := range priors {
			if pair.priorCurrent(prior) != pair.currentRequest || changeDirection(pair.priorCurrent(prior), pair.priorRecommended(prior), policy.MinimumChangeRatio, pair.floor) != reduce {
				break
			}
			if !last.IsZero() && last.Sub(prior.CreatedAt) < confirmationSpacing {
				continue
			}
			consistent++
			last = prior.CreatedAt
		}
		if stability.ConsistentObservations == 0 || consistent < stability.ConsistentObservations {
			stability.ConsistentObservations = consistent
		}
		if consistent < policy.RequiredObservations {
			hold(pair, fmt.Sprintf("%d of %d consistent observations at least %s apart; the reduction is offered after it is confirmed", consistent, policy.RequiredObservations, confirmationSpacing))
		}
	}
	if stability.ConsistentObservations == 0 {
		stability.ConsistentObservations = 1
	}
	stability.Changed = values.cpuRequest != input.CPURequestsMilli || values.cpuLimit != input.CPULimitMilli ||
		values.memoryRequest != input.MemoryRequestsBytes || values.memoryLimit != input.MemoryLimitBytes
	return stability
}
