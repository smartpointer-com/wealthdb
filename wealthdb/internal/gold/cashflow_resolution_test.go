package gold

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"
	"testing"
)

// The resolution's pins. Two table-driven walks, each over one of the
// design's own tables: every row of the kind table (§2.3, §4), and
// every tax wrapper of the boundary table (§2.6) in both directions.
//
// Table-driven rather than scenario-driven on purpose. What is being
// pinned is a LOOKUP — this kind with this verdict lands on that node —
// and a scenario would test a handful of them and read as though it had
// tested the rule. Every expectation below names the node as
// `section.class.group`, which is the node's identity, so a row moving
// between two classes of one section cannot pass.

// ---- the fixture ---------------------------------------------------------

// seedResolutionFixture lays down the accounts, wrappers, instruments
// and overlay rows the walks read. Every id and amount is invented.
//
// The account roster is built to have one account on each side of the
// boundary, because the far-account test is what most of the rules turn
// on: a pooled cash account, a second pooled account to move between,
// one account per vehicle class, a giving one, a mortgage, and one
// pooled-by-wrapper account the cashflow scope fences out.
func seedResolutionFixture(t *testing.T, db *sql.DB, ctx context.Context) {
	t.Helper()
	if _, err := db.ExecContext(ctx, `
        INSERT INTO accounts (silver_source_id, account_external_id, account_kind,
                              tax_wrapper, display_name, first_seen_at, last_seen_at) VALUES
            ('cf', 'CASH',     'cash',      'taxable_personal',  'Everyday',   1, 1),
            ('cf', 'SAVE',     'cash',      'taxable_joint',     'Savings',    1, 1),
            ('cf', 'CARD',     'card',      NULL,                'Card',       1, 1),
            ('cf', 'BROK',     'brokerage', 'taxable_personal',  'Brokerage',  1, 1),
            ('cf', 'IRA',      'brokerage', 'roth_ira',          'Plan',       1, 1),
            ('cf', 'PLAN529',  'brokerage', '529',               'Education',  1, 1),
            ('cf', 'HSA',      'cash',      'hsa',               'Health',     1, 1),
            ('cf', 'TRUST',    'brokerage', 'trust_non_grantor', 'Trust',      1, 1),
            ('cf', 'DAF',      'donor_advised_fund', 'charitable', 'Giving',   1, 1),
            ('cf', 'UTMA',     'brokerage', 'custodial_utma',    'Custodial',  1, 1),
            ('cf', 'MORT',     'mortgage',  'taxable_personal',  'Mortgage',   1, 1),
            ('cf', 'FENCED',   'cash',      'taxable_personal',  'Fenced out', 1, 1);

        -- The boundary, as the enrichment pass stamps it.
        INSERT INTO cashflow_wrapper_sides (tax_wrapper, side, class) VALUES
            ('taxable_personal',  'household', NULL),
            ('taxable_joint',     'household', NULL),
            ('trust_grantor',     'household', NULL),
            ('other',             'household', NULL),
            ('traditional_ira',   'vehicle', 'retirement'),
            ('roth_ira',          'vehicle', 'retirement'),
            ('sep_ira',           'vehicle', 'retirement'),
            ('simple_ira',        'vehicle', 'retirement'),
            ('401k',              'vehicle', 'retirement'),
            ('403b',              'vehicle', 'retirement'),
            ('457b',              'vehicle', 'retirement'),
            ('pillar_2',          'vehicle', 'retirement'),
            ('vested_benefits',   'vehicle', 'retirement'),
            ('pillar_3a',         'vehicle', 'retirement'),
            ('529',               'vehicle', 'education'),
            ('coverdell_esa',     'vehicle', 'education'),
            ('hsa',               'vehicle', 'health'),
            ('trust_non_grantor', 'vehicle', 'trusts'),
            ('charitable',        'giving', NULL),
            ('trust_charitable',  'giving', NULL),
            ('foundation',        'giving', NULL),
            ('custodial_utma',    'giving', NULL),
            ('custodial_ugma',    'giving', NULL);

        INSERT INTO cashflow_account_scope (silver_source_id, account_external_id, mode) VALUES
            ('cf', 'FENCED', 'exclude');

        INSERT INTO instruments (silver_source_id, instrument_external_id, asset_class,
                                 symbol, name, first_seen_at, last_seen_at) VALUES
            ('cf', 'EQ',   'public_equity', 'EXEQ', 'Example Equity Fund',  1, 1),
            ('cf', 'PE',   'private_equity','EXPE', 'Example Venture Fund', 1, 1),
            ('cf', 'MMF',  'cash',          'EXMM', 'Example Money Market', 1, 1);
    `); err != nil {
		t.Fatalf("seed resolution fixture: %v", err)
	}
}

// line is one seeded transaction plus the verdicts the enrichment pass
// would have written for it.
type line struct {
	id          string
	account     string
	kind        string
	amount      float64
	instrument  string
	spend       string // spend_detailed, "" for none
	income      string // income_detailed, "" for none
	farAccount  string // the matcher's partner, "" for none
	farClass    string // a rule's own word, "" for none
	spendClass  string // stated_asset_class on the spending overlay, "" for none
	incomeClass string // stated_asset_class on the income overlay, "" for none
	want        string // "section.class.group", or "" for not a line
	why         string
}

// seedLines writes the transactions and both overlays, then returns the
// node each row resolved to, keyed by transaction id. A row that is not
// a line comes back as its disposition — `internal` or `excluded` — so
// a test can tell "pool-internal" from "declined and counted", which is
// the difference between an invisible movement and a canary.
func seedLines(t *testing.T, db *sql.DB, ctx context.Context, lines []line) map[string]string {
	t.Helper()
	for _, l := range lines {
		if _, err := db.ExecContext(ctx, `
            INSERT INTO transactions (silver_source_id, transaction_external_id, occurred_at,
                                      account_external_id, instrument_external_id,
                                      kind, currency, net_amount, description)
                 VALUES ('cf', ?, 1728000, ?, ?, ?, 'USD', ?, ?)`,
			l.id, l.account, nullable(l.instrument), l.kind, l.amount, l.id); err != nil {
			t.Fatalf("seed transaction %s: %v", l.id, err)
		}
		if l.spend != "" || l.farAccount != "" || l.farClass != "" || l.spendClass != "" {
			if _, err := db.ExecContext(ctx, `
                INSERT INTO spend_txn_enrichment (silver_source_id, transaction_external_id,
                        merchant_signature, signature_version, spend_detailed, provenance,
                        far_silver_source_id, far_account_external_id, far_class,
                        stated_asset_class, assigned_at)
                     VALUES ('cf', ?, 'sig', 1, ?, 'rule', ?, ?, ?, ?, 100)`,
				l.id, nullable(l.spend),
				nullable(map[bool]string{true: "cf", false: ""}[l.farAccount != ""]),
				nullable(l.farAccount), nullable(l.farClass), nullable(l.spendClass)); err != nil {
				t.Fatalf("seed spending overlay for %s: %v", l.id, err)
			}
		}
		if l.income != "" || l.incomeClass != "" {
			if _, err := db.ExecContext(ctx, `
                INSERT INTO income_txn_enrichment (silver_source_id, transaction_external_id,
                        payer_signature, signature_version, income_detailed, provenance,
                        stated_asset_class, assigned_at)
                     VALUES ('cf', ?, 'sig', 1, ?, 'rule', ?, 100)`,
				l.id, nullable(l.income), nullable(l.incomeClass)); err != nil {
				t.Fatalf("seed income overlay for %s: %v", l.id, err)
			}
		}
	}

	rows, err := db.QueryContext(ctx, `
        SELECT transaction_external_id, disposition,
               COALESCE(section, ''), COALESCE(class, ''), COALESCE(grp, '')
          FROM cashflow_txn_nodes(0, 3500000)`)
	if err != nil {
		t.Fatalf("read cashflow_txn_nodes: %v", err)
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var id, disposition, section, class, grp string
		if err := rows.Scan(&id, &disposition, &section, &class, &grp); err != nil {
			t.Fatalf("scan cashflow_txn_nodes: %v", err)
		}
		if disposition != "line" {
			out[id] = disposition
			continue
		}
		out[id] = section + "." + class + "." + grp
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate cashflow_txn_nodes: %v", err)
	}
	return out
}

func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// check walks the table, comparing the node each row landed on with the
// one the design's table says it should.
func check(t *testing.T, got map[string]string, lines []line) {
	t.Helper()
	for _, l := range lines {
		want := l.want
		if want == "" {
			want = "excluded"
		}
		if got[l.id] != want {
			t.Errorf("%s (%s): %s, want %s — %s", l.id, l.kind, got[l.id], want, l.why)
		}
	}
}

// ---- the kind table ------------------------------------------------------

// TestTheKindTable walks every row of the design's kind table: which
// section a transaction lands in given its kind and the verdict the two
// families resolved for it.
func TestTheKindTable(t *testing.T) {
	db, ctx := openMigrated(t)
	seedResolutionFixture(t, db, ctx)

	lines := []line{
		// The income kinds resolve by type, and the type decides the class.
		{id: "K-DIV", account: "BROK", kind: "dividend", amount: 100, instrument: "EQ",
			income: "INCOME_DIVIDENDS", want: "operating_in.yield.INCOME_DIVIDENDS",
			why: "a dividend is money the household's assets produced"},
		{id: "K-COUPON", account: "BROK", kind: "coupon", amount: 40, instrument: "EQ",
			income: "INCOME_INTEREST_EARNED", want: "operating_in.yield.INCOME_INTEREST_EARNED",
			why: "a bond coupon is interest earned"},
		{id: "K-STAKE", account: "BROK", kind: "staking", amount: 5, instrument: "EQ",
			income: "INCOME_STAKING", want: "operating_in.yield.INCOME_STAKING",
			why: "a staking reward is yield"},
		{id: "K-GAIN", account: "BROK", kind: "capital_gain", amount: 25, instrument: "EQ",
			income: "INCOME_DISTRIBUTIONS", want: "operating_in.yield.INCOME_DISTRIBUTIONS",
			why: "a fund's payout of realised gains returns no basis"},
		{id: "K-REWARD", account: "CARD", kind: "reward", amount: 3,
			income: "INCOME_REWARDS", want: "operating_in.other_receipts.INCOME_REWARDS",
			why: "a card credit is not income from wealth"},
		{id: "K-FEED-IN", account: "CASH", kind: "deposit", amount: 80,
			income: "INCOME_ENERGY_FEED_IN", want: "operating_in.yield.INCOME_ENERGY_FEED_IN",
			why: "feed-in revenue is what an owned installation produced"},
		{id: "K-INT-POS", account: "CASH", kind: "interest", amount: 6,
			income: "INCOME_INTEREST_EARNED", want: "operating_in.yield.INCOME_INTEREST_EARNED",
			why: "credited interest is yield"},
		{id: "K-INT-NEG", account: "CASH", kind: "interest", amount: -6,
			spend: "BANK_FEES_INTEREST_CHARGE", want: "operating_out.fees.BANK_FEES_INTEREST_CHARGE",
			why: "a finance charge is what an account cost"},
		{id: "K-WAGE", account: "CASH", kind: "deposit", amount: 5000,
			income: "INCOME_WAGES", want: "operating_in.earnings.INCOME_WAGES",
			why: "wages are money from labour"},
		{id: "K-PENSION", account: "CASH", kind: "deposit", amount: 900,
			income: "INCOME_RETIREMENT_PENSION", want: "operating_in.benefits.INCOME_RETIREMENT_PENSION",
			why: "a pension paid by a fund is an entitlement"},

		// The four verdicts the design re-homes.
		{id: "K-CAPRET", account: "CASH", kind: "deposit", amount: 800,
			income: "capital_return", want: "investing.elsewhere.capital_return",
			why: "the holder's own capital coming back is investing, not income"},
		{id: "K-LOAN-IN", account: "CASH", kind: "deposit", amount: 20000,
			income: "loan_proceeds", want: "financing.loans.loan_proceeds",
			why: "money borrowed is a liability incurred"},
		{id: "K-CLAIM", account: "CASH", kind: "deposit", amount: 120,
			income: "reimbursement", want: "operating_in.other_receipts.reimbursement",
			why: "income drops it; cashflow keeps it, because it is cash that arrived"},
		{id: "K-DEPLOY", account: "CASH", kind: "withdrawal", amount: -9000,
			spend: "investment", want: "investing.elsewhere.investment",
			why: "capital deployed to a destination the product does not track"},

		// The outflow kinds, by resolved category, with the three lifts.
		// The outflow leaf is the PRIMARY: the spending vocabulary has
		// ninety detailed values, and a diagram with ninety leaves is
		// not a diagram. The detailed value is a column away, behind
		// `-C +detailed` on the transactions view.
		{id: "K-SHOP", account: "CARD", kind: "purchase", amount: -40,
			spend: "FOOD_AND_DRINK_GROCERIES", want: "operating_out.consumption.FOOD_AND_DRINK",
			why: "groceries are consumption, and the leaf is the primary they roll up to"},
		{id: "K-REFUND", account: "CARD", kind: "refund", amount: 12,
			spend: "FOOD_AND_DRINK_GROCERIES", want: "operating_out.consumption.FOOD_AND_DRINK",
			why: "a refund nets inside the category it reverses"},
		// The three lifted classes keep the DETAILED value: each was
		// lifted for the distinction inside it, and a class whose one
		// leaf repeats its own name is a self-edge.
		{id: "K-FEE", account: "BROK", kind: "fee", amount: -30,
			spend: "BANK_FEES_INVESTMENT_FEES", want: "operating_out.fees.BANK_FEES_INVESTMENT_FEES",
			why: "a fee for investing is not a fee for banking, which is why the class exists"},
		{id: "K-TAX", account: "CASH", kind: "tax", amount: -4000,
			spend: "GOVERNMENT_AND_NON_PROFIT_TAX_PAYMENT", want: "operating_out.taxes.GOVERNMENT_AND_NON_PROFIT_TAX_PAYMENT",
			why: "every household-balance diagram lifts taxes"},
		{id: "K-WITHHELD", account: "BROK", kind: "tax", amount: -60,
			spend: "GOVERNMENT_AND_NON_PROFIT_WITHHOLDING_TAX", want: "operating_out.taxes.GOVERNMENT_AND_NON_PROFIT_WITHHOLDING_TAX",
			why: "withholding the household paid from its own accounts is cash that left"},
		{id: "K-DONATE", account: "CASH", kind: "withdrawal", amount: -500,
			spend: "GOVERNMENT_AND_NON_PROFIT_DONATIONS", want: "operating_out.giving.GOVERNMENT_AND_NON_PROFIT_DONATIONS",
			why: "a donation is giving, not a government department"},
		{id: "K-PASSPORT", account: "CASH", kind: "purchase", amount: -90,
			spend: "GOVERNMENT_AND_NON_PROFIT_GOVERNMENT_DEPARTMENTS_AND_AGENCIES",
			want:  "operating_out.consumption.GOVERNMENT_AND_NON_PROFIT",
			why:   "what remains of the government primary is consumption, under that primary's own leaf"},
		{id: "K-ATM", account: "CASH", kind: "withdrawal", amount: -200,
			spend: "cash_withdrawal", want: "operating_out.consumption.cash_withdrawal",
			why: "cash out is spend whose use is unobservable"},
		{id: "K-CARDSPEND", account: "CASH", kind: "withdrawal", amount: -430,
			spend: "card_spend", want: "operating_out.consumption.card_spend",
			why: "a bill on an unitemised card is real consumption"},

		// A gift is one value read from either side, and the two sides
		// are two nodes: keyed by section.class.group, never by the leaf.
		{id: "K-GIFT-OUT", account: "CASH", kind: "withdrawal", amount: -1000,
			spend: "gift", want: "operating_out.giving.gift",
			why: "a gift given is giving"},
		{id: "K-GIFT-IN", account: "CASH", kind: "deposit", amount: 1000,
			income: "gift", want: "operating_in.other_receipts.gift",
			why: "a gift received is a receipt, and not negative giving"},

		// The backlog is a node on each side, and they are two nodes.
		{id: "K-UNPLACED-OUT", account: "CASH", kind: "withdrawal", amount: -77,
			want: "operating_out.(uncategorized).(uncategorized)",
			why:  "an unplaced outflow is shown as the backlog, not guessed at"},
		{id: "K-UNPLACED-IN", account: "CASH", kind: "deposit", amount: 77,
			want: "operating_in.(uncategorized).(uncategorized)",
			why:  "an unplaced receipt is the other backlog"},

		// Investing: trades by asset class, private capital beside them,
		// and a cash-class instrument is cash becoming cash.
		{id: "K-BUY", account: "BROK", kind: "buy", amount: -10000, instrument: "EQ",
			want: "investing.public_equity.trades", why: "buying a security is cash leaving the pool"},
		{id: "K-SELL", account: "BROK", kind: "sell", amount: 12000, instrument: "EQ",
			want: "investing.public_equity.trades", why: "selling is cash arriving, gain or loss"},
		{id: "K-CALL", account: "BROK", kind: "contribution", amount: -5000, instrument: "PE",
			want: "investing.private_equity.private_capital", why: "a capital call is private capital out"},
		{id: "K-DIST", account: "BROK", kind: "distribution", amount: 7000, instrument: "PE",
			want: "investing.private_equity.private_capital",
			why:  "a fund's distribution returns contributed basis until something proves otherwise"},
		{id: "K-DIST-PROMOTED", account: "BROK", kind: "distribution", amount: 300, instrument: "PE",
			income: "INCOME_INTEREST_EARNED", want: "operating_in.yield.INCOME_INTEREST_EARNED",
			why: "a rule promoting it to an income type moves it to operating in, as the holder's word should"},
		{id: "K-MMF-BUY", account: "BROK", kind: "buy", amount: -50000, instrument: "MMF",
			want: "internal", why: "an instrument whose class is cash is pool-internal"},
		{id: "K-NOINST", account: "BROK", kind: "sell", amount: 400,
			want: "investing.elsewhere.trades", why: "a trade gold cannot name still moved cash"},

		// The kinds a section that reads direction cannot admit.
		{id: "K-FX", account: "CASH", kind: "fx", amount: -100, why: "no canonical sign, and cash becoming cash besides"},
		{id: "K-FWD", account: "BROK", kind: "fx_forward", amount: 20, why: "no canonical sign"},
		{id: "K-SWAP", account: "BROK", kind: "fx_swap", amount: -20, why: "no canonical sign"},
		{id: "K-CORP", account: "BROK", kind: "corporate_action", amount: 15, why: "a change to a holding, with no pinned sign"},
		{id: "K-JOURNAL", account: "CASH", kind: "journal", amount: 9, why: "a bookkeeping entry"},
		{id: "K-OTHERKIND", account: "CASH", kind: "other", amount: 9, why: "the catch-all both families decline"},
		{id: "K-CARDPAY-ALONE", account: "CARD", kind: "card_payment", amount: 430,
			why: "an unpaired card bill is in neither family's population, so no tier could place it"},
		{id: "K-XFERIN-ALONE", account: "BROK", kind: "transfer_in", amount: 300, instrument: "EQ",
			why: "an unmatched in-kind leg is counted rather than guessed at"},
		{id: "K-XFEROUT-ALONE", account: "BROK", kind: "transfer_out", amount: -300, instrument: "EQ",
			why: "the same on the way out"},
	}
	check(t, seedLines(t, db, ctx, lines), lines)
}

// ---- the boundary table --------------------------------------------------

// TestTheBoundaryTableInBothDirections walks every wrapper of the
// design's boundary table, as the far account of a matched own-account
// move, in both directions.
//
// Both directions matter because two of the five arms are DIRECTIONAL:
// a vehicle crossing reads the row's own sign, and a giving one lands
// in a different section depending on it — an irrevocable transfer out
// is giving, while a charitable remainder trust's annuity or a
// custodial account drawn for the minor's costs is cash arriving, and
// none of that is negative giving.
func TestTheBoundaryTableInBothDirections(t *testing.T) {
	db, ctx := openMigrated(t)
	seedResolutionFixture(t, db, ctx)

	var lines []line
	add := func(id, far, wantOut, wantIn, why string) {
		lines = append(lines,
			line{id: id + "-OUT", account: "CASH", kind: "withdrawal", amount: -1000,
				spend: "internal_transfer", farAccount: far, want: wantOut, why: why},
			line{id: id + "-IN", account: "CASH", kind: "deposit", amount: 1000,
				spend: "internal_transfer", income: "internal_transfer", farAccount: far,
				want: wantIn, why: why})
	}
	add("W-RET", "IRA", "vehicles.retirement.retirement", "vehicles.retirement.retirement",
		"a contribution and a distribution are one class, told apart by the row's sign")
	add("W-EDU", "PLAN529", "vehicles.education.education", "vehicles.education.education",
		"an education plan is the household's capital in an earmarked pool")
	add("W-HSA", "HSA", "vehicles.health.health", "vehicles.health.health",
		"a health account is earmarked for a cost, not for spending")
	add("W-TRUST", "TRUST", "vehicles.trusts.trusts", "vehicles.trusts.trusts",
		"a non-grantor trust is a separate taxpayer")
	add("W-DAF", "DAF", "operating_out.giving.vehicle_giving", "operating_in.other_receipts.vehicle_receipt",
		"the giving boundary is directional: out is irrevocable, in is cash arriving")
	add("W-UTMA", "UTMA", "operating_out.giving.vehicle_giving", "operating_in.other_receipts.vehicle_receipt",
		"a custodial account is legally the minor's the moment it is funded")
	add("W-MORT", "MORT", "financing.mortgage.mortgage", "financing.mortgage.mortgage",
		"a mortgage drawdown is an inflow and the same class as servicing it")
	add("W-POOL", "SAVE", "internal", "internal",
		"a wire between two pooled accounts is cash becoming cash")
	add("W-FENCED", "FENCED", "vehicles.untracked.untracked", "vehicles.untracked.untracked",
		"an account the pool excludes is treated exactly like one the product does not hold")

	// No far account at all: a rule placed the verdict.
	lines = append(lines,
		line{id: "W-RULE", account: "CASH", kind: "withdrawal", amount: -900,
			spend: "internal_transfer", want: "vehicles.untracked.untracked",
			why: "money at an institution the product does not collect, visible rather than lost to the residual"},
		line{id: "W-RULE-MORT", account: "CASH", kind: "withdrawal", amount: -1800,
			spend: "internal_transfer", farClass: "mortgage", want: "financing.mortgage.mortgage",
			why: "the mortgage rule's own word, for the lender the product does not track"},
	)
	check(t, seedLines(t, db, ctx, lines), lines)
}

// TestTheWrapperIsAskedBeforeTheKind pins the ORDER of the far-account
// test, which is the part a reordering would break silently. A card
// inside a foundation and a mortgage on a trust-owned property are both
// a kind and a wrapper, and the wrapper is what says whose money moved.
func TestTheWrapperIsAskedBeforeTheKind(t *testing.T) {
	db, ctx := openMigrated(t)
	seedResolutionFixture(t, db, ctx)
	if _, err := db.ExecContext(ctx, `
        INSERT INTO accounts (silver_source_id, account_external_id, account_kind,
                              tax_wrapper, display_name, first_seen_at, last_seen_at) VALUES
            ('cf', 'TRUSTMORT', 'mortgage', 'trust_non_grantor', 'Trust mortgage', 1, 1),
            ('cf', 'DAFCARD',   'card',     'charitable',        'Foundation card', 1, 1);
    `); err != nil {
		t.Fatalf("seed: %v", err)
	}
	lines := []line{
		{id: "O-TRUSTMORT", account: "CASH", kind: "withdrawal", amount: -1800,
			spend: "internal_transfer", farAccount: "TRUSTMORT",
			want: "vehicles.trusts.trusts",
			why:  "servicing a trust's mortgage moves the trust's money, not the household's debt"},
		{id: "O-DAFCARD", account: "CASH", kind: "withdrawal", amount: -300,
			spend: "internal_transfer", farAccount: "DAFCARD",
			want: "operating_out.giving.vehicle_giving",
			why:  "paying a foundation's card is giving, whatever the container is called"},
	}
	check(t, seedLines(t, db, ctx, lines), lines)
}

// TestTheCrossingDeltasAreDirectional pins the four values that exist
// for a crossing whose far side the product does not hold: one class
// each, and the row's own sign says whether it was a contribution or a
// distribution.
func TestTheCrossingDeltasAreDirectional(t *testing.T) {
	db, ctx := openMigrated(t)
	seedResolutionFixture(t, db, ctx)
	lines := []line{
		{id: "D-RET-OUT", account: "CASH", kind: "withdrawal", amount: -500,
			spend: "retirement_transfer", want: "vehicles.retirement.retirement",
			why: "a contribution wired to a plan nobody collects"},
		{id: "D-RET-IN", account: "CASH", kind: "deposit", amount: 500,
			income: "retirement_transfer", want: "vehicles.retirement.retirement",
			why: "the same value read the other way is a distribution"},
		{id: "D-EDU", account: "CASH", kind: "withdrawal", amount: -600,
			spend: "education_transfer", want: "vehicles.education.education",
			why: "an education plan the product does not collect"},
		{id: "D-HSA", account: "CASH", kind: "deposit", amount: 700,
			income: "health_transfer", want: "vehicles.health.health",
			why: "a medical cost reimbursed out of a health account"},
		{id: "D-TRUST", account: "CASH", kind: "withdrawal", amount: -800,
			spend: "trust_transfer", want: "vehicles.trusts.trusts",
			why: "funding a trust the product does not collect"},
		// The fifth delta, and the one with no wrapper behind it: a
		// bank's own deposit product. Both legs must name it, because
		// a far account could only ever have been written on the
		// spending one — the return leg reaches the resolution through
		// the income overlay and nothing else.
		{id: "D-DEP-OUT", account: "CASH", kind: "withdrawal", amount: -900,
			spend: "deposit_transfer", want: "vehicles.deposits.deposits",
			why: "cash parked in a call deposit the bank books under the funding account"},
		{id: "D-DEP-IN", account: "CASH", kind: "deposit", amount: 900,
			income: "deposit_transfer", want: "vehicles.deposits.deposits",
			why: "the principal coming back, read from the income side"},
		{id: "D-DEBT", account: "CASH", kind: "withdrawal", amount: -400,
			spend: "debt_repayment", want: "financing.loans.debt_repayment",
			why: "an instalment to a lender the product does not track"},
	}
	check(t, seedLines(t, db, ctx, lines), lines)
}

// TestAMortgageServicerNeedsNoFarAccount pins the verdict that reaches
// `financing · Mortgage` on its own.
//
// Both other roads to that node read the far side — a far account of
// kind `mortgage`, or `far_class`, which only the built-in tier may
// write — so a servicer gold holds no account for fell through to the
// untracked residual. `mortgage_transfer` is tested here with NEITHER
// far signal set, because needing neither is the whole of the point;
// and on both legs, since a drawdown arrives the other way.
//
// `debt_repayment` is asserted beside it unchanged: the two values
// must not collapse, because a mortgage is split into interest and
// principal downstream and a car loan is not.
func TestAMortgageServicerNeedsNoFarAccount(t *testing.T) {
	db, ctx := openMigrated(t)
	seedResolutionFixture(t, db, ctx)
	lines := []line{
		{id: "MT-OUT", account: "CASH", kind: "withdrawal", amount: -1800,
			spend: "mortgage_transfer", want: "financing.mortgage.mortgage",
			why: "an instalment to a servicer the product holds no account for"},
		{id: "MT-IN", account: "CASH", kind: "deposit", amount: 50000,
			income: "mortgage_transfer", want: "financing.mortgage.mortgage",
			why: "a tranche drawn from the same servicer, read from the income side"},
		{id: "MT-NOT-DEBT", account: "CASH", kind: "withdrawal", amount: -400,
			spend: "debt_repayment", want: "financing.loans.debt_repayment",
			why: "every other untracked lender still draws as loans, unsplit"},
	}
	check(t, seedLines(t, db, ctx, lines), lines)
}

// TestAServicerWithNoBalanceDrawsAsInterest pins what the leaf stage
// makes of the verdict above. The instalment/principal split reads a
// mortgage's own balance series, and a servicer gold holds no account
// for has none — so nothing is apportioned and the row draws whole.
//
// Interest is the right whole: it is exact for an interest-only
// tranche, which is the shape that motivated the verdict, and for any
// other it is a visible approximation rather than invented principal.
func TestAServicerWithNoBalanceDrawsAsInterest(t *testing.T) {
	db, ctx := openMigrated(t)
	seedResolutionFixture(t, db, ctx)
	seedLines(t, db, ctx, []line{
		{id: "MT-LEAF", account: "CASH", kind: "withdrawal", amount: -1800,
			spend: "mortgage_transfer"},
	})
	got := mortgageShares(t, db, ctx)
	if got["MT-LEAF"]["mortgage_interest"] != -1800 {
		t.Errorf("interest = %v, want the whole instalment",
			got["MT-LEAF"]["mortgage_interest"])
	}
	if _, split := got["MT-LEAF"]["mortgage_amortization"]; split {
		t.Error("principal was apportioned against a servicer with no balance series")
	}
}

// TestTheBaseIsThePoolsLines pins what cashflow_lines_base adds over
// the resolution: the near account must be in the pool, and the row
// must be a line. A vehicle's own dividends, trades and fees are not
// the household's cash flow.
func TestTheBaseIsThePoolsLines(t *testing.T) {
	db, ctx := openMigrated(t)
	seedResolutionFixture(t, db, ctx)
	seedLines(t, db, ctx, []line{
		{id: "B-HOUSEHOLD", account: "CASH", kind: "deposit", amount: 100, income: "INCOME_WAGES"},
		{id: "B-VEHICLE", account: "IRA", kind: "dividend", amount: 50, instrument: "EQ",
			income: "INCOME_DIVIDENDS"},
		{id: "B-VEHICLE-TRADE", account: "IRA", kind: "buy", amount: -50, instrument: "EQ"},
		{id: "B-FENCED", account: "FENCED", kind: "deposit", amount: 10, income: "INCOME_WAGES"},
		{id: "B-INTERNAL", account: "CASH", kind: "withdrawal", amount: -20,
			spend: "internal_transfer", farAccount: "SAVE"},
		{id: "B-EXCLUDED", account: "CASH", kind: "journal", amount: 1},
	})

	got := macroTxnIDs(t, db, ctx, "cashflow_lines_base", 0, 3500000)
	if _, ok := got["B-HOUSEHOLD"]; !ok {
		t.Error("the base dropped a household receipt")
	}
	for id, why := range map[string]string{
		"B-VEHICLE":       "a plan's own dividend is the vehicle's income, not the household's cash flow",
		"B-VEHICLE-TRADE": "what happens inside a vehicle is not the household's investing",
		"B-FENCED":        "an account the pool excludes contributes no lines",
		"B-INTERNAL":      "a wire between two pooled accounts is invisible",
		"B-EXCLUDED":      "a catch-all kind is declined and counted, never charted",
	} {
		if _, ok := got[id]; ok {
			t.Errorf("the base charts %s: %s", id, why)
		}
	}
}

// TestExcludedMeansCounted pins the canary's population: a row the
// resolution declined, on a POOLED account, is reachable and countable.
// A counter nobody reads is how a silent hole starts, and a counter
// that also swept up the vehicles' own rows would be permanently large
// and therefore unreadable.
func TestExcludedMeansCounted(t *testing.T) {
	db, ctx := openMigrated(t)
	seedResolutionFixture(t, db, ctx)
	seedLines(t, db, ctx, []line{
		{id: "C-FX", account: "CASH", kind: "fx", amount: -100},
		{id: "C-CARDPAY", account: "CARD", kind: "card_payment", amount: 430},
		{id: "C-VEHICLE-FX", account: "IRA", kind: "fx", amount: -100},
		{id: "C-MMF", account: "BROK", kind: "buy", amount: -50, instrument: "MMF"},
	})

	rows, err := db.QueryContext(ctx, `
        SELECT n.transaction_external_id
          FROM cashflow_txn_nodes(0, 3500000) n
          JOIN cashflow_pool_accounts() p
                 ON p.silver_source_id    = n.silver_source_id
                AND p.account_external_id = n.account_external_id
         WHERE n.disposition = 'excluded'`)
	if err != nil {
		t.Fatalf("count the canary: %v", err)
	}
	defer rows.Close()
	var got []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatalf("scan: %v", err)
		}
		got = append(got, id)
	}
	sort.Strings(got)
	want := []string{"C-CARDPAY", "C-FX"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("the canary counts %v, want %v — a vehicle's rows are not the pool's, "+
			"and a money-market trade is cash becoming cash rather than a decline", got, want)
	}
}

// TestNodeLabelsReadAsVocabulary pins the display names, which are what
// a diagram's nodes are called. No node is ever a merchant, a payer, an
// account or an instrument; the leaves are vocabulary, which is what
// makes the privacy twin normalisation alone.
func TestNodeLabelsReadAsVocabulary(t *testing.T) {
	db, ctx := openMigrated(t)
	seedResolutionFixture(t, db, ctx)
	seedLines(t, db, ctx, []line{
		{id: "L-WAGE", account: "CASH", kind: "deposit", amount: 100, income: "INCOME_WAGES"},
		{id: "L-BUY", account: "BROK", kind: "buy", amount: -100, instrument: "PE"},
		{id: "L-MORT", account: "CASH", kind: "withdrawal", amount: -100,
			spend: "internal_transfer", farClass: "mortgage"},
		{id: "L-UNTRACKED", account: "CASH", kind: "withdrawal", amount: -100,
			spend: "internal_transfer"},
		{id: "L-DAF", account: "CASH", kind: "withdrawal", amount: -100,
			spend: "internal_transfer", farAccount: "DAF"},
		// The two earmarked vehicles. Their labels name the ACT — money
		// set aside — because the word alone names two things: an
		// education plan and a tuition payment are both "Education",
		// and a contribution and a pension are both "Retirement".
		{id: "L-PLAN", account: "CASH", kind: "withdrawal", amount: -100,
			spend: "internal_transfer", farAccount: "PLAN529"},
		{id: "L-IRA", account: "CASH", kind: "withdrawal", amount: -100,
			spend: "internal_transfer", farAccount: "IRA"},
		{id: "L-UNPLACED", account: "CASH", kind: "withdrawal", amount: -100},
	})

	rows, err := db.QueryContext(ctx, `
        SELECT transaction_external_id, class_label, group_label
          FROM cashflow_txn_nodes(0, 3500000) WHERE disposition = 'line'`)
	if err != nil {
		t.Fatalf("read labels: %v", err)
	}
	defer rows.Close()
	got := map[string]string{}
	for rows.Next() {
		var id, class, grp string
		if err := rows.Scan(&id, &class, &grp); err != nil {
			t.Fatalf("scan labels: %v", err)
		}
		got[id] = class + " / " + grp
	}
	for id, want := range map[string]string{
		"L-WAGE": "Earnings / Wages",
		// The class says what the money was in and the group what it
		// did, so a direct holding bought on an exchange reads as a
		// trade even inside the private-markets class.
		"L-BUY":       "Private markets / Trades",
		"L-MORT":      "Mortgage / Mortgage",
		"L-UNTRACKED": "Untracked accounts / Untracked accounts",
		"L-DAF":       "Giving / To giving vehicles",
		"L-PLAN":      "Education savings / Education savings",
		"L-IRA":       "Retirement savings / Retirement savings",
		"L-UNPLACED":  "Uncategorised / Uncategorised",
	} {
		if got[id] != want {
			t.Errorf("%s labels = %q, want %q", id, got[id], want)
		}
	}
}

// TestMigration0081DDLIsRerunnable holds the resolution to the replay
// bar: three OR REPLACE macros and nothing else.
func TestMigration0081DDLIsRerunnable(t *testing.T) {
	db, ctx := openMigrated(t)
	seedResolutionFixture(t, db, ctx)
	seedLines(t, db, ctx, []line{
		{id: "R-WAGE", account: "CASH", kind: "deposit", amount: 100, income: "INCOME_WAGES"},
	})
	rerunMigrationDDL(t, db, ctx, "0081_cashflow_resolution.sql")
	// A downgrade, not a no-op: 0081 puts both macros back at their
	// pre-0085 shape. Replay forward, exactly as Migrate would — and
	// extend this list when another migration re-issues them.
	rerunMigrationDDL(t, db, ctx, "0085_cashflow_resolution_fixes.sql")
	if _, ok := macroTxnIDs(t, db, ctx, "cashflow_lines_base", 0, 3500000)["R-WAGE"]; !ok {
		t.Error("the replayed base lost a line")
	}
}

// TestNoVerdictPromotesAnUnsignedKind pins the RULE rather than the one
// instance that exposed it: the ladder tests the kinds gold pins no
// canonical sign for before it reads any verdict, so a pin or a rule
// cannot pull such a row out of the excluded set and into a section
// that reads direction.
//
// Every canonical kind is seeded once, each carrying the strongest
// verdict a tier can write — `internal_transfer` with a far account
// inside a retirement plan, which places a row in `vehicles` — so a
// kind that declines to be promoted declines on the kind alone. The six
// that must decline are exactly canonical.canonicalSign's zero-signed
// set minus `interest`, `staking` and `capital_gain`: the ladder reads
// interest BY its sign, and a negative staking or capital gain nets
// inside its own class rather than choosing a side of the diagram.
func TestNoVerdictPromotesAnUnsignedKind(t *testing.T) {
	db, ctx := openMigrated(t)
	seedResolutionFixture(t, db, ctx)

	unsigned := map[string]bool{
		"fx": true, "fx_forward": true, "fx_swap": true,
		"corporate_action": true, "journal": true, "other": true,
	}
	var rows []line
	for _, k := range []string{
		"buy", "sell", "dividend", "coupon", "capital_gain", "interest",
		"staking", "contribution", "distribution", "fee", "tax", "deposit",
		"withdrawal", "purchase", "refund", "card_payment", "reward",
		"fx", "fx_forward", "fx_swap", "corporate_action",
		"transfer_in", "transfer_out", "journal", "other",
	} {
		rows = append(rows, line{
			id: "U-" + k, account: "CASH", kind: k, amount: -100,
			spend: "internal_transfer", farAccount: "IRA",
		})
	}
	got := seedLines(t, db, ctx, rows)

	for _, l := range rows {
		kind := strings.TrimPrefix(l.id, "U-")
		node := got[l.id]
		if unsigned[kind] {
			if node != "excluded" {
				t.Errorf("a pin promoted the unsigned kind %q to %q; gold pins no sign for it",
					kind, node)
			}
			continue
		}
		if node == "excluded" {
			t.Errorf("kind %q refused a verdict it should have taken", kind)
		}
	}
}

// TestElsewhereIsForARowWithNoInstrument pins what "Untracked
// investments" means. A row that names an instrument is tracked — a
// missing dimension row or an unset asset class is a gap IN the
// instrument — so it belongs in `other`, where the gap is visible as
// what it is. `elsewhere` keeps the rows that name no instrument at
// all, which is what the `investment` and `capital_return` verdicts
// place.
// gold's `instruments.asset_class` is NOT NULL, so a null class on a
// joined row means one thing only: the dimension row is absent. That is
// the case this pins — and it is why the arm tests the row's OWN
// instrument_external_id rather than the joined class.
func TestElsewhereIsForARowWithNoInstrument(t *testing.T) {
	db, ctx := openMigrated(t)
	seedResolutionFixture(t, db, ctx)

	got := seedLines(t, db, ctx, []line{
		{id: "E-MISSING", account: "BROK", kind: "buy", amount: -300, instrument: "GHOST",
			want: "investing.other.trades",
			why:  "an instrument whose dimension row is absent is still a tracked holding"},
		{id: "E-DEPLOYED", account: "CASH", kind: "withdrawal", amount: -900, spend: "investment",
			want: "investing.elsewhere.investment",
			why:  "capital deployed where the product holds nothing names no instrument"},
	})
	for _, id := range []string{"E-MISSING", "E-DEPLOYED"} {
		if got[id] == "" {
			t.Errorf("%s resolved to nothing", id)
		}
	}
}

// TestMigration0085DDLIsRerunnable holds the corrected resolution to the
// replay bar: two OR REPLACE macros and nothing else.
func TestMigration0085DDLIsRerunnable(t *testing.T) {
	db, ctx := openMigrated(t)
	seedResolutionFixture(t, db, ctx)
	seedLines(t, db, ctx, []line{
		{id: "R85-WAGE", account: "CASH", kind: "deposit", amount: 100, income: "INCOME_WAGES"},
	})
	rerunMigrationDDL(t, db, ctx, "0085_cashflow_resolution_fixes.sql")
	if _, ok := macroTxnIDs(t, db, ctx, "cashflow_lines_base", 0, 3500000)["R85-WAGE"]; !ok {
		t.Error("the replayed base lost a line")
	}
}

// TestAConsumptionLeafIsItsPrimaryUnlessThePrimarySaysNothing pins the
// one exception to "the outflow leaves are the spending PRIMARIES".
//
// The rule earns its keep: ninety detailed values would make a diagram
// nobody can read. But `GENERAL_SERVICES` is a catch-all rather than a
// category, and the value filed under it for school fees is a bigger
// line in most households than several primaries that do get an edge of
// their own. So that one value is promoted, and its neighbours are not.
func TestAConsumptionLeafIsItsPrimaryUnlessThePrimarySaysNothing(t *testing.T) {
	db, ctx := openMigrated(t)
	seedResolutionFixture(t, db, ctx)

	lines := []line{
		{id: "L-SCHOOL", account: "CASH", kind: "withdrawal", amount: -9000,
			spend: "GENERAL_SERVICES_EDUCATION",
			want:  "operating_out.consumption.GENERAL_SERVICES_EDUCATION",
			why:   "school fees are comparable to a home improvement, not to dry cleaning"},
		{id: "L-SIBLING", account: "CASH", kind: "withdrawal", amount: -60,
			spend: "GENERAL_SERVICES_OTHER_GENERAL_SERVICES",
			want:  "operating_out.consumption.GENERAL_SERVICES",
			why:   "everything else under the catch-all still groups by primary"},
		{id: "L-HOME", account: "CASH", kind: "withdrawal", amount: -4000,
			spend: "HOME_IMPROVEMENT_REPAIR_AND_MAINTENANCE",
			want:  "operating_out.consumption.HOME_IMPROVEMENT",
			why:   "a primary that describes its members is left alone"},
	}
	check(t, seedLines(t, db, ctx, lines), lines)
}

// ---- migration 0102: the holder may say what the capital went into ------

// TestAStatedExposureNamesTheInvestingClass walks the whole of 0102.
// The investing class is otherwise the instrument's, and a bank payment
// order names none — so capital deployed into a destination the product
// holds, in a source that carries no transactions to pair against, drew
// as `elsewhere`: "Untracked investments", for a book gold holds.
//
// The GROUP is asserted alongside the class on every row, and that is
// the point of asserting the whole triple: until 0102 the verdict
// reached the group only while the class was still `elsewhere`, so
// giving a row its class would have cost it its group and a house
// purchase would have read `Real estate · Private capital`.
func TestAStatedExposureNamesTheInvestingClass(t *testing.T) {
	db, ctx := openMigrated(t)
	seedResolutionFixture(t, db, ctx)
	lines := []line{
		{id: "X-HOUSE", account: "CASH", kind: "withdrawal", amount: -250000,
			spend: "investment", spendClass: "real_estate",
			want: "investing.real_estate.investment",
			why:  "a payment order that bought a property the product holds elsewhere"},
		{id: "X-STAKE", account: "CASH", kind: "withdrawal", amount: -50000,
			spend: "investment", spendClass: "private_equity",
			want: "investing.private_equity.investment",
			why:  "a private-markets commitment, by the same road"},
		{id: "X-BACK", account: "CASH", kind: "deposit", amount: 9000,
			income: "capital_return", incomeClass: "private_debt",
			want: "investing.private_debt.capital_return",
			why:  "the return leg, stated on the income side, keeps its own group"},
		{id: "X-SILENT", account: "CASH", kind: "withdrawal", amount: -7000,
			spend: "investment",
			want:  "investing.elsewhere.investment",
			why:   "a deployment nobody named still means what elsewhere says"},
	}
	check(t, seedLines(t, db, ctx, lines), lines)
}

// TestAStatedExposureYieldsToEveryRoadAboveIt pins the ordering the
// class arm exists to express: the FEED first, the instrument second,
// the holder last — and nothing at all once the movement stops being a
// deployment.
func TestAStatedExposureYieldsToEveryRoadAboveIt(t *testing.T) {
	db, ctx := openMigrated(t)
	seedResolutionFixture(t, db, ctx)
	if _, err := db.ExecContext(ctx, `
        INSERT INTO transactions (silver_source_id, transaction_external_id, occurred_at,
                                  account_external_id, kind, currency, net_amount, asset_class)
             VALUES ('cf', 'X-FEED', 1728000, 'CASH', 'withdrawal', 'USD', -1000, 'metal')`); err != nil {
		t.Fatalf("seed feed-stated row: %v", err)
	}
	if _, err := db.ExecContext(ctx, `
        INSERT INTO spend_txn_enrichment (silver_source_id, transaction_external_id,
                merchant_signature, signature_version, spend_detailed, provenance,
                stated_asset_class, assigned_at)
             VALUES ('cf', 'X-FEED', 'sig', 1, 'investment', 'rule', 'real_estate', 100)`); err != nil {
		t.Fatalf("seed feed-stated overlay: %v", err)
	}
	lines := []line{
		// A named instrument whose dimension row is MISSING must still
		// fall to `other`. gold's instruments.asset_class is NOT NULL,
		// so a null on the join means the row is absent — and a stated
		// exposure read any earlier would hide that hole behind a
		// plausible class. This is the case that decided the term's
		// placement (beside TestElsewhereIsForARowWithNoInstrument).
		{id: "X-GAP", account: "CASH", kind: "withdrawal", amount: -2000,
			instrument: "NO-SUCH", spend: "investment", spendClass: "real_estate",
			want: "investing.other.investment",
			why:  "a dimension gap stays visible as one"},
		// The holder may not mint the statement's residual node: `cash`
		// is labelled "Cash savings", which the Sankey already uses.
		// Refused at config load AND here, so the guard is structural.
		{id: "X-CASH", account: "CASH", kind: "withdrawal", amount: -3000,
			spend: "investment", spendClass: "cash",
			want: "investing.elsewhere.investment",
			why:  "a stated `cash` is ignored rather than drawn"},
		// The matcher's verdict replaces the rule's, and the pass
		// clears the exposure with it; seeded here as the overlay the
		// pass would have written, to pin the reader's half.
		{id: "X-PAIRED", account: "CASH", kind: "withdrawal", amount: -4000,
			spend: "internal_transfer", spendClass: "real_estate", farAccount: "BROK",
			want: "internal", why: "an own-account move inside the pool draws nothing"},
	}
	got := seedLines(t, db, ctx, lines)
	check(t, got, lines)
	if got["X-FEED"] != "investing.metal.investment" {
		t.Errorf("X-FEED = %s, want investing.metal.investment — the feed's own word outranks the holder's",
			got["X-FEED"])
	}
}

// TestTheExposureTravelsWithTheVerdictThatWon pins the one thing a flat
// COALESCE would have got wrong. The node macro picks the VERDICT by
// kind — income first on an inflow — so the exposure has to be picked
// the same way, or a row carrying both overlays can show the class of
// the family whose verdict lost.
func TestTheExposureTravelsWithTheVerdictThatWon(t *testing.T) {
	db, ctx := openMigrated(t)
	seedResolutionFixture(t, db, ctx)
	lines := []line{
		{id: "X-BOTH", account: "CASH", kind: "deposit", amount: 12000,
			spend: "investment", spendClass: "real_estate",
			income: "capital_return", incomeClass: "private_debt",
			want: "investing.private_debt.capital_return",
			why:  "a deposit reads income's verdict, so it must read income's exposure"},
	}
	check(t, seedLines(t, db, ctx, lines), lines)
}

// TestMigration0101DDLIsRerunnable pins the contract 0101's own header
// claims and no test held it to: three macros, an INSERT OR REPLACE and
// an UPDATE guarded on a NULL, all replayable against a database that
// already has them.
func TestMigration0101DDLIsRerunnable(t *testing.T) {
	db, ctx := openMigrated(t)
	rerunMigrationDDL(t, db, ctx, "0101_cashflow_mortgage_transfer.sql")
}

// TestMigration0102DDLIsRerunnable pins the replay contract: both
// ALTERs are IF NOT EXISTS and the macro is OR REPLACE, so applying the
// file twice is a no-op rather than an error.
func TestMigration0102DDLIsRerunnable(t *testing.T) {
	db, ctx := openMigrated(t)
	rerunMigrationDDL(t, db, ctx, "0102_cashflow_stated_exposure.sql")
}
