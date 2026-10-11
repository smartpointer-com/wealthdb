package fidelity

import (
	"testing"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/lots"
)

func TestCorporateRuleReadsTheAction(t *testing.T) {
	for desc, want := range map[string]lots.CorporateRule{
		"RETURN OF CAPITAL EXAMPLE CORP":         lots.CorpReturnOfCapital,
		"IN LIEU OF FRX SHARE EXAMPLE CORP":      lots.CorpCashInLieu,
		"EXPIRED PUT (XYZ) EXAMPLE CORP":         lots.CorpExpiry,
		"MERGER MER FROM 000000000 EXAMPLE":      lots.CorpReorg,
		"REVERSE SPLIT R/S FROM 000000000":       lots.CorpReorg,
		"DISTRIBUTION EXAMPLE CORP (XYZ) (Cash)": lots.CorpReorg,
	} {
		if got := corporateRule(lots.Txn{Description: desc}); got != want {
			t.Errorf("%q: %v, want %v", desc, got, want)
		}
	}
	p, ok := lots.PolicyFor(kindName)
	if !ok || p.Mode != lots.Fill || !p.DatedBySettlement {
		t.Errorf("policy %+v", p)
	}
	if c := p.Classify(lots.Txn{Kind: "corporate_action", Description: "EXPIRED CALL"}); c.Action != lots.Corporate || c.Rule != lots.CorpExpiry {
		t.Errorf("classified %+v", c)
	}
}

// An option leg keyed on its underlying moves no lot of the stock,
// whatever its kind; the stock's own rows read as usual.
func TestAnOptionLegOnTheStockMovesNoLot(t *testing.T) {
	for desc, kind := range map[string]string{
		"YOU BOUGHT PUT (XYZ) EXAMPLE CORP CL A":           "buy",
		"YOU SOLD CALL (XYZ) EXAMPLE CORP CL A":            "sell",
		"INTER BROKER DELIVER PUT (XYZ) EXAMPLE CORP CL A": "journal",
		"EXPIRED CALL (XYZ) EXAMPLE CORP CL A":             "corporate_action",
	} {
		if c := classify(lots.Txn{Kind: kind, Description: desc, Quantity: -3}); c.Action != lots.Ignore {
			t.Errorf("%q: %+v, want ignored", desc, c)
		}
	}
	if c := classify(lots.Txn{Kind: "sell", Description: "YOU SOLD EXAMPLE CORP CL A @ 100.00", Quantity: -3}); c.Action != lots.Sell {
		t.Errorf("a share sale: %+v", c)
	}
	if c := classify(lots.Txn{Kind: "corporate_action", Description: "CALLED BOND EXAMPLE 4.5% 2030"}); c.Action != lots.Corporate {
		t.Errorf("a call of a bond is no option leg: %+v", c)
	}
}
