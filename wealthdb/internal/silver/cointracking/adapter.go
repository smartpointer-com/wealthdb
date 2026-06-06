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
//   - positions = the latest-snapshot per (account, coin) where
//     the running balance (SUM(buy_amount) − SUM(sell_amount)
//     across transactions for that wallet) is positive. Quantity
//     is the coin amount; market_value is the coin amount times
//     the corresponding row in silver.portfolio_prices for the
//     portfolio's quote_currency on the latest priced day.
//     Positions whose portfolio_prices entry doesn't exist (long-
//     tail / pre-listing) get a NULL market_value; gold's
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
// Transactions are NOT emitted in this iteration. The silver
// transactions table is the source of truth for the trade history
// and the gold layer can read from it directly when needed; gold's
// `transactions` table only carries adapter-emitted events. A
// future pass can lift the trade history into gold if the
// downstream queries call for it.
package cointracking

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"

	_ "github.com/marcboeker/go-duckdb/v2"

	"github.com/ptu/wealthdb/internal/silver"
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
	return &Connection{db: db, path: spec.Path}, nil
}

type Connection struct {
	db   *sql.DB
	path string
}

func (c *Connection) Close() error {
	if c == nil || c.db == nil {
		return nil
	}
	err := c.db.Close()
	c.db = nil
	return err
}
