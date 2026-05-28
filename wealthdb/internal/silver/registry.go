package silver

import (
	"fmt"
	"sort"
	"sync"
)

var (
	registryMu sync.RWMutex
	registry   = map[string]Adapter{}
)

// Register adds an adapter to the global registry. Typically
// called from a backend package's init() function:
//
//	func init() { silver.Register(&Adapter{}) }
//
// Panics if an adapter with the same Kind() is already registered;
// the only legitimate caller is package init, so a panic is a
// build-time bug, not a runtime concern.
func Register(a Adapter) {
	registryMu.Lock()
	defer registryMu.Unlock()
	name := a.Kind()
	if _, exists := registry[name]; exists {
		panic(fmt.Sprintf("silver: adapter %q already registered", name))
	}
	registry[name] = a
}

// Get returns the adapter for the given kind, or a non-nil error
// if no adapter is registered under that name.
func Get(kind string) (Adapter, error) {
	registryMu.RLock()
	defer registryMu.RUnlock()
	a, ok := registry[kind]
	if !ok {
		return nil, fmt.Errorf("silver: no adapter registered for kind %q (known: %v)", kind, sortedKindsLocked())
	}
	return a, nil
}

// Kinds returns the sorted list of registered adapter names.
// Useful for `wealthdb config` to list valid `kind` values.
func Kinds() []string {
	registryMu.RLock()
	defer registryMu.RUnlock()
	return sortedKindsLocked()
}

func sortedKindsLocked() []string {
	out := make([]string, 0, len(registry))
	for k := range registry {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// resetForTesting clears the registry. Test-only; not exposed
// outside the package via a public name.
func resetForTesting() {
	registryMu.Lock()
	defer registryMu.Unlock()
	registry = map[string]Adapter{}
}
