package postgres

import (
	"context"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"sync"
	"time"
)

// lockNamespace keeps KubeRight's named locks apart from the migration lock
// and from any other application sharing the database.
const lockNamespace = "kuberight-named-lock/v1\x00"

func lockKey(name string) int64 {
	hash := fnv.New64a()
	_, _ = hash.Write([]byte(lockNamespace + name))
	return int64(hash.Sum64())
}

// TryLock takes a session advisory lock without waiting, so exactly one API
// replica runs a singleton task such as an automation pass. The lock lives
// on a dedicated pooled connection and is released by the returned function;
// if the process dies, PostgreSQL releases it with the session.
func (store *Store) TryLock(ctx context.Context, name string) (func(), bool, error) {
	conn, err := store.pool.Acquire(ctx)
	if err != nil {
		return nil, false, fmt.Errorf("acquire lock connection: %w", err)
	}
	var acquired bool
	key := lockKey(name)
	if err := conn.QueryRow(ctx, "SELECT pg_try_advisory_lock($1)", key).Scan(&acquired); err != nil {
		conn.Release()
		return nil, false, fmt.Errorf("try lock %s: %w", name, err)
	}
	if !acquired {
		conn.Release()
		return nil, false, nil
	}
	var once sync.Once
	release := func() {
		once.Do(func() {
			unlockContext, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			defer cancel()
			var released bool
			if err := conn.QueryRow(unlockContext, "SELECT pg_advisory_unlock($1)", key).Scan(&released); err != nil || !released {
				// A session lock that cannot be released must not return to the pool.
				_ = conn.Conn().Close(unlockContext)
			}
			conn.Release()
		})
	}
	return release, true, nil
}

// jsonArray encodes a slice for a jsonb column constrained to arrays. A nil
// slice is an empty list, never JSON null, which the constraint rejects.
func jsonArray[T any](values []T) ([]byte, error) {
	if values == nil {
		return []byte("[]"), nil
	}
	return json.Marshal(values)
}
