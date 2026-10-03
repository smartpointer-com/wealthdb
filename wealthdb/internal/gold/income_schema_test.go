package gold

import (
	"context"
	"database/sql"
	"testing"
)

// seedIncomeFixture lays down the accounts, instruments, transactions
// and overlay rows the income macro tests read. Every row is there to
// pin one rule of the population, the floor or the payer resolution;
// the comments say which.
//
// Payers and instruments are invented. A real one never appears in a
// fixture: the income side is where employers, agencies and tenants
// would be, and a fixture is a tracked file.
func seedIncomeFixture(t *testing.T, db *sql.DB, ctx context.Context) {
	t.Helper()
	if _, err := db.ExecContext(ctx, `
        INSERT INTO accounts (silver_source_id, account_external_id, account_kind,
                              display_name, first_seen_at, last_seen_at) VALUES
            ('inc-src', 'CASH1', 'cash',      'Everyday',  1, 1),
            ('inc-src', 'BRK1',  'brokerage', 'Brokerage', 1, 1),
            ('inc-src', 'CUST1', 'custody',   'Custody',   1, 1),
            ('inc-src', 'CASH2', 'cash',      'Opted out', 1, 1);

        -- The scope is income's own, and takes an account out the way
        -- spending's does. Since the default is include, the exclude is
        -- what carries the proof.
        INSERT INTO income_account_scope (silver_source_id, account_external_id, mode) VALUES
            ('inc-src', 'CASH2', 'exclude');

        -- One instrument that knows its name, one that knows only its
        -- symbol: the payer step falls through from the first to the
        -- second.
        INSERT INTO instruments (silver_source_id, instrument_external_id, asset_class,
                                 symbol, name, first_seen_at, last_seen_at) VALUES
            ('inc-src', 'INST-NAMED',  'equity', 'EXDC', 'Example Dividend Corp', 1, 1),
            ('inc-src', 'INST-SYMBOL', 'equity', 'EXSY', NULL,                    1, 1),
            ('inc-src', 'INST-BLANK',  'equity', NULL,   NULL,                    1, 1),
            ('inc-src', 'INST-CHAIN',  'crypto', 'EXCH', 'Example Chain Protocol', 1, 1);

        INSERT INTO transactions (silver_source_id, transaction_external_id, occurred_at,
                                  account_external_id, instrument_external_id,
                                  kind, currency, net_amount) VALUES
            -- The income kinds, both signs: a negative row of an income
            -- kind is a reversal and nets inside its own type.
            ('inc-src', 'T-DIVIDEND',  1000, 'BRK1',  'INST-NAMED',  'dividend',     'USD',   40),
            ('inc-src', 'T-DIV-NEG',   1000, 'BRK1',  'INST-NAMED',  'dividend',     'USD',  -10),
            ('inc-src', 'T-DIV-SYMBOL',1000, 'BRK1',  'INST-SYMBOL', 'dividend',     'USD',   11),
            ('inc-src', 'T-DIV-STORE', 1000, 'BRK1',  'INST-NAMED',  'dividend',     'USD',   45),
            ('inc-src', 'T-COUPON',    1000, 'BRK1',  'INST-NAMED',  'coupon',       'USD',   15),
            ('inc-src', 'T-STAKING',   1000, 'CUST1', 'INST-CHAIN',  'staking',      'USD',    2),
            ('inc-src', 'T-CAPGAIN',   1000, 'BRK1',  'INST-NAMED',  'capital_gain', 'USD',   25),
            -- A realised-gain payout whose signature the payer store
            -- has an answer for, and a different one from the floor's.
            ('inc-src', 'T-GAIN-STORE',1000, 'BRK1',  'INST-NAMED',  'capital_gain', 'USD',   35),
            ('inc-src', 'T-REWARD',    1000, 'CASH1', NULL,          'reward',       'USD',    3),
            -- A private fund's distribution, and the same kind promoted
            -- by a pin to the exception it names.
            ('inc-src', 'T-DISTRIB',   1000, 'BRK1',  'INST-NAMED',  'distribution', 'USD',  500),
            ('inc-src', 'T-DIST-PIN',  1000, 'BRK1',  'INST-NAMED',  'distribution', 'USD',  600),
            -- interest is the ONE kind with a sign guard: one
            -- canonical kind carries interest credited and interest
            -- charged, and the sign is all that tells them apart.
            ('inc-src', 'T-INT-POS',   1000, 'CASH1', NULL,          'interest',     'USD',    5),
            ('inc-src', 'T-INT-NEG',   1000, 'CASH1', NULL,          'interest',     'USD',  -10),
            ('inc-src', 'T-DEPOSIT',   1000, 'CASH1', NULL,          'deposit',      'USD',  100),
            ('inc-src', 'T-DEP-NEG',   1000, 'CASH1', NULL,          'deposit',      'USD',  -50),
            -- Kinds that are somebody else's question.
            ('inc-src', 'T-REFUND',    1000, 'CASH1', NULL,          'refund',       'USD',   20),
            ('inc-src', 'T-SELL',      1000, 'BRK1',  'INST-NAMED',  'sell',         'USD',  900),
            ('inc-src', 'T-CONTRIB',   1000, 'BRK1',  'INST-NAMED',  'contribution', 'USD',   30),
            ('inc-src', 'T-XFER-IN',   1000, 'BRK1',  'INST-NAMED',  'transfer_in',  'USD',  300),
            ('inc-src', 'T-CARDPAY',   1000, 'CASH1', NULL,          'card_payment', 'USD',  200),
            ('inc-src', 'T-OTHER',     1000, 'CASH1', NULL,          'other',        'USD',   12),
            ('inc-src', 'T-JOURNAL',   1000, 'CASH1', NULL,          'journal',      'USD',   12),
            -- Account scope, and the window.
            ('inc-src', 'T-OPTOUT',    1000, 'CASH2', NULL,          'deposit',      'USD',   80),
            ('inc-src', 'T-LATE',      9000, 'BRK1',  'INST-NAMED',  'dividend',     'USD',   55),
            -- Deposits, which are the one kind with no floor and
            -- therefore the one the narrative tiers decide.
            ('inc-src', 'T-WIRE',      1000, 'CASH1', NULL,          'deposit',      'USD',  700),
            ('inc-src', 'T-SIGONLY',   1000, 'CASH1', NULL,          'deposit',      'USD',   50),
            ('inc-src', 'T-XFER',      1000, 'CASH1', NULL,          'deposit',      'USD',  400),
            ('inc-src', 'T-GIFT',      1000, 'CASH1', NULL,          'deposit',      'USD',  250),
            ('inc-src', 'T-RETURNED',  1000, 'CASH1', NULL,          'deposit',      'USD',  350),
            ('inc-src', 'T-BORROWED',  1000, 'CASH1', NULL,          'deposit',      'USD',  900),
            ('inc-src', 'T-CLAIM',     1000, 'CASH1', NULL,          'deposit',      'USD',   60),
            ('inc-src', 'T-ESTATE',    1000, 'CASH1', NULL,          'deposit',      'USD', 1200),
            ('inc-src', 'T-COUNTER',   1000, 'CASH1', NULL,          'deposit',      'USD',   75),
            ('inc-src', 'T-UNPLACED',  1000, 'CASH1', NULL,          'deposit',      'USD',   90),
            -- A row the pass has not reached: loaded after the last
            -- enrichment run, so no overlay row exists for it yet.
            ('inc-src', 'T-UNSEEN',    1000, 'BRK1',  'INST-NAMED',  'dividend',     'USD',   70),
            -- An instrument that names nothing, on a row whose source
            -- did give a narrative: the payer falls through to it.
            ('inc-src', 'T-DIV-NONAME',1000, 'BRK1',  'INST-BLANK',  'dividend',     'USD',    8),
            -- A deposit carrying the SAME signature as the capital_gain
            -- row above. This is the shape the ordering turns on: one
            -- payer, two kinds, and a single store verdict behind both.
            ('inc-src', 'T-DEP-SHARED',1000, 'CASH1', NULL,          'deposit',      'USD',  150),
            -- A deposit carrying no amount, and one carrying zero.
            -- net_amount is nullable and the deposit kind has no amount
            -- guard of its own, so both are rows of the base that
            -- contribute nothing to a sum.
            ('inc-src', 'T-DEP-NOAMT', 1000, 'CASH1', NULL,          'deposit',      'USD',  NULL),
            ('inc-src', 'T-DEP-ZERO',  1000, 'CASH1', NULL,          'deposit',      'USD',    0),
            -- Interest that moved nothing: it satisfies neither
            -- family's sign guard and is in neither population.
            ('inc-src', 'T-INT-ZERO',  1000, 'CASH1', NULL,          'interest',     'USD',    0);

        INSERT INTO income_txn_enrichment (silver_source_id, transaction_external_id,
                                           payer_signature, signature_version,
                                           income_detailed, provenance, assigned_at) VALUES
            -- The floor's rows. The pass records every population row it
            -- reaches, whether or not it could place it — which is what
            -- gives the floor something to floor. The first nine carry
            -- NO signature: a dividend, a coupon, a staking reward and a
            -- fund distribution arrive with an instrument and no
            -- narrative to fold, and a rewards credit and credited
            -- interest arrive with neither.
            ('inc-src', 'T-DIVIDEND',  NULL,          1, NULL,                     'signature-only', 100),
            ('inc-src', 'T-DIV-NEG',   NULL,          1, NULL,                     'signature-only', 100),
            ('inc-src', 'T-DIV-SYMBOL',NULL,          1, NULL,                     'signature-only', 100),
            ('inc-src', 'T-COUPON',    NULL,          1, NULL,                     'signature-only', 100),
            ('inc-src', 'T-STAKING',   NULL,          1, NULL,                     'signature-only', 100),
            ('inc-src', 'T-CAPGAIN',   NULL,          1, NULL,                     'signature-only', 100),
            ('inc-src', 'T-REWARD',    NULL,          1, NULL,                     'signature-only', 100),
            ('inc-src', 'T-DISTRIB',   NULL,          1, NULL,                     'signature-only', 100),
            ('inc-src', 'T-INT-POS',   NULL,          1, NULL,                     'signature-only', 100),
            -- ...and one that does carry a narrative to fold, because
            -- gold holds its instrument under neither a name nor a
            -- symbol.
            ('inc-src', 'T-DIV-NONAME','EXAMPLE DEPOT',1, NULL,                    'signature-only', 100),
            -- A deposit: the one kind with no floor, so its signature is
            -- the only thing any tier has to go on.
            ('inc-src', 'T-DEPOSIT',   'sig-unknown', 1, NULL,                     'signature-only', 100),
            ('inc-src', 'T-GAIN-STORE','sig-gains',   1, NULL,                     'signature-only', 100),
            ('inc-src', 'T-DEP-SHARED','sig-gains',   1, NULL,                     'signature-only', 100),
            -- Interest CHARGED. The pass records every population row it
            -- reaches, so this one has an enrichment row too; what must
            -- not happen is the floor placing it, because the floor's
            -- sign guard is what keeps a finance charge out of interest
            -- earned.
            ('inc-src', 'T-INT-NEG',   'sig-margin',  1, NULL,                     'signature-only', 100),
            -- A deposit reversal, carrying the signature of the
            -- booking it corrects so the two net inside one type.
            ('inc-src', 'T-DEP-NEG',   'sig-payroll', 1, NULL,                     'signature-only', 100),
            -- A pin promotes one private-fund distribution to the
            -- income it turned out to be; its unpinned twin floors to
            -- capital_return and leaves the base.
            ('inc-src', 'T-DIST-PIN', 'sig-fund',    1, 'INCOME_INTEREST_EARNED', 'manual',         100),
            -- The matcher's verdict, written on the receiving leg by
            -- the same pass that wrote the paying one.
            ('inc-src', 'T-XFER',     'sig-own',     1, 'internal_transfer',      'matcher',        100),
            ('inc-src', 'T-GIFT',     'sig-person',  1, 'gift',                   'manual',         100),
            ('inc-src', 'T-RETURNED', 'sig-loan',    1, 'capital_return',         'manual',         100),
            ('inc-src', 'T-BORROWED', 'sig-lender',  1, 'loan_proceeds',          'rule',           100),
            ('inc-src', 'T-CLAIM',    'sig-insurer', 1, 'reimbursement',          'rule',           100),
            -- The three deltas that are receipts in their own right and
            -- stay in the base, beside the gift above.
            ('inc-src', 'T-ESTATE',   'sig-estate',  1, 'inheritance',            'manual',         100),
            ('inc-src', 'T-COUNTER',  'sig-counter', 1, 'cash_deposit',           'rule',           100),
            ('inc-src', 'T-UNPLACED', 'sig-nobody',  1, 'other',                  'manual',         100),
            -- Placed by nothing: the store answers for one, and the
            -- other is the backlog the model tier works from.
            ('inc-src', 'T-WIRE',     'sig-payroll', 1, NULL,                     'signature-only', 100),
            ('inc-src', 'T-SIGONLY',  '  SAMPLE TAX OFFICE  ', 1, NULL,           'signature-only', 100),
            -- An instrument-bearing row whose signature ALSO has a
            -- store verdict: the instrument still names the payer.
            ('inc-src', 'T-DIV-STORE','sig-admin',   1, NULL,                     'signature-only', 100);

        INSERT INTO income_payer_categories (payer_signature, payer_name, income_detailed,
                                             signature_version, assigned_at, model_name) VALUES
            ('sig-payroll', 'Blue Harbour Payroll',       'INCOME_SALARY',       1, 100, 'test-model'),
            ('sig-admin',   'Example Fund Administrator', 'INCOME_DIVIDENDS',    1, 100, 'test-model'),
            -- Disagrees with the capital_gain floor, which says
            -- INCOME_DISTRIBUTIONS. Two rows carry this signature: the
            -- capital_gain, where the floor overrules the store, and a
            -- deposit, where there is no floor and the store is the
            -- only answer. One verdict, two outcomes (migration 0073).
            ('sig-gains',   'Example Gain Payer',         'INCOME_DIVIDENDS',    1, 100, 'test-model'),
            -- Disagrees with the overlay verdict pinned on T-DIST-PIN,
            -- which is read before either.
            ('sig-fund',    'Example Fund',               'INCOME_OTHER',        1, 100, 'test-model');
    `); err != nil {
		t.Fatalf("seed income fixture: %v", err)
	}
}

// TestIncomeLinesBasePopulation pins the population definition: which
// transaction kinds are income, that seven of the eight carry both
// signs while `interest` alone enters positive only, that
// income_account_scope fences an account out, and that a row leaves the
// base when what it resolves to is not income after all.
func TestIncomeLinesBasePopulation(t *testing.T) {
	db, ctx := openMigrated(t)
	seedIncomeFixture(t, db, ctx)

	got := macroTxnIDs(t, db, ctx, "income_lines_base", 0, 5000)
	want := map[string]string{
		"T-DIVIDEND":   "a dividend is income",
		"T-DIV-NEG":    "a dividend clawed back is a reversal, and nets inside its own type",
		"T-DIV-SYMBOL": "a dividend is a dividend whatever the instrument is called",
		"T-DIV-STORE":  "a dividend whose signature also has a store verdict agreeing with the floor",
		"T-COUPON":     "a bond coupon is interest earned",
		"T-STAKING":    "a staking reward is income",
		"T-CAPGAIN":    "a fund's payout of realised gains returns no basis",
		"T-REWARD":     "a rewards credit is income (migration 0070)",
		"T-DIST-PIN":   "a distribution a pin proved to be income",
		"T-INT-POS":    "credited interest is income",
		"T-DEPOSIT":    "a deposit nothing placed stays visible as the backlog",
		"T-WIRE":       "a deposit the payer store placed",
		"T-SIGONLY":    "a deposit nothing placed, carrying only its signature",
		"T-GIFT":       "a gift received is income to the household, with no payer behind it",
		"T-UNSEEN":     "a row the pass has not reached yet is shown, uncategorised, not dropped",
		"T-GAIN-STORE": "a realised-gain payout, placed by its kind over the store verdict on its payer",
		"T-DEP-SHARED": "a deposit sharing that payer's signature, placed by the store because no floor claims a deposit",
		"T-DEP-NEG":    "a negative deposit is a reversal by construction, and nets inside the type of the booking it corrects",
		"T-DEP-ZERO":   "a deposit is an ordinary income kind now: no amount guard, as on the other unguarded kinds",
		"T-DEP-NOAMT":  "likewise a booking with no amount — it counts as a row and contributes nothing to a sum",
		"T-ESTATE":     "an estate's distribution is a receipt in its own right",
		"T-COUNTER":    "cash paid in over a counter is a receipt whose origin is unobservable",
		"T-UNPLACED":   "`other` is what a receipt nothing could place reads as, and it is charted",
		"T-DIV-NONAME": "a dividend is a dividend whether or not gold can name the instrument",
	}
	excluded := map[string]string{
		"T-DISTRIB":  "a private fund's distribution returns contributed capital until something proves otherwise",
		"T-XFER":     "the receiving leg of an own-account move, from the shared matcher",
		"T-RETURNED": "capital of the holder's own coming back",
		"T-BORROWED": "money borrowed is a liability incurred, not income",
		"T-CLAIM":    "money back for money spent is not income",
		"T-INT-NEG":  "interest charged is a finance cost, and is spending's",

		"T-INT-ZERO": "interest that moved nothing satisfies neither family's guard",
		"T-REFUND":   "a merchant credit nets inside its category on the spending side",
		"T-SELL":     "sale proceeds are the holder's own capital, and are the cashflow feature's",
		"T-CONTRIB":  "an over-subscribed commitment refunded is own capital back",
		"T-XFER-IN":  "a securities transfer is not a receipt",
		"T-CARDPAY":  "the card-side leg of a bill is an own-account move by kind",
		"T-OTHER":    "the `other` kind carries no reliable sign",
		"T-JOURNAL":  "a bookkeeping entry is not a receipt",
		"T-OPTOUT":   "income_account_scope fences an account out",
		"T-LATE":     "outside the window",
	}
	for id, why := range want {
		if _, ok := got[id]; !ok {
			t.Errorf("%s missing from income_lines_base (%s)", id, why)
		}
	}
	for id, why := range excluded {
		if _, ok := got[id]; ok {
			t.Errorf("%s present in income_lines_base (%s)", id, why)
		}
	}
	if len(got) != len(want) {
		t.Errorf("income_lines_base returned %d rows, want %d", len(got), len(want))
	}

	// The floor reads the overlay, not the transaction: a row no pass
	// has recorded has nothing to floor, so it reaches the base
	// uncategorised rather than as the dividend its kind says it is.
	// Transient by construction — the pass runs inside `load`, so a row
	// is enriched in the same run that loads it — but the base must
	// show it either way rather than hide a row it cannot place.
	var detailed, provenance sql.NullString
	if err := db.QueryRowContext(ctx, `
        SELECT income_detailed, provenance FROM income_lines_base(0, 5000)
         WHERE transaction_external_id = 'T-UNSEEN'`).Scan(&detailed, &provenance); err != nil {
		t.Fatalf("read T-UNSEEN: %v", err)
	}
	if detailed.Valid || provenance.Valid {
		t.Errorf("T-UNSEEN = (%v, %v), want both NULL: the floor has no overlay row to read",
			detailed, provenance)
	}

	// The `reward` move, asserted from both sides: the kind is income's
	// now, and the spending population it sat in since migration 0041
	// no longer holds it. Both accounts here are in spending's scope
	// too — nothing excludes them — so its absence there is the move
	// and not a scope artefact.
	spending := macroTxnIDs(t, db, ctx, "spend_enrichment_population", 0, 5000)
	if _, ok := spending["T-REWARD"]; ok {
		t.Error("T-REWARD is still in the spending population; a rewards credit is never netted against card spend")
	}
	if _, ok := spending["T-REFUND"]; !ok {
		t.Error("T-REFUND left the spending population: the reward move must take one kind, not its neighbours")
	}
}

// TestIncomePopulationLayering pins the relationship between the two
// income macros: the enrichment pass sees a strictly broader set than
// the base charts, because the pass is what decides the four
// exclusions and cannot read a population that has already applied
// them.
func TestIncomePopulationLayering(t *testing.T) {
	db, ctx := openMigrated(t)
	seedIncomeFixture(t, db, ctx)

	base := macroTxnIDs(t, db, ctx, "income_lines_base", 0, 5000)
	population := macroTxnIDs(t, db, ctx, "income_enrichment_population", 0, 5000)

	for id := range base {
		if _, ok := population[id]; !ok {
			t.Errorf("%s is in income_lines_base but not in the enrichment population", id)
		}
	}
	// The base is the population less exactly the rows that resolved to
	// one of the four excluded values.
	dropped := []string{"T-DISTRIB", "T-XFER", "T-RETURNED", "T-BORROWED", "T-CLAIM"}
	for _, id := range dropped {
		if _, ok := population[id]; !ok {
			t.Errorf("%s missing from the enrichment population; the pass cannot re-decide a row it never sees", id)
		}
	}
	if len(population) != len(base)+len(dropped) {
		t.Errorf("enrichment population = %d rows, want %d (the base plus its %d exclusions)",
			len(population), len(base)+len(dropped), len(dropped))
	}

	// The kind, sign and scope rules the population shares with the base.
	for id, why := range map[string]string{
		"T-INT-NEG":  "interest charged is spending's",
		"T-INT-ZERO": "interest that moved nothing satisfies neither family's guard",

		"T-REFUND": "a merchant credit is spending's",
		"T-SELL":   "sale proceeds are the cashflow feature's",
		"T-OTHER":  "the `other` kind carries no reliable sign",
		"T-OPTOUT": "income_account_scope fences an account out",
		"T-LATE":   "outside the window",
	} {
		if _, ok := population[id]; ok {
			t.Errorf("%s reached the enrichment population (%s)", id, why)
		}
	}

	var scoped int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM income_scoped_accounts()`).Scan(&scoped); err != nil {
		t.Fatalf("count income_scoped_accounts: %v", err)
	}
	if scoped != 3 {
		t.Errorf("income_scoped_accounts = %d, want 3 (every seeded account, minus the one fenced out)", scoped)
	}
}

// incomeResolution reads one row of income_txn_categories().
type incomeResolution struct {
	detailed, primary, label, primaryLabel, payer sql.NullString
	provenance                                    string
}

func readIncomeResolution(t *testing.T, db *sql.DB, ctx context.Context, id string) incomeResolution {
	t.Helper()
	var r incomeResolution
	err := db.QueryRowContext(ctx, `
        SELECT income_detailed, income_primary, income_label, income_primary_label,
               payer_name, provenance
          FROM income_txn_categories()
         WHERE transaction_external_id = ?`, id).
		Scan(&r.detailed, &r.primary, &r.label, &r.primaryLabel, &r.payer, &r.provenance)
	if err != nil {
		t.Fatalf("read %s: %v", id, err)
	}
	return r
}

// TestIncomeKindFloorPlacesWhatNothingElseCould pins the arm of the
// resolution that carries most of the income side.
//
// A dividend, a coupon, a staking reward and a fund distribution arrive
// with an instrument and no narrative — there is no payee for a rule to
// key on, and nothing for a model to read. The transaction's KIND says
// what the row is, and each adapter derives that from whatever evidence
// its own source gives, so the floor reads that verdict rather than
// re-deriving it from prose.
//
// The floor sits OVER the payer store on this side (migration 0073),
// which is where the two families part company: spending's floor sits
// UNDER its merchant store, because there the model names a merchant
// and a name is finer than a kind. Here the model is asked what kind of
// income a receipt is, and the kind answers that itself. One of the
// floor's arms also places a DELTA — the only floor in either family
// that does.
func TestIncomeKindFloorPlacesWhatNothingElseCould(t *testing.T) {
	db, ctx := openMigrated(t)
	seedIncomeFixture(t, db, ctx)

	for id, want := range map[string][2]string{
		"T-DIVIDEND": {"INCOME_DIVIDENDS", "kind"},
		"T-DIV-NEG":  {"INCOME_DIVIDENDS", "kind"},
		"T-COUPON":   {"INCOME_INTEREST_EARNED", "kind"},
		"T-STAKING":  {"INCOME_STAKING", "kind"},
		"T-CAPGAIN":  {"INCOME_DISTRIBUTIONS", "kind"},
		"T-REWARD":   {"INCOME_REWARDS", "kind"},
		"T-INT-POS":  {"INCOME_INTEREST_EARNED", "kind"},
		// The floor that places a delta: a private fund's distribution
		// is contributed capital coming back until something proves
		// otherwise.
		"T-DISTRIB": {"capital_return", "kind"},
		// ...and the pin that proves it, on the same kind. A verdict
		// outranks the floor, and the row stays in the base.
		"T-DIST-PIN": {"INCOME_INTEREST_EARNED", "manual"},
		// The one kind with no floor. It is also the one the narrative
		// tiers exist for, so leaving it unplaced is the point: an
		// unplaced deposit is shown as uncategorised, never guessed at.
		"T-DEPOSIT": {"", "signature-only"},
		"T-SIGONLY": {"", "signature-only"},
		// The floor read OVER the store (migration 0073), on rows
		// where the two disagree. The model is asked what kind of
		// income a receipt is, and on a kind that answers that
		// question itself the model can only be wrong: a capital_gain
		// is a distribution whatever a verdict bought on its payer's
		// signature says.
		"T-GAIN-STORE": {"INCOME_DISTRIBUTIONS", "kind"},
		// ...and the same store verdict on a DEPOSIT, the one kind
		// with no floor, where it is the only answer there is. The two
		// rows share a signature on purpose: one verdict, read where
		// the data makes no claim and overruled where it does. Flip
		// the COALESCE back and this pair fails both ways at once.
		"T-DEP-SHARED": {"INCOME_DIVIDENDS", "model"},
		// The overlay read before the floor, on a row where THOSE
		// disagree: the pin says interest earned, the floor says
		// capital_return. With the pair above this pins the whole
		// order, overlay > floor > store, which a COALESCE can
		// otherwise be permuted in without any assertion noticing.
		// (T-DIST-PIN is asserted above.)
		//
		// The store where nothing else reaches: T-WIRE and T-DEP-NEG
		// are deposits, so the floor is absent and the verdict stands.
		"T-WIRE":    {"INCOME_SALARY", "model"},
		"T-DEP-NEG": {"INCOME_SALARY", "model"},
		// A floor-kind row whose signature also has a store verdict
		// AGREEING with the floor: the value is the same either way,
		// so the provenance is the only thing that says which placed
		// it. It reads `kind` now.
		"T-DIV-STORE": {"INCOME_DIVIDENDS", "kind"},
	} {
		r := readIncomeResolution(t, db, ctx, id)
		got := [2]string{r.detailed.String, r.provenance}
		if got != want {
			t.Errorf("%s = %v, want %v", id, got, want)
		}
	}

	// Interest CHARGED, reaching the macro through the overlay rather
	// than the population: the floor's sign guard is the only thing
	// that stops it reading as interest earned, and without a row that
	// exercises it the guard can be deleted with the suite green.
	if r := readIncomeResolution(t, db, ctx, "T-INT-NEG"); r.detailed.Valid {
		t.Errorf("T-INT-NEG = %q, want unplaced: a finance charge is not interest earned",
			r.detailed.String)
	}

	// A row the floor placed reads as words like any other, in both
	// columns and by name. The label join keys on the resolved value,
	// so a floor arm added to the COALESCE but not to that join would
	// leave the type legible in the id column and blank in the one a
	// report prints — and a label pair projected the wrong way round
	// would read as prose either way.
	for id, want := range map[string][3]string{
		"T-DIVIDEND": {"INCOME", "Dividends", "Income"},
		"T-CAPGAIN":  {"INCOME", "Distributions", "Income"},
		"T-DISTRIB":  {"capital_return", "Capital return", "Capital return"},
	} {
		r := readIncomeResolution(t, db, ctx, id)
		got := [3]string{r.primary.String, r.label.String, r.primaryLabel.String}
		if got != want {
			t.Errorf("%s labels = %v, want %v", id, got, want)
		}
	}

	// The pinned distribution is IN the base and its unpinned twin is
	// not, which is the whole of decision 4 expressed in rows.
	base := macroTxnIDs(t, db, ctx, "income_lines_base", 0, 5000)
	if _, ok := base["T-DIST-PIN"]; !ok {
		t.Error("T-DIST-PIN left the base; a pin promotes the exception the floor cannot see")
	}
	if _, ok := base["T-DISTRIB"]; ok {
		t.Error("T-DISTRIB is in the base; the floor places capital_return, which the base excludes")
	}
}

// TestIncomeLinesBaseCarriesTheResolution pins the wiring between the
// two macros, which nothing else reads.
//
// Every other test here takes row IDS from the base and resolved VALUES
// from income_txn_categories(), so the base's own projection is never
// read. That leaves a whole class of defect invisible: the base's
// eleven resolved columns could be projected as NULL, or crossed over
// each other, and every assertion would still pass — the exclusion
// clause reads the joined relation rather than the projection, so even
// the row set would be unchanged.
func TestIncomeLinesBaseCarriesTheResolution(t *testing.T) {
	db, ctx := openMigrated(t)
	seedIncomeFixture(t, db, ctx)

	rows, err := db.QueryContext(ctx, `
        SELECT b.transaction_external_id,
               b.payer_signature       IS NOT DISTINCT FROM c.payer_signature,
               b.payer_name            IS NOT DISTINCT FROM c.payer_name,
               b.income_detailed       IS NOT DISTINCT FROM c.income_detailed,
               b.income_primary        IS NOT DISTINCT FROM c.income_primary,
               b.income_label          IS NOT DISTINCT FROM c.income_label,
               b.income_primary_label  IS NOT DISTINCT FROM c.income_primary_label,
               b.provenance            IS NOT DISTINCT FROM c.provenance
          FROM income_lines_base(0, 5000) b
          JOIN income_txn_categories() c
                 ON c.silver_source_id        = b.silver_source_id
                AND c.transaction_external_id = b.transaction_external_id`)
	if err != nil {
		t.Fatalf("compare base to resolution: %v", err)
	}
	defer rows.Close()
	names := []string{"payer_signature", "payer_name", "income_detailed",
		"income_primary", "income_label", "income_primary_label", "provenance"}
	seen := 0
	for rows.Next() {
		var id string
		agree := make([]bool, len(names))
		dest := []any{&id}
		for i := range agree {
			dest = append(dest, &agree[i])
		}
		if err := rows.Scan(dest...); err != nil {
			t.Fatalf("scan: %v", err)
		}
		seen++
		for i, ok := range agree {
			if !ok {
				t.Errorf("%s: income_lines_base.%s does not carry what the resolution says", id, names[i])
			}
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate: %v", err)
	}
	// Every base row the resolution answers for, and at least one of
	// each interesting shape — a floored row, a store-placed row, a
	// delta line and an unplaced one — so the comparison above is over
	// something rather than over nothing.
	if seen < 10 {
		t.Errorf("compared %d rows, want the whole base; the join matched almost nothing", seen)
	}
	var placed, blank int
	if err := db.QueryRowContext(ctx, `
        SELECT COUNT(*) FILTER (WHERE income_detailed IS NOT NULL),
               COUNT(*) FILTER (WHERE income_detailed IS NULL)
          FROM income_lines_base(0, 5000)`).Scan(&placed, &blank); err != nil {
		t.Fatalf("count: %v", err)
	}
	if placed == 0 || blank == 0 {
		t.Errorf("base carries %d placed and %d unplaced rows; both must be present for the comparison to mean anything",
			placed, blank)
	}
}

// TestIncomePayerResolution pins the four steps, in order. The order is
// the load-bearing part: step 2 answers for every row that carries an
// instrument, and a step-3 store verdict must not win over it.
func TestIncomePayerResolution(t *testing.T) {
	db, ctx := openMigrated(t)
	seedIncomeFixture(t, db, ctx)

	for id, tc := range map[string]struct {
		payer string
		why   string
	}{
		// 1. a delta line has no payer at all.
		"T-XFER": {"", "an own-account move names the holder's own bank, which is not a payer"},
		"T-GIFT": {"", "a gift names a relative, which is not a payer"},
		// 2. the instrument, by name and then by symbol.
		"T-DIVIDEND":   {"Example Dividend Corp", "the company that paid the dividend"},
		"T-DIV-SYMBOL": {"EXSY", "the symbol answers where the name is unknown"},
		"T-COUPON":     {"Example Dividend Corp", "the issuer that paid the coupon"},
		"T-STAKING":    {"Example Chain Protocol", "the protocol that paid the staking reward"},
		// ...and it outranks a store verdict on the same row's
		// signature, which is what makes this step 2 rather than step 3.
		"T-DIV-STORE": {"Example Dividend Corp", "the instrument names the payer even where the store has an answer"},
		// 3. the payer store's name for the signature.
		"T-WIRE": {"Blue Harbour Payroll", "the name the model wrote for that signature"},
		// 4. the signature itself, trimmed.
		"T-SIGONLY": {"SAMPLE TAX OFFICE", "the fold of the narrative, where nothing else answers"},
		// ...and a step that yields nothing has not answered: an
		// instrument gold holds under neither a name nor a symbol
		// falls through to the narrative rather than reading blank.
		"T-DIV-NONAME": {"EXAMPLE DEPOT", "step 2 applied but did not answer, so step 4 did"},
		// ...and the other deltas blank the column too, wherever the
		// receipt came from.
		"T-ESTATE":   {"", "an estate is not a payer"},
		"T-UNPLACED": {"", "a receipt nothing could place names nobody"},
	} {
		r := readIncomeResolution(t, db, ctx, id)
		if tc.payer == "" {
			if r.payer.Valid {
				t.Errorf("%s payer = %q, want none (%s)", id, r.payer.String, tc.why)
			}
			continue
		}
		if r.payer.String != tc.payer {
			t.Errorf("%s payer = %q, want %q (%s)", id, r.payer.String, tc.payer, tc.why)
		}
	}
}

// TestIncomeProviderViewRecordsWithoutDeciding is the income twin of
// the spending test of the same shape. The provider's own filing is
// recorded beside our verdict and never summed with it, so the columns
// have to resolve their primary and label independently of what any
// tier decided — including on a row where no tier decided anything.
func TestIncomeProviderViewRecordsWithoutDeciding(t *testing.T) {
	db, ctx := openMigrated(t)
	if _, err := db.ExecContext(ctx, `
        INSERT INTO income_txn_enrichment (silver_source_id, transaction_external_id,
              payer_signature, signature_version, income_detailed, provenance,
              provider_income_detailed, assigned_at) VALUES
            -- the bank's booking type said something specific, and it decided
            ('s', 'T-SPECIFIC', 'SIG-A', 1, 'INCOME_SALARY', 'provider', 'INCOME_SALARY', 1),
            -- it said only "somewhere in income": recorded, but the row is
            -- left for a tier that can read the payer
            ('s', 'T-CATCHALL', 'SIG-B', 1, NULL, 'signature-only', 'INCOME_OTHER', 1),
            -- it said nothing at all: NULL, which is not the same
            ('s', 'T-SILENT', 'SIG-C', 1, NULL, 'signature-only', NULL, 1)`); err != nil {
		t.Fatalf("seed: %v", err)
	}
	type got struct{ ours, theirs, theirPrim, theirLabel sql.NullString }
	str := func(v string) sql.NullString { return sql.NullString{String: v, Valid: true} }
	for id, want := range map[string]got{
		"T-SPECIFIC": {ours: str("INCOME_SALARY"), theirs: str("INCOME_SALARY"),
			theirPrim: str("INCOME"), theirLabel: str("Salary")},
		"T-CATCHALL": {theirs: str("INCOME_OTHER"),
			theirPrim: str("INCOME"), theirLabel: str("Other income")},
		"T-SILENT": {},
	} {
		var g got
		if err := db.QueryRowContext(ctx, `
            SELECT income_detailed, provider_income_detailed, provider_income_primary,
                   provider_income_label
              FROM income_txn_categories() WHERE transaction_external_id = ?`, id).
			Scan(&g.ours, &g.theirs, &g.theirPrim, &g.theirLabel); err != nil {
			t.Fatalf("read %s: %v", id, err)
		}
		if g != want {
			t.Errorf("%s = %+v, want %+v", id, g, want)
		}
	}
}

// TestIncomeOverlayCheckConstraints holds the two closed vocabularies
// the overlay tables carry. DuckDB cannot widen a CHECK in place, so
// admitting a further tier or scope mode later costs a table rewrite —
// which is the reason to pin what they admit today.
func TestIncomeOverlayCheckConstraints(t *testing.T) {
	db, ctx := openMigrated(t)

	insertEnrichment := func(id, provenance string) error {
		_, err := db.ExecContext(ctx, `
            INSERT INTO income_txn_enrichment (
                silver_source_id, transaction_external_id, payer_signature,
                signature_version, income_detailed, provenance, assigned_at
            ) VALUES ('inc-src', ?, 'sig', 1, NULL, ?, 100)`, id, provenance)
		return err
	}
	for _, p := range []string{"matcher", "rule", "provider", "signature-only", "manual"} {
		if err := insertEnrichment("OK-"+p, p); err != nil {
			t.Errorf("provenance %q rejected by CHECK: %v", p, err)
		}
	}
	// 'model' and 'kind' are resolved by income_txn_categories() rather
	// than stored, exactly as on the spending side.
	for _, p := range []string{"model", "kind", "llm", "", "MANUAL"} {
		if err := insertEnrichment("REJ-"+p, p); err == nil {
			t.Errorf("provenance %q accepted, want CHECK violation", p)
		}
	}

	insertScope := func(id, mode string) error {
		_, err := db.ExecContext(ctx, `
            INSERT INTO income_account_scope (silver_source_id, account_external_id, mode)
            VALUES ('inc-src', ?, ?)`, id, mode)
		return err
	}
	for _, m := range []string{"include", "exclude"} {
		if err := insertScope("OK-"+m, m); err != nil {
			t.Errorf("scope mode %q rejected by CHECK: %v", m, err)
		}
	}
	for _, m := range []string{"only", "", "INCLUDE"} {
		if err := insertScope("REJ-"+m, m); err == nil {
			t.Errorf("scope mode %q accepted, want CHECK violation", m)
		}
	}
}

// TestIncomeScopeIsItsOwn pins the decision that the two families keep
// separate account scopes: an account excluded from spending is not
// thereby excluded from income, and the reverse. The two questions have
// different exceptions, and one table answering both would make every
// exception a compromise.
func TestIncomeScopeIsItsOwn(t *testing.T) {
	db, ctx := openMigrated(t)
	seedIncomeFixture(t, db, ctx)
	if _, err := db.ExecContext(ctx, `
        INSERT INTO spend_account_scope (silver_source_id, account_external_id, mode)
        VALUES ('inc-src', 'BRK1', 'exclude')`); err != nil {
		t.Fatalf("seed a spending exclusion: %v", err)
	}

	got := macroTxnIDs(t, db, ctx, "income_lines_base", 0, 5000)
	if _, ok := got["T-DIVIDEND"]; !ok {
		t.Error("an account spending excludes lost its income: the two scopes are separate tables")
	}
	// ...and the exclusion income wrote is not read by spending either.
	var n int
	if err := db.QueryRowContext(ctx, `
        SELECT COUNT(*) FROM spend_scoped_accounts()
         WHERE silver_source_id = 'inc-src' AND account_external_id = 'CASH2'`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 1 {
		t.Error("income_account_scope fenced an account out of SPENDING; each scope answers one question")
	}
}

// TestMigration0073DDLIsRerunnable holds the floor-over-store re-issue
// to the replay bar, and pins the re-issue itself.
//
// A re-issue replaces a whole macro body, so the risk here is not the
// change but everything carried forward with it: the payer's four
// steps, the delta arm, the floor's seven kinds and the four provider
// columns all live in the body 0073 restates. The projection check and
// the resolution table below are what would catch a clause dropped in
// the copy.
func TestMigration0073DDLIsRerunnable(t *testing.T) {
	db, ctx := openMigrated(t)
	seedIncomeFixture(t, db, ctx)

	rerunMigrationDDL(t, db, ctx, "0073_income_floor_over_store.sql")

	assertMacroProjects(t, db, ctx, "income_txn_categories()",
		"silver_source_id", "transaction_external_id", "payer_signature",
		"payer_name", "income_detailed", "income_primary", "income_label",
		"income_primary_label", "provenance", "provider_income_detailed",
		"provider_income_primary", "provider_income_label",
		"provider_income_primary_label")

	// The whole lattice, after the re-run: overlay, then floor, then
	// store, and the payer steps untouched by the re-order.
	for id, want := range map[string][3]string{
		"T-DIST-PIN":   {"INCOME_INTEREST_EARNED", "manual", "Example Dividend Corp"},
		"T-WIRE":       {"INCOME_SALARY", "model", "Blue Harbour Payroll"},
		"T-GAIN-STORE": {"INCOME_DISTRIBUTIONS", "kind", "Example Dividend Corp"},
		"T-DEP-SHARED": {"INCOME_DIVIDENDS", "model", "Example Gain Payer"},
		"T-DIVIDEND":   {"INCOME_DIVIDENDS", "kind", "Example Dividend Corp"},
		"T-DIV-SYMBOL": {"INCOME_DIVIDENDS", "kind", "EXSY"},
		"T-SIGONLY":    {"", "signature-only", "SAMPLE TAX OFFICE"},
		"T-XFER":       {"internal_transfer", "matcher", ""},
	} {
		r := readIncomeResolution(t, db, ctx, id)
		got := [3]string{r.detailed.String, r.provenance, r.payer.String}
		if got != want {
			t.Errorf("%s after the re-run = %v, want %v", id, got, want)
		}
	}
}

// TestMigration0070DDLIsRerunnable holds the income overlay to the
// replay bar: the CREATEs are IF NOT EXISTS, the macros are OR REPLACE,
// and a re-run leaves every macro answering. It also pins the `reward`
// move, which is the one thing in this migration that changes an
// existing answer — a re-issue replaces a whole macro body, so a
// carried-forward clause dropped here would be a silent regression in
// what every spending report reads.
func TestMigration0070DDLIsRerunnable(t *testing.T) {
	db, ctx := openMigrated(t)
	seedIncomeFixture(t, db, ctx)

	rerunMigrationDDL(t, db, ctx, "0070_income_overlay.sql")

	for _, tbl := range []string{
		"income_txn_enrichment", "income_payer_categories", "income_account_scope",
	} {
		var n int
		if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+tbl).Scan(&n); err != nil {
			t.Errorf("table %s after re-run: %v", tbl, err)
		}
	}
	for _, macro := range []string{
		"income_enrichment_population(0, 9223372036854775807)",
		"income_lines_base(0, 9223372036854775807)",
		"income_txn_categories()",
		"income_scoped_accounts()",
		"spend_enrichment_population(0, 9223372036854775807)",
		"spending_lines_base(0, 9223372036854775807)",
	} {
		var n int
		if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+macro).Scan(&n); err != nil {
			t.Errorf("%s after re-run: %v", macro, err)
		}
	}

	// The spending population still projects everything 0055 gave it. A
	// re-issue replaces a whole body, so a column dropped here would not
	// fail this migration — it would fail the rule tier, or a report,
	// later and elsewhere.
	assertMacroProjects(t, db, ctx, "spend_enrichment_population(0, 5000)",
		"silver_source_id", "transaction_external_id", "occurred_at",
		"account_external_id", "account_kind", "display_name", "nickname",
		"account_category", "portfolio_external_id", "kind", "currency",
		"net_amount", "description", "counterparty", "provider_category")
	// ...and the income macros project what the milestones above them
	// will read.
	assertMacroProjects(t, db, ctx, "income_enrichment_population(0, 5000)",
		"silver_source_id", "transaction_external_id", "occurred_at",
		"account_external_id", "account_kind", "display_name", "nickname",
		"account_category", "portfolio_external_id", "kind", "currency",
		"net_amount", "description", "counterparty", "provider_category",
		"instrument_external_id")
	assertMacroProjects(t, db, ctx, "income_txn_categories()",
		"silver_source_id", "transaction_external_id", "payer_signature",
		"payer_name", "income_detailed", "income_primary", "income_label",
		"income_primary_label", "provenance", "provider_income_detailed",
		"provider_income_primary", "provider_income_label",
		"provider_income_primary_label")
	assertMacroProjects(t, db, ctx, "income_lines_base(0, 5000)",
		"silver_source_id", "transaction_external_id", "occurred_at",
		"account_external_id", "account_kind", "display_name", "nickname",
		"account_category", "kind", "currency", "net_amount", "description",
		"counterparty", "provider_category", "payer_signature", "payer_name",
		"income_detailed", "income_primary", "income_label",
		"income_primary_label", "provenance", "provider_income_detailed",
		"provider_income_primary", "provider_income_label",
		"provider_income_primary_label")

	got := macroTxnIDs(t, db, ctx, "spend_enrichment_population", 0, 5000)
	for id, why := range map[string]string{
		"T-INT-NEG": "interest charged is still spend",
		"T-REFUND":  "a refund still offsets spend",
	} {
		if _, ok := got[id]; !ok {
			t.Errorf("%s left the spending population after the re-issue (%s)", id, why)
		}
	}
	for id, why := range map[string]string{
		"T-REWARD":  "a rewards credit is income now",
		"T-INT-POS": "credited interest was never spend",
	} {
		if _, ok := got[id]; ok {
			t.Errorf("%s is in the spending population after the re-issue (%s)", id, why)
		}
	}
}

// assertMacroProjects names every column a macro must project. The
// pattern is TestMigration0055DDLIsRerunnable's: a macro is re-issued
// whole, so what a re-issue silently drops is a COLUMN, and the query
// that stops working is somewhere else entirely.
func assertMacroProjects(t *testing.T, db *sql.DB, ctx context.Context, macro string, want ...string) {
	t.Helper()
	rows, err := db.QueryContext(ctx, "SELECT * FROM "+macro+" LIMIT 0")
	if err != nil {
		t.Errorf("%s columns: %v", macro, err)
		return
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		t.Errorf("%s columns: %v", macro, err)
		return
	}
	have := map[string]bool{}
	for _, c := range cols {
		have[c] = true
	}
	for _, c := range want {
		if !have[c] {
			t.Errorf("%s no longer projects %q", macro, c)
		}
	}
	if len(cols) != len(want) {
		t.Errorf("%s projects %d columns (%v), want exactly %d", macro, len(cols), cols, len(want))
		return
	}
	// ORDER, not just membership. Every reader of these macros scans
	// `SELECT *` positionally, so two columns of the same type swapped
	// is not a compile error and not a query error — it is a silent
	// column shift, which is the failure this guard exists for.
	for i, c := range want {
		if cols[i] != c {
			t.Errorf("%s column %d is %q, want %q: a positional scan reads them in order",
				macro, i, cols[i], c)
		}
	}
}
