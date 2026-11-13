package repository

import (
	"context"
	"sync"
)

// Locker coordinates singleton work across API replicas. TryLock never
// waits: it reports false when another holder owns the name.
type Locker interface {
	TryLock(ctx context.Context, name string) (release func(), acquired bool, err error)
}

// processLocks serves the in-process store, which by construction has one
// replica; it still prevents overlapping runs inside that process.
var processLocks sync.Map

// TryLock implements Locker for the in-process store.
func (memory *Memory) TryLock(ctx context.Context, name string) (func(), bool, error) {
	if err := ctx.Err(); err != nil {
		return nil, false, err
	}
	key := memory.lockKey(name)
	if _, loaded := processLocks.LoadOrStore(key, struct{}{}); loaded {
		return nil, false, nil
	}
	var once sync.Once
	return func() { once.Do(func() { processLocks.Delete(key) }) }, true, nil
}

type memoryLockKey struct {
	store *Memory
	name  string
}

func (memory *Memory) lockKey(name string) memoryLockKey {
	return memoryLockKey{store: memory, name: name}
}
