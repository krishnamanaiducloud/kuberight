package optimizer

import (
	"math"
	"strings"
	"testing"
	"time"
)

func steadyInput() Input {
	input := baseInput()
	input.CPUSamplesMilli = samples(600, 300)
	input.MemorySamplesBytes = samples(600, 400*1024*1024)
	input.SampleIntervalSeconds = 300
	input.Replicas = 3
	return input
}

func mustRecommend(t *testing.T, input Input) Recommendation {
	t.Helper()
	recommendation, err := Recommend(input)
	if err != nil {
		t.Fatal(err)
	}
	if recommendation.Assessment == nil {
		t.Fatal("assessment missing")
	}
	return recommendation
}

func checkStatus(t *testing.T, assessment *Assessment, prefix, want string) Check {
	t.Helper()
	for _, check := range assessment.Checks {
		if strings.HasPrefix(check.Name, prefix) {
			if check.Status != want {
				t.Fatalf("%s: %s (%s), want %s", check.Name, check.Status, check.Detail, want)
			}
			return check
		}
	}
	t.Fatalf("check %s missing: %+v", prefix, assessment.Checks)
	return Check{}
}

func TestQuantities(t *testing.T) {
	for millicores, want := range map[int64]string{0: "0", 250: "250m", 1000: "1", 1500: "1500m", 4000: "4"} {
		if got := CPUQuantity(millicores); got != want {
			t.Fatalf("CPUQuantity(%d) = %s", millicores, got)
		}
	}
	for bytes, want := range map[int64]string{0: "0", 512 * mebibyte: "512Mi", 2 * gibibyte: "2Gi", 1536: "1536", 2048: "2Ki", 1024 * gibibyte: "1Ti"} {
		if got := MemoryQuantity(bytes); got != want {
			t.Fatalf("MemoryQuantity(%d) = %s", bytes, got)
		}
	}
	if RoundCPU(999) != 999 || RoundCPU(1001) != 1005 || RoundMemory(1) != mebibyte || RoundMemory(0) != 0 {
		t.Fatal("rounding must only round up")
	}
	for text, want := range map[string]int64{"250m": 250, "0.5": 500, "2": 2000, "0.0001": 1, "1.5m": 2} {
		if got, err := ParseCPUQuantity(text); err != nil || got != want {
			t.Fatalf("ParseCPUQuantity(%s) = %d, %v", text, got, err)
		}
	}
	for text, want := range map[string]int64{"512Mi": 512 * mebibyte, "1Gi": gibibyte, "1G": 1e9, "100": 100, "1.5Ki": 1536, "2k": 2000} {
		if got, err := ParseMemoryQuantity(text); err != nil || got != want {
			t.Fatalf("ParseMemoryQuantity(%s) = %d, %v", text, got, err)
		}
	}
	for _, bad := range []string{"", "-1", "abc", "1e400"} {
		if _, err := ParseCPUQuantity(bad); err == nil {
			t.Fatalf("CPU %q accepted", bad)
		}
		if _, err := ParseMemoryQuantity(bad); err == nil {
			t.Fatalf("memory %q accepted", bad)
		}
	}
	if _, err := ParseCPUQuantity("-5m"); err == nil {
		t.Fatal("negative millicores accepted")
	}
	got := quantities(1500, 0, 512*mebibyte, gibibyte)
	if got.CPURequest != "1500m" || got.CPULimit != "" || got.MemoryRequest != "512Mi" || got.MemoryLimit != "1Gi" {
		t.Fatalf("quantities %+v", got)
	}
}

func TestSteadyWorkloadIsRecommendedWithEvidence(t *testing.T) {
	recommendation := mustRecommend(t, steadyInput())
	assessment := recommendation.Assessment
	if assessment.Status != StatusRecommend || len(assessment.StatusReasons) != 0 {
		t.Fatalf("status %s %v", assessment.Status, assessment.StatusReasons)
	}
	if ok, reason := recommendation.Approvable(); !ok {
		t.Fatalf("approvable: %s", reason)
	}
	if recommendation.CPURequestMilli%5 != 0 && recommendation.CPURequestMilli > 1000 {
		t.Fatal("CPU above one core rounds to 5 millicores")
	}
	if recommendation.MemoryRequestBytes%mebibyte != 0 || assessment.Recommended.MemoryRequest != MemoryQuantity(recommendation.MemoryRequestBytes) {
		t.Fatalf("memory must be whole mebibytes: %d %+v", recommendation.MemoryRequestBytes, assessment.Recommended)
	}
	if assessment.Current.CPURequest != "1" || assessment.Current.MemoryLimit != "2Gi" {
		t.Fatalf("current quantities %+v", assessment.Current)
	}
	if len(assessment.Validation) != 3 || !assessment.OutOfSample.Performed || !assessment.OutOfSample.Passed {
		t.Fatalf("validation %+v %+v", assessment.Validation, assessment.OutOfSample)
	}
	if assessment.SafetyMargins.CPU.Total != assessment.SafetyMargins.CPU.Base {
		t.Fatalf("steady usage needs no extra margin: %+v", assessment.SafetyMargins.CPU)
	}
	if assessment.ValidFor != (24*time.Hour).String() || assessment.ValidForSeconds != 86_400 {
		t.Fatalf("validity %s", assessment.ValidFor)
	}
	if assessment.ExpectedSavings.CPUMilli != (1000-recommendation.CPURequestMilli)*3 {
		t.Fatalf("savings must cover all replicas: %+v", assessment.ExpectedSavings)
	}
	if workload := assessment.Workload; workload == nil || !workload.Approximate || workload.TotalRecommendedCPUMilli != recommendation.CPURequestMilli*3 {
		t.Fatalf("workload totals %+v", assessment.Workload)
	}
	for _, name := range []string{"LimitRange", "ResourceQuota", "Scheduling"} {
		checkStatus(t, assessment, name, CheckUnknown)
	}
	if assessment.RiskScore < 0 || assessment.RiskScore > 1 || len(assessment.Explanation) < 2 {
		t.Fatalf("risk %v explanation %v", assessment.RiskScore, assessment.Explanation)
	}
	if recommendation.Provenance.OptimizerVersion != "kuberight-optimizer/v2" {
		t.Fatal("algorithm version must change with the new semantics")
	}
}

func TestDynamicMarginGrowsWithRisk(t *testing.T) {
	input := steadyInput()
	for index := range input.CPUSamplesMilli {
		input.CPUSamplesMilli[index] = 100 + float64(index%10)*60
	}
	input.Criticality = "high"
	input.Kind = "StatefulSet"
	margin := mustRecommend(t, input).Assessment.SafetyMargins.CPU
	if margin.Variability <= 0 || margin.Criticality != 0.15 || margin.Total <= margin.Base {
		t.Fatalf("margin %+v", margin)
	}
	capped := dynamicMargin(0.3, []float64{0, 0, 0, 0, 0, 0, 0, 1000}, true, 5, Input{})
	if capped.Variability != 0.25 || capped.Uncertainty != 0.2 || capped.Burst != 0.1 {
		t.Fatalf("components are capped: %+v", capped)
	}
	if forecastWidth(0, 10) != 0 || forecastWidth(10, 5) != 0 || forecastWidth(10, 15) != 0.5 {
		t.Fatal("forecast width")
	}
}

func TestStatusesForUnsafeSizing(t *testing.T) {
	sparse := steadyInput()
	sparse.CPUSamplesMilli, sparse.MemorySamplesBytes = samples(10, 300), samples(10, 1e8)
	if assessment := mustRecommend(t, sparse).Assessment; assessment.Status != StatusInsufficientData || len(assessment.StatusReasons) == 0 {
		t.Fatalf("sparse: %+v", assessment.Status)
	}

	vpa := steadyInput()
	vpa.Autoscaling.VPAControlsCPU, vpa.Autoscaling.VPAControlsMemory = true, true
	recommendation := mustRecommend(t, vpa)
	if recommendation.Assessment.Status != StatusUnsupported {
		t.Fatalf("VPA: %s", recommendation.Assessment.Status)
	}
	if ok, reason := recommendation.Approvable(); ok || !strings.Contains(reason, "unsupported") {
		t.Fatalf("an unsupported recommendation must not be approvable: %s", reason)
	}
	if recommendation.CPURequestMilli != vpa.CPURequestsMilli || recommendation.MemoryRequestBytes != vpa.MemoryRequestsBytes {
		t.Fatal("VPA-managed values must stay exactly as they are")
	}

	// Usage that doubles in the most recent part of the window is caught by
	// the recent-window replay and the held-back test, even though the
	// full-window statistics dilute it.
	rising := steadyInput()
	rising.CPURequestsMilli = 400
	rising.CPULimitMilli = 0
	for index := range rising.CPUSamplesMilli {
		if index >= 480 {
			rising.CPUSamplesMilli[index] = 900
		}
	}
	assessment := mustRecommend(t, rising).Assessment
	if assessment.Status != StatusHold || !assessment.OutOfSample.Performed || assessment.OutOfSample.Passed {
		t.Fatalf("rising: %s %+v", assessment.Status, assessment.OutOfSample)
	}
	if (Recommendation{}).Assessment != nil {
		t.Fatal("zero value")
	}
	if ok, _ := (Recommendation{}).Approvable(); !ok {
		t.Fatal("legacy recommendations without an assessment stay approvable")
	}
}

func TestQoSProtectionAndClassification(t *testing.T) {
	input := steadyInput()
	input.CPULimitMilli, input.MemoryLimitBytes = input.CPURequestsMilli, input.MemoryRequestsBytes
	// Rare spikes size the CPU limit above the request, which protection
	// must reconcile.
	for index := range input.CPUSamplesMilli {
		if index%100 == 0 {
			input.CPUSamplesMilli[index] = 900
		}
	}
	recommendation := mustRecommend(t, input)
	qos := recommendation.Assessment.QoS
	if qos.Current != QoSGuaranteed || qos.Recommended != QoSGuaranteed || !qos.Protected {
		t.Fatalf("qos %+v", qos)
	}
	if recommendation.CPURequestMilli != recommendation.CPULimitMilli || recommendation.MemoryRequestBytes != recommendation.MemoryLimitBytes {
		t.Fatalf("Guaranteed pods keep requests equal to limits: %+v", recommendation)
	}
	containers := []ContainerResources{{Name: "a", CPURequestMilli: 100, CPULimitMilli: 100, MemoryRequestBytes: 10, MemoryLimitBytes: 10}, {Name: "b"}}
	if podQoS(containers) != QoSBurstable || podQoS([]ContainerResources{{Name: "x"}}) != QoSBestEffort {
		t.Fatal("classification")
	}
	// Limits alone imply equal requests.
	if podQoS([]ContainerResources{{Name: "x", CPULimitMilli: 100, MemoryLimitBytes: 10}}) != QoSGuaranteed {
		t.Fatal("limits without requests are Guaranteed")
	}
	// A sidecar without limits makes the whole pod Burstable.
	input.Pod = &PodContext{
		Containers:     []ContainerResources{{Name: "app"}},
		InitContainers: []InitContainer{{ContainerResources: ContainerResources{Name: "proxy", CPURequestMilli: 50}, Sidecar: true}},
	}
	if qos := mustRecommend(t, input).Assessment.QoS; qos.Current != QoSBurstable || qos.Protected || qos.Approximate {
		t.Fatalf("pod with an unlimited sidecar: %+v", qos)
	}
}

func TestEffectivePodRequestsCountInitAndSidecarsOnce(t *testing.T) {
	input := steadyInput()
	input.Pod = &PodContext{
		Containers: []ContainerResources{{Name: "app", CPURequestMilli: 1000, MemoryRequestBytes: gibibyte}, {Name: "worker", CPURequestMilli: 200, MemoryRequestBytes: 100 * mebibyte}},
		InitContainers: []InitContainer{
			{ContainerResources: ContainerResources{Name: "migrate", CPURequestMilli: 3000, MemoryRequestBytes: 64 * mebibyte}},
			{ContainerResources: ContainerResources{Name: "proxy", CPURequestMilli: 100, MemoryRequestBytes: 50 * mebibyte}, Sidecar: true},
			{ContainerResources: ContainerResources{Name: "warm", CPURequestMilli: 500, MemoryRequestBytes: 2 * gibibyte}},
		},
		OverheadCPUMilli: 10,
	}
	cpu, memory := effectivePodRequests(input, ContainerResources{Name: "app", CPURequestMilli: 1000, MemoryRequestBytes: gibibyte})
	// CPU: init "migrate" (3000) beats running containers plus sidecar (1300).
	// Memory: init "warm" plus the sidecar started before it (2Gi+50Mi) wins.
	if cpu != 3010 || memory != 2*gibibyte+50*mebibyte {
		t.Fatalf("effective pod requests cpu=%d memory=%d", cpu, memory)
	}
	totals := mustRecommend(t, input).Assessment.Workload
	if totals.Approximate || totals.Replicas != 3 || totals.TotalCurrentCPUMilli != 3010*3 {
		t.Fatalf("workload totals %+v", totals)
	}
}

func TestNamespacePolicyChecks(t *testing.T) {
	input := steadyInput()
	input.NamespacePolicy = &NamespacePolicy{}
	assessment := mustRecommend(t, input).Assessment
	checkStatus(t, assessment, "LimitRange", CheckNotApplicable)
	checkStatus(t, assessment, "ResourceQuota", CheckNotApplicable)

	// A minimum above the sized value raises it; a tight ratio raises the
	// request to stay within it.
	input.NamespacePolicy = &NamespacePolicy{LimitRanges: []LimitRange{
		{Name: "floor", Type: "Container", MinCPUMilli: 900, MinMemoryBytes: gibibyte, MaxCPULimitRequestRatio: 1.1},
	}}
	recommendation := mustRecommend(t, input)
	check := checkStatus(t, recommendation.Assessment, "LimitRange floor", CheckAdjusted)
	if recommendation.CPURequestMilli < 900 || recommendation.MemoryRequestBytes < gibibyte || !strings.Contains(check.Detail, "within 1.1x") {
		t.Fatalf("adjusted %+v %s", recommendation, check.Detail)
	}
	if float64(recommendation.CPULimitMilli) > float64(recommendation.CPURequestMilli)*1.1+1 {
		t.Fatal("CPU limit-to-request ratio exceeds the LimitRange")
	}

	input.NamespacePolicy = &NamespacePolicy{LimitRanges: []LimitRange{
		{Name: "ceiling", Type: "Container", MaxCPUMilli: 100, MaxMemoryBytes: 10 * mebibyte},
		{Name: "pod", Type: "Pod", MaxCPUMilli: 100},
	}}
	assessment = mustRecommend(t, input).Assessment
	checkStatus(t, assessment, "LimitRange ceiling", CheckFail)
	checkStatus(t, assessment, "LimitRange pod", CheckFail)
	if assessment.Status != StatusHold {
		t.Fatalf("a LimitRange violation holds the change: %s", assessment.Status)
	}

	// A quota that cannot absorb an increase across all replicas fails.
	increase := steadyInput()
	increase.CPURequestsMilli = 100
	increase.CPULimitMilli = 0
	increase.NamespacePolicy = &NamespacePolicy{Quotas: []ResourceQuota{
		{Name: "team", Hard: QuotaAmounts{RequestsCPUMilli: 1000, RequestsMemoryBytes: -1, LimitsCPUMilli: -1, LimitsMemoryBytes: -1}, Used: QuotaAmounts{RequestsCPUMilli: 900}},
		{Name: "roomy", Hard: QuotaAmounts{RequestsCPUMilli: 100_000, RequestsMemoryBytes: -1, LimitsCPUMilli: -1, LimitsMemoryBytes: -1}},
	}}
	assessment = mustRecommend(t, increase).Assessment
	failed := checkStatus(t, assessment, "ResourceQuota team", CheckFail)
	checkStatus(t, assessment, "ResourceQuota roomy", CheckPass)
	if !strings.Contains(failed.Detail, "requests.cpu") || assessment.Status != StatusHold {
		t.Fatalf("quota %+v %s", failed, assessment.Status)
	}
}

func TestSchedulingFeasibility(t *testing.T) {
	input := steadyInput()
	input.Nodes = []NodeShape{{Name: "small", CPUMilli: 100, MemoryBytes: gibibyte}, {Name: "large", CPUMilli: 8000, MemoryBytes: 32 * gibibyte}}
	if check := checkStatus(t, mustRecommend(t, input).Assessment, "Scheduling", CheckPass); !strings.Contains(check.Detail, "large") {
		t.Fatal(check.Detail)
	}
	input.Nodes = []NodeShape{{Name: "tiny", CPUMilli: 100, MemoryBytes: mebibyte}}
	assessment := mustRecommend(t, input).Assessment
	if check := checkStatus(t, assessment, "Scheduling", CheckFail); !strings.Contains(check.Detail, "largest node") || assessment.Status != StatusHold {
		t.Fatalf("%s %s", check.Detail, assessment.Status)
	}
}

func TestHPAImpactProjection(t *testing.T) {
	input := steadyInput()
	input.Autoscaling.HPAUtilizationBased = true
	input.HPA = &HPAContext{MinReplicas: 2, MaxReplicas: 10, TargetCPUUtilization: 70, CurrentReplicas: 3, ContainerMetric: true}
	assessment := mustRecommend(t, input).Assessment
	impact := assessment.Autoscaling.HPA
	if impact == nil || impact.CurrentUtilization >= impact.ProjectedUtilization || impact.ProjectedReplicas < impact.CurrentDesiredReplicas || impact.Approximate {
		t.Fatalf("a smaller request raises utilization and replicas: %+v", impact)
	}
	if assessment.Status != StatusRecommend {
		t.Fatalf("within bounds: %s %v", assessment.Status, assessment.StatusReasons)
	}
	input.HPA.MaxReplicas, input.HPA.MinReplicas = 3, 1
	input.HPA.TargetCPUUtilization = 10
	assessment = mustRecommend(t, input).Assessment
	if assessment.Status != StatusHold || !strings.Contains(strings.Join(assessment.StatusReasons, " "), "above maxReplicas") {
		t.Fatalf("exceeding maxReplicas must hold: %s %v", assessment.Status, assessment.StatusReasons)
	}
	if hpaImpact(Input{HPA: &HPAContext{TargetCPUUtilization: 0}}, 1, 1) != nil {
		t.Fatal("no projection without a utilization target")
	}
	input.Autoscaling.KEDAEnabled = true
	if !strings.Contains(mustRecommend(t, input).Assessment.Autoscaling.KEDA, "CPU utilization") {
		t.Fatal("KEDA with a CPU trigger behaves like an HPA")
	}
	input.Autoscaling.HPAUtilizationBased = false
	if !strings.Contains(mustRecommend(t, input).Assessment.Autoscaling.KEDA, "event or external") {
		t.Fatal("KEDA on external metrics")
	}
}

func TestForecastAwareSizing(t *testing.T) {
	input := steadyInput()
	coverage := 0.93
	input.Forecast = &ForecastContext{Horizon: 7 * 24 * time.Hour, CPUPeakMilli: 600, CPUUpperMilli: 700, MemoryPeakBytes: 100, MemoryUpperBytes: 120, IntervalLevel: 0.95, EmpiricalCoverage: &coverage}
	recommendation := mustRecommend(t, input)
	forecast := recommendation.Assessment.Forecast
	if forecast == nil || !forecast.Calibrated || !forecast.UsedForSizing || forecast.Source != "ml" {
		t.Fatalf("forecast %+v", forecast)
	}
	if float64(recommendation.CPURequestMilli) < 600 || recommendation.Assessment.SafetyMargins.CPU.Uncertainty <= 0 {
		t.Fatalf("calibrated forecast peak must be covered: %d %+v", recommendation.CPURequestMilli, recommendation.Assessment.SafetyMargins.CPU)
	}
	// An uncalibrated forecast informs the margin but never drives sizing.
	poor := 0.6
	input.Forecast.EmpiricalCoverage = &poor
	recommendation = mustRecommend(t, input)
	if recommendation.Assessment.Forecast.UsedForSizing || recommendation.Assessment.Forecast.Calibrated {
		t.Fatalf("uncalibrated forecast: %+v", recommendation.Assessment.Forecast)
	}
	invalid := steadyInput()
	invalid.Forecast = &ForecastContext{CPUPeakMilli: math.NaN()}
	if _, err := Recommend(invalid); err == nil {
		t.Fatal("non-finite forecasts are rejected")
	}
}

func TestTrendForecastAndValidity(t *testing.T) {
	input := steadyInput()
	for index := range input.MemorySamplesBytes {
		input.MemorySamplesBytes[index] = 300*1024*1024 + float64(index)*200_000
	}
	assessment := mustRecommend(t, input).Assessment
	if assessment.Forecast == nil || assessment.Forecast.Source != "trend" || assessment.ValidFor != (12*time.Hour).String() {
		t.Fatalf("trend forecast %+v validity %s", assessment.Forecast, assessment.ValidFor)
	}
	input = steadyInput()
	input.RecentRollout = true
	if valid := mustRecommend(t, input).Assessment.ValidFor; valid != (6 * time.Hour).String() {
		t.Fatalf("recent rollout validity %s", valid)
	}
}

func TestContextValidation(t *testing.T) {
	for name, mutate := range map[string]func(*Input){
		"pod size":   func(input *Input) { input.Pod = &PodContext{Containers: make([]ContainerResources, 65)} },
		"pod values": func(input *Input) { input.Pod = &PodContext{Containers: []ContainerResources{{CPURequestMilli: -1}}} },
		"init values": func(input *Input) {
			input.Pod = &PodContext{InitContainers: []InitContainer{{ContainerResources: ContainerResources{MemoryLimitBytes: -1}}}}
		},
		"overhead": func(input *Input) { input.Pod = &PodContext{OverheadCPUMilli: -1} },
		"limitrange": func(input *Input) {
			input.NamespacePolicy = &NamespacePolicy{LimitRanges: []LimitRange{{Type: "Node"}}}
		},
		"negative range": func(input *Input) {
			input.NamespacePolicy = &NamespacePolicy{LimitRanges: []LimitRange{{Type: "Pod", MinCPUMilli: -1}}}
		},
		"many quotas":    func(input *Input) { input.NamespacePolicy = &NamespacePolicy{Quotas: make([]ResourceQuota, 33)} },
		"node":           func(input *Input) { input.Nodes = []NodeShape{{CPUMilli: -1}} },
		"hpa":            func(input *Input) { input.HPA = &HPAContext{MinReplicas: 5, MaxReplicas: 2} },
		"forecast range": func(input *Input) { input.Forecast = &ForecastContext{IntervalLevel: 2} },
		"coverage": func(input *Input) {
			bad := 1.5
			input.Forecast = &ForecastContext{EmpiricalCoverage: &bad}
		},
	} {
		input := steadyInput()
		mutate(&input)
		if _, err := Recommend(input); err == nil {
			t.Fatalf("%s: invalid context accepted", name)
		}
	}
}

func TestRiskScoreFollowsCategory(t *testing.T) {
	for _, category := range []string{"low", "medium", "high"} {
		lowest, highest := riskScore(category, 1, 0, 0), riskScore(category, 0, 10, 10)
		if lowest > highest {
			t.Fatalf("%s: %v > %v", category, lowest, highest)
		}
	}
	if riskScore("low", 1, 0, 0) != 0 || riskScore("high", 0, 10, 10) != 1 || riskScore("medium", 1, 0, 0) != 0.34 {
		t.Fatal("bands")
	}
	if windowLabel(10, 0, 1) != "all samples" || windowLabel(10, 0, 0.5) != "last 50% of samples" || roundDuration(90*time.Second) != "2m0s" {
		t.Fatal("labels")
	}
	if !strings.HasPrefix(windowLabel(1000, 300, 1), "full window (3.5d") {
		t.Fatal(windowLabel(1000, 300, 1))
	}
}
