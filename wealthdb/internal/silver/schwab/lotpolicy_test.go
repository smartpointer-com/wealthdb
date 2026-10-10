package schwab

import (
	"testing"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/lots"
)

func TestCorporateRuleReadsTheActionThenTheDescription(t *testing.T) {
	cases := []struct {
		action, desc string
		want         lots.CorporateRule
	}{
		{"Return Of Capital", "EXAMPLE CORP", lots.CorpReturnOfCapital},
		{"Unissued Rights Redemption", "EXAMPLE CORP", lots.CorpIgnore},
		{"Litigation", "EXAMPLE CORP", lots.CorpIgnore},
		{"", "Exchange CALL EXAMPLE CORP $10", lots.CorpIgnore},
		{"", "Removed due to Expiration CALL EXAMPLE", lots.CorpExpiry},
		{"Reverse Split", "EXAMPLE CORP", lots.CorpReorg},
		{"", "EXAMPLE FUND FORWARD SPLIT WITH STOCK SPLIT SHARES", lots.CorpReorg},
	}
	for _, c := range cases {
		if got := corporateRule(lots.Txn{Action: c.action, Description: c.desc}); got != c.want {
			t.Errorf("%q %q: %v, want %v", c.action, c.desc, got, c.want)
		}
	}
}
