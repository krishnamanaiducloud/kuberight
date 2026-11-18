package httpapi

import (
	"errors"
	"net/http"

	"github.com/kuberight/kuberight/internal/repository"
)

// notificationOutbox returns the durable webhook outbox, or nil when
// durable delivery is not enabled.
func (server *apiServer) notificationOutbox() repository.NotificationOutbox { return server.outbox }

// listNotifications pages the webhook outbox, newest first, optionally by
// status: pending, delivered, or dead.
func (server *apiServer) listNotifications(w http.ResponseWriter, r *http.Request) {
	if !server.authorize(w, r, AccessAttributes{Verb: "list", Resource: "notifications"}) {
		return
	}
	page, pageSize, err := parsePagination(r)
	if err == nil && pageSize > 500 {
		err = errors.New("pageSize must be at most 500")
	}
	status := r.URL.Query().Get("status")
	if err == nil && !repository.ValidOutboxStatus(status) {
		err = errors.New("status must be pending, delivered, or dead")
	}
	if err != nil {
		respondError(w, r, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	outbox := server.notificationOutbox()
	if outbox == nil {
		respondJSON(w, http.StatusOK, map[string]any{
			"items": []any{}, "pagination": map[string]int{"page": page, "pageSize": pageSize, "total": 0},
			"status": Availability{Available: false, Source: "notification_outbox", Reason: "durable webhook delivery requires a webhook URL and the PostgreSQL store"},
		})
		return
	}
	result, err := outbox.ListNotifications(r.Context(), status, page, pageSize)
	if err != nil {
		server.repositoryFailure(w, r, "list notifications", err)
		return
	}
	respondJSON(w, http.StatusOK, map[string]any{
		"items": result.Items, "pagination": map[string]int{"page": result.Page, "pageSize": result.PageSize, "total": result.Total},
		"status": Availability{Available: true, Source: "notification_outbox"},
	})
}

// replayNotification returns a dead-lettered webhook event to the queue.
func (server *apiServer) replayNotification(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !server.authorize(w, r, AccessAttributes{Verb: "update", Resource: "notifications", Subresource: "replay", Name: id}) {
		return
	}
	outbox := server.notificationOutbox()
	if outbox == nil {
		respondError(w, r, http.StatusServiceUnavailable, "outbox_unavailable", "durable webhook delivery requires a webhook URL and the PostgreSQL store")
		return
	}
	entry, err := outbox.ReplayNotification(r.Context(), id, server.now().UTC())
	principal, _ := principalFromContext(r.Context())
	audit := server.auditEvent(r, principal, "notification.replay", "notifications/"+id, "")
	switch {
	case errors.Is(err, repository.ErrNotFound):
		respondError(w, r, http.StatusNotFound, "not_found", "notification was not found")
		return
	case errors.Is(err, repository.ErrNotReplayable):
		respondError(w, r, http.StatusConflict, "notification_not_dead", err.Error())
		return
	case err != nil:
		server.repositoryFailure(w, r, "replay notification", err)
		return
	}
	if auditErr := server.repository.AppendAuditEvent(r.Context(), audit); auditErr != nil {
		server.logger.Error("notification replay audit event could not be stored", "notification_id", id, "error", auditErr)
	}
	respondJSON(w, http.StatusOK, entry)
}
