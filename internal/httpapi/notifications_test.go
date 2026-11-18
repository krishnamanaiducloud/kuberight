package httpapi

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/kuberight/kuberight/internal/repository"
)

func newOutboxAPI(t *testing.T) testAPI {
	t.Helper()
	access := &recordingAccess{}
	store := repository.NewMemory(1000)
	handler, err := NewHandler(fallbackConfig(), Dependencies{Access: access, Repository: store, Outbox: store, Now: func() time.Time { return fixedNow }})
	if err != nil {
		t.Fatal(err)
	}
	return testAPI{handler: handler, access: access, store: store}
}

func TestNotificationsListAndReplayDeadLetters(t *testing.T) {
	api := newOutboxAPI(t)
	ctx := context.Background()
	entry, err := api.store.EnqueueNotification(ctx, "recommendation.regressed", []byte(`{"recommendationId":"rec_1"}`), fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := api.store.ClaimNotifications(ctx, fixedNow, time.Minute, 1); err != nil {
		t.Fatal(err)
	}
	if err := api.store.FailNotification(ctx, entry.ID, "receiver rejected the event with HTTP 410", fixedNow, true); err != nil {
		t.Fatal(err)
	}
	response, body := api.do(t, http.MethodGet, "/api/v1/notifications?status=dead", "alice", "")
	items, _ := body["items"].([]any)
	if response.Code != http.StatusOK || len(items) != 1 || items[0].(map[string]any)["id"] != entry.ID || items[0].(map[string]any)["status"] != repository.OutboxDead {
		t.Fatalf("status=%d body=%v", response.Code, body)
	}
	if call := api.access.calls[len(api.access.calls)-1]; call.Verb != "list" || call.Resource != "notifications" {
		t.Fatalf("authorization=%+v", call)
	}
	if response, _ := api.do(t, http.MethodGet, "/api/v1/notifications?status=lost", "alice", ""); response.Code != http.StatusBadRequest {
		t.Fatalf("unknown status: %d", response.Code)
	}
	response, body = api.do(t, http.MethodPost, "/api/v1/notifications/"+entry.ID+"/replay", "alice", "")
	if response.Code != http.StatusOK || body["status"] != repository.OutboxPending || body["attempts"].(float64) != 0 {
		t.Fatalf("replay status=%d body=%v", response.Code, body)
	}
	if call := api.access.calls[len(api.access.calls)-1]; call.Verb != "update" || call.Subresource != "replay" || call.Name != entry.ID {
		t.Fatalf("authorization=%+v", call)
	}
	if response, body := api.do(t, http.MethodPost, "/api/v1/notifications/"+entry.ID+"/replay", "alice", ""); response.Code != http.StatusConflict ||
		body["error"].(map[string]any)["code"] != "notification_not_dead" {
		t.Fatalf("second replay status=%d body=%v", response.Code, body)
	}
	if response, _ := api.do(t, http.MethodPost, "/api/v1/notifications/ntf_00000000000000000000000000000000/replay", "alice", ""); response.Code != http.StatusNotFound {
		t.Fatalf("unknown replay: %d", response.Code)
	}
	audit, _ := api.store.ListAuditEvents(ctx, 1, 10)
	if len(audit.Items) == 0 || audit.Items[0].Action != "notification.replay" {
		t.Fatalf("audit=%+v", audit.Items)
	}
}

func TestNotificationsAreUnavailableWithoutDurableDelivery(t *testing.T) {
	api := newTestAPI(t, nil, Dependencies{})
	response, body := api.do(t, http.MethodGet, "/api/v1/notifications", "alice", "")
	if response.Code != http.StatusOK || body["status"].(map[string]any)["available"] != false {
		t.Fatalf("status=%d body=%v", response.Code, body)
	}
	if response, _ := api.do(t, http.MethodPost, "/api/v1/notifications/ntf_00000000000000000000000000000000/replay", "alice", ""); response.Code != http.StatusServiceUnavailable {
		t.Fatalf("replay without an outbox: %d", response.Code)
	}
}
