package yield

import (
	"cmp"
	"slices"
	"sort"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/lots"
)

// A UnitKey names an instrument held in one account of a source, by the
// id the account's aliases fold it into (Fold).
type UnitKey struct{ Source, Account, Instrument string }

// A UnitPoint is a key's quantity from a snapshot that speaks for its
// account on, to its next point: nil where the snapshot states none, 0
// where the key is not in it. Prev is the snapshot before it that speaks
// for the account, 0 for none.
type UnitPoint struct {
	At, Prev int64
	Qty      *float64
}

// Holds says the point holds some of its instrument.
func (p UnitPoint) Holds() bool { return p.Qty != nil && *p.Qty > lots.Tolerance(*p.Qty) }

// Holders are what tells which account a line of the inputs In moves
// or is paid on, over time, where HoldingOf tells it at the instant:
// the quantities of the instruments in each account that held them, at
// the points they change (Points), and the accounts whose points each
// instrument has, by account (Accounts).
type Holders struct {
	In       *Inputs
	Points   map[UnitKey][]UnitPoint
	Accounts map[InstrumentKey][]string
	// siblings memoises PortfolioKeys for Place.
	siblings map[UnitKey][]UnitKey
}

// A Placing says which step of Place placed a line.
type Placing string

const (
	// PlacedOwn: the account the line is booked on held its instrument.
	PlacedOwn Placing = "own"
	// PlacedPortfolio: the one other account of its portfolio that did.
	PlacedPortfolio Placing = "portfolio"
	// PlacedSource: the one account of its source that did.
	PlacedSource Placing = "source"
	// PlacedNone: no account, or more than one, did.
	PlacedNone Placing = "none"
)

// PlaceVersion numbers Place's rule. A stored placement of another
// version is read as stale.
const PlaceVersion = 2

// PortfolioKeys are the keys a line of account acct reads the instrument
// iid names under, iid folded under each account (Fold): acct's own, then
// each other account's of its portfolio, by account.
func (in *Inputs) PortfolioKeys(src, acct, iid string) []UnitKey {
	keys := []UnitKey{{src, acct, in.Fold(src, acct, iid)}}
	portfolio := in.Accounts[AccountKey{Source: src, Account: acct}].Portfolio
	if portfolio == "" {
		return keys
	}
	for a, x := range in.Accounts {
		if a.Source == src && a.Account != acct && x.Portfolio == portfolio {
			keys = append(keys, UnitKey{src, a.Account, in.Fold(src, a.Account, iid)})
		}
	}
	slices.SortFunc(keys[1:], func(a, b UnitKey) int { return cmp.Compare(a.Account, b.Account) })
	return keys
}

// KeysOf are the keys a payment reads its quantities under, of all, the
// PortfolioKeys of its account and the instrument it names: its own
// where the account held it, else its portfolio's accounts', else that
// of the one account of its source that held it. A bank books a dividend
// to a cash account beside the custody account, or in another portfolio.
func (h *Holders) KeysOf(all []UnitKey) []UnitKey {
	if _, ok := h.Points[all[0]]; ok {
		return all[:1]
	}
	var keys []UnitKey
	for _, k := range all[1:] {
		if _, ok := h.Points[k]; ok {
			keys = append(keys, k)
		}
	}
	src, own := all[0].Source, all[0].Instrument
	if accts := h.Accounts[InstrumentKey{Source: src, Instrument: own}]; len(keys) == 0 && len(accts) == 1 {
		keys = []UnitKey{{src, accts[0], own}}
	}
	return keys
}

// Place is the key of the holding transaction t belongs to, booked under
// key on its account, which names its instrument iid, and the step that
// placed it: the account that held the instrument as t happened
// (holdsAt). That is the booking account, where it held it (PlacedOwn).
// A line that moves no units stays with its booking account where that
// account held the instrument at any point up to it (heldBy): a
// dividend paid after a holding moved on is paid on what the account
// held on its record day.
// Else the one other account of its portfolio that held it then, its
// instrument folded under that account or under the booking account
// (PortfolioKeys, key; PlacedPortfolio). Else the one account of its
// source that held it under the booking account's id (PlacedSource). A
// bank books a trade or a dividend to a cash account beside the custody
// account, or in another portfolio. Where none held it, or several did,
// the booking key (PlacedNone): which account it went to is not known.
// A trade that brings units (a buy) also reads the first point after it,
// as its units land there; any other line reads only what was held as
// it happened.
func (h *Holders) Place(key UnitKey, iid string, t UnitTrade) (UnitKey, Placing) {
	if h.holdsAt(key, t) || (t.Qty == 0 && h.heldBy(key, t.At)) {
		return key, PlacedOwn
	}
	own := UnitKey{key.Source, key.Account, iid}
	siblings, ok := h.siblings[own]
	if !ok {
		siblings = h.In.PortfolioKeys(key.Source, key.Account, iid)[1:]
		if h.siblings == nil {
			h.siblings = map[UnitKey][]UnitKey{}
		}
		h.siblings[own] = siblings
	}
	var holder []UnitKey
	for _, k := range siblings {
		for _, c := range []UnitKey{k, {k.Source, k.Account, key.Instrument}} {
			if h.holdsAt(c, t) {
				holder = append(holder, c)
				break
			}
		}
	}
	switch len(holder) {
	case 1:
		return holder[0], PlacedPortfolio
	case 0:
		for _, a := range h.Accounts[InstrumentKey{Source: key.Source, Instrument: key.Instrument}] {
			if c := (UnitKey{key.Source, a, key.Instrument}); a != key.Account && h.holdsAt(c, t) {
				holder = append(holder, c)
			}
		}
		if len(holder) == 1 {
			return holder[0], PlacedSource
		}
	}
	return key, PlacedNone
}

// heldBy says key k held some of its instrument at a point at or before
// at.
func (h *Holders) heldBy(k UnitKey, at int64) bool {
	for _, p := range h.Points[k] {
		if p.At > at {
			return false
		}
		if p.Holds() {
			return true
		}
	}
	return false
}

// holdsAt says key k holds its instrument as trade t happens: its newest
// point at or before the trade holds some, or, for a buy, the first after
// it does.
func (h *Holders) holdsAt(k UnitKey, t UnitTrade) bool {
	ps := h.Points[k]
	i := sort.Search(len(ps), func(i int) bool { return ps[i].At > t.At })
	return (i > 0 && ps[i-1].Holds()) || (t.Qty > 0 && i < len(ps) && ps[i].Holds())
}
