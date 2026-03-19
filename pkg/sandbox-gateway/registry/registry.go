package registry

import "sync"

var (
	mu      sync.RWMutex
	entries = make(map[string]string)
)

// Get returns the podIP for the given sandbox ID.
// The sandbox ID format is "{namespace}--{name}".
func Get(id string) (string, bool) {
	mu.RLock()
	defer mu.RUnlock()
	ip, ok := entries[id]
	return ip, ok
}

// Update sets the podIP for the given sandbox ID.
func Update(id, ip string) {
	mu.Lock()
	defer mu.Unlock()
	entries[id] = ip
}

// Delete removes the entry for the given sandbox ID.
func Delete(id string) {
	mu.Lock()
	defer mu.Unlock()
	delete(entries, id)
}
