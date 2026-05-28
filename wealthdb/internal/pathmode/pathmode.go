// Package pathmode chooses whether wealthdb opens the gold
// database read-write or read-only. The decision combines an
// explicit -r flag with a filesystem-permission probe; see
// docs/DESIGN.md §4.10 for the full rules.
package pathmode

import (
	"errors"
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

// Mode mirrors gold.Mode but lives here so the cmd layer can
// route the value without dragging in the gold package.
type Mode int

const (
	// ModeReadWrite — DuckDB opened with default access.
	ModeReadWrite Mode = iota
	// ModeReadOnly — DuckDB opened with access_mode=read_only;
	// (RW) subcommands refuse to run.
	ModeReadOnly
)

func (m Mode) String() string {
	if m == ModeReadOnly {
		return "read-only"
	}
	return "read-write"
}

// Reason explains why a particular Mode was chosen. Mostly used
// for log lines and error messages so the user sees *why* a
// (RW) subcommand was refused.
type Reason string

const (
	ReasonExplicitFlag        Reason = "-r/--read-only flag set"
	ReasonDirNotWriteable     Reason = "parent directory is not writeable"
	ReasonFileNotWriteable    Reason = "file mode lacks owner write bit"
	ReasonDBMissingForInit    Reason = "gold database does not exist (init will create)"
	ReasonDBWriteable         Reason = "gold database and parent directory are writeable"
	ReasonDBMissingForRO      Reason = "gold database does not exist"
)

// Decision is the full result of a Detect call.
type Decision struct {
	Mode   Mode
	Reason Reason
	// DBExists reports whether the gold DB file existed at probe
	// time. Useful for cmd_init's "refuse if exists" check.
	DBExists bool
}

// Detect chooses the access mode for the gold DB at `path`.
// `forceReadOnly` reflects the -r/--read-only flag.
// `subcommandIsInit` is true when the caller is `wealthdb init`,
// which has a special rule (missing DB ⇒ proceed RW because
// init will create it).
//
// Returns an error only when the state is unsalvageable in any
// mode (e.g. the parent directory doesn't exist either). Routine
// "RO is the right answer here" outcomes are reported via the
// Decision, not an error.
func Detect(path string, forceReadOnly, subcommandIsInit bool) (Decision, error) {
	if forceReadOnly {
		return Decision{Mode: ModeReadOnly, Reason: ReasonExplicitFlag, DBExists: pathExists(path)}, nil
	}

	st, err := os.Stat(path)
	switch {
	case err == nil:
		// DB file exists; probe writeability.
		_ = st // intentionally not reading flags from st; permission probe is more robust
		return decidePermissionMode(path), nil

	case errors.Is(err, os.ErrNotExist):
		if subcommandIsInit {
			return Decision{Mode: ModeReadWrite, Reason: ReasonDBMissingForInit, DBExists: false}, nil
		}
		return Decision{Mode: ModeReadOnly, Reason: ReasonDBMissingForRO, DBExists: false}, nil

	default:
		// Anything else (permission denied on stat, ENOTDIR, ...)
		// — report it so the caller can produce a useful error.
		return Decision{}, fmt.Errorf("pathmode: stat %q: %w", path, err)
	}
}

// decidePermissionMode runs the two unix.Access probes that
// determine writeability. DuckDB needs to write the WAL sidecar
// and lock file alongside the DB, so we check the parent dir
// FIRST — a writeable file under a read-only mount still can't
// be opened RW.
func decidePermissionMode(path string) Decision {
	dir := parentDir(path)
	if err := unix.Access(dir, unix.W_OK); err != nil {
		return Decision{Mode: ModeReadOnly, Reason: ReasonDirNotWriteable, DBExists: true}
	}
	if err := unix.Access(path, unix.W_OK); err != nil {
		return Decision{Mode: ModeReadOnly, Reason: ReasonFileNotWriteable, DBExists: true}
	}
	return Decision{Mode: ModeReadWrite, Reason: ReasonDBWriteable, DBExists: true}
}

func parentDir(path string) string {
	for i := len(path) - 1; i >= 0; i-- {
		if path[i] == '/' {
			if i == 0 {
				return "/"
			}
			return path[:i]
		}
	}
	return "."
}

func pathExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
