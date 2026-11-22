// Package raiffeisenat projects the raiffeisen_at collector's silver (an
// Austrian Raiffeisen / Mein ELBA retail deposit relationship — checking /
// savings) into canonical change records. See collectors/raiffeisen_at/ for
// the bronze/silver schema and the gold mapping this implements. The
// projection is the shared bank adapter's (silver/bank); this file declares
// the source.
//
// A deposit relationship is cash-only, so the projection is simple (and
// parallel to the chase / firstcitizens siblings):
//
//   - One ACCOUNT (AccountKind 'cash') per deposit account. DisplayName from
//     the account type (Kontoart) and the last-4 mask; TaxWrapper
//     'taxable_personal', ManagementStyle 'self_directed'. All overridable via
//     account_overrides. The rates its details state ride under the
//     stated-rate keys (rates.go). Deposit accounts are not investments. The
//     source registers the plain bank policy, and the engine keeps every
//     cash account out of returns by its kind, on any source: no return
//     rows of its own at any grain, while its balances and flows still
//     enter the aggregates.
//
//   - CASH BALANCES, not positions or instruments — cash is not an instrument.
//     A CLOSING balance for every day the daily-balance series carries a saldo
//     (the `kontostaende` series, in silver's daily_balances table — unlike the
//     US siblings, the transaction history has no per-row running balance, so
//     the cash time series comes from this dedicated series). Plus a CURRENT
//     balance from the roster's live balance at each run whose roster listed
//     the account (silver's run_accounts), so the latest net worth reflects
//     the balance now. Each listing run covers every account of the
//     roster, so an account a run leaves out ends at that run
//     (bank.appendListedCovers). Each day's saldo is
//     stamped at the day's last second, so the series gives the day's value.
//
//   - TRANSACTIONS: the whole deposit ledger. The collector signs amounts the
//     canonical way (positive = balance increase), so the source sign is
//     preserved; summing NetAmount reproduces the account's net cash flow.
//     The kontoumsaetze row id is the stable external id. The text columns
//     follow the contract documented on textColumns; they are a projection
//     of text only and never influence a row's kind or amount.
package raiffeisenat

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/returns"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/silver"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/silver/bank"
)

// kindName is the silver_kind / collector name (the whitelist entry the
// 0035 gold migration adds).
const kindName = "raiffeisen_at"

// spanExtrema is the MIN/MAX of every date the projection touches: account
// snapshots (accounts.snapshot_at), the loaded runs (dump_runs.snapshot_at,
// where the CURRENT balances are stamped), the transaction ledger
// (posted_at), and the daily-balance series (the closing cash marks, at each
// balance day's last second, which reach back further than the ledger's own
// window). It is the snapshot range Status reports.
var spanExtrema = fmt.Sprintf(`
SELECT MIN(t), MAX(t) FROM (
    SELECT snapshot_at AS t FROM accounts
    UNION ALL SELECT snapshot_at FROM dump_runs
    UNION ALL SELECT posted_at FROM transactions
    UNION ALL SELECT balance_date + %d FROM daily_balances)`, silver.DayEndOffset)

// adapter projects the source.
var adapter = bank.New(bank.Source{
	Kind:            kindName,
	DefaultCurrency: "EUR",
	// The bank policy: the ledger is the complete cash history. The engine
	// hides a cash account's own rows on any source (gold.hiddenConstituent)
	// and drops a card at load (gold.returnsInvisibleKind).
	Returns: returns.DefaultReturnsPolicy(returns.BankFlowPolicy()),
	// The shared load clock (silver.LoadClock): this source has no change
	// feed of its own, so a loaded dump is the trigger and a full re-emit
	// is the response.
	Clock:          silver.LoadClock(kindName, spanExtrema, silver.Range("transactions", "posted_at")),
	NameCol:        "nickname",
	TypeCol:        "account_type",
	NameIsNickname: true,
	AccountKind:    canonical.AccountKindCash,
	Display:        func(r bank.RosterRow) string { return displayName(r.Name, r.AccountType, r.Mask) },
	Account:        withDetailsRates,
	Listing:        "run_accounts",
	ListingVersion: 3,
	Closings: []bank.Series{{
		// One CLOSING cash balance per day the kontostaende series
		// carries a saldo — the cash time series (silver's
		// daily_balances table). Unlike the US siblings the transaction
		// history has no per-row balance, so this dedicated series is
		// the source of the closing marks.
		Label: "appendDailyBalances",
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
		Description: "description", Counterparty: "counterparty", Category: "category",
	},
	// The kind is classified from the raw purpose column, never from the
	// composed description, so the text projection cannot move a row's
	// kind.
	TxKind: func(r bank.Row, _ bool) (canonical.TxKind, bool, bool) {
		return txKind(r.Category, r.Description, r.Amount), true, false
	},
	Text: func(r bank.Row, tx *canonical.TransactionChange) {
		tx.Description, tx.Counterparty = textColumns(r.Description, r.Counterparty, r.Payload)
	},
})

// collectors are the collectors whose silver the kind reads
// (silver.Collector).
var collectors = []silver.Collector{
	{
		Name:    "raiffeisen_at",
		Display: "Raiffeisen (Mein ELBA)",
		Group:   silver.GroupAustria,
		Covers:  "checking and savings",
		Files:   []string{"raiffeisen_at.db"},
	},
}

// init registers the kind (silver.Source). An account's display name is
// the roster's nickname where it has one.
func init() {
	s := adapter.Source()
	s.Collectors = collectors
	silver.Register(s)
}

// withDetailsRates states the rates an account's details state under the
// stated-rate keys (detailsRateKeys).
func withDetailsRates(acc *canonical.AccountChange) error {
	keys := detailsRateKeys(acc.Payload)
	if keys == nil {
		return nil
	}
	var err error
	if acc.Payload, err = silver.WithStatedRates(acc.Payload, keys); err != nil {
		return fmt.Errorf("raiffeisen_at appendAccounts %s: %w", acc.AccountExternalID, err)
	}
	return nil
}

// displayName prefers a user nickname, then the account type paired with the
// last-4 mask (e.g. "Testkonto …1234"), then either alone.
func displayName(nickname, accType, mask string) string {
	if nickname != "" {
		return nickname
	}
	switch {
	case accType != "" && mask != "":
		return accType + " " + mask
	case accType != "":
		return accType
	default:
		return mask
	}
}

// textColumns applies the text contract to one row. Silver promotes the
// purpose line (Verwendungszweck) to `description` and the participant line
// (Transaktionsteilnehmer) to `counterparty`; a card row carries neither,
// its merchant sitting in the preserved row's card-network object and its
// only narrative being the payment reference. The promoted columns win
// verbatim, and the preserved kontoumsaetze row (`payload`) supplies the
// fallbacks:
//
//   - counterparty: the participant line verbatim; when empty, the
//     card-network merchant name (`payload.ethocaHaendler.name`) verbatim.
//     Either feeds gold's merchant signature, so neither is reformatted.
//   - description: the purpose line when present (unchanged); otherwise the
//     short order purpose (`auftragskurzVerwendungszweck`) followed by the
//     payment reference (`zahlungsreferenz`), "; "-joined, whichever exist.
//     Nothing is fabricated: a row with no text stays NULL.
//
// provider_category is the source category slug (`category`, kategorieCode)
// verbatim. A payload that does not decode contributes nothing.
func textColumns(desc, cp, payload string) (description, counterparty *string) {
	var p struct {
		ShortPurpose string `json:"auftragskurzVerwendungszweck"`
		PaymentRef   string `json:"zahlungsreferenz"`
		Merchant     struct {
			Name string `json:"name"`
		} `json:"ethocaHaendler"`
	}
	_ = json.Unmarshal([]byte(payload), &p)
	description = silver.StrPtrIfNonEmpty(desc)
	if description == nil {
		description = silver.StrPtrIfNonEmpty(silver.JoinText(p.ShortPurpose, p.PaymentRef))
	}
	counterparty = silver.StrPtrIfNonEmpty(cp)
	if counterparty == nil {
		counterparty = silver.StrPtrIfNonEmpty(p.Merchant.Name)
	}
	return description, counterparty
}

// txKind maps a Raiffeisen deposit transaction to a canonical TxKind. Interest
// and fees are recognised from the source category (kategorieCode) or the
// description text — German and English tokens both, since the wire mixes them;
// everything else is a deposit or withdrawal by the (canonical) sign of the
// amount, the faithful classification for a deposit account. The full
// category is preserved in `payload` for any later refinement.
//
// The long, unambiguous tokens (INTEREST, ZINS, ENTGELT, …) are matched as
// substrings so a German compound (Kontoentgelt) or an inflection (Zinsen)
// still hits; the short, ambiguous ones (FEE) are matched only as whole slug /
// word tokens, so "coffee" is not mistaken for a fee. Rows from a supplied
// transaction listing carry no category, so their account-closing entries
// are recognised from the booking text alone — "Kontoführung" (the account
// maintenance charge) names no ENTGELT and needs its own token.
func txKind(category, description string, amt canonical.Decimal) canonical.TxKind {
	hay := strings.ToUpper(category + " " + description)
	switch {
	case containsAny(hay, "INTEREST", "ZINS"):
		return canonical.TxKindInterest
	case containsAny(hay, "ENTGELT", "SPESEN", "GEBÜHR", "GEBUEHR", "KONTOFÜHRUNG", "KONTOFUEHRUNG",
		"SERVICE CHARGE", "SERVICE FEE"):
		return canonical.TxKindFee
	case silver.HasWord(hay, "FEE", "FEES", "CHARGE", "CHARGES"):
		return canonical.TxKindFee
	case amt.IsNegative():
		return canonical.TxKindWithdrawal
	default:
		return canonical.TxKindDeposit
	}
}

func containsAny(s string, subs ...string) bool {
	for _, sub := range subs {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}
