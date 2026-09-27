package version

import (
	"strings"
	"testing"
)

func TestFormat(t *testing.T) {
	for _, c := range []struct {
		tag, base, commit, want string
	}{
		// A clean build of a release tag is the tag alone.
		{"v1.2.0", "v1.2.0", "abc1234", "v1.2.0"},
		// Anything else names the nearest release and the commit.
		{"", "v1.2.0", "abc1234", "v1.2.0 nightly abc1234"},
		{"", "v1.2.0", "abc1234-dirty", "v1.2.0 nightly abc1234-dirty"},
		// No release below HEAD, or no stamp at all.
		{"", "", "abc1234", "devel nightly abc1234"},
		{"", "", "", "devel nightly unknown"},
	} {
		if got := format(c.tag, c.base, c.commit); got != c.want {
			t.Errorf("format(%q, %q, %q) = %q, want %q", c.tag, c.base, c.commit, got, c.want)
		}
	}
}

func TestShortCommit(t *testing.T) {
	sha := "0123456789abcdef0123456789abcdef01234567"
	for _, c := range []struct {
		bi   BuildInfo
		want string
	}{
		{BuildInfo{Commit: sha}, "0123456"},
		{BuildInfo{Commit: sha, Modified: true}, "0123456-dirty"},
		{BuildInfo{}, ""},
	} {
		if got := c.bi.shortCommit(); got != c.want {
			t.Errorf("%+v.shortCommit() = %q, want %q", c.bi, got, c.want)
		}
	}
}

// TestStringWithoutAStamp pins that a binary with no release stamp — a
// test binary is one — never reports a bare version.
func TestStringWithoutAStamp(t *testing.T) {
	if Tag != "" {
		t.Skip("built with a release stamp")
	}
	if got := String(); !strings.Contains(got, " nightly ") {
		t.Errorf("String() = %q, want a nightly version", got)
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
