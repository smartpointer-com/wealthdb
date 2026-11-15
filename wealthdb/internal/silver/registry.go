package silver

import (
	"context"
	"fmt"
	"sort"
	"sync"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/lots"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/returns"
)

// Source is everything wealthdb knows of one kind of silver source,
// stated once beside its adapter and registered from the adapter
// package's init (Register). The package silver/all imports every
// adapter, so a binary that imports it knows every kind.
type Source struct {
	// Kind is the source kind, the `kind` of a config's silver source
	// and gold's silver_sources.silver_kind.
	Kind string
	// Open opens a source of the kind (Adapter.Open).
	Open func(ctx context.Context, spec OpenSpec) (Connection, error)
	// Returns is the kind's returns policy (returns.ReturnsPolicyFor);
	// nil for a kind that emits no accounts.
	Returns *returns.ReturnsPolicy
	// Lots is the kind's lot policy (lots.PolicyFor); nil takes the
	// default.
	Lots *lots.Policy
	// Labels says whose words the kind's names are, for -p (TraitsOf).
	Labels LabelTraits
	// CashSigned are the kinds of transaction with no canonical sign
	// whose amount the adapter passes through signed as the statement or
	// the API states it, for the rows that move no shares
	// (canonical.CashSignedKinds).
	CashSigned []canonical.TxKind
	// RefineClass, where set, takes a lookup's type of a security over
	// the class the adapter gave it (ClassRefiners).
	RefineClass RefineClassFunc
}

// LabelTraits says whose words a kind's names are: a provider's, which
// -p leaves legible, or the holder's own, which it pseudonymizes.
type LabelTraits struct {
	// TypedAccountLabels: an account's display name may be the
	// holder's own words, such as a crypto tracker's wallet label or a
	// manual silver's account label.
	TypedAccountLabels bool
	// NicknameIsLabel: an account's display name is the bank roster's
	// nickname where it has one, the holder's words. A config nickname
	// replaces the account's nickname in gold but not its display name,
	// so the kind says what comparing the two cannot.
	NicknameIsLabel bool
	// TypedAccountIDs: an account's id is a name its holder chose
	// rather than one the provider issues, such as a portal slug, which
	// can spell a name.
	TypedAccountIDs bool
	// TypedHoldings: holdings are named by hand: a holding's name,
	// position key and instrument id are its holder's words.
	TypedHoldings bool
}

var (
	registryMu sync.RWMutex
	registry   = map[string]Source{}
)

// Register records a source kind, and forwards its returns and lot
// policies and its sign pins to the registries gold reads, so gold need
// not import silver. Typically called from an adapter package's init:
//
//	func init() { silver.Register(silver.Source{Kind: kindName, Open: (&Adapter{}).Open}) }
//
// Panics if the kind is already registered; the only legitimate caller
// is package init, so a panic is a build-time bug, not a runtime
// concern.
func Register(s Source) {
	registryMu.Lock()
	defer registryMu.Unlock()
	if _, exists := registry[s.Kind]; exists {
		panic(fmt.Sprintf("silver: adapter %q already registered", s.Kind))
	}
	registry[s.Kind] = s
	if s.Returns != nil {
		returns.RegisterPolicy(s.Kind, *s.Returns)
	}
	if s.Lots != nil {
		lots.RegisterPolicy(s.Kind, *s.Lots)
	}
	if len(s.CashSigned) > 0 {
		canonical.RegisterCashSigned(s.Kind, s.CashSigned...)
	}
}

// Get returns the source of the given kind, or a non-nil error if no
// source is registered under that name.
func Get(kind string) (Source, error) {
	registryMu.RLock()
	defer registryMu.RUnlock()
	s, ok := registry[kind]
	if !ok {
		return Source{}, fmt.Errorf("silver: no adapter registered for kind %q (known: %v)", kind, sortedKindsLocked())
	}
	return s, nil
}

// Kinds returns the sorted list of registered adapter names.
// Useful for `wealthdb config` to list valid `kind` values.
func Kinds() []string {
	registryMu.RLock()
	defer registryMu.RUnlock()
	return sortedKindsLocked()
}

// TraitsOf is the label traits of a kind; an unregistered kind's are
// all false: its names read as the provider's.
func TraitsOf(kind string) LabelTraits {
	registryMu.RLock()
	defer registryMu.RUnlock()
	return registry[kind].Labels
}

// ClassRefiners returns the RefineClass of every registered source
// that states one, by kind.
func ClassRefiners() map[string]RefineClassFunc {
	registryMu.RLock()
	defer registryMu.RUnlock()
	out := map[string]RefineClassFunc{}
	for kind, s := range registry {
		if s.RefineClass != nil {
			out[kind] = s.RefineClass
		}
	}
	return out
}

func sortedKindsLocked() []string {
	out := make([]string, 0, len(registry))
	for k := range registry {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
