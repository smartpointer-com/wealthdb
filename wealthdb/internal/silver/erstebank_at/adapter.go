// Package erstebankat projects the erstebank_at collector's silver (an
// Austrian Erste Bank und Sparkassen / George retail relationship: checking
// accounts and credit cards) into canonical change records. See
// collectors/erstebank_at/ for the bronze/silver schema. The projection is
// the shared bank adapter's (silver/bank); this file declares the source.
//
//   - One ACCOUNT per silver account: AccountKind 'cash' for a checking
//     account, 'card' for a credit card. DisplayName is George's product name
//     with the last-4 mask; TaxWrapper 'taxable_personal', ManagementStyle
//     'self_directed'. All overridable via account_overrides. The source
//     registers the plain bank policy, and the engine keeps every cash
//     account out of returns by its kind and drops cards at the loader.
//
//   - CASH BALANCES, never positions. A CURRENT balance from the roster at
//     each run that listed the account (silver's run_accounts), and a CLOSING
//     balance for every day the derived daily-balance series carries, stamped
//     at the day's last second. Silver holds a card's balance as the POSITIVE
//     amount owed; the engine negates every card figure and passes a checking
//     figure through.
//
//   - TRANSACTIONS: both ledgers, keyed by George's row id. Amounts are
//     already signed the canonical way (money out negative). The kind comes
//     from George's transaction type (kinds.go); information rows that book
//     no amount are left out.
package erstebankat

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/returns"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/silver"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/silver/bank"
)

const kindName = "erstebank_at"

// silverCard is silver's account_kind for a credit card; every other account
// is a checking account.
const silverCard = "credit_card"

// spanExtrema is the MIN/MAX of every date the projection touches: account
// snapshots, the loaded runs (where the CURRENT balances are stamped), the
// ledger (posted_at), and the daily closes at each day's last second.
var spanExtrema = fmt.Sprintf(`
SELECT MIN(t), MAX(t) FROM (
    SELECT snapshot_at AS t FROM accounts
    UNION ALL SELECT snapshot_at FROM dump_runs
    UNION ALL SELECT posted_at FROM transactions
    UNION ALL SELECT balance_date + %d FROM daily_balances)`, silver.DayEndOffset)

var adapter = bank.New(bank.Source{
	Kind:            kindName,
	DefaultCurrency: "EUR",
	Returns:         returns.DefaultReturnsPolicy(returns.BankFlowPolicy()),
	Clock:           silver.LoadClock(kindName, spanExtrema, silver.Range("transactions", "posted_at")),
	NameCol:         "display_name",
	ProductCol:      "account_kind",
	CardProduct:     silverCard,
	AccountKind:     canonical.AccountKindCash,
	Display:         func(r bank.RosterRow) string { return displayName(r.Name, r.Mask) },
	Listing:         "run_accounts",
	ListingVersion:  1,
	Closings: []bank.Series{{
		Label: "daily balances",
		Basis: "daily_balance",
		Query: `
SELECT account_external_id, balance_date, balance
  FROM daily_balances
 WHERE balance_date BETWEEN ? AND ?
 ORDER BY account_external_id, balance_date`,
		DayEnd: true,
	}},
	IDCol: "txn_id",
	Cols: bank.LedgerCols{
		Kind:         "tx_type",
		Description:  "description",
		Counterparty: "counterparty",
		Currency:     "currency",
		// George's own sub-category, which it lets the account holder
		// re-file, so it never sets a kind.
		Category: "json_extract(payload, '$.categories[0].subCategory')",
	},
	// A row without IS_BAL_RELEVANT books no amount and moves no balance.
	Where:  "balance_relevant = 1",
	TxKind: txKind,
	Text: func(r bank.Row, tx *canonical.TransactionChange) {
		tx.Memo = silver.StrPtrIfNonEmpty(memo(r.Payload))
	},
})

var collectors = []silver.Collector{
	{
		Name:    kindName,
		Display: "Erste Bank (George)",
		Group:   silver.GroupAustria,
		Covers:  "checking accounts and credit cards",
		Files:   []string{"erstebank_at.db"},
	},
}

func init() {
	s := adapter.Source()
	s.Collectors = collectors
	silver.Register(s)
}

// displayName is George's product name with the last-4 mask (e.g.
// "Girokonto …1234"), or either alone.
func displayName(product, mask string) string {
	return strings.TrimSpace(product + " " + mask)
}

// memo is the payer's free-text purpose lines, which gold keeps out of the
// merchant signature.
func memo(payload string) string {
	var p struct {
		Texts struct {
			Lines []any `json:"lineItems"`
		} `json:"additionalTexts"`
	}
	_ = json.Unmarshal([]byte(payload), &p)
	var parts []string
	for _, l := range p.Texts.Lines {
		if s, ok := l.(string); ok && s != "" {
			parts = append(parts, s)
		}
	}
	return strings.Join(parts, " ")
}

// merchant says the row carries a merchant category code.
func merchant(payload string) bool {
	var p struct {
		MCC any `json:"mcc"`
	}
	_ = json.Unmarshal([]byte(payload), &p)
	switch v := p.MCC.(type) {
	case nil:
		return false
	case string:
		return v != ""
	default:
		return true
	}
}
