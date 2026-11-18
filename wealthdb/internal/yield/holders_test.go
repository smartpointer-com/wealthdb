package yield

import (
	"slices"
	"testing"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
)

// holdersInputs are a source of three accounts of one portfolio, a cash
// account beside two custody accounts, and a custody account of another
// portfolio. The cash account folds the id RAW into QX, the first
// custody account into QD. Invented.
func holdersInputs() *Inputs {
	cash := string(canonical.AccountKindCash)
	return &Inputs{Accounts: map[AccountKey]Account{
		{Source: "src", Account: "CASH"}:  {Kind: cash, Portfolio: "P"},
		{Source: "src", Account: "DEPOT"}: {Kind: "brokerage", Portfolio: "P"},
		{Source: "src", Account: "SAFE"}:  {Kind: "brokerage", Portfolio: "P"},
		{Source: "src", Account: "OTHER"}: {Kind: "brokerage", Portfolio: "Q"},
	}, Aliases: map[AliasKey]string{{"src", "CASH", "RAW"}: "QX", {"src", "DEPOT", "RAW"}: "QD"}}
}

// A line belongs to the account that holds its instrument as it
// happens: the account it is booked on, else the one other account of
// its portfolio, under that account's id or the booking account's, else
// the one account of its source. A line that moves no units stays with
// the account it is booked on where that account held the instrument
// before it. A buy also reads the first point after it. With two such
// accounts, or none, it stays with its booking key.
func TestALinesHolder(t *testing.T) {
	t.Parallel()
	q := func(x float64) *float64 { return &x }
	held := []UnitPoint{{At: 10, Qty: q(100)}}
	cash := UnitKey{"src", "CASH", "QX"}
	for _, c := range []struct {
		what   string
		key    UnitKey
		points map[UnitKey][]UnitPoint
		trade  UnitTrade
		want   UnitKey
		basis  Placing
	}{
		{"its own", UnitKey{"src", "DEPOT", "QD"},
			map[UnitKey][]UnitPoint{{"src", "DEPOT", "QD"}: held, {"src", "SAFE", "RAW"}: held}, UnitTrade{At: 20, Qty: -5},
			UnitKey{"src", "DEPOT", "QD"}, PlacedOwn},
		{"a custody account's trade one sibling holds", UnitKey{"src", "DEPOT", "QD"},
			map[UnitKey][]UnitPoint{{"src", "SAFE", "RAW"}: held}, UnitTrade{At: 20, Qty: 5},
			UnitKey{"src", "SAFE", "RAW"}, PlacedPortfolio},
		{"a cash account's trade one sibling holds", cash,
			map[UnitKey][]UnitPoint{{"src", "SAFE", "RAW"}: held}, UnitTrade{At: 20, Qty: -5},
			UnitKey{"src", "SAFE", "RAW"}, PlacedPortfolio},
		{"a sibling that holds it under the cash account's id", cash,
			map[UnitKey][]UnitPoint{{"src", "DEPOT", "QX"}: held}, UnitTrade{At: 20, Qty: -5},
			UnitKey{"src", "DEPOT", "QX"}, PlacedPortfolio},
		{"two siblings that hold it", cash,
			map[UnitKey][]UnitPoint{{"src", "DEPOT", "QD"}: held, {"src", "SAFE", "RAW"}: held},
			UnitTrade{At: 20, Qty: -5}, cash, PlacedNone},
		{"the one account of its source", cash,
			map[UnitKey][]UnitPoint{{"src", "OTHER", "QX"}: held}, UnitTrade{At: 20}, UnitKey{"src", "OTHER", "QX"},
			PlacedSource},
		{"an account of its source under another id", cash,
			map[UnitKey][]UnitPoint{{"src", "OTHER", "RAW"}: held}, UnitTrade{At: 20}, cash, PlacedNone},
		{"a buy the cash account holds after", cash,
			map[UnitKey][]UnitPoint{cash: {{At: 30, Qty: q(5)}}, {"src", "SAFE", "RAW"}: held},
			UnitTrade{At: 20, Qty: 5}, cash, PlacedOwn},
		{"a sale the cash account holds only after", cash,
			map[UnitKey][]UnitPoint{cash: {{At: 30, Qty: q(5)}}, {"src", "SAFE", "RAW"}: held},
			UnitTrade{At: 20, Qty: -5}, UnitKey{"src", "SAFE", "RAW"}, PlacedPortfolio},
		{"a payment before the account holds it", cash,
			map[UnitKey][]UnitPoint{cash: {{At: 30, Qty: q(5)}}}, UnitTrade{At: 20}, cash, PlacedNone},
		{"a payment after it moved to a sibling", cash,
			map[UnitKey][]UnitPoint{{"src", "DEPOT", "QD"}: {{At: 10, Qty: q(100)}, {At: 15, Qty: q(0)}},
				{"src", "SAFE", "RAW"}: {{At: 10, Qty: q(0)}, {At: 15, Qty: q(100)}}},
			UnitTrade{At: 20}, UnitKey{"src", "SAFE", "RAW"}, PlacedPortfolio},
		{"a payment after its own account's holding moved on",
			UnitKey{"src", "DEPOT", "QD"},
			map[UnitKey][]UnitPoint{{"src", "DEPOT", "QD"}: {{At: 10, Qty: q(100)}, {At: 15, Qty: q(0)}},
				{"src", "SAFE", "RAW"}: {{At: 10, Qty: q(0)}, {At: 15, Qty: q(100)}}},
			UnitTrade{At: 20}, UnitKey{"src", "DEPOT", "QD"}, PlacedOwn},
		{"a sale after its own account's holding moved on",
			UnitKey{"src", "DEPOT", "QD"},
			map[UnitKey][]UnitPoint{{"src", "DEPOT", "QD"}: {{At: 10, Qty: q(100)}, {At: 15, Qty: q(0)}},
				{"src", "SAFE", "RAW"}: {{At: 10, Qty: q(0)}, {At: 15, Qty: q(100)}}},
			UnitTrade{At: 20, Qty: -5}, UnitKey{"src", "SAFE", "RAW"}, PlacedPortfolio},
		{"a payment before its own account held it", UnitKey{"src", "DEPOT", "QD"},
			map[UnitKey][]UnitPoint{{"src", "DEPOT", "QD"}: {{At: 30, Qty: q(100)}}, {"src", "SAFE", "RAW"}: held},
			UnitTrade{At: 20}, UnitKey{"src", "SAFE", "RAW"}, PlacedPortfolio},
		{"a payment before it moved", cash,
			map[UnitKey][]UnitPoint{{"src", "DEPOT", "QD"}: {{At: 10, Qty: q(100)}, {At: 25, Qty: q(0)}},
				{"src", "SAFE", "RAW"}: {{At: 10, Qty: q(0)}, {At: 25, Qty: q(100)}}},
			UnitTrade{At: 20}, UnitKey{"src", "DEPOT", "QD"}, PlacedPortfolio},
	} {
		accounts := map[InstrumentKey][]string{}
		for k := range c.points {
			ik := InstrumentKey{Source: k.Source, Instrument: k.Instrument}
			accounts[ik] = append(accounts[ik], k.Account)
		}
		h := &Holders{In: holdersInputs(), Points: c.points, Accounts: accounts}
		if got, basis := h.Place(c.key, "RAW", c.trade); got != c.want || basis != c.basis {
			t.Errorf("%s: %+v %s, want %+v %s", c.what, got, basis, c.want, c.basis)
		}
	}
}

// A payment reads its account's own key where the account held the
// instrument, else the keys of its portfolio's accounts that did, else
// the key of the one account of its source that did.
func TestAPaymentsKeys(t *testing.T) {
	t.Parallel()
	q := func(x float64) *float64 { return &x }
	held := []UnitPoint{{At: 10, Qty: q(100)}}
	in := holdersInputs()
	for _, c := range []struct {
		what   string
		acct   string
		points map[UnitKey][]UnitPoint
		accts  []string
		want   []UnitKey
	}{
		{"its own", "DEPOT", map[UnitKey][]UnitPoint{{"src", "DEPOT", "QD"}: held, {"src", "SAFE", "RAW"}: held},
			[]string{"DEPOT", "SAFE"}, []UnitKey{{"src", "DEPOT", "QD"}}},
		{"its portfolio's", "CASH", map[UnitKey][]UnitPoint{{"src", "DEPOT", "QD"}: held, {"src", "SAFE", "RAW"}: held},
			[]string{"DEPOT", "SAFE"}, []UnitKey{{"src", "DEPOT", "QD"}, {"src", "SAFE", "RAW"}}},
		{"the one account of its source", "OTHER", map[UnitKey][]UnitPoint{{"src", "SAFE", "RAW"}: held},
			[]string{"SAFE"}, []UnitKey{{"src", "SAFE", "RAW"}}},
		{"one of two accounts of its source", "OTHER",
			map[UnitKey][]UnitPoint{{"src", "DEPOT", "RAW"}: held, {"src", "SAFE", "RAW"}: held},
			[]string{"DEPOT", "SAFE"}, nil},
	} {
		h := &Holders{In: in, Points: c.points,
			Accounts: map[InstrumentKey][]string{{Source: "src", Instrument: "RAW"}: c.accts}}
		if got := h.KeysOf(in.PortfolioKeys("src", c.acct, "RAW")); !slices.Equal(got, c.want) {
			t.Errorf("%s: %+v, want %+v", c.what, got, c.want)
		}
	}
}
