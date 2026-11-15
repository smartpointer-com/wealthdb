package fidelity

import (
	"testing"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/lots"
)

func TestCorporateActionReadsTheAction(t *testing.T) {
	for desc, want := range map[string]canonical.TxAction{
		"RETURN OF CAPITAL EXAMPLE CORP":         canonical.TxActionReturnOfCapital,
		"IN LIEU OF FRX SHARE EXAMPLE CORP":      canonical.TxActionCashInLieu,
		"Expired call":                           canonical.TxActionExpiry,
		"MERGER MER FROM 000000000 EXAMPLE":      canonical.TxActionReorganisation,
		"REVERSE SPLIT R/S FROM 000000000":       canonical.TxActionReorganisation,
		"DISTRIBUTION EXAMPLE CORP (XYZ) (Cash)": canonical.TxActionReorganisation,
		"":                                       canonical.TxActionReorganisation,
	} {
		if got := lotAction(canonical.TxKindCorporateAction, desc); got != want {
			t.Errorf("%q: %q, want %q", desc, got, want)
		}
	}
	if got := lotAction(canonical.TxKindDividend, "RETURN OF CAPITAL EXAMPLE CORP"); got != "" {
		t.Errorf("a dividend's kind says it: %q", got)
	}
	p, ok := lots.PolicyFor(kindName)
	if !ok || p.Mode != lots.Fill || !p.DatedBySettlement {
		t.Errorf("policy %+v", p)
	}
}

// An option leg keyed on its underlying moves no lot of the stock,
// whatever its kind; the stock's own rows read by their kind.
func TestAnOptionLegOnTheStockMovesNoLot(t *testing.T) {
	for desc, kind := range map[string]canonical.TxKind{
		"YOU BOUGHT PUT (XYZ) EXAMPLE CORP CL A":           canonical.TxKindBuy,
		"YOU SOLD CALL (XYZ) EXAMPLE CORP CL A":            canonical.TxKindSell,
		"INTER BROKER DELIVER PUT (XYZ) EXAMPLE CORP CL A": canonical.TxKindTransferOut,
		"EXPIRED CALL (XYZ) EXAMPLE CORP CL A":             canonical.TxKindCorporateAction,
	} {
		if got := lotAction(kind, desc); got != canonical.TxActionIgnore {
			t.Errorf("%q: %q, want ignored", desc, got)
		}
	}
	if got := lotAction(canonical.TxKindSell, "YOU SOLD EXAMPLE CORP CL A @ 100.00"); got != "" {
		t.Errorf("a share sale: %q", got)
	}
	if got := lotAction(canonical.TxKindCorporateAction, "CALLED BOND EXAMPLE 4.5% 2030"); got != canonical.TxActionReorganisation {
		t.Errorf("a call of a bond is no option leg: %q", got)
	}
}
