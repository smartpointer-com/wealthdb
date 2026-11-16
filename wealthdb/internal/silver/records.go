package silver

import (
	"encoding/json"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
)

// CashBalance builds one cash balance record. payload is the record's JSON
// payload, typically a fixed `{"basis": …}` naming where the figure came from.
func CashBalance(snap int64, accountID, currency string, kind canonical.BalanceKind,
	amount canonical.Decimal, payload string) canonical.CashBalanceChange {
	return canonical.CashBalanceChange{
		SnapshotAt:        snap,
		AccountExternalID: accountID,
		Currency:          currency,
		BalanceKind:       kind,
		Amount:            amount,
		Payload:           json.RawMessage(payload),
	}
}

// DayEndOffset is the seconds from a day's midnight to its last second,
// where a closing balance that speaks for the whole day is stamped.
const DayEndOffset = 86399

// CustodyAccount is the account record, as of t, of a USD login that holds
// private-market interests: a custody account in the taxable personal
// wrapper whose holder self-directs what it holds. The management inside
// each vehicle is not modelled. Callers add the name or category.
func CustodyAccount(id string, t int64) canonical.AccountChange {
	wrapper := canonical.TaxWrapperTaxablePersonal
	style := canonical.ManagementStyleSelfDirected
	usd := "USD"
	return canonical.AccountChange{
		AccountExternalID: id,
		AccountKind:       canonical.AccountKindCustody,
		BaseCurrency:      &usd,
		TaxWrapper:        &wrapper,
		ManagementStyle:   &style,
		FirstSeenAt:       t,
		LastSeenAt:        t,
	}
}

// PrivateFundTaxonomy is the (exposure, vehicle) pair of a private-market
// offering of the given kind. A single-company SPV and a multi-company
// fund are both private-company ownership, so the exposure is
// private_equity for both; the vehicle carries the packaging: "spv" is
// an SPV, any other kind (an unknown or empty one included) the generic
// fund, which never over-claims a single company. Both pairs satisfy
// canonical.ValidTaxonomyPair.
func PrivateFundTaxonomy(kind string) (canonical.AssetClass, canonical.Vehicle) {
	if kind == "spv" {
		return canonical.AssetClassPrivateEquity, canonical.VehicleSPV
	}
	return canonical.AssetClassPrivateEquity, canonical.VehicleFund
}
