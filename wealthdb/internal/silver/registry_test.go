package silver

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
)

// resetForTesting clears the registry.
func resetForTesting() {
	registryMu.Lock()
	defer registryMu.Unlock()
	registry = map[string]Source{}
}

// stub is a no-op Source used for registry exercises.
func stub(kind string) Source {
	return Source{Kind: kind, Open: func(context.Context, OpenSpec) (Connection, error) {
		return nil, errors.New("not implemented")
	}}
}

func TestRegisterAndGet(t *testing.T) {
	resetForTesting()

	Register(stub("alpha"))
	Register(stub("bravo"))

	got, err := Get("alpha")
	if err != nil {
		t.Fatalf("Get(alpha): %v", err)
	}
	if got.Kind != "alpha" {
		t.Errorf("Kind = %q, want %q", got.Kind, "alpha")
	}

	if kinds := Kinds(); len(kinds) != 2 || kinds[0] != "alpha" || kinds[1] != "bravo" {
		t.Errorf("Kinds() = %v, want [alpha bravo]", kinds)
	}
}

func TestGetMissingMentionsKnown(t *testing.T) {
	resetForTesting()
	Register(stub("alpha"))
	Register(stub("bravo"))

	_, err := Get("nope")
	if err == nil {
		t.Fatal("expected error for unknown kind")
	}
	if !strings.Contains(err.Error(), "alpha") || !strings.Contains(err.Error(), "bravo") {
		t.Errorf("error %q should mention known kinds", err)
	}
}

func TestRegisterDuplicatePanics(t *testing.T) {
	resetForTesting()
	Register(stub("alpha"))

	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("expected panic on duplicate Register")
		}
	}()
	Register(stub("alpha"))
}

// TestTraitsAndRefiners pins the lookups by kind: an unregistered kind
// has no traits, and only a source that states a refiner offers one.
func TestTraitsAndRefiners(t *testing.T) {
	resetForTesting()
	typed := stub("typed")
	typed.Labels = LabelTraits{TypedHoldings: true}
	typed.RefineClass = func(_ string, ac canonical.AssetClass, v canonical.Vehicle, _ string) (canonical.AssetClass, canonical.Vehicle) {
		return ac, v
	}
	Register(typed)
	Register(stub("plain"))
	if !TraitsOf("typed").TypedHoldings || TraitsOf("plain") != (LabelTraits{}) || TraitsOf("nope") != (LabelTraits{}) {
		t.Errorf("TraitsOf: typed %+v, plain %+v, nope %+v", TraitsOf("typed"), TraitsOf("plain"), TraitsOf("nope"))
	}
	if r := ClassRefiners(); len(r) != 1 || r["typed"] == nil {
		t.Errorf("ClassRefiners = %v, want typed alone", r)
	}
}
