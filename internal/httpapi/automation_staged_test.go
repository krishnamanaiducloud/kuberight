package httpapi

import (
	"net/http"
	"strings"
	"testing"

	"github.com/kuberight/kuberight/internal/automation"
)

func TestAutomationReportsTheFirstStageOfAnInPlaceResize(t *testing.T) {
	body, patches, resizes := runResizing(t, true, func(inventory *resizingInventory) {
		inventory.result = automation.ResizeResult{Attempted: true, Pods: 3, Resized: 1, Remaining: 2}
	})
	decision := decisionFor(t, body)
	inPlace, _ := decision["inPlace"].(map[string]any)
	if decision["outcome"] != AutomationApplied || body["failed"].(float64) != 0 || inPlace["remaining"].(float64) != 2 || len(*patches) != 1 || len(*resizes) != 1 {
		t.Fatalf("decision=%v body=%v", decision, body)
	}
	if reasons := strings.Join(stringsOf(decision["reasons"]), "; "); !strings.Contains(reasons, "resized 1 of 3 running pods in place; the other 2 follow in later passes") {
		t.Fatalf("reasons = %s", reasons)
	}
}

func TestAutomationKeepsTheChangeWhenTheFirstStageIsHalted(t *testing.T) {
	body, _, _ := runResizing(t, true, func(inventory *resizingInventory) {
		inventory.result = automation.ResizeResult{Attempted: true, Pods: 3, Remaining: 2, Halted: true, Error: "pod db-2 carries the new resources and is not ready"}
	})
	decision := decisionFor(t, body)
	inPlace, _ := decision["inPlace"].(map[string]any)
	if decision["outcome"] != AutomationApplied || body["applied"].(float64) != 1 || body["failed"].(float64) != 0 || inPlace["halted"] != true {
		t.Fatalf("decision=%v body=%v", decision, body)
	}
	if reasons := strings.Join(stringsOf(decision["reasons"]), "; "); !strings.Contains(reasons, "in-place resize did not start: pod db-2 carries the new resources") {
		t.Fatalf("reasons = %s", reasons)
	}
}

func TestAutomationContinuesAStagedResizeOnLaterPasses(t *testing.T) {
	// Pods the first stage left behind are resized by the next pass without
	// touching the template again.
	body, patches, resizes, _, _ := runRetry(t, retryCase{stale: 1, mutate: func(_ *WorkloadSpec, inventory *resizingInventory) {
		inventory.result = automation.ResizeResult{Attempted: true, Pods: 2, Resized: 1}
	}})
	decision := decisionFor(t, body)
	if decision["action"] != AutomationApply || decision["outcome"] != AutomationApplied || body["failed"].(float64) != 0 || len(*patches) != 0 || len(*resizes) != 1 {
		t.Fatalf("decision=%v body=%v patches=%d resizes=%d", decision, body, len(*patches), len(*resizes))
	}
}

func TestAutomationFailsAndBacksOffWhenAStagedResizeHalts(t *testing.T) {
	body, patches, resizes, api, _ := runRetry(t, retryCase{stale: 1, mutate: func(_ *WorkloadSpec, inventory *resizingInventory) {
		inventory.result = automation.ResizeResult{Attempted: true, Pods: 2, Remaining: 1, Halted: true, Error: "pod db-1 carries the new resources and is not ready"}
	}})
	decision := decisionFor(t, body)
	inPlace, _ := decision["inPlace"].(map[string]any)
	if decision["outcome"] != AutomationFailed || body["failed"].(float64) != 1 || body["applied"].(float64) != 0 || inPlace["halted"] != true ||
		!strings.Contains(decision["error"].(string), "in-place resize stopped: pod db-1 carries the new resources") || len(*patches) != 0 || len(*resizes) != 1 {
		t.Fatalf("decision=%v body=%v", decision, body)
	}
	// The halt is not re-reported on every pass.
	_, body = api.do(t, http.MethodPost, "/api/v1/automation:run", "alice", "")
	if decision := decisionFor(t, body); decision["action"] != AutomationSkip || body["failed"].(float64) != 0 || len(*resizes) != 1 {
		t.Fatalf("a halted resize was retried at once: decision=%v resizes=%d", decision, len(*resizes))
	}
}
