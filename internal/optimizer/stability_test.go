package optimizer

import (
	"strings"
	"testing"
	"time"
)

var stabilityEnd = time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)

// reducibleInput is a well-observed container whose requests are far above
// usage, so the balanced policy reduces both resources by the 20% bound.
func reducibleInput() Input {
	input := baseInput()
	input.CPUSamplesMilli = samples(600, 100)
	input.MemorySamplesBytes = samples(600, 100<<20)
	start := stabilityEnd.Add(-600 * time.Minute)
	input.Provenance.ObservationStart, input.Provenance.ObservationEnd = &start, &stabilityEnd
	return input
}

func stableRecommend(t *testing.T, input Input) Recommendation {
	t.Helper()
	recommendation, err := Recommend(input)
	if err != nil {
		t.Fatal(err)
	}
	if recommendation.Stability == nil {
		t.Fatal("stability evidence is missing")
	}
	return recommendation
}

func TestStabilityBaselineReducesWithoutHolds(t *testing.T) {
	recommendation := stableRecommend(t, reducibleInput())
	if recommendation.CPURequestMilli >= 1000 || recommendation.MemoryRequestBytes >= 1<<30 || len(recommendation.Stability.ReductionsHeld) != 0 || recommendation.Assessment.Status != StatusRecommend {
		t.Fatalf("baseline: cpu=%d memory=%d stability=%+v status=%s", recommendation.CPURequestMilli, recommendation.MemoryRequestBytes, recommendation.Stability, recommendation.Assessment.Status)
	}
}

func TestHysteresisSuppressesChangesBelowTheMinimum(t *testing.T) {
	input := reducibleInput()
	// No limits, and usage that sizes to about 3% below the current requests
	// (P95 x 1.3 for CPU, P99 x 1.25 for memory).
	input.CPULimitMilli, input.MemoryLimitBytes = 0, 0
	input.CPUSamplesMilli = samples(600, 742)
	input.MemorySamplesBytes = samples(600, 795<<20)
	recommendation := stableRecommend(t, input)
	if recommendation.CPURequestMilli != input.CPURequestsMilli || recommendation.MemoryRequestBytes != input.MemoryRequestsBytes {
		t.Fatalf("small changes were not suppressed: cpu %d->%d memory %d->%d suppressed=%v", input.CPURequestsMilli, recommendation.CPURequestMilli, input.MemoryRequestsBytes, recommendation.MemoryRequestBytes, recommendation.Stability.SuppressedChanges)
	}
	if recommendation.Assessment.Status != StatusNoChange || recommendation.Stability.Changed || len(recommendation.Stability.SuppressedChanges) != 2 {
		t.Fatalf("status=%s stability=%+v", recommendation.Assessment.Status, recommendation.Stability)
	}
	if approvable, _ := recommendation.Approvable(); approvable {
		t.Fatal("a no-change recommendation must not be approvable")
	}
	// A 10% reduction is meaningful and is offered.
	input.CPUSamplesMilli = samples(600, 690)
	if meaningful := stableRecommend(t, input); meaningful.CPURequestMilli >= input.CPURequestsMilli {
		t.Fatalf("a meaningful reduction was suppressed: %d", meaningful.CPURequestMilli)
	}
}

func TestHysteresisNeverSuppressesASafetyIncrease(t *testing.T) {
	input := reducibleInput()
	input.ObservedCPUThrottlingRatio = 0.3
	input.CPUSamplesMilli = samples(600, 980)
	recommendation := stableRecommend(t, input)
	if recommendation.CPULimitMilli <= input.CPULimitMilli {
		t.Fatalf("throttling relief was suppressed: limit %d->%d", input.CPULimitMilli, recommendation.CPULimitMilli)
	}
}

func TestCooldownHoldsReductionsAfterAResourceChange(t *testing.T) {
	input := reducibleInput()
	changed := stabilityEnd.Add(-2 * time.Hour)
	input.LastResourceChangeAt = &changed
	recommendation := stableRecommend(t, input)
	stability := recommendation.Stability
	if recommendation.CPURequestMilli != input.CPURequestsMilli || recommendation.MemoryRequestBytes != input.MemoryRequestsBytes || !stability.CooldownActive || len(stability.ReductionsHeld) != 2 {
		t.Fatalf("cooldown: cpu=%d memory=%d stability=%+v", recommendation.CPURequestMilli, recommendation.MemoryRequestBytes, stability)
	}
	if recommendation.Assessment.Status != StatusHold || !strings.Contains(strings.Join(recommendation.Assessment.StatusReasons, "; "), "reductions wait 24h") {
		t.Fatalf("status=%s reasons=%v", recommendation.Assessment.Status, recommendation.Assessment.StatusReasons)
	}
	if want := changed.Add(24 * time.Hour); stability.CooldownEndsAt == nil || !stability.CooldownEndsAt.Equal(want) {
		t.Fatalf("cooldown end = %v, want %v", stability.CooldownEndsAt, want)
	}
	// After the cooldown the same evidence reduces.
	old := stabilityEnd.Add(-25 * time.Hour)
	input.LastResourceChangeAt = &old
	if after := stableRecommend(t, input); after.CPURequestMilli >= input.CPURequestsMilli || after.Stability.CooldownActive {
		t.Fatalf("reduction still held after cooldown: %+v", after.Stability)
	}
}

func TestCooldownStillAllowsIncreases(t *testing.T) {
	input := reducibleInput()
	input.CPUSamplesMilli = samples(600, 1500)
	changed := stabilityEnd.Add(-time.Hour)
	input.LastResourceChangeAt = &changed
	recommendation := stableRecommend(t, input)
	if recommendation.CPURequestMilli <= input.CPURequestsMilli || recommendation.Assessment.Status != StatusRecommend {
		t.Fatalf("an increase was held during cooldown: cpu %d->%d status=%s", input.CPURequestsMilli, recommendation.CPURequestMilli, recommendation.Assessment.Status)
	}
	if recommendation.MemoryRequestBytes != input.MemoryRequestsBytes {
		t.Fatalf("the memory reduction should be held: %d", recommendation.MemoryRequestBytes)
	}
}

func TestOscillationHoldsAlternatingReductions(t *testing.T) {
	input := reducibleInput()
	current := input.CPURequestsMilli
	input.PriorRecommendations = []PriorRecommendation{
		{CreatedAt: stabilityEnd.Add(-72 * time.Hour), CurrentCPURequestMilli: current, CPURequestMilli: 700, CurrentMemoryRequestBytes: input.MemoryRequestsBytes, MemoryRequestBytes: input.MemoryRequestsBytes},
		{CreatedAt: stabilityEnd.Add(-24 * time.Hour), CurrentCPURequestMilli: 700, CPURequestMilli: current, CurrentMemoryRequestBytes: input.MemoryRequestsBytes, MemoryRequestBytes: input.MemoryRequestsBytes},
	}
	recommendation := stableRecommend(t, input)
	stability := recommendation.Stability
	if !stability.Oscillating || stability.DirectionChanges != 2 || recommendation.CPURequestMilli != current {
		t.Fatalf("oscillation: cpu=%d stability=%+v", recommendation.CPURequestMilli, stability)
	}
	if recommendation.MemoryRequestBytes >= input.MemoryRequestsBytes {
		t.Fatalf("memory has no alternating history and should still reduce: %d", recommendation.MemoryRequestBytes)
	}
	// Recommendations older than the oscillation window are ignored.
	for index := range input.PriorRecommendations {
		input.PriorRecommendations[index].CreatedAt = stabilityEnd.Add(-30 * 24 * time.Hour)
	}
	if later := stableRecommend(t, input); later.Stability.Oscillating {
		t.Fatalf("stale history caused an oscillation hold: %+v", later.Stability)
	}
}

func TestRepeatedObservationsConfirmReductions(t *testing.T) {
	input := reducibleInput()
	input.Policy = new(Policy)
	*input.Policy = DefaultPolicy()
	input.Policy.RequiredObservations = 2
	held := stableRecommend(t, input)
	if held.CPURequestMilli != input.CPURequestsMilli || held.Stability.ConsistentObservations != 1 || held.Assessment.Status != StatusHold {
		t.Fatalf("unconfirmed reduction: cpu=%d stability=%+v status=%s", held.CPURequestMilli, held.Stability, held.Assessment.Status)
	}
	prior := PriorRecommendation{
		CreatedAt: stabilityEnd.Add(-6 * time.Hour), CurrentCPURequestMilli: input.CPURequestsMilli, CPURequestMilli: 800,
		CurrentMemoryRequestBytes: input.MemoryRequestsBytes, MemoryRequestBytes: input.MemoryRequestsBytes * 8 / 10,
	}
	// Too close in time to count as an independent observation.
	close := prior
	close.CreatedAt = stabilityEnd.Add(-10 * time.Minute)
	input.PriorRecommendations = []PriorRecommendation{close}
	if stillHeld := stableRecommend(t, input); stillHeld.CPURequestMilli != input.CPURequestsMilli {
		t.Fatalf("a recommendation from ten minutes earlier confirmed the reduction: %+v", stillHeld.Stability)
	}
	input.PriorRecommendations = []PriorRecommendation{prior}
	confirmed := stableRecommend(t, input)
	if confirmed.CPURequestMilli >= input.CPURequestsMilli || confirmed.MemoryRequestBytes >= input.MemoryRequestsBytes || confirmed.Stability.ConsistentObservations != 2 {
		t.Fatalf("confirmed reduction: cpu=%d memory=%d stability=%+v", confirmed.CPURequestMilli, confirmed.MemoryRequestBytes, confirmed.Stability)
	}
	// A prior observation against different current requests does not count.
	prior.CurrentCPURequestMilli, prior.CurrentMemoryRequestBytes = 4000, 8<<30
	input.PriorRecommendations = []PriorRecommendation{prior}
	if different := stableRecommend(t, input); different.CPURequestMilli != input.CPURequestsMilli {
		t.Fatalf("a prior against other current values confirmed the reduction: %+v", different.Stability)
	}
}

func TestMemoryLimitReductionIsHeldDuringCooldown(t *testing.T) {
	input := reducibleInput()
	input.MemoryRequestsBytes = 160 << 20
	input.MemoryLimitBytes = 4 << 30
	changed := stabilityEnd.Add(-time.Hour)
	input.LastResourceChangeAt = &changed
	recommendation := stableRecommend(t, input)
	if recommendation.MemoryLimitBytes != input.MemoryLimitBytes {
		t.Fatalf("memory limit %d -> %d during cooldown", input.MemoryLimitBytes, recommendation.MemoryLimitBytes)
	}
}

func TestStabilityPolicyValidation(t *testing.T) {
	for name, mutate := range map[string]func(*Policy){
		"ratio too small":    func(policy *Policy) { policy.MinimumChangeRatio = 0.001 },
		"ratio too large":    func(policy *Policy) { policy.MinimumChangeRatio = 0.9 },
		"cooldown too long":  func(policy *Policy) { policy.CooldownHours = 1000 },
		"cooldown negative":  func(policy *Policy) { policy.CooldownHours = -1 },
		"observations above": func(policy *Policy) { policy.RequiredObservations = 6 },
	} {
		t.Run(name, func(t *testing.T) {
			input := reducibleInput()
			policy := DefaultPolicy()
			mutate(&policy)
			input.Policy = &policy
			if _, err := Recommend(input); err == nil {
				t.Fatal("invalid stability policy was accepted")
			}
		})
	}
	conservative, _ := ProfileByName("conservative")
	if conservative.RequiredObservations != 2 || conservative.CooldownHours != 48 {
		t.Fatalf("conservative profile = %+v", conservative)
	}
	input := reducibleInput()
	input.PriorRecommendations = make([]PriorRecommendation, maxPriorRecommendations+1)
	if _, err := Recommend(input); err == nil {
		t.Fatal("too many prior recommendations were accepted")
	}
}
