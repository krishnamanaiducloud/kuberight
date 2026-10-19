// Package repository provides bounded in-process state for recommendation and
// audit history. It is concurrency-safe, but intentionally not a replacement
// for durable production persistence.
package repository

import (
	"cmp"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/kuberight/kuberight/internal/optimizer"
)

const maxIdempotencyKeyLength = 128

// ErrIdempotencyConflict is returned when a caller reuses an idempotency key
// in the same scope for a semantically different request.
var ErrIdempotencyConflict = errors.New("idempotency key was already used for a different request")

type RecommendationRecord struct {
	optimizer.Recommendation
	CreatedAt time.Time `json:"createdAt"`
}

type AuditEvent struct {
	ID        string    `json:"id"`
	Timestamp time.Time `json:"timestamp"`
	Actor     string    `json:"actor"`
	Action    string    `json:"action"`
	Resource  string    `json:"resource"`
	Namespace string    `json:"namespace,omitempty"`
	Outcome   string    `json:"outcome"`
	RequestID string    `json:"requestId"`
}

type ListOptions struct {
	Page       int
	PageSize   int
	Namespace  string
	Workload   string
	Confidence string
	Risk       string
	Filter     string
	Sort       string
	Order      string
}

type CreateOptions struct {
	IdempotencyKey string
	Scope          string
	Fingerprint    string
}

type ListResult struct {
	Items    []RecommendationRecord
	Page     int
	PageSize int
	Total    int
}

type Memory struct {
	mu             sync.RWMutex
	maximumRecords int
	records        []storedRecommendation
	byID           map[string]int
	idempotency    map[string]idempotencyRecord
	audit          []AuditEvent
	now            func() time.Time
}

type storedRecommendation struct {
	record         RecommendationRecord
	idempotencyKey string
}

type idempotencyRecord struct {
	recordID    string
	fingerprint string
}

func NewMemory(maximumRecords int) *Memory {
	if maximumRecords <= 0 {
		maximumRecords = 10_000
	}
	return &Memory{
		maximumRecords: maximumRecords,
		byID:           make(map[string]int),
		idempotency:    make(map[string]idempotencyRecord),
		now:            time.Now,
	}
}

func validIdempotencyKey(key string) bool {
	if len(key) == 0 || len(key) > maxIdempotencyKeyLength {
		return false
	}
	for _, character := range key {
		if (character >= 'a' && character <= 'z') || (character >= 'A' && character <= 'Z') || (character >= '0' && character <= '9') || strings.ContainsRune("._:-", character) {
			continue
		}
		return false
	}
	return true
}

func newID(prefix string) (string, error) {
	random := make([]byte, 16)
	if _, err := rand.Read(random); err != nil {
		return "", err
	}
	return prefix + hex.EncodeToString(random), nil
}

func cloneRecord(record RecommendationRecord) RecommendationRecord {
	record.Warnings = append([]string(nil), record.Warnings...)
	record.DataQuality.Issues = append([]string(nil), record.DataQuality.Issues...)
	return record
}

// Create stores a recommendation. When an idempotency key is supplied, a
// repeated request returns the original record and created=false.
func (memory *Memory) Create(recommendation optimizer.Recommendation, options CreateOptions) (record RecommendationRecord, created bool, err error) {
	idempotencyKey := options.IdempotencyKey
	if idempotencyKey != "" && !validIdempotencyKey(idempotencyKey) {
		return RecommendationRecord{}, false, errors.New("idempotency key contains unsupported characters or is too long")
	}
	idempotencyMapKey := ""
	if idempotencyKey != "" {
		if strings.TrimSpace(options.Scope) == "" || strings.TrimSpace(options.Fingerprint) == "" {
			return RecommendationRecord{}, false, errors.New("idempotency scope and request fingerprint are required")
		}
		digest := sha256.Sum256([]byte(options.Scope + "\x00" + idempotencyKey))
		idempotencyMapKey = hex.EncodeToString(digest[:])
	}
	memory.mu.Lock()
	defer memory.mu.Unlock()
	if existing, ok := memory.idempotency[idempotencyMapKey]; idempotencyMapKey != "" && ok {
		if existing.fingerprint != options.Fingerprint {
			return RecommendationRecord{}, false, ErrIdempotencyConflict
		}
		return cloneRecord(memory.records[memory.byID[existing.recordID]].record), false, nil
	}
	id, err := newID("rec_")
	if err != nil {
		return RecommendationRecord{}, false, errors.New("could not generate recommendation identifier")
	}
	recommendation.ID = id
	record = RecommendationRecord{Recommendation: recommendation, CreatedAt: memory.now().UTC()}
	if len(memory.records) == memory.maximumRecords {
		evicted := memory.records[0]
		delete(memory.byID, evicted.record.ID)
		if evicted.idempotencyKey != "" {
			delete(memory.idempotency, evicted.idempotencyKey)
		}
		memory.records = append(memory.records[:0], memory.records[1:]...)
		for index := range memory.records {
			memory.byID[memory.records[index].record.ID] = index
		}
	}
	memory.records = append(memory.records, storedRecommendation{record: cloneRecord(record), idempotencyKey: idempotencyMapKey})
	memory.byID[id] = len(memory.records) - 1
	if idempotencyMapKey != "" {
		memory.idempotency[idempotencyMapKey] = idempotencyRecord{recordID: id, fingerprint: options.Fingerprint}
	}
	return cloneRecord(record), true, nil
}

func (memory *Memory) Get(id string) (RecommendationRecord, bool) {
	memory.mu.RLock()
	defer memory.mu.RUnlock()
	index, ok := memory.byID[id]
	if !ok {
		return RecommendationRecord{}, false
	}
	return cloneRecord(memory.records[index].record), true
}

func (memory *Memory) Count() int {
	memory.mu.RLock()
	defer memory.mu.RUnlock()
	return len(memory.records)
}

func (memory *Memory) List(options ListOptions) ListResult {
	memory.mu.RLock()
	filtered := make([]RecommendationRecord, 0, len(memory.records))
	for _, stored := range memory.records {
		record := stored.record
		if options.Namespace != "" && record.Namespace != options.Namespace || options.Workload != "" && record.Workload != options.Workload || options.Confidence != "" && record.Confidence != options.Confidence || options.Risk != "" && record.Risk != options.Risk {
			continue
		}
		if filter := strings.ToLower(strings.TrimSpace(options.Filter)); filter != "" &&
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

	start := (options.Page - 1) * options.PageSize
	if start > len(filtered) {
		start = len(filtered)
	}
	end := min(start+options.PageSize, len(filtered))
	return ListResult{Items: filtered[start:end], Page: options.Page, PageSize: options.PageSize, Total: len(filtered)}
}

func levelRank(level string) int {
	switch strings.ToLower(level) {
	case "low":
		return 1
	case "medium":
		return 2
	case "high":
		return 3
	default:
		return 0
	}
}

func (memory *Memory) AddAudit(event AuditEvent) error {
	id, err := newID("evt_")
	if err != nil {
		return errors.New("could not generate audit event identifier")
	}
	event.ID = id
	if event.Timestamp.IsZero() {
		event.Timestamp = memory.now().UTC()
	}
	memory.mu.Lock()
	defer memory.mu.Unlock()
	if len(memory.audit) == memory.maximumRecords {
		memory.audit = append(memory.audit[:0], memory.audit[1:]...)
	}
	memory.audit = append(memory.audit, event)
	return nil
}

func (memory *Memory) ListAudit(page, pageSize int) ([]AuditEvent, int) {
	memory.mu.RLock()
	defer memory.mu.RUnlock()
	total := len(memory.audit)
	start := (page - 1) * pageSize
	if start > total {
		start = total
	}
	end := min(start+pageSize, total)
	items := make([]AuditEvent, 0, end-start)
	for index := total - start - 1; index >= total-end; index-- {
		items = append(items, memory.audit[index])
	}
	return items, total
}
