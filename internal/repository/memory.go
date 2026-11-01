// Package repository defines KubeRight's persistence boundary and provides a
// bounded in-process implementation. The in-process store is concurrency-safe
// but is lost on restart and is not shared between replicas; production
// deployments should use the PostgreSQL implementation.
package repository

import (
	"cmp"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/kuberight/kuberight/internal/optimizer"
)

var errTransactionClosed = errors.New("transaction is no longer active")

// Memory is a bounded, process-local Store. Writers, including whole
// transactions, are serialized. A transaction that fails is rolled back by
// restoring a snapshot taken when it began. Concurrent readers may observe a
// transaction's uncommitted writes; this store makes no isolation guarantee
// beyond atomicity.
type Memory struct {
	writeMu        sync.Mutex
	mu             sync.RWMutex
	maximumRecords int
	state          memoryState
	now            func() time.Time
}

type memoryState struct {
	records     []storedRecommendation
	byID        map[string]int
	idempotency map[string]idempotencyRecord
	audit       []AuditEvent
}

type storedRecommendation struct {
	record         RecommendationRecord
	idempotencyKey string
}

type idempotencyRecord struct {
	recordID    string
	fingerprint [32]byte
}

var _ Store = (*Memory)(nil)

func NewMemory(maximumRecords int) *Memory {
	if maximumRecords <= 0 {
		maximumRecords = 10_000
	}
	return &Memory{
		maximumRecords: maximumRecords,
		state: memoryState{
			byID:        make(map[string]int),
			idempotency: make(map[string]idempotencyRecord),
		},
		now: time.Now,
	}
}

func (memory *Memory) Kind() string                { return "memory" }
func (memory *Memory) Ready(context.Context) error { return nil }

// NewRecommendationID returns a random identifier in the durable rec_ format.
func NewRecommendationID() (string, error) { return newID("rec_") }

// NewAuditEventID returns a random identifier in the durable evt_ format.
func NewAuditEventID() (string, error) { return newID("evt_") }

func newID(prefix string) (string, error) {
	random := make([]byte, 16)
	if _, err := rand.Read(random); err != nil {
		return "", err
	}
	return prefix + hex.EncodeToString(random), nil
}

func cloneRecord(record RecommendationRecord) RecommendationRecord {
	// slices.Clone keeps an empty slice empty, so it serializes as [] not null.
	record.Warnings = slices.Clone(record.Warnings)
	record.DataQuality.Issues = slices.Clone(record.DataQuality.Issues)
	if record.ReviewedAt != nil {
		reviewedAt := *record.ReviewedAt
		record.ReviewedAt = &reviewedAt
	}
	return record
}

func (state memoryState) clone() memoryState {
	cloned := memoryState{
		records:     append([]storedRecommendation(nil), state.records...),
		byID:        make(map[string]int, len(state.byID)),
		idempotency: make(map[string]idempotencyRecord, len(state.idempotency)),
		audit:       append([]AuditEvent(nil), state.audit...),
	}
	for key, value := range state.byID {
		cloned.byID[key] = value
	}
	for key, value := range state.idempotency {
		cloned.idempotency[key] = value
	}
	return cloned
}

// CreateRecommendation stores a recommendation. When an idempotency key is
// supplied, a repeated request returns the original record and created=false.
func (memory *Memory) CreateRecommendation(ctx context.Context, recommendation optimizer.Recommendation, options CreateOptions) (RecommendationRecord, bool, error) {
	if err := ctx.Err(); err != nil {
		return RecommendationRecord{}, false, err
	}
	memory.writeMu.Lock()
	defer memory.writeMu.Unlock()
	return memory.create(recommendation, options)
}

func (memory *Memory) create(recommendation optimizer.Recommendation, options CreateOptions) (RecommendationRecord, bool, error) {
	claim, err := NewIdempotencyClaim(options)
	if err != nil {
		return RecommendationRecord{}, false, err
	}
	source, err := NormalizeSource(options.Source)
	if err != nil {
		return RecommendationRecord{}, false, err
	}
	idempotencyMapKey := ""
	if claim != nil {
		idempotencyMapKey = hex.EncodeToString(claim.ScopeDigest[:]) + "\x00" + claim.Key
	}

	memory.mu.Lock()
	defer memory.mu.Unlock()
	state := &memory.state
	if existing, ok := state.idempotency[idempotencyMapKey]; claim != nil && ok {
		if existing.fingerprint != claim.RequestFingerprint {
			return RecommendationRecord{}, false, ErrIdempotencyConflict
		}
		return cloneRecord(state.records[state.byID[existing.recordID]].record), false, nil
	}
	id, err := NewRecommendationID()
	if err != nil {
		return RecommendationRecord{}, false, errors.New("could not generate recommendation identifier")
	}
	recommendation.ID = id
	record := RecommendationRecord{
		Recommendation: recommendation,
		CreatedAt:      memory.now().UTC(),
		CreatedBy:      options.CreatedBy,
		Source:         source,
		ReviewStatus:   ReviewPending,
	}
	if len(state.records) == memory.maximumRecords {
		evicted := state.records[0]
		delete(state.byID, evicted.record.ID)
		if evicted.idempotencyKey != "" {
			delete(state.idempotency, evicted.idempotencyKey)
		}
		state.records = append(state.records[:0], state.records[1:]...)
		for index := range state.records {
			state.byID[state.records[index].record.ID] = index
		}
	}
	state.records = append(state.records, storedRecommendation{record: cloneRecord(record), idempotencyKey: idempotencyMapKey})
	state.byID[id] = len(state.records) - 1
	if claim != nil {
		state.idempotency[idempotencyMapKey] = idempotencyRecord{recordID: id, fingerprint: claim.RequestFingerprint}
	}
	return cloneRecord(record), true, nil
}

func (memory *Memory) GetRecommendation(ctx context.Context, id string) (RecommendationRecord, error) {
	if err := ctx.Err(); err != nil {
		return RecommendationRecord{}, err
	}
	memory.mu.RLock()
	defer memory.mu.RUnlock()
	index, ok := memory.state.byID[id]
	if !ok {
		return RecommendationRecord{}, ErrNotFound
	}
	return cloneRecord(memory.state.records[index].record), nil
}

func (memory *Memory) CountRecommendations(ctx context.Context) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	memory.mu.RLock()
	defer memory.mu.RUnlock()
	return len(memory.state.records), nil
}

func (memory *Memory) ReviewRecommendation(ctx context.Context, id string, decision ReviewDecision) (RecommendationRecord, error) {
	if err := ctx.Err(); err != nil {
		return RecommendationRecord{}, err
	}
	memory.writeMu.Lock()
	defer memory.writeMu.Unlock()
	return memory.review(id, decision)
}

func (memory *Memory) review(id string, decision ReviewDecision) (RecommendationRecord, error) {
	if err := decision.Validate(); err != nil {
		return RecommendationRecord{}, err
	}
	memory.mu.Lock()
	defer memory.mu.Unlock()
	index, ok := memory.state.byID[id]
	if !ok {
		return RecommendationRecord{}, ErrNotFound
	}
	record := memory.state.records[index].record
	if record.ReviewStatus != ReviewPending {
		return RecommendationRecord{}, ErrReviewConflict
	}
	reviewedAt := decision.At.UTC()
	record.ReviewStatus = decision.Status
	record.ReviewedBy = decision.Reviewer
	record.ReviewedAt = &reviewedAt
	record.ReviewComment = decision.Comment
	memory.state.records[index].record = record
	return cloneRecord(record), nil
}

func (memory *Memory) ListRecommendations(ctx context.Context, options ListOptions) (ListResult, error) {
	if err := ctx.Err(); err != nil {
		return ListResult{}, err
	}
	if err := ValidateListOptions(options); err != nil {
		return ListResult{}, err
	}
	filter := strings.ToLower(strings.TrimSpace(options.Filter))
	seen := make(map[string]struct{})
	memory.mu.RLock()
	filtered := make([]RecommendationRecord, 0, len(memory.state.records))
	// Iterate newest first so LatestOnly keeps the most recent record.
	for index := len(memory.state.records) - 1; index >= 0; index-- {
		record := memory.state.records[index].record
		if options.LatestOnly {
			key := latestKey(record)
			if _, duplicate := seen[key]; duplicate {
				continue
			}
			seen[key] = struct{}{}
		}
		if options.Namespace != "" && record.Namespace != options.Namespace || options.Workload != "" && record.Workload != options.Workload ||
			options.Confidence != "" && record.Confidence != options.Confidence || options.Risk != "" && record.Risk != options.Risk ||
			options.ReviewStatus != "" && record.ReviewStatus != options.ReviewStatus {
			continue
		}
		if filter != "" &&
			!strings.Contains(strings.ToLower(record.Cluster), filter) &&
			!strings.Contains(strings.ToLower(record.Namespace), filter) &&
			!strings.Contains(strings.ToLower(record.Workload), filter) &&
			!strings.Contains(strings.ToLower(record.Container), filter) {
			continue
		}
		filtered = append(filtered, cloneRecord(record))
	}
	memory.mu.RUnlock()

	sort.SliceStable(filtered, func(left, right int) bool {
		comparison := 0
		switch options.Sort {
		case "namespace":
			comparison = cmp.Compare(filtered[left].Namespace, filtered[right].Namespace)
		case "workload":
			comparison = cmp.Compare(filtered[left].Workload, filtered[right].Workload)
		case "confidence":
			comparison = cmp.Compare(filtered[left].ConfidenceScore, filtered[right].ConfidenceScore)
		case "risk":
			comparison = cmp.Compare(levelRank(filtered[left].Risk), levelRank(filtered[right].Risk))
		default:
			comparison = filtered[left].CreatedAt.Compare(filtered[right].CreatedAt)
		}
		if comparison == 0 {
			comparison = cmp.Compare(filtered[left].ID, filtered[right].ID)
		}
		if options.Order == "asc" {
			return comparison < 0
		}
		return comparison > 0
	})

	start := min((options.Page-1)*options.PageSize, len(filtered))
	end := min(start+options.PageSize, len(filtered))
	return ListResult{Items: filtered[start:end], Page: options.Page, PageSize: options.PageSize, Total: len(filtered)}, nil
}

func (memory *Memory) AppendAuditEvent(ctx context.Context, event AuditEvent) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	memory.writeMu.Lock()
	defer memory.writeMu.Unlock()
	return memory.appendAudit(event)
}

func (memory *Memory) appendAudit(event AuditEvent) error {
	if event.ID == "" {
		id, err := NewAuditEventID()
		if err != nil {
			return errors.New("could not generate audit event identifier")
		}
		event.ID = id
	}
	if event.Timestamp.IsZero() {
		event.Timestamp = memory.now().UTC()
	}
	memory.mu.Lock()
	defer memory.mu.Unlock()
	if len(memory.state.audit) == memory.maximumRecords {
		memory.state.audit = append(memory.state.audit[:0], memory.state.audit[1:]...)
	}
	memory.state.audit = append(memory.state.audit, event)
	return nil
}

func (memory *Memory) ListAuditEvents(ctx context.Context, page, pageSize int) (AuditListResult, error) {
	if err := ctx.Err(); err != nil {
		return AuditListResult{}, err
	}
	if page < 1 || pageSize < 1 {
		return AuditListResult{}, errors.New("page and pageSize must be positive")
	}
	memory.mu.RLock()
	defer memory.mu.RUnlock()
	total := len(memory.state.audit)
	start := min((page-1)*pageSize, total)
	end := min(start+pageSize, total)
	items := make([]AuditEvent, 0, end-start)
	for index := total - start - 1; index >= total-end; index-- {
		items = append(items, memory.state.audit[index])
	}
	return AuditListResult{Items: items, Page: page, PageSize: pageSize, Total: total}, nil
}

// InTransaction runs fn with exclusive write access. Writes are rolled back
// when fn returns an error, the context is canceled, or fn panics.
func (memory *Memory) InTransaction(ctx context.Context, fn func(context.Context, Transaction) error) (err error) {
	if err := ctx.Err(); err != nil {
		return err
	}
	memory.writeMu.Lock()
	defer memory.writeMu.Unlock()

	memory.mu.RLock()
	snapshot := memory.state.clone()
	memory.mu.RUnlock()
	rollback := func() {
		memory.mu.Lock()
		memory.state = snapshot
		memory.mu.Unlock()
	}

	tx := &memoryTx{memory: memory}
	tx.active.Store(true)
	defer tx.active.Store(false)
	defer func() {
		if recovered := recover(); recovered != nil {
			rollback()
			panic(recovered)
		}
	}()
	if err = fn(ctx, tx); err != nil {
		rollback()
		return err
	}
	if err = ctx.Err(); err != nil {
		rollback()
		return err
	}
	return nil
}

// memoryTx performs writes without re-acquiring the writer lock held by
// InTransaction. It rejects use after the transaction callback returns.
type memoryTx struct {
	memory *Memory
	active atomic.Bool
}

func (tx *memoryTx) check(ctx context.Context) error {
	if !tx.active.Load() {
		return errTransactionClosed
	}
	return ctx.Err()
}

func (tx *memoryTx) CreateRecommendation(ctx context.Context, recommendation optimizer.Recommendation, options CreateOptions) (RecommendationRecord, bool, error) {
	if err := tx.check(ctx); err != nil {
		return RecommendationRecord{}, false, err
	}
	return tx.memory.create(recommendation, options)
}

func (tx *memoryTx) GetRecommendation(ctx context.Context, id string) (RecommendationRecord, error) {
	if err := tx.check(ctx); err != nil {
		return RecommendationRecord{}, err
	}
	return tx.memory.GetRecommendation(ctx, id)
}

func (tx *memoryTx) ListRecommendations(ctx context.Context, options ListOptions) (ListResult, error) {
	if err := tx.check(ctx); err != nil {
		return ListResult{}, err
	}
	return tx.memory.ListRecommendations(ctx, options)
}

func (tx *memoryTx) CountRecommendations(ctx context.Context) (int, error) {
	if err := tx.check(ctx); err != nil {
		return 0, err
	}
	return tx.memory.CountRecommendations(ctx)
}

func (tx *memoryTx) ReviewRecommendation(ctx context.Context, id string, decision ReviewDecision) (RecommendationRecord, error) {
	if err := tx.check(ctx); err != nil {
		return RecommendationRecord{}, err
	}
	return tx.memory.review(id, decision)
}

func (tx *memoryTx) AppendAuditEvent(ctx context.Context, event AuditEvent) error {
	if err := tx.check(ctx); err != nil {
		return err
	}
	return tx.memory.appendAudit(event)
}

func (tx *memoryTx) ListAuditEvents(ctx context.Context, page, pageSize int) (AuditListResult, error) {
	if err := tx.check(ctx); err != nil {
		return AuditListResult{}, err
	}
	return tx.memory.ListAuditEvents(ctx, page, pageSize)
}
