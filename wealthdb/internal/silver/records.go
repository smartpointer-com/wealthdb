package silver

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
)

// CashBalance builds one cash balance record. payload is the record's JSON
// payload, typically a fixed `{"basis": …}` naming where the figure came from.
func CashBalance(snap int64, accountID, currency string, kind canonical.BalanceKind,
	amount canonical.Decimal, payload string) canonical.CashBalanceChange {
	return canonical.CashBalanceChange{
		SnapshotAt:        snap,
		AccountExternalID: accountID,
		Currency:          currency,
		BalanceKind:       kind,
		Amount:            amount,
		Payload:           json.RawMessage(payload),
	}
}

// CustodyAccount is the account record, as of t, of a USD login that holds
// private-market interests: a custody account in the taxable personal
// wrapper whose holder self-directs what it holds. The management inside
// each vehicle is not modelled. Callers add the name or category.
func CustodyAccount(id string, t int64) canonical.AccountChange {
	wrapper := canonical.TaxWrapperTaxablePersonal
	style := canonical.ManagementStyleSelfDirected
	usd := "USD"
	return canonical.AccountChange{
		AccountExternalID: id,
		AccountKind:       canonical.AccountKindCustody,
		BaseCurrency:      &usd,
		TaxWrapper:        &wrapper,
		ManagementStyle:   &style,
		FirstSeenAt:       t,
		LastSeenAt:        t,
	}
}

// AccountCurrencies maps each account to the ISO currency of its latest row
// in a silver `accounts` roster (account_external_id, snapshot_at, currency).
type AccountCurrencies struct {
	byID     map[string]string
	fallback string
}

// ReadAccountCurrencies reads the roster's currencies. An account whose
// latest row names none, or that has no row, takes fallback: the currency
// the source books in.
func ReadAccountCurrencies(ctx context.Context, db *sql.DB, fallback string) (AccountCurrencies, error) {
	const q = `
SELECT a.account_external_id, COALESCE(a.currency, '')
  FROM accounts a
 WHERE a.snapshot_at = (SELECT MAX(a2.snapshot_at) FROM accounts a2
                         WHERE a2.account_external_id = a.account_external_id)`
	out := AccountCurrencies{byID: map[string]string{}, fallback: fallback}
	rows, err := db.QueryContext(ctx, q)
	if err != nil {
		return out, fmt.Errorf("account currencies: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id, ccy string
		if err := rows.Scan(&id, &ccy); err != nil {
			return out, err
		}
		out.byID[id] = ccy
	}
	return out, rows.Err()
}

// Of returns the account's currency.
func (a AccountCurrencies) Of(accountID string) string {
	if ccy := a.byID[accountID]; ccy != "" {
		return ccy
	}
	return a.fallback
}
