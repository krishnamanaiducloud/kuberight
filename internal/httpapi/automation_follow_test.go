package httpapi

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/kuberight/kuberight/internal/automation"
	"github.com/kuberight/kuberight/internal/config"
	"github.com/kuberight/kuberight/internal/repository"
	"github.com/kuberight/kuberight/internal/usage"
)

// supersede stores a newer, unreviewed recommendation for the container of
// the automation test workload, as a scheduled scan or a user would.
func supersede(t *testing.T, api testAPI, kind string) {
	t.Helper()
	recommendation := automationRecommendation()
	recommendation.Kind = kind
	if _, _, err := api.store.CreateRecommendation(context.Background(), recommendation, repository.CreateOptions{CreatedBy: "alice", Source: repository.SourcePrometheus}); err != nil {
		t.Fatal(err)
	}
	latest, err := api.store.ListRecommendations(context.Background(), repository.ListOptions{Page: 1, PageSize: 10, ReviewStatus: repository.ReviewApproved, LatestOnly: true})
	if err != nil || len(latest.Items) != 0 {
		t.Fatalf("the approved change is still the latest: %+v err=%v", latest.Items, err)
	}
}

func marks(t *testing.T, api testAPI) []repository.ResizeState {
	t.Helper()
	states, err := api.store.ListResizeStates(context.Background(), 100)
	if err != nil {
		t.Fatal(err)
	}
	return states
}

func TestAutomationFinishesAResizeAfterANewerRecommendation(t *testing.T) {
	// One stage has run; one pod is left. Then a newer recommendation for
	// the same container becomes the latest one.
	body, patches, resizes, api, record := runRetry(t, retryCase{stale: 1})
	if decision := decisionFor(t, body); decision["outcome"] != AutomationApplied || len(*resizes) != 1 {
		t.Fatalf("decision=%v", decision)
	}
	if states := marks(t, api); len(states) != 1 || states[0].RecommendationID != record.ID {
		t.Fatalf("the unfinished resize is not marked: %+v", states)
	}
	supersede(t, api, "StatefulSet")

	// The plan still shows the remaining pod, and the pass resizes it.
	_, plan := api.do(t, http.MethodGet, "/api/v1/automation", "alice", "")
	if decision := decisionFor(t, plan); decision["recommendationId"] != record.ID || decision["action"] != AutomationApply || decision["outcome"] != AutomationPlanned {
		t.Fatalf("plan = %v", decision)
	}
	_, body = api.do(t, http.MethodPost, "/api/v1/automation:run", "alice", "")
	decision := decisionFor(t, body)
	if decision["recommendationId"] != record.ID || decision["action"] != AutomationApply || decision["outcome"] != AutomationApplied || len(*patches) != 0 || len(*resizes) != 2 {
		t.Fatalf("the half-resized workload was abandoned: decision=%v patches=%d resizes=%d", decision, len(*patches), len(*resizes))
	}

	// A resize that stays halted is still rolled back.
	haltedFor(t, api, record.ID, config.DefaultAutomationInterval)
	_, body = api.do(t, http.MethodPost, "/api/v1/automation:run", "alice", "")
	if decision := decisionFor(t, body); decision["action"] != AutomationRollback || decision["outcome"] != AutomationRolledBack || len(*patches) != 1 || !(*patches)[0].Rollback {
		t.Fatalf("a halted resize lost its rollback: decision=%v patches=%d", decision, len(*patches))
	}

	// Only a marked resize is followed: a superseded approval with nothing
	// in flight is not acted on.
	if err := api.store.DeleteResizeState(context.Background(), record.ID); err != nil {
		t.Fatal(err)
	}
	_, body = api.do(t, http.MethodPost, "/api/v1/automation:run", "alice", "")
	if decisions := body["decisions"].([]any); len(decisions) != 0 || len(*patches) != 1 {
		t.Fatalf("an unmarked superseded approval was acted on: %v", decisions)
	}
}

func TestAutomationMarksOnlyAResizeThatLeavesPodsBehind(t *testing.T) {
	for name, test := range map[string]struct {
		enabled bool
		result  automation.ResizeResult
		marked  bool
	}{
		"a stage with pods left":    {true, automation.ResizeResult{Attempted: true, Pods: 3, Resized: 1, Remaining: 2}, true},
		"every pod at once":         {true, automation.ResizeResult{Attempted: true, Pods: 3, Resized: 3}, true},
		"a failed first stage":      {true, automation.ResizeResult{Attempted: true, Pods: 3, Failed: 1, Remaining: 2, Error: "pod rejected"}, true},
		"a first stage that waits":  {true, automation.ResizeResult{Pods: 3, Remaining: 3, Reason: "the next stage follows in 13m"}, true},
		"a workload that rolls out": {true, automation.ResizeResult{Reason: "the workload rolls the change out itself"}, false},
		"in-place resize disabled":  {false, automation.ResizeResult{Attempted: true, Pods: 3, Resized: 1, Remaining: 2}, false},
	} {
		body, _, _, api := runResizingAPI(t, test.enabled, func(inventory *resizingInventory) { inventory.result = test.result })
		decision := decisionFor(t, body)
		states := marks(t, api)
		if decision["outcome"] != AutomationApplied || (len(states) == 1) != test.marked || (test.marked && states[0].RecommendationID != decision["recommendationId"]) {
			t.Fatalf("%s: marks=%+v decision=%v", name, states, decision)
		}
	}
}

func TestAutomationDropsTheMarkOfAChangeTheWorkloadNoLongerCarries(t *testing.T) {
	// The workload is not annotated with this recommendation: whatever was
	// in flight for it has been replaced.
	_, patches, resizes, api, record := runRetry(t, retryCase{stale: 1, unmarked: true})
	if err := api.store.SaveResizeState(context.Background(), repository.ResizeState{RecommendationID: record.ID, Failures: 2}); err != nil {
		t.Fatal(err)
	}
	supersede(t, api, "StatefulSet")
	before := len(*patches)
	if _, plan := api.do(t, http.MethodGet, "/api/v1/automation", "alice", ""); len(plan["decisions"].([]any)) != 0 || len(marks(t, api)) != 1 {
		t.Fatalf("a dry run acted on or dropped the mark: %v", plan["decisions"])
	}
	_, body := api.do(t, http.MethodPost, "/api/v1/automation:run", "alice", "")
	if decisions := body["decisions"].([]any); len(decisions) != 0 || len(*patches) != before || len(*resizes) != 0 || len(marks(t, api)) != 0 {
		t.Fatalf("decisions=%v patches=%d resizes=%d marks=%+v", decisions, len(*patches), len(*resizes), marks(t, api))
	}
}

func TestAutomationHandsAFinishedResizeToVerification(t *testing.T) {
	var identity *usage.WorkloadIdentity
	_, _, resizes, api, record := runRetry(t, retryCase{stale: 1, mutate: func(spec *WorkloadSpec, _ *resizingInventory) { identity = spec.MetricIdentity }})
	supersede(t, api, "StatefulSet")
	if _, err := api.store.GetVerificationOutcome(context.Background(), record.ID); err == nil {
		t.Fatal("verification ran before the resize finished")
	}

	// Every pod carries the template, but the kubelet has not finished one.
	for index := range identity.Pods {
		identity.Pods[index].Resources, identity.Pods[index].Ready = identity.Pods[1].Resources, true
	}
	identity.Pods[0].Resizing = true
	_, body := api.do(t, http.MethodPost, "/api/v1/automation:run", "alice", "")
	if decisions := body["decisions"].([]any); len(decisions) != 0 || len(marks(t, api)) != 1 || len(*resizes) != 1 {
		t.Fatalf("a resize the kubelet has not finished was treated as over: %v marks=%+v", decisions, marks(t, api))
	}

	// Once it has, the mark goes and verification is asked to look, which a
	// superseded change would otherwise never get.
	identity.Pods[0].Resizing = false
	_, body = api.do(t, http.MethodPost, "/api/v1/automation:run", "alice", "")
	if decisions := body["decisions"].([]any); len(decisions) != 0 || len(marks(t, api)) != 0 || len(*resizes) != 1 {
		t.Fatalf("decisions=%v marks=%+v resizes=%d", decisions, marks(t, api), len(*resizes))
	}
	outcome, err := api.store.GetVerificationOutcome(context.Background(), record.ID)
	if err != nil || !outcome.CheckedAt.Equal(fixedNow) {
		t.Fatalf("verification was not asked to look at the finished resize: %+v err=%v", outcome, err)
	}
	if strings.Contains(strings.Join(outcome.Reasons, "; "), "live pods do not run them yet") {
		t.Fatalf("verification still sees pods behind: %v", outcome.Reasons)
	}
}
