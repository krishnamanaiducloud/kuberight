package httpapi

import (
	"net/http"
	"strings"
	"testing"
)

func TestAutomationEndpointsAreGone(t *testing.T) {
	api := newTestAPI(t, nil, Dependencies{Provider: fakeInventory{spec: checkoutSpec()}})
	for _, request := range []struct{ method, path string }{
		{http.MethodGet, "/api/v1/automation"},
		{http.MethodPost, "/api/v1/automation:run"},
	} {
		response, body := api.do(t, request.method, request.path, "alice", "")
		failure, _ := body["error"].(map[string]any)
		if response.Code != http.StatusGone || failure["code"] != "automation_removed" || !strings.Contains(failure["message"].(string), "read-only") {
			t.Fatalf("%s %s: status=%d body=%v", request.method, request.path, response.Code, body)
		}
	}
}

// stringsOf converts a decoded JSON array of strings.
func stringsOf(value any) []string {
	items, _ := value.([]any)
	result := make([]string, 0, len(items))
	for _, item := range items {
		result = append(result, item.(string))
	}
	return result
}
