package lots

import (
	"fmt"
	"math"
	"slices"
	"strings"
)

// Method is the order a book's lots are relieved in.
type Method uint8

const (
	// FIFO relieves the earliest acquisition first.
	FIFO Method = iota
	// LIFO relieves the latest acquisition first.
	LIFO
	// HIFO relieves the highest unit cost first.
	HIFO
	// LOFO relieves the lowest unit cost first.
	LOFO
	// Average keeps two pools, the costed and the uncosted lots; a
	// disposal takes from both pro rata by quantity, at each pool's
	// average unit cost.
	Average
)

var methodNames = [...]string{"fifo", "lifo", "hifo", "lofo", "average"}

func (m Method) String() string { return methodNames[m] }

// ParseMethod reads a method's name as the config spells it.
func ParseMethod(s string) (Method, error) { return parseName[Method]("lot method", methodNames[:], s) }

// parseName reads one of names as its index; what names the vocabulary
// in the error.
func parseName[T ~uint8](what string, names []string, s string) (T, error) {
	i, err := lookupName(what, names, s)
	return T(i), err
}

// lookupName is s's index in names.
func lookupName(what string, names []string, s string) (int, error) {
	if i := slices.Index(names, s); i >= 0 {
		return i, nil
	}
	return 0, fmt.Errorf("unknown %s %q (want one of %s)", what, s, strings.Join(names, ", "))
}

// Fees says whether the purchase fees are in a rebuilt cost, in the
// vocabulary of the basis stamp's third part.
type Fees uint8

const (
	FeesUnknown Fees = iota
	FeesIncluded
	FeesExcluded
)

var feesNames = [...]string{"unknown", "included", "excluded"}

func (f Fees) String() string { return feesNames[f] }

// Origin says what opened a lot.
type Origin uint8

const (
	OriginBuy Origin = iota
	OriginTransferIn
	OriginIncome
	// OriginSeed is a lot the engine opened for quantity the history
	// does not explain: a snapshot holding more than the book, or a
	// disposal larger than it.
	OriginSeed
	OriginSplit
	OriginReorg
	// OriginAnchor is a lot a source states at a snapshot, adopted into
	// the book.
	OriginAnchor
	// OriginAdjust is a lot reopened at a lower cost by a return of
	// capital.
	OriginAdjust
)

var originNames = [...]string{"buy", "transfer_in", "income", "seed", "split", "reorg", "anchor", "adjust"}

func (o Origin) String() string { return originNames[o] }

// CostOrigin says where a lot's cost comes from.
type CostOrigin uint8

const (
	// CostNone is a lot whose cost is unknown.
	CostNone CostOrigin = iota
	// CostTrade is the amount a trade paid.
	CostTrade
	// CostCarried is a cost carried from another lot: a move, a reorg,
	// a split.
	CostCarried
	// CostFMV is the market value on the day of receipt.
	CostFMV
	// CostStated is a lot the source states.
	CostStated
	// CostResolved is a seed's cost taken from a lot the source states
	// later: an anchor's lots, or the realized lots of the sale that
	// relieved it.
	CostResolved
)

var costOriginNames = [...]string{"", "trade", "carried", "fmv", "stated", "resolved"}

func (c CostOrigin) String() string { return costOriginNames[c] }

// DisposalKind says why quantity left a lot.
type DisposalKind uint8

const (
	DisposeSell DisposalKind = iota
	// DisposeFee is quantity given up as a fee, at its market value.
	DisposeFee
	// DisposeSpend is quantity spent on goods, at its market value.
	DisposeSpend
	DisposeTender
	DisposeCashInLieu
	// DisposeExpiry is an option that expired: proceeds zero.
	DisposeExpiry
	// DisposeTransferOut is quantity sent where gold sees no receipt.
	DisposeTransferOut
	// DisposeLost is quantity lost or stolen.
	DisposeLost
	// DisposeGift is quantity given away or donated.
	DisposeGift
	// DisposeMove is quantity moved to another key; its lots reopen
	// there with their cost and date.
	DisposeMove
	// DisposeImplied is quantity a snapshot no longer shows and no
	// transaction explains.
	DisposeImplied
	DisposeReorg
	DisposeSplit
	DisposeAdjust
	// DisposeAnchor closes the book a source's stated lots replace.
	DisposeAnchor
	// DisposeMerge closes an average pool that an acquisition reopens
	// with the combined quantity and cost.
	DisposeMerge
)

var disposalNames = [...]string{"sell", "fee", "spend", "tender", "cash_in_lieu", "expiry",
	"transfer_out", "lost", "gift", "move", "implied", "reorg", "split", "adjust", "anchor", "merge"}

func (k DisposalKind) String() string { return disposalNames[k] }

// Realizes reports whether a disposal of this kind is a realization: a
// sale or an exchange for value, which gets a realized lot when its
// proceeds are known.
func (k DisposalKind) Realizes() bool { return k <= DisposeExpiry }

// FindingKind names something the replay, or the feed and writer
// around it, could not take at face value.
type FindingKind uint8

const (
	// FindSeed: a seed lot opened (see OriginSeed).
	FindSeed FindingKind = iota
	// FindImplied: an implied disposal (see DisposeImplied).
	FindImplied
	// FindBlip: quantity an implied disposal took came back within the
	// blip window, so its lots were restored.
	FindBlip
	// FindResolved: a seed took its cost from a stated lot.
	FindResolved
	// FindSkipped: a snapshot was not reconciled because a move touching
	// the key was in flight.
	FindSkipped
	// FindUnpairedTransfer: the feed paired a transfer leg with nothing.
	FindUnpairedTransfer
	// FindFeeUnvalued: the feed found no rate for a fee in a third
	// currency, so the cost or proceeds leave it out.
	FindFeeUnvalued
	// FindWashSaleWindow: a loss the engine realized where the key
	// bought within thirty days either side; no wash sale rule applies.
	FindWashSaleWindow
)

var findingNames = [...]string{"seed", "implied", "blip", "resolved", "skipped",
	"unpaired_transfer", "fee_unvalued", "wash_sale_window"}

func (f FindingKind) String() string { return findingNames[f] }

// NoDay marks an unknown date.
const NoDay int32 = math.MinInt32

// DayOf is the Unix day of a Unix second.
func DayOf(at int64) int32 {
	d := at / 86400
	if at < 0 && at%86400 != 0 {
		d--
	}
	return int32(d)
}

// Tolerance is how far two quantities may differ and still agree:
// max(1e-8, 1e-6 × |q|), so a rounding difference in a snapshot is not a
// seed or an implied disposal.
func Tolerance(q float64) float64 { return math.Max(1e-8, 1e-6*math.Abs(q)) }

// MissingBasis is how a reader counts a cost the ledger, or a source,
// does not know (docs/GAINS.md §8): MissingIgnore leaves a figure that
// needs it blank; MissingZero counts it as 0.
type MissingBasis string

const (
	MissingIgnore MissingBasis = "ignore"
	MissingZero   MissingBasis = "zero"
)

// MissingBasisReadings are both readings, the default first.
var MissingBasisReadings = []MissingBasis{MissingIgnore, MissingZero}

// ParseMissingBasis reads a reading by its name.
func ParseMissingBasis(s string) (MissingBasis, error) {
	names := make([]string, len(MissingBasisReadings))
	for i, r := range MissingBasisReadings {
		names[i] = string(r)
	}
	i, err := lookupName("missing-basis reading", names, s)
	if err != nil {
		return "", err
	}
	return MissingBasisReadings[i], nil
}
