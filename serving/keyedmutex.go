package serving

import "sync"

// keyedMutex serializes critical sections keyed by a string (e.g. an object
// ref). Writes to the same key are mutually exclusive; writes to different keys
// proceed concurrently. Idle keys are dropped so the map does not grow without
// bound.
//
// It exists so a PUT's read-check-write (index lookup → precondition/revision
// check → store write → index update) is atomic per ref, which is what makes
// If-Match optimistic locking race-safe.
type keyedMutex struct {
	mu    sync.Mutex
	locks map[string]*refLock
}

type refLock struct {
	mu      sync.Mutex
	waiters int
}

func newKeyedMutex() *keyedMutex {
	return &keyedMutex{locks: make(map[string]*refLock)}
}

// Lock blocks until the caller holds the lock for key.
func (k *keyedMutex) Lock(key string) {
	k.mu.Lock()
	l, ok := k.locks[key]
	if !ok {
		l = &refLock{}
		k.locks[key] = l
	}
	l.waiters++
	k.mu.Unlock()

	l.mu.Lock()
}

// Unlock releases the lock for key. Must be paired with a preceding Lock(key).
func (k *keyedMutex) Unlock(key string) {
	k.mu.Lock()
	l := k.locks[key]
	if l == nil {
		k.mu.Unlock()
		panic("keyedMutex: Unlock of unlocked key " + key)
	}
	l.waiters--
	if l.waiters == 0 {
		delete(k.locks, key)
	}
	k.mu.Unlock()

	l.mu.Unlock()
}
