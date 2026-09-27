package main

import (
	"strings"
	"testing"

	"github.com/ptu-gh/wealthdb/wealthdb/internal/version"
)

func TestVersion(t *testing.T) {
	t.Parallel()
	so, _, code := run(t, "version")
	if code != 0 {
		t.Errorf("exit = %d, want 0", code)
	}
	want := "wealthdb " + version.String() + "\n"
	if so != want {
		t.Errorf("stdout = %q, want %q", so, want)
	}
	// A test binary carries no release stamp, so it must not pass for a
	// release.
	if !strings.Contains(so, " nightly ") {
		t.Errorf("stdout = %q, want a nightly version", so)
	}
}

func TestVersionRejectsArgs(t *testing.T) {
	t.Parallel()
	_, se, code := run(t, "version", "extra")
	if code != 2 {
		t.Errorf("exit = %d, want 2", code)
	}
	if !strings.Contains(se, "unexpected argument") {
		t.Errorf("stderr missing 'unexpected argument': %s", se)
	}
}
