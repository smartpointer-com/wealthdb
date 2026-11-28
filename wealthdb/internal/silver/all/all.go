// Package all imports every silver adapter, so a binary or a test that
// imports it knows every source kind (silver.Register).
package all

import (
	_ "github.com/smartpointer-com/wealthdb/wealthdb/internal/silver/amex"
	_ "github.com/smartpointer-com/wealthdb/wealthdb/internal/silver/angellist"
	_ "github.com/smartpointer-com/wealthdb/wealthdb/internal/silver/carta"
	_ "github.com/smartpointer-com/wealthdb/wealthdb/internal/silver/chase"
	_ "github.com/smartpointer-com/wealthdb/wealthdb/internal/silver/cointracking"
	_ "github.com/smartpointer-com/wealthdb/wealthdb/internal/silver/equityzen"
	_ "github.com/smartpointer-com/wealthdb/wealthdb/internal/silver/erstebank_at"
	_ "github.com/smartpointer-com/wealthdb/wealthdb/internal/silver/fidelity"
	_ "github.com/smartpointer-com/wealthdb/wealthdb/internal/silver/firstcitizens"
	_ "github.com/smartpointer-com/wealthdb/wealthdb/internal/silver/manual"
	_ "github.com/smartpointer-com/wealthdb/wealthdb/internal/silver/plaid"
	_ "github.com/smartpointer-com/wealthdb/wealthdb/internal/silver/raiffeisen_at"
	_ "github.com/smartpointer-com/wealthdb/wealthdb/internal/silver/refdata"
	_ "github.com/smartpointer-com/wealthdb/wealthdb/internal/silver/relevate"
	_ "github.com/smartpointer-com/wealthdb/wealthdb/internal/silver/schwab"
	_ "github.com/smartpointer-com/wealthdb/wealthdb/internal/silver/series"
	_ "github.com/smartpointer-com/wealthdb/wealthdb/internal/silver/swissquote"
	_ "github.com/smartpointer-com/wealthdb/wealthdb/internal/silver/synthetic"
	_ "github.com/smartpointer-com/wealthdb/wealthdb/internal/silver/ubs"
	_ "github.com/smartpointer-com/wealthdb/wealthdb/internal/silver/viac"
)
