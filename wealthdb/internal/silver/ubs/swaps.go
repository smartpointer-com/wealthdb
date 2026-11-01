package ubs

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/silver"
)

// The FX forwards open before the PSN feed states any, inferred from
// the FX swaps the netbanking ledger books (docs/adapters/ubs.md §14).
//
// A swap's near leg converts on the day it opens and its far leg
// converts back on the day it matures; between the two the swap is a
// forward. The ledger books each leg as a `Purchase from FX Swap` row
// and a `Sale from FX Swap` row, one per cash account. Both rows name
// the leg (`(near)`, `(far)`) and the deal reference (`FX <ref>`), and
// state the leg's two amounts. The near and far legs of one swap carry
// one reference.
//
// Each swap becomes a forward held on the portfolio's overlay account,
// on the days the statement snapshots state the portfolio's securities,
// from the near leg's day to the far leg's, and never from the PSN
// feed's first forward day on: the feed states its forwards itself.
// The far leg says what the forward buys and sells; without one, the
// near leg's opposite does. The bank states no mark for these days, so
// the forward is worth zero and only its legs count, in exposure.

// swapProvider names the netbanking ledger as the inferred legs'
// composition provider.
const swapProvider = "ubs-web"

// swapLedgerKinds are the booking types of a swap leg.
var swapLedgerKinds = []string{"Purchase from FX Swap", "Sale from FX Swap"}

var (
	// swapLegInNarrative is the leg and the deal reference, as in
	// `You sold CHF (near); You bought USD; FX AB-12345`.
	swapLegInNarrative = regexp.MustCompile(`\((near|far)\).*\bFX ([0-9A-Z]+-[0-9A-Z]+)\b`)
	// swapSoldInNarrative and swapBoughtInNarrative are the leg's two
	// amounts, as in `You sold: -1000 CHF; You bought: 1100.00 USD`.
	swapSoldInNarrative   = regexp.MustCompile(`You sold: -?([0-9]+(?:\.[0-9]+)?) ([A-Z]{3})\b`)
	swapBoughtInNarrative = regexp.MustCompile(`You bought: -?([0-9]+(?:\.[0-9]+)?) ([A-Z]{3})\b`)
	// statementDealInNarrative is the deal number a statement prints
	// first under a conversion, as in `0001234/123456789`.
	statementDealInNarrative = regexp.MustCompile(`^([0-9]+)/[0-9]+$`)
)

// swapLeg is one leg of a swap: its day, the accounts it books on, and
// what it sells and buys, both amounts above zero.
type swapLeg struct {
	day          int64
	accounts     []string
	sold, bought forwardLeg
}

// inferredSwap is a swap as a forward: open over [start, end), long
// and short at notional (the short amount below zero).
type inferredSwap struct {
	ref         string
	start, end  int64
	maturity    int64 // the far leg's day; 0 where none is booked
	long, short forwardLeg
	accounts    []string
	// beforeLedger says the near leg was found nowhere: the swap
	// opens on the first day of the netbanking ledger.
	beforeLedger bool
}

// id is the adapter's own id of the forward, from its deal reference.
func (s inferredSwap) id() string { return "fx-swap " + s.ref }

func (s inferredSwap) name() string {
	maturity := "open"
	if s.maturity > 0 {
		maturity = time.Unix(s.maturity, 0).UTC().Format(time.DateOnly)
	}
	return "FX forward " + s.long.ccy + "/" + s.short.ccy + " " + maturity
}

func (s inferredSwap) payload() json.RawMessage {
	p := map[string]string{"inferred": "fx_swap"}
	if s.beforeLedger {
		p["opened"] = "before_ledger"
	}
	b, _ := json.Marshal(p)
	return b
}

// swapLegs reads the ledger's swap legs, per deal reference the near
// leg and the far leg (either nil). A row whose narrative names no leg,
// no reference or not both amounts is no leg.
func (r *webReader) swapLegs(ctx context.Context) (map[string]*[2]*swapLeg, error) {
	rows, err := r.db.QueryContext(ctx, `
SELECT value_date, account_external_id, payload FROM transactions
 WHERE description_kind IN (?, ?)
 ORDER BY value_date, account_external_id`, swapLedgerKinds[0], swapLedgerKinds[1])
	if err != nil {
		return nil, fmt.Errorf("swapLegs: %w", err)
	}
	defer rows.Close()
	out := map[string]*[2]*swapLeg{}
	for rows.Next() {
		var (
			day              int64
			account, payload string
		)
		if err := rows.Scan(&day, &account, &payload); err != nil {
			return nil, err
		}
		var pl webTxPayload
		if json.Unmarshal([]byte(payload), &pl) != nil {
			continue
		}
		m := swapLegInNarrative.FindStringSubmatch(pl.Description1)
		sold, okSold := narrativeLeg(swapSoldInNarrative, pl.Description3)
		bought, okBought := narrativeLeg(swapBoughtInNarrative, pl.Description3)
		if m == nil || !okSold || !okBought || sold.ccy == bought.ccy {
			continue
		}
		legs := out[m[2]]
		if legs == nil {
			legs = &[2]*swapLeg{}
			out[m[2]] = legs
		}
		i := 0
		if m[1] == "far" {
			i = 1
		}
		if legs[i] == nil {
			legs[i] = &swapLeg{day: utcDay(day), sold: sold, bought: bought}
		}
		if !slices.Contains(legs[i].accounts, account) {
			legs[i].accounts = append(legs[i].accounts, account)
		}
	}
	return out, rows.Err()
}

// narrativeLeg is the amount and currency re finds in s.
func narrativeLeg(re *regexp.Regexp, s string) (forwardLeg, bool) {
	m := re.FindStringSubmatch(s)
	if m == nil {
		return forwardLeg{}, false
	}
	amount, err := canonical.NewDecimalFromString(m[1])
	if err != nil || amount.IsZero() {
		return forwardLeg{}, false
	}
	return forwardLeg{ccy: m[2], amount: amount}, true
}

// ledgerStart is the first day of the netbanking ledger on any of the
// accounts, or 0 where none has one.
func (r *webReader) ledgerStart(ctx context.Context, accounts []string) (int64, error) {
	var first int64
	for _, a := range accounts {
		var day sql.NullInt64
		if err := r.db.QueryRowContext(ctx, `
SELECT MIN(value_date) FROM transactions
 WHERE account_external_id = ? AND transaction_external_id NOT LIKE ?`, a, statementIDPrefix+"%").
			Scan(&day); err != nil {
			return 0, fmt.Errorf("ledgerStart: %w", err)
		}
		if day.Valid && (first == 0 || utcDay(day.Int64) < first) {
			first = utcDay(day.Int64)
		}
	}
	return first, nil
}

// statementNearLeg is the day of a far leg's near leg in the statement
// era, which types a conversion as plain FX. The near leg is the one
// deal on the far leg's accounts, in the year before the far leg and
// before the netbanking ledger starts, whose two rows sell the currency
// the far leg buys, at the far leg's amount, and buy the currency the
// far leg sells. A statement prints the deal's number first under each
// of its rows. ok is false where no deal or more than one matches.
func (r *webReader) statementNearLeg(ctx context.Context, far *swapLeg, ledger int64) (int64, bool, error) {
	from, to := far.day-366*86400, far.day
	if ledger > 0 && ledger < to {
		to = ledger
	}
	args := []any{from, to, statementIDPrefix + "%"}
	for _, a := range far.accounts {
		args = append(args, a)
	}
	rows, err := r.db.QueryContext(ctx, `
SELECT value_date, currency_iso, amount_debit, amount_credit, description_kind, payload
  FROM transactions
 WHERE value_date >= ? AND value_date < ? AND transaction_external_id LIKE ?
   AND account_external_id IN (?`+strings.Repeat(", ?", len(far.accounts)-1)+`)`, args...)
	if err != nil {
		return 0, false, fmt.Errorf("statementNearLeg: %w", err)
	}
	defer rows.Close()
	type deal struct {
		day int64
		ref string
	}
	// Per deal, whether a row sells the far leg's bought amount, buys
	// its sold currency, and how many rows it has.
	type seen struct{ sells, buys, rows int }
	deals := map[deal]*seen{}
	for rows.Next() {
		var (
			ccy, payload  string
			day           int64
			debit, credit sql.NullFloat64
			descKind      sql.NullString
		)
		if err := rows.Scan(&day, &ccy, &debit, &credit, &descKind, &payload); err != nil {
			return 0, false, err
		}
		if webKindByType[strings.ToUpper(descKind.String)] != canonical.TxKindFx ||
			webReversal(descKind.String, true, debit, credit) {
			continue
		}
		var pl webTxPayload
		if json.Unmarshal([]byte(payload), &pl) != nil || len(pl.Continuation) == 0 {
			continue
		}
		m := statementDealInNarrative.FindStringSubmatch(strings.TrimSpace(pl.Continuation[0]))
		if m == nil {
			continue
		}
		k := deal{utcDay(day), m[1]}
		if deals[k] == nil {
			deals[k] = &seen{}
		}
		s := deals[k]
		s.rows++
		switch {
		case ccy == far.bought.ccy && debit.Valid && !credit.Valid &&
			canonical.NewDecimalFromFloat(debit.Float64).Equal(far.bought.amount):
			s.sells++
		case ccy == far.sold.ccy && credit.Valid && !debit.Valid && credit.Float64 > 0:
			s.buys++
		}
	}
	if err := rows.Err(); err != nil {
		return 0, false, err
	}
	var found []int64
	for k, s := range deals {
		if s.rows == 2 && s.sells == 1 && s.buys == 1 {
			found = append(found, k.day)
		}
	}
	if len(found) != 1 {
		return 0, false, nil
	}
	return found[0], true, nil
}

// inferredSwaps are the ledger's swaps as forwards, each ended by the
// PSN feed's first forward day (psnForwards; 0 for none). A swap that
// would open on or after its end is left out.
func (r *webReader) inferredSwaps(ctx context.Context, psnForwards int64) ([]inferredSwap, error) {
	legs, err := r.swapLegs(ctx)
	if err != nil {
		return nil, err
	}
	refs := make([]string, 0, len(legs))
	for ref := range legs {
		refs = append(refs, ref)
	}
	slices.Sort(refs)
	var out []inferredSwap
	for _, ref := range refs {
		near, far := legs[ref][0], legs[ref][1]
		s := inferredSwap{ref: ref}
		switch {
		case far != nil:
			s.long, s.short, s.maturity, s.end = far.bought, far.sold, far.day, far.day
			s.accounts = far.accounts
		default:
			s.long, s.short = near.sold, near.bought
			s.accounts = near.accounts
		}
		s.short.amount = s.short.amount.Neg()
		switch {
		case near != nil:
			s.start = near.day
		default:
			ledger, err := r.ledgerStart(ctx, far.accounts)
			if err != nil {
				return nil, err
			}
			day, ok, err := r.statementNearLeg(ctx, far, ledger)
			if err != nil {
				return nil, err
			}
			s.start, s.beforeLedger = day, !ok
			if !ok {
				s.start = ledger
			}
		}
		if psnForwards > 0 && (s.end == 0 || psnForwards < s.end) {
			s.end = psnForwards
		}
		if s.start > 0 && (s.end == 0 || s.start < s.end) {
			out = append(out, s)
		}
	}
	return out, nil
}

// appendInferredForwards puts each swap on its portfolio's overlay
// account, on the snapshots in its span that hold the portfolio's
// securities: the days the portfolio's other holdings carry. It runs
// after the statement securities, which it reads the days from. A swap
// on no cash account of a known portfolio is left out.
//
// The overlay carries its last such snapshot until its next one. So on
// the first of those days, before f.until, where no swap is open any
// more, the overlay states the forwards of the day before at zero: a
// closure, as silver.ClosureMarkerBatch writes one, but with no
// instrument, so no composition of the forward's splits it again.
func appendInferredForwards(byTime map[int64]*canonical.SnapshotBatch, f inferredForwards,
	safekeepingByPortfolio map[string]string) {
	snaps := slices.Sorted(maps.Keys(byTime))
	byPortfolio := map[string][]inferredSwap{}
	for _, s := range f.swaps {
		for _, a := range s.accounts {
			if p := f.portfolioOf[a]; p != "" {
				byPortfolio[p] = append(byPortfolio[p], s)
				break
			}
		}
	}
	for _, portfolio := range slices.Sorted(maps.Keys(byPortfolio)) {
		securities, _ := statementSecuritiesAccount(safekeepingByPortfolio, portfolio)
		overlayID := overlayAccountID(portfolio)
		var held []canonical.PositionChange
		for _, at := range snaps {
			batch := byTime[at]
			if !slices.ContainsFunc(batch.Positions, func(p canonical.PositionChange) bool {
				return p.AccountExternalID == securities
			}) {
				continue
			}
			var open []inferredSwap
			for _, s := range byPortfolio[portfolio] {
				if at >= s.start && (s.end == 0 || at < s.end) {
					open = append(open, s)
				}
			}
			switch {
			case len(open) > 0:
				held = held[:0]
				for _, s := range open {
					held = append(held, appendInferredForward(batch, at, f.source, overlayID, s))
				}
			case len(held) > 0 && (f.until == 0 || at < f.until):
				for _, p := range held {
					zero := canonical.Decimal{}
					p.SnapshotAt, p.InstrumentExternalID, p.MarketValue, p.Payload = at, nil, &zero,
						silver.ClosureMarkerPayload
					batch.Positions = append(batch.Positions, p)
				}
				held = nil
			default:
				continue
			}
			if !slices.ContainsFunc(batch.Accounts, func(a canonical.AccountChange) bool {
				return a.AccountExternalID == overlayID
			}) {
				pid := portfolio
				batch.Accounts = append(batch.Accounts, canonical.AccountChange{
					AccountExternalID:   overlayID,
					AccountKind:         canonical.AccountKindOverlay,
					TaxWrapper:          relationshipTaxWrapper(),
					DisplayName:         silver.StrPtrIfNonEmpty("Portfolio overlay"),
					PortfolioExternalID: &pid,
					FirstSeenAt:         at,
					LastSeenAt:          at,
				})
			}
		}
	}
}

// appendInferredForward adds the swap s to batch as a forward on the
// overlay account at the snapshot at, worth zero, with its instrument
// and its legs as the instrument's composition, keyed in source, and
// returns the position.
func appendInferredForward(batch *canonical.SnapshotBatch, at int64, source, overlayID string,
	s inferredSwap) canonical.PositionChange {
	zero := canonical.Decimal{}
	ccy := s.short.ccy
	inst, comp, line := forwardRecords(at, source, s.id(), s.name(), ccy, &zero, s.long, s.short)
	comp.Provider, comp.Payload = swapProvider, s.payload()
	pos := canonical.PositionChange{
		SnapshotAt:           at,
		AccountExternalID:    overlayID,
		PositionKey:          inst.InstrumentExternalID,
		InstrumentExternalID: &inst.InstrumentExternalID,
		AssetClass:           canonical.AssetClassForeignExchange,
		Vehicle:              canonical.VehicleForward,
		Currency:             ccy,
		MarketValue:          &zero,
		Payload:              s.payload(),
	}
	batch.Instruments = append(batch.Instruments, inst)
	batch.FundCompositions = append(batch.FundCompositions, comp)
	batch.FundLines = append(batch.FundLines, line)
	batch.Positions = append(batch.Positions, pos)
	return pos
}

// inferredForwards is what the statement stream places the swaps by:
// the swaps, each cash account's portfolio, and the PSN feed's first
// forward day (until; 0 for none), from which the feed states them.
type inferredForwards struct {
	source      string
	swaps       []inferredSwap
	portfolioOf map[string]string
	until       int64
}

// firstForwardDay is the UTC day of the feed's first forward_contracts
// snapshot, or 0 where it has none.
func (r *psnReader) firstForwardDay(ctx context.Context) (int64, error) {
	var first sql.NullInt64
	if err := r.db.QueryRowContext(ctx, `SELECT MIN(snapshot_at) FROM forward_contracts`).Scan(&first); err != nil {
		return 0, fmt.Errorf("psn firstForwardDay: %w", err)
	}
	if !first.Valid {
		return 0, nil
	}
	return utcDay(first.Int64), nil
}

// portfolioByCashAccount maps each cash account to the portfolio its
// latest snapshot names.
func (r *psnReader) portfolioByCashAccount(ctx context.Context) (map[string]string, error) {
	rows, err := r.db.QueryContext(ctx, `
SELECT account_external_id, portfolio_external_id FROM cash_accounts
 WHERE portfolio_external_id <> ''
 ORDER BY snapshot_at`)
	if err != nil {
		return nil, fmt.Errorf("psn portfolioByCashAccount: %w", err)
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var account, portfolio string
		if err := rows.Scan(&account, &portfolio); err != nil {
			return nil, err
		}
		out[account] = portfolio
	}
	return out, rows.Err()
}
