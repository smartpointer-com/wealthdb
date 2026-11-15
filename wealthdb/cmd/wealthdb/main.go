// wealthdb — gold-layer CLI for the personal-portfolio pipeline.
// See docs/DESIGN.md for the architecture, schema, and package layout.
package main

import (
	"os"

	"golang.org/x/sys/unix"

	// Every adapter registers itself in init(); silver/all imports them.
	_ "github.com/smartpointer-com/wealthdb/wealthdb/internal/silver/all"
)

func main() {
	// The compact and reload temps are the whole merged ledger, and they
	// are created by DuckDB inside the build — there is no call site to
	// pass a mode to, so the umask is the only thing that reaches them.
	//
	// Creation-time only, so nothing here narrows a file that already
	// exists: an operator's explicit `chmod 0444` read-only gold
	// (docs/DESIGN.md §4.10) still takes effect and is still detected,
	// and a fresh-swap rebuild still copies the live file's mode onto its
	// replacement.
	unix.Umask(0o077)
	os.Exit(Run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}
