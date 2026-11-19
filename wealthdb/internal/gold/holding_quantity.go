package gold

import (
	"context"
	"database/sql"
	"fmt"
	"math"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/lots"
)

// How the returns netter (pairInKind) and the lot engine
// (lotFeed.pairTransfers) pair the two legs of a move of a holding they find
// by themselves, a move no stored link states. Both read the same holding
// (holdingUnion) and the same rule (oneMove): a security within
// inKindWindowDay calendar days on the exact quantity, a coin by the coin
// rule, which the transfer matcher applies too. The best ranked partner is
// first (pairByQuantity), so the two pair the same legs.

// holdingIdentity are the identities of the holding a leg moved, any of which it
// may share with another leg's: its instrument's ISIN and CUSIP, which read
// the same in every source, so shares moved between two brokers of
// different kinds pair, and its instrument id among the sources of one kind,
// whose ids mean the same thing. "" for one gold does not state.
type holdingIdentity struct{ isin, cusip, id string }

// holdingIdentityOf are the identities of instrument inst of a source of kind
// srcKind, which gold describes as f.
func holdingIdentityOf(srcKind, inst string, f instrumentFacts) holdingIdentity {
	return holdingIdentity{isin: f.isin, cusip: f.cusip, id: acctKey(srcKind, inst)}
}

// tokens are the identities as keys of one space: no ISIN reads as a CUSIP
// or an instrument id.
func (h holdingIdentity) tokens() []string {
	out := []string{acctKey("id", h.id)}
	if h.isin != "" {
		out = append(out, acctKey("isin", h.isin))
	}
	if h.cusip != "" {
		out = append(out, acctKey("cusip", h.cusip))
	}
	return out
}

// holdingUnion finds the holdings legs move: two legs move one holding where
// any identity agrees, the same ISIN, the same CUSIP or the same instrument
// id among the sources of one kind, directly or through other legs. A leg
// whose instrument states an ISIN therefore pairs with one whose instrument
// states only the CUSIP both share, or only the id both share.
type holdingUnion struct{ parent map[string]string }

func newHoldingUnion() *holdingUnion { return &holdingUnion{parent: map[string]string{}} }

func (u *holdingUnion) find(t string) string {
	for u.parent[t] != t {
		u.parent[t] = u.parent[u.parent[t]]
		t = u.parent[t]
	}
	return t
}

// add joins h's identities into one holding. The holding is known by its
// smallest identity, so the key does not follow the order legs are added.
func (u *holdingUnion) add(h holdingIdentity) {
	var root string
	for _, t := range h.tokens() {
		if _, ok := u.parent[t]; !ok {
			u.parent[t] = t
		}
		r := u.find(t)
		switch {
		case root == "":
			root = r
		case r < root:
			u.parent[root], root = r, r
		case r > root:
			u.parent[r] = root
		}
	}
}

// key is the holding h's identities name: the holding of the first of them
// a leg added, else h's own instrument id.
func (u *holdingUnion) key(h holdingIdentity) string {
	ts := h.tokens()
	for _, t := range ts {
		if _, ok := u.parent[t]; ok {
			return u.find(t)
		}
	}
	return ts[0]
}

// exactQuantity says a holding of asset class and vehicle moves only whole:
// a security, a share, an option contract, a unit of a fund or a bond,
// never arrives short, so its legs pair, and fund a debut, only on the exact
// quantity. A coin, class crypto held physical, can arrive short by a network
// fee, so it pairs by the coin rule (coinQuantities). A share of a fund that
// holds coins (class crypto, vehicle etf or fund) is a security, and so is a
// class or vehicle gold does not state.
func exactQuantity(class, vehicle string) bool {
	return class != string(canonical.AssetClassCrypto) || vehicle != string(canonical.VehiclePhysical)
}

// IsCoin says a holding of asset class and vehicle is a coin: one that can
// arrive short by a network fee (exactQuantity).
func IsCoin(class, vehicle string) bool { return !exactQuantity(class, vehicle) }

// The in-kind pairing rule for a security: two legs pair within 7 calendar
// days of each other (lots.PairWindow, counted in days, not hours), only on
// the exact quantity, up to float noise (exactTolerance).
const (
	inKindWindowDay = lots.PairWindow / SecondsPerDay
	exactTolerance  = 1e-9
)

// The coin rule: whether a coin sent and a coin received are one transfer.
// It is one rule wherever two legs of a coin pair: the transfer matcher
// (banded.agree), the returns netter and the lot engine (pairByQuantity,
// quantitiesAgree), and the override ledger's selector (TransferLeg.answers).
//
//   - The legs lie within coinTransferDays calendar days of each other, in
//     either order: venues stamp a transfer's two ends at different times.
//   - The coin received is at most coinSurplusShare more than the coin sent.
//   - The fee, sent less received, is at most the larger of coinFeeShare of
//     the sent quantity and coinFeeUSD's worth at the coin's USD price on
//     the sending leg's day, and always less than coinFeeShareMax of it. A
//     coin with no price that day may lose coinFeeUnpriced of it.
//
// Among several candidates the smallest relative shortfall wins, then the
// nearest day, so two transfers of nearby sizes never pair crosswise.
const (
	coinTransferDays = 2
	coinSurplusShare = 0.001
	coinFeeShare     = 0.001
	// coinFeeUSD bounds a network's fee in USD, whatever the size of the
	// transfer. A network charges per transfer, so on a small transfer
	// its fee is a large share, and a bound in the coin's own unit would
	// need a figure for every coin. A hundred dollars is above what a
	// network charges to move a coin, so a shortfall worth more is not
	// its fee. It is one figure, the same wherever a coin pairs.
	coinFeeUSD      = 100.0
	coinFeeShareMax = 0.25
	coinFeeUnpriced = 0.01
)

// coinQuantities says received of a coin is what a transfer of sent
// delivers (the coin rule, less its time): at most coinSurplusShare more,
// or short by a fee it allows. usd is the coin's USD price on the sending
// leg's day, 0 for none.
func coinQuantities(sent, received, usd float64) bool {
	if !(sent > 0) || !(received > 0) || received > sent*(1+coinSurplusShare) {
		return false
	}
	fee := sent - received
	if fee <= 0 {
		return true
	}
	limit := coinFeeUnpriced * sent
	if usd > 0 {
		limit = math.Max(coinFeeShare*sent, coinFeeUSD/usd)
	}
	return fee <= limit && fee < coinFeeShareMax*sent
}

// coinDays says two legs of a coin on days a and b lie within
// coinTransferDays of each other.
func coinDays(a, b int64) bool { return absDay(a-b) <= coinTransferDays }

// coinShortfall is how far received falls short of sent, or exceeds it,
// as a share of sent: the coin rule's order among candidates.
func coinShortfall(sent, received float64) float64 { return math.Abs(sent-received) / sent }

// CoinUSD is coin's USD price on day: fx_daily's direct rate at or before
// the day, the rate an amount in the coin converts to USD at, 0 where gold
// has none.
func CoinUSD(r *DailyRates, coin string, day int64) float64 {
	if r == nil {
		return 0
	}
	x, ok := r.Direct(coin, "USD", day)
	if !ok || !(x > 0) {
		return 0
	}
	return x
}

// LoadCoinUSD reads the USD rates of coins from fx_daily, for CoinUSD.
// Nil for no coins.
func LoadCoinUSD(ctx context.Context, q interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}, coins []string) (*DailyRates, error) {
	if len(coins) == 0 {
		return nil, nil
	}
	rows, err := q.QueryContext(ctx, `
		SELECT from_ccy, to_ccy, day, rate FROM fx_daily
		 WHERE to_ccy = 'USD' AND list_contains(?, from_ccy)
		 ORDER BY 1, 2, 3`, coins)
	if err != nil {
		return nil, fmt.Errorf("gold: read coin prices: %w", err)
	}
	defer rows.Close()
	r := &DailyRates{pairs: map[[2]string][]dayRate{}}
	for rows.Next() {
		var from, to string
		var d dayRate
		if err := rows.Scan(&from, &to, &d.day, &d.rate); err != nil {
			return nil, fmt.Errorf("gold: read coin prices: %w", err)
		}
		k := [2]string{from, to}
		r.pairs[k] = append(r.pairs[k], d)
	}
	return r, rows.Err()
}

// quantitiesAgree says sent and received of one holding are what one
// transfer of it moves, whatever their days: the exact quantity for a
// security (exact), the coin rule's quantities for a coin, at usd, the
// coin's USD price on the sending leg's day.
func quantitiesAgree(sent, received, usd float64, exact bool) bool {
	if exact {
		return math.Abs(sent-received) <= exactTolerance*math.Max(sent, received)
	}
	return coinQuantities(sent, received, usd)
}

// excessIsTransfer says that where a linked pair sent and received one
// holding in quantities that do not agree (quantitiesAgree), what the
// larger side holds beyond the other is a transfer of its own, unpaired,
// rather than a fee: always for a security, which never arrives short, and
// for a coin beyond the coin rule. A coin's shortfall within it is the
// network's fee.
func excessIsTransfer(sent, received, usd float64, exact bool) bool {
	return !quantitiesAgree(sent, received, usd, exact)
}

// moveLegs are the legs of moves of holdings that pairByQuantity pairs,
// sorted by holding, then time.
type moveLegs interface {
	// holding names leg i's holding: only legs of one holding pair.
	holding(i int) string
	// at is the epoch day leg i happened on.
	at(i int) int64
	// in says leg i receives the holding; else it sends it.
	in(i int) bool
	// qty is the quantity leg i moved, unsigned.
	qty(i int) float64
	// exact says leg i pairs only on the exact quantity (exactQuantity).
	exact(i int) bool
	// usd is the USD price of leg i's coin on its day (CoinUSD), 0 for
	// none and for a security.
	usd(i int) float64
	// may says legs i and j may pair, beyond the quantity rule.
	may(i, j int) bool
}

// oneMove says legs i and j, of opposite directions, are one move of their
// holding, and ranks it among a leg's candidates, the smaller the better: a
// security on the exact quantity, ranked by the quantities' difference, a
// coin by the coin rule, ranked by its shortfall (coinShortfall). The
// caller bounds a security's days by the pairing window.
func oneMove(legs moveLegs, i, j int) (rank float64, ok bool) {
	if legs.exact(i) || legs.exact(j) {
		return math.Abs(legs.qty(i) - legs.qty(j)), quantitiesAgree(legs.qty(i), legs.qty(j), 0, true)
	}
	out, in := i, j
	if legs.in(i) {
		out, in = j, i
	}
	if !coinDays(legs.at(i), legs.at(j)) || !coinQuantities(legs.qty(out), legs.qty(in), legs.usd(out)) {
		return 0, false
	}
	return coinShortfall(legs.qty(out), legs.qty(in)), true
}

// pairByQuantity pairs the legs of moves by holding and quantity, as
// returns (pairInKind) and the lot engine (lotFeed.pairTransfers) pair
// them. Each leg no earlier pair claimed, in order, takes the earlier leg
// of the other direction still open within window days of it that is one
// move with it (oneMove), the best ranked, the latest on a tie. A leg that
// takes none stays open. mate holds each leg's partner, -1 for none; a leg
// it already pairs is left alone.
func pairByQuantity(legs moveLegs, window int64, mate []int) {
	dir := func(i int) int {
		if legs.in(i) {
			return 1
		}
		return 0
	}
	// open lists the earlier legs of the holding still unpaired, per
	// direction (sends, receipts), oldest first.
	var open [2][]int
	for i := range mate {
		if i == 0 || legs.holding(i-1) != legs.holding(i) {
			open[0], open[1] = open[0][:0], open[1][:0]
		}
		if mate[i] >= 0 {
			continue
		}
		other := 1 - dir(i)
		q := open[other][:0]
		for _, j := range open[other] {
			if mate[j] < 0 && legs.at(i)-legs.at(j) <= window {
				q = append(q, j)
			}
		}
		open[other] = q
		best, bestRank := -1, math.Inf(1)
		for x := len(q) - 1; x >= 0; x-- {
			j := q[x]
			if !legs.may(i, j) {
				continue
			}
			if rank, ok := oneMove(legs, i, j); ok && rank < bestRank {
				best, bestRank = j, rank
			}
		}
		if best >= 0 {
			mate[i], mate[best] = best, i
		} else {
			open[dir(i)] = append(open[dir(i)], i)
		}
	}
}

// instrumentFacts is what gold states of an instrument that the in-kind
// pairing of returns and lots reads: its identities across sources, the
// ISIN and the CUSIP, and its asset class and vehicle ("" for none).
type instrumentFacts struct{ isin, cusip, class, vehicle string }

// loadInstrumentFacts reads every instrument's facts, keyed acctKey(source,
// instrument id).
func loadInstrumentFacts(ctx context.Context, q querier) (map[string]instrumentFacts, error) {
	rows, err := q.QueryContext(ctx, `
		SELECT silver_source_id, instrument_external_id, COALESCE(isin, ''), COALESCE(cusip, ''), asset_class,
		       COALESCE(vehicle, '')
		  FROM instruments`)
	if err != nil {
		return nil, fmt.Errorf("instrument facts: %w", err)
	}
	defer rows.Close()
	out := map[string]instrumentFacts{}
	for rows.Next() {
		var src, id string
		var f instrumentFacts
		if err := rows.Scan(&src, &id, &f.isin, &f.cusip, &f.class, &f.vehicle); err != nil {
			return nil, fmt.Errorf("instrument facts scan: %w", err)
		}
		out[acctKey(src, id)] = f
	}
	return out, rows.Err()
}
