// Package errs defines the typed error that subcommand code uses
// to signal a specific process exit code. The dispatcher in
// cmd/wealthdb checks for *ExitError via errors.As and exits
// accordingly; everything else falls through to exit code 1.
//
// Exit codes are defined in docs/DESIGN.md §4.10.
package errs

import "fmt"

// Process exit codes. Code 0 is reserved for success, code 1 for
// unexpected runtime errors / panics.
const (
	// ExitRWNeeded — a write-requiring subcommand was invoked but
	// the gold DB is read-only (or -r was specified).
	ExitRWNeeded = 2

	// ExitMissingDB — a read subcommand was invoked but the gold
	// DB doesn't exist; run `wealthdb init` to create it.
	ExitMissingDB = 3

	// ExitInitExisting — `wealthdb init` was invoked but the gold
	// DB already exists, or `wealthdb config` was invoked but the
	// config file already exists.
	ExitInitExisting = 4

	// ExitOpenFailed — opening the gold DuckDB file failed for a
	// reason other than missing-file: lock conflict, corruption,
	// permissions, stale WAL.
	ExitOpenFailed = 5

	// ExitSilverIO — a silver DB referenced by the config could
	// not be read.
	ExitSilverIO = 6
)

// ExitError carries a specific process exit code along with the
// underlying error. Construct one when a subcommand fails in a
// way that the §4.10 error table prescribes a code for; otherwise
// return a plain error and let the dispatcher exit with code 1.
type ExitError struct {
	Code int
	Err  error
}

// Error returns the underlying error's message. Returns the empty
// string if e or e.Err is nil so the value remains safe to format
// in a string context.
func (e *ExitError) Error() string {
	if e == nil || e.Err == nil {
		return ""
	}
	return e.Err.Error()
}

// Unwrap exposes the wrapped error for use with errors.Is /
// errors.As.
func (e *ExitError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

// Newf wraps fmt.Errorf with a specific exit code. Convenience
// for the common "format a message and assign a code" pattern.
func Newf(code int, format string, args ...any) *ExitError {
	return &ExitError{Code: code, Err: fmt.Errorf(format, args...)}
}

// Wrap attaches an exit code to an existing error, preserving the
// original via Unwrap. Returns nil if err is nil.
func Wrap(code int, err error) *ExitError {
	if err == nil {
		return nil
	}
	return &ExitError{Code: code, Err: err}
}
