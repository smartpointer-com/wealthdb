package gold

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"strconv"
	"strings"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/yield"
)

// placementInputs are the tables a placement reads (placeTransactions)
// beside a source's facts: the accounts and their portfolios and the
// holding aliases. A source's positions, cash balances, instruments and
// transactions move with its row in silver_sources, which every load
// that changes them rewrites.
var placementInputs = []string{"silver_sources", "accounts", "holding_aliases"}

// placementStamp is the stamp of what a placement reads: the rule's
// version (yield.PlaceVersion) and a stamp of each of placementInputs.
func placementStamp(ctx context.Context, q querier) (string, error) {
	parts := []string{strconv.Itoa(yield.PlaceVersion)}
	for _, table := range placementInputs {
		stamp, err := tableStamp(ctx, q, table)
		if err != nil {
			return "", err
		}
		parts = append(parts, stamp)
	}
	return strings.Join(parts, ":"), nil
}

// placement is the holder of one transaction (yield.Holders.Place): the
// key it belongs to and the step that placed it.
type placement struct {
	id    string
	key   unitKey
	basis yield.Placing
}

// placeTransactions places every transaction that names an instrument,
// in the order of its source and id, with the holder of the instrument
// as it happened (yield.Holders.Place). Who held what is read from
// every snapshot gold holds (readPoints), the instrument folded as the
// matching folds it (yield.Inputs.Fold), under the account that holds
// it and each other account of its portfolio
// (yield.Inputs.PortfolioKeys). A transaction that moves units moves
// them as the units reader reads it (unitMove), so a buy places by the
// first point after it too.
func placeTransactions(ctx context.Context, q querier) ([]placement, error) {
	in := &yield.Inputs{Accounts: map[yield.AccountKey]yield.Account{}, Aliases: map[yield.AliasKey]string{}}
	if err := readYieldAccounts(ctx, q, in); err != nil {
		return nil, err
	}
	if err := readYieldAliases(ctx, q, in); err != nil {
		return nil, err
	}
	type line struct {
		placement
		iid   string
		trade yield.UnitTrade
	}
	var lines []line
	// seen are the ids of each account whose keys paid holds.
	paid, seen := map[yield.InstrumentKey]bool{}, map[unitKey]bool{}
	if err := scanAll(ctx, q, `
SELECT silver_source_id, transaction_external_id, account_external_id, instrument_external_id, occurred_at, kind,
       CAST(COALESCE(quantity, 0) AS DOUBLE), COALESCE(action, '')
  FROM transactions
 WHERE instrument_external_id IS NOT NULL
 ORDER BY silver_source_id, transaction_external_id`, func(rows *sql.Rows) error {
		var l line
		var kind string
		var qty float64
		var action canonical.TxAction
		if err := rows.Scan(&l.key.Source, &l.id, &l.key.Account, &l.iid, &l.trade.At, &kind, &qty, &action); err != nil {
			return err
		}
		l.key.Instrument = in.Fold(l.key.Source, l.key.Account, l.iid)
		l.trade.Qty, l.trade.Corp = unitMove(lotClassify(kind, qty, action), qty)
		lines = append(lines, l)
		if own := (unitKey{Source: l.key.Source, Account: l.key.Account, Instrument: l.iid}); !seen[own] {
			seen[own] = true
			for _, k := range in.PortfolioKeys(l.key.Source, l.key.Account, l.iid) {
				paid[yield.InstrumentKey{Source: k.Source, Instrument: k.Instrument}] = true
			}
		}
		return nil
	}); err != nil {
		return nil, err
	}
	h := newHolders(in)
	if len(paid) > 0 {
		srcs, iids := paidLists(paid)
		if err := readHoldingPoints(ctx, q, h, srcs, iids); err != nil {
			return nil, err
		}
	}
	out := make([]placement, len(lines))
	for i, l := range lines {
		out[i] = l.placement
		out[i].key, out[i].basis = h.Place(l.key, l.iid, l.trade)
	}
	return out, nil
}

// readHoldingPoints reads into h the points readPoints reads of the
// instruments srcs and iids name (a pair at each index), over every
// snapshot gold holds: the same points, read from the positions alone.
// A key's quantity is 0 at a snapshot that speaks for its account
// (account_grid) and does not name it, so it changes at its first
// snapshot that speaks for the account (0 there where the key is not
// named), at each snapshot that names it whose quantity is not the
// one before it, at the snapshot after a run of it where the next names
// it no more, and at the snapshot after such a gap that names it again.
func readHoldingPoints(ctx context.Context, q querier, h *yield.Holders, srcs, iids []string) error {
	return scanAll(ctx, q, unitWindowCTE+unitAccountsCTE+unitHeldCTE+`
grid AS (
    SELECT silver_source_id, account_external_id, snapshot_at, lag(snapshot_at) OVER a AS acct_prev,
           lead(snapshot_at) OVER a AS acct_next, first_value(snapshot_at) OVER a AS acct_first
      FROM account_grid
    WINDOW a AS (PARTITION BY silver_source_id, account_external_id ORDER BY snapshot_at)),
runs AS (
    SELECT h.*, g.acct_prev, g.acct_next, g.acct_first, lag(h.snapshot_at) OVER k AS prev_held,
           lag(h.qty) OVER k AS prev_qty, lead(h.snapshot_at) OVER k AS next_held, row_number() OVER k AS i
      FROM held h
      JOIN grid g ON g.silver_source_id = h.silver_source_id AND g.account_external_id = h.account_external_id
           AND g.snapshot_at = h.snapshot_at
    WINDOW k AS (PARTITION BY h.silver_source_id, h.account_external_id, h.instrument_external_id
                 ORDER BY h.snapshot_at))
SELECT silver_source_id, account_external_id, instrument_external_id, acct_first, 0::DOUBLE FROM runs
 WHERE i = 1 AND acct_prev IS NOT NULL
UNION ALL
SELECT silver_source_id, account_external_id, instrument_external_id, snapshot_at, qty FROM runs
 WHERE CASE WHEN i = 1 THEN acct_prev IS NULL OR qty IS DISTINCT FROM 0
            WHEN prev_held = acct_prev THEN qty IS DISTINCT FROM prev_qty
            ELSE qty IS DISTINCT FROM 0 END
UNION ALL
SELECT silver_source_id, account_external_id, instrument_external_id, acct_next, 0::DOUBLE FROM runs
 WHERE acct_next IS NOT NULL AND acct_next IS DISTINCT FROM next_held AND qty IS DISTINCT FROM 0
 ORDER BY 1, 2, 3, 4`, func(rows *sql.Rows) error {
		var k unitKey
		var at int64
		var qty sql.NullFloat64
		if err := rows.Scan(&k.Source, &k.Account, &k.Instrument, &at, &qty); err != nil {
			return err
		}
		if _, seen := h.Points[k]; !seen {
			ik := yield.InstrumentKey{Source: k.Source, Instrument: k.Instrument}
			h.Accounts[ik] = append(h.Accounts[ik], k.Account)
		}
		h.Points[k] = append(h.Points[k], yield.UnitPoint{At: at, Qty: nullFloatToPtr(qty)})
		return nil
	}, srcs, iids, 0, int64(math.MaxInt64))
}

// writeTransactionHoldings rewrites transaction_holdings with every
// transaction's placement (placeTransactions) and the stamp of the
// inputs it read.
func writeTransactionHoldings(ctx context.Context, tx *sql.Tx) error {
	stamp, err := placementStamp(ctx, tx)
	if err != nil {
		return err
	}
	ps, err := placeTransactions(ctx, tx)
	if err != nil {
		return err
	}
	return storePlacements(ctx, tx, ps, stamp)
}

// storePlacements replaces transaction_holdings with ps and their
// stamp.
func storePlacements(ctx context.Context, tx *sql.Tx, ps []placement, stamp string) error {
	for _, stmt := range []string{`DELETE FROM transaction_holdings`, `DELETE FROM transaction_holdings_inputs`} {
		if _, err := tx.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("transaction holdings: %w", err)
		}
	}
	if err := insertColumns(ctx, tx, "transaction holdings", "transaction_holdings",
		"silver_source_id, transaction_external_id, holder_account, instrument, basis", len(ps),
		func(i int, args []any) []any {
			p := ps[i]
			return append(args, p.key.Source, p.id, p.key.Account, p.key.Instrument, string(p.basis))
		}); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO transaction_holdings_inputs (stamp) VALUES (?)`, stamp); err != nil {
		return fmt.Errorf("transaction holdings: stamp: %w", err)
	}
	return nil
}

// readPlacements are the holders of the transactions a step placed
// (yield.Inputs.Placed): transaction_holdings where its stamp is that of
// the inputs gold holds, else placed here (placeTransactions), and
// stored where write is set. So a gold the yield pass has not placed
// since its inputs moved, such as one before its first pass, reads the
// same holders.
func readPlacements(ctx context.Context, q querier, write *sql.Tx) (map[yield.TxnKey]unitKey, error) {
	stamp, err := placementStamp(ctx, q)
	if err != nil {
		return nil, err
	}
	stored, err := storedPlacementStamp(ctx, q)
	if err != nil {
		return nil, err
	}
	out := map[yield.TxnKey]unitKey{}
	if stored == stamp {
		return out, scanAll(ctx, q, `
SELECT silver_source_id, transaction_external_id, holder_account, instrument FROM transaction_holdings
 WHERE basis <> ?`, func(rows *sql.Rows) error {
			var id string
			var k unitKey
			if err := rows.Scan(&k.Source, &id, &k.Account, &k.Instrument); err != nil {
				return err
			}
			out[yield.TxnKey{Source: k.Source, Transaction: id}] = k
			return nil
		}, string(yield.PlacedNone))
	}
	ps, err := placeTransactions(ctx, q)
	if err != nil {
		return nil, err
	}
	if write != nil {
		if err := storePlacements(ctx, write, ps, stamp); err != nil {
			return nil, err
		}
	}
	for _, p := range ps {
		if p.basis != yield.PlacedNone {
			out[yield.TxnKey{Source: p.key.Source, Transaction: p.id}] = p.key
		}
	}
	return out, nil
}

// storedPlacementStamp is the stamp transaction_holdings was placed
// from, "" where none is stored or gold has no such table.
func storedPlacementStamp(ctx context.Context, q querier) (string, error) {
	cols, err := tableColumns(ctx, q, "transaction_holdings_inputs")
	if err != nil || len(cols) == 0 {
		return "", err
	}
	var stamp string
	err = q.QueryRowContext(ctx, `SELECT stamp FROM transaction_holdings_inputs`).Scan(&stamp)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return stamp, err
}
