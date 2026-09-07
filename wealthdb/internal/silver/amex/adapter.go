// Package amex projects the amex collector's silver (an American Express
// credit- and charge-card relationship) into canonical change records. See
// collectors/amex/ for the bronze/silver schema and the gold mapping this
// implements.
//
// A card relationship is a pure liability, so the projection is the card half
// of the chase sibling with none of its deposit half:
//
//   - One ACCOUNT (AccountKind 'card') per card. DisplayName from the
//     product's own name (falling back to the displayed mask); TaxWrapper
//     'taxable_personal', ManagementStyle 'self_directed'. All overridable via
//     account_overrides.
//
//   - CASH BALANCES, not positions — a card carries no instrument. The
//     outstanding balance lives as NEGATIVE cash (the margin-debit precedent,
//     migration 0037): silver holds every figure the provider's way, owed
//     POSITIVE, and this package negates it at exactly one point
//     (signedBalance). A credit limit and available credit are not balances of
//     anything owned and never become rows.
//
//     Two kinds of mark feed the series: the roster's live figure as a
//     CURRENT mark at the source's latest load, and every stated period close
//     as a CLOSING mark at its own period_end — the printed figure for the
//     archive's statements, the activity payload's cycle summary for the
//     window a run fetched. They are told apart by kind, not by date, so a
//     CURRENT and a CLOSING mark on one day do not compete. There is no
//     per-day series, because no Amex channel carries a running balance on a
//     card — the balance history is monthly, at the density the provider
//     actually asserts it. (chase reconstructs a daily series by rolling its
//     ledger between the same anchors; that is available here later if the
//     granularity proves insufficient, and is deliberately not guessed at now.)
//
//   - TRANSACTIONS: the whole card ledger. The collector already stores
//     amounts in the fleet's card convention (spend negative), having negated
//     Amex's own spend-positive figures at load; kinds are mapped from the
//     provider's direction plus its spend category, and routed through
//     ApplyCanonicalSign so a purchase is negative and a refund or bill
//     payment positive whatever the row said.
//
// RETURNS. Nothing here reaches the returns engine: it drops every `card`
// account at the loader (returnsInvisibleKind), because a revolving-credit
// liability is a spending instrument, not an investment — running its balance
// swings through TWR/MWR would report shopping as performance. The registered
// ReturnsPolicy (policy.go) exists because gold guards that every whitelisted
// kind declares one, and it says exactly that.
//
// SPENDING is where this source earns its keep. Every modern-era row carries
// the provider's own spend category unless the issuer left it unfiled, which
// the provider tier translates without the model
// (internal/spending/providermap.go); the deep era carries none, and the
// model reads the merchant. The card's `card_payment`
// legs let the internal-transfer matcher net out the bills paid from a
// collected cash account — replacing the `card_spend` placeholder with the
// purchases the card itemises.
package amex

import (
	"context"
	"database/sql"

	"github.com/ptu-gh/wealthdb/wealthdb/internal/silver"
)

const kindName = "amex"

func init() {
	silver.Register(&Adapter{})
}

type Adapter struct{}

func (*Adapter) Kind() string { return kindName }

func (*Adapter) Open(_ context.Context, spec silver.OpenSpec) (silver.Connection, error) {
	db, err := silver.OpenReadOnlySQLite(spec.Path, "amex silver")
	if err != nil {
		return nil, err
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

// accountCurrencies maps each account_external_id to its ISO currency, read
// from the latest accounts snapshot. The relationship is USD, but the currency
// is read rather than assumed; a missing/blank value falls back to USD.
func (c *Connection) accountCurrencies(ctx context.Context) (map[string]string, error) {
	const q = `
SELECT a.account_external_id, COALESCE(a.currency, '')
  FROM accounts a
 WHERE a.snapshot_at = (SELECT MAX(a2.snapshot_at) FROM accounts a2
                         WHERE a2.account_external_id = a.account_external_id)`
	rows, err := c.db.QueryContext(ctx, q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var id, ccy string
		if err := rows.Scan(&id, &ccy); err != nil {
			return nil, err
		}
		out[id] = ccy
	}
	return out, rows.Err()
}

// currencyOf returns the account's currency, defaulting to USD.
func currencyOf(m map[string]string, accountID string) string {
	if ccy := m[accountID]; ccy != "" {
		return ccy
	}
	return "USD"
}
