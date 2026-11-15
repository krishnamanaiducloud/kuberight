package httpapi

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kuberight/kuberight/internal/automation"
	"github.com/kuberight/kuberight/internal/config"
	"github.com/kuberight/kuberight/internal/repository"
	"github.com/kuberight/kuberight/internal/usage"
)

type retryCase struct {
	// rolledBack leaves the template on the previous values, as after an
	// automated rollback; otherwise it carries the approved target.
	rolledBack bool
	// stale is the number of the two running pods on the other values.
	stale    int
	mutate   func(*WorkloadSpec, *resizingInventory)
	config   func(*config.Config)
	unmarked bool
}

// runRetry evaluates one approved recommendation against an OnDelete
// workload whose template KubeRight already changed.
func runRetry(t *testing.T, test retryCase) (map[string]any, *[]automation.Change, *[]automation.Change, testAPI, repository.RecommendationRecord) {
	t.Helper()
	recommendation := automationRecommendation()
	target := automation.Resources{CPURequestMilli: recommendation.CPURequestMilli, CPULimitMilli: recommendation.CPULimitMilli, MemoryRequestBytes: recommendation.MemoryRequestBytes, MemoryLimitBytes: recommendation.MemoryLimitBytes}
	previous := automation.Resources{CPURequestMilli: 1500, CPULimitMilli: 2500, MemoryRequestBytes: 3 << 30, MemoryLimitBytes: 4 << 30}
	template, other := target, previous
	if test.rolledBack {
		template, other = previous, target
	}
	pod := func(resources automation.Resources) map[string]usage.ContainerResources {
		return map[string]usage.ContainerResources{"api": {CPURequestMilli: resources.CPURequestMilli, CPULimitMilli: resources.CPULimitMilli, MemoryRequestBytes: resources.MemoryRequestBytes, MemoryLimitBytes: resources.MemoryLimitBytes}}
	}
	spec := automationSpec()
	spec.Kind, spec.OnDelete = "StatefulSet", true
	spec.Containers = []ContainerSpec{{Name: "api", CPURequestMilli: template.CPURequestMilli, CPULimitMilli: template.CPULimitMilli, MemoryRequestBytes: template.MemoryRequestBytes, MemoryLimitBytes: template.MemoryLimitBytes}}
	spec.MetricIdentity = recommendationIdentity(2)
	for index := range spec.MetricIdentity.Pods {
		spec.MetricIdentity.Pods[index].Resources = pod(template)
		if index < test.stale {
			spec.MetricIdentity.Pods[index].Resources = pod(other)
		}
	}
	patches, resizes := &[]automation.Change{}, &[]automation.Change{}
	inventory := resizingInventory{
		automationInventory: automationInventory{fakeInventory: fakeInventory{spec: spec}, mu: &sync.Mutex{}, enabled: true, patches: patches},
		result:              automation.ResizeResult{Attempted: true, Pods: 2, Resized: 2},
		resizes:             resizes,
	}
	if test.mutate != nil {
		test.mutate(&spec, &inventory)
		inventory.spec = spec
	}
	api := newTestAPI(t, func(cfg *config.Config) {
		cfg.OperationMode, cfg.AuthMode, cfg.APIToken, cfg.AutomationInPlaceResize = config.ModeApply, "kubernetes", "", true
		if test.config != nil {
			test.config(cfg)
		}
	}, Dependencies{Provider: inventory})
	recommendation.Kind = spec.Kind
	record := storeApproved(t, api, recommendation, fixedNow.Add(-time.Hour))
	if !test.unmarked {
		// The annotations map is shared with the inventory's copy of the spec.
		spec.Annotations[automation.AppliedAnnotation] = record.ID
	}
	response, body := api.do(t, http.MethodPost, "/api/v1/automation:run", "alice", "")
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%v", response.Code, body)
	}
	return body, patches, resizes, api, record
}

func TestAutomationRetriesAnUnfinishedInPlaceResize(t *testing.T) {
	body, patches, resizes, api, record := runRetry(t, retryCase{stale: 1})
	decision := decisionFor(t, body)
	if decision["action"] != AutomationApply || decision["outcome"] != AutomationApplied || body["applied"].(float64) != 1 {
		t.Fatalf("decision=%v body=%v", decision, body)
	}
	if len(*patches) != 0 || len(*resizes) != 1 {
		t.Fatalf("a retry must resize pods without patching the template: patches=%v resizes=%v", *patches, *resizes)
	}
	change := (*resizes)[0]
	if change.Rollback || change.Desired.CPURequestMilli != record.CPURequestMilli || change.Previous.CPURequestMilli != 1500 || change.RecommendationID != record.ID {
		t.Fatalf("change = %+v", change)
	}
	if reasons := strings.Join(stringsOf(decision["reasons"]), "; "); !strings.Contains(reasons, "1 of 2 running pods still run others") {
		t.Fatalf("reasons = %s", reasons)
	}
	events, err := api.store.ListAuditEvents(context.Background(), 1, 20)
	if err != nil {
		t.Fatal(err)
	}
	audited := false
	for _, event := range events.Items {
		audited = audited || (event.Action == "automation.resize" && event.Outcome == "success" && strings.HasSuffix(event.Resource, record.ID))
		if event.Action == "automation.apply" {
			t.Fatalf("a retry was audited as a new change: %+v", event)
		}
	}
	if !audited {
		t.Fatalf("the retry was not audited: %+v", events.Items)
	}
}

func TestAutomationFinishesARollbackOutsideTheChangeWindow(t *testing.T) {
	closed := func(cfg *config.Config) {
		cfg.AutomationWindow = fixedNow.Add(2*time.Hour).Format("15:04") + "-" + fixedNow.Add(3*time.Hour).Format("15:04")
	}
	body, patches, resizes, _, _ := runRetry(t, retryCase{rolledBack: true, stale: 2, config: closed})
	decision := decisionFor(t, body)
	if body["windowOpen"] != false || decision["action"] != AutomationRollback || decision["outcome"] != AutomationRolledBack || len(*patches) != 0 || len(*resizes) != 1 {
		t.Fatalf("decision=%v body=%v patches=%v", decision, body, *patches)
	}
	if change := (*resizes)[0]; !change.Rollback || change.Desired.CPURequestMilli != 1500 {
		t.Fatalf("change = %+v", change)
	}

	// The same retry toward the approved target waits for the window.
	body, _, resizes, _, _ = runRetry(t, retryCase{stale: 1, config: closed})
	if decision := decisionFor(t, body); decision["outcome"] != AutomationDeferred || len(*resizes) != 0 {
		t.Fatalf("an apply retry ignored the change window: %v", decision)
	}
}

func TestAutomationDoesNotRetryWithoutCause(t *testing.T) {
	for name, test := range map[string]retryCase{
		"every pod runs the template":       {stale: 0},
		"the option is off":                 {stale: 1, config: func(cfg *config.Config) { cfg.AutomationInPlaceResize = false }},
		"the workload rolls out by itself":  {stale: 1, mutate: func(spec *WorkloadSpec, _ *resizingInventory) { spec.OnDelete = false }},
		"KubeRight did not make the change": {stale: 1, unmarked: true},
		"the workload opted out": {stale: 1, mutate: func(spec *WorkloadSpec, _ *resizingInventory) {
			spec.Annotations[automation.WorkloadAnnotation] = "disabled"
		}},
		"the namespace opted out": {stale: 1, mutate: func(_ *WorkloadSpec, inventory *resizingInventory) { inventory.enabled = false }},
		"a stale pod is not running": {stale: 1, mutate: func(spec *WorkloadSpec, _ *resizingInventory) {
			spec.MetricIdentity.Pods[0].Phase = "Pending"
		}},
	} {
		body, patches, resizes, _, _ := runRetry(t, test)
		if decision := decisionFor(t, body); decision["action"] != AutomationSkip || len(*patches) != 0 || len(*resizes) != 0 {
			t.Fatalf("%s: decision=%v patches=%v resizes=%v", name, decision, *patches, *resizes)
		}
	}
}

func TestAutomationReportsAFailedOrInapplicableRetry(t *testing.T) {
	body, _, _, _, _ := runRetry(t, retryCase{stale: 1, mutate: func(_ *WorkloadSpec, inventory *resizingInventory) {
		inventory.result = automation.ResizeResult{Attempted: true, Pods: 2, Resized: 1, Failed: 1, Error: "pod db-1: rejected"}
	}})
	decision := decisionFor(t, body)
	if decision["outcome"] != AutomationFailed || body["failed"].(float64) != 1 || !strings.Contains(decision["error"].(string), "1 of 2 running pods") {
		t.Fatalf("failed retry: decision=%v body=%v", decision, body)
	}

	body, _, resizes, api, _ := runRetry(t, retryCase{stale: 1, mutate: func(_ *WorkloadSpec, inventory *resizingInventory) {
		inventory.result = automation.ResizeResult{Reason: "the container resize policy restarts it"}
	}})
	decision = decisionFor(t, body)
	if decision["action"] != AutomationSkip || decision["outcome"] != nil || body["eligible"].(float64) != 0 || body["failed"].(float64) != 0 || len(*resizes) != 1 ||
		!strings.Contains(strings.Join(stringsOf(decision["reasons"]), "; "), "resize policy restarts") {
		t.Fatalf("inapplicable retry: decision=%v body=%v", decision, body)
	}
	events, err := api.store.ListAuditEvents(context.Background(), 1, 20)
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range events.Items {
		if strings.HasPrefix(event.Action, "automation.") {
			t.Fatalf("a retry that changed nothing was audited: %+v", event)
		}
	}
}
