package canonical

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// Cost basis means different things in different sources: a sum of tax
// lots or a weighted average, with or without the purchase fees, a
// figure the source prints or one an adapter computes from it. A basis
// that answers for some sources and not others is worse than none when
// nothing says which, so every book value in gold carries a stamp: its
// origin, its method and its fee treatment (docs/DESIGN.md §7.4).

// BasisOrigin says where a book value comes from.
type BasisOrigin string

const (
	// BasisStated is one figure the source states, taken as is.
	BasisStated BasisOrigin = "stated"
	// BasisDerived is arithmetic over figures the source states:
	// quantity × average cost, market value − unrealized gain, a sum of
	// lots, a figure converted at the source's own rate.
	BasisDerived BasisOrigin = "derived"
	// BasisRebuilt is a basis a lot engine replayed from trades.
	BasisRebuilt BasisOrigin = "rebuilt"
	// BasisSeeded is an opening basis entered by hand.
	BasisSeeded BasisOrigin = "seeded"
)

var basisOriginValues = map[BasisOrigin]struct{}{
	BasisStated: {}, BasisDerived: {}, BasisRebuilt: {}, BasisSeeded: {},
}

func (o BasisOrigin) Valid() bool {
	_, ok := basisOriginValues[o]
	return ok
}

// BasisMethod says how the source arrives at a holding's basis.
type BasisMethod string

const (
	// BasisMethodLots is the sum of the source's tax lots, whatever
	// method it relieves them by (FIFO, specific identification, an
	// election per account).
	BasisMethodLots BasisMethod = "lots"
	// BasisMethodAverage is a weighted average cost per unit.
	BasisMethodAverage BasisMethod = "average"
	// BasisMethodPaidIn is the capital paid in, gross of any capital
	// paid back: the private-market definition.
	BasisMethodPaidIn BasisMethod = "paid_in"
	// BasisMethodAcquisitionValue is the holding's value on the day it
	// was acquired, where nothing states what was paid.
	BasisMethodAcquisitionValue BasisMethod = "acquisition_value"
	// BasisMethodUnknown is a basis whose source does not say how it
	// was computed (an aggregator passing on the institution's figure).
	BasisMethodUnknown BasisMethod = "unknown"
)

var basisMethodValues = map[BasisMethod]struct{}{
	BasisMethodLots: {}, BasisMethodAverage: {}, BasisMethodPaidIn: {},
	BasisMethodAcquisitionValue: {}, BasisMethodUnknown: {},
}

func (m BasisMethod) Valid() bool {
	_, ok := basisMethodValues[m]
	return ok
}

// BasisFees says whether the purchase fees are in a book value.
type BasisFees string

const (
	BasisFeesIncluded BasisFees = "included"
	BasisFeesExcluded BasisFees = "excluded"
	// BasisFeesNone is a source that charges no purchase fee, so there
	// is nothing to include.
	BasisFeesNone    BasisFees = "none"
	BasisFeesUnknown BasisFees = "unknown"
)

var basisFeesValues = map[BasisFees]struct{}{
	BasisFeesIncluded: {}, BasisFeesExcluded: {}, BasisFeesNone: {}, BasisFeesUnknown: {},
}

func (f BasisFees) Valid() bool {
	_, ok := basisFeesValues[f]
	return ok
}

// Basis is the stamp on a book value. Its zero value is "no basis".
type Basis struct {
	Origin BasisOrigin
	Method BasisMethod
	Fees   BasisFees
}

// IsZero reports whether the stamp is unset.
func (b Basis) IsZero() bool { return b == Basis{} }

// Validate reports a stamp that is not complete and valid.
func (b Basis) Validate() error {
	if !b.Origin.Valid() {
		return fmt.Errorf("invalid basis_origin %q", b.Origin)
	}
	if !b.Method.Valid() {
		return fmt.Errorf("invalid basis_method %q", b.Method)
	}
	if !b.Fees.Valid() {
		return fmt.Errorf("invalid basis_fees %q", b.Fees)
	}
	return nil
}

// ValidateBookValue checks a book value against its stamp: a value
// carries a complete stamp, and no value carries none.
func ValidateBookValue(v *Decimal, b Basis) error {
	if v == nil {
		if !b.IsZero() {
			return fmt.Errorf("basis stamp %+v on a NULL book_value", b)
		}
		return nil
	}
	if err := b.Validate(); err != nil {
		return fmt.Errorf("book_value without a valid stamp: %w", err)
	}
	return nil
}

// SetBookValue sets a position's book value and its stamp together. A
// nil value leaves both unset, so an adapter can pass whatever its
// computation produced.
func (p *PositionChange) SetBookValue(v *Decimal, b Basis) {
	if v == nil {
		p.BookValue, p.Basis = nil, Basis{}
		return
	}
	p.BookValue, p.Basis = v, b
}

// LotTerm is a lot's holding period as the source states it.
type LotTerm string

const (
	LotTermShort LotTerm = "short"
	LotTermLong  LotTerm = "long"
)

func (t LotTerm) Valid() bool { return t == LotTermShort || t == LotTermLong }

// ParseLotTerm reads a source's printed term, "SHORT" or "long" in any
// case or spacing. Any other label states no term: "".
func ParseLotTerm(s string) LotTerm {
	if t := LotTerm(strings.ToLower(strings.TrimSpace(s))); t.Valid() {
		return t
	}
	return ""
}

// ValidateTerm reports a term that is set but not in the vocabulary.
func ValidateTerm(t LotTerm) error {
	if t != "" && !t.Valid() {
		return fmt.Errorf("invalid term %q", t)
	}
	return nil
}

// PositionLotChange is one insert into gold's `position_lots` table: one
// open lot of the position row with the same source, snapshot, account
// and position key.
type PositionLotChange struct {
	SilverSourceID    string
	SnapshotAt        int64
	AccountExternalID string
	PositionKey       string
	// LotKey is unique within the position. Adapters use the source's
	// own lot order where it has one.
	LotKey               string
	InstrumentExternalID *string
	Currency             string
	// Quantity is signed like positions.quantity: a short lot is
	// negative.
	Quantity    *Decimal
	BookValue   *Decimal
	MarketValue *Decimal
	// AcquisitionDate is a calendar date at UTC midnight.
	AcquisitionDate *time.Time
	// Term is "" where the source does not state it.
	Term    LotTerm
	Covered *bool
	// BasisOrigin is set exactly when BookValue is.
	BasisOrigin    BasisOrigin
	SourceDocument *string
	Payload        json.RawMessage
}

// SetBookValue sets a lot's book value and the origin that stamps it
// together. A nil value leaves both unset.
func (l *PositionLotChange) SetBookValue(v *Decimal, origin BasisOrigin) {
	if v == nil {
		l.BookValue, l.BasisOrigin = nil, ""
		return
	}
	l.BookValue, l.BasisOrigin = v, origin
}

// ValidateLotOrigin checks a lot's book value against its origin, the
// lot's form of ValidateBookValue: a value carries a valid origin, and
// no value carries none.
func ValidateLotOrigin(v *Decimal, o BasisOrigin) error {
	if (v == nil) != (o == "") {
		return fmt.Errorf("basis_origin %q does not match book_value", o)
	}
	if o != "" && !o.Valid() {
		return fmt.Errorf("invalid basis_origin %q", o)
	}
	return nil
}

// EarliestLotDate is the earliest acquisition date among lots, nil when
// none states one: the position's acquisition_date where it has lots.
func EarliestLotDate(lots []PositionLotChange) *time.Time {
	var earliest *time.Time
	for i := range lots {
		if d := lots[i].AcquisitionDate; d != nil && (earliest == nil || d.Before(*earliest)) {
			earliest = d
		}
	}
	return earliest
}

// RealizedDocKind names the document a realized lot is read from.
type RealizedDocKind string

const (
	RealizedForm1099B       RealizedDocKind = "form_1099b"
	RealizedYearEndSummary  RealizedDocKind = "year_end_summary"
	RealizedGainLossReport  RealizedDocKind = "gain_loss_report"
	RealizedClosedPositions RealizedDocKind = "closed_positions"
	RealizedStatement       RealizedDocKind = "statement"
	RealizedTrade           RealizedDocKind = "trade"
)

var realizedDocKindValues = map[RealizedDocKind]struct{}{
	RealizedForm1099B: {}, RealizedYearEndSummary: {}, RealizedGainLossReport: {},
	RealizedClosedPositions: {}, RealizedStatement: {}, RealizedTrade: {},
}

func (k RealizedDocKind) Valid() bool {
	_, ok := realizedDocKindValues[k]
	return ok
}

// RealizedLotChange is one insert into gold's `realized_lots` table: one
// realized lot, or one sale where the document prints no lots, as one
// document states it. The same sale stated by several documents is
// several rows; IsPrimary marks the set that counts each sale once per
// account and tax year (see silver.MarkPrimary).
type RealizedLotChange struct {
	SilverSourceID        string
	RealizedLotExternalID string
	AccountExternalID     string
	InstrumentExternalID  *string
	// InstrumentHint is the token the instrument lookup failed on; set
	// only where InstrumentExternalID is nil, like the transaction field
	// of the same name.
	InstrumentHint string
	// Description is the security as the document names it.
	Description  *string
	DocumentKind RealizedDocKind
	TaxYear      int
	// AcquisitionDate is nil where the document prints none, and when
	// it prints "Various" (AcquiredVarious).
	AcquisitionDate *time.Time
	AcquiredVarious bool
	DisposalDate    *time.Time
	SettlementDate  *time.Time
	Currency        string
	// Quantity, Proceeds and BookValue are magnitudes; the gain is
	// signed. Each is nil where the document does not state it.
	Quantity              *Decimal
	Proceeds              *Decimal
	BookValue             *Decimal
	RealizedGainLoss      *Decimal
	WashSaleDisallowed    *Decimal
	AccruedMarketDiscount *Decimal
	Term                  LotTerm
	Covered               *bool
	Form8949Box           *string
	// Basis stamps BookValue, under the same rule as a position's.
	Basis          Basis
	IsPrimary      bool
	SourceDocument *string
	Payload        json.RawMessage
}

// SetBookValue sets a realized lot's book value and its stamp together.
// A nil value leaves both unset.
func (r *RealizedLotChange) SetBookValue(v *Decimal, b Basis) {
	if v == nil {
		r.BookValue, r.Basis = nil, Basis{}
		return
	}
	r.BookValue, r.Basis = v, b
}
