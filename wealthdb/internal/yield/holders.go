package yield

import (
	"cmp"
	"slices"
	"sort"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
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

// Holders are what tells which account's holding a line of the inputs
// In moves or is paid on, over time, where HoldingOf tells it at the
// instant: the quantities of the instruments paid in each account that
// held them, at the points they change (Points), and the accounts whose
// points each instrument has, by account (Accounts).
type Holders struct {
	In       *Inputs
	Points   map[UnitKey][]UnitPoint
	Accounts map[InstrumentKey][]string
	// siblings memoises PortfolioKeys for TradeKey.
	siblings map[UnitKey][]UnitKey
}

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

// TradeKey is the key trade t moves the quantity of, booked under key on
// its account, which names its instrument iid: the booking account's,
// where that account holds its instrument as the trade happens (holdsAt).
// Else, for a trade booked on a cash account, the one other account of
// its portfolio that holds it then, its instrument folded under that
// account or under the booking account (PortfolioKeys, key). Where none
// does, or several do, the booking key: which account the trade went to
// is not known, and it moves no holding. A trade of any other account
// moves only its own.
func (h *Holders) TradeKey(key UnitKey, iid string, t UnitTrade) UnitKey {
	if h.holdsAt(key, t) ||
		h.In.Accounts[AccountKey{Source: key.Source, Account: key.Account}].Kind != string(canonical.AccountKindCash) {
		return key
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
	if len(holder) != 1 {
		return key
	}
	return holder[0]
}

// holdsAt says key k holds its instrument as trade t happens: its newest
// point at or before the trade holds some, or, for a buy, the first after
// it does.
func (h *Holders) holdsAt(k UnitKey, t UnitTrade) bool {
	ps := h.Points[k]
	i := sort.Search(len(ps), func(i int) bool { return ps[i].At > t.At })
	return (i > 0 && ps[i-1].Holds()) || (t.Qty > 0 && i < len(ps) && ps[i].Holds())
}
