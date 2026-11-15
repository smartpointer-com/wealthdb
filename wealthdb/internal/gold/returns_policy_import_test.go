package gold

// Test-only blank import of the silver adapters. The returns policy of each
// source is stated in its silver.Source, and silver.Register forwards it to
// returns.RegisterPolicy from the package's init(). RunReturns resolves it by
// kind via returns.ReturnsPolicyFor, so the gold returns tests must trigger
// those init()s to see anything but the Known=false default, exactly as the
// real binary does by importing silver/all in cmd/wealthdb/main.go.
//
// This is a _test.go file, so it pulls silver into the gold TEST binary only;
// the production internal/gold package still does not import internal/silver
// (the boundary documented in open.go). No import cycle: silver imports
// internal/returns, not internal/gold.
import (
	_ "github.com/smartpointer-com/wealthdb/wealthdb/internal/silver/all"
)
