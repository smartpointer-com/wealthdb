// wealthdb — gold-layer CLI for the personal-portfolio pipeline.
// See docs/DESIGN.md for the architecture, schema, and package layout.
package main

import (
	"os"

	"golang.org/x/sys/unix"

	// Adapter packages register themselves in init(); blank-import
	// here so they show up in the silver registry by the time the
	// dispatcher runs.
	_ "github.com/smartpointer-com/wealthdb/wealthdb/internal/silver/amex"
	_ "github.com/smartpointer-com/wealthdb/wealthdb/internal/silver/angellist"
	_ "github.com/smartpointer-com/wealthdb/wealthdb/internal/silver/carta"
	_ "github.com/smartpointer-com/wealthdb/wealthdb/internal/silver/chase"
	_ "github.com/smartpointer-com/wealthdb/wealthdb/internal/silver/cointracking"
	_ "github.com/smartpointer-com/wealthdb/wealthdb/internal/silver/equityzen"
	_ "github.com/smartpointer-com/wealthdb/wealthdb/internal/silver/fidelity"
	_ "github.com/smartpointer-com/wealthdb/wealthdb/internal/silver/firstcitizens"
	_ "github.com/smartpointer-com/wealthdb/wealthdb/internal/silver/fred"
	_ "github.com/smartpointer-com/wealthdb/wealthdb/internal/silver/manual"
	_ "github.com/smartpointer-com/wealthdb/wealthdb/internal/silver/raiffeisen_at"
	_ "github.com/smartpointer-com/wealthdb/wealthdb/internal/silver/relevate"
	_ "github.com/smartpointer-com/wealthdb/wealthdb/internal/silver/schwab"
	_ "github.com/smartpointer-com/wealthdb/wealthdb/internal/silver/swissquote"
	_ "github.com/smartpointer-com/wealthdb/wealthdb/internal/silver/ubs"
	_ "github.com/smartpointer-com/wealthdb/wealthdb/internal/silver/viac"
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
