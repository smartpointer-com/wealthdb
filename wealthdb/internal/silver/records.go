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

// DayEndOffset is the seconds from a day's midnight to its last second,
// where a closing balance that speaks for the whole day is stamped.
const DayEndOffset = 86399

// RunningBalanceQuery yields the (account, day, balance) rows of a bank
// silver's ledger for AppendClosingBalances: one per account and posting day
// in [?, ?] with a balance, valued at the day's fitid-highest row. posted_at
// is day-granular, so that row stands for the day's end-of-day balance.
const RunningBalanceQuery = `
SELECT account_external_id, posted_at, balance FROM (
    SELECT account_external_id, posted_at, balance,
           ROW_NUMBER() OVER (PARTITION BY account_external_id, posted_at
                              ORDER BY fitid DESC) AS rn
      FROM transactions
     WHERE balance IS NOT NULL AND posted_at BETWEEN ? AND ?
) WHERE rn = 1
 ORDER BY account_external_id, posted_at`

// AppendClosingBalances runs q, which yields (account, time, balance) rows
// bound by args, and appends one CLOSING cash balance per row to batch: in
// the account's currency (ccy), in the canonical sign (sign, nil for the
// figure as stored), with payload as its JSON payload. label prefixes a
// query error.
func AppendClosingBalances(ctx context.Context, db *sql.DB, label, q string, args []any,
	payload string, ccy func(accountID string) string,
	sign func(accountID string, v float64) canonical.Decimal,
	batch *canonical.SnapshotBatch) error {
	return appendBalances(ctx, db, label, q, args, canonical.BalanceKindClosing, payload, ccy, sign, batch)
}

// appendBalances runs q, which yields (account, time, balance) rows bound
// by args, and appends one cash balance of kind per row to batch, as
// AppendClosingBalances describes.
func appendBalances(ctx context.Context, db *sql.DB, label, q string, args []any,
	kind canonical.BalanceKind, payload string, ccy func(accountID string) string,
	sign func(accountID string, v float64) canonical.Decimal,
	batch *canonical.SnapshotBatch) error {
	rows, err := db.QueryContext(ctx, q, args...)
	if err != nil {
		return fmt.Errorf("%s: %w", label, err)
	}
	defer rows.Close()
	for rows.Next() {
		var (
			id  string
			at  int64
			bal float64
		)
		if err := rows.Scan(&id, &at, &bal); err != nil {
			return err
		}
		var amount canonical.Decimal
		if sign != nil {
			amount = sign(id, bal)
		} else {
			amount = canonical.NewDecimalFromFloat(bal)
		}
		batch.CashBalances = append(batch.CashBalances, CashBalance(
			at, id, ccy(id), kind, amount, payload))
	}
	return rows.Err()
}

// AppendReportedBalances appends one CURRENT cash balance per account and
// run that listed it inside w: the account's roster balance as of that run,
// from its latest `accounts` row at or before it, in its currency (ccy) and
// as stored. runsTable holds one (snapshot_at, account_external_id) row per
// loaded run and account the run's roster listed; it is the adapter's own
// constant. An account's own roster rows count as listings too. A roster
// row with no balance emits none. kind names the collector in errors. A
// silver without runsTable is an error that names the collector's load,
// which adds it.
//
// Silver content-dedups an unchanged roster row, so the runs table is the
// only record that a quiet account was still listed. An account the
// latest runs leave out has no balance after the last run that listed it,
// so gold can end it (wealthdb/docs/DESIGN.md §10.7).
func AppendReportedBalances(ctx context.Context, db *sql.DB, kind, runsTable string,
	w canonical.Window, ccy func(accountID string) string, batch *canonical.SnapshotBatch) error {
	label := kind + " roster balances"
	has, err := HasTables(ctx, db, runsTable)
	if err != nil {
		return fmt.Errorf("%s: %w", label, err)
	}
	if !has {
		return fmt.Errorf("%s silver has no %s; run `%s load` first", kind, runsTable, kind)
	}
	q := `
SELECT id, at, balance FROM (
    SELECT l.account_external_id AS id, l.snapshot_at AS at,
           (SELECT a.balance FROM accounts a
             WHERE a.account_external_id = l.account_external_id AND a.snapshot_at <= l.snapshot_at
             ORDER BY a.snapshot_at DESC LIMIT 1) AS balance
      FROM (SELECT account_external_id, snapshot_at FROM ` + runsTable + `
            UNION SELECT account_external_id, snapshot_at FROM accounts) l
     WHERE l.snapshot_at BETWEEN ? AND ?)
 WHERE balance IS NOT NULL
 ORDER BY id, at`
	return appendBalances(ctx, db, label, q, []any{w.Start, w.End},
		canonical.BalanceKindCurrent, `{"basis":"roster"}`, ccy, nil, batch)
}

// BankRosterRow is an account's latest row in a bank silver's `accounts`
// roster, with the span of roster snapshots it and the source appear in.
type BankRosterRow struct {
	ID          string
	Name        string // the roster's name column, "" when NULL
	AccountType string // the roster's type column, "" when NULL or not read
	Mask        string // the last-digits mask, "" when NULL
	Balance     sql.NullFloat64
	Payload     string
	FirstSeen   int64 // the account's first roster snapshot
	LastSeen    int64 // the account's latest roster snapshot
	// SourceLatest is the latest roster snapshot of any account.
	SourceLatest int64
}

// ReadBankRoster reads each account's latest roster row, ordered by account.
// nameCol and typeCol name the roster's name and type columns; an empty
// typeCol reads no type. Both are the adapter's own constants. label
// prefixes a query error.
func ReadBankRoster(ctx context.Context, db *sql.DB, label, nameCol, typeCol string) ([]BankRosterRow, error) {
	typeExpr := "''"
	if typeCol != "" {
		typeExpr = "COALESCE(a." + typeCol + ", '')"
	}
	q := `
SELECT a.account_external_id,
       COALESCE(a.` + nameCol + `, ''), ` + typeExpr + `, COALESCE(a.mask, ''),
       a.balance, a.payload,
       (SELECT MIN(snapshot_at) FROM accounts a2
         WHERE a2.account_external_id = a.account_external_id),
       a.snapshot_at,
       (SELECT MAX(snapshot_at) FROM accounts)
  FROM accounts a
 WHERE a.snapshot_at = (SELECT MAX(snapshot_at) FROM accounts a3
                         WHERE a3.account_external_id = a.account_external_id)
 ORDER BY a.account_external_id`
	rows, err := db.QueryContext(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", label, err)
	}
	defer rows.Close()
	var out []BankRosterRow
	for rows.Next() {
		var r BankRosterRow
		if err := rows.Scan(&r.ID, &r.Name, &r.AccountType, &r.Mask, &r.Balance, &r.Payload,
			&r.FirstSeen, &r.LastSeen, &r.SourceLatest); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// DisplayName is the row's name, falling back to its mask.
func (r BankRosterRow) DisplayName() string {
	if r.Name != "" {
		return r.Name
	}
	return r.Mask
}

// Account is the row's account record: a bank account of kind in the
// taxable personal wrapper whose holder self-directs it, shown as display
// and kept in currency. Callers add the nickname where the name column
// holds one.
func (r BankRosterRow) Account(kind canonical.AccountKind, display, currency string) canonical.AccountChange {
	wrapper := canonical.TaxWrapperTaxablePersonal
	style := canonical.ManagementStyleSelfDirected
	return canonical.AccountChange{
		AccountExternalID: r.ID,
		AccountKind:       kind,
		DisplayName:       StrPtrIfNonEmpty(display),
		BaseCurrency:      &currency,
		TaxWrapper:        &wrapper,
		ManagementStyle:   &style,
		FirstSeenAt:       r.FirstSeen,
		LastSeenAt:        r.LastSeen,
		Payload:           json.RawMessage(r.Payload),
	}
}

// CurrentBalance is the row's roster balance as a CURRENT cash balance of
// amount, the figure in the canonical sign, stamped at the source's latest
// roster snapshot. ok is false when the roster captured no balance or that
// snapshot lies outside w.
//
// The stamp is the source's, not the account's. Silver content-dedups an
// unchanged roster row, so a quiet account's latest snapshot falls behind
// the source's, and gold ends an account at the first later snapshot that
// re-covers the accounts it was observed with, once the source has run one
// and a half of the account's cadences past it (wealthdb/docs/DESIGN.md §10.7). An
// account stamped at its own would drop out of the net-worth views once it
// stopped changing, and a card would take its debt with it. The figure is
// still the account's latest known balance; the stamp says "as of this
// load", which is what an unchanged roster row asserts.
func (r BankRosterRow) CurrentBalance(w canonical.Window, currency string,
	amount canonical.Decimal) (canonical.CashBalanceChange, bool) {
	if !r.Balance.Valid || r.SourceLatest < w.Start || r.SourceLatest > w.End {
		return canonical.CashBalanceChange{}, false
	}
	return CashBalance(r.SourceLatest, r.ID, currency, canonical.BalanceKindCurrent,
		amount, `{"basis":"roster"}`), true
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
