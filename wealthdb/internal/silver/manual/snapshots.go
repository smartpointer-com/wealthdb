package manual

import (
	"context"
	"database/sql"
	"encoding/json"
	"sort"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/silver"
)

// defaultAccountKey is the account a position falls into when the book
// declares none, so a book without accounts.csv holds one account under this
// stable id.
const defaultAccountKey = "manual"

// Snapshots reconstructs the per-date portfolio from the silver's positions +
// valuations (silver.Book). The silver stores a position once (with
// acquired_at / closed_at) and a valuation per (position, as_of_date); gold's
// as-of query, by contrast, takes each account's latest snapshot and reads
// ALL its positions. So for every event date — any date a position is
// acquired, re-valued, or closed — the stream carries a COMPLETE
// forward-filled snapshot: every position live at that date (acquired_at ≤
// date < closed_at), each marked at its latest valuation on/before the date.
// A closed position drops out exactly at its closed_at; a complete portfolio
// exists at every date so historical as-of queries work. A position carries
// no ValuedAt, and a book that empties emits an empty batch.
func (c *Connection) Snapshots(ctx context.Context, w canonical.Window) (silver.SnapshotStream, error) {
	if !w.HasChanges {
		return silver.NewSnapshotStream(nil), nil
	}
	states, err := c.states(ctx)
	if err != nil {
		return nil, err
	}
	declared, err := c.declaredAccounts(ctx)
	if err != nil {
		return nil, err
	}
	batches, err := silver.Book{
		States: states,
		Emit: func(t int64, held []silver.Holding) (canonical.SnapshotBatch, error) {
			return emit(t, held, declared), nil
		},
	}.Batches(w)
	if err != nil {
		return nil, err
	}
	return silver.NewSnapshotStream(batches), nil
}

// state is a position as of one of its event dates (silver.Holding.Row).
type state struct {
	account, kind, vehicle, currency, displayName, payload string
	acquired                                               sql.NullInt64
	marketValue, bookValue                                 sql.NullString
	paidIn                                                 bool
}

// states are every position as of each of its own event dates
// (positionEvents), oldest first (silver.Book). A position is held at a
// date while it is live (acquired_at ≤ date < closed_at), marked at its
// latest valuation on/before the date, with its cost basis
// (bookValueSQL). Every figure reads only the position's own events, so
// the state at its latest event date stands for every later date up to
// its next event. A valuation or cost basis entry whose position the book
// does not hold is a date with no position live.
func (c *Connection) states(ctx context.Context) ([]silver.Holding, error) {
	q := `
SELECT e.position_id, e.t,
       COALESCE(CAST(strftime('%s', p.acquired_at) AS INTEGER) <= e.t
                AND (p.closed_at IS NULL
                     OR CAST(strftime('%s', p.closed_at) AS INTEGER) > e.t), 0),
       COALESCE(p.account_id, '` + defaultAccountKey + `') AS account_id,
       COALESCE(p.kind, ''), COALESCE(p.vehicle, '') AS vehicle, COALESCE(p.currency, ''),
       COALESCE(p.display_name, ''),
       CAST(strftime('%s', p.acquired_at) AS INTEGER) AS acq_unix,
       COALESCE(p.payload, ''),
       (SELECT v.value FROM valuations v
         WHERE v.position_id = p.id
           AND CAST(strftime('%s', v.as_of_date) AS INTEGER) <= e.t
         ORDER BY CAST(strftime('%s', v.as_of_date) AS INTEGER) DESC
         LIMIT 1) AS market_value,
       ` + c.bookValueSQL("e.t") + ` AS book_value,
       ` + c.paidInSQL() + ` AS paid_in
  FROM (SELECT DISTINCT position_id, t FROM (` + c.positionEvents() + `) WHERE t IS NOT NULL) e
  LEFT JOIN positions p ON p.id = e.position_id
 ORDER BY e.position_id, e.t`
	return silver.Query(ctx, c.db, "states", q, func(rows *sql.Rows) (silver.Holding, error) {
		var (
			h      silver.Holding
			s      state
			paidIn sql.NullBool
		)
		err := rows.Scan(&h.Key, &h.At, &h.Open, &s.account, &s.kind, &s.vehicle, &s.currency,
			&s.displayName, &s.acquired, &s.payload, &s.marketValue, &s.bookValue, &paidIn)
		s.paidIn = paidIn.Bool
		h.Row = s
		return h, err
	})
}

// positionEvents is the SQL that lists every holding event date of each
// position, as unix seconds in column t beside position_id: the dates
// eventDates lists.
func (c *Connection) positionEvents() string {
	q := `
    SELECT id AS position_id, CAST(strftime('%s', acquired_at) AS INTEGER) AS t FROM positions
    UNION ALL SELECT id, CAST(strftime('%s', closed_at) AS INTEGER) FROM positions WHERE closed_at IS NOT NULL
    UNION ALL SELECT position_id, CAST(strftime('%s', as_of_date) AS INTEGER) FROM valuations`
	if c.schema.costBasis {
		q += `
    UNION ALL SELECT position_id, CAST(strftime('%s', as_of_date) AS INTEGER) FROM cost_basis`
	}
	return q
}

// emit is the batch at t of the positions held then: one position + one
// instrument per live position, plus one account per account those
// positions are held in.
//
// Only accounts holding something at t are emitted. Gold reads a snapshot as
// the complete state of the source at that date, so an account with nothing
// live in it has nothing to say — and emitting it would assert a container
// that held no value on a date it may not have existed.
func emit(t int64, held []silver.Holding, declared map[string]declaredAccount) canonical.SnapshotBatch {
	var batch canonical.SnapshotBatch
	accounts := map[string]bool{}
	for _, h := range held {
		s := h.Row.(state)
		accounts[s.account] = true
		acNew, veh := taxonomyFor(s.kind, s.vehicle)
		instKey := h.Key

		pos := canonical.PositionChange{
			SnapshotAt:           t,
			AccountExternalID:    s.account,
			PositionKey:          h.Key,
			InstrumentExternalID: &instKey,
			AssetClass:           acNew,
			Vehicle:              veh,
			Currency:             s.currency,
			AcquisitionDate:      silver.DatePtrFromNullUnix(s.acquired),
			Payload:              json.RawMessage(s.payload),
			// Quantity stays nil: manual holdings are valued by amount, not a
			// unit count (real estate, a loan, a whole-company stake, an LP
			// interest — none is unit-denominated).
		}
		// market_value = latest valuation ≤ t (forward-filled); book_value =
		// the cost basis (bookValueSQL). A liability kind (mortgage) is
		// entered as a positive outstanding balance — "direction comes from
		// kind" — so both are negated here, matching the gold convention
		// that liability positions carry a negative market_value and net
		// against assets in rollups.
		neg := s.kind == "mortgage"
		if s.marketValue.Valid {
			if mv, err := canonical.NewDecimalFromString(signed(s.marketValue.String, neg)); err == nil {
				pos.MarketValue = &mv
			}
		}
		if s.bookValue.Valid {
			if bv, err := canonical.NewDecimalFromString(signed(s.bookValue.String, neg)); err == nil {
				pos.SetBookValue(&bv, manualBasis(s.paidIn))
			}
		}
		batch.Positions = append(batch.Positions, pos)

		inst := canonical.InstrumentChange{
			InstrumentExternalID: instKey,
			AssetClass:           acNew,
			Vehicle:              veh,
			FirstSeenAt:          t,
			LastSeenAt:           t,
			Payload:              json.RawMessage(s.payload),
		}
		if s.displayName != "" {
			n := s.displayName
			inst.Name = &n
		}
		if s.currency != "" {
			ccy := s.currency
			inst.Currency = &ccy
		}
		batch.Instruments = append(batch.Instruments, inst)
	}
	batch.Accounts = accountsFor(declared, accounts, t)
	return batch
}

// bookValueSQL is the SQL for a position's cost basis at t, inside the
// states query over positions p. A position the cost_basis series covers takes its
// latest entry on or before t: the capital paid in so far, which for a
// commitment paid in over time is less than the commitment. It has none
// before its first entry. Any other position takes the valuation dated at its
// acquired_at, held constant while the market moves: the price of a bought
// asset. It reads t from the SQL expression at.
func (c *Connection) bookValueSQL(at string) string {
	atAcquisition := `(SELECT v.value FROM valuations v
         WHERE v.position_id = p.id
           AND CAST(strftime('%s', v.as_of_date) AS INTEGER)
               = CAST(strftime('%s', p.acquired_at) AS INTEGER)
         LIMIT 1)`
	if !c.schema.costBasis {
		return atAcquisition
	}
	return `CASE WHEN ` + seriesCovers + `
         THEN (SELECT cb.amount FROM cost_basis cb
                WHERE cb.position_id = p.id
                  AND CAST(strftime('%s', cb.as_of_date) AS INTEGER) <= ` + at + `
                ORDER BY cb.as_of_date DESC LIMIT 1)
         ELSE ` + atAcquisition + ` END`
}

// seriesCovers is the SQL predicate for whether the cost_basis series
// covers position p.
const seriesCovers = `EXISTS (SELECT 1 FROM cost_basis cb WHERE cb.position_id = p.id)`

// paidInSQL is the SQL for whether bookValueSQL reads the cost_basis
// series for the position, inside the same query.
func (c *Connection) paidInSQL() string {
	if !c.schema.costBasis {
		return `0`
	}
	return seriesCovers
}

// manualBasis stamps a manual book value: the capital paid in where the
// cost_basis series covers the position, else the valuation entered for
// the acquisition date. Both are figures entered as they are, with no
// statement of fees.
func manualBasis(paidIn bool) canonical.Basis {
	m := canonical.BasisMethodAcquisitionValue
	if paidIn {
		m = canonical.BasisMethodPaidIn
	}
	return canonical.Basis{Origin: canonical.BasisStated, Method: m, Fees: canonical.BasisFeesUnknown}
}

// declaredAccount is one row of the accounts table.
type declaredAccount struct{ name, kind, wrapper, style string }

// declaredAccounts are the accounts the book declares, by id.
func (c *Connection) declaredAccounts(ctx context.Context) (map[string]declaredAccount, error) {
	declared := map[string]declaredAccount{}
	if err := silver.ScanRows(ctx, c.db, "declaredAccounts", `
SELECT id, COALESCE(display_name, ''), COALESCE(account_kind, ''),
       COALESCE(tax_wrapper, ''), COALESCE(management_style, '')
  FROM accounts ORDER BY id`, func(rows *sql.Rows) error {
		var id string
		var a declaredAccount
		if err := rows.Scan(&id, &a.name, &a.kind, &a.wrapper, &a.style); err != nil {
			return err
		}
		declared[id] = a
		return nil
	}); err != nil {
		return nil, err
	}
	return declared, nil
}

// accountsFor is one AccountChange per account holding something at t,
// by id.
//
// The declared taxonomy wins over accountChange's defaults. No base_currency on any of them: a manual book's
// positions may span currencies, and the account is a sleeve rather than a
// denominated container.
func accountsFor(declared map[string]declaredAccount, held map[string]bool, t int64) []canonical.AccountChange {
	if len(held) == 0 {
		return nil
	}
	out := make([]canonical.AccountChange, 0, len(held))
	for id := range held {
		if a, ok := declared[id]; ok {
			out = append(out, accountChange(id, a.name, a.kind, a.wrapper, a.style, t))
			continue
		}
		// A position naming an account the book does not declare. load.py
		// refuses that, so reaching here means a silver without accounts.csv
		// (or one written by hand): project the defaults rather than drop the
		// account and take its positions down with it.
		out = append(out, accountChange(id, "Manual", "", "", "", t))
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].AccountExternalID < out[j].AccountExternalID
	})
	return out
}

// accountChange builds one account, filling each empty or unknown field with
// a default: display name "Manual"; account_kind 'other', because a
// directly-held asset has no institutional container; self_directed, because
// the holder decides what to hold; taxable_personal, because that is the
// common case and a sleeve is the exception worth declaring.
func accountChange(id, name, kind, wrapper, style string, t int64) canonical.AccountChange {
	if name == "" {
		name = "Manual"
	}
	ak := canonical.AccountKind(kind)
	if !ak.Valid() {
		ak = canonical.AccountKindOther
	}
	tw := canonical.TaxWrapper(wrapper)
	if !tw.Valid() {
		tw = canonical.TaxWrapperTaxablePersonal
	}
	ms := canonical.ManagementStyle(style)
	if !ms.Valid() {
		ms = canonical.ManagementStyleSelfDirected
	}
	return canonical.AccountChange{
		AccountExternalID: id,
		AccountKind:       ak,
		DisplayName:       &name,
		TaxWrapper:        &tw,
		ManagementStyle:   &ms,
		FirstSeenAt:       t,
		LastSeenAt:        t,
	}
}

// signed flips a positive-magnitude decimal string to negative for liability
// positions (mortgage); asset values pass through unchanged. The collector
// validates valuations as non-negative, so the input is always a magnitude.
func signed(magnitude string, neg bool) string {
	if neg {
		return "-" + magnitude
	}
	return magnitude
}
