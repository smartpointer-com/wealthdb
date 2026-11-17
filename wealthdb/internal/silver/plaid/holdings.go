package plaid

import (
	"cmp"
	"database/sql"
	"encoding/json"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/silver"
)

// holding is one row of silver's `holdings` table.
type holding struct {
	accountID, securityID       string
	quantity, value, costBasis  sql.NullString
	price                       sql.NullString
	vestedQuantity, vestedValue sql.NullString
	currency, payload           string
	lots                        []taxLot
}

// owned is the part of a holding its holder owns, and the quantity not yet
// vested. Plaid states a vested quantity for equity compensation. Where it
// is below the whole, the position is the vested quantity at the vested
// value. A vested value Plaid leaves empty is the price times the vested
// quantity, else the whole value pro rata. Plaid states the cost of the
// whole holding only, so the vested part's cost is that cost pro rata.
func (h holding) owned() (quantity, value, cost, unvested *canonical.Decimal) {
	quantity, value = silver.DecimalPtrOrNil(h.quantity), silver.DecimalPtrOrNil(h.value)
	cost = silver.DecimalPtrOrNil(h.costBasis)
	vested := silver.DecimalPtrOrNil(h.vestedQuantity)
	if quantity == nil || vested == nil || vested.IsNegative() || !vested.LessThan(*quantity) {
		return quantity, value, cost, nil
	}
	rest := quantity.Sub(*vested)
	vestedValue := silver.DecimalPtrOrNil(h.vestedValue)
	if price := silver.DecimalPtrOrNil(h.price); vestedValue == nil && price != nil {
		v := price.Mul(*vested)
		vestedValue = &v
	}
	if vestedValue == nil && value != nil {
		v := value.Mul(*vested).Div(*quantity)
		vestedValue = &v
	}
	if cost != nil {
		c := cost.Mul(*vested).Div(*quantity)
		cost = &c
	}
	return vested, vestedValue, cost, &rest
}

// position is the running sum of the holdings of one instrument in one
// account. Two holdings land on one instrument when Plaid lists a security
// twice, or two securities share an identifier.
type position struct {
	accountID, key, currency             string
	sec                                  security
	quantity, value, costBasis, unvested *canonical.Decimal
	costKnown, lotsKnown                 bool
	lots                                 []taxLot
	payloads                             []json.RawMessage
}

// appendHoldings adds the positions of the run's investment accounts, and
// one CURRENT balance per account and currency for the cash among them. A
// position holds the vested part of its holdings (holding.owned), and the
// tax lots they state where those add up to it (positionLots, lotsAddUp);
// it counts in unsummed the positions whose lots do not. It returns the
// cash keys it stated.
func appendHoldings(b *canonical.SnapshotBatch, t int64, accounts map[string]account,
	holdings []holding, secs map[string]security, unsummed *int) []cashKey {
	positions := map[[2]string]*position{}
	var order [][2]string
	cash := map[cashKey]canonical.Decimal{}
	var cashOrder []cashKey
	for _, h := range holdings {
		a, ok := accounts[h.accountID]
		if !ok || !investmentLedgerKind(a.kind) {
			continue
		}
		sec := secs[h.securityID]
		if sec.id == "" {
			sec = security{id: h.securityID}
		}
		currency := cmp.Or(h.currency, sec.currency, a.currency)
		if currency == "" {
			continue
		}
		if isCash(sec, currency) {
			value := silver.DecimalPtrOrNil(h.value)
			if value == nil {
				value = silver.DecimalPtrOrNil(h.quantity)
			}
			if value == nil {
				continue
			}
			k := cashKey{a.id, currency}
			if _, seen := cash[k]; !seen {
				cashOrder = append(cashOrder, k)
			}
			cash[k] = cash[k].Add(*value)
			continue
		}
		k := [2]string{a.id, instrumentKey(sec)}
		p, ok := positions[k]
		if !ok {
			p = &position{accountID: a.id, key: k[1], currency: currency, sec: sec,
				costKnown: true, lotsKnown: true}
			positions[k] = p
			order = append(order, k)
		}
		quantity, value, cost, unvested := h.owned()
		p.quantity = addPtr(p.quantity, quantity)
		p.value = addPtr(p.value, value)
		p.unvested = addPtr(p.unvested, unvested)
		p.costKnown = p.costKnown && cost != nil
		p.costBasis = addPtr(p.costBasis, cost)
		p.lotsKnown = p.lotsKnown && len(h.lots) > 0
		p.lots = append(p.lots, h.lots...)
		p.payloads = append(p.payloads, json.RawMessage(h.payload))
	}

	seenInstrument := map[string]bool{}
	for _, k := range order {
		p := positions[k]
		ac, veh, known := pairFor(p.sec)
		extra := map[string]any{}
		if !known {
			extra["source_type"] = p.sec.typ
		}
		if p.unvested != nil {
			extra["unvested_quantity"] = p.unvested.String()
		}
		payload := p.payloads[0]
		if len(p.payloads) > 1 {
			extra["holdings"] = p.payloads
			payload = json.RawMessage(`{}`)
		}
		var book *canonical.Decimal
		if p.costKnown {
			book = p.costBasis
		}
		key := p.key
		pos := canonical.PositionChange{
			SnapshotAt:           t,
			AccountExternalID:    p.accountID,
			PositionKey:          key,
			InstrumentExternalID: &key,
			AssetClass:           ac,
			Vehicle:              veh,
			Currency:             p.currency,
			Quantity:             p.quantity,
			MarketValue:          p.value,
			Payload:              silver.PayloadWith(string(payload), extra),
		}
		pos.SetBookValue(book, holdingBasis)
		// Plaid states the lots of the whole holding, unvested shares
		// included, and does not say which lots have vested. A position
		// that holds the vested part at a cost pro rata would not be the
		// sum of those lots, so it carries none. Nor does one whose
		// holdings do not all state their lots, or whose lots do not add
		// up to it.
		if p.lotsKnown && p.unvested == nil {
			if lots := positionLots(pos, p.lots); lotsAddUp(lots, pos) {
				pos.AcquisitionDate = canonical.EarliestLotDate(lots)
				b.PositionLots = append(b.PositionLots, lots...)
			} else {
				*unsummed++
			}
		}
		b.Positions = append(b.Positions, pos)
		if !seenInstrument[key] {
			seenInstrument[key] = true
			b.Instruments = append(b.Instruments, instrumentChange(p.sec, key, t, t))
		}
	}
	for _, k := range cashOrder {
		b.CashBalances = append(b.CashBalances, silver.CashBalance(t, k.account, k.currency,
			canonical.BalanceKindCurrent, cash[k], `{"basis":"holdings"}`))
	}
	return cashOrder
}

// instrumentChange is the gold instrument of a security, seen over
// [first, last].
func instrumentChange(s security, key string, first, last int64) canonical.InstrumentChange {
	ac, veh, known := pairFor(s)
	extra := map[string]any{}
	if !known {
		extra["source_type"] = s.typ
	}
	payload := s.payload
	if payload == "" {
		payload = "{}"
	}
	return canonical.InstrumentChange{
		InstrumentExternalID: key,
		AssetClass:           ac,
		Vehicle:              veh,
		ISIN:                 silver.StrPtrIfNonEmpty(s.isin),
		CUSIP:                silver.StrPtrIfNonEmpty(s.cusip),
		Symbol:               silver.StrPtrIfNonEmpty(s.ticker),
		Name:                 silver.StrPtrIfNonEmpty(s.name),
		Currency:             silver.StrPtrIfNonEmpty(s.currency),
		FirstSeenAt:          first,
		LastSeenAt:           last,
		Payload:              silver.PayloadWith(payload, extra),
	}
}

// addPtr adds two optional values; the sum is absent only when both are.
func addPtr(a, b *canonical.Decimal) *canonical.Decimal {
	switch {
	case a == nil:
		return b
	case b == nil:
		return a
	}
	sum := a.Add(*b)
	return &sum
}
