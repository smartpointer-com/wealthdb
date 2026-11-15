// Package firstcitizens projects the firstcitizens collector's silver (a First
// Citizens Bank retail deposit relationship — checking / savings) into
// canonical change records. See collectors/firstcitizens/ for the bronze/silver
// schema and the gold mapping this implements. The projection is the shared
// bank adapter's (silver/bank); this file declares the source.
//
// A deposit relationship is cash-only, so the projection is simple (and
// parallel to the chase sibling, whose silver is column-compatible):
//
//   - One ACCOUNT (AccountKind 'cash') per deposit account. DisplayName from
//     the nickname (falling back to the last-4 mask); TaxWrapper
//     'taxable_personal', ManagementStyle 'self_directed'. All overridable via
//     account_overrides. These are conduit accounts — cash passes through them
//     between other sources — so the registered ReturnsPolicy hides their
//     return rows at every grain; the balances and flows still enter the
//     aggregates, where only the transaction flow matters.
//
//   - CASH BALANCES, not positions or instruments — cash is not an instrument.
//     They land in gold's cash_balances table (report_cash synthesises a
//     read-time cash position from them, so they still surface in holdings):
//     a CLOSING balance for every day the balance moved, valued at that day's
//     end-of-day running balance from the transaction ledger (the exact cash
//     time series, back to the ledger's start). Unlike chase, the running
//     balance is present on every history row, so the series is exact with no
//     gaps. Plus a CURRENT balance from the roster's live balance at each
//     run whose roster listed the account (silver's run_accounts), so the
//     latest net worth reflects the balance now. An account the latest runs
//     leave out has no balance after its last listing run, so gold can end
//     it. Each closing balance is stamped at its day's last second, so an
//     as-of query at a day's end returns that day's closing balance. The
//     rate a statement states rides on a closing balance of its cycle
//     (rates.go).
//
//   - TRANSACTIONS: the whole deposit ledger. The collector signs amounts the
//     canonical way (positive = balance increase), so the source sign is
//     preserved; summing NetAmount reproduces the account's net cash flow.
//     The history row's transactionId is the stable external id.
package firstcitizens

import (
	"fmt"
	"slices"
	"strings"
	"unicode"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/returns"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/silver"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/silver/bank"
)

const kindName = "firstcitizens"

// spanExtrema is the MIN/MAX of every date the projection touches: account
// snapshots (accounts.snapshot_at), the loaded runs (dump_runs.snapshot_at,
// where the CURRENT balances are stamped) and the transaction ledger: the
// transaction events at posted_at, and the per-day cash marks at the day's
// last second. It is the snapshot range Status reports.
var spanExtrema = fmt.Sprintf(`
SELECT MIN(t), MAX(t) FROM (
    SELECT snapshot_at AS t FROM accounts
    UNION ALL SELECT snapshot_at FROM dump_runs
    UNION ALL SELECT posted_at FROM transactions
    UNION ALL SELECT posted_at + %d FROM transactions WHERE balance IS NOT NULL)`, silver.DayEndOffset)

// adapter projects the source.
var adapter = bank.New(bank.Source{
	Kind:            kindName,
	DefaultCurrency: "USD",
	// See returns.DepositBankPolicy for the flow-complete + hidden-plumbing
	// rationale.
	Returns: returns.DepositBankPolicy(),
	// The shared load clock (silver.LoadClock): this source has no change
	// feed of its own, so a loaded dump is the trigger and a full re-emit
	// is the response.
	Clock:          silver.LoadClock(kindName, spanExtrema, silver.Range("transactions", "posted_at")),
	NameCol:        "nickname",
	NameIsNickname: true,
	AccountKind:    canonical.AccountKindCash,
	Listing:        "run_accounts",
	Closings: []bank.Series{{
		// One CLOSING cash balance per day the balance moved, valued at
		// that day's end-of-day running balance from the transaction
		// ledger — the exact cash time series, back to the ledger's
		// start. Every history row carries a runningBalance, so the
		// series has no gaps.
		//
		// The ledger has no intra-day order, so the fitid-highest row of
		// the day stands for its end-of-day balance
		// (silver.RunningBalanceQuery); the imprecision is bounded by
		// same-day activity and resolves by the next day.
		Label:  "appendRunningBalances",
		Basis:  "running_balance",
		Query:  silver.RunningBalanceQuery,
		DayEnd: true,
	}},
	Probe: probeStatementRates,
	IDCol: "fitid",
	Cols: bank.LedgerCols{
		Description: "description",
		Category:    "json_extract(payload, '$.pfmData.classificationDesc')",
	},
	TxKind: func(r bank.Row, _ bool) (canonical.TxKind, bool, bool) {
		return txKind(r.Description, r.Amount), true, false
	},
	Text: func(r bank.Row, tx *canonical.TransactionChange) {
		tx.ProviderCategory = providerCategory(r.Category)
	},
})

// init registers the kind (silver.Source). An account's display name is
// the roster's nickname where it has one.
func init() { silver.Register(adapter.Source()) }

// providerCategory is the bank's own filing of the row: the personal-finance
// classification its app shows ("Transfer", "Interest Income", "Federal
// Tax"). It is carried as found and translated nowhere — the bank files by
// guess, and a rule the holder wrote outranks it — so it is what a rule and
// the transfer fence can read. `Uncategorized` says nothing and is dropped.
func providerCategory(class string) *string {
	class = strings.TrimSpace(class)
	if strings.EqualFold(class, "Uncategorized") {
		return nil
	}
	return silver.StrPtrIfNonEmpty(class)
}

// txKind maps a First Citizens deposit transaction to a canonical TxKind.
// Unlike chase there is no OFX TRNTYPE in silver (the coarse silver `kind` is
// only DEBIT/CREDIT from `isDebit`), so interest and fees are recognised from
// the description text; everything else is a deposit or withdrawal by the
// (canonical) sign of the amount, which is the faithful classification for a
// cash conduit account with no richer categorisation.
//
// A fee is a service charge, or FEE or FEES as a word of its own. A longer
// word that only starts with the letters (a payee's name) names no fee.
func txKind(description string, amt canonical.Decimal) canonical.TxKind {
	d := strings.ToUpper(description)
	switch {
	case strings.Contains(d, "INTEREST"):
		return canonical.TxKindInterest
	case strings.Contains(d, "SERVICE CHARGE") || hasWord(d, "FEE", "FEES"):
		return canonical.TxKindFee
	case amt.IsNegative():
		return canonical.TxKindWithdrawal
	default:
		return canonical.TxKindDeposit
	}
}

// hasWord reports whether any of words is a word of s: a run of letters
// and digits between any other runes.
func hasWord(s string, words ...string) bool {
	for _, w := range strings.FieldsFunc(s, func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	}) {
		if slices.Contains(words, w) {
			return true
		}
	}
	return false
}
