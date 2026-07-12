package gold

// Test-only blank imports of the silver adapters. The returns policy for each
// source is co-located in its silver package and registered from that package's
// init() (returns.RegisterPolicy). RunReturns resolves it by kind via
// returns.ReturnsPolicyFor, so the gold returns tests must trigger those init()s to
// see anything but the Known=false default — exactly as the real binary does by
// blank-importing the adapters in cmd/wealthdb/main.go.
//
// This is a _test.go file, so it pulls silver into the gold TEST binary only;
// the production internal/gold package still does not import internal/silver
// (the boundary documented in open.go). No import cycle: silver/<kind> imports
// internal/returns, not internal/gold.
import (
	_ "github.com/ptu-gh/wealthdb/wealthdb/internal/silver/angellist"
	_ "github.com/ptu-gh/wealthdb/wealthdb/internal/silver/carta"
	_ "github.com/ptu-gh/wealthdb/wealthdb/internal/silver/cointracking"
	_ "github.com/ptu-gh/wealthdb/wealthdb/internal/silver/equityzen"
	_ "github.com/ptu-gh/wealthdb/wealthdb/internal/silver/fidelity"
	_ "github.com/ptu-gh/wealthdb/wealthdb/internal/silver/fred"
	_ "github.com/ptu-gh/wealthdb/wealthdb/internal/silver/manual"
	_ "github.com/ptu-gh/wealthdb/wealthdb/internal/silver/relevate"
	_ "github.com/ptu-gh/wealthdb/wealthdb/internal/silver/schwab"
	_ "github.com/ptu-gh/wealthdb/wealthdb/internal/silver/swissquote"
	_ "github.com/ptu-gh/wealthdb/wealthdb/internal/silver/ubs"
	_ "github.com/ptu-gh/wealthdb/wealthdb/internal/silver/viac"
)
