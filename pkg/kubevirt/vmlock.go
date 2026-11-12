package kubevirt

import "sync"

// keyedMutex hands out one mutex per key so that work on different keys runs
// in parallel while work on the same key is serialized. Entries are reference
// counted and dropped as soon as nobody holds or waits for them, so the map
// does not grow with the number of VMs seen over the lifetime of the process.
type keyedMutex struct {
	mu      sync.Mutex
	entries map[string]*keyedMutexEntry
}

type keyedMutexEntry struct {
	sync.Mutex
	refs int
}

func newKeyedMutex() *keyedMutex {
	return &keyedMutex{entries: make(map[string]*keyedMutexEntry)}
}

// Lock blocks until the caller owns key and returns the function that releases it.
func (k *keyedMutex) Lock(key string) func() {
	k.mu.Lock()
	entry, ok := k.entries[key]
	if !ok {
		entry = &keyedMutexEntry{}
		k.entries[key] = entry
	}
	entry.refs++
	k.mu.Unlock()

	entry.Lock()
	return func() {
		entry.Unlock()
		k.mu.Lock()
		entry.refs--
		if entry.refs == 0 {
			delete(k.entries, key)
		}
		k.mu.Unlock()
	}
}

// size reports how many keys are currently held or waited for.
func (k *keyedMutex) size() int {
	k.mu.Lock()
	defer k.mu.Unlock()
	return len(k.entries)
}
