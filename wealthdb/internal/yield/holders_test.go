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

// A trade moves the holding of the account it is booked on where that
// account holds the instrument as it happens, or, for a buy, holds it at
// its first point after; a trade of any other account than a cash
// account moves its own too. A cash account's trade moves the one other
// account of its portfolio that holds the instrument then, under that
// account's id or the cash account's; with two such accounts, or none,
// its own.
func TestATradesKey(t *testing.T) {
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
	}{
		{"a custody account's own trade", UnitKey{"src", "DEPOT", "QD"},
			map[UnitKey][]UnitPoint{{"src", "SAFE", "RAW"}: held}, UnitTrade{At: 20, Qty: 5},
			UnitKey{"src", "DEPOT", "QD"}},
		{"a cash account's trade one sibling holds", cash,
			map[UnitKey][]UnitPoint{{"src", "SAFE", "RAW"}: held}, UnitTrade{At: 20, Qty: -5},
			UnitKey{"src", "SAFE", "RAW"}},
		{"a sibling that holds it under the cash account's id", cash,
			map[UnitKey][]UnitPoint{{"src", "DEPOT", "QX"}: held}, UnitTrade{At: 20, Qty: -5},
			UnitKey{"src", "DEPOT", "QX"}},
		{"two siblings that hold it", cash,
			map[UnitKey][]UnitPoint{{"src", "DEPOT", "QD"}: held, {"src", "SAFE", "RAW"}: held},
			UnitTrade{At: 20, Qty: -5}, cash},
		{"no sibling that holds it", cash,
			map[UnitKey][]UnitPoint{{"src", "OTHER", "RAW"}: held}, UnitTrade{At: 20, Qty: -5}, cash},
		{"a buy the cash account holds after", cash,
			map[UnitKey][]UnitPoint{cash: {{At: 30, Qty: q(5)}}, {"src", "SAFE", "RAW"}: held},
			UnitTrade{At: 20, Qty: 5}, cash},
		{"a sale the cash account holds only after", cash,
			map[UnitKey][]UnitPoint{cash: {{At: 30, Qty: q(5)}}, {"src", "SAFE", "RAW"}: held},
			UnitTrade{At: 20, Qty: -5}, UnitKey{"src", "SAFE", "RAW"}},
	} {
		h := &Holders{In: holdersInputs(), Points: c.points}
		if got := h.TradeKey(c.key, "RAW", c.trade); got != c.want {
			t.Errorf("%s: %+v, want %+v", c.what, got, c.want)
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
