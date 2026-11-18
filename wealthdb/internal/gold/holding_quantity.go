package gold

import (
	"context"
	"fmt"
	"math"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/lots"
)

// How the returns netter (pairInKind) and the lot engine
// (lotFeed.pairTransfers) pair the two legs of a move of a holding they find
// by themselves, a move no stored link states. Both read the same holding
// (holdingUnion), the same window in calendar days (inKindWindowDay) and the
// same quantity rule (sameQuantity), the closest quantity first
// (pairByQuantity), so the two pair the same legs.

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
// fee, so it pairs within the in-kind tolerance. A share of a fund that holds
// coins (class crypto, vehicle etf or fund) is a security, and so is a class
// or vehicle gold does not state.
func exactQuantity(class, vehicle string) bool {
	return class != string(canonical.AssetClassCrypto) || vehicle != string(canonical.VehiclePhysical)
}

// The in-kind pairing rule: two legs pair within 7 calendar days of each
// other (lots.PairWindow, counted in days, not hours), in the same quantity
// (sameQuantity). For a coin that is
// within 5% of the larger (lots.PairTolerance): a network fee can take the
// rest. A security pairs only on the exact quantity, up to float noise
// (exactTolerance).
const (
	inKindWindowDay = lots.PairWindow / SecondsPerDay
	inKindTolerance = lots.PairTolerance
	exactTolerance  = 1e-9
)

// excessIsTransfer says that where a linked pair sent a and received b of
// one holding, what the larger side holds beyond the other is a transfer of
// its own, unpaired, rather than a fee: always for a security, which never
// arrives short, and for a coin short beyond the tolerance. A coin's
// shortfall within it is the network's fee.
func excessIsTransfer(a, b float64, exact bool) bool { return !sameQuantity(a, b, exact) }

// sameQuantity says two quantities of one holding are the same: exactly,
// up to float noise, where exact (exactQuantity), else within
// inKindTolerance of the larger.
func sameQuantity(a, b float64, exact bool) bool {
	tol := inKindTolerance
	if exact {
		tol = exactTolerance
	}
	return math.Abs(a-b) <= tol*math.Max(a, b)
}

// moveLegs are the legs of moves of holdings that pairByQuantity pairs,
// sorted by holding, then time.
type moveLegs interface {
	// holding names leg i's holding: only legs of one holding pair.
	holding(i int) string
	// at is when leg i happened, in the unit of the pairing window.
	at(i int) int64
	// in says leg i receives the holding; else it sends it.
	in(i int) bool
	// qty is the quantity leg i moved, unsigned.
	qty(i int) float64
	// exact says leg i pairs only on the exact quantity (exactQuantity).
	exact(i int) bool
	// may says legs i and j may pair, beyond the quantity rule.
	may(i, j int) bool
}

// pairByQuantity pairs the legs of moves by holding and quantity, as
// returns (pairInKind) and the lot engine (lotFeed.pairTransfers) pair
// them. Each leg no earlier pair claimed, in order, takes the earlier leg
// of the other direction still open within window of it whose quantity is
// closest, the latest on a tie, where the two may pair in the same
// quantity (sameQuantity). A leg that takes none stays open. mate holds
// each leg's partner, -1 for none; a leg it already pairs is left alone.
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
		best, bestDiff := -1, math.Inf(1)
		for x := len(q) - 1; x >= 0; x-- {
			j := q[x]
			if !legs.may(i, j) || !sameQuantity(legs.qty(i), legs.qty(j), legs.exact(i) || legs.exact(j)) {
				continue
			}
			if diff := math.Abs(legs.qty(i) - legs.qty(j)); diff < bestDiff {
				best, bestDiff = j, diff
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
