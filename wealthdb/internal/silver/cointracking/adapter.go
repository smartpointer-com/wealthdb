// Package cointracking projects the cointracking silver DuckDB
// into canonical change records. cointracking.info is the
// aggregator-of-record for crypto holdings across centralised
// exchanges and on-chain wallets; the silver loader replays its
// trade history into a daily-holdings view per portfolio.
//
// Mapping into gold:
//
//   - portfolios = the per-CT-account rows in silver.portfolios.
//     One row per CT user account; the display name carries
//     through from silver.
//
//   - accounts = the per-wallet rows in silver.wallets, scoped to
//     one portfolio (an exchange, a hardware wallet, a staking provider, …). The
//     account_external_id is `<cu_id>:<wallet_name>` — the same
//     composite key silver uses, so cross-table joins line up.
//
//   - instruments = one row per coin ticker observed across any
//     portfolio. AssetClass is `crypto`; the symbol is the ticker
//     (BTC, ETH, …); the name comes from the static cointracking-
//     ticker → coin-name map in coinnames.go (fallback: ticker).
//
//   - positions = one snapshot per distinct positions_daily
//     as_of_date, so gold can answer historical "what did I hold
//     on 2024-12-31" queries. Each snapshot reflects the full
//     portfolio state that day, forward-filled from the latest
//     per-(portfolio, wallet, instrument) positions_daily entry on
//     or before it. Quantity is the coin amount; market_value is
//     that amount priced against silver.portfolio_prices for the
//     portfolio's quote_currency. Positions with no price row
//     (long-tail / pre-listing) get a NULL market_value; gold's
//     per-column upsert leaves prior values intact.
//
// Account taxonomy:
//
//   - account_kind is always `crypto`. CT's wallet vocabulary
//     (an exchange, a hardware wallet, a staking provider, …) doesn't expose a
//     reliable custodial-vs-self-custody flag, and downstream
//     reporting treats both the same way.
//
//   - management_style is always `self_directed`.
//
//   - tax_wrapper defaults to `taxable_personal`. The
//     `portfolio_overrides` block in wealthdb.cfg overrides the
//     wrapper per CT portfolio (e.g. `roth_ira` for a portfolio
//     held inside an IRA wrapper). The existing per-account
//     override block still works in parallel for any wallet-
//     specific corrections.
//
// Transactions projects every silver transactions row into one or
// two canonical events — see the Transactions method doc in
// transactions.go for the buy/sell, balanced cross-currency-pair,
// and non-Trade mappings and the closing-balance invariant. policy.go
// registers deposit/withdrawal as the external capital flows the
// returns engine books.
package cointracking

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"

	_ "github.com/duckdb/duckdb-go/v2"

	"github.com/ptu-gh/wealthdb/wealthdb/internal/silver"
)

const kindName = "cointracking"

func init() {
	silver.Register(&Adapter{})
}

type Adapter struct{}

func (*Adapter) Kind() string { return kindName }

func (*Adapter) Open(_ context.Context, spec silver.OpenSpec) (silver.Connection, error) {
	// DuckDB takes options as `?key=value` query params on the DSN
	// (cf. internal/gold/open.go).
	dsn := spec.Path + "?access_mode=" + url.QueryEscape("read_only")
	db, err := sql.Open("duckdb", dsn)
	if err != nil {
		return nil, fmt.Errorf("open cointracking silver %q: %w", spec.Path, err)
	}
	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("ping cointracking silver %q: %w", spec.Path, err)
	}
	return &Connection{db: db}, nil
}

type Connection struct {
	db *sql.DB
}

func (c *Connection) Close() error {
	if c == nil || c.db == nil {
		return nil
	}
	err := c.db.Close()
	c.db = nil
	return err
}
