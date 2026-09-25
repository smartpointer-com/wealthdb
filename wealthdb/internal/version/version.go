// Package version exposes the build identity of the wealthdb
// binary, used by gold.Open() to detect a stale binary trying to
// read a database that's already been written by a newer one.
package version

import (
	"runtime/debug"
	"sync"
	"time"
)

// Version is the symbolic build version, reported by `wealthdb
// version`. Kept in lockstep with the repo's release tag; builds
// may override at link time:
//
//	go build -ldflags '-X github.com/ptu-gh/wealthdb/wealthdb/internal/version.Version=v0.3.0'
var Version = "v0.3.0"

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
			}
		}
	})
	return cached
}

var (
	cached     BuildInfo
	cachedOnce sync.Once
)
