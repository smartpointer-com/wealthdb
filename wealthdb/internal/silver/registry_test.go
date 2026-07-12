package silver

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// stubAdapter is a no-op Adapter used for registry exercises.
type stubAdapter struct{ kind string }

func (s *stubAdapter) Kind() string { return s.kind }
func (s *stubAdapter) Open(context.Context, OpenSpec) (Connection, error) {
	return nil, errors.New("not implemented")
}

func TestRegisterAndGet(t *testing.T) {
	resetForTesting()

	Register(&stubAdapter{kind: "alpha"})
	Register(&stubAdapter{kind: "bravo"})

	got, err := Get("alpha")
	if err != nil {
		t.Fatalf("Get(alpha): %v", err)
	}
	if got.Kind() != "alpha" {
		t.Errorf("Kind() = %q, want %q", got.Kind(), "alpha")
	}

	if kinds := Kinds(); len(kinds) != 2 || kinds[0] != "alpha" || kinds[1] != "bravo" {
		t.Errorf("Kinds() = %v, want [alpha bravo]", kinds)
	}
}

func TestGetMissingMentionsKnown(t *testing.T) {
	resetForTesting()
	Register(&stubAdapter{kind: "alpha"})
	Register(&stubAdapter{kind: "bravo"})

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
	Register(&stubAdapter{kind: "alpha"})

	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("expected panic on duplicate Register")
		}
	}()
	Register(&stubAdapter{kind: "alpha"})
}
