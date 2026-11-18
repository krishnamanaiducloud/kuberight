package optimizer

// Candidate is the configuration a stock profile would produce from the same
// observations, for comparing savings against risk. Candidates are
// informational: only the recommendation itself can be approved or applied.
type Candidate struct {
	Profile string `json:"profile"`
	// Selected marks the profile whose policy produced the recommendation.
	// A recommendation from a custom policy selects none.
	Selected                     bool    `json:"selected"`
	CPURequestMilli              int64   `json:"cpuRequestMilli"`
	MemoryRequestBytes           int64   `json:"memoryRequestBytes"`
	CPULimitMilli                int64   `json:"cpuLimitMilli"`
	MemoryLimitBytes             int64   `json:"memoryLimitBytes"`
	EstimatedCPUChangePercent    float64 `json:"estimatedCpuChangePercent"`
	EstimatedMemoryChangePercent float64 `json:"estimatedMemoryChangePercent"`
	// Status is the candidate's assessment verdict, such as "recommend" or
	// "hold".
	Status string `json:"status,omitempty"`
	// StatusReasons explain a verdict other than "recommend", and RiskScore
	// is the candidate's 0-100 assessment risk.
	StatusReasons []string `json:"statusReasons,omitempty"`
	RiskScore     *float64 `json:"riskScore,omitempty"`
	// Backtest replays every observed sample against the candidate;
	// OutOfSample sizes it from the first 70% of samples and measures the
	// rest.
	Backtest    Backtest    `json:"backtest"`
	OutOfSample OutOfSample `json:"outOfSample"`
}

// candidates sizes the input with every stock profile. Each profile keeps the
// effective policy's resource bounds and CPU-limit strategy, and the stricter
// of its own and the effective change guards, so a candidate never leaves the
// range or the pace an operator allowed. Each candidate runs the full
// assessment, including LimitRange, quota, QoS, and autoscaler checks.
func candidates(input Input, effective Policy) []Candidate {
	selected := policyFingerprint(effective)
	result := make([]Candidate, 0, 3)
	for _, profile := range Profiles() {
		policy := profile.Policy
		policy.MinimumCPUMilli, policy.MaximumCPUMilli = effective.MinimumCPUMilli, effective.MaximumCPUMilli
		policy.MinimumMemoryBytes, policy.MaximumMemoryBytes = effective.MinimumMemoryBytes, effective.MaximumMemoryBytes
		policy.CPULimitStrategy = effective.CPULimitStrategy
		policy = guarded(normalizePolicy(policy), effective)
		alternative := input
		alternative.Policy = &policy
		alternative.skipCandidates = true
		recommendation, err := Recommend(alternative)
		if err != nil {
			continue
		}
		candidate := Candidate{
			Profile: profile.Name, Selected: policyFingerprint(policy) == selected,
			CPURequestMilli: recommendation.CPURequestMilli, MemoryRequestBytes: recommendation.MemoryRequestBytes,
			CPULimitMilli: recommendation.CPULimitMilli, MemoryLimitBytes: recommendation.MemoryLimitBytes,
			EstimatedCPUChangePercent: recommendation.EstimatedCPUChangePercent, EstimatedMemoryChangePercent: recommendation.EstimatedMemoryChangePct,
			Backtest: recommendation.Backtest,
		}
		if recommendation.Assessment != nil {
			candidate.Status, candidate.OutOfSample = recommendation.Assessment.Status, recommendation.Assessment.OutOfSample
			risk := recommendation.Assessment.RiskScore
			candidate.RiskScore = &risk
			if candidate.Status != StatusRecommend {
				candidate.StatusReasons = recommendation.Assessment.StatusReasons
			}
		}
		result = append(result, candidate)
	}
	return result
}

// guarded applies the stricter of a profile's and the effective policy's
// change guards: the largest reduction per change, the evidence required, the
// hysteresis, the cooldown, the agreeing observations, and the trend horizon.
// Margins and percentiles stay the profile's, since they are what the
// profiles compare.
func guarded(profile, effective Policy) Policy {
	profile.MaximumReduction = min(profile.MaximumReduction, effective.MaximumReduction)
	profile.MinimumSamples = max(profile.MinimumSamples, effective.MinimumSamples)
	profile.HighConfidenceSamples = max(profile.HighConfidenceSamples, effective.HighConfidenceSamples)
	profile.MinimumChangeRatio = max(profile.MinimumChangeRatio, effective.MinimumChangeRatio)
	profile.CooldownHours = max(profile.CooldownHours, effective.CooldownHours)
	profile.RequiredObservations = max(profile.RequiredObservations, effective.RequiredObservations)
	profile.TrendHorizonDays = max(profile.TrendHorizonDays, effective.TrendHorizonDays)
	return profile
}
