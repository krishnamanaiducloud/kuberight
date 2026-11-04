package httpapi

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/kuberight/kuberight/internal/config"
	"github.com/kuberight/kuberight/internal/optimizer"
	"github.com/kuberight/kuberight/internal/repository"
)

func jsonBody(t *testing.T, value any) string {
	t.Helper()
	payload, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(payload)
}

func TestPersistentPolicyCRUDResolutionAndAudit(t *testing.T) {
	api := newTestAPI(t, nil, Dependencies{})
	request := policyMutationRequest{
		Name: "shop-safe", Description: "protect checkout", Enabled: true, Priority: 10,
		Scope:  repository.PolicyScope{Level: repository.PolicyScopeWorkload, Namespace: "shop", Kind: "Deployment", Workload: "checkout"},
		Policy: optimizer.DefaultPolicy(),
	}
	response, body := api.do(t, http.MethodPost, "/api/v1/policies", "alice", jsonBody(t, request))
	if response.Code != http.StatusCreated {
		t.Fatalf("create status=%d body=%s", response.Code, response.Body.String())
	}
	id, _ := body["id"].(string)
	if !validPolicyID(id) || body["version"] != float64(1) || body["createdBy"] != "alice" {
		t.Fatalf("created=%v", body)
	}

	response, resolved := api.do(t, http.MethodPost, "/api/v1/policies:resolve", "alice", `{"namespace":"shop","kind":"Deployment","workload":"checkout","container":"api"}`)
	if response.Code != http.StatusOK || resolved["source"] != "persistent" {
		t.Fatalf("resolve status=%d body=%s", response.Code, response.Body.String())
	}
	matched := resolved["matchedPolicy"].(map[string]any)
	if matched["id"] != id {
		t.Fatalf("matched=%v", matched)
	}

	request.Name, request.Version = "shop-safe-v2", 1
	response, updated := api.do(t, http.MethodPut, "/api/v1/policies/"+id, "bob", jsonBody(t, request))
	if response.Code != http.StatusOK || updated["version"] != float64(2) || updated["updatedBy"] != "bob" {
		t.Fatalf("update status=%d body=%s", response.Code, response.Body.String())
	}
	response, _ = api.do(t, http.MethodPut, "/api/v1/policies/"+id, "bob", jsonBody(t, request))
	if response.Code != http.StatusConflict {
		t.Fatalf("stale update status=%d body=%s", response.Code, response.Body.String())
	}

	response, listed := api.do(t, http.MethodGet, "/api/v1/policies?namespace=shop", "alice", "")
	if response.Code != http.StatusOK || len(listed["items"].([]any)) != 4 {
		t.Fatalf("list status=%d body=%s", response.Code, response.Body.String())
	}
	response, _ = api.do(t, http.MethodDelete, "/api/v1/policies/"+id, "alice", `{"version":2}`)
	if response.Code != http.StatusNoContent {
		t.Fatalf("delete status=%d body=%s", response.Code, response.Body.String())
	}
	response, fallback := api.do(t, http.MethodPost, "/api/v1/policies:resolve", "alice", `{"namespace":"shop","kind":"Deployment","workload":"checkout","container":"api"}`)
	if response.Code != http.StatusOK || fallback["source"] != "built_in" || fallback["name"] != "balanced" {
		t.Fatalf("fallback status=%d body=%s", response.Code, response.Body.String())
	}
	audit, err := api.store.ListAuditEvents(t.Context(), 1, 20)
	if err != nil || audit.Total != 3 {
		t.Fatalf("audit=%#v err=%v", audit, err)
	}
}

func TestFleetScheduleAndScanLifecycleAPI(t *testing.T) {
	api := newTestAPI(t, nil, Dependencies{})
	next := fixedNow.Add(time.Hour)
	scheduleRequest := fleetScheduleRequest{Name: "hourly", Enabled: true, IntervalSeconds: 3600, Scope: repository.FleetScope{Namespaces: []string{"shop"}, Kinds: []string{"Deployment"}}, WindowSeconds: 7 * 24 * 3600, NextRunAt: &next}
	response, schedule := api.do(t, http.MethodPost, "/api/v1/fleet/schedules", "alice", jsonBody(t, scheduleRequest))
	if response.Code != http.StatusCreated {
		t.Fatalf("schedule status=%d body=%s", response.Code, response.Body.String())
	}
	scheduleID := schedule["id"].(string)
	if !validFleetScheduleID(scheduleID) {
		t.Fatalf("schedule=%v", schedule)
	}

	scanRequest := fleetScanRequest{Scope: repository.FleetScope{Namespaces: []string{"shop"}}, WindowSeconds: 24 * 3600}
	response, scan := api.do(t, http.MethodPost, "/api/v1/fleet/scans", "alice", jsonBody(t, scanRequest))
	if response.Code != http.StatusAccepted || scan["status"] != repository.FleetScanQueued {
		t.Fatalf("scan status=%d body=%s", response.Code, response.Body.String())
	}
	scanID := scan["id"].(string)
	response, canceled := api.do(t, http.MethodPost, "/api/v1/fleet/scans/"+scanID+"/cancel", "alice", "")
	if response.Code != http.StatusOK || canceled["status"] != repository.FleetScanCanceled {
		t.Fatalf("cancel status=%d body=%s", response.Code, response.Body.String())
	}
	response, resumed := api.do(t, http.MethodPost, "/api/v1/fleet/scans/"+scanID+"/resume", "alice", "")
	if response.Code != http.StatusOK || resumed["status"] != repository.FleetScanQueued {
		t.Fatalf("resume status=%d body=%s", response.Code, response.Body.String())
	}
	response, scans := api.do(t, http.MethodGet, "/api/v1/fleet/scans?status=queued", "alice", "")
	if response.Code != http.StatusOK || scans["pagination"].(map[string]any)["total"] != float64(1) {
		t.Fatalf("scans status=%d body=%s", response.Code, response.Body.String())
	}

	scheduleRequest.Name, scheduleRequest.Version = "hourly-v2", 1
	response, updated := api.do(t, http.MethodPut, "/api/v1/fleet/schedules/"+scheduleID, "alice", jsonBody(t, scheduleRequest))
	if response.Code != http.StatusOK || updated["version"] != float64(2) {
		t.Fatalf("schedule update status=%d body=%s", response.Code, response.Body.String())
	}
	response, _ = api.do(t, http.MethodDelete, "/api/v1/fleet/schedules/"+scheduleID, "alice", `{"version":2}`)
	if response.Code != http.StatusNoContent {
		t.Fatalf("schedule delete status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestPolicyAndFleetWritesAreDisabledInObserveMode(t *testing.T) {
	api := newTestAPI(t, func(cfg *config.Config) { cfg.OperationMode = config.ModeObserve }, Dependencies{})
	for _, test := range []struct{ path, body string }{
		{"/api/v1/policies", `{}`}, {"/api/v1/fleet/schedules", `{}`}, {"/api/v1/fleet/scans", `{}`},
	} {
		response, _ := api.do(t, http.MethodPost, test.path, "alice", test.body)
		if response.Code != http.StatusForbidden {
			t.Fatalf("%s status=%d body=%s", test.path, response.Code, response.Body.String())
		}
	}
}
