// wealthdb — gold-layer CLI for the personal-portfolio pipeline.
// See docs/DESIGN.md for the architecture, schema, and package layout.
package main

import (
	"os"

	// Adapter packages register themselves in init(); blank-import
	// here so they show up in the silver registry by the time the
	// dispatcher runs.
	_ "github.com/ptu-gh/wealthdb/wealthdb/internal/silver/angellist"
	_ "github.com/ptu-gh/wealthdb/wealthdb/internal/silver/carta"
	_ "github.com/ptu-gh/wealthdb/wealthdb/internal/silver/chase"
	_ "github.com/ptu-gh/wealthdb/wealthdb/internal/silver/cointracking"
	_ "github.com/ptu-gh/wealthdb/wealthdb/internal/silver/equityzen"
	_ "github.com/ptu-gh/wealthdb/wealthdb/internal/silver/fidelity"
	_ "github.com/ptu-gh/wealthdb/wealthdb/internal/silver/firstcitizens"
	_ "github.com/ptu-gh/wealthdb/wealthdb/internal/silver/fred"
	_ "github.com/ptu-gh/wealthdb/wealthdb/internal/silver/manual"
	_ "github.com/ptu-gh/wealthdb/wealthdb/internal/silver/relevate"
	_ "github.com/ptu-gh/wealthdb/wealthdb/internal/silver/schwab"
	_ "github.com/ptu-gh/wealthdb/wealthdb/internal/silver/swissquote"
	_ "github.com/ptu-gh/wealthdb/wealthdb/internal/silver/ubs"
	_ "github.com/ptu-gh/wealthdb/wealthdb/internal/silver/viac"
)

func main() {
	os.Exit(Run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}
