package postgres

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kuberight/kuberight/internal/optimizer"
	"github.com/kuberight/kuberight/internal/repository"
)

var errTransactionClosed = errors.New("transaction is no longer active")

// Options bounds the connection pool. Zero values select safe defaults.
type Options struct {
	MaxConns       int32
	ConnectTimeout time.Duration
}

// Store is a PostgreSQL-backed repository.Store. It is safe for concurrent
// use and may be shared by several API replicas.
type Store struct {
	pool *pgxpool.Pool
}

var _ repository.Store = (*Store)(nil)

// Open creates a connection pool and verifies connectivity. The connection
// string may contain a password; it is never logged or included in errors
// beyond pgx's own redacted form.
func Open(ctx context.Context, connectionString string, options Options) (*Store, error) {
	config, err := pgxpool.ParseConfig(connectionString)
	if err != nil {
		return nil, fmt.Errorf("parse database connection string: %w", err)
	}
	config.MaxConns = 10
	if options.MaxConns > 0 {
		config.MaxConns = options.MaxConns
	}
	config.ConnConfig.ConnectTimeout = 10 * time.Second
	if options.ConnectTimeout > 0 {
		config.ConnConfig.ConnectTimeout = options.ConnectTimeout
	}
	if _, set := config.ConnConfig.RuntimeParams["application_name"]; !set {
		config.ConnConfig.RuntimeParams["application_name"] = "kuberight"
	}
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		return nil, fmt.Errorf("create database pool: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("connect to database: %w", err)
	}
	return &Store{pool: pool}, nil
}

// Pool exposes the underlying pool for migrations.
func (store *Store) Pool() *pgxpool.Pool { return store.pool }

// Close releases every pooled connection.
func (store *Store) Close() { store.pool.Close() }

func (store *Store) Kind() string { return "postgres" }

// Ready confirms connectivity and that the schema matches this binary.
func (store *Store) Ready(ctx context.Context) error {
	if err := store.pool.Ping(ctx); err != nil {
		return err
	}
	return VerifySchema(ctx, store.pool)
}

type queryer interface {
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
	Query(context.Context, string, ...any) (pgx.Rows, error)
	QueryRow(context.Context, string, ...any) pgx.Row
}

// InTransaction commits only when fn returns nil and the context is still
// active. Rollback uses a non-canceled context so a canceled request cannot
// strand an open transaction; a panic in fn rolls back and propagates.
func (store *Store) InTransaction(ctx context.Context, fn func(context.Context, repository.Transaction) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	tx, err := store.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}
	wrapped := &transaction{tx: tx}
	wrapped.active.Store(true)
	committed := false
	defer func() {
		wrapped.active.Store(false)
		if !committed {
			rollbackContext, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			defer cancel()
			_ = tx.Rollback(rollbackContext)
		}
	}()
	if err := fn(ctx, wrapped); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	wrapped.active.Store(false)
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit transaction: %w", err)
	}
	committed = true
	return nil
}

func (store *Store) CreateRecommendation(ctx context.Context, recommendation optimizer.Recommendation, options repository.CreateOptions) (record repository.RecommendationRecord, created bool, err error) {
	err = store.InTransaction(ctx, func(ctx context.Context, tx repository.Transaction) error {
		record, created, err = tx.CreateRecommendation(ctx, recommendation, options)
		return err
	})
	return record, created, err
}

func (store *Store) GetRecommendation(ctx context.Context, id string) (repository.RecommendationRecord, error) {
	return getRecommendation(ctx, store.pool, id)
}

func (store *Store) ListRecommendations(ctx context.Context, options repository.ListOptions) (repository.ListResult, error) {
	return listRecommendations(ctx, store.pool, options)
}

func (store *Store) CountRecommendations(ctx context.Context) (int, error) {
	return countRecommendations(ctx, store.pool)
}

func (store *Store) ReviewRecommendation(ctx context.Context, id string, decision repository.ReviewDecision) (record repository.RecommendationRecord, err error) {
	err = store.InTransaction(ctx, func(ctx context.Context, tx repository.Transaction) error {
		record, err = tx.ReviewRecommendation(ctx, id, decision)
		return err
	})
	return record, err
}

func (store *Store) AppendAuditEvent(ctx context.Context, event repository.AuditEvent) error {
	return appendAuditEvent(ctx, store.pool, event)
}

func (store *Store) ListAuditEvents(ctx context.Context, page, pageSize int) (repository.AuditListResult, error) {
	return listAuditEvents(ctx, store.pool, page, pageSize)
}

// transaction rejects use after InTransaction's callback returns.
type transaction struct {
	tx     pgx.Tx
	active atomic.Bool
}

func (tx *transaction) check(ctx context.Context) error {
	if !tx.active.Load() {
		return errTransactionClosed
	}
	return ctx.Err()
}

func (tx *transaction) CreateRecommendation(ctx context.Context, recommendation optimizer.Recommendation, options repository.CreateOptions) (repository.RecommendationRecord, bool, error) {
	if err := tx.check(ctx); err != nil {
		return repository.RecommendationRecord{}, false, err
	}
	return createRecommendation(ctx, tx.tx, recommendation, options)
}

func (tx *transaction) GetRecommendation(ctx context.Context, id string) (repository.RecommendationRecord, error) {
	if err := tx.check(ctx); err != nil {
		return repository.RecommendationRecord{}, err
	}
	return getRecommendation(ctx, tx.tx, id)
}

func (tx *transaction) ListRecommendations(ctx context.Context, options repository.ListOptions) (repository.ListResult, error) {
	if err := tx.check(ctx); err != nil {
		return repository.ListResult{}, err
	}
	return listRecommendations(ctx, tx.tx, options)
}

func (tx *transaction) CountRecommendations(ctx context.Context) (int, error) {
	if err := tx.check(ctx); err != nil {
		return 0, err
	}
	return countRecommendations(ctx, tx.tx)
}

func (tx *transaction) ReviewRecommendation(ctx context.Context, id string, decision repository.ReviewDecision) (repository.RecommendationRecord, error) {
	if err := tx.check(ctx); err != nil {
		return repository.RecommendationRecord{}, err
	}
	return reviewRecommendation(ctx, tx.tx, id, decision)
}

func (tx *transaction) AppendAuditEvent(ctx context.Context, event repository.AuditEvent) error {
	if err := tx.check(ctx); err != nil {
		return err
	}
	return appendAuditEvent(ctx, tx.tx, event)
}

func (tx *transaction) ListAuditEvents(ctx context.Context, page, pageSize int) (repository.AuditListResult, error) {
	if err := tx.check(ctx); err != nil {
		return repository.AuditListResult{}, err
	}
	return listAuditEvents(ctx, tx.tx, page, pageSize)
}

const recommendationColumns = "payload, created_at, created_by, source, review_status, reviewed_by, reviewed_at, review_comment"

// createRecommendation must run inside a transaction. The idempotency claim is
// inserted first; the deferred foreign key lets concurrent requests contend on
// the claim's primary key before either writes a recommendation.
func createRecommendation(ctx context.Context, db queryer, recommendation optimizer.Recommendation, options repository.CreateOptions) (repository.RecommendationRecord, bool, error) {
	claim, err := repository.NewIdempotencyClaim(options)
	if err != nil {
		return repository.RecommendationRecord{}, false, err
	}
	source, err := repository.NormalizeSource(options.Source)
	if err != nil {
		return repository.RecommendationRecord{}, false, err
	}
	id, err := repository.NewRecommendationID()
	if err != nil {
		return repository.RecommendationRecord{}, false, errors.New("could not generate recommendation identifier")
	}
	if claim != nil {
		tag, err := db.Exec(ctx, `INSERT INTO recommendation_idempotency (scope_digest, idempotency_key, request_fingerprint, recommendation_id)
VALUES ($1, $2, $3, $4) ON CONFLICT (scope_digest, idempotency_key) DO NOTHING`,
			claim.ScopeDigest[:], claim.Key, claim.RequestFingerprint[:], id)
		if err != nil {
			return repository.RecommendationRecord{}, false, fmt.Errorf("claim idempotency key: %w", err)
		}
		if tag.RowsAffected() == 0 {
			var fingerprint []byte
			var existingID string
			if err := db.QueryRow(ctx, "SELECT request_fingerprint, recommendation_id FROM recommendation_idempotency WHERE scope_digest = $1 AND idempotency_key = $2",
				claim.ScopeDigest[:], claim.Key).Scan(&fingerprint, &existingID); err != nil {
				return repository.RecommendationRecord{}, false, fmt.Errorf("read idempotency claim: %w", err)
			}
			if !bytes.Equal(fingerprint, claim.RequestFingerprint[:]) {
				return repository.RecommendationRecord{}, false, repository.ErrIdempotencyConflict
			}
			record, err := getRecommendation(ctx, db, existingID)
			return record, false, err
		}
	}

	recommendation.ID = id
	payload, err := json.Marshal(recommendation)
	if err != nil {
		return repository.RecommendationRecord{}, false, fmt.Errorf("encode recommendation: %w", err)
	}
	record := repository.RecommendationRecord{
		Recommendation: recommendation,
		CreatedBy:      options.CreatedBy,
		Source:         source,
		ReviewStatus:   repository.ReviewPending,
	}
	if err := db.QueryRow(ctx, `INSERT INTO recommendations
(id, cluster, namespace, workload_kind, workload, container, confidence, risk, payload, created_by, source)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11) RETURNING created_at`,
		id, recommendation.Cluster, recommendation.Namespace, recommendation.Kind, recommendation.Workload, recommendation.Container,
		recommendation.Confidence, recommendation.Risk, payload, options.CreatedBy, source,
	).Scan(&record.CreatedAt); err != nil {
		return repository.RecommendationRecord{}, false, fmt.Errorf("insert recommendation: %w", err)
	}
	record.CreatedAt = record.CreatedAt.UTC()
	return record, true, nil
}

func scanRecommendation(row pgx.Row) (repository.RecommendationRecord, error) {
	var record repository.RecommendationRecord
	var payload []byte
	var reviewedBy, reviewComment *string
	var reviewedAt *time.Time
	if err := row.Scan(&payload, &record.CreatedAt, &record.CreatedBy, &record.Source, &record.ReviewStatus, &reviewedBy, &reviewedAt, &reviewComment); err != nil {
		return repository.RecommendationRecord{}, err
	}
	if err := json.Unmarshal(payload, &record.Recommendation); err != nil {
		return repository.RecommendationRecord{}, fmt.Errorf("decode stored recommendation: %w", err)
	}
	record.CreatedAt = record.CreatedAt.UTC()
	if reviewedBy != nil {
		record.ReviewedBy = *reviewedBy
	}
	if reviewedAt != nil {
		utc := reviewedAt.UTC()
		record.ReviewedAt = &utc
	}
	if reviewComment != nil {
		record.ReviewComment = *reviewComment
	}
	if record.Warnings == nil {
		record.Warnings = []string{}
	}
	if record.DataQuality.Issues == nil {
		record.DataQuality.Issues = []string{}
	}
	return record, nil
}

func getRecommendation(ctx context.Context, db queryer, id string) (repository.RecommendationRecord, error) {
	record, err := scanRecommendation(db.QueryRow(ctx, "SELECT "+recommendationColumns+" FROM recommendations WHERE id = $1", id))
	if errors.Is(err, pgx.ErrNoRows) {
		return repository.RecommendationRecord{}, repository.ErrNotFound
	}
	if err != nil {
		return repository.RecommendationRecord{}, fmt.Errorf("read recommendation: %w", err)
	}
	return record, nil
}

func countRecommendations(ctx context.Context, db queryer) (int, error) {
	var count int64
	if err := db.QueryRow(ctx, "SELECT count(*) FROM recommendations").Scan(&count); err != nil {
		return 0, fmt.Errorf("count recommendations: %w", err)
	}
	return int(count), nil
}

func escapeLike(value string) string {
	return strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(value)
}

// sortExpressions map validated sort names to fixed SQL. User input never
// reaches the query text; text columns use byte order to match the memory
// store regardless of database collation.
var sortExpressions = map[string]string{
	"":           "created_at",
	"createdAt":  "created_at",
	"namespace":  `namespace COLLATE "C"`,
	"workload":   `workload COLLATE "C"`,
	"confidence": "(payload ->> 'confidenceScore')::double precision",
	"risk":       "CASE risk WHEN 'low' THEN 1 WHEN 'medium' THEN 2 WHEN 'high' THEN 3 ELSE 0 END",
}

func listRecommendations(ctx context.Context, db queryer, options repository.ListOptions) (repository.ListResult, error) {
	if err := repository.ValidateListOptions(options); err != nil {
		return repository.ListResult{}, err
	}
	source := "recommendations"
	if options.LatestOnly {
		source = `(SELECT DISTINCT ON (cluster, namespace, workload_kind, workload, container) *
FROM recommendations
ORDER BY cluster, namespace, workload_kind, workload, container, created_at DESC, id DESC) AS latest`
	}
	conditions := make([]string, 0, 6)
	arguments := make([]any, 0, 8)
	add := func(condition string, value any) {
		arguments = append(arguments, value)
		conditions = append(conditions, strings.ReplaceAll(condition, "?", "$"+strconv.Itoa(len(arguments))))
	}
	if options.Namespace != "" {
		add("namespace = ?", options.Namespace)
	}
	if options.Workload != "" {
		add("workload = ?", options.Workload)
	}
	if options.Confidence != "" {
		add("confidence = ?", options.Confidence)
	}
	if options.Risk != "" {
		add("risk = ?", options.Risk)
	}
	if options.ReviewStatus != "" {
		add("review_status = ?", options.ReviewStatus)
	}
	if filter := strings.TrimSpace(options.Filter); filter != "" {
		add(`(cluster ILIKE ? ESCAPE '\' OR namespace ILIKE ? ESCAPE '\' OR workload ILIKE ? ESCAPE '\' OR container ILIKE ? ESCAPE '\')`, "%"+escapeLike(filter)+"%")
	}
	where := ""
	if len(conditions) > 0 {
		where = " WHERE " + strings.Join(conditions, " AND ")
	}

	var total int64
	if err := db.QueryRow(ctx, "SELECT count(*) FROM "+source+where, arguments...).Scan(&total); err != nil {
		return repository.ListResult{}, fmt.Errorf("count recommendations: %w", err)
	}
	direction := "DESC"
	if options.Order == "asc" {
		direction = "ASC"
	}
	query := "SELECT " + recommendationColumns + " FROM " + source + where +
		" ORDER BY " + sortExpressions[options.Sort] + " " + direction + ", id " + direction +
		" LIMIT $" + strconv.Itoa(len(arguments)+1) + " OFFSET $" + strconv.Itoa(len(arguments)+2)
	arguments = append(arguments, options.PageSize, int64(options.Page-1)*int64(options.PageSize))
	rows, err := db.Query(ctx, query, arguments...)
	if err != nil {
		return repository.ListResult{}, fmt.Errorf("list recommendations: %w", err)
	}
	items, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (repository.RecommendationRecord, error) {
		return scanRecommendation(row)
	})
	if err != nil {
		return repository.ListResult{}, fmt.Errorf("list recommendations: %w", err)
	}
	return repository.ListResult{Items: items, Page: options.Page, PageSize: options.PageSize, Total: int(total)}, nil
}

func reviewRecommendation(ctx context.Context, db queryer, id string, decision repository.ReviewDecision) (repository.RecommendationRecord, error) {
	if err := decision.Validate(); err != nil {
		return repository.RecommendationRecord{}, err
	}
	var comment *string
	if decision.Comment != "" {
		comment = &decision.Comment
	}
	record, err := scanRecommendation(db.QueryRow(ctx, `UPDATE recommendations
SET review_status = $2, reviewed_by = $3, reviewed_at = $4, review_comment = $5
WHERE id = $1 AND review_status = 'pending'
RETURNING `+recommendationColumns, id, decision.Status, decision.Reviewer, decision.At.UTC(), comment))
	if err == nil {
		return record, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return repository.RecommendationRecord{}, fmt.Errorf("review recommendation: %w", err)
	}
	var exists bool
	if err := db.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM recommendations WHERE id = $1)", id).Scan(&exists); err != nil {
		return repository.RecommendationRecord{}, fmt.Errorf("review recommendation: %w", err)
	}
	if !exists {
		return repository.RecommendationRecord{}, repository.ErrNotFound
	}
	return repository.RecommendationRecord{}, repository.ErrReviewConflict
}

func appendAuditEvent(ctx context.Context, db queryer, event repository.AuditEvent) error {
	if event.ID == "" {
		id, err := repository.NewAuditEventID()
		if err != nil {
			return errors.New("could not generate audit event identifier")
		}
		event.ID = id
	}
	if event.Timestamp.IsZero() {
		event.Timestamp = time.Now()
	}
	var namespace *string
	if event.Namespace != "" {
		namespace = &event.Namespace
	}
	if _, err := db.Exec(ctx, `INSERT INTO audit_events (id, event_timestamp, actor, action, resource, namespace, outcome, request_id)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
		event.ID, event.Timestamp.UTC(), event.Actor, event.Action, event.Resource, namespace, event.Outcome, event.RequestID); err != nil {
		return fmt.Errorf("append audit event: %w", err)
	}
	return nil
}

func listAuditEvents(ctx context.Context, db queryer, page, pageSize int) (repository.AuditListResult, error) {
	if page < 1 || pageSize < 1 || pageSize > 10_000 {
		return repository.AuditListResult{}, errors.New("page must be positive and pageSize must be between 1 and 10000")
	}
	var total int64
	if err := db.QueryRow(ctx, "SELECT count(*) FROM audit_events").Scan(&total); err != nil {
		return repository.AuditListResult{}, fmt.Errorf("count audit events: %w", err)
	}
	rows, err := db.Query(ctx, `SELECT id, event_timestamp, actor, action, resource, namespace, outcome, request_id
FROM audit_events ORDER BY event_timestamp DESC, id DESC LIMIT $1 OFFSET $2`, pageSize, int64(page-1)*int64(pageSize))
	if err != nil {
		return repository.AuditListResult{}, fmt.Errorf("list audit events: %w", err)
	}
	items, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (repository.AuditEvent, error) {
		var event repository.AuditEvent
		var namespace *string
		if err := row.Scan(&event.ID, &event.Timestamp, &event.Actor, &event.Action, &event.Resource, &namespace, &event.Outcome, &event.RequestID); err != nil {
			return repository.AuditEvent{}, err
		}
		event.Timestamp = event.Timestamp.UTC()
		if namespace != nil {
			event.Namespace = *namespace
		}
		return event, nil
	})
	if err != nil {
		return repository.AuditListResult{}, fmt.Errorf("list audit events: %w", err)
	}
	return repository.AuditListResult{Items: items, Page: page, PageSize: pageSize, Total: int(total)}, nil
}
