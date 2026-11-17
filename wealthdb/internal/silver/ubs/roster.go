package ubs

import (
	"cmp"
	"context"
	"database/sql"
	"fmt"
	"slices"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/silver"
)

// psnRoster is the PSN feed's account master data: the cash accounts,
// the custody (safekeeping) accounts, and the names the relationship is
// held under. It is read once per connection (psnReader.roster), and
// every pass asks it through the lookups below.
//
// The loader writes a master-data row per (relationship, account) only
// when its payload changed, so each table holds an account's row at the
// snapshots where it changed. A lookup's "current" is one of three
// readings of that:
//
//   - each account at its latest row (current);
//   - each account at its latest row that names a portfolio
//     (portfolioByCashAccount);
//   - any row the table holds (cashAccountIBANs, knownCustody, holders,
//     firstSnapshotByRelationship).
type psnRoster struct {
	// cash and custody are every row of cash_accounts and
	// safekeeping_accounts, in the table's row order.
	cash    []rosterRow
	custody []rosterRow
	// holders is the names the relationship is held under (holderNames).
	holders map[string]bool
}

// rosterRow is one master-data row, with the payload fields the lookups
// read as the silver states them (json_extract).
type rosterRow struct {
	snapshot             int64
	relationship         string
	account              string
	portfolio            sql.NullString // the promoted portfolio_external_id
	payloadPortfolio     sql.NullString // payload PrtflId
	currency, internalID sql.NullString // payload AcctCcyIsoCd and AcctId (cash)
}

// portfolioOf is the portfolio the row names: the promoted column, else
// the payload the collector promoted it from. The column is empty on
// snapshots taken before it existed, and the roster is long-lived
// enough that such a snapshot can still be the one read.
func (row rosterRow) portfolioOf() string {
	if row.portfolio.Valid {
		return row.portfolio.String
	}
	return row.payloadPortfolio.String
}

// roster is the feed's master data, read on the first call; later calls
// return the same roster. A reader with no silver behind it has an
// empty one.
func (r *psnReader) roster(ctx context.Context) (*psnRoster, error) {
	if r == nil || r.db == nil {
		return &psnRoster{holders: map[string]bool{}}, nil
	}
	return r.accounts.Get(func() (*psnRoster, error) { return r.readRoster(ctx) })
}

// readRoster is roster's read.
func (r *psnReader) readRoster(ctx context.Context) (*psnRoster, error) {
	out := &psnRoster{holders: map[string]bool{}}
	if err := silver.ScanRows(ctx, r.db, "ubs-psn cash accounts", `
SELECT snapshot_at, relationship_id, account_external_id, portfolio_external_id,
       json_extract(payload, '$.PrtflId'), json_extract(payload, '$.AcctCcyIsoCd'),
       json_extract(payload, '$.AcctId')
  FROM cash_accounts
 ORDER BY rowid`, func(rows *sql.Rows) error {
		var row rosterRow
		if err := rows.Scan(&row.snapshot, &row.relationship, &row.account, &row.portfolio,
			&row.payloadPortfolio, &row.currency, &row.internalID); err != nil {
			return fmt.Errorf("ubs-psn cash accounts scan: %w", err)
		}
		out.cash = append(out.cash, row)
		return nil
	}); err != nil {
		return nil, err
	}
	if err := silver.ScanRows(ctx, r.db, "ubs-psn safekeeping accounts", `
SELECT snapshot_at, relationship_id, account_external_id, portfolio_external_id,
       json_extract(payload, '$.PrtflId')
  FROM safekeeping_accounts
 ORDER BY rowid`, func(rows *sql.Rows) error {
		var row rosterRow
		if err := rows.Scan(&row.snapshot, &row.relationship, &row.account, &row.portfolio,
			&row.payloadPortfolio); err != nil {
			return fmt.Errorf("ubs-psn safekeeping accounts scan: %w", err)
		}
		out.custody = append(out.custody, row)
		return nil
	}); err != nil {
		return nil, err
	}
	if err := silver.ScanRows(ctx, r.db, "ubs holderNames", `
SELECT DISTINCT json_extract(payload, '$.FrstSurNm') FROM account_holders`, func(rows *sql.Rows) error {
		var name sql.NullString
		if err := rows.Scan(&name); err != nil {
			return fmt.Errorf("ubs holderNames scan: %w", err)
		}
		if n := normalizeHolderName(name.String); n != "" {
			out.holders[n] = true
		}
		return nil
	}); err != nil {
		return nil, err
	}
	return out, nil
}

// byKey orders rows as the tables' primary key does: snapshot,
// relationship, account.
func byKey(a, b rosterRow) int {
	return cmp.Or(cmp.Compare(a.snapshot, b.snapshot),
		cmp.Compare(a.relationship, b.relationship), cmp.Compare(a.account, b.account))
}

// current is the roster as it stands at the table's newest snapshot:
// each account of each relationship at its latest row, in relationship
// and account order. The loader reads an account's state at a snapshot
// the same way when it decides whether a row changed. An account the
// newest batch left unchanged has no row there and stands at an older
// one.
func current(rows []rosterRow) []rosterRow {
	type key struct{ relationship, account string }
	at := map[key]int{}
	var out []rosterRow
	for _, row := range rows {
		k := key{row.relationship, row.account}
		switch i, seen := at[k]; {
		case !seen:
			at[k] = len(out)
			out = append(out, row)
		case row.snapshot > out[i].snapshot:
			out[i] = row
		}
	}
	slices.SortFunc(out, func(a, b rosterRow) int {
		return cmp.Or(cmp.Compare(a.relationship, b.relationship), cmp.Compare(a.account, b.account))
	})
	return out
}

// holderNames is the names the relationship is held under, as the
// master-data feed states them. A conversion between own accounts names
// the holder on both sides; a payment abroad in a foreign currency,
// which carries the same subfield, names someone else.
func (ro *psnRoster) holderNames() map[string]bool { return ro.holders }

// portfolioCashAccounts maps (portfolio, currency) onto the cash
// account gold keys by, in the current roster.
//
// The export names the custody account the securities moved in and the
// currency the cash moved in, never the cash account itself. A managed
// portfolio holds one cash account per currency, so the pair
// determines it — through the bank's own account master data, which is
// the sibling feed's: the web collector's account rows carry the
// banking relationship and whichever scope a positions export was
// taken under, not the numbered portfolio.
//
// A pair naming more than one account is dropped rather than guessed
// at. Two accounts of one currency under one portfolio is a dormant
// account beside a live one, and settling a trade against the wrong
// one would move a balance that never moved.
func (ro *psnRoster) portfolioCashAccounts() map[portfolioCashKey]string {
	accounts := map[portfolioCashKey][]string{}
	for _, row := range current(ro.cash) {
		portfolio := row.portfolioOf()
		if portfolio == "" || row.currency.String == "" || row.account == "" {
			continue
		}
		k := portfolioCashKey{portfolio: portfolio, currency: bookingCurrency(row.currency.String)}
		accounts[k] = append(accounts[k], row.account)
	}
	out := make(map[portfolioCashKey]string, len(accounts))
	for k, accts := range accounts {
		if len(accts) == 1 {
			out[k] = accts[0]
		}
	}
	return out
}

// portfolioOfSafekeeping maps a custody account onto the portfolio it
// belongs to, in the current roster — the half of portfolioCashKey
// the export does not state in a form the rest of the adapter shares.
//
// The list names the portfolio in a column of its own, and that column
// is not this: the two identifier spaces look alike and pad
// differently, and one scope of the list (a consolidated view) reports
// a booking under an id that names no portfolio at all. The custody
// account it moved in is stated the same way by both feeds, and it
// belongs to exactly one portfolio, so it answers for every row
// whichever scope reported it.
func (ro *psnRoster) portfolioOfSafekeeping() map[string]string {
	return portfolioOfAccount(current(ro.custody))
}

// portfolioOfCashAccount maps each cash account onto the portfolio it
// belongs to, in the current roster — the other half of
// portfolioCashKey for a row the feed booked on a cash account.
func (ro *psnRoster) portfolioOfCashAccount() map[string]string {
	return portfolioOfAccount(current(ro.cash))
}

// portfolioOfAccount maps each row's account onto the portfolio it
// names, where it names one; a later row of an account wins.
func portfolioOfAccount(rows []rosterRow) map[string]string {
	out := map[string]string{}
	for _, row := range rows {
		if p := row.portfolioOf(); p != "" {
			out[row.account] = p
		}
	}
	return out
}

// portfolioByCashAccount maps each cash account to the portfolio its
// latest snapshot names: its latest row whose promoted column names
// one.
func (ro *psnRoster) portfolioByCashAccount() map[string]string {
	rows := slices.Clone(ro.cash)
	slices.SortFunc(rows, byKey)
	out := map[string]string{}
	for _, row := range rows {
		if row.portfolio.String != "" {
			out[row.account] = row.portfolio.String
		}
	}
	return out
}

// cashAccountIBANs maps the bank's internal cash-account id onto the
// IBAN the rest of the adapter — and gold's account registry — is keyed
// by, over every row. The master-data feed states both, and the MT940
// feed already names accounts by IBAN; only the MT515 confirmation uses
// the internal form, so without the map its rows name an account
// nothing else does. Of two IBANs for one id, the pair the table holds
// later wins.
func (ro *psnRoster) cashAccountIBANs() map[string]string {
	type pair struct{ internalID, iban string }
	seen := map[pair]bool{}
	out := map[string]string{}
	for _, row := range ro.cash {
		if !row.internalID.Valid {
			continue
		}
		p := pair{row.internalID.String, row.account}
		if seen[p] {
			continue
		}
		seen[p] = true
		if p.internalID != "" && p.iban != "" {
			out[p.internalID] = p.iban
		}
	}
	return out
}

// knownCustody is the custody accounts the feed reports, over every row.
func (ro *psnRoster) knownCustody() map[string]bool {
	out := map[string]bool{}
	for _, row := range ro.custody {
		out[row.account] = true
	}
	return out
}

// custodyByPortfolio groups the custody accounts of the current roster
// by the portfolio their payload names, each portfolio's accounts in
// account order (psnReader.safekeepingByPortfolio). ok is false where
// the table holds no row.
func (ro *psnRoster) custodyByPortfolio() (map[string][]string, bool) {
	rows := current(ro.custody)
	if len(rows) == 0 {
		return nil, false
	}
	slices.SortStableFunc(rows, func(a, b rosterRow) int { return cmp.Compare(a.account, b.account) })
	out := map[string][]string{}
	for _, row := range rows {
		if p := row.payloadPortfolio.String; p != "" {
			out[p] = append(out[p], row.account)
		}
	}
	return out, true
}

// firstSnapshotByRelationship maps each relationship onto the earliest
// snapshot of its cash accounts, over every row: the day the feed starts
// to cover it (buildPSNStartByWebRel).
func (ro *psnRoster) firstSnapshotByRelationship() map[string]int64 {
	out := map[string]int64{}
	for _, row := range ro.cash {
		if first, ok := out[row.relationship]; !ok || row.snapshot < first {
			out[row.relationship] = row.snapshot
		}
	}
	return out
}
