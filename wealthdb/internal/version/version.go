// Package version exposes the build identity of the wealthdb
// binary: the version `wealthdb version` reports, and the VCS identity
// gold.Open() uses to detect a stale binary trying to read a database
// that's already been written by a newer one.
package version

import (
	"runtime/debug"
	"sync"
	"time"
)

// The build's stamp, set at link time by the image build (`./wealthdb
// build` reads it from git and the Dockerfile links it in):
//
//	Tag     the release tag HEAD sits on; set only when the engine tree
//	        had no uncommitted change
//	Base    the nearest release tag at or below HEAD
//	Commit  HEAD's abbreviated hash, with "-dirty" when the engine tree
//	        had uncommitted changes
//
// A binary built any other way (a plain `go build`, `go test`) carries
// none of them, and String falls back to the toolchain's VCS stamp.
var (
	Tag    string
	Base   string
	Commit string
)

// String is the version `wealthdb version` reports: the release tag
// alone for a build of a clean release checkout, otherwise
// "<base> nightly <commit>", so a build between releases never passes
// for one.
func String() string {
	commit := Commit
	if commit == "" {
		commit = Build().shortCommit()
	}
	return format(Tag, Base, commit)
}

func format(tag, base, commit string) string {
	if tag != "" {
		return tag
	}
	if base == "" {
		base = "devel"
	}
	if commit == "" {
		commit = "unknown"
	}
	return base + " nightly " + commit
}

// BuildInfo is the binary's VCS-derived build identity.
//
// CommitAt = 0 means "no VCS info available" — the binary was
// built outside a git checkout, or with -buildvcs=false. Callers
// MUST treat the staleness check as inapplicable in that case
// rather than failing, otherwise CI / Docker-built binaries
// without `.git` in their build context can't open any database.
type BuildInfo struct {
	Commit   string // git SHA (40 hex chars) or empty when no VCS info
	CommitAt int64  // commit's Unix timestamp UTC, 0 when no VCS info
	Modified bool   // the checkout had uncommitted changes
}

// shortCommit renders the commit as String reports it: abbreviated,
// with "-dirty" for a modified checkout; empty when no VCS info.
func (b BuildInfo) shortCommit() string {
	c := b.Commit
	if c == "" {
		return ""
	}
	if len(c) > 7 {
		c = c[:7]
	}
	if b.Modified {
		c += "-dirty"
	}
	return c
}

// Build returns the binary's VCS-derived identity. Reads
// runtime/debug.ReadBuildInfo, which Go 1.18+ auto-populates from
// .git when building from a checkout (default -buildvcs=true).
// Cached after the first call.
func Build() BuildInfo {
	cachedOnce.Do(func() {
		info, ok := debug.ReadBuildInfo()
		if !ok {
			return
		}
		for _, s := range info.Settings {
			switch s.Key {
			case "vcs.revision":
				cached.Commit = s.Value
			case "vcs.time":
				if t, err := time.Parse(time.RFC3339, s.Value); err == nil {
					cached.CommitAt = t.Unix()
				}
			case "vcs.modified":
				cached.Modified = s.Value == "true"
			}
		}
	})
	return cached
}

var (
	cached     BuildInfo
	cachedOnce sync.Once
)
