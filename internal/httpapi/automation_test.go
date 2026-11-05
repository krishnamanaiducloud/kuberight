package httpapi

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kuberight/kuberight/internal/automation"
	"github.com/kuberight/kuberight/internal/config"
	"github.com/kuberight/kuberight/internal/optimizer"
	"github.com/kuberight/kuberight/internal/repository"
)

// automationInventory is a write-capable fake provider.
type automationInventory struct {
	fakeInventory
	mu        *sync.Mutex
	enabled   bool
	patchErr  error
	patches   *[]automation.Change
	namespace error
}

func (inventory automationInventory) NamespaceAutomationEnabled(context.Context, string) (bool, error) {
	return inventory.enabled, inventory.namespace
}

func (inventory automationInventory) PatchResources(_ context.Context, change automation.Change) error {
	inventory.mu.Lock()
	defer inventory.mu.Unlock()
	*inventory.patches = append(*inventory.patches, change)
	return inventory.patchErr
}

// automationSpec is the live workload a fresh recommendation was computed
// against: current resources 1500m/2500m CPU and 3Gi/4Gi memory.
func automationSpec() WorkloadSpec {
	spec := checkoutSpec()
	spec.Generation, spec.ResourceVersion, spec.Replicas = 12, "9001", 2
	spec.Containers = []ContainerSpec{{Name: "api", CPURequestMilli: 1500, CPULimitMilli: 2500, MemoryRequestBytes: 3 << 30, MemoryLimitBytes: 4 << 30}}
	spec.Annotations, spec.Labels = map[string]string{}, map[string]string{}
	return spec
}

func automationRecommendation() optimizer.Recommendation {
	recommendation := liveRecommendation()
	recommendation.Assessment = &optimizer.Assessment{Status: optimizer.StatusRecommend}
	return recommendation
}

type automationHarness struct {
	api       testAPI
	inventory automationInventory
	patches   *[]automation.Change
}

func newAutomationHarness(t *testing.T, mode string, spec WorkloadSpec, mutate func(*config.Config)) automationHarness {
	t.Helper()
	patches := &[]automation.Change{}
	inventory := automationInventory{fakeInventory: fakeInventory{spec: spec}, mu: &sync.Mutex{}, enabled: true, patches: patches}
	api := newTestAPI(t, func(cfg *config.Config) {
		cfg.OperationMode, cfg.AuthMode, cfg.APIToken = mode, "kubernetes", ""
		if mutate != nil {
			mutate(cfg)
		}
	}, Dependencies{Provider: inventory})
	return automationHarness{api: api, inventory: inventory, patches: patches}
}

func decisionFor(t *testing.T, body map[string]any) map[string]any {
	t.Helper()
	decisions := body["decisions"].([]any)
	if len(decisions) != 1 {
		t.Fatalf("decisions = %v", body["decisions"])
	}
	return decisions[0].(map[string]any)
}

func TestAutomationPlansWithoutWritingOutsideApplyMode(t *testing.T) {
	harness := newAutomationHarness(t, config.ModeRecommend, automationSpec(), nil)
	storeApproved(t, harness.api, automationRecommendation(), fixedNow.Add(-time.Hour))
	response, body := harness.api.do(t, http.MethodGet, "/api/v1/automation", "alice", "")
	if response.Code != http.StatusOK || body["dryRun"] != true || body["enabled"] != false {
		t.Fatalf("status=%d body=%v", response.Code, body)
	}
	decision := decisionFor(t, body)
	if decision["action"] != AutomationApply || decision["outcome"] != AutomationPlanned || len(*harness.patches) != 0 {
		t.Fatalf("decision=%v patches=%v", decision, *harness.patches)
	}
	if response, body := harness.api.do(t, http.MethodPost, "/api/v1/automation:run", "alice", ""); response.Code != http.StatusConflict || body["error"].(map[string]any)["code"] != "automation_disabled" {
		t.Fatalf("run outside apply mode: status=%d body=%v", response.Code, body)
	}
	if !harness.api.access.checked("list", "automation") || !harness.api.access.checked("update", "automation") {
		t.Fatal("automation endpoints must be authorized against the automation resource")
	}
}

func TestAutomationAppliesWithPreconditionAuditAndRecordsPrevious(t *testing.T) {
	harness := newAutomationHarness(t, config.ModeApply, automationSpec(), nil)
	record := storeApproved(t, harness.api, automationRecommendation(), fixedNow.Add(-time.Hour))
	response, body := harness.api.do(t, http.MethodPost, "/api/v1/automation:run", "alice", "")
	if response.Code != http.StatusOK || body["applied"].(float64) != 1 {
		t.Fatalf("status=%d body=%v", response.Code, body)
	}
	if len(*harness.patches) != 1 {
		t.Fatalf("patches = %v", *harness.patches)
	}
	change := (*harness.patches)[0]
	if change.ResourceVersion != "9001" || change.RecommendationID != record.ID || change.Desired.CPURequestMilli != record.CPURequestMilli ||
		change.Previous != (automation.Resources{CPURequestMilli: 1500, CPULimitMilli: 2500, MemoryRequestBytes: 3 << 30, MemoryLimitBytes: 4 << 30}) {
		t.Fatalf("change = %+v", change)
	}
	events, err := harness.api.store.ListAuditEvents(context.Background(), 1, 20)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, event := range events.Items {
		found = found || (event.Action == "automation.apply" && event.Actor == automationActor && event.Outcome == "success" && strings.HasSuffix(event.Resource, record.ID))
	}
	if !found {
		t.Fatalf("no audit event for the automated change: %+v", events.Items)
	}
}

func TestAutomationRefusesUnsafeOrUnauthorizedChanges(t *testing.T) {
	cases := map[string]struct {
		spec      func(WorkloadSpec) WorkloadSpec
		record    func(optimizer.Recommendation) optimizer.Recommendation
		inventory func(*automationInventory)
		reason    string
	}{
		"namespace not opted in": {inventory: func(inventory *automationInventory) { inventory.enabled = false }, reason: "is not labeled kuberight.io/automation=enabled"},
		"namespace unreadable":   {inventory: func(inventory *automationInventory) { inventory.namespace = errors.New("forbidden") }, reason: "opt-in could not be read"},
		"workload opted out": {spec: func(spec WorkloadSpec) WorkloadSpec {
			spec.Annotations = map[string]string{automation.WorkloadAnnotation: "disabled"}
			return spec
		}, reason: "annotated kuberight.io/automation=disabled"},
		"Argo CD managed": {spec: func(spec WorkloadSpec) WorkloadSpec {
			spec.Annotations = map[string]string{"argocd.argoproj.io/tracking-id": "shop:apps/Deployment:shop/checkout"}
			return spec
		}, reason: "Argo CD manages the workload"},
		"spec changed": {spec: func(spec WorkloadSpec) WorkloadSpec { spec.Generation = 13; return spec }, reason: "spec changed after the recommendation"},
		"recreated":    {spec: func(spec WorkloadSpec) WorkloadSpec { spec.UID = "uid-new"; return spec }, reason: "recreated or moved"},
		"already live": {spec: func(spec WorkloadSpec) WorkloadSpec {
			record := automationRecommendation()
			spec.Containers[0].CPURequestMilli, spec.Containers[0].CPULimitMilli = record.CPURequestMilli, record.CPULimitMilli
			spec.Containers[0].MemoryRequestBytes, spec.Containers[0].MemoryLimitBytes = record.MemoryRequestBytes, record.MemoryLimitBytes
			return spec
		}, reason: "already live"},
		"live drifted": {spec: func(spec WorkloadSpec) WorkloadSpec { spec.Containers[0].CPURequestMilli = 1800; return spec }, reason: "differ from the values the recommendation was calculated against"},
		"held verdict": {record: func(recommendation optimizer.Recommendation) optimizer.Recommendation {
			recommendation.Assessment = &optimizer.Assessment{Status: optimizer.StatusHold, StatusReasons: []string{"held-back test failed"}}
			return recommendation
		}, reason: "status is hold"},
		"expired": {record: func(recommendation optimizer.Recommendation) optimizer.Recommendation {
			expired := fixedNow.Add(-time.Minute)
			recommendation.Provenance.ExpiresAt = &expired
			return recommendation
		}, reason: "evidence has expired"},
		"incomplete replicas": {record: func(recommendation optimizer.Recommendation) optimizer.Recommendation {
			recommendation.DataQuality.ReplicaCoverage.Complete = false
			return recommendation
		}, reason: "did not cover every replica"},
		"VPA managed": {spec: func(spec WorkloadSpec) WorkloadSpec { spec.Containers[0].VPAControlsMemory = true; return spec }, reason: "VerticalPodAutoscaler"},
		"unsupported kind": {record: func(recommendation optimizer.Recommendation) optimizer.Recommendation {
			recommendation.Kind = "CronJob"
			return recommendation
		}, reason: "advisory only"},
	}
	for name, test := range cases {
		t.Run(name, func(t *testing.T) {
			spec := automationSpec()
			if test.spec != nil {
				spec = test.spec(spec)
			}
			harness := newAutomationHarness(t, config.ModeApply, spec, nil)
			if test.inventory != nil {
				test.inventory(&harness.inventory)
				// The provider dependency is captured by value at construction.
				harness = rebuildAutomation(t, harness)
			}
			recommendation := automationRecommendation()
			if test.record != nil {
				recommendation = test.record(recommendation)
			}
			storeApproved(t, harness.api, recommendation, fixedNow.Add(-time.Hour))
			_, body := harness.api.do(t, http.MethodPost, "/api/v1/automation:run", "alice", "")
			decision := decisionFor(t, body)
			if decision["action"] != AutomationSkip || len(*harness.patches) != 0 || !strings.Contains(strings.Join(stringsOf(decision["reasons"]), "; "), test.reason) {
				t.Fatalf("decision=%v patches=%v", decision, *harness.patches)
			}
		})
	}
}

func rebuildAutomation(t *testing.T, harness automationHarness) automationHarness {
	t.Helper()
	api := newTestAPI(t, func(cfg *config.Config) {
		cfg.OperationMode, cfg.AuthMode, cfg.APIToken = config.ModeApply, "kubernetes", ""
	}, Dependencies{Provider: harness.inventory})
	return automationHarness{api: api, inventory: harness.inventory, patches: harness.patches}
}

func stringsOf(value any) []string {
	items, _ := value.([]any)
	result := make([]string, 0, len(items))
	for _, item := range items {
		result = append(result, item.(string))
	}
	return result
}

func TestAutomationDefersOutsideWindowAndBeyondTheRunLimit(t *testing.T) {
	closed := fixedNow.UTC().Add(time.Hour).Format("15:04") + "-" + fixedNow.UTC().Add(2*time.Hour).Format("15:04")
	harness := newAutomationHarness(t, config.ModeApply, automationSpec(), func(cfg *config.Config) { cfg.AutomationWindow = closed })
	storeApproved(t, harness.api, automationRecommendation(), fixedNow.Add(-time.Hour))
	_, body := harness.api.do(t, http.MethodPost, "/api/v1/automation:run", "alice", "")
	if decision := decisionFor(t, body); decision["outcome"] != AutomationDeferred || body["windowOpen"] != false || len(*harness.patches) != 0 {
		t.Fatalf("closed window: body=%v patches=%v", body, *harness.patches)
	}
}

func TestAutomationRollsBackARegressedAutomatedChange(t *testing.T) {
	recommendation := automationRecommendation()
	spec := automationSpec()
	// The automated change is live and annotated with its recommendation.
	spec.Containers[0].CPURequestMilli, spec.Containers[0].CPULimitMilli = recommendation.CPURequestMilli, recommendation.CPULimitMilli
	spec.Containers[0].MemoryRequestBytes, spec.Containers[0].MemoryLimitBytes = recommendation.MemoryRequestBytes, recommendation.MemoryLimitBytes
	spec.Generation, spec.ResourceVersion = 13, "9100"
	harness := newAutomationHarness(t, config.ModeApply, spec, nil)
	record := storeApproved(t, harness.api, recommendation, fixedNow.Add(-3*time.Hour))
	harness.inventory.spec.Annotations = map[string]string{automation.AppliedAnnotation: record.ID}
	harness = rebuildAutomation(t, harness)
	// The rebuilt API has a new store; seed the record and its regressed outcome there.
	record = storeApproved(t, harness.api, recommendation, fixedNow.Add(-3*time.Hour))
	harness.inventory.spec.Annotations[automation.AppliedAnnotation] = record.ID
	harness = automationHarness{api: harness.api, inventory: harness.inventory, patches: harness.patches}
	first := fixedNow.Add(-2 * time.Hour)
	if _, err := harness.api.store.RecordVerification(context.Background(), repository.VerificationRun{
		RecommendationID: record.ID, Status: repository.VerificationRegressed, ConfigurationObserved: true, FirstObservedAt: &first,
		ObservedGeneration: 13, ObservedSpecFingerprint: strings.Repeat("b", 64), EvidenceComplete: true, Attribution: repository.VerificationAttributionExactPodUID,
		Checks: []repository.VerificationCheck{}, Reasons: []string{"2 OOM kills after the change"}, CheckedAt: fixedNow,
	}); err != nil {
		t.Fatal(err)
	}
	_, body := harness.api.do(t, http.MethodPost, "/api/v1/automation:run", "alice", "")
	decision := decisionFor(t, body)
	if decision["action"] != AutomationRollback || decision["outcome"] != AutomationRolledBack || body["rolledBack"].(float64) != 1 {
		t.Fatalf("body=%v", body)
	}
	change := (*harness.patches)[0]
	if !change.Rollback || change.Desired != (automation.Resources{CPURequestMilli: 1500, CPULimitMilli: 2500, MemoryRequestBytes: 3 << 30, MemoryLimitBytes: 4 << 30}) || change.ResourceVersion != "9100" {
		t.Fatalf("rollback change = %+v", change)
	}
}

func TestAutomationRecordsPatchFailures(t *testing.T) {
	harness := newAutomationHarness(t, config.ModeApply, automationSpec(), nil)
	harness.inventory.patchErr = errors.New("conflict")
	harness = rebuildAutomation(t, harness)
	storeApproved(t, harness.api, automationRecommendation(), fixedNow.Add(-time.Hour))
	_, body := harness.api.do(t, http.MethodPost, "/api/v1/automation:run", "alice", "")
	if decision := decisionFor(t, body); decision["outcome"] != AutomationFailed || body["failed"].(float64) != 1 || decision["error"] != "conflict" {
		t.Fatalf("body=%v", body)
	}
}

func TestNewAutomatorRequiresApplyModeAndAWriteCapableProvider(t *testing.T) {
	cfg := fallbackConfig()
	if _, err := NewAutomator(cfg, Dependencies{Repository: repository.NewMemory(10), Access: &recordingAccess{}}); err == nil {
		t.Fatal("automation started outside apply mode")
	}
	cfg.OperationMode, cfg.AuthMode, cfg.APIToken = config.ModeApply, "kubernetes", ""
	if _, err := NewAutomator(cfg, Dependencies{Repository: repository.NewMemory(10), Access: &recordingAccess{}, Provider: fakeInventory{spec: automationSpec()}}); err == nil {
		t.Fatal("automation started with a read-only provider")
	}
}

// checked reports whether an authorization check was made for the verb and
// resource.
func (access *recordingAccess) checked(verb, resource string) bool {
	access.mu.Lock()
	defer access.mu.Unlock()
	for _, call := range access.calls {
		if call.Verb == verb && call.Resource == resource {
			return true
		}
	}
	return false
}
