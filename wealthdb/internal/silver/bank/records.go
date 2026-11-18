package bank

import (
	"context"
	"database/sql"
	"encoding/json"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/silver"
)

// RunningBalanceQuery yields the (account, day, balance) rows of a bank
// silver's ledger for a CLOSING series (Series.Query): one per account and
// posting day in [?, ?] with a balance, valued at the day's fitid-highest
// row. posted_at is day-granular, so that row stands for the day's
// end-of-day balance.
const RunningBalanceQuery = `
SELECT account_external_id, posted_at, balance FROM (
    SELECT account_external_id, posted_at, balance,
           ROW_NUMBER() OVER (PARTITION BY account_external_id, posted_at
                              ORDER BY fitid DESC) AS rn
      FROM transactions
     WHERE balance IS NOT NULL AND posted_at BETWEEN ? AND ?
) WHERE rn = 1
 ORDER BY account_external_id, posted_at`

// appendClosingBalances runs q, which yields (account, time, balance) rows
// bound by args, and appends one CLOSING cash balance per row to batch: in
// the account's currency (ccy), in the canonical sign (sign, nil for the
// figure as stored), with payload as its JSON payload. label prefixes the
// query's errors.
func appendClosingBalances(ctx context.Context, db *sql.DB, label, q string, args []any,
	payload string, ccy func(accountID string) string,
	sign func(accountID string, v float64) canonical.Decimal,
	batch *canonical.SnapshotBatch) error {
	return appendBalances(ctx, db, label, q, args, canonical.BalanceKindClosing, payload, ccy, sign, batch)
}

// appendBalances runs q, which yields (account, time, balance) rows bound
// by args, and appends one cash balance of kind per row to batch, as
// appendClosingBalances describes.
func appendBalances(ctx context.Context, db *sql.DB, label, q string, args []any,
	kind canonical.BalanceKind, payload string, ccy func(accountID string) string,
	sign func(accountID string, v float64) canonical.Decimal,
	batch *canonical.SnapshotBatch) error {
	return silver.ScanRows(ctx, db, label, q, func(rows *sql.Rows) error {
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
		batch.CashBalances = append(batch.CashBalances, silver.CashBalance(
			at, id, ccy(id), kind, amount, payload))
		return nil
	}, args...)
}

// appendReportedBalances appends one CURRENT cash balance per account and
// run that listed it inside w: the account's roster balance as of that run,
// from its latest `accounts` row at or before it, in its currency (ccy) and
// as stored. runsTable holds one (snapshot_at, account_external_id) row per
// loaded run and account the run's roster listed; it is the adapter's own
// constant, and the caller has probed that the silver holds it. An
// account's own roster rows count as listings too. A roster row with no
// balance emits none. kind names the collector in errors. A CURRENT
// balance read from the roster rows alone would stamp a quiet account at
// its last changed row, and read it as left where a later run listed only
// the others.
//
// Silver content-dedups an unchanged roster row, so the runs table is the
// only record that a quiet account was still listed. An account a run
// leaves out ends at that run, by the run's coverage (appendListedCovers,
// wealthdb/docs/DESIGN.md §10.7).
func appendReportedBalances(ctx context.Context, db *sql.DB, kind, runsTable string,
	w canonical.Window, ccy func(accountID string) string, batch *canonical.SnapshotBatch) error {
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
	return appendBalances(ctx, db, kind+" roster balances", q, []any{w.Start, w.End},
		canonical.BalanceKindCurrent, `{"basis":"roster"}`, ccy, nil, batch)
}

// appendListedCovers states, for each run inside w that recorded a
// listing in runsTable, the accounts it reports whole at its instant.
// A run reads the bank's whole roster, so it covers every account the
// roster has held since before it: the ones it listed and the ones it
// left out, which the bank no longer holds. A listed account is covered
// only where the run stated its balance (reported, the CURRENT balances
// appendReportedBalances emitted): a listing with no figure says nothing
// of the account's cash. A run that recorded no listing covers nothing.
func appendListedCovers(ctx context.Context, db *sql.DB, kind, runsTable string,
	w canonical.Window, roster []RosterRow, reported []canonical.CashBalanceChange,
	batch *canonical.SnapshotBatch) error {
	type runAccount struct {
		at int64
		id string
	}
	stated := make(map[runAccount]bool, len(reported))
	for _, b := range reported {
		stated[runAccount{b.SnapshotAt, b.AccountExternalID}] = true
	}
	listed := map[runAccount]bool{}
	var runs []int64
	err := silver.ScanRows(ctx, db, kind+" run listings", `
SELECT snapshot_at, account_external_id FROM `+runsTable+`
 WHERE snapshot_at BETWEEN ? AND ?
 ORDER BY snapshot_at, account_external_id`, func(rows *sql.Rows) error {
		var r runAccount
		if err := rows.Scan(&r.at, &r.id); err != nil {
			return err
		}
		if len(runs) == 0 || runs[len(runs)-1] != r.at {
			runs = append(runs, r.at)
		}
		listed[r] = true
		return nil
	}, w.Start, w.End)
	if err != nil {
		return err
	}
	for _, at := range runs {
		for _, r := range roster {
			k := runAccount{at, r.ID}
			if r.FirstSeen > at || listed[k] && !stated[k] {
				continue
			}
			batch.Covers = append(batch.Covers, canonical.CoverageChange{SnapshotAt: at, AccountExternalID: r.ID})
		}
	}
	return nil
}

// RosterRow is an account's latest row in a bank silver's `accounts`
// roster, with the span of roster snapshots it and the source appear in.
type RosterRow struct {
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

// readRoster reads each account's latest roster row, ordered by account.
// nameCol and typeCol name the roster's name and type columns; an empty
// typeCol reads no type. Both are the adapter's own constants. label
// prefixes the query's errors.
func readRoster(ctx context.Context, db *sql.DB, label, nameCol, typeCol string) ([]RosterRow, error) {
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
	return silver.Query(ctx, db, label, q, func(rows *sql.Rows) (RosterRow, error) {
		var r RosterRow
		err := rows.Scan(&r.ID, &r.Name, &r.AccountType, &r.Mask, &r.Balance, &r.Payload,
			&r.FirstSeen, &r.LastSeen, &r.SourceLatest)
		return r, err
	})
}

// DisplayName is the row's name, falling back to its mask.
func (r RosterRow) DisplayName() string {
	if r.Name != "" {
		return r.Name
	}
	return r.Mask
}

// Account is the row's account record: a bank account of kind in the
// taxable personal wrapper whose holder self-directs it, shown as display
// and kept in currency. Callers add the nickname where the name column
// holds one.
func (r RosterRow) Account(kind canonical.AccountKind, display, currency string) canonical.AccountChange {
	wrapper := canonical.TaxWrapperTaxablePersonal
	style := canonical.ManagementStyleSelfDirected
	return canonical.AccountChange{
		AccountExternalID: r.ID,
		AccountKind:       kind,
		DisplayName:       silver.StrPtrIfNonEmpty(display),
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
func (r RosterRow) CurrentBalance(w canonical.Window, currency string,
	amount canonical.Decimal) (canonical.CashBalanceChange, bool) {
	if !r.Balance.Valid || r.SourceLatest < w.Start || r.SourceLatest > w.End {
		return canonical.CashBalanceChange{}, false
	}
	return silver.CashBalance(r.SourceLatest, r.ID, currency, canonical.BalanceKindCurrent,
		amount, `{"basis":"roster"}`), true
}
