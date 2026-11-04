package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/kuberight/kuberight/internal/repository"
)

type rowScanner interface{ Scan(...any) error }

const policyColumns = `id, name, description, scope_level, cluster_id, namespace, workload_kind, workload_name,
container_name, priority, enabled, policy, version, created_at, created_by, updated_at, updated_by`

func scanPolicy(row rowScanner) (repository.PolicyRecord, error) {
	var record repository.PolicyRecord
	var payload []byte
	err := row.Scan(&record.ID, &record.Name, &record.Description, &record.Scope.Level, &record.Scope.Cluster,
		&record.Scope.Namespace, &record.Scope.Kind, &record.Scope.Workload, &record.Scope.Container,
		&record.Priority, &record.Enabled, &payload, &record.Version, &record.CreatedAt, &record.CreatedBy,
		&record.UpdatedAt, &record.UpdatedBy)
	if err != nil {
		return repository.PolicyRecord{}, err
	}
	if err := json.Unmarshal(payload, &record.Policy); err != nil {
		return repository.PolicyRecord{}, fmt.Errorf("decode stored policy %s: %w", record.ID, err)
	}
	return record, nil
}

func createPolicy(ctx context.Context, db queryer, mutation repository.PolicyMutation) (repository.PolicyRecord, error) {
	if err := mutation.Validate(); err != nil {
		return repository.PolicyRecord{}, err
	}
	id, err := repository.NewPolicyID()
	if err != nil {
		return repository.PolicyRecord{}, errors.New("could not generate policy identifier")
	}
	payload, err := json.Marshal(mutation.Policy)
	if err != nil {
		return repository.PolicyRecord{}, err
	}
	now := time.Now().UTC()
	return scanPolicy(db.QueryRow(ctx, `INSERT INTO policies
(id,name,description,scope_level,cluster_id,namespace,workload_kind,workload_name,container_name,priority,enabled,policy,version,created_at,created_by,updated_at,updated_by)
VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,1,$13,$14,$13,$14) RETURNING `+policyColumns,
		id, strings.TrimSpace(mutation.Name), mutation.Description, mutation.Scope.Level, mutation.Scope.Cluster,
		mutation.Scope.Namespace, mutation.Scope.Kind, mutation.Scope.Workload, mutation.Scope.Container,
		mutation.Priority, mutation.Enabled, payload, now, mutation.Actor))
}

func (store *Store) CreatePolicy(ctx context.Context, mutation repository.PolicyMutation) (repository.PolicyRecord, error) {
	return createPolicy(ctx, store.pool, mutation)
}
func (store *Store) GetPolicy(ctx context.Context, id string) (repository.PolicyRecord, error) {
	return getPolicy(ctx, store.pool, id)
}
func getPolicy(ctx context.Context, db queryer, id string) (repository.PolicyRecord, error) {
	record, err := scanPolicy(db.QueryRow(ctx, "SELECT "+policyColumns+" FROM policies WHERE id=$1", id))
	if errors.Is(err, pgx.ErrNoRows) {
		return repository.PolicyRecord{}, repository.ErrNotFound
	}
	return record, err
}

func listPolicies(ctx context.Context, db queryer, options repository.PolicyListOptions) (repository.PolicyListResult, error) {
	if err := repository.ValidatePolicyListOptions(options); err != nil {
		return repository.PolicyListResult{}, err
	}
	rows, err := db.Query(ctx, `SELECT `+policyColumns+`, count(*) OVER()
FROM policies
WHERE ($1='' OR cluster_id='' OR cluster_id=$1)
  AND ($2='' OR namespace='' OR namespace=$2)
  AND ($3='' OR scope_level=$3)
  AND ($4::boolean IS NULL OR enabled=$4)
ORDER BY updated_at DESC, id DESC LIMIT $5 OFFSET $6`, options.Cluster, options.Namespace, options.Level, options.Enabled, options.PageSize, (options.Page-1)*options.PageSize)
	if err != nil {
		return repository.PolicyListResult{}, err
	}
	defer rows.Close()
	result := repository.PolicyListResult{Page: options.Page, PageSize: options.PageSize}
	for rows.Next() {
		var record repository.PolicyRecord
		var payload []byte
		if err := rows.Scan(&record.ID, &record.Name, &record.Description, &record.Scope.Level, &record.Scope.Cluster,
			&record.Scope.Namespace, &record.Scope.Kind, &record.Scope.Workload, &record.Scope.Container,
			&record.Priority, &record.Enabled, &payload, &record.Version, &record.CreatedAt, &record.CreatedBy,
			&record.UpdatedAt, &record.UpdatedBy, &result.Total); err != nil {
			return repository.PolicyListResult{}, err
		}
		if err := json.Unmarshal(payload, &record.Policy); err != nil {
			return repository.PolicyListResult{}, fmt.Errorf("decode stored policy %s: %w", record.ID, err)
		}
		result.Items = append(result.Items, record)
	}
	return result, rows.Err()
}
func (store *Store) ListPolicies(ctx context.Context, options repository.PolicyListOptions) (repository.PolicyListResult, error) {
	return listPolicies(ctx, store.pool, options)
}

func versionError(ctx context.Context, db queryer, table, id string, conflict error) error {
	var exists bool
	if err := db.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM "+table+" WHERE id=$1)", id).Scan(&exists); err != nil {
		return err
	}
	if !exists {
		return repository.ErrNotFound
	}
	return conflict
}

func updatePolicy(ctx context.Context, db queryer, id string, expectedVersion int64, mutation repository.PolicyMutation) (repository.PolicyRecord, error) {
	if err := mutation.Validate(); err != nil {
		return repository.PolicyRecord{}, err
	}
	if expectedVersion < 1 {
		return repository.PolicyRecord{}, repository.ErrPolicyConflict
	}
	payload, err := json.Marshal(mutation.Policy)
	if err != nil {
		return repository.PolicyRecord{}, err
	}
	record, err := scanPolicy(db.QueryRow(ctx, `UPDATE policies SET name=$3,description=$4,scope_level=$5,cluster_id=$6,namespace=$7,
workload_kind=$8,workload_name=$9,container_name=$10,priority=$11,enabled=$12,policy=$13,version=version+1,updated_at=$14,updated_by=$15
WHERE id=$1 AND version=$2 RETURNING `+policyColumns, id, expectedVersion, strings.TrimSpace(mutation.Name), mutation.Description,
		mutation.Scope.Level, mutation.Scope.Cluster, mutation.Scope.Namespace, mutation.Scope.Kind, mutation.Scope.Workload,
		mutation.Scope.Container, mutation.Priority, mutation.Enabled, payload, time.Now().UTC(), mutation.Actor))
	if errors.Is(err, pgx.ErrNoRows) {
		return repository.PolicyRecord{}, versionError(ctx, db, "policies", id, repository.ErrPolicyConflict)
	}
	return record, err
}
func (store *Store) UpdatePolicy(ctx context.Context, id string, v int64, m repository.PolicyMutation) (repository.PolicyRecord, error) {
	return updatePolicy(ctx, store.pool, id, v, m)
}

func deletePolicy(ctx context.Context, db queryer, id string, version int64) error {
	if version < 1 {
		return repository.ErrPolicyConflict
	}
	tag, err := db.Exec(ctx, "DELETE FROM policies WHERE id=$1 AND version=$2", id, version)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return versionError(ctx, db, "policies", id, repository.ErrPolicyConflict)
	}
	return nil
}
func (store *Store) DeletePolicy(ctx context.Context, id string, v int64) error {
	return deletePolicy(ctx, store.pool, id, v)
}

func resolvePolicy(ctx context.Context, db queryer, subject repository.PolicyScope) (repository.PolicyRecord, error) {
	if subject.Namespace == "" || subject.Kind == "" || subject.Workload == "" || subject.Container == "" {
		return repository.PolicyRecord{}, repository.ErrInvalidPolicy
	}
	record, err := scanPolicy(db.QueryRow(ctx, `SELECT `+policyColumns+` FROM policies WHERE enabled
AND (cluster_id='' OR cluster_id=$1) AND (namespace='' OR namespace=$2)
AND (workload_kind='' OR lower(workload_kind)=lower($3)) AND (workload_name='' OR workload_name=$4)
AND (container_name='' OR container_name=$5)
ORDER BY CASE scope_level WHEN 'container' THEN 4 WHEN 'workload' THEN 3 WHEN 'namespace' THEN 2 ELSE 1 END DESC,
priority DESC, updated_at DESC, id ASC LIMIT 1`, subject.Cluster, subject.Namespace, subject.Kind, subject.Workload, subject.Container))
	if errors.Is(err, pgx.ErrNoRows) {
		return repository.PolicyRecord{}, repository.ErrNotFound
	}
	return record, err
}
func (store *Store) ResolvePolicy(ctx context.Context, s repository.PolicyScope) (repository.PolicyRecord, error) {
	return resolvePolicy(ctx, store.pool, s)
}

const scheduleColumns = `id,name,enabled,interval_seconds,scope,policy_id,window_seconds,next_run_at,last_run_at,version,created_at,created_by,updated_at,updated_by`

func scanSchedule(row rowScanner) (repository.FleetSchedule, error) {
	var record repository.FleetSchedule
	var payload []byte
	err := row.Scan(&record.ID, &record.Name, &record.Enabled, &record.IntervalSeconds, &payload, &record.PolicyID, &record.WindowSeconds, &record.NextRunAt, &record.LastRunAt, &record.Version, &record.CreatedAt, &record.CreatedBy, &record.UpdatedAt, &record.UpdatedBy)
	if err != nil {
		return repository.FleetSchedule{}, err
	}
	if err := json.Unmarshal(payload, &record.Scope); err != nil {
		return repository.FleetSchedule{}, fmt.Errorf("decode fleet schedule scope: %w", err)
	}
	return record, nil
}
func createSchedule(ctx context.Context, db queryer, m repository.FleetScheduleMutation) (repository.FleetSchedule, error) {
	if err := m.Validate(); err != nil {
		return repository.FleetSchedule{}, err
	}
	id, err := repository.NewFleetScheduleID()
	if err != nil {
		return repository.FleetSchedule{}, err
	}
	payload, _ := json.Marshal(m.Scope)
	now := time.Now().UTC()
	return scanSchedule(db.QueryRow(ctx, `INSERT INTO fleet_schedules(id,name,enabled,interval_seconds,scope,policy_id,window_seconds,next_run_at,version,created_at,created_by,updated_at,updated_by) VALUES($1,$2,$3,$4,$5,$6,$7,$8,1,$9,$10,$9,$10) RETURNING `+scheduleColumns, id, strings.TrimSpace(m.Name), m.Enabled, m.IntervalSeconds, payload, m.PolicyID, m.WindowSeconds, m.NextRunAt.UTC(), now, m.Actor))
}
func (store *Store) CreateFleetSchedule(ctx context.Context, m repository.FleetScheduleMutation) (repository.FleetSchedule, error) {
	return createSchedule(ctx, store.pool, m)
}
func getSchedule(ctx context.Context, db queryer, id string) (repository.FleetSchedule, error) {
	record, err := scanSchedule(db.QueryRow(ctx, "SELECT "+scheduleColumns+" FROM fleet_schedules WHERE id=$1", id))
	if errors.Is(err, pgx.ErrNoRows) {
		return repository.FleetSchedule{}, repository.ErrNotFound
	}
	return record, err
}
func (store *Store) GetFleetSchedule(ctx context.Context, id string) (repository.FleetSchedule, error) {
	return getSchedule(ctx, store.pool, id)
}
func listSchedules(ctx context.Context, db queryer, o repository.FleetListOptions) (repository.FleetScheduleListResult, error) {
	if err := validateFleetList(o); err != nil {
		return repository.FleetScheduleListResult{}, err
	}
	rows, err := db.Query(ctx, "SELECT "+scheduleColumns+",count(*) OVER() FROM fleet_schedules ORDER BY updated_at DESC,id DESC LIMIT $1 OFFSET $2", o.PageSize, (o.Page-1)*o.PageSize)
	if err != nil {
		return repository.FleetScheduleListResult{}, err
	}
	defer rows.Close()
	result := repository.FleetScheduleListResult{Page: o.Page, PageSize: o.PageSize}
	for rows.Next() {
		var record repository.FleetSchedule
		var payload []byte
		if err := rows.Scan(&record.ID, &record.Name, &record.Enabled, &record.IntervalSeconds, &payload, &record.PolicyID, &record.WindowSeconds, &record.NextRunAt, &record.LastRunAt, &record.Version, &record.CreatedAt, &record.CreatedBy, &record.UpdatedAt, &record.UpdatedBy, &result.Total); err != nil {
			return repository.FleetScheduleListResult{}, err
		}
		if err := json.Unmarshal(payload, &record.Scope); err != nil {
			return repository.FleetScheduleListResult{}, err
		}
		result.Items = append(result.Items, record)
	}
	return result, rows.Err()
}
func (store *Store) ListFleetSchedules(ctx context.Context, o repository.FleetListOptions) (repository.FleetScheduleListResult, error) {
	return listSchedules(ctx, store.pool, o)
}
func updateSchedule(ctx context.Context, db queryer, id string, v int64, m repository.FleetScheduleMutation) (repository.FleetSchedule, error) {
	if err := m.Validate(); err != nil {
		return repository.FleetSchedule{}, err
	}
	if v < 1 {
		return repository.FleetSchedule{}, repository.ErrFleetConflict
	}
	payload, _ := json.Marshal(m.Scope)
	record, err := scanSchedule(db.QueryRow(ctx, `UPDATE fleet_schedules SET name=$3,enabled=$4,interval_seconds=$5,scope=$6,policy_id=$7,window_seconds=$8,next_run_at=$9,version=version+1,updated_at=$10,updated_by=$11 WHERE id=$1 AND version=$2 RETURNING `+scheduleColumns, id, v, strings.TrimSpace(m.Name), m.Enabled, m.IntervalSeconds, payload, m.PolicyID, m.WindowSeconds, m.NextRunAt.UTC(), time.Now().UTC(), m.Actor))
	if errors.Is(err, pgx.ErrNoRows) {
		return repository.FleetSchedule{}, versionError(ctx, db, "fleet_schedules", id, repository.ErrFleetConflict)
	}
	return record, err
}
func (store *Store) UpdateFleetSchedule(ctx context.Context, id string, v int64, m repository.FleetScheduleMutation) (repository.FleetSchedule, error) {
	return updateSchedule(ctx, store.pool, id, v, m)
}
func deleteSchedule(ctx context.Context, db queryer, id string, v int64) error {
	if v < 1 {
		return repository.ErrFleetConflict
	}
	tag, err := db.Exec(ctx, "DELETE FROM fleet_schedules WHERE id=$1 AND version=$2", id, v)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return versionError(ctx, db, "fleet_schedules", id, repository.ErrFleetConflict)
	}
	return nil
}
func (store *Store) DeleteFleetSchedule(ctx context.Context, id string, v int64) error {
	return deleteSchedule(ctx, store.pool, id, v)
}

const scanColumns = `id,schedule_id,scheduled_for,available_at,scope,policy_id,window_seconds,status,expected,processed,succeeded,failed,attempt,max_attempts,lease_owner,lease_expires_at,last_error,failure_details,created_at,created_by,started_at,completed_at,cancellation_at,COALESCE(idempotency_hash,''),request_fingerprint`

func scanFleetScan(row rowScanner) (repository.FleetScan, error) {
	var record repository.FleetScan
	var payload, failurePayload []byte
	err := row.Scan(&record.ID, &record.ScheduleID, &record.ScheduledFor, &record.AvailableAt, &payload, &record.PolicyID, &record.WindowSeconds, &record.Status, &record.Expected, &record.Processed, &record.Succeeded, &record.Failed, &record.Attempt, &record.MaxAttempts, &record.LeaseOwner, &record.LeaseExpiresAt, &record.LastError, &failurePayload, &record.CreatedAt, &record.CreatedBy, &record.StartedAt, &record.CompletedAt, &record.CancellationAt, &record.IdempotencyHash, &record.RequestFingerprint)
	if err != nil {
		return repository.FleetScan{}, err
	}
	if err := json.Unmarshal(payload, &record.Scope); err != nil {
		return repository.FleetScan{}, fmt.Errorf("decode fleet scan scope: %w", err)
	}
	if err := json.Unmarshal(failurePayload, &record.FailureDetails); err != nil {
		return repository.FleetScan{}, fmt.Errorf("decode fleet scan failure details: %w", err)
	}
	return record, nil
}
func createScan(ctx context.Context, db queryer, r repository.FleetScanRequest, scheduleID string) (repository.FleetScan, bool, error) {
	if err := r.Validate(); err != nil {
		return repository.FleetScan{}, false, err
	}
	id, err := repository.NewFleetScanID()
	if err != nil {
		return repository.FleetScan{}, false, err
	}
	payload, _ := json.Marshal(r.Scope)
	hash := repository.FleetIdempotencyHash(r.CreatedBy, r.IdempotencyKey)
	fingerprint := repository.FleetRequestFingerprint(r)
	var nullableHash any
	if hash != "" {
		nullableHash = hash
	}
	now := time.Now().UTC()
	tag, err := db.Exec(ctx, `INSERT INTO fleet_scans(id,schedule_id,scheduled_for,available_at,scope,policy_id,window_seconds,status,max_attempts,created_at,created_by,idempotency_hash,request_fingerprint) VALUES($1,$2,$3,$3,$4,$5,$6,'queued',$7,$8,$9,$10,$11) ON CONFLICT DO NOTHING`, id, scheduleID, r.ScheduledFor.UTC(), payload, r.PolicyID, r.WindowSeconds, r.MaxAttempts, now, r.CreatedBy, nullableHash, fingerprint)
	if err != nil {
		return repository.FleetScan{}, false, err
	}
	if tag.RowsAffected() == 0 {
		if hash == "" {
			return repository.FleetScan{}, false, repository.ErrFleetScanConflict
		}
		record, err := scanFleetScan(db.QueryRow(ctx, "SELECT "+scanColumns+" FROM fleet_scans WHERE idempotency_hash=$1", hash))
		if err == nil && record.RequestFingerprint != fingerprint {
			return repository.FleetScan{}, false, repository.ErrIdempotencyConflict
		}
		return record, false, err
	}
	record, err := scanFleetScan(db.QueryRow(ctx, "SELECT "+scanColumns+" FROM fleet_scans WHERE id=$1", id))
	return record, true, err
}
func (store *Store) CreateFleetScan(ctx context.Context, r repository.FleetScanRequest) (repository.FleetScan, bool, error) {
	return createScan(ctx, store.pool, r, "")
}
func getScan(ctx context.Context, db queryer, id string) (repository.FleetScan, error) {
	record, err := scanFleetScan(db.QueryRow(ctx, "SELECT "+scanColumns+" FROM fleet_scans WHERE id=$1", id))
	if errors.Is(err, pgx.ErrNoRows) {
		return repository.FleetScan{}, repository.ErrNotFound
	}
	return record, err
}
func (store *Store) GetFleetScan(ctx context.Context, id string) (repository.FleetScan, error) {
	return getScan(ctx, store.pool, id)
}
func listScans(ctx context.Context, db queryer, o repository.FleetListOptions) (repository.FleetScanListResult, error) {
	if err := validateFleetList(o); err != nil {
		return repository.FleetScanListResult{}, err
	}
	rows, err := db.Query(ctx, "SELECT "+scanColumns+",count(*) OVER() FROM fleet_scans WHERE ($1='' OR status=$1) AND ($2='' OR schedule_id=$2) ORDER BY created_at DESC,id DESC LIMIT $3 OFFSET $4", o.Status, o.ScheduleID, o.PageSize, (o.Page-1)*o.PageSize)
	if err != nil {
		return repository.FleetScanListResult{}, err
	}
	defer rows.Close()
	result := repository.FleetScanListResult{Page: o.Page, PageSize: o.PageSize}
	for rows.Next() {
		var record repository.FleetScan
		var payload, failurePayload []byte
		if err := rows.Scan(&record.ID, &record.ScheduleID, &record.ScheduledFor, &record.AvailableAt, &payload, &record.PolicyID, &record.WindowSeconds, &record.Status, &record.Expected, &record.Processed, &record.Succeeded, &record.Failed, &record.Attempt, &record.MaxAttempts, &record.LeaseOwner, &record.LeaseExpiresAt, &record.LastError, &failurePayload, &record.CreatedAt, &record.CreatedBy, &record.StartedAt, &record.CompletedAt, &record.CancellationAt, &record.IdempotencyHash, &record.RequestFingerprint, &result.Total); err != nil {
			return repository.FleetScanListResult{}, err
		}
		if err := json.Unmarshal(payload, &record.Scope); err != nil {
			return repository.FleetScanListResult{}, err
		}
		if err := json.Unmarshal(failurePayload, &record.FailureDetails); err != nil {
			return repository.FleetScanListResult{}, err
		}
		result.Items = append(result.Items, record)
	}
	return result, rows.Err()
}
func (store *Store) ListFleetScans(ctx context.Context, o repository.FleetListOptions) (repository.FleetScanListResult, error) {
	return listScans(ctx, store.pool, o)
}

func mutateScanState(ctx context.Context, db queryer, query, id string, now time.Time) (repository.FleetScan, error) {
	record, err := scanFleetScan(db.QueryRow(ctx, query, id, now.UTC()))
	if errors.Is(err, pgx.ErrNoRows) {
		if _, getErr := getScan(ctx, db, id); errors.Is(getErr, repository.ErrNotFound) {
			return repository.FleetScan{}, getErr
		}
		return repository.FleetScan{}, repository.ErrFleetScanConflict
	}
	return record, err
}
func cancelScan(ctx context.Context, db queryer, id string, now time.Time) (repository.FleetScan, error) {
	return mutateScanState(ctx, db, "UPDATE fleet_scans SET status='canceled',cancellation_at=$2,completed_at=$2,lease_owner='',lease_expires_at=NULL WHERE id=$1 AND status IN ('queued','running') RETURNING "+scanColumns, id, now)
}
func (store *Store) CancelFleetScan(ctx context.Context, id string, now time.Time) (repository.FleetScan, error) {
	return cancelScan(ctx, store.pool, id, now)
}
func resumeScan(ctx context.Context, db queryer, id string, now time.Time) (repository.FleetScan, error) {
	return mutateScanState(ctx, db, "UPDATE fleet_scans SET status='queued',available_at=$2,attempt=0,cancellation_at=NULL,completed_at=NULL,last_error='',lease_owner='',lease_expires_at=NULL WHERE id=$1 AND status IN ('canceled','failed') RETURNING "+scanColumns, id, now)
}
func (store *Store) ResumeFleetScan(ctx context.Context, id string, now time.Time) (repository.FleetScan, error) {
	return resumeScan(ctx, store.pool, id, now)
}

func (store *Store) EnqueueDueFleetScans(ctx context.Context, now time.Time, limit int) (result []repository.FleetScan, err error) {
	if limit < 1 || limit > 1000 {
		return nil, errors.New("limit must be between 1 and 1000")
	}
	tx, err := store.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback(context.WithoutCancel(ctx))
		}
	}()
	rows, err := tx.Query(ctx, "SELECT "+scheduleColumns+" FROM fleet_schedules WHERE enabled AND next_run_at<=$1 ORDER BY next_run_at,id FOR UPDATE SKIP LOCKED LIMIT $2", now.UTC(), limit)
	if err != nil {
		return nil, err
	}
	var schedules []repository.FleetSchedule
	for rows.Next() {
		record, scanErr := scanSchedule(rows)
		if scanErr != nil {
			rows.Close()
			return nil, scanErr
		}
		schedules = append(schedules, record)
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	rows.Close()
	for _, schedule := range schedules {
		request := repository.FleetScanRequest{Scope: schedule.Scope, PolicyID: schedule.PolicyID, WindowSeconds: schedule.WindowSeconds, ScheduledFor: schedule.NextRunAt, CreatedBy: "fleet-scheduler", MaxAttempts: 3}
		scan, created, createErr := createScan(ctx, tx, request, schedule.ID)
		if createErr != nil {
			return nil, createErr
		}
		if created {
			result = append(result, scan)
		}
		interval := time.Duration(schedule.IntervalSeconds) * time.Second
		steps := int64(now.Sub(schedule.NextRunAt)/interval) + 1
		if steps < 1 {
			steps = 1
		}
		next := schedule.NextRunAt.Add(time.Duration(steps) * interval)
		if _, err = tx.Exec(ctx, "UPDATE fleet_schedules SET last_run_at=next_run_at,next_run_at=$2,version=version+1,updated_at=$1,updated_by='fleet-scheduler' WHERE id=$3", now.UTC(), next, schedule.ID); err != nil {
			return nil, err
		}
	}
	if err = tx.Commit(ctx); err != nil {
		return nil, err
	}
	return result, nil
}

func (store *Store) ClaimFleetScan(ctx context.Context, worker string, now time.Time, lease time.Duration) (repository.FleetScan, error) {
	if strings.TrimSpace(worker) == "" || len(worker) > 128 || lease < time.Minute || lease > time.Hour {
		return repository.FleetScan{}, repository.ErrFleetScanConflict
	}
	record, err := scanFleetScan(store.pool.QueryRow(ctx, `WITH candidate AS (SELECT id FROM fleet_scans WHERE available_at<=$1 AND attempt<max_attempts AND (status='queued' OR (status='running' AND lease_expires_at<=$1)) ORDER BY available_at,id FOR UPDATE SKIP LOCKED LIMIT 1) UPDATE fleet_scans f SET status='running',lease_owner=$2,lease_expires_at=$3,attempt=f.attempt+1,started_at=COALESCE(f.started_at,$1) FROM candidate WHERE f.id=candidate.id RETURNING `+scanColumns, now.UTC(), worker, now.UTC().Add(lease)))
	if errors.Is(err, pgx.ErrNoRows) {
		return repository.FleetScan{}, repository.ErrNotFound
	}
	return record, err
}
func (store *Store) UpdateFleetScan(ctx context.Context, id, worker string, now time.Time, p repository.FleetScanProgress) (repository.FleetScan, error) {
	if err := repository.ValidateFleetScanProgress(p); err != nil {
		return repository.FleetScan{}, repository.ErrFleetScanConflict
	}
	failurePayload, err := json.Marshal(p.FailureDetails)
	if err != nil {
		return repository.FleetScan{}, err
	}
	status := p.Status
	if status == "" {
		status = repository.FleetScanRunning
	}
	record, err := scanFleetScan(store.pool.QueryRow(ctx, `UPDATE fleet_scans SET
expected=$5,processed=$6,succeeded=$7,failed=$8,last_error=$9,failure_details=$11,
status=CASE WHEN $4='running' THEN 'running' WHEN $4='completed' THEN 'completed' WHEN attempt<max_attempts THEN 'queued' ELSE 'failed' END,
available_at=CASE WHEN $4='failed' AND attempt<max_attempts THEN $12 ELSE available_at END,
lease_owner=CASE WHEN $4='running' THEN lease_owner ELSE '' END,
lease_expires_at=CASE WHEN $4='running' THEN $3 ELSE NULL END,
completed_at=CASE WHEN $4='running' OR ($4='failed' AND attempt<max_attempts) THEN NULL ELSE $2 END
WHERE id=$1 AND status='running' AND lease_owner=$10 AND lease_expires_at>=$2 RETURNING `+scanColumns,
		id, now.UTC(), now.UTC().Add(5*time.Minute), status, p.Expected, p.Processed, p.Succeeded, p.Failed, p.Error, worker, failurePayload, now.UTC().Add(repository.FleetScanRetryDelay)))
	if errors.Is(err, pgx.ErrNoRows) {
		if _, getErr := getScan(ctx, store.pool, id); errors.Is(getErr, repository.ErrNotFound) {
			return repository.FleetScan{}, getErr
		}
		return repository.FleetScan{}, repository.ErrFleetScanConflict
	}
	return record, err
}

func validateFleetList(options repository.FleetListOptions) error {
	if options.Page < 1 || options.PageSize < 1 || options.PageSize > 200 {
		return errors.New("page must be positive and pageSize must be between 1 and 200")
	}
	switch options.Status {
	case "", repository.FleetScanQueued, repository.FleetScanRunning, repository.FleetScanCompleted, repository.FleetScanFailed, repository.FleetScanCanceled:
		return nil
	default:
		return errors.New("unsupported fleet scan status")
	}
}

func (tx *transaction) CreatePolicy(ctx context.Context, m repository.PolicyMutation) (repository.PolicyRecord, error) {
	if err := tx.check(ctx); err != nil {
		return repository.PolicyRecord{}, err
	}
	return createPolicy(ctx, tx.tx, m)
}
func (tx *transaction) GetPolicy(ctx context.Context, id string) (repository.PolicyRecord, error) {
	if err := tx.check(ctx); err != nil {
		return repository.PolicyRecord{}, err
	}
	return getPolicy(ctx, tx.tx, id)
}
func (tx *transaction) ListPolicies(ctx context.Context, o repository.PolicyListOptions) (repository.PolicyListResult, error) {
	if err := tx.check(ctx); err != nil {
		return repository.PolicyListResult{}, err
	}
	return listPolicies(ctx, tx.tx, o)
}
func (tx *transaction) UpdatePolicy(ctx context.Context, id string, v int64, m repository.PolicyMutation) (repository.PolicyRecord, error) {
	if err := tx.check(ctx); err != nil {
		return repository.PolicyRecord{}, err
	}
	return updatePolicy(ctx, tx.tx, id, v, m)
}
func (tx *transaction) DeletePolicy(ctx context.Context, id string, v int64) error {
	if err := tx.check(ctx); err != nil {
		return err
	}
	return deletePolicy(ctx, tx.tx, id, v)
}
func (tx *transaction) ResolvePolicy(ctx context.Context, s repository.PolicyScope) (repository.PolicyRecord, error) {
	if err := tx.check(ctx); err != nil {
		return repository.PolicyRecord{}, err
	}
	return resolvePolicy(ctx, tx.tx, s)
}
func (tx *transaction) CreateFleetSchedule(ctx context.Context, m repository.FleetScheduleMutation) (repository.FleetSchedule, error) {
	if err := tx.check(ctx); err != nil {
		return repository.FleetSchedule{}, err
	}
	return createSchedule(ctx, tx.tx, m)
}
func (tx *transaction) GetFleetSchedule(ctx context.Context, id string) (repository.FleetSchedule, error) {
	if err := tx.check(ctx); err != nil {
		return repository.FleetSchedule{}, err
	}
	return getSchedule(ctx, tx.tx, id)
}
func (tx *transaction) ListFleetSchedules(ctx context.Context, o repository.FleetListOptions) (repository.FleetScheduleListResult, error) {
	if err := tx.check(ctx); err != nil {
		return repository.FleetScheduleListResult{}, err
	}
	return listSchedules(ctx, tx.tx, o)
}
func (tx *transaction) UpdateFleetSchedule(ctx context.Context, id string, v int64, m repository.FleetScheduleMutation) (repository.FleetSchedule, error) {
	if err := tx.check(ctx); err != nil {
		return repository.FleetSchedule{}, err
	}
	return updateSchedule(ctx, tx.tx, id, v, m)
}
func (tx *transaction) DeleteFleetSchedule(ctx context.Context, id string, v int64) error {
	if err := tx.check(ctx); err != nil {
		return err
	}
	return deleteSchedule(ctx, tx.tx, id, v)
}
func (tx *transaction) CreateFleetScan(ctx context.Context, r repository.FleetScanRequest) (repository.FleetScan, bool, error) {
	if err := tx.check(ctx); err != nil {
		return repository.FleetScan{}, false, err
	}
	return createScan(ctx, tx.tx, r, "")
}
func (tx *transaction) GetFleetScan(ctx context.Context, id string) (repository.FleetScan, error) {
	if err := tx.check(ctx); err != nil {
		return repository.FleetScan{}, err
	}
	return getScan(ctx, tx.tx, id)
}
func (tx *transaction) ListFleetScans(ctx context.Context, o repository.FleetListOptions) (repository.FleetScanListResult, error) {
	if err := tx.check(ctx); err != nil {
		return repository.FleetScanListResult{}, err
	}
	return listScans(ctx, tx.tx, o)
}
func (tx *transaction) CancelFleetScan(ctx context.Context, id string, t time.Time) (repository.FleetScan, error) {
	if err := tx.check(ctx); err != nil {
		return repository.FleetScan{}, err
	}
	return cancelScan(ctx, tx.tx, id, t)
}
func (tx *transaction) ResumeFleetScan(ctx context.Context, id string, t time.Time) (repository.FleetScan, error) {
	if err := tx.check(ctx); err != nil {
		return repository.FleetScan{}, err
	}
	return resumeScan(ctx, tx.tx, id, t)
}
func (tx *transaction) EnqueueDueFleetScans(context.Context, time.Time, int) ([]repository.FleetScan, error) {
	return nil, errors.New("enqueueing due scans is not supported inside a transaction")
}
func (tx *transaction) ClaimFleetScan(context.Context, string, time.Time, time.Duration) (repository.FleetScan, error) {
	return repository.FleetScan{}, errors.New("claiming scans is not supported inside a transaction")
}
func (tx *transaction) UpdateFleetScan(context.Context, string, string, time.Time, repository.FleetScanProgress) (repository.FleetScan, error) {
	return repository.FleetScan{}, errors.New("updating scan workers is not supported inside a transaction")
}
