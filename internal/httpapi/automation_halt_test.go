package httpapi

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/kuberight/kuberight/internal/automation"
	"github.com/kuberight/kuberight/internal/config"
	"github.com/kuberight/kuberight/internal/repository"
	"github.com/kuberight/kuberight/internal/usage"
)

// haltedFor backdates the first sighting of a halted staged resize and
// ends any retry delay, as if that much time had passed.
func haltedFor(t *testing.T, api testAPI, id string, age time.Duration) {
	t.Helper()
	state, _ := haltState(t, api, id)
	since := fixedNow.Add(-age)
	state.RecommendationID, state.HaltedSince, state.NextAttemptAt = id, &since, fixedNow
	if err := api.store.SaveResizeState(context.Background(), state); err != nil {
		t.Fatal(err)
	}
}

// haltState reads what a pass left in the store, where another replica or
// a restarted process would find it.
func haltState(t *testing.T, api testAPI, id string) (repository.ResizeState, bool) {
	t.Helper()
	state, known, err := api.store.GetResizeState(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return state, known
}

var haltedResize = automation.ResizeResult{Attempted: true, Pods: 2, Remaining: 1, Halted: true, Error: "pod checkout-current-bbbbb carries the new resources and is not ready"}

func TestAutomationRollsBackAStagedResizeThatStaysHalted(t *testing.T) {
	// One pod took the new values and is not ready; the other never took them.
	body, patches, resizes, api, record := runRetry(t, retryCase{stale: 1, mutate: func(_ *WorkloadSpec, inventory *resizingInventory) {
		inventory.result = haltedResize
	}})
	if decision := decisionFor(t, body); decision["action"] != AutomationApply || decision["outcome"] != AutomationFailed || len(*patches) != 0 || len(*resizes) != 1 {
		t.Fatalf("first sighting: decision=%v patches=%d", decision, len(*patches))
	}
	if state, known := haltState(t, api, record.ID); !known || state.HaltedSince == nil || !state.HaltedSince.Equal(fixedNow) {
		t.Fatalf("the halt was not noted: %+v", state)
	}

	// Well short of one automation interval the stage is tried again.
	haltedFor(t, api, record.ID, config.DefaultAutomationInterval*8/10)
	_, body = api.do(t, http.MethodPost, "/api/v1/automation:run", "alice", "")
	if decision := decisionFor(t, body); decision["action"] != AutomationApply || len(*patches) != 0 || len(*resizes) != 2 {
		t.Fatalf("rolled back too early: decision=%v patches=%d resizes=%d", decision, len(*patches), len(*resizes))
	}

	// The next pass comes one interval later, give or take its own duration.
	haltedFor(t, api, record.ID, config.DefaultAutomationInterval-10*time.Second)
	_, body = api.do(t, http.MethodPost, "/api/v1/automation:run", "alice", "")
	decision := decisionFor(t, body)
	if decision["action"] != AutomationRollback || decision["outcome"] != AutomationRolledBack || body["rolledBack"].(float64) != 1 || body["failed"].(float64) != 0 || len(*patches) != 1 || len(*resizes) != 3 {
		t.Fatalf("decision=%v body=%v patches=%d resizes=%d", decision, body, len(*patches), len(*resizes))
	}
	reasons := strings.Join(stringsOf(decision["reasons"]), "; ")
	if !strings.Contains(reasons, "the staged in-place resize stayed halted since") || !strings.Contains(reasons, "pod checkout-current-bbbbb runs the new resources") || !strings.Contains(reasons, "1 of 2 running pods never took them") {
		t.Fatalf("reasons = %s", reasons)
	}
	// The template returns to the values the change replaced, and so do the pods.
	previous := automation.Resources{CPURequestMilli: 1500, CPULimitMilli: 2500, MemoryRequestBytes: 3 << 30, MemoryLimitBytes: 4 << 30}
	for name, change := range map[string]automation.Change{"template": (*patches)[0], "pods": (*resizes)[2]} {
		if !change.Rollback || change.Desired != previous || change.Previous.CPURequestMilli != record.CPURequestMilli || change.RecommendationID != record.ID {
			t.Fatalf("%s rollback = %+v", name, change)
		}
	}
	// The rollback resizes pods in place too, so it stays marked until every
	// pod runs the restored values; the halt and its backoff are gone.
	if state, known := haltState(t, api, record.ID); !known || state.HaltedSince != nil || state.Failures != 0 || !state.NextAttemptAt.IsZero() {
		t.Fatalf("state after the rollback: %+v known=%v", state, known)
	}
	_, audit := api.do(t, http.MethodGet, "/api/v1/audit?page=1&pageSize=10", "alice", "")
	found := false
	for _, item := range audit["items"].([]any) {
		event := item.(map[string]any)
		found = found || (event["action"] == "automation.rollback" && event["outcome"] == "success")
	}
	if !found {
		t.Fatalf("no rollback audit event: %v", audit["items"])
	}
}

func TestAutomationRollsBackAHaltedResizeOutsideTheChangeWindow(t *testing.T) {
	// Outside the window no stage runs, so the halt is found by evaluation
	// alone; the rollback is not held back by the window.
	closed := func(cfg *config.Config) {
		cfg.AutomationWindow = fixedNow.Add(2*time.Hour).Format("15:04") + "-" + fixedNow.Add(3*time.Hour).Format("15:04")
	}
	body, patches, resizes, api, record := runRetry(t, retryCase{stale: 1, config: closed})
	if decision := decisionFor(t, body); decision["outcome"] != AutomationDeferred || len(*resizes) != 0 {
		t.Fatalf("decision=%v", decision)
	}
	haltedFor(t, api, record.ID, config.DefaultAutomationInterval)
	_, body = api.do(t, http.MethodPost, "/api/v1/automation:run", "alice", "")
	if decision := decisionFor(t, body); body["windowOpen"] != false || decision["action"] != AutomationRollback || decision["outcome"] != AutomationRolledBack || len(*patches) != 1 || !(*patches)[0].Rollback {
		t.Fatalf("decision=%v body=%v patches=%d", decision, body, len(*patches))
	}
}

func TestAutomationContinuesWhenTheHaltedPodRecovers(t *testing.T) {
	var identity *usage.WorkloadIdentity
	body, patches, resizes, api, record := runRetry(t, retryCase{stale: 1, mutate: func(spec *WorkloadSpec, inventory *resizingInventory) {
		identity, inventory.result = spec.MetricIdentity, haltedResize
	}})
	if decision := decisionFor(t, body); decision["outcome"] != AutomationFailed {
		t.Fatalf("decision=%v", decision)
	}
	// Long after the halt was first seen, the pod is ready again.
	for index := range identity.Pods {
		identity.Pods[index].Ready = true
	}
	haltedFor(t, api, record.ID, 4*config.DefaultAutomationInterval)
	_, body = api.do(t, http.MethodPost, "/api/v1/automation:run", "alice", "")
	if decision := decisionFor(t, body); decision["action"] != AutomationApply || len(*patches) != 0 || len(*resizes) != 2 {
		t.Fatalf("a recovered pod was rolled back: decision=%v patches=%d resizes=%d", decision, len(*patches), len(*resizes))
	}
	if state, _ := haltState(t, api, record.ID); state.HaltedSince != nil {
		t.Fatalf("the halt was not forgotten: %+v", state)
	}
}

func TestAutomationDoesNotRollBackARestartFromBeforeTheChange(t *testing.T) {
	written := fixedNow.Add(-time.Hour)
	for name, test := range map[string]struct {
		restarted time.Time
		rollback  bool
	}{
		"before the change": {written.Add(-time.Minute), false},
		"after the change":  {written.Add(time.Minute), true},
	} {
		var id string
		_, patches, _, api, record := runRetry(t, retryCase{stale: 1, mutate: func(spec *WorkloadSpec, _ *resizingInventory) {
			spec.AutomationWrite = written
			for index := range spec.MetricIdentity.Pods {
				spec.MetricIdentity.Pods[index].Ready = true
				spec.MetricIdentity.Pods[index].Restarted = map[string]time.Time{"api": test.restarted}
			}
		}})
		id = record.ID
		haltedFor(t, api, id, config.DefaultAutomationInterval)
		_, body := api.do(t, http.MethodPost, "/api/v1/automation:run", "alice", "")
		if decision := decisionFor(t, body); (decision["action"] == AutomationRollback) != test.rollback || (len(*patches) == 1) != test.rollback {
			t.Fatalf("%s: decision=%v patches=%d", name, decision, len(*patches))
		}
	}
}

func TestAHaltDoesNotTurnAFinishedRollbackAround(t *testing.T) {
	// After a rollback the template carries the previous values; an unready
	// pod there must not be read as a halted forward change.
	body, patches, resizes, api, record := runRetry(t, retryCase{rolledBack: true, stale: 1})
	if decision := decisionFor(t, body); decision["action"] != AutomationRollback || len(*patches) != 0 || len(*resizes) != 1 {
		t.Fatalf("decision=%v", decision)
	}
	haltedFor(t, api, record.ID, config.DefaultAutomationInterval)
	_, body = api.do(t, http.MethodPost, "/api/v1/automation:run", "alice", "")
	if decision := decisionFor(t, body); decision["action"] != AutomationRollback || len(*patches) != 0 || !(*resizes)[len(*resizes)-1].Rollback {
		t.Fatalf("decision=%v patches=%d", decision, len(*patches))
	}
}

// unreadableResizeState is a store whose in-place resize state cannot be
// read, as when the database is unreachable.
type unreadableResizeState struct{ *repository.Memory }

func (unreadableResizeState) wrap(memory *repository.Memory) repository.Store {
	return unreadableResizeState{memory}
}

func (unreadableResizeState) GetResizeState(context.Context, string) (repository.ResizeState, bool, error) {
	return repository.ResizeState{}, false, errors.New("connection refused")
}

func TestAutomationResizesNothingWhenItsStateCannotBeRead(t *testing.T) {
	// Without the stored state the backoff and the age of a halt are
	// unknown: no stage runs and nothing is rolled back.
	body, patches, resizes, _, _ := runRetry(t, retryCase{stale: 1, repository: unreadableResizeState{}})
	decision := decisionFor(t, body)
	if decision["action"] != AutomationSkip || body["failed"].(float64) != 0 || body["applied"].(float64) != 0 || body["rolledBack"].(float64) != 0 || len(*patches) != 0 || len(*resizes) != 0 {
		t.Fatalf("decision=%v body=%v patches=%d resizes=%d", decision, body, len(*patches), len(*resizes))
	}
	if reasons := strings.Join(stringsOf(decision["reasons"]), "; "); !strings.Contains(reasons, "the state of the unfinished in-place resize could not be read") {
		t.Fatalf("reasons = %s", reasons)
	}
}
