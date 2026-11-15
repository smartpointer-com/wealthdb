package silver

import (
	"context"
	"errors"
	"strings"
	"testing"
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

// TestTraitsOf pins the lookup by kind: an unregistered kind has no
// traits.
func TestTraitsOf(t *testing.T) {
	resetForTesting()
	typed := stub("typed")
	typed.Labels = LabelTraits{TypedHoldings: true}
	Register(typed)
	Register(stub("plain"))
	if !TraitsOf("typed").TypedHoldings || TraitsOf("plain") != (LabelTraits{}) || TraitsOf("nope") != (LabelTraits{}) {
		t.Errorf("TraitsOf: typed %+v, plain %+v, nope %+v", TraitsOf("typed"), TraitsOf("plain"), TraitsOf("nope"))
	}
}
