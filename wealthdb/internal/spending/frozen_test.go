package spending

import (
	"context"
	"database/sql"
	"fmt"
	"regexp"
	"strings"
	"testing"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
)

// The frozen oracle for the spending pass.
//
// The income family is being folded into this package: one pass, two
// vocabularies, with the tier ladder written once over a per-family
// descriptor instead of once per family. That refactor may not change
// a single spending verdict, and "may not" is worth more as a test
// than as an intention — every assertion elsewhere in this package
// checks one row at a time, so a refactor that quietly moved a
// provenance on some row nothing asserts would ship green.
//
// So: one fixture that reaches every tier, and the WHOLE overlay
// compared against a literal captured before the refactor began. The
// golden is deliberately unreadable as a data structure and
// deliberately exact. If it fails, the question is never "is the new
// output also fine" — it is which tier moved, and why.
//
// assigned_at is excluded, being wall-clock. Everything else the pass
// writes is here, the provider column included.

// frozenSnapshot is enrichmentSnapshot plus every other column the
// pass writes: the oracle's contract is to cover all of them, and a
// column outside it is a column a refactor could move in silence. The
// three far columns and the stated exposure were outside it until
// migration 0102 gave the golden a reason to move anyway.
func frozenSnapshot(t *testing.T, db *sql.DB, ctx context.Context) string {
	t.Helper()
	rows, err := db.QueryContext(ctx, `
        SELECT silver_source_id, transaction_external_id,
               COALESCE(merchant_signature, '(null)'), signature_version,
               COALESCE(spend_detailed, '(null)'), provenance,
               COALESCE(merchant_label, '(null)'),
               COALESCE(far_silver_source_id, '(null)'),
               COALESCE(far_account_external_id, '(null)'),
               COALESCE(far_class, '(null)'),
               COALESCE(stated_asset_class, '(null)'),
               COALESCE(provider_spend_detailed, '(null)')
          FROM spend_txn_enrichment
         ORDER BY silver_source_id, transaction_external_id`)
	if err != nil {
		t.Fatalf("read overlay: %v", err)
	}
	defer rows.Close()
	var b strings.Builder
	for rows.Next() {
		var src, id, sig, detailed, prov, label string
		var farSrc, farAcct, farClass, exposure, provider string
		var version int
		if err := rows.Scan(&src, &id, &sig, &version, &detailed, &prov, &label,
			&farSrc, &farAcct, &farClass, &exposure, &provider); err != nil {
			t.Fatalf("scan overlay: %v", err)
		}
		fmt.Fprintf(&b, "%s/%s sig=%q v%d cat=%s via=%s label=%s far=%s/%s farclass=%s exposure=%s issuer=%s\n",
			src, id, sig, version, detailed, prov, label,
			farSrc, farAcct, farClass, exposure, provider)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate overlay: %v", err)
	}
	return b.String()
}

// seedFrozenFixture reaches every tier the pass has, and each row says
// which one it is there for. Narratives are synthetic and modelled on
// the shapes the tier tests above already prove fire.
func seedFrozenFixture(t *testing.T, db *sql.DB, ctx context.Context) Options {
	t.Helper()
	seedTxns(t, db, ctx,
		// signature-only: nothing places it, and it is the model
		// tier's backlog.
		txn{"bank", "T-BACKLOG", "CARD1", "purchase", day(10), -11,
			"Unplaceable Counterparty", "", ""},
		// provider: the issuer's own filing, translated and claimed.
		txn{"bank", "T-PROVIDER", "CARD1", "purchase", day(10), -50,
			"Corner Market", "", "Groceries"},
		// provider, recorded but NOT claimed: a catch-all says only
		// "somewhere in this primary".
		txn{"bank", "T-PROVIDER-CATCHALL", "CARD1", "purchase", day(10), -60,
			"Some Department Store", "", "Shopping"},
		// built-in rule over provider: the issuer called an ATM
		// withdrawal "Shopping".
		txn{"bank", "T-RULE", "CARD1", "purchase", day(10), -200,
			"ATM Withdrawal Main Street", "", "Shopping"},
		// built-in card rule: a bill with no card leg anywhere, which
		// keeps the label the rule wrote.
		txn{"bank", "T-CARD-BILL", "CASH1", "withdrawal", day(11), -450,
			"PAYMENT TO CHASE CARD ENDING IN ####", "", ""},
		// config rule: the holder's own word about a narrative.
		txn{"bank", "T-CONFIG-RULE", "CASH1", "withdrawal", day(12), -900,
			"EXAMPLE BROKER SUBSCRIPTION", "", ""},
		// matcher, both legs, across two sources: evidence outranks
		// every inference.
		txn{"bank", "T-MATCH-OUT", "CASH1", "withdrawal", day(20), -400,
			"Autopay Payment", "", "Shopping"},
		txn{"other-bank", "T-MATCH-IN", "CASH2", "deposit", day(20), 400, "", "", ""},
		// a matched leg OUTSIDE the spending population: the deposit
		// side is not spend, and carries the verdict all the same.
		txn{"bank", "T-FUND-OUT", "CASH1", "withdrawal", day(25), -700,
			"Transfer To Investment", "", ""},
		txn{"bank", "T-FUND-IN", "BRK1", "deposit", day(25), 700, "", "", ""},
		// pin over everything, on a row the matcher also paired.
		txn{"bank", "T-PIN-OVER-MATCH", "CASH1", "withdrawal", day(30), -300,
			"Looks Internal", "", ""},
		txn{"bank", "T-PIN-PAIR", "CUST1", "deposit", day(30), 300, "", "", ""},
		// pin on a row OUTSIDE the population: a deposit the holder
		// names, which nothing else would reach.
		txn{"other-bank", "T-PIN-OUTSIDE", "CASH2", "deposit", day(31), 250,
			"Named By Hand", "", ""},
		// the memo field, which the built-ins do not read.
		txn{"bank", "T-MEMO", "CASH1", "withdrawal", day(32), -75,
			"", "atm withdrawal main street", ""},
	)
	return Options{
		Rules: []Rule{
			{Match: regexp.MustCompile(`(?i)EXAMPLE BROKER`), Category: canonical.SpendDetailedInvestment,
				AssetClass: "private_equity"},
		},
		Pins: []Pin{
			// A pin names a row the way a statement shows it: source,
			// account, day, amount, currency.
			{Source: "bank", Account: "CASH1", Day: day(30), Amount: -300, Currency: "USD",
				Detailed: canonical.SpendDetailedGift},
			{Source: "other-bank", Account: "CASH2", Day: day(31), Amount: 250, Currency: "USD",
				Detailed: canonical.SpendDetailedOther},
		},
	}
}

// TestSpendingPassOutputIsFrozen is the oracle itself.
func TestSpendingPassOutputIsFrozen(t *testing.T) {
	db, ctx := openGold(t)
	opts := seedFrozenFixture(t, db, ctx)
	res := runPass(t, db, ctx, opts)

	if got := frozenSnapshot(t, db, ctx); got != frozenSpendingOverlay {
		t.Errorf("the spending overlay moved.\n--- got ---\n%s\n--- want ---\n%s", got, frozenSpendingOverlay)
	}

	// The counters are the pass's own observability, and a refactor
	// that routed a row through a different tier would move one of
	// them even if the verdict happened to come out the same.
	for _, tc := range []struct {
		name string
		got  int
		want int
	}{
		{"Population", res.Population, frozenPopulation},
		{"Enriched", res.Enriched, frozenEnriched},
		{"MatcherRows", res.MatcherRows, frozenMatcherRows},
		{"RuleRows", res.RuleRows, frozenRuleRows},
		{"ProviderRows", res.ProviderRows, frozenProviderRows},
		{"PinRows", res.PinRows, frozenPinRows},
		{"SignatureOnlyRows", res.SignatureOnlyRows, frozenSignatureOnlyRows},
	} {
		if tc.got != tc.want {
			t.Errorf("Result.%s = %d, want %d", tc.name, tc.got, tc.want)
		}
	}
}

// The golden. The verdict fields were captured from the pass as it
// stood before the income family was folded in; the three far columns
// and the exposure were widened in when migration 0102 moved the
// golden anyway, having been outside the oracle until then. Never
// edited to make a test pass.
const frozenSpendingOverlay = `bank/T-BACKLOG sig="UNPLACEABLE COUNTERPARTY" v12 cat=(null) via=signature-only label=(null) far=(null)/(null) farclass=(null) exposure=(null) issuer=(null)
bank/T-CARD-BILL sig="PAYMENT TO CHASE CARD ENDING IN" v12 cat=card_spend via=rule label=Chase far=(null)/(null) farclass=(null) exposure=(null) issuer=(null)
bank/T-CONFIG-RULE sig="EXAMPLE BROKER SUBSCRIPTION" v12 cat=investment via=rule label=(null) far=(null)/(null) farclass=(null) exposure=private_equity issuer=(null)
bank/T-FUND-IN sig="(null)" v12 cat=internal_transfer via=matcher label=(null) far=bank/CASH1 farclass=(null) exposure=(null) issuer=(null)
bank/T-FUND-OUT sig="TRANSFER TO INVESTMENT" v12 cat=internal_transfer via=matcher label=(null) far=bank/BRK1 farclass=(null) exposure=(null) issuer=(null)
bank/T-MATCH-OUT sig="AUTOPAY PAYMENT" v12 cat=internal_transfer via=matcher label=(null) far=other-bank/CASH2 farclass=(null) exposure=(null) issuer=GENERAL_MERCHANDISE_OTHER_GENERAL_MERCHANDISE
bank/T-MEMO sig="ATM WITHDRAWAL MAIN STREET" v12 cat=cash_withdrawal via=rule label=(null) far=(null)/(null) farclass=(null) exposure=(null) issuer=(null)
bank/T-PIN-OVER-MATCH sig="LOOKS INTERNAL" v12 cat=gift via=manual label=(null) far=bank/CUST1 farclass=(null) exposure=(null) issuer=(null)
bank/T-PIN-PAIR sig="(null)" v12 cat=internal_transfer via=matcher label=(null) far=bank/CASH1 farclass=(null) exposure=(null) issuer=(null)
bank/T-PROVIDER sig="CORNER MARKET" v12 cat=FOOD_AND_DRINK_GROCERIES via=provider label=(null) far=(null)/(null) farclass=(null) exposure=(null) issuer=FOOD_AND_DRINK_GROCERIES
bank/T-PROVIDER-CATCHALL sig="SOME DEPARTMENT STORE" v12 cat=(null) via=signature-only label=(null) far=(null)/(null) farclass=(null) exposure=(null) issuer=GENERAL_MERCHANDISE_OTHER_GENERAL_MERCHANDISE
bank/T-RULE sig="ATM WITHDRAWAL MAIN STREET" v12 cat=cash_withdrawal via=rule label=(null) far=(null)/(null) farclass=(null) exposure=(null) issuer=GENERAL_MERCHANDISE_OTHER_GENERAL_MERCHANDISE
other-bank/T-MATCH-IN sig="(null)" v12 cat=internal_transfer via=matcher label=(null) far=bank/CASH1 farclass=(null) exposure=(null) issuer=(null)
other-bank/T-PIN-OUTSIDE sig="NAMED BY HAND" v12 cat=other via=manual label=(null) far=(null)/(null) farclass=(null) exposure=(null) issuer=(null)
`

// Ten population rows — every purchase and withdrawal above — plus the
// four rows the pass reaches outside it: three matched deposit legs
// (T-MATCH-IN, T-FUND-IN, T-PIN-PAIR) and one pinned deposit
// (T-PIN-OUTSIDE). The per-tier counts sum to Enriched exactly, which
// is the arithmetic a misrouted row breaks.
const (
	frozenPopulation        = 10
	frozenEnriched          = 14
	frozenMatcherRows       = 5
	frozenRuleRows          = 4
	frozenProviderRows      = 1
	frozenPinRows           = 2
	frozenSignatureOnlyRows = 2
)
