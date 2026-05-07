// Package registry holds the mutable map of function-name -> on-disk
// implementation. The map is rebuilt on each repo refresh; lookups are
// performed by the FunctionImplementation closures on every invocation, so a
// refresh that updates a script body takes effect without restarting the
// worker.
package registry

import (
	"sort"
	"sync"

	"github.com/jesperfj/ghfn-worker/internal/scriptexec"
)

// Registry is a concurrency-safe map of fullName -> *scriptexec.Entry.
type Registry struct {
	mu      sync.RWMutex
	entries map[string]*scriptexec.Entry
}

func New() *Registry {
	return &Registry{entries: make(map[string]*scriptexec.Entry)}
}

// Replace atomically swaps in a new set of entries.
func (r *Registry) Replace(entries map[string]*scriptexec.Entry) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.entries = entries
}

// Get returns the current entry for fullName, or nil.
func (r *Registry) Get(fullName string) *scriptexec.Entry {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.entries[fullName]
}

// Names returns the sorted list of currently-registered function names.
func (r *Registry) Names() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	names := make([]string, 0, len(r.entries))
	for n := range r.entries {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// Lookup returns a scriptexec.Lookup bound to this registry.
func (r *Registry) Lookup() scriptexec.Lookup {
	return func(fullName string) *scriptexec.Entry {
		return r.Get(fullName)
	}
}
