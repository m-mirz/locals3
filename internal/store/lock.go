package store

import "sync"

// KeyedLock hands out one reader/writer mutex per name, so that concurrent
// operations on distinct objects never contend. Entries are reference-counted
// and dropped once idle, keeping the map proportional to in-flight work rather
// than to the number of objects ever touched.
//
// Safe for concurrent use.
type KeyedLock struct {
	mu sync.Mutex
	m  map[string]*lockEntry
}

type lockEntry struct {
	rw   sync.RWMutex
	refs int
}

func (k *KeyedLock) acquire(name string) *lockEntry {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.m == nil {
		k.m = make(map[string]*lockEntry)
	}
	e, ok := k.m[name]
	if !ok {
		e = &lockEntry{}
		k.m[name] = e
	}
	e.refs++
	return e
}

func (k *KeyedLock) release(name string) {
	k.mu.Lock()
	defer k.mu.Unlock()
	e, ok := k.m[name]
	if !ok {
		return
	}
	e.refs--
	if e.refs == 0 {
		delete(k.m, name)
	}
}

// Lock takes the write lock for name and returns the corresponding unlock.
func (k *KeyedLock) Lock(name string) func() {
	e := k.acquire(name)
	e.rw.Lock()
	return func() {
		e.rw.Unlock()
		k.release(name)
	}
}

// RLock takes the read lock for name and returns the corresponding unlock.
func (k *KeyedLock) RLock(name string) func() {
	e := k.acquire(name)
	e.rw.RLock()
	return func() {
		e.rw.RUnlock()
		k.release(name)
	}
}
