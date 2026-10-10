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
