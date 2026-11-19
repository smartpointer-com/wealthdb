package ubs

import (
	"context"
	"math"
	"slices"
	"sort"
	"strconv"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
)

// offsetVetoEps bounds the amount mismatch for the same-day offset veto. Book
// transfers preserve the amount exactly, so the tolerance only absorbs float
// scanning noise, never fees.
const offsetVetoEps = 0.01

// offsetLeg is one deposit/withdrawal-kinded cash row in the same-day offset
// probe. vetoKey carries the leg's emitted TransactionExternalID form —
// txID@acct for web rows, the event id for PSN rows — routed to the matching
// feed's veto map on a pair. Legs the classifier already demotes (an
// internal transfer, deep era, non-rail shape) still participate: an
// internal leg's same-day mirror on another own account is genuinely
// internal too (an UEBERTRAG order pdfInternalTransfer reads as internal
// has its receiving leg in the MT940 feed), and
// since a pair always drops on BOTH sides, even a coincidental false pair
// costs only a net-zero same-day pair.
type offsetLeg struct {
	vetoKey string
	psnLeg  bool
	txID    string
	acct    string
	amt     float64
	// day, ccy and the stated counter leg are what the CONVERSION phase
	// needs: it pairs across currencies, so it cannot read them off the
	// per-(day, currency) bucket the other two phases are grouped into.
	day       int64
	ccy       string
	statedCcy string
	statedAmt string
}

// buildSameDayOffsetVeto pairs cash rows that offset each other on the same
// value day — same currency, equal amount (within offsetVetoEps), opposite
// direction, different own account — and returns the emitted-row keys of the
// paired legs per feed (web: txID@acct; PSN: event id) so BOTH transaction
// loops can demote them to a non-flow kind. Demoting both sides is what makes
// a pair — true or false — cost at most a net-zero same-day pair dropped from
// the flow stream; a one-sided demotion would fabricate a phantom external
// flow, the exact failure the veto exists to prevent.
//
// An intra-relationship move recorded without a counter IBAN is
// indistinguishable from an external payment on the row alone (both feeds
// record many shapes with a free-text beneficiary only), but the receiving own
// account books the mirror leg on the same value day. That mirror's membership
// in the own-account universe is the IBAN-free form of the same-relationship
// test, and it works across the feed seams (PDF ↔ MT940 ↔ PSN) where the ID
// schemes differ. The probe covers the EMITTED universe: web rows the
// reconcile table leaves standing (a dropped web row is represented by the
// row that kept its booking and must not consume a match) plus PSN cash
// movements, over the FULL silver rather than the load window — a row's
// classification depends only on silver contents, never on load slicing; a
// mirror leg that lands in a later dump is picked up on the next `reload`.
// The web half reads the pass's own verdict on each row (skip), so a rule
// that drops a web row from the stream drops it here too.
//
// Matching is 1:1 and deterministic: first the currency conversions
// (vetoConversions), then, per value day and currency, every bank-linked
// twin (a shared Transaction no. — UBS stamps both sides of an
// inter-account transfer with one number) and the loose offsets among the
// legs left (pairSameDayOffsets).
func (r *webReader) buildSameDayOffsetVeto(ctx context.Context, psn *psnReader, p *webTxPass) (webVeto, psnVeto map[string]bool, err error) {
	legs, err := r.webOffsetLegs(ctx, p)
	if err != nil {
		return nil, nil, err
	}
	psnLegs, err := psn.offsetLegs(ctx, p.dividends)
	if err != nil {
		return nil, nil, err
	}
	// A conversion mirror stands in for a booking no feed carries. Where
	// the export DOES carry it, the reconcile table withholds the mirror,
	// here and in the PSN stream alike, so the booking is one leg in the
	// veto's universe and one row in gold.
	for _, l := range psnLegs {
		if isMirrorID(l.txID) && p.rc.withheld[l.txID] {
			continue
		}
		legs = append(legs, l)
	}

	webVeto, psnVeto = map[string]bool{}, map[string]bool{}
	record := func(l offsetLeg) {
		if l.psnLeg {
			psnVeto[l.vetoKey] = true
		} else {
			webVeto[l.vetoKey] = true
		}
	}
	consumed := vetoConversions(slices.Clone(legs), record)
	pairSameDayOffsets(legs, consumed, record)
	return webVeto, psnVeto, nil
}

// webOffsetLegs reads the web half of the offset veto's universe: every
// deposit/withdrawal row the transaction pass emits.
func (r *webReader) webOffsetLegs(ctx context.Context, p *webTxPass) ([]offsetLeg, error) {
	var legs []offsetLeg
	err := r.eachWebRow(ctx, func(row *webRow) error {
		// A row the reconcile table drops is not in the ledger: its booking
		// is represented by the row that kept it, and letting the dropped
		// copy stand here would let one booking consume two mirrors.
		if p.skip(row) != webEmitted {
			return nil
		}
		// The amount the row will REACH GOLD with, not the raw column
		// difference: the two web eras write the amount columns to
		// different conventions, and only the adapter's own projection
		// resolves them (the rule `bookingKey` names for the reconcile
		// table).
		// Differencing the raw columns would read every export-era
		// withdrawal as a positive figure, where it could mirror nothing.
		kind, netAmount := row.kind, row.signed
		if kind != canonical.TxKindDeposit && kind != canonical.TxKindWithdrawal || netAmount == nil {
			return nil
		}
		statedCcy, statedAmt := "", ""
		if row.decoded {
			statedCcy, statedAmt = counterLegFromNarrative(row.pl)
		}
		legs = append(legs, offsetLeg{
			vetoKey: row.emittedID(), txID: row.txID, acct: row.account,
			amt: netAmount.InexactFloat64(), day: utcDay(row.valueDate), ccy: row.currency,
			statedCcy: statedCcy, statedAmt: statedAmt,
		})
		return nil
	})
	return legs, err
}

// offsetLegs reads the PSN half of the offset veto's universe: every
// deposit/withdrawal cash movement, as the stream emits it. A reversal of
// a dividend credit is a dividend reversal in the stream
// (creditReversal), so it is not one.
func (r *psnReader) offsetLegs(ctx context.Context, dividends *dividendSecurities) ([]offsetLeg, error) {
	var legs []offsetLeg
	err := r.eachCashMovement(ctx, func(row *feedRow) error {
		// The cash the line moves, as the stream projects it
		// (feedRow.project), a reversal's included.
		tx, p, ok := row.project()
		if !ok || tx.Kind != canonical.TxKindDeposit && tx.Kind != canonical.TxKindWithdrawal {
			return nil
		}
		if _, reversal := dividends.creditReversal(row.eventID); reversal {
			return nil
		}
		amt, _ := tx.NetAmount.Float64()
		// The other leg a converted movement states (`/OCMT/`), for the
		// conversion phase — the fact the statement era writes as
		// `CCY amount Rate`, in the feed's own spelling of it.
		statedCcy, statedAmt := statedConversion(p.Narrative)
		if statedAmt != "" {
			if d, err := parseSwiftDecimal(statedAmt); err == nil {
				statedAmt = d.String()
			} else {
				statedCcy, statedAmt = "", ""
			}
		}
		legs = append(legs, offsetLeg{
			vetoKey: row.eventID, psnLeg: true, txID: row.eventID, acct: tx.AccountExternalID,
			amt: amt, day: utcDay(tx.OccurredAt), ccy: tx.Currency,
			statedCcy: statedCcy, statedAmt: statedAmt,
		})
		return nil
	})
	if err != nil {
		return nil, err
	}
	// The conversion mirrors, as legs of their own: the other side of a
	// stated conversion, where no feed booked it (conversionMirrors).
	mirrors, err := r.conversionMirrors(ctx)
	if err != nil {
		return nil, err
	}
	for _, m := range mirrors.byID {
		amt, _ := m.tx.NetAmount.Float64()
		legs = append(legs, offsetLeg{
			vetoKey: m.tx.TransactionExternalID, psnLeg: true, txID: m.tx.TransactionExternalID,
			acct: m.tx.AccountExternalID, amt: amt, day: utcDay(m.tx.OccurredAt), ccy: m.tx.Currency,
		})
	}
	return legs, nil
}

// pairSameDayOffsets matches the legs the conversion phase left, per
// value day and currency: greedily, first every bank-linked twin, then
// loose equal-and-opposite offsets. The twin phase covers the whole
// bucket before the loose one starts, so a twin-less leg that merely
// sorts earlier can never steal another leg's twin.
func pairSameDayOffsets(legs []offsetLeg, consumed map[string]bool, record func(offsetLeg)) {
	type groupKey struct {
		day int64
		ccy string
	}
	groups := map[groupKey][]offsetLeg{}
	for _, l := range legs {
		if !consumed[l.vetoKey] {
			k := groupKey{day: l.day, ccy: l.ccy}
			groups[k] = append(groups[k], l)
		}
	}
	less := func(s []offsetLeg) func(i, j int) bool {
		return func(i, j int) bool {
			if s[i].acct != s[j].acct {
				return s[i].acct < s[j].acct
			}
			return s[i].txID < s[j].txID
		}
	}
	for _, group := range groups {
		var debits, credits []offsetLeg
		for _, l := range group {
			if l.amt < 0 {
				debits = append(debits, l)
			} else if l.amt > 0 {
				credits = append(credits, l)
			}
		}
		sort.Slice(debits, less(debits))
		sort.Slice(credits, less(credits))
		used := make([]bool, len(credits))
		matched := make([]bool, len(debits))
		for _, twinsOnly := range []bool{true, false} {
			for di, d := range debits {
				if matched[di] {
					continue
				}
				for ci, c := range credits {
					if used[ci] || c.acct == d.acct || math.Abs(d.amt+c.amt) > offsetVetoEps ||
						twinsOnly && c.txID != d.txID {
						continue
					}
					matched[di], used[ci] = true, true
					record(d)
					record(c)
					break
				}
			}
		}
	}
}

// vetoConversions demotes both legs of an own-account move that CONVERTED
// CURRENCY, and returns the legs it consumed so the phases after it do not
// pair them with anything else.
//
// The two phases after it bucket by (day, currency) and match equal,
// opposite amounts, so neither can see a movement whose two legs are
// denominated differently. And the gate the veto backstops
// (pdfCashIsExternal) reads a statement-era arrival credit as an interbank
// arrival on its booking type alone, because the intra-relationship shapes
// that booking could smuggle in are supposed to be peeled off HERE. A
// conversion between two accounts of one relationship is precisely such a
// shape. Left unpaired, the paying leg is demoted by the gate's conservative
// default while its receiving twin is promoted to external, which is the
// one-sided demotion the whole mechanism exists to prevent — a phantom
// arrival of owner capital, counted as return.
//
// The link it reads is the bank's own: on one of the two rows the statement
// writes what the other row holds, its currency and its figure
// (counterLegFromNarrative). A description is weaker than the shared
// transaction number the twin phase uses, so it is guarded harder — the
// described leg must be the ONLY leg of the day answering to it, and the only
// one so described. Anything else demotes NOTHING, because a demotion is only
// safe in pairs.
func vetoConversions(all []offsetLeg, record func(offsetLeg)) map[string]bool {
	type ownKey struct {
		day int64
		ccy string
		amt string
	}
	key := func(day int64, ccy string, amt float64) ownKey {
		return ownKey{day, ccy, strconv.FormatFloat(math.Abs(amt), 'f', 2, 64)}
	}
	// Ordered before anything is read. The caller flattens a map, and a map
	// is not an order; the phases after this one tolerate that because they
	// act within a bucket, but this one reaches across buckets and would
	// otherwise let two equally eligible legs resolve differently per run.
	sort.Slice(all, func(i, j int) bool {
		a, b := all[i], all[j]
		if a.day != b.day {
			return a.day < b.day
		}
		if a.acct != b.acct {
			return a.acct < b.acct
		}
		return a.vetoKey < b.vetoKey
	})

	owners, described := map[ownKey]int{}, map[ownKey]int{}
	at := map[ownKey]int{}
	stated := func(l offsetLeg) (ownKey, bool) {
		if l.statedCcy == "" || l.statedAmt == "" {
			return ownKey{}, false
		}
		amt, err := strconv.ParseFloat(l.statedAmt, 64)
		if err != nil || amt == 0 {
			return ownKey{}, false
		}
		return key(l.day, l.statedCcy, amt), true
	}
	for i, l := range all {
		k := key(l.day, l.ccy, l.amt)
		owners[k]++
		at[k] = i
		if sk, ok := stated(l); ok {
			described[sk]++
		}
	}

	consumed := map[string]bool{}
	for _, l := range all {
		if consumed[l.vetoKey] || l.amt == 0 {
			continue
		}
		sk, ok := stated(l)
		if !ok || owners[sk] != 1 || described[sk] != 1 {
			continue
		}
		other := all[at[sk]]
		// A movement crosses two accounts and nets out across them. Same
		// account, same sign, or a leg already spoken for is not one.
		if other.acct == l.acct || consumed[other.vetoKey] || other.amt == 0 ||
			(other.amt < 0) == (l.amt < 0) {
			continue
		}
		consumed[l.vetoKey], consumed[other.vetoKey] = true, true
		record(l)
		record(other)
	}
	return consumed
}
