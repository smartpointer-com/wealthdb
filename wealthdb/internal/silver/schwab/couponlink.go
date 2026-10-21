package schwab

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"time"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
)

// apiMaturity is a bond's maturity as the API stamps it, nil where it
// states none.
func apiMaturity(i schwabInstrument) *time.Time {
	if len(i.MaturityDate) < 10 {
		return nil
	}
	d, err := time.Parse(time.DateOnly, i.MaturityDate[:10])
	if err != nil {
		return nil
	}
	return &d
}

// apiCoupon is a bond's coupon as the API states it, nil where it states
// none: a variableRate of 0 states none, as the collector reads it.
func apiCoupon(i schwabInstrument) *float64 {
	if i.VariableRate == nil || *i.VariableRate == 0 {
		return nil
	}
	return i.VariableRate
}

// bondTermsPayload is a bond instrument's payload: the maturity and the
// coupon the API states, under the keys the yield pass reads a payload's
// terms by (canonical.PayloadTermsKeys). nil for any other instrument,
// and for a bond that states neither.
func bondTermsPayload(vehicle canonical.Vehicle, i schwabInstrument) json.RawMessage {
	if vehicle != canonical.VehicleBond {
		return nil
	}
	terms := map[string]string{}
	if d := apiMaturity(i); d != nil {
		terms[canonical.PayloadMaturityDate] = d.Format(time.DateOnly)
	}
	if c := apiCoupon(i); c != nil {
		terms[canonical.PayloadInterestRate] = strconv.FormatFloat(*c, 'f', -1, 64)
	}
	if len(terms) == 0 {
		return nil
	}
	raw, _ := json.Marshal(terms)
	return raw
}

// heldBond is a bond an account's positions name: its instrument key and
// the coupon and maturity its terms state.
type heldBond struct {
	key      string
	coupon   *float64
	maturity *time.Time
}

// readHeldBonds reads, per account, every bond its positions name at any
// snapshot, the newest payload of each: a bond's last coupon comes with
// its maturity, when it has left the positions. The maturity and the
// coupon are the API's, else its description's.
func (c *apiReader) readHeldBonds(ctx context.Context) (map[string][]heldBond, error) {
	rows, err := c.db.QueryContext(ctx, `SELECT account_external_id, payload FROM positions
	    ORDER BY snapshot_at DESC, account_external_id, instrument_key`)
	if err != nil {
		return nil, fmt.Errorf("schwab held bonds: %w", err)
	}
	defer rows.Close()
	out := map[string][]heldBond{}
	seen := map[[2]string]bool{}
	for rows.Next() {
		var acct, payload string
		if err := rows.Scan(&acct, &payload); err != nil {
			return nil, err
		}
		var p struct {
			Instrument schwabInstrument `json:"instrument"`
		}
		if json.Unmarshal([]byte(payload), &p) != nil || isCashAssetType(p.Instrument.AssetType) {
			continue
		}
		key := preferredInstrumentKey(p.Instrument)
		if key == "" || seen[[2]string{acct, key}] {
			continue
		}
		seen[[2]string{acct, key}] = true
		terms := canonical.ReadBondTerms(p.Instrument.Description)
		coupon, maturity := apiCoupon(p.Instrument), apiMaturity(p.Instrument)
		if coupon == nil {
			coupon = terms.Coupon
		}
		if maturity == nil {
			maturity = terms.Maturity
		}
		if coupon != nil && maturity != nil {
			out[acct] = append(out[acct], heldBond{key: key, coupon: coupon, maturity: maturity})
		}
	}
	return out, rows.Err()
}

// bondPaying is the key of the one bond of bonds whose coupon an interest
// line's description names (canonical.CouponLine.OnePaying); false where
// the line names no bond, or none of bonds or more than one pays it.
func bondPaying(bonds []heldBond, desc string) (string, bool) {
	line, ok := canonical.ReadCouponLine(desc)
	if !ok {
		return "", false
	}
	return line.OnePaying(len(bonds), func(i int) (string, *float64, *time.Time) {
		return bonds[i].key, bonds[i].coupon, bonds[i].maturity
	})
}
