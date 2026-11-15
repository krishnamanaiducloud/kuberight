package optimizer

import "testing"

func TestCandidatesCompareProfilesWithinTheOperatorsBounds(t *testing.T) {
	input := baseInput()
	recommendation, err := Recommend(input)
	if err != nil {
		t.Fatal(err)
	}
	if len(recommendation.Candidates) != 3 {
		t.Fatalf("candidates=%#v", recommendation.Candidates)
	}
	byProfile := map[string]Candidate{}
	for _, candidate := range recommendation.Candidates {
		byProfile[candidate.Profile] = candidate
	}
	conservative, balanced, aggressive := byProfile["conservative"], byProfile["balanced"], byProfile["aggressive"]
	// The default policy is the balanced profile, which reproduces the recommendation.
	if !balanced.Selected || conservative.Selected || aggressive.Selected ||
		balanced.CPURequestMilli != recommendation.CPURequestMilli || balanced.MemoryRequestBytes != recommendation.MemoryRequestBytes {
		t.Fatalf("balanced=%#v recommendation cpu=%d memory=%d", balanced, recommendation.CPURequestMilli, recommendation.MemoryRequestBytes)
	}
	// Wider margins and smaller reduction caps keep more CPU.
	if conservative.CPURequestMilli < balanced.CPURequestMilli || balanced.CPURequestMilli < aggressive.CPURequestMilli || conservative.CPURequestMilli == aggressive.CPURequestMilli {
		t.Fatalf("cpu conservative=%d balanced=%d aggressive=%d", conservative.CPURequestMilli, balanced.CPURequestMilli, aggressive.CPURequestMilli)
	}
	for _, candidate := range recommendation.Candidates {
		if candidate.Status == "" || candidate.Backtest.CPURecommended.OverRequest == nil {
			t.Fatalf("%s lacks its verdict or replay: %#v", candidate.Profile, candidate)
		}
	}

	// A custom policy selects no profile, and its bounds bind every candidate.
	policy := DefaultPolicy()
	policy.CPUSafetyMargin = 0.33
	policy.MinimumCPUMilli = 900
	input.Policy = &policy
	custom, err := Recommend(input)
	if err != nil {
		t.Fatal(err)
	}
	for _, candidate := range custom.Candidates {
		if candidate.Selected || candidate.CPURequestMilli < 900 {
			t.Fatalf("custom-policy candidate=%#v", candidate)
		}
	}
}
