package payment

import "sync"

// keyedMutex serialises operations on the same payment within this process.
// Combined with optimistic versioning in the store it prevents two
// concurrent captures (or capture + void) from both reaching the acquirer.
type keyedMutex struct {
	mu    sync.Mutex
	locks map[string]*refLock
}

type refLock struct {
	sync.Mutex
	refs int
}

func newKeyedMutex() *keyedMutex { return &keyedMutex{locks: map[string]*refLock{}} }

// Lock acquires the lock for key and returns its release function.
func (k *keyedMutex) Lock(key string) func() {
	k.mu.Lock()
	l, ok := k.locks[key]
	if !ok {
		l = &refLock{}
		k.locks[key] = l
	}
	l.refs++
	k.mu.Unlock()

	l.Lock()
	return func() {
		l.Unlock()
		k.mu.Lock()
		l.refs--
		if l.refs == 0 {
			delete(k.locks, key)
		}
		k.mu.Unlock()
	}
}

// size returns the number of live lock entries (for tests).
func (k *keyedMutex) size() int {
	k.mu.Lock()
	defer k.mu.Unlock()
	return len(k.locks)
}
