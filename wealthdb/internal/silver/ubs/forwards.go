package ubs

import (
	"strings"
	"time"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
)

// What an open FX forward holds: its two legs (docs/adapters/ubs.md §14,
// docs/EXPOSURE.md §5).
//
// A PSN forward position states both legs at notional: TradeAmt in
// TradeCcyIsoCd and CounterCcyAmt in CounterCcyIsoCd. The leg with the
// positive amount is the one bought; where the two amounts do not carry
// opposite signs, BuySellInd says whether the trade leg is bought (Buy)
// or sold (Sell). Each open contract that is not a spot deal becomes an
// instrument of its own, keyed by its contract id, and its legs become
// that instrument's composition on the position's day: one line, long
// the bought leg and short the sold one, over the mark as net assets. The
// look-through then counts the bought notional in its currency and the
// rest of the mark in the sold one.

// forwardProvider names the forwards' legs as a composition's provider.
const forwardProvider = "ubs-psn"

// forwardAssetType is the legs' line label, which reads as foreign
// exchange (canonical.ExposureLabels).
const forwardAssetType = "fx forward"

// forwardPayload is what the adapter reads of a forward_contracts row.
type forwardPayload struct {
	MrktValueAmt      *canonical.Decimal `json:"MrktValueAmt"`
	MrktValueCcyIsoCd string             `json:"MrktValueCcyIsoCd"`
	PrtflId           string             `json:"PrtflId"`
	DealTpDesc        string             `json:"DealTpDesc"`
	BuySellInd        string             `json:"BuySellInd"`
	TradeAmt          *canonical.Decimal `json:"TradeAmt"`
	TradeCcyIsoCd     string             `json:"TradeCcyIsoCd"`
	CounterCcyAmt     *canonical.Decimal `json:"CounterCcyAmt"`
	CounterCcyIsoCd   string             `json:"CounterCcyIsoCd"`
	MtrtyDt           string             `json:"MtrtyDt"`
	PosDt             string             `json:"PosDt"`
}

// matured says the contract has matured by its position day: PosDt,
// else the snapshot's day. A contract without a readable maturity date
// is not known to have matured.
func (p forwardPayload) matured(snap int64) bool {
	maturity, ok := p.maturity()
	if !ok {
		return false
	}
	day := time.Unix(snap, 0).UTC().Truncate(24 * time.Hour)
	if d, err := time.Parse(time.DateOnly, p.PosDt); err == nil {
		day = d
	}
	return !maturity.After(day)
}

func (p forwardPayload) maturity() (time.Time, bool) {
	t, err := time.Parse(time.DateOnly, p.MtrtyDt)
	return t, err == nil
}

// forwardLeg is one leg of a forward at notional.
type forwardLeg struct {
	ccy    string
	amount canonical.Decimal
}

// legs are the bought and the sold leg, the bought amount above zero and
// the sold one below. ok is false where a leg's currency or amount is
// missing, the two currencies are one, or nothing says which leg is
// bought.
func (p forwardPayload) legs() (bought, sold forwardLeg, ok bool) {
	if p.TradeAmt == nil || p.CounterCcyAmt == nil || p.TradeCcyIsoCd == "" || p.CounterCcyIsoCd == "" ||
		p.TradeCcyIsoCd == p.CounterCcyIsoCd {
		return bought, sold, false
	}
	trade, counter := forwardLeg{p.TradeCcyIsoCd, *p.TradeAmt}, forwardLeg{p.CounterCcyIsoCd, *p.CounterCcyAmt}
	switch {
	case trade.amount.Sign() > 0 && counter.amount.Sign() < 0:
		bought, sold = trade, counter
	case trade.amount.Sign() < 0 && counter.amount.Sign() > 0:
		bought, sold = counter, trade
	case strings.EqualFold(p.BuySellInd, "buy"):
		bought, sold = trade, counter
	case strings.EqualFold(p.BuySellInd, "sell"):
		bought, sold = counter, trade
	default:
		return bought, sold, false
	}
	bought.amount, sold.amount = bought.amount.Abs(), sold.amount.Abs().Neg()
	return bought, sold, !bought.amount.IsZero() && !sold.amount.IsZero()
}

// forwardHolding is an open forward as an instrument and its legs as
// the instrument's composition, on the snapshot snap. ok is false for a
// spot deal, a contract with no maturity date, no mark currency or no
// legs: such a position keeps no instrument and counts at its mark.
func forwardHolding(snap int64, contractID string, p forwardPayload) (canonical.InstrumentChange,
	canonical.FundCompositionChange, canonical.FundLineChange, bool) {
	var (
		inst canonical.InstrumentChange
		comp canonical.FundCompositionChange
		line canonical.FundLineChange
	)
	maturity, dated := p.maturity()
	bought, sold, legs := p.legs()
	if !dated || !legs || p.MrktValueCcyIsoCd == "" || strings.Contains(strings.ToLower(p.DealTpDesc), "spot") {
		return inst, comp, line, false
	}
	name := "FX forward " + bought.ccy + "/" + sold.ccy + " " + maturity.Format(time.DateOnly)
	ccy := p.MrktValueCcyIsoCd
	inst = canonical.InstrumentChange{
		InstrumentExternalID: contractID,
		AssetClass:           canonical.AssetClassForeignExchange,
		Vehicle:              canonical.VehicleForward,
		Name:                 &name,
		Currency:             &ccy,
		FirstSeenAt:          snap,
		LastSeenAt:           snap,
	}
	weight := canonical.NewDecimalFromInt(100)
	comp = canonical.FundCompositionChange{
		FundRef: canonical.ContractKey(contractID), AsOf: snap, Basis: canonical.CompositionHoldings,
		Provider: forwardProvider, SourceKind: "daily_file", Origin: canonical.CompositionParsed,
		FundName: &name, Currency: &ccy, NetAssets: p.MrktValueAmt, LineCount: 1, WeightSum: &weight,
	}
	assetType := forwardAssetType
	line = canonical.FundLineChange{
		FundRef: comp.FundRef, AsOf: snap, Basis: comp.Basis, LineNo: 1,
		// The legs are keyed by the product's name, never by the
		// contract's own key, so the line reads as a leaf.
		SecurityKey: canonical.SecurityKey("", "", "", "", name), Name: &name,
		AssetType: &assetType, Weight: &weight, MarketValue: p.MrktValueAmt, Currency: &ccy,
		LongCurrency: &bought.ccy, LongAmount: &bought.amount,
		ShortCurrency: &sold.ccy, ShortAmount: &sold.amount,
	}
	return inst, comp, line, true
}
