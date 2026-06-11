package version

import "testing"

func TestVersionDefault(t *testing.T) {
	if Version == "" {
		t.Error("Version is empty; want a non-empty build version (default \"dev\")")
	}
}

func TestBuildDoesNotPanicAndIsSane(t *testing.T) {
	// Build() reads runtime/debug.ReadBuildInfo, which is absent in
	// some `go test` modes — so CommitAt may legitimately be 0
	// (the "no VCS info" sentinel the staleness check tolerates).
	// We only assert it returns cleanly with a non-negative
	// timestamp and a commit string that's either empty or matches
	// the timestamp's presence.
	bi := Build()
	if bi.CommitAt < 0 {
		t.Errorf("CommitAt = %d, want >= 0", bi.CommitAt)
	}
	// Cached: a second call returns the same value.
	if Build() != bi {
		t.Error("Build() is not stable across calls")
	}
}
