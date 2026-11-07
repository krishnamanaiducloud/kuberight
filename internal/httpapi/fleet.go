package httpapi

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/kuberight/kuberight/internal/repository"
)

type fleetScheduleRequest struct {
	Name            string                `json:"name"`
	Enabled         bool                  `json:"enabled"`
	IntervalSeconds int64                 `json:"intervalSeconds"`
	Scope           repository.FleetScope `json:"scope"`
	PolicyID        string                `json:"policyId,omitempty"`
	WindowSeconds   int64                 `json:"windowSeconds"`
	NextRunAt       *time.Time            `json:"nextRunAt,omitempty"`
	Version         int64                 `json:"version,omitempty"`
}

func validFleetScheduleID(id string) bool { return len(id) == 36 && strings.HasPrefix(id, "fsc_") }
func validFleetScanID(id string) bool     { return len(id) == 36 && strings.HasPrefix(id, "fsn_") }

func (server *apiServer) fleetListOptions(r *http.Request) (repository.FleetListOptions, error) {
	page, pageSize, err := parsePagination(r)
	return repository.FleetListOptions{Page: page, PageSize: pageSize, Status: r.URL.Query().Get("status"), ScheduleID: r.URL.Query().Get("scheduleId")}, err
}

func (server *apiServer) listFleetSchedules(w http.ResponseWriter, r *http.Request) {
	options, err := server.fleetListOptions(r)
	if err != nil {
		respondError(w, r, http.StatusBadRequest, "invalid_pagination", err.Error())
		return
	}
	if !server.authorize(w, r, AccessAttributes{Verb: "list", Resource: "fleet-schedules"}) {
		return
	}
	result, err := server.repository.ListFleetSchedules(r.Context(), options)
	if err != nil {
		server.repositoryFailure(w, r, "list fleet schedules", err)
		return
	}
	respondJSON(w, http.StatusOK, map[string]any{"items": result.Items, "pagination": map[string]int{"page": result.Page, "pageSize": result.PageSize, "total": result.Total}, "status": Availability{Available: true, Source: server.repository.Kind() + "_fleet"}})
}

func scheduleMutation(request fleetScheduleRequest, actor string, now time.Time) repository.FleetScheduleMutation {
	next := now
	if request.NextRunAt != nil {
		next = request.NextRunAt.UTC()
	}
	return repository.FleetScheduleMutation{Name: request.Name, Enabled: request.Enabled, IntervalSeconds: request.IntervalSeconds, Scope: request.Scope, PolicyID: request.PolicyID, WindowSeconds: request.WindowSeconds, NextRunAt: next, Actor: actor}
}

func (server *apiServer) createFleetSchedule(w http.ResponseWriter, r *http.Request) {
	if server.config.OperationMode == "observe" {
		respondError(w, r, http.StatusForbidden, "operation_disabled", "fleet schedule changes are disabled in observe mode")
		return
	}
	var request fleetScheduleRequest
	if err := decodeJSON(w, r, &request); err != nil {
		writeDecodeError(w, r, err)
		return
	}
	if !server.authorize(w, r, AccessAttributes{Verb: "create", Resource: "fleet-schedules"}) {
		return
	}
	principal, _ := principalFromContext(r.Context())
	var record repository.FleetSchedule
	err := server.repository.InTransaction(r.Context(), func(ctx context.Context, tx repository.Transaction) error {
		var createErr error
		record, createErr = tx.CreateFleetSchedule(ctx, scheduleMutation(request, truncate(principal.Username, 512), server.now().UTC()))
		if createErr != nil {
			return createErr
		}
		return tx.AppendAuditEvent(ctx, server.auditEvent(r, principal, "create", "fleet-schedules/"+record.ID, ""))
	})
	if server.fleetError(w, r, "create fleet schedule", err) {
		return
	}
	w.Header().Set("Location", "/api/v1/fleet/schedules/"+record.ID)
	respondJSON(w, http.StatusCreated, record)
}
func (server *apiServer) getFleetSchedule(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !validFleetScheduleID(id) {
		respondError(w, r, http.StatusBadRequest, "invalid_id", "fleet schedule identifier is invalid")
		return
	}
	if !server.authorize(w, r, AccessAttributes{Verb: "get", Resource: "fleet-schedules", Name: id}) {
		return
	}
	record, err := server.repository.GetFleetSchedule(r.Context(), id)
	if server.fleetError(w, r, "get fleet schedule", err) {
		return
	}
	respondJSON(w, http.StatusOK, record)
}
func (server *apiServer) updateFleetSchedule(w http.ResponseWriter, r *http.Request) {
	if server.config.OperationMode == "observe" {
		respondError(w, r, http.StatusForbidden, "operation_disabled", "fleet schedule changes are disabled in observe mode")
		return
	}
	id := r.PathValue("id")
	if !validFleetScheduleID(id) {
		respondError(w, r, http.StatusBadRequest, "invalid_id", "fleet schedule identifier is invalid")
		return
	}
	var request fleetScheduleRequest
	if err := decodeJSON(w, r, &request); err != nil {
		writeDecodeError(w, r, err)
		return
	}
	if !server.authorize(w, r, AccessAttributes{Verb: "update", Resource: "fleet-schedules", Name: id}) {
		return
	}
	principal, _ := principalFromContext(r.Context())
	var record repository.FleetSchedule
	err := server.repository.InTransaction(r.Context(), func(ctx context.Context, tx repository.Transaction) error {
		var updateErr error
		record, updateErr = tx.UpdateFleetSchedule(ctx, id, request.Version, scheduleMutation(request, truncate(principal.Username, 512), server.now().UTC()))
		if updateErr != nil {
			return updateErr
		}
		return tx.AppendAuditEvent(ctx, server.auditEvent(r, principal, "update", "fleet-schedules/"+id, ""))
	})
	if server.fleetError(w, r, "update fleet schedule", err) {
		return
	}
	respondJSON(w, http.StatusOK, record)
}
func (server *apiServer) deleteFleetSchedule(w http.ResponseWriter, r *http.Request) {
	if server.config.OperationMode == "observe" {
		respondError(w, r, http.StatusForbidden, "operation_disabled", "fleet schedule changes are disabled in observe mode")
		return
	}
	id := r.PathValue("id")
	if !validFleetScheduleID(id) {
		respondError(w, r, http.StatusBadRequest, "invalid_id", "fleet schedule identifier is invalid")
		return
	}
	var request versionRequest
	if err := decodeJSON(w, r, &request); err != nil {
		writeDecodeError(w, r, err)
		return
	}
	if !server.authorize(w, r, AccessAttributes{Verb: "delete", Resource: "fleet-schedules", Name: id}) {
		return
	}
	principal, _ := principalFromContext(r.Context())
	err := server.repository.InTransaction(r.Context(), func(ctx context.Context, tx repository.Transaction) error {
		if deleteErr := tx.DeleteFleetSchedule(ctx, id, request.Version); deleteErr != nil {
			return deleteErr
		}
		return tx.AppendAuditEvent(ctx, server.auditEvent(r, principal, "delete", "fleet-schedules/"+id, ""))
	})
	if server.fleetError(w, r, "delete fleet schedule", err) {
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

type fleetScanRequest struct {
	Scope         repository.FleetScope `json:"scope"`
	PolicyID      string                `json:"policyId,omitempty"`
	WindowSeconds int64                 `json:"windowSeconds"`
	MaxAttempts   int                   `json:"maxAttempts,omitempty"`
}

func (server *apiServer) createFleetScan(w http.ResponseWriter, r *http.Request) {
	if server.config.OperationMode == "observe" {
		respondError(w, r, http.StatusForbidden, "operation_disabled", "fleet scans are disabled in observe mode")
		return
	}
	var request fleetScanRequest
	if err := decodeJSON(w, r, &request); err != nil {
		writeDecodeError(w, r, err)
		return
	}
	if !server.authorize(w, r, AccessAttributes{Verb: "create", Resource: "fleet-scans"}) {
		return
	}
	if request.MaxAttempts == 0 {
		request.MaxAttempts = 3
	}
	principal, _ := principalFromContext(r.Context())
	var record repository.FleetScan
	var created bool
	err := server.repository.InTransaction(r.Context(), func(ctx context.Context, tx repository.Transaction) error {
		var createErr error
		record, created, createErr = tx.CreateFleetScan(ctx, repository.FleetScanRequest{Scope: request.Scope, PolicyID: request.PolicyID, WindowSeconds: request.WindowSeconds, ScheduledFor: server.now().UTC(), CreatedBy: truncate(principal.Username, 512), IdempotencyKey: strings.TrimSpace(r.Header.Get("Idempotency-Key")), MaxAttempts: request.MaxAttempts})
		if createErr != nil {
			return createErr
		}
		action := "replay"
		if created {
			action = "create"
		}
		return tx.AppendAuditEvent(ctx, server.auditEvent(r, principal, action, "fleet-scans/"+record.ID, ""))
	})
	if server.fleetError(w, r, "create fleet scan", err) {
		return
	}
	status := http.StatusOK
	if created {
		status = http.StatusAccepted
	}
	w.Header().Set("Location", "/api/v1/fleet/scans/"+record.ID)
	respondJSON(w, status, record)
}
func (server *apiServer) listFleetScans(w http.ResponseWriter, r *http.Request) {
	options, err := server.fleetListOptions(r)
	if err != nil {
		respondError(w, r, http.StatusBadRequest, "invalid_pagination", err.Error())
		return
	}
	if !server.authorize(w, r, AccessAttributes{Verb: "list", Resource: "fleet-scans"}) {
		return
	}
	result, err := server.repository.ListFleetScans(r.Context(), options)
	if err != nil {
		if strings.Contains(err.Error(), "status") {
			respondError(w, r, http.StatusBadRequest, "invalid_filter", err.Error())
			return
		}
		server.repositoryFailure(w, r, "list fleet scans", err)
		return
	}
	respondJSON(w, http.StatusOK, map[string]any{"items": result.Items, "pagination": map[string]int{"page": result.Page, "pageSize": result.PageSize, "total": result.Total}, "status": Availability{Available: true, Source: server.repository.Kind() + "_fleet"}})
}
func (server *apiServer) getFleetScan(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !validFleetScanID(id) {
		respondError(w, r, http.StatusBadRequest, "invalid_id", "fleet scan identifier is invalid")
		return
	}
	if !server.authorize(w, r, AccessAttributes{Verb: "get", Resource: "fleet-scans", Name: id}) {
		return
	}
	record, err := server.repository.GetFleetScan(r.Context(), id)
	if server.fleetError(w, r, "get fleet scan", err) {
		return
	}
	respondJSON(w, http.StatusOK, record)
}
func (server *apiServer) cancelFleetScan(w http.ResponseWriter, r *http.Request) {
	server.changeFleetScanState(w, r, "cancel")
}
func (server *apiServer) resumeFleetScan(w http.ResponseWriter, r *http.Request) {
	server.changeFleetScanState(w, r, "resume")
}
func (server *apiServer) changeFleetScanState(w http.ResponseWriter, r *http.Request, action string) {
	if server.config.OperationMode == "observe" {
		respondError(w, r, http.StatusForbidden, "operation_disabled", "fleet scan changes are disabled in observe mode")
		return
	}
	id := r.PathValue("id")
	if !validFleetScanID(id) {
		respondError(w, r, http.StatusBadRequest, "invalid_id", "fleet scan identifier is invalid")
		return
	}
	if !server.authorize(w, r, AccessAttributes{Verb: "update", Resource: "fleet-scans", Subresource: action, Name: id}) {
		return
	}
	principal, _ := principalFromContext(r.Context())
	var record repository.FleetScan
	err := server.repository.InTransaction(r.Context(), func(ctx context.Context, tx repository.Transaction) error {
		var stateErr error
		if action == "cancel" {
			record, stateErr = tx.CancelFleetScan(ctx, id, server.now().UTC())
		} else {
			record, stateErr = tx.ResumeFleetScan(ctx, id, server.now().UTC())
		}
		if stateErr != nil {
			return stateErr
		}
		return tx.AppendAuditEvent(ctx, server.auditEvent(r, principal, action, "fleet-scans/"+id, ""))
	})
	if server.fleetError(w, r, action+" fleet scan", err) {
		return
	}
	respondJSON(w, http.StatusOK, record)
}

func (server *apiServer) fleetError(w http.ResponseWriter, r *http.Request, operation string, err error) bool {
	if err == nil {
		return false
	}
	switch {
	case errors.Is(err, repository.ErrNotFound):
		respondError(w, r, http.StatusNotFound, "not_found", "fleet record was not found")
	case errors.Is(err, repository.ErrInvalidFleetSchedule):
		respondError(w, r, http.StatusUnprocessableEntity, "invalid_fleet_request", err.Error())
	case errors.Is(err, repository.ErrFleetConflict), errors.Is(err, repository.ErrFleetScanConflict):
		respondError(w, r, http.StatusConflict, "state_conflict", "fleet record changed or cannot transition from its current state")
	case errors.Is(err, repository.ErrIdempotencyConflict):
		respondError(w, r, http.StatusConflict, "idempotency_conflict", "the idempotency key was already used for a different fleet scan request")
	default:
		server.repositoryFailure(w, r, operation, err)
	}
	return true
}
