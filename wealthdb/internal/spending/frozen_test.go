package spending

import (
	"context"
	"database/sql"
	"fmt"
	"regexp"
	"strings"
	"testing"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/gold"
)

// The frozen oracle for the spending pass.
//
// Every assertion elsewhere in this package checks one row at a time,
// so a change that quietly moved a provenance on some row nothing
// asserts would ship green.
//
// So: one fixture reaches every tier, and the WHOLE overlay is compared
// against a literal. The golden is deliberately exact: if it fails, the
// question is which tier moved, and why. It moves with
// `SignatureVersion` and with a deliberate verdict change, never to make
// a test pass.
//
// assigned_at is excluded, being wall-clock. Everything else the pass
// writes is here, the provider column included.

// frozenSnapshot is enrichmentSnapshot plus every other column the
// pass writes: the oracle's contract is to cover all of them, and a
// column outside it is a column a change could move in silence. The
// far side the pass links for each row and the stated exposure are
// inside it.
func frozenSnapshot(t *testing.T, db *sql.DB, ctx context.Context) string {
	t.Helper()
	rows, err := db.QueryContext(ctx, `
        SELECT e.silver_source_id, e.transaction_external_id,
               COALESCE(e.merchant_signature, '(null)'), e.signature_version,
               COALESCE(e.spend_detailed, '(null)'), e.provenance,
               COALESCE(e.merchant_label, '(null)'),
               COALESCE(l.far_silver_source_id, '(null)'),
               COALESCE(l.far_account_external_id, '(null)'),
               COALESCE(e.stated_asset_class, '(null)'),
               COALESCE(e.provider_spend_detailed, '(null)')
          FROM spend_txn_enrichment e
          LEFT JOIN transfer_links l USING (silver_source_id, transaction_external_id)
         ORDER BY e.silver_source_id, e.transaction_external_id`)
	if err != nil {
		t.Fatalf("read overlay: %v", err)
	}
	defer rows.Close()
	var b strings.Builder
	for rows.Next() {
		var src, id, sig, detailed, prov, label string
		var farSrc, farAcct, exposure, provider string
		var version int
		if err := rows.Scan(&src, &id, &sig, &version, &detailed, &prov, &label,
			&farSrc, &farAcct, &exposure, &provider); err != nil {
			t.Fatalf("scan overlay: %v", err)
		}
		fmt.Fprintf(&b, "%s/%s sig=%q v%d cat=%s via=%s label=%s far=%s/%s exposure=%s issuer=%s\n",
			src, id, sig, version, detailed, prov, label,
			farSrc, farAcct, exposure, provider)
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
	assertLinksAreTheOverlay(t, db)

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

// The golden. It moves only with SignatureVersion or a
// deliberate verdict change, never to make a test pass.
const frozenSpendingOverlay = `bank/T-BACKLOG sig="UNPLACEABLE COUNTERPARTY" v13 cat=(null) via=signature-only label=(null) far=(null)/(null) exposure=(null) issuer=(null)
bank/T-CARD-BILL sig="PAYMENT TO CHASE CARD ENDING IN" v13 cat=card_spend via=rule label=Chase far=(null)/(null) exposure=(null) issuer=(null)
bank/T-CONFIG-RULE sig="EXAMPLE BROKER SUBSCRIPTION" v13 cat=investment via=rule label=(null) far=(null)/(null) exposure=private_equity issuer=(null)
bank/T-FUND-IN sig="(null)" v13 cat=internal_transfer via=matcher label=(null) far=bank/CASH1 exposure=(null) issuer=(null)
bank/T-FUND-OUT sig="TRANSFER TO INVESTMENT" v13 cat=internal_transfer via=matcher label=(null) far=bank/BRK1 exposure=(null) issuer=(null)
bank/T-MATCH-OUT sig="AUTOPAY PAYMENT" v13 cat=internal_transfer via=matcher label=(null) far=other-bank/CASH2 exposure=(null) issuer=GENERAL_MERCHANDISE_OTHER_GENERAL_MERCHANDISE
bank/T-MEMO sig="ATM WITHDRAWAL MAIN STREET" v13 cat=cash_withdrawal via=rule label=(null) far=(null)/(null) exposure=(null) issuer=(null)
bank/T-PIN-OVER-MATCH sig="LOOKS INTERNAL" v13 cat=gift via=manual label=(null) far=bank/CUST1 exposure=(null) issuer=(null)
bank/T-PIN-PAIR sig="(null)" v13 cat=internal_transfer via=matcher label=(null) far=bank/CASH1 exposure=(null) issuer=(null)
bank/T-PROVIDER sig="CORNER MARKET" v13 cat=FOOD_AND_DRINK_GROCERIES via=provider label=(null) far=(null)/(null) exposure=(null) issuer=FOOD_AND_DRINK_GROCERIES
bank/T-PROVIDER-CATCHALL sig="SOME DEPARTMENT STORE" v13 cat=(null) via=signature-only label=(null) far=(null)/(null) exposure=(null) issuer=GENERAL_MERCHANDISE_OTHER_GENERAL_MERCHANDISE
bank/T-RULE sig="ATM WITHDRAWAL MAIN STREET" v13 cat=cash_withdrawal via=rule label=(null) far=(null)/(null) exposure=(null) issuer=GENERAL_MERCHANDISE_OTHER_GENERAL_MERCHANDISE
other-bank/T-MATCH-IN sig="(null)" v13 cat=internal_transfer via=matcher label=(null) far=bank/CASH1 exposure=(null) issuer=(null)
other-bank/T-PIN-OUTSIDE sig="NAMED BY HAND" v13 cat=other via=manual label=(null) far=(null)/(null) exposure=(null) issuer=(null)
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

// The frozen oracle for the income family, and for the far facts the
// income family's rules and pins move on the spending overlay.
//
// Same contract as the spending oracle above: one fixture reaches every
// income tier, the WHOLE income overlay is compared against a literal,
// and so is the spending overlay the same fixture leaves, because an
// income rule's far account and an income pin's step-around are written
// there. assigned_at is excluded, being wall-clock.

// frozenIncomeSnapshot is every column the pass writes on the income
// overlay but assigned_at.
func frozenIncomeSnapshot(t *testing.T, db *sql.DB, ctx context.Context) string {
	t.Helper()
	rows, err := db.QueryContext(ctx, `
        SELECT silver_source_id, transaction_external_id,
               COALESCE(payer_signature, '(null)'), signature_version,
               COALESCE(income_detailed, '(null)'), provenance,
               COALESCE(stated_asset_class, '(null)'),
               COALESCE(provider_income_detailed, '(null)')
          FROM income_txn_enrichment
         ORDER BY silver_source_id, transaction_external_id`)
	if err != nil {
		t.Fatalf("read income overlay: %v", err)
	}
	defer rows.Close()
	var b strings.Builder
	for rows.Next() {
		var src, id, sig, detailed, prov, exposure, provider string
		var version int
		if err := rows.Scan(&src, &id, &sig, &version, &detailed, &prov, &exposure, &provider); err != nil {
			t.Fatalf("scan income overlay: %v", err)
		}
		fmt.Fprintf(&b, "%s/%s sig=%q v%d cat=%s via=%s exposure=%s provider=%s\n",
			src, id, sig, version, detailed, prov, exposure, provider)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate income overlay: %v", err)
	}
	return b.String()
}

// seedFrozenIncomeFixture reaches every income tier, and the far facts
// the spending overlay records: a pair, a reversal, a far rule in both
// directions, a stated counter account and the mortgage rule's class.
// Narratives are synthetic; amounts differ so that no two rows pair
// except the ones meant to.
func seedFrozenIncomeFixture(t *testing.T, db *sql.DB, ctx context.Context) Options {
	t.Helper()
	if _, err := db.ExecContext(ctx, `
        INSERT INTO silver_sources (silver_source_id, silver_kind, silver_path,
                                    high_watermark, first_loaded_at, last_loaded_at)
             VALUES ('swiss', 'ubs', '/tmp/ubs.db', -1, 0, 0);
        INSERT INTO accounts (silver_source_id, account_external_id, account_kind,
                              tax_wrapper, display_name, first_seen_at, last_seen_at)
             VALUES ('swiss', 'CASH9', 'cash', NULL, 'Swiss', 1, 1),
                    (?, 'savings-bank', 'cash', 'taxable_personal', 'Example Savings', 1, 1);
    `, canonical.DeclaredSourceID); err != nil {
		t.Fatalf("seed a bank source and a declared account: %v", err)
	}
	seedTxns(t, db, ctx,
		// signature-only: nothing places it, and it is the model
		// tier's backlog.
		txn{"bank", "I-BACKLOG", "CASH1", "deposit", day(10), 901, "Example Payer", "", ""},
		// provider: the bank names the movement.
		txn{"swiss", "I-PROVIDER", "CASH9", "deposit", day(20), 5000, "", "", "SALARY PAYMENT"},
		// provider, a rail rather than a payer.
		txn{"swiss", "I-PROVIDER-RAIL", "CASH9", "deposit", day(22), 601, "Example Payer Two", "", "e-banking credit"},
		// built-in narrative rule over provider.
		txn{"swiss", "I-RULE", "CASH9", "deposit", day(24), 402, "CASH DEPOSIT BRANCH", "", "SALARY PAYMENT"},
		// built-in custody rule: a deposit on a custody account.
		txn{"bank", "I-CUSTODY", "CUST1", "deposit", day(26), 2503, "Wire received", "", ""},
		// config rule over provider.
		txn{"swiss", "I-CONFIG-RULE", "CASH9", "deposit", day(28), 904, "EXAMPLE LETTING AGENT", "", "SALARY PAYMENT"},
		// config rule with a stated exposure.
		txn{"bank", "I-EXPOSURE", "CASH1", "deposit", day(30), 9005, "", "EXAMPLE VENTURE FUND RETURN", ""},
		// matcher over everything below it, across two sources.
		txn{"swiss", "I-MATCH-IN", "CASH9", "deposit", day(40), 706, "Own Transfer", "", "SALARY PAYMENT"},
		txn{"bank", "I-MATCH-OUT", "CASH1", "withdrawal", day(40), -706, "Own Transfer", "", ""},
		// pin over everything, on a row the matcher also paired.
		txn{"swiss", "I-PIN", "CASH9", "deposit", day(50), 257, "Looks Internal", "", "SALARY PAYMENT"},
		txn{"bank", "I-PIN-PAIR", "CASH1", "withdrawal", day(50), -257, "Looks Internal", "", ""},
		// pin on a row OUTSIDE the income population: its account is
		// excluded from income's scope.
		txn{"other-bank", "I-PIN-OUTSIDE", "CUST2", "deposit", day(60), 158, "Funds received", "", ""},
		// an income far rule: the verdict on the income overlay, the
		// declared account on the spending one.
		txn{"bank", "I-FAR-IN", "CASH1", "deposit", day(70), 2009, "", "Transfer from Example Savings Bank", ""},
		// the same rule on a row income pinned: the pin is the whole
		// answer, and the spending overlay steps around it.
		txn{"bank", "I-FAR-PINNED", "CASH1", "deposit", day(72), 510, "", "Transfer from Example Savings Bank", ""},
		// a kind the floor answers.
		txn{"bank", "I-DIVIDEND", "BRK1", "dividend", day(80), 41, "", "", ""},
		// a deposit and its reversal on one account.
		txn{"bank", "I-BOOKED", "CASH1", "deposit", day(90), 911, "Example Payer Three", "", ""},
		txn{"bank", "I-REVERSED", "CASH1", "deposit", day(91), -911, "Example Payer Three", "", ""},
		// the mortgage rule's own value, on the spending side.
		txn{"bank", "I-MORTGAGE", "CASH1", "withdrawal", day(100), -1812, "", "MORTGAGE PAYMENT", ""},
		// a stated counter account on an outflow, and on an inflow.
		txn{"bank", "I-STATED-OUT", "CASH1", "withdrawal", day(110), -333, "", "Wire to own account", ""},
		txn{"bank", "I-STATED-IN", "CASH1", "deposit", day(120), 444, "", "Wire from own account", ""},
		// a spending far rule on the spending side's own row.
		txn{"bank", "I-FAR-OUT", "CASH1", "withdrawal", day(130), -3007, "", "Transfer to Example Savings Bank", ""},
	)
	seedCounterAccount(t, db, ctx, "bank", "I-STATED-OUT", "CUST2")
	seedCounterAccount(t, db, ctx, "bank", "I-STATED-IN", "CASH2")

	far := Rule{Match: regexp.MustCompile(`(?i)EXAMPLE SAVINGS BANK`),
		Category: canonical.SpendDetailedInternalTransfer, Far: "savings-bank"}
	return Options{
		Rules: []Rule{far},
		Income: IncomeOptions{
			Exclude: map[string][]string{"other-bank": {"CUST2"}},
			Rules: []Rule{
				{Match: regexp.MustCompile(`(?i)EXAMPLE LETTING AGENT`), Category: "INCOME_RENTAL"},
				{Match: regexp.MustCompile(`(?i)EXAMPLE VENTURE FUND RETURN`),
					Category: canonical.IncomeDetailedCapitalReturn, AssetClass: "private_debt"},
				far,
			},
			Pins: []Pin{
				{Source: "swiss", Account: "CASH9", Day: day(50), Amount: 257, Currency: "USD",
					Detailed: canonical.IncomeDetailedInheritance},
				{Source: "other-bank", Account: "CUST2", Day: day(60), Amount: 158, Currency: "USD",
					Detailed: "INCOME_OTHER"},
				{Source: "bank", Account: "CASH1", Day: day(72), Amount: 510, Currency: "USD",
					Detailed: "INCOME_OTHER"},
			},
		},
	}
}

// TestIncomePassOutputIsFrozen is the income oracle.
func TestIncomePassOutputIsFrozen(t *testing.T) {
	db, ctx := openGold(t)
	opts := seedFrozenIncomeFixture(t, db, ctx)
	res := runPass(t, db, ctx, opts)

	if got := frozenIncomeSnapshot(t, db, ctx); got != frozenIncomeOverlay {
		t.Errorf("the income overlay moved.\n--- got ---\n%s\n--- want ---\n%s", got, frozenIncomeOverlay)
	}
	if got := frozenSnapshot(t, db, ctx); got != frozenIncomeFixtureSpendingOverlay {
		t.Errorf("the spending overlay of the income fixture moved.\n--- got ---\n%s\n--- want ---\n%s",
			got, frozenIncomeFixtureSpendingOverlay)
	}
	assertLinksAreTheOverlay(t, db)
	// Fifteen population rows, plus the one pinned row outside it
	// (I-PIN-OUTSIDE). The pair counters are the load summary's, read
	// off the same pass.
	for _, tc := range []struct {
		name string
		got  int
		want int
	}{
		{"Income.Population", res.Income.Population, 15},
		{"Income.Enriched", res.Income.Enriched, 16},
		{"Income.MatcherRows", res.Income.MatcherRows, 3},
		{"Income.RuleRows", res.Income.RuleRows, 5},
		{"Income.ProviderRows", res.Income.ProviderRows, 1},
		{"Income.PinRows", res.Income.PinRows, 3},
		{"Income.SignatureOnlyRows", res.Income.SignatureOnlyRows, 4},
		{"Income.StatedExposures", res.Income.StatedExposures, 1},
		{"StatedFarAccounts", res.StatedFarAccounts, 1},
		{"Cashflow.ReferencePairs", res.Cashflow.ReferencePairs, 0},
		{"Cashflow.StatedCounterPairs", res.Cashflow.StatedCounterPairs, 0},
		{"Cashflow.NamedPairs", res.Cashflow.NamedPairs, 0},
		{"Cashflow.ReversalPairs", res.Cashflow.ReversalPairs, 1},
		{"Cashflow.ConvertedPairs", res.Cashflow.ConvertedPairs, 0},
	} {
		if tc.got != tc.want {
			t.Errorf("Result.%s = %d, want %d", tc.name, tc.got, tc.want)
		}
	}
}

// The goldens of the income oracle. They move only with SignatureVersion
// or a deliberate verdict change, never to make a test pass.
const frozenIncomeOverlay = `bank/I-BACKLOG sig="EXAMPLE PAYER" v13 cat=(null) via=signature-only exposure=(null) provider=(null)
bank/I-BOOKED sig="EXAMPLE PAYER THREE" v13 cat=internal_transfer via=matcher exposure=(null) provider=(null)
bank/I-CUSTODY sig="WIRE RECEIVED" v13 cat=capital_return via=rule exposure=(null) provider=(null)
bank/I-DIVIDEND sig="(null)" v13 cat=(null) via=signature-only exposure=(null) provider=(null)
bank/I-EXPOSURE sig="EXAMPLE VENTURE FUND RETURN" v13 cat=capital_return via=rule exposure=private_debt provider=(null)
bank/I-FAR-IN sig="TRANSFER FROM EXAMPLE SAVINGS BANK" v13 cat=internal_transfer via=rule exposure=(null) provider=(null)
bank/I-FAR-PINNED sig="TRANSFER FROM EXAMPLE SAVINGS BANK" v13 cat=INCOME_OTHER via=manual exposure=(null) provider=(null)
bank/I-REVERSED sig="EXAMPLE PAYER THREE" v13 cat=internal_transfer via=matcher exposure=(null) provider=(null)
bank/I-STATED-IN sig="WIRE FROM OWN ACCOUNT" v13 cat=(null) via=signature-only exposure=(null) provider=(null)
other-bank/I-PIN-OUTSIDE sig="FUNDS RECEIVED" v13 cat=INCOME_OTHER via=manual exposure=(null) provider=(null)
swiss/I-CONFIG-RULE sig="EXAMPLE LETTING AGENT" v13 cat=INCOME_RENTAL via=rule exposure=(null) provider=INCOME_SALARY
swiss/I-MATCH-IN sig="OWN TRANSFER" v13 cat=internal_transfer via=matcher exposure=(null) provider=INCOME_SALARY
swiss/I-PIN sig="LOOKS INTERNAL" v13 cat=inheritance via=manual exposure=(null) provider=INCOME_SALARY
swiss/I-PROVIDER sig="(null)" v13 cat=INCOME_SALARY via=provider exposure=(null) provider=INCOME_SALARY
swiss/I-PROVIDER-RAIL sig="EXAMPLE PAYER TWO" v13 cat=(null) via=signature-only exposure=(null) provider=(null)
swiss/I-RULE sig="CASH DEPOSIT BRANCH" v13 cat=cash_deposit via=rule exposure=(null) provider=INCOME_SALARY
`

const frozenIncomeFixtureSpendingOverlay = `bank/I-BOOKED sig="EXAMPLE PAYER THREE" v13 cat=internal_transfer via=matcher label=(null) far=bank/CASH1 exposure=(null) issuer=(null)
bank/I-FAR-IN sig="TRANSFER FROM EXAMPLE SAVINGS BANK" v13 cat=internal_transfer via=rule label=(null) far=declared/savings-bank exposure=(null) issuer=(null)
bank/I-FAR-OUT sig="TRANSFER TO EXAMPLE SAVINGS BANK" v13 cat=internal_transfer via=rule label=(null) far=declared/savings-bank exposure=(null) issuer=(null)
bank/I-MATCH-OUT sig="OWN TRANSFER" v13 cat=internal_transfer via=matcher label=(null) far=swiss/CASH9 exposure=(null) issuer=(null)
bank/I-MORTGAGE sig="MORTGAGE PAYMENT" v13 cat=mortgage_transfer via=rule label=(null) far=(null)/(null) exposure=(null) issuer=(null)
bank/I-PIN-PAIR sig="LOOKS INTERNAL" v13 cat=internal_transfer via=matcher label=(null) far=swiss/CASH9 exposure=(null) issuer=(null)
bank/I-REVERSED sig="EXAMPLE PAYER THREE" v13 cat=internal_transfer via=matcher label=(null) far=bank/CASH1 exposure=(null) issuer=(null)
bank/I-STATED-OUT sig="WIRE TO OWN ACCOUNT" v13 cat=(null) via=signature-only label=(null) far=other-bank/CUST2 exposure=(null) issuer=(null)
swiss/I-MATCH-IN sig="OWN TRANSFER" v13 cat=internal_transfer via=matcher label=(null) far=bank/CASH1 exposure=(null) issuer=(null)
swiss/I-PIN sig="LOOKS INTERNAL" v13 cat=internal_transfer via=matcher label=(null) far=bank/CASH1 exposure=(null) issuer=(null)
`

// TestTransferLinksAreFrozen holds transfer_links to a literal over the
// income fixture, which reaches every road a link has but the ledger,
// the reference, the described counter leg and the name
// (TestTransferLinksCarryTheAssertedRoads): a pair on amount, a
// reversal, a stated counter account and a far rule in both directions.
func TestTransferLinksAreFrozen(t *testing.T) {
	db, ctx := openGold(t)
	runPass(t, db, ctx, seedFrozenIncomeFixture(t, db, ctx))
	if got := strings.Join(linkRows(t, db, ctx), "\n") + "\n"; got != frozenTransferLinks {
		t.Errorf("transfer_links moved.\n--- got ---\n%s\n--- want ---\n%s", got, frozenTransferLinks)
	}
}

// The golden of transfer_links. It moves only with SignatureVersion or a
// deliberate verdict change, never to make a test pass.
const frozenTransferLinks = `bank/I-BOOKED far=bank/CASH1 tx=I-REVERSED by=reversal measure=cash
bank/I-FAR-IN far=declared/savings-bank tx=(null) by=rule measure=cash
bank/I-FAR-OUT far=declared/savings-bank tx=(null) by=rule measure=cash
bank/I-MATCH-OUT far=swiss/CASH9 tx=I-MATCH-IN by=amount measure=cash
bank/I-PIN-PAIR far=swiss/CASH9 tx=I-PIN by=amount measure=cash
bank/I-REVERSED far=bank/CASH1 tx=I-BOOKED by=reversal measure=cash
bank/I-STATED-OUT far=other-bank/CUST2 tx=(null) by=stated-account measure=cash
swiss/I-MATCH-IN far=bank/CASH1 tx=I-MATCH-OUT by=amount measure=cash
swiss/I-PIN far=bank/CASH1 tx=I-PIN-PAIR by=amount measure=cash
`

// assertLinksAreTheOverlay holds transfer_links to the spending overlay:
// every cash link is the far side of a row the pass wrote there. The
// in-kind ledger links are writeStatedMoveLinks', and a stated leg the
// matcher never paired has no overlay row.
func assertLinksAreTheOverlay(t *testing.T, db *sql.DB) {
	t.Helper()
	orphans := scanPairs(t, db, `
        SELECT l.silver_source_id || '/' || l.transaction_external_id, l.evidence
          FROM transfer_links l
          LEFT JOIN spend_txn_enrichment e USING (silver_source_id, transaction_external_id)
         WHERE l.measure = ? AND e.silver_source_id IS NULL`, gold.LinkMeasureCash)
	if len(orphans) > 0 {
		t.Errorf("links with no spending row: %v", orphans)
	}
}

// TestTransferLinksCarryTheAssertedRoads holds the links of the three
// roads the income fixture does not reach to a literal: a pair the
// override ledger forces, a pair one leg's payload describes in another
// currency, and a pair a configured name points at. Each pair's counter
// and the phase the audit listing reads back move with it.
func TestTransferLinksCarryTheAssertedRoads(t *testing.T) {
	db, ctx := openGold(t)
	seedTxns(t, db, ctx,
		// Fifteen days apart: only the ledger pairs them.
		txn{"bank", "L-LEDGER-OUT", "CASH1", "withdrawal", day(10), -620, "", "Outgoing payment", ""},
		txn{"other-bank", "L-LEDGER-IN", "CUST2", "deposit", day(25), 620, "", "Incoming payment", ""},
		// The debit's payload states the CHF figure it became.
		txn{"bank", "L-FX-OUT", "CASH1", "withdrawal", day(40), -500, "", "Currency exchange", ""},
		// A narrative names the receiving account.
		txn{"bank", "L-NAMED-OUT", "CASH1", "withdrawal", day(60), -1234, "", "TO EXAMPLE NAMED BANK", ""},
		txn{"other-bank", "L-NAMED-IN", "CASH2", "deposit", day(61), 1234, "", "Incoming transfer", ""},
	)
	seedTxnsIn(t, db, ctx, "CHF",
		txn{"bank", "L-FX-IN", "BRK1", "deposit", day(40), 450, "", "Currency exchange", ""})
	if _, err := db.ExecContext(ctx, `
        UPDATE transactions SET payload = '{"counter_currency":"CHF","counter_amount":450}'
         WHERE transaction_external_id = 'L-FX-OUT'`); err != nil {
		t.Fatalf("stamp the counter leg: %v", err)
	}
	res := runPass(t, db, ctx, Options{
		MatchNames: []CounterpartyName{
			{Source: "other-bank", Account: "CASH2", Pattern: regexp.MustCompile(`(?i)example named bank`)},
		},
		TransferOverrides: []gold.TransferOverrideRule{{
			Verb: gold.VerbMatch,
			A:    gold.TransferOverrideSelector{Source: "bank", Account: "CASH1", Day: day(10), Amount: -620, Currency: "USD"},
			B:    &gold.TransferOverrideSelector{Source: "other-bank", Account: "CUST2", Day: day(25), Amount: 620, Currency: "USD"},
		}},
	})

	if got := strings.Join(linkRows(t, db, ctx), "\n") + "\n"; got != frozenAssertedLinks {
		t.Errorf("transfer_links moved.\n--- got ---\n%s\n--- want ---\n%s", got, frozenAssertedLinks)
	}
	assertLinksAreTheOverlay(t, db)
	if res.Cashflow.StatedCounterPairs != 1 || res.Cashflow.NamedPairs != 1 {
		t.Errorf("StatedCounterPairs, NamedPairs = %d, %d, want 1, 1",
			res.Cashflow.StatedCounterPairs, res.Cashflow.NamedPairs)
	}

	pairs, _, err := MatchedPairs(ctx, db)
	if err != nil {
		t.Fatalf("MatchedPairs: %v", err)
	}
	by := map[string]gold.TransferMatchPhase{}
	for _, p := range pairs {
		by[p.Debit.TxID+"/"+p.Credit.TxID] = p.By
	}
	want := map[string]gold.TransferMatchPhase{
		"L-LEDGER-OUT/L-LEDGER-IN": gold.MatchedByOverride,
		"L-FX-OUT/L-FX-IN":         gold.MatchedByStatedCounter,
		"L-NAMED-OUT/L-NAMED-IN":   gold.MatchedByName,
	}
	if fmt.Sprint(by) != fmt.Sprint(want) {
		t.Errorf("the listed pairs are %v, want %v", by, want)
	}
}

// The golden of the asserted roads' links. It moves only with
// SignatureVersion or a deliberate verdict change, never to make a test
// pass.
const frozenAssertedLinks = `bank/L-FX-IN far=bank/CASH1 tx=L-FX-OUT by=stated-counter measure=cash
bank/L-FX-OUT far=bank/BRK1 tx=L-FX-IN by=stated-counter measure=cash
bank/L-LEDGER-OUT far=other-bank/CUST2 tx=L-LEDGER-IN by=ledger measure=cash
bank/L-NAMED-OUT far=other-bank/CASH2 tx=L-NAMED-IN by=named measure=cash
other-bank/L-LEDGER-IN far=bank/CASH1 tx=L-LEDGER-OUT by=ledger measure=cash
other-bank/L-NAMED-IN far=bank/CASH1 tx=L-NAMED-OUT by=named measure=cash
`
