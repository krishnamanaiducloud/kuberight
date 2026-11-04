package httpapi

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/kuberight/kuberight/internal/optimizer"
	"github.com/kuberight/kuberight/internal/repository"
)

type policyMutationRequest struct {
	Name        string                 `json:"name"`
	Description string                 `json:"description,omitempty"`
	Scope       repository.PolicyScope `json:"scope"`
	Priority    int                    `json:"priority,omitempty"`
	Enabled     bool                   `json:"enabled"`
	Policy      optimizer.Policy       `json:"policy"`
	Version     int64                  `json:"version,omitempty"`
}

func validPolicyID(id string) bool { return len(id) == 36 && strings.HasPrefix(id, "pol_") }

func (server *apiServer) listPolicies(w http.ResponseWriter, r *http.Request) {
	page, pageSize, err := parsePagination(r)
	if err != nil {
		respondError(w, r, http.StatusBadRequest, "invalid_pagination", err.Error())
		return
	}
	query := r.URL.Query()
	var enabled *bool
	if raw := query.Get("enabled"); raw != "" {
		value, parseErr := strconv.ParseBool(raw)
		if parseErr != nil {
			respondError(w, r, http.StatusBadRequest, "invalid_filter", "enabled must be true or false")
			return
		}
		enabled = &value
	}
	options := repository.PolicyListOptions{Page: page, PageSize: pageSize, Cluster: query.Get("cluster"), Namespace: query.Get("namespace"), Level: query.Get("level"), Enabled: enabled}
	if !server.authorize(w, r, AccessAttributes{Verb: "list", Resource: "policies", Namespace: options.Namespace}) {
		return
	}
	result, err := server.repository.ListPolicies(r.Context(), options)
	if err != nil {
		if strings.Contains(err.Error(), "level must") {
			respondError(w, r, http.StatusBadRequest, "invalid_filter", err.Error())
			return
		}
		server.repositoryFailure(w, r, "list policies", err)
		return
	}
	items := make([]any, 0, len(result.Items)+len(optimizer.Profiles()))
	// Built-ins remain first for backward compatibility with policy pickers.
	for _, profile := range optimizer.Profiles() {
		items = append(items, map[string]any{"name": profile.Name, "builtIn": true, "default": profile.Name == "balanced", "description": profile.Description, "policy": profile.Policy})
	}
	for _, record := range result.Items {
		items = append(items, map[string]any{"id": record.ID, "name": record.Name, "builtIn": false, "default": false, "description": record.Description, "scope": record.Scope, "priority": record.Priority, "enabled": record.Enabled, "policy": record.Policy, "version": record.Version, "createdAt": record.CreatedAt, "createdBy": record.CreatedBy, "updatedAt": record.UpdatedAt, "updatedBy": record.UpdatedBy})
	}
	respondJSON(w, http.StatusOK, map[string]any{"items": items, "pagination": map[string]int{"page": page, "pageSize": pageSize, "total": result.Total + len(optimizer.Profiles())}, "status": Availability{Available: true, Source: server.repository.Kind() + "_policy"}})
}

func policyMutation(request policyMutationRequest, actor string) repository.PolicyMutation {
	return repository.PolicyMutation{Name: request.Name, Description: request.Description, Scope: request.Scope, Priority: request.Priority, Enabled: request.Enabled, Policy: request.Policy, Actor: actor}
}

func (server *apiServer) createPolicy(w http.ResponseWriter, r *http.Request) {
	if server.config.OperationMode == "observe" {
		respondError(w, r, http.StatusForbidden, "operation_disabled", "policy changes are disabled in observe mode")
		return
	}
	var request policyMutationRequest
	if err := decodeJSON(w, r, &request); err != nil {
		writeDecodeError(w, r, err)
		return
	}
	if !server.authorize(w, r, AccessAttributes{Verb: "create", Resource: "policies", Namespace: request.Scope.Namespace}) {
		return
	}
	principal, _ := principalFromContext(r.Context())
	var record repository.PolicyRecord
	err := server.repository.InTransaction(r.Context(), func(ctx context.Context, tx repository.Transaction) error {
		var createErr error
		record, createErr = tx.CreatePolicy(ctx, policyMutation(request, truncate(principal.Username, 512)))
		if createErr != nil {
			return createErr
		}
		return tx.AppendAuditEvent(ctx, server.auditEvent(r, principal, "create", "policies/"+record.ID, record.Scope.Namespace))
	})
	if server.policyError(w, r, "create policy", err) {
		return
	}
	w.Header().Set("Location", "/api/v1/policies/"+record.ID)
	respondJSON(w, http.StatusCreated, record)
}

func (server *apiServer) getPolicy(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !validPolicyID(id) {
		respondError(w, r, http.StatusBadRequest, "invalid_id", "policy identifier is invalid")
		return
	}
	record, err := server.repository.GetPolicy(r.Context(), id)
	if server.policyError(w, r, "get policy", err) {
		return
	}
	if !server.authorize(w, r, AccessAttributes{Verb: "get", Resource: "policies", Namespace: record.Scope.Namespace, Name: id}) {
		return
	}
	respondJSON(w, http.StatusOK, record)
}

func (server *apiServer) updatePolicy(w http.ResponseWriter, r *http.Request) {
	if server.config.OperationMode == "observe" {
		respondError(w, r, http.StatusForbidden, "operation_disabled", "policy changes are disabled in observe mode")
		return
	}
	id := r.PathValue("id")
	if !validPolicyID(id) {
		respondError(w, r, http.StatusBadRequest, "invalid_id", "policy identifier is invalid")
		return
	}
	var request policyMutationRequest
	if err := decodeJSON(w, r, &request); err != nil {
		writeDecodeError(w, r, err)
		return
	}
	existing, err := server.repository.GetPolicy(r.Context(), id)
	if server.policyError(w, r, "get policy", err) {
		return
	}
	if !server.authorize(w, r, AccessAttributes{Verb: "update", Resource: "policies", Namespace: existing.Scope.Namespace, Name: id}) {
		return
	}
	if existing.Scope.Namespace != request.Scope.Namespace && !server.authorize(w, r, AccessAttributes{Verb: "update", Resource: "policies", Namespace: request.Scope.Namespace, Name: id}) {
		return
	}
	principal, _ := principalFromContext(r.Context())
	var record repository.PolicyRecord
	err = server.repository.InTransaction(r.Context(), func(ctx context.Context, tx repository.Transaction) error {
		var updateErr error
		record, updateErr = tx.UpdatePolicy(ctx, id, request.Version, policyMutation(request, truncate(principal.Username, 512)))
		if updateErr != nil {
			return updateErr
		}
		return tx.AppendAuditEvent(ctx, server.auditEvent(r, principal, "update", "policies/"+id, record.Scope.Namespace))
	})
	if server.policyError(w, r, "update policy", err) {
		return
	}
	respondJSON(w, http.StatusOK, record)
}

type versionRequest struct {
	Version int64 `json:"version"`
}

func (server *apiServer) deletePolicy(w http.ResponseWriter, r *http.Request) {
	if server.config.OperationMode == "observe" {
		respondError(w, r, http.StatusForbidden, "operation_disabled", "policy changes are disabled in observe mode")
		return
	}
	id := r.PathValue("id")
	if !validPolicyID(id) {
		respondError(w, r, http.StatusBadRequest, "invalid_id", "policy identifier is invalid")
		return
	}
	var request versionRequest
	if err := decodeJSON(w, r, &request); err != nil {
		writeDecodeError(w, r, err)
		return
	}
	existing, err := server.repository.GetPolicy(r.Context(), id)
	if server.policyError(w, r, "get policy", err) {
		return
	}
	if !server.authorize(w, r, AccessAttributes{Verb: "delete", Resource: "policies", Namespace: existing.Scope.Namespace, Name: id}) {
		return
	}
	principal, _ := principalFromContext(r.Context())
	err = server.repository.InTransaction(r.Context(), func(ctx context.Context, tx repository.Transaction) error {
		if deleteErr := tx.DeletePolicy(ctx, id, request.Version); deleteErr != nil {
			return deleteErr
		}
		return tx.AppendAuditEvent(ctx, server.auditEvent(r, principal, "delete", "policies/"+id, existing.Scope.Namespace))
	})
	if server.policyError(w, r, "delete policy", err) {
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (server *apiServer) resolvePolicy(w http.ResponseWriter, r *http.Request) {
	var subject repository.PolicyScope
	if err := decodeJSON(w, r, &subject); err != nil {
		writeDecodeError(w, r, err)
		return
	}
	if subject.Cluster == "" {
		subject.Cluster = server.config.ClusterID
	}
	if !server.authorize(w, r, AccessAttributes{Verb: "get", Resource: "policies", Namespace: subject.Namespace}) {
		return
	}
	record, err := server.repository.ResolvePolicy(r.Context(), subject)
	if errors.Is(err, repository.ErrNotFound) {
		policy, _ := optimizer.ProfileByName("balanced")
		respondJSON(w, http.StatusOK, map[string]any{"source": "built_in", "name": "balanced", "builtIn": true, "policy": policy, "subject": subject})
		return
	}
	if server.policyError(w, r, "resolve policy", err) {
		return
	}
	respondJSON(w, http.StatusOK, map[string]any{"source": "persistent", "matchedPolicy": record, "policy": record.Policy, "subject": subject, "precedence": []string{"container", "workload", "namespace", "global"}})
}

func (server *apiServer) policyError(w http.ResponseWriter, r *http.Request, operation string, err error) bool {
	if err == nil {
		return false
	}
	switch {
	case errors.Is(err, repository.ErrNotFound):
		respondError(w, r, http.StatusNotFound, "not_found", "policy was not found")
	case errors.Is(err, repository.ErrInvalidPolicy):
		respondError(w, r, http.StatusUnprocessableEntity, "invalid_policy", err.Error())
	case errors.Is(err, repository.ErrPolicyConflict):
		respondError(w, r, http.StatusConflict, "version_conflict", "policy changed since it was read; reload it before retrying")
	case errors.Is(err, context.DeadlineExceeded):
		server.repositoryFailure(w, r, operation, err)
	default:
		server.repositoryFailure(w, r, operation, err)
	}
	return true
}
