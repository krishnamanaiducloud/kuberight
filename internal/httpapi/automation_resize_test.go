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
)

type resizingInventory struct {
	automationInventory
	result  automation.ResizeResult
	err     error
	resizes *[]automation.Change
}

func (inventory resizingInventory) ResizeInPlace(_ context.Context, change automation.Change) (automation.ResizeResult, error) {
	inventory.mu.Lock()
	defer inventory.mu.Unlock()
	*inventory.resizes = append(*inventory.resizes, change)
	return inventory.result, inventory.err
}

func runResizing(t *testing.T, enabled bool, mutate func(*resizingInventory)) (map[string]any, *[]automation.Change, *[]automation.Change) {
	t.Helper()
	patches, resizes := &[]automation.Change{}, &[]automation.Change{}
	inventory := resizingInventory{
		automationInventory: automationInventory{fakeInventory: fakeInventory{spec: automationSpec()}, mu: &sync.Mutex{}, enabled: true, patches: patches},
		result:              automation.ResizeResult{Attempted: true, Pods: 3, Resized: 3},
		resizes:             resizes,
	}
	if mutate != nil {
		mutate(&inventory)
	}
	api := newTestAPI(t, func(cfg *config.Config) {
		cfg.OperationMode, cfg.AuthMode, cfg.APIToken, cfg.AutomationInPlaceResize = config.ModeApply, "kubernetes", "", enabled
	}, Dependencies{Provider: inventory})
	storeApproved(t, api, automationRecommendation(), fixedNow.Add(-time.Hour))
	response, body := api.do(t, http.MethodPost, "/api/v1/automation:run", "alice", "")
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%v", response.Code, body)
	}
	return body, patches, resizes
}

func TestAutomationResizesInPlaceOnlyWhenEnabled(t *testing.T) {
	body, patches, resizes := runResizing(t, false, nil)
	if decision := decisionFor(t, body); decision["outcome"] != AutomationApplied || decision["inPlace"] != nil || len(*patches) != 1 || len(*resizes) != 0 {
		t.Fatalf("in-place resize ran while disabled: decision=%v resizes=%v", decision, *resizes)
	}

	body, patches, resizes = runResizing(t, true, nil)
	decision := decisionFor(t, body)
	inPlace, _ := decision["inPlace"].(map[string]any)
	if decision["outcome"] != AutomationApplied || inPlace["resized"].(float64) != 3 || len(*patches) != 1 || len(*resizes) != 1 {
		t.Fatalf("decision=%v", decision)
	}
	if (*resizes)[0] != (*patches)[0] {
		t.Fatalf("the resize must carry the patched change: %+v vs %+v", (*resizes)[0], (*patches)[0])
	}
	if reasons := strings.Join(stringsOf(decision["reasons"]), "; "); !strings.Contains(reasons, "resized 3 running pods in place") {
		t.Fatalf("reasons = %s", reasons)
	}
}

func TestAutomationKeepsTheChangeWhenInPlaceResizeFails(t *testing.T) {
	for name, mutate := range map[string]func(*resizingInventory){
		"some pods rejected": func(inventory *resizingInventory) {
			inventory.result = automation.ResizeResult{Attempted: true, Pods: 3, Resized: 1, Failed: 2, Error: "pod db-1: rejected"}
		},
		"provider error": func(inventory *resizingInventory) { inventory.err = errors.New("list pods: timeout") },
	} {
		body, _, _ := runResizing(t, true, mutate)
		decision := decisionFor(t, body)
		inPlace, _ := decision["inPlace"].(map[string]any)
		if decision["outcome"] != AutomationApplied || body["applied"].(float64) != 1 || body["failed"].(float64) != 0 || inPlace["failed"].(float64) < 1 || inPlace["error"] == "" {
			t.Fatalf("%s: decision=%v body=%v", name, decision, body)
		}
		if reasons := strings.Join(stringsOf(decision["reasons"]), "; "); !strings.Contains(reasons, "take the new resources when recreated") {
			t.Fatalf("%s: reasons = %s", name, reasons)
		}
	}
}

func TestAutomationDoesNotResizeAfterAFailedPatch(t *testing.T) {
	body, _, resizes := runResizing(t, true, func(inventory *resizingInventory) { inventory.patchErr = errors.New("conflict") })
	if decision := decisionFor(t, body); decision["outcome"] != AutomationFailed || decision["inPlace"] != nil || len(*resizes) != 0 {
		t.Fatalf("decision=%v resizes=%v", decision, *resizes)
	}
}
