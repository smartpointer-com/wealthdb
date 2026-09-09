package gold

import (
	"context"
	"database/sql"
	"math"
	"testing"
	"time"
)

// spendAt is the epoch second at noon UTC on the given date — noon so a
// bucket boundary is never ambiguous about which day a row belongs to.
func spendAt(y int, m time.Month, d int) int64 {
	return time.Date(y, m, d, 12, 0, 0, 0, time.UTC).Unix()
}

// bucketAt is the epoch second at UTC midnight, which is what
// spend_period_bucket emits for the period a date opens.
func bucketAt(y int, m time.Month, d int) int64 {
	return time.Date(y, m, d, 0, 0, 0, 0, time.UTC).Unix()
}

// seedSpendingReportFixture lays down two months of spending across
// three accounts and two currencies, with every category-resolution
// path represented: merchant-store, transaction-scope, an own-account
// move that must not reach a report, and one row nothing could place.
//
// January (USD):  spend 380, refunds 20, net 360
//
//	T-GROC-JAN     -100  FOOD_AND_DRINK_GROCERIES  (merchant store)
//	T-GROC-REFUND   +20  FOOD_AND_DRINK_GROCERIES  (merchant store)
//	T-FLIGHT-JAN    -50  TRAVEL_FLIGHTS            (transaction scope)
//	T-ATM-JAN      -200  cash_withdrawal           (rule)
//	T-BACKLOG-JAN   -30  (uncategorized)
//	T-CARDPAY-JAN  -500  internal_transfer — excluded from every report
//	T-SUBSCR-JAN   -700  investment        — excluded from every report
//
// February: spend 104 USD, refunds 0, net 104
//
//	T-REST-FEB      -60 USD  FOOD_AND_DRINK_RESTAURANT
//	T-HOTEL-FEB     -40 EUR  TRAVEL_LODGING  (44 USD at the seeded rate)
func seedSpendingReportFixture(t *testing.T, db *sql.DB, ctx context.Context) {
	t.Helper()

	// 1 EUR = 1.10 USD, 1 CHF = 1.25 USD. USD->CHF and EUR->CHF (via
	// USD) fall out of fx_norm's reciprocal + the macros' triangulation.
	seedFX(t, db, spendAt(2026, time.January, 1), "USD", "EUR", "1.10")
	seedFX(t, db, spendAt(2026, time.January, 1), "USD", "CHF", "1.25")

	if _, err := db.ExecContext(ctx, `
        INSERT INTO accounts (silver_source_id, account_external_id, account_kind,
                              display_name, base_currency, first_seen_at, last_seen_at) VALUES
            ('test-src', 'CARD1', 'card', 'Everyday Card', 'USD', 1, 1),
            ('test-src', 'CASH1', 'cash', 'Everyday',      'USD', 1, 1),
            ('test-src', 'CARDE', 'card', 'Travel Card',   'EUR', 1, 1);

        INSERT INTO spend_merchant_categories (merchant_signature, merchant_name, spend_detailed,
                                               signature_version, assigned_at, model_name) VALUES
            ('sig-market', 'Corner Market', 'FOOD_AND_DRINK_GROCERIES', 1, 100, 'test-model');
    `); err != nil {
		t.Fatalf("seed spending report accounts: %v", err)
	}

	txns := []struct {
		id    string
		at    int64
		acct  string
		kind  string
		ccy   string
		net   string
		sig   string
		cat   string // spend_txn_enrichment.spend_detailed; "" = NULL
		prov  string
		descr string
	}{
		{"T-GROC-JAN", spendAt(2026, time.January, 5), "CARD1", "purchase", "USD", "-100", "sig-market", "", "signature-only", "CORNER MARKET"},
		{"T-FLIGHT-JAN", spendAt(2026, time.January, 6), "CARD1", "purchase", "USD", "-50", "sig-air", "TRAVEL_FLIGHTS", "provider", "AIRLINE"},
		{"T-GROC-REFUND", spendAt(2026, time.January, 7), "CARD1", "refund", "USD", "20", "sig-market", "", "signature-only", "CORNER MARKET"},
		{"T-CARDPAY-JAN", spendAt(2026, time.January, 15), "CASH1", "withdrawal", "USD", "-500", "sig-bank", "internal_transfer", "matcher", "PAYMENT TO CARD"},
		{"T-ATM-JAN", spendAt(2026, time.January, 20), "CASH1", "withdrawal", "USD", "-200", "sig-atm", "cash_withdrawal", "rule", "ATM WITHDRAWAL"},
		{"T-SUBSCR-JAN", spendAt(2026, time.January, 22), "CASH1", "withdrawal", "USD", "-700", "sig-sub", "investment", "manual", "SUBSCRIPTION"},
		{"T-BACKLOG-JAN", spendAt(2026, time.January, 21), "CARD1", "purchase", "USD", "-30", "sig-unknown", "", "signature-only", "UNKNOWN SHOP"},
		{"T-REST-FEB", spendAt(2026, time.February, 3), "CARD1", "purchase", "USD", "-60", "sig-diner", "FOOD_AND_DRINK_RESTAURANT", "provider", "THE DINER"},
		{"T-HOTEL-FEB", spendAt(2026, time.February, 4), "CARDE", "purchase", "EUR", "-40", "sig-hotel", "TRAVEL_LODGING", "provider", "HOTEL"},
	}
	for _, x := range txns {
		if _, err := db.ExecContext(ctx, `
            INSERT INTO transactions (silver_source_id, transaction_external_id, occurred_at,
                                      account_external_id, kind, currency, net_amount, description)
            VALUES ('test-src', ?, ?, ?, ?, ?, CAST(? AS DECIMAL(28,4)), ?)`,
			x.id, x.at, x.acct, x.kind, x.ccy, x.net, x.descr); err != nil {
			t.Fatalf("seed transaction %s: %v", x.id, err)
		}
		var cat any
		if x.cat != "" {
			cat = x.cat
		}
		if _, err := db.ExecContext(ctx, `
            INSERT INTO spend_txn_enrichment (silver_source_id, transaction_external_id,
                                              merchant_signature, signature_version,
                                              spend_detailed, provenance, assigned_at)
            VALUES ('test-src', ?, ?, 1, ?, ?, 100)`,
			x.id, x.sig, cat, x.prov); err != nil {
			t.Fatalf("seed enrichment %s: %v", x.id, err)
		}
	}
}

// spendWindow is the whole fixture window.
func spendWindow() (int64, int64) {
	return spendAt(2026, time.January, 1), spendAt(2026, time.March, 1)
}

type spendSummaryRow struct {
	period                sql.NullInt64
	txnCount              int64
	spend, refunds, netSp sql.NullFloat64
}

func readSpendingSummary(t *testing.T, db *sql.DB, ctx context.Context, ccy, period string) []spendSummaryRow {
	t.Helper()
	from, to := spendWindow()
	rows, err := db.QueryContext(ctx, `
        SELECT period_start, txn_count,
               CAST(spend AS DOUBLE), CAST(refunds AS DOUBLE), CAST(net_spend AS DOUBLE)
          FROM report_spending_summary(?, ?, ?, ?)`, from, to, ccy, period)
	if err != nil {
		t.Fatalf("report_spending_summary(%s, %s): %v", ccy, period, err)
	}
	defer rows.Close()
	var out []spendSummaryRow
	for rows.Next() {
		var r spendSummaryRow
		if err := rows.Scan(&r.period, &r.txnCount, &r.spend, &r.refunds, &r.netSp); err != nil {
			t.Fatalf("scan summary: %v", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate summary: %v", err)
	}
	return out
}

type spendCategoryRow struct {
	period                sql.NullInt64
	category              string
	txnCount              int64
	spend, refunds, netSp sql.NullFloat64
	share                 sql.NullFloat64
}

func readSpendingCategories(t *testing.T, db *sql.DB, ctx context.Context, ccy, period, level string) []spendCategoryRow {
	t.Helper()
	from, to := spendWindow()
	rows, err := db.QueryContext(ctx, `
        SELECT period_start, category, txn_count,
               CAST(spend AS DOUBLE), CAST(refunds AS DOUBLE), CAST(net_spend AS DOUBLE), share
          FROM report_spending_categories(?, ?, ?, ?, ?)`, from, to, ccy, period, level)
	if err != nil {
		t.Fatalf("report_spending_categories(%s, %s, %s): %v", ccy, period, level, err)
	}
	defer rows.Close()
	var out []spendCategoryRow
	for rows.Next() {
		var r spendCategoryRow
		if err := rows.Scan(&r.period, &r.category, &r.txnCount,
			&r.spend, &r.refunds, &r.netSp, &r.share); err != nil {
			t.Fatalf("scan category: %v", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate categories: %v", err)
	}
	return out
}

func nearly(a, b float64) bool { return math.Abs(a-b) < 1e-6 }

// TestSpendingSummaryBuckets pins the period bucketing and the
// sign-split magnitudes against known amounts: `spend` and `refunds`
// are both positive, `net_spend` is their difference, and the
// own-account move never appears in either.
func TestSpendingSummaryBuckets(t *testing.T) {
	db, ctx := openMigrated(t)
	seedSpendingReportFixture(t, db, ctx)

	months := readSpendingSummary(t, db, ctx, "USD", "month")
	if len(months) != 2 {
		t.Fatalf("monthly summary = %d buckets, want 2: %+v", len(months), months)
	}
	want := []struct {
		bucket                int64
		count                 int64
		spend, refunds, netSp float64
	}{
		{bucketAt(2026, time.January, 1), 5, 380, 20, 360},
		{bucketAt(2026, time.February, 1), 2, 104, 0, 104},
	}
	for i, w := range want {
		got := months[i]
		if !got.period.Valid || got.period.Int64 != w.bucket {
			t.Errorf("bucket[%d] period_start = %v, want %d", i, got.period, w.bucket)
		}
		if got.txnCount != w.count {
			t.Errorf("bucket[%d] txn_count = %d, want %d", i, got.txnCount, w.count)
		}
		if !nearly(got.spend.Float64, w.spend) {
			t.Errorf("bucket[%d] spend = %v, want %v", i, got.spend.Float64, w.spend)
		}
		if !nearly(got.refunds.Float64, w.refunds) {
			t.Errorf("bucket[%d] refunds = %v, want %v", i, got.refunds.Float64, w.refunds)
		}
		if !nearly(got.netSp.Float64, w.netSp) {
			t.Errorf("bucket[%d] net_spend = %v, want %v", i, got.netSp.Float64, w.netSp)
		}
	}

	// `total` is one bucket with a NULL period and the window's sums —
	// the CASE branch that must never let 'total' reach date_trunc.
	total := readSpendingSummary(t, db, ctx, "USD", "total")
	if len(total) != 1 {
		t.Fatalf("total summary = %d buckets, want 1", len(total))
	}
	if total[0].period.Valid {
		t.Errorf("total bucket period_start = %v, want NULL", total[0].period)
	}
	if !nearly(total[0].netSp.Float64, 464) || total[0].txnCount != 7 {
		t.Errorf("total bucket = (%d txns, net %v), want (7, 464)",
			total[0].txnCount, total[0].netSp.Float64)
	}

	// Every other bucket part the CLI can ask for resolves, and each
	// one partitions the same seven lines.
	for _, period := range []string{"day", "week", "month", "quarter", "year"} {
		buckets := readSpendingSummary(t, db, ctx, "USD", period)
		var n int64
		var net float64
		for _, b := range buckets {
			n += b.txnCount
			net += b.netSp.Float64
		}
		if n != 7 || !nearly(net, 464) {
			t.Errorf("period %q covers %d txns / net %v, want 7 / 464", period, n, net)
		}
	}
	if days := readSpendingSummary(t, db, ctx, "USD", "day"); len(days) != 7 {
		t.Errorf("daily summary = %d buckets, want 7 (one per transaction day)", len(days))
	}
}

// TestSpendingSummaryConvertsAtTheTransactionDay proves the reports
// carry the same FX contract as every other report macro: the EUR line
// is converted at its own day's rate, so the same window reads
// differently in a different output currency.
func TestSpendingSummaryConvertsAtTheTransactionDay(t *testing.T) {
	db, ctx := openMigrated(t)
	seedSpendingReportFixture(t, db, ctx)

	feb := readSpendingSummary(t, db, ctx, "USD", "month")[1]
	if !nearly(feb.spend.Float64, 104) { // 60 USD + 40 EUR * 1.10
		t.Errorf("February spend in USD = %v, want 104", feb.spend.Float64)
	}
	// CHF: 1 CHF = 1.25 USD, so the same two lines are 48 + 35.2.
	febCHF := readSpendingSummary(t, db, ctx, "CHF", "month")[1]
	if !nearly(febCHF.spend.Float64, 83.2) {
		t.Errorf("February spend in CHF = %v, want 83.2", febCHF.spend.Float64)
	}
}

// TestSpendingCategoriesReconcileWithSummary is the reconciliation
// pin: for every bucketing and both grouping levels, the categories of
// a bucket must add up to that bucket's summary row.
//
// This identity is STRUCTURAL — both macros aggregate
// spending_lines_base with the same sign split — so it holds by
// construction and cannot catch a wrong population. It is worth
// asserting anyway because it is the property a consumer relies on
// when it charts a total beside a breakdown, and a future refactor
// that gave either macro its own population would break it.
// TestSpendingWronglyMatchedTransferStaysVisible covers what this one
// structurally cannot.
func TestSpendingCategoriesReconcileWithSummary(t *testing.T) {
	db, ctx := openMigrated(t)
	seedSpendingReportFixture(t, db, ctx)

	for _, ccy := range []string{"USD", "CHF", "EUR"} {
		for _, period := range []string{"day", "month", "quarter", "total"} {
			for _, level := range []string{"primary", "detailed"} {
				summary := readSpendingSummary(t, db, ctx, ccy, period)
				cats := readSpendingCategories(t, db, ctx, ccy, period, level)

				type totals struct {
					count                 int64
					spend, refunds, netSp float64
					share                 float64
				}
				rolled := map[int64]*totals{}
				const nullBucket = math.MinInt64
				key := func(p sql.NullInt64) int64 {
					if !p.Valid {
						return nullBucket
					}
					return p.Int64
				}
				for _, c := range cats {
					k := key(c.period)
					if rolled[k] == nil {
						rolled[k] = &totals{}
					}
					rolled[k].count += c.txnCount
					rolled[k].spend += c.spend.Float64
					rolled[k].refunds += c.refunds.Float64
					rolled[k].netSp += c.netSp.Float64
					rolled[k].share += c.share.Float64
				}
				if len(rolled) != len(summary) {
					t.Errorf("%s/%s/%s: %d category buckets, %d summary buckets",
						ccy, period, level, len(rolled), len(summary))
				}
				for _, s := range summary {
					got := rolled[key(s.period)]
					if got == nil {
						t.Errorf("%s/%s/%s: summary bucket %v has no categories", ccy, period, level, s.period)
						continue
					}
					if got.count != s.txnCount {
						t.Errorf("%s/%s/%s bucket %v: Σ category txn_count = %d, summary = %d",
							ccy, period, level, s.period, got.count, s.txnCount)
					}
					for _, f := range []struct {
						name     string
						cat, sum float64
					}{
						{"spend", got.spend, s.spend.Float64},
						{"refunds", got.refunds, s.refunds.Float64},
						{"net_spend", got.netSp, s.netSp.Float64},
					} {
						if !nearly(f.cat, f.sum) {
							t.Errorf("%s/%s/%s bucket %v: Σ category %s = %v, summary = %v",
								ccy, period, level, s.period, f.name, f.cat, f.sum)
						}
					}
					// Shares are over magnitudes, so a bucket's shares always sum to 1.
					if !nearly(got.share, 1) {
						t.Errorf("%s/%s/%s bucket %v: Σ share = %v, want 1",
							ccy, period, level, s.period, got.share)
					}
				}
			}
		}
	}
}

// TestSpendingCategoriesLevelsAndLabel pins the two grouping levels
// against known amounts and the `(uncategorized)` label: the backlog is
// a bucket of its own at BOTH levels, materialised before the GROUP BY
// so it can never render as a blank row.
func TestSpendingCategoriesLevelsAndLabel(t *testing.T) {
	db, ctx := openMigrated(t)
	seedSpendingReportFixture(t, db, ctx)

	byCategory := func(level string) map[string]spendCategoryRow {
		out := map[string]spendCategoryRow{}
		for _, c := range readSpendingCategories(t, db, ctx, "USD", "total", level) {
			if c.category == "" {
				t.Errorf("level %s: empty category label — the COALESCE ran too late", level)
			}
			out[c.category] = c
		}
		return out
	}

	detailed := byCategory("detailed")
	for cat, want := range map[string][3]float64{
		"FOOD_AND_DRINK_GROCERIES":  {100, 20, 80},
		"FOOD_AND_DRINK_RESTAURANT": {60, 0, 60},
		"TRAVEL_FLIGHTS":            {50, 0, 50},
		"TRAVEL_LODGING":            {44, 0, 44},
		"cash_withdrawal":           {200, 0, 200},
		"(uncategorized)":           {30, 0, 30},
	} {
		got, ok := detailed[cat]
		if !ok {
			t.Errorf("detailed level is missing %q", cat)
			continue
		}
		if !nearly(got.spend.Float64, want[0]) || !nearly(got.refunds.Float64, want[1]) ||
			!nearly(got.netSp.Float64, want[2]) {
			t.Errorf("detailed %q = (%v, %v, %v), want %v", cat,
				got.spend.Float64, got.refunds.Float64, got.netSp.Float64, want)
		}
	}
	if len(detailed) != 6 {
		t.Errorf("detailed level = %d categories, want 6: %v", len(detailed), detailed)
	}
	if _, ok := detailed["internal_transfer"]; ok {
		t.Error("internal_transfer reached a spending report; an own-account move is not spend")
	}
	if _, ok := detailed["investment"]; ok {
		t.Error("investment reached a spending report; capital deployed is not spend (migration 0045)")
	}

	primary := byCategory("primary")
	for cat, want := range map[string]float64{
		"FOOD_AND_DRINK":  140, // groceries 80 + restaurant 60
		"TRAVEL":          94,  // flights 50 + lodging 44
		"cash_withdrawal": 200, // primary == detailed for the deltas
		"(uncategorized)": 30,
	} {
		got, ok := primary[cat]
		if !ok {
			t.Errorf("primary level is missing %q", cat)
			continue
		}
		if !nearly(got.netSp.Float64, want) {
			t.Errorf("primary %q net_spend = %v, want %v", cat, got.netSp.Float64, want)
		}
	}
	if len(primary) != 4 {
		t.Errorf("primary level = %d categories, want 4: %v", len(primary), primary)
	}
	// The share is over magnitudes: 200 of a 464 net-spend window.
	if got := primary["cash_withdrawal"].share.Float64; !nearly(got, 200.0/464.0) {
		t.Errorf("cash_withdrawal share = %v, want %v", got, 200.0/464.0)
	}
}

// TestSpendingInBaseDeltasAreLines pins the two deltas that stay IN
// the reports. A card bill on a card wealthdb does not itemise is
// generic card spend, and a cash gift is spending with no merchant
// behind it: each must reach the summary, appear as its own line at
// BOTH grouping levels (a delta is its own primary), and be listed at
// transaction grain with the tier that placed it — while
// internal_transfer and investment stay out as before. The base's
// exclusion list is enumerated, not "every delta", and this is what
// pins that. Each case runs against a fresh gold, so the fixture's
// 7 lines / 464 net are the baseline every time and a delta the base
// silently dropped would show as 7 rather than 8.
func TestSpendingInBaseDeltasAreLines(t *testing.T) {
	cases := []struct {
		detailed, provenance, signature, description string
		amount                                       int
	}{
		{"card_spend", "rule", "sig-issuer", "CARD BILL", 250},
		{"gift", "manual", "sig-person", "FAMILY SUPPORT", 180},
	}
	for _, tc := range cases {
		t.Run(tc.detailed, func(t *testing.T) {
			db, ctx := openMigrated(t)
			seedSpendingReportFixture(t, db, ctx)
			from, to := spendWindow()
			const id = "T-DELTA-JAN"

			// Two statements: the driver takes no parameters in a
			// multi-statement Exec.
			if _, err := db.ExecContext(ctx, `
        INSERT INTO transactions (silver_source_id, transaction_external_id, occurred_at,
                                  account_external_id, kind, currency, net_amount, description) VALUES
            ('test-src', ?, ?, 'CASH1', 'withdrawal', 'USD', ?, ?)`,
				id, spendAt(2026, time.January, 25), -tc.amount, tc.description); err != nil {
				t.Fatalf("seed %s line: %v", tc.detailed, err)
			}
			if _, err := db.ExecContext(ctx, `
        INSERT INTO spend_txn_enrichment (silver_source_id, transaction_external_id,
                                          merchant_signature, signature_version,
                                          spend_detailed, provenance, assigned_at) VALUES
            ('test-src', ?, ?, 1, ?, ?, 100)`,
				id, tc.signature, tc.detailed, tc.provenance); err != nil {
				t.Fatalf("seed %s enrichment: %v", tc.detailed, err)
			}

			net := float64(464 + tc.amount)
			total := readSpendingSummary(t, db, ctx, "USD", "total")
			if len(total) != 1 || total[0].txnCount != 8 || !nearly(total[0].netSp.Float64, net) {
				t.Errorf("total summary = %+v, want 8 txns / net %v (the %s line is counted)", total, net, tc.detailed)
			}

			for _, level := range []string{"detailed", "primary"} {
				cats := map[string]spendCategoryRow{}
				for _, c := range readSpendingCategories(t, db, ctx, "USD", "total", level) {
					cats[c.category] = c
				}
				line, ok := cats[tc.detailed]
				if !ok {
					t.Errorf("level %s has no %s line: %v", level, tc.detailed, cats)
					continue
				}
				want := float64(tc.amount)
				if line.txnCount != 1 || !nearly(line.spend.Float64, want) || !nearly(line.netSp.Float64, want) {
					t.Errorf("level %s %s = %+v, want 1 txn / %v", level, tc.detailed, line, want)
				}
				if !nearly(line.share.Float64, want/net) {
					t.Errorf("level %s %s share = %v, want %v", level, tc.detailed, line.share.Float64, want/net)
				}
				for _, excluded := range []string{"internal_transfer", "investment"} {
					if _, ok := cats[excluded]; ok {
						t.Errorf("level %s lists %s; only the in-base deltas join the reports", level, excluded)
					}
				}
			}

			var detailed, primary, provenance sql.NullString
			if err := db.QueryRowContext(ctx, `
        SELECT spend_detailed, spend_primary, provenance
          FROM report_spending_transactions(?, ?, 'USD')
         WHERE transaction_external_id = ?`, from, to, id).
				Scan(&detailed, &primary, &provenance); err != nil {
				t.Fatalf("the %s line is not listed at transaction grain: %v", tc.detailed, err)
			}
			if detailed.String != tc.detailed || primary.String != tc.detailed || provenance.String != tc.provenance {
				t.Errorf("%s = (%q, %q, %q), want (%s, %s, %s)", id,
					detailed.String, primary.String, provenance.String, tc.detailed, tc.detailed, tc.provenance)
			}
		})
	}
}

// TestSpendingDeltaLinesCarryNoMerchant pins the merchant column's
// second condition (migration 0048): the store's name for a signature
// is published only on a row whose resolved category is vendored. The
// store is reached through the signature, so a narrative the fence let
// through can hold a name for a row that is no merchant transaction —
// the holder's own on a card-bill payment, a relative's on a gift, a
// company's on a subscription — and every delta line blanks it,
// whatever the store holds. The vendored siblings keep theirs, on the
// very signature a pinned gift shares with them: the rule is per row,
// not per signature, and the resolution underneath it does not move.
//
// And the one exception (migration 0052): a card bill the built-in
// rule labelled with the issuer it was paid to shows that ISSUER,
// which comes off the enrichment row rather than the store. Two card
// bills share one signature here, one labelled and one not, so the
// column is proved to follow the label rather than the key: the
// labelled bill names its issuer, the unlabelled one names nothing,
// and neither shows the store's name for the signature they share.
//
// Four surfaces are read in one test because inheritance is the
// property: the macro that defines the column, the spending
// transaction report, the web view over it and the whole-ledger
// transaction macro all read merchant_name from spend_txn_categories()
// by name, and none re-derives it from the signature.
func TestSpendingDeltaLinesCarryNoMerchant(t *testing.T) {
	db, ctx := openMigrated(t)
	seedSpendingReportFixture(t, db, ctx)
	from, to := spendWindow()

	// A store name for every delta line's signature — the fixture's
	// own-account move, subscription and ATM row, and the three lines
	// added below. Vendored categories throughout, as the gauntlet
	// requires of a store row.
	if _, err := db.ExecContext(ctx, `
        INSERT INTO spend_merchant_categories (merchant_signature, merchant_name, spend_detailed,
                                               signature_version, assigned_at, model_name) VALUES
            ('sig-bank',   'Example Holder',   'GENERAL_SERVICES_OTHER_GENERAL_SERVICES',           1, 100, 'test-model'),
            ('sig-sub',    'Example Fund',     'GENERAL_SERVICES_ACCOUNTING_AND_FINANCIAL_PLANNING', 1, 100, 'test-model'),
            ('sig-atm',    'Example Bank ATM', 'BANK_FEES_ATM_FEES',                                1, 100, 'test-model'),
            ('sig-issuer', 'Example Issuer',   'BANK_FEES_OTHER_BANK_FEES',                         1, 100, 'test-model'),
            ('sig-person', 'Example Relative', 'GENERAL_SERVICES_OTHER_GENERAL_SERVICES',           1, 100, 'test-model')`); err != nil {
		t.Fatalf("seed store names: %v", err)
	}
	lines := []struct {
		id, acct, kind, descr string
		sig, detailed, prov   string
		net                   int
		label                 string // the card rule's issuer label, if any
	}{
		{"T-CARDBILL-JAN", "CASH1", "withdrawal", "CARD BILL", "sig-issuer", "card_spend", "rule", -250, ""},
		// The same signature, labelled: the bill's narrative named the
		// issuer it was paid to.
		{"T-CARDBILL-NAMED", "CASH1", "withdrawal", "CARD BILL", "sig-issuer", "card_spend", "rule", -260,
			"Example Card Issuer"},
		{"T-GIFT-JAN", "CASH1", "withdrawal", "FAMILY SUPPORT", "sig-person", "gift", "manual", -180, ""},
		// A pin on the grocery signature: the store names it and files
		// it as groceries, and this one row is a gift.
		{"T-GIFT-PINNED", "CARD1", "purchase", "CORNER MARKET", "sig-market", "gift", "manual", -25, ""},
	}
	for _, l := range lines {
		if _, err := db.ExecContext(ctx, `
        INSERT INTO transactions (silver_source_id, transaction_external_id, occurred_at,
                                  account_external_id, kind, currency, net_amount, description)
        VALUES ('test-src', ?, ?, ?, ?, 'USD', ?, ?)`,
			l.id, spendAt(2026, time.January, 25), l.acct, l.kind, l.net, l.descr); err != nil {
			t.Fatalf("seed %s: %v", l.id, err)
		}
		if _, err := db.ExecContext(ctx, `
        INSERT INTO spend_txn_enrichment (silver_source_id, transaction_external_id,
                                          merchant_signature, signature_version,
                                          spend_detailed, provenance, merchant_label, assigned_at)
        VALUES ('test-src', ?, ?, 1, ?, ?, ?, 100)`,
			l.id, l.sig, l.detailed, l.prov, sql.NullString{String: l.label, Valid: l.label != ""}); err != nil {
			t.Fatalf("seed %s enrichment: %v", l.id, err)
		}
	}

	// The delta lines, every one with a store name for its signature,
	// and the vendored lines that keep theirs. The resolved category is
	// checked beside the blank so a NULL from a broken resolution
	// cannot pass for the rule.
	blank := map[string]string{
		"T-CARDPAY-JAN":  "internal_transfer",
		"T-SUBSCR-JAN":   "investment",
		"T-ATM-JAN":      "cash_withdrawal",
		"T-CARDBILL-JAN": "card_spend",
		"T-GIFT-JAN":     "gift",
		"T-GIFT-PINNED":  "gift",
	}
	named := map[string]string{
		"T-GROC-JAN":       "Corner Market",
		"T-GROC-REFUND":    "Corner Market",
		"T-CARDBILL-NAMED": "Example Card Issuer",
	}
	type verdict struct{ merchant, detailed sql.NullString }
	check := func(surface string, got map[string]verdict, ids ...string) {
		t.Helper()
		for _, id := range ids {
			v, ok := got[id]
			if !ok {
				t.Errorf("%s: %s is not listed", surface, id)
				continue
			}
			if want, isDelta := blank[id]; isDelta {
				if v.merchant.Valid {
					t.Errorf("%s: %s shows merchant %q on a %s line, want none", surface, id, v.merchant.String, want)
				}
				if v.detailed.String != want {
					t.Errorf("%s: %s resolved to %q, want %s (the resolution must not move)", surface, id, v.detailed.String, want)
				}
			} else if v.merchant.String != named[id] {
				t.Errorf("%s: %s merchant = %q, want %q", surface, id, v.merchant.String, named[id])
			}
		}
	}
	read := func(query string, args ...any) map[string]verdict {
		t.Helper()
		rows, err := db.QueryContext(ctx, query, args...)
		if err != nil {
			t.Fatalf("%s: %v", query, err)
		}
		defer rows.Close()
		out := map[string]verdict{}
		for rows.Next() {
			var id string
			var v verdict
			if err := rows.Scan(&id, &v.merchant, &v.detailed); err != nil {
				t.Fatalf("scan: %v", err)
			}
			out[id] = v
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("iterate: %v", err)
		}
		return out
	}
	inBase := []string{"T-ATM-JAN", "T-CARDBILL-JAN", "T-CARDBILL-NAMED", "T-GIFT-JAN",
		"T-GIFT-PINNED", "T-GROC-JAN", "T-GROC-REFUND"}
	ledger := append([]string{"T-CARDPAY-JAN", "T-SUBSCR-JAN"}, inBase...)

	check("spend_txn_categories", read(`
        SELECT transaction_external_id, merchant_name, spend_detailed FROM spend_txn_categories()`), ledger...)
	check("report_spending_transactions", read(`
        SELECT transaction_external_id, merchant_name, spend_detailed
          FROM report_spending_transactions(?, ?, 'USD')`, from, to), inBase...)

	// `wealthdb transactions` is the whole ledger: the own-account move
	// and the subscription are listed there, with their category and
	// no merchant.
	txns, err := TransactionsBetween(ctx, db, from, to, "USD", SortAscending)
	if err != nil {
		t.Fatalf("TransactionsBetween: %v", err)
	}
	null := func(p *string) sql.NullString {
		if p == nil {
			return sql.NullString{}
		}
		return sql.NullString{String: *p, Valid: true}
	}
	whole := map[string]verdict{}
	for _, r := range txns {
		whole[r.TransactionExternalID] = verdict{null(r.MerchantName), null(r.SpendDetailed)}
	}
	check("report_transactions", whole, ledger...)

	// web_spending carries no transaction id, so it is read the way the
	// dashboard reads it: by category. Every delta line is blank, both
	// gift lines are there (so the pinned row is among them), and the
	// grocery lines keep their name.
	rows, err := db.QueryContext(ctx, `SELECT spend_detailed, merchant_name FROM web_spending`)
	if err != nil {
		t.Fatalf("web_spending: %v", err)
	}
	defer rows.Close()
	gifts, issuers := 0, 0
	for rows.Next() {
		var detailed string
		var merchant sql.NullString
		if err := rows.Scan(&detailed, &merchant); err != nil {
			t.Fatalf("scan web_spending: %v", err)
		}
		switch detailed {
		case "cash_withdrawal", "card_spend", "gift":
			// A card bill is the one delta line that may name
			// something: the issuer it was paid to, and never the
			// store's name for its signature.
			if merchant.Valid && !(detailed == "card_spend" && merchant.String == "Example Card Issuer") {
				t.Errorf("web_spending: a %s line shows merchant %q, want none", detailed, merchant.String)
			}
			if merchant.Valid {
				issuers++
			}
			if detailed == "gift" {
				gifts++
			}
		case "FOOD_AND_DRINK_GROCERIES":
			if merchant.String != "Corner Market" {
				t.Errorf("web_spending: a groceries line shows merchant %q, want Corner Market", merchant.String)
			}
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate web_spending: %v", err)
	}
	if gifts != 2 {
		t.Errorf("web_spending lists %d gift lines, want 2 (the pinned row on the grocery signature among them)", gifts)
	}
	if issuers != 1 {
		t.Errorf("web_spending lists %d issuer-named delta lines, want 1 (the labelled card bill)", issuers)
	}

	// The store is untouched: the names stay where `wealthdb
	// categorizations` lists them, and only the lines' column moves.
	var stored int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM spend_merchant_categories`).Scan(&stored); err != nil {
		t.Fatalf("count the store: %v", err)
	}
	if stored != 6 {
		t.Errorf("spend_merchant_categories = %d rows, want 6 (the fixture's one and the five seeded here)", stored)
	}
}

// TestSpendingLinesFallBackToTheirSignature pins the merchant column's
// third step (migration 0054). The merchant store is written by the
// model tier alone, and the model tier is the weakest scope: a line a
// stronger tier placed — the provider filing a card row under its
// merchant category, a rule, a pin — is never a model candidate and
// never acquires a store row, and a line nothing placed has none by
// construction. All of them carry a signature the pass computed, which
// is the very key a store row would have hung on, so the column falls
// back to it.
//
// The three steps are read together, because the fallback is the LAST
// of them: a delta line still shows its issuer label or nothing at all,
// a line the store named still shows the store's name rather than the
// fold underneath it, and only what neither answers falls through. A
// line whose signature is absent, empty, or nothing but space still
// shows nothing.
//
// The rendering is the fold VERBATIM, so one signature always renders
// one way and a report groups its lines as a single merchant — asserted
// here by grouping two lines that share a signature.
func TestSpendingLinesFallBackToTheirSignature(t *testing.T) {
	db, ctx := openMigrated(t)
	seedSpendingReportFixture(t, db, ctx)
	from, to := spendWindow()

	const branch = "EXAMPLE GROCERS BRANCH 12"
	branchSig, sigMarket, sigIssuer, empty := branch, "sig-market", "sig-issuer", ""
	blank := "   "
	lines := []struct {
		id, acct, kind, detailed, prov string
		sig                            *string
		label                          string
		at                             int64
		net                            int
	}{
		// The provider filed both under a merchant category, so neither
		// is ever a model candidate and the store never sees the key.
		{"T-PROV-JAN", "CARD1", "purchase", "FOOD_AND_DRINK_GROCERIES", "provider",
			&branchSig, "", spendAt(2026, time.January, 26), -40},
		{"T-PROV-FEB", "CARD1", "purchase", "FOOD_AND_DRINK_GROCERIES", "provider",
			&branchSig, "", spendAt(2026, time.February, 6), -35},
		// A rule placed this one and the store happens to hold a name
		// for its signature: the name outranks the fold.
		{"T-RULE-NAMED", "CARD1", "purchase", "FOOD_AND_DRINK_GROCERIES", "rule",
			&sigMarket, "", spendAt(2026, time.February, 7), -15},
		// A delta with a label: the issuer, never the fold.
		{"T-CARDBILL", "CASH1", "withdrawal", "card_spend", "rule",
			&sigIssuer, "Example Card Issuer", spendAt(2026, time.February, 8), -250},
		// Nothing to fall back to.
		{"T-EMPTYSIG", "CARD1", "purchase", "TRAVEL_LODGING", "provider",
			&empty, "", spendAt(2026, time.February, 9), -22},
		{"T-NOSIG", "CARD1", "purchase", "TRAVEL_LODGING", "provider",
			nil, "", spendAt(2026, time.February, 10), -18},
		// Normalize joins tokens and cannot emit one, but the column
		// carries no CHECK: a blank fold is guarded like an empty one
		// rather than ranking as a merchant made of spaces.
		{"T-BLANKSIG", "CARD1", "purchase", "TRAVEL_LODGING", "provider",
			&blank, "", spendAt(2026, time.February, 11), -12},
	}
	for _, l := range lines {
		if _, err := db.ExecContext(ctx, `
        INSERT INTO transactions (silver_source_id, transaction_external_id, occurred_at,
                                  account_external_id, kind, currency, net_amount, description)
        VALUES ('test-src', ?, ?, ?, ?, 'USD', ?, 'SEEDED LINE')`,
			l.id, l.at, l.acct, l.kind, l.net); err != nil {
			t.Fatalf("seed %s: %v", l.id, err)
		}
		var sig any
		if l.sig != nil {
			sig = *l.sig
		}
		if _, err := db.ExecContext(ctx, `
        INSERT INTO spend_txn_enrichment (silver_source_id, transaction_external_id,
                                          merchant_signature, signature_version,
                                          spend_detailed, provenance, merchant_label, assigned_at)
        VALUES ('test-src', ?, ?, 1, ?, ?, ?, 100)`,
			l.id, sig, l.detailed, l.prov,
			sql.NullString{String: l.label, Valid: l.label != ""}); err != nil {
			t.Fatalf("seed %s enrichment: %v", l.id, err)
		}
	}

	// "" means the column must be empty on that line.
	want := map[string]string{
		"T-PROV-JAN":   branch,
		"T-PROV-FEB":   branch,
		"T-RULE-NAMED": "Corner Market",
		"T-CARDBILL":   "Example Card Issuer",
		"T-EMPTYSIG":   "",
		"T-NOSIG":      "",
		"T-BLANKSIG":   "",
		// The fixture's own lines: the store's name where it holds one,
		// the fold where it does not — including on the line nothing
		// resolved, which is the backlog — and nothing on a delta.
		"T-GROC-JAN":    "Corner Market",
		"T-FLIGHT-JAN":  "sig-air",
		"T-BACKLOG-JAN": "sig-unknown",
		"T-ATM-JAN":     "",
	}
	check := func(surface string, got map[string]sql.NullString) {
		t.Helper()
		for id, w := range want {
			v, ok := got[id]
			if !ok {
				t.Errorf("%s: %s is not listed", surface, id)
				continue
			}
			if w == "" && v.Valid {
				t.Errorf("%s: %s shows merchant %q, want none", surface, id, v.String)
			}
			if w != "" && v.String != w {
				t.Errorf("%s: %s merchant = %q, want %q", surface, id, v.String, w)
			}
		}
	}
	read := func(query string, args ...any) map[string]sql.NullString {
		t.Helper()
		rows, err := db.QueryContext(ctx, query, args...)
		if err != nil {
			t.Fatalf("%s: %v", query, err)
		}
		defer rows.Close()
		out := map[string]sql.NullString{}
		for rows.Next() {
			var id string
			var merchant sql.NullString
			if err := rows.Scan(&id, &merchant); err != nil {
				t.Fatalf("scan: %v", err)
			}
			out[id] = merchant
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("iterate: %v", err)
		}
		return out
	}

	// The macro that defines the column, the single-currency spending
	// report the CLI reads, and the whole-ledger macro `wealthdb
	// transactions` reads: none of them re-derives the column.
	check("spend_txn_categories", read(`
        SELECT transaction_external_id, merchant_name FROM spend_txn_categories()`))
	check("report_spending_transactions", read(`
        SELECT transaction_external_id, merchant_name
          FROM report_spending_transactions(?, ?, 'USD')`, from, to))
	check("report_spending_transactions_multi", read(`
        SELECT transaction_external_id, merchant_name
          FROM report_spending_transactions_multi(?, ?)`, from, to))

	txns, err := TransactionsBetween(ctx, db, from, to, "USD", SortAscending)
	if err != nil {
		t.Fatalf("TransactionsBetween: %v", err)
	}
	whole := map[string]sql.NullString{}
	for _, r := range txns {
		var merchant sql.NullString
		if r.MerchantName != nil {
			merchant = sql.NullString{String: *r.MerchantName, Valid: true}
		}
		whole[r.TransactionExternalID] = merchant
	}
	check("report_transactions", whole)

	// One signature, two lines, one merchant: the rendering is stable,
	// so a report groups them rather than splitting the merchant in
	// two. web_spending carries no transaction id, so it is counted the
	// way the dashboard groups it.
	grouped := func(surface, query string, args ...any) {
		t.Helper()
		var groups, members int
		if err := db.QueryRowContext(ctx, query, args...).Scan(&groups, &members); err != nil {
			t.Fatalf("%s group: %v", surface, err)
		}
		if groups != 1 || members != 2 {
			t.Errorf("%s: the shared signature groups into %d merchant(s) over %d line(s), want 1 over 2",
				surface, groups, members)
		}
	}
	grouped("report_spending_transactions", `
        SELECT COUNT(*), COALESCE(SUM(n), 0) FROM (
            SELECT merchant_name, COUNT(*) AS n
              FROM report_spending_transactions(?, ?, 'USD')
             WHERE merchant_name = ? GROUP BY merchant_name)`, from, to, branch)
	grouped("web_spending", `
        SELECT COUNT(*), COALESCE(SUM(n), 0) FROM (
            SELECT merchant_name, COUNT(*) AS n
              FROM web_spending WHERE merchant_name = ? GROUP BY merchant_name)`, branch)

	// The store is untouched: the fallback is a rendering of the line's
	// own key, not a verdict written anywhere.
	var stored int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM spend_merchant_categories`).Scan(&stored); err != nil {
		t.Fatalf("count the store: %v", err)
	}
	if stored != 1 {
		t.Errorf("spend_merchant_categories = %d rows, want 1 (the fixture's own)", stored)
	}
}

// TestSpendingTransactionsMacro pins the drill-down grain: exactly the
// lines the aggregates are made of, with the merchant, the resolved
// category and the tier that decided it, and the category left NULL
// (not labelled) at this grain.
func TestSpendingTransactionsMacro(t *testing.T) {
	db, ctx := openMigrated(t)
	seedSpendingReportFixture(t, db, ctx)
	from, to := spendWindow()

	rows, err := db.QueryContext(ctx, `
        SELECT transaction_external_id, merchant_name, spend_primary, spend_detailed,
               provenance, CAST(value_outccy AS DOUBLE)
          FROM report_spending_transactions(?, ?, 'USD')`, from, to)
	if err != nil {
		t.Fatalf("report_spending_transactions: %v", err)
	}
	defer rows.Close()
	type line struct {
		merchant, primary, detailed, provenance sql.NullString
		value                                   sql.NullFloat64
	}
	got := map[string]line{}
	for rows.Next() {
		var id string
		var l line
		if err := rows.Scan(&id, &l.merchant, &l.primary, &l.detailed, &l.provenance, &l.value); err != nil {
			t.Fatalf("scan spending transaction: %v", err)
		}
		got[id] = l
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate spending transactions: %v", err)
	}

	if len(got) != 7 {
		t.Errorf("report_spending_transactions = %d rows, want 7", len(got))
	}
	if _, ok := got["T-CARDPAY-JAN"]; ok {
		t.Error("the own-account move reached the transaction report")
	}
	if _, ok := got["T-SUBSCR-JAN"]; ok {
		t.Error("the pinned investment reached the transaction report")
	}
	groc := got["T-GROC-JAN"]
	if groc.merchant.String != "Corner Market" || groc.detailed.String != "FOOD_AND_DRINK_GROCERIES" ||
		groc.primary.String != "FOOD_AND_DRINK" {
		t.Errorf("T-GROC-JAN = %+v, want the merchant-store verdict", groc)
	}
	// The store placed it, so provenance names the model tier even
	// though the overlay row is stamped signature-only: the model
	// writes no overlay row, and the tier that decided is only knowable
	// where the two scopes resolve.
	if groc.provenance.String != "model" {
		t.Errorf("T-GROC-JAN provenance = %q, want model (the store answered it)",
			groc.provenance.String)
	}
	if !nearly(groc.value.Float64, -100) {
		t.Errorf("T-GROC-JAN value = %v, want -100 (canonical sign at transaction grain)", groc.value.Float64)
	}
	backlog := got["T-BACKLOG-JAN"]
	if backlog.detailed.Valid || backlog.primary.Valid {
		t.Errorf("T-BACKLOG-JAN category = (%v, %v), want NULL at transaction grain", backlog.primary, backlog.detailed)
	}
	if backlog.provenance.String != "signature-only" {
		t.Errorf("T-BACKLOG-JAN provenance = %q, want signature-only", backlog.provenance.String)
	}
	hotel := got["T-HOTEL-FEB"]
	if !nearly(hotel.value.Float64, -44) {
		t.Errorf("T-HOTEL-FEB value = %v, want -44 (40 EUR at 1.10)", hotel.value.Float64)
	}
}

// TestSpendingMultiMacrosMatchSingleCurrency holds the `_multi`
// siblings to their contract: each per-currency column equals the
// single-currency macro called for that currency, so the web and the
// CLI cannot disagree.
func TestSpendingMultiMacrosMatchSingleCurrency(t *testing.T) {
	db, ctx := openMigrated(t)
	seedSpendingReportFixture(t, db, ctx)
	from, to := spendWindow()

	for i, ccy := range []string{"USD", "CHF", "EUR"} {
		col := []string{"net_spend_usd", "net_spend_chf", "net_spend_eur"}[i]
		rows, err := db.QueryContext(ctx,
			`SELECT period_start, txn_count, CAST(`+col+` AS DOUBLE)
               FROM report_spending_summary_multi(?, ?, 'month')`, from, to)
		if err != nil {
			t.Fatalf("report_spending_summary_multi: %v", err)
		}
		var multi []spendSummaryRow
		for rows.Next() {
			var r spendSummaryRow
			if err := rows.Scan(&r.period, &r.txnCount, &r.netSp); err != nil {
				t.Fatalf("scan summary_multi: %v", err)
			}
			multi = append(multi, r)
		}
		rows.Close()

		single := readSpendingSummary(t, db, ctx, ccy, "month")
		if len(multi) != len(single) {
			t.Fatalf("%s: multi = %d buckets, single = %d", ccy, len(multi), len(single))
		}
		for j := range single {
			if multi[j].period != single[j].period || multi[j].txnCount != single[j].txnCount ||
				!nearly(multi[j].netSp.Float64, single[j].netSp.Float64) {
				t.Errorf("%s bucket %d: multi %+v != single %+v", ccy, j, multi[j], single[j])
			}
		}
	}

	// The category shares are per currency, and each currency's still sum to 1.
	rows, err := db.QueryContext(ctx, `
        SELECT SUM(share_usd), SUM(share_chf), SUM(share_eur)
          FROM report_spending_categories_multi(?, ?, 'total', 'primary')`, from, to)
	if err != nil {
		t.Fatalf("report_spending_categories_multi: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var usd, chf, eur sql.NullFloat64
		if err := rows.Scan(&usd, &chf, &eur); err != nil {
			t.Fatalf("scan shares: %v", err)
		}
		for name, v := range map[string]sql.NullFloat64{"usd": usd, "chf": chf, "eur": eur} {
			if !nearly(v.Float64, 1) {
				t.Errorf("Σ share_%s = %v, want 1", name, v.Float64)
			}
		}
	}
}

// TestSpendingWronglyMatchedTransferStaysVisible is the negative half
// of the reconciliation.
//
// The summary-equals-categories identity can never fail, because both
// sides read the same base: an over-eager internal-transfer match
// removes a row from BOTH at once and the identity still holds — the
// month is simply, silently, cheaper. So the property worth proving is
// not the identity but that the removal is still VISIBLE somewhere.
//
// It is: the enrichment population (migration 0041) is deliberately
// the layer BEFORE the internal-transfer exclusion, so the rows the
// matcher took out are still there, still carrying the tier that took
// them. The test wrongly pairs a real purchase with an unrelated
// withdrawal, shows the reports quietly lose 600 while their identity
// survives, and shows the population accounting for exactly that 600.
func TestSpendingWronglyMatchedTransferStaysVisible(t *testing.T) {
	db, ctx := openMigrated(t)
	seedSpendingReportFixture(t, db, ctx)
	from, to := spendWindow()

	// A -300 purchase and a -300 withdrawal two days apart: the shape
	// the matcher pairs, on two rows that are really spending.
	if _, err := db.ExecContext(ctx, `
        INSERT INTO transactions (silver_source_id, transaction_external_id, occurred_at,
                                  account_external_id, kind, currency, net_amount, description) VALUES
            ('test-src', 'T-LOOKALIKE-BUY',  ?, 'CARD1', 'purchase',   'USD', -300, 'FURNITURE STORE'),
            ('test-src', 'T-LOOKALIKE-CASH', ?, 'CASH1', 'withdrawal', 'USD', -300, 'COUNTER WITHDRAWAL')`,
		spendAt(2026, time.January, 10), spendAt(2026, time.January, 12)); err != nil {
		t.Fatalf("seed look-alike pair: %v", err)
	}
	if _, err := db.ExecContext(ctx, `
        INSERT INTO spend_txn_enrichment (silver_source_id, transaction_external_id,
                                          merchant_signature, signature_version,
                                          spend_detailed, provenance, assigned_at) VALUES
            ('test-src', 'T-LOOKALIKE-BUY',  'sig-furniture', 1, 'HOME_IMPROVEMENT_FURNITURE', 'provider', 100),
            ('test-src', 'T-LOOKALIKE-CASH', 'sig-counter',   1, 'cash_withdrawal',            'rule',     100)`); err != nil {
		t.Fatalf("seed look-alike enrichment: %v", err)
	}

	// auditMagnitude is what the enrichment population says was taken
	// out of spending by the matcher: the layer before the exclusion,
	// joined to the resolution that removed it.
	auditMagnitude := func() (int, float64) {
		t.Helper()
		var n int
		var amt sql.NullFloat64
		if err := db.QueryRowContext(ctx, `
            SELECT COUNT(*), CAST(SUM(ABS(p.net_amount)) AS DOUBLE)
              FROM spend_enrichment_population(?, ?) p
              JOIN spend_txn_categories() c
                     ON c.silver_source_id        = p.silver_source_id
                    AND c.transaction_external_id = p.transaction_external_id
             WHERE c.spend_detailed = 'internal_transfer'
               AND c.provenance     = 'matcher'`, from, to).Scan(&n, &amt); err != nil {
			t.Fatalf("audit population: %v", err)
		}
		return n, amt.Float64
	}

	beforeNet := readSpendingSummary(t, db, ctx, "USD", "total")[0].netSp.Float64
	beforeLegs, beforeAudit := auditMagnitude()
	if !nearly(beforeNet, 464+600) {
		t.Fatalf("net_spend before the wrong match = %v, want 1064", beforeNet)
	}

	// The matcher pairs them, wrongly: both legs become own-account
	// moves and leave the spending base.
	if _, err := db.ExecContext(ctx, `
        UPDATE spend_txn_enrichment
           SET spend_detailed = 'internal_transfer', provenance = 'matcher'
         WHERE transaction_external_id IN ('T-LOOKALIKE-BUY', 'T-LOOKALIKE-CASH')`); err != nil {
		t.Fatalf("apply the wrong match: %v", err)
	}

	afterNet := readSpendingSummary(t, db, ctx, "USD", "total")[0].netSp.Float64
	if !nearly(afterNet, 464) {
		t.Fatalf("net_spend after the wrong match = %v, want 464", afterNet)
	}

	// The identity survives untouched — which is exactly why it cannot
	// be the test for this.
	var catSum float64
	for _, c := range readSpendingCategories(t, db, ctx, "USD", "total", "detailed") {
		catSum += c.netSp.Float64
	}
	if !nearly(catSum, afterNet) {
		t.Errorf("Σ categories = %v, summary = %v: the identity broke, which is not what a wrong match does",
			catSum, afterNet)
	}
	// ...and the drill-down agrees with the aggregate, so a reader
	// cannot find the missing money by opening the report either.
	var listed int
	if err := db.QueryRowContext(ctx, `
        SELECT COUNT(*) FROM report_spending_transactions(?, ?, 'USD')
         WHERE transaction_external_id IN ('T-LOOKALIKE-BUY', 'T-LOOKALIKE-CASH')`,
		from, to).Scan(&listed); err != nil {
		t.Fatalf("count listed legs: %v", err)
	}
	if listed != 0 {
		t.Errorf("the wrongly matched legs are still listed (%d); the premise of this test is that they vanish", listed)
	}

	// The audit surface is where they went: the enrichment population
	// keeps them, tagged with the tier that removed them, and accounts
	// for exactly the money the report lost.
	afterLegs, afterAudit := auditMagnitude()
	if afterLegs != beforeLegs+2 {
		t.Errorf("matcher-removed legs = %d, want %d (the two new ones)", afterLegs, beforeLegs+2)
	}
	if !nearly(afterAudit-beforeAudit, beforeNet-afterNet) {
		t.Errorf("audit surface accounts for %v of removed spend, but the report lost %v",
			afterAudit-beforeAudit, beforeNet-afterNet)
	}
}

// TestTransactionsBetweenCarriesSpendCategory pins the F1 lockstep:
// report_transactions gained the merchant and category columns and
// TransactionsBetween scans it positionally, so a drifted projection
// surfaces here rather than at runtime. The neighbouring fields are
// checked too — a shifted scan lands a category in the description.
func TestTransactionsBetweenCarriesSpendCategory(t *testing.T) {
	db, ctx := openMigrated(t)
	seedSpendingReportFixture(t, db, ctx)
	from, to := spendWindow()

	rows, err := TransactionsBetween(ctx, db, from, to, "USD", SortAscending)
	if err != nil {
		t.Fatalf("TransactionsBetween: %v", err)
	}
	byID := map[string]TransactionRow{}
	for _, r := range rows {
		byID[r.TransactionExternalID] = r
	}
	if len(byID) != 9 {
		t.Fatalf("TransactionsBetween = %d rows, want 9 (the whole ledger, spending or not)", len(byID))
	}

	groc := byID["T-GROC-JAN"]
	if groc.MerchantName == nil || *groc.MerchantName != "Corner Market" {
		t.Errorf("T-GROC-JAN merchant = %v, want Corner Market", groc.MerchantName)
	}
	if groc.SpendPrimary == nil || *groc.SpendPrimary != "FOOD_AND_DRINK" {
		t.Errorf("T-GROC-JAN spend_primary = %v, want FOOD_AND_DRINK", groc.SpendPrimary)
	}
	if groc.SpendDetailed == nil || *groc.SpendDetailed != "FOOD_AND_DRINK_GROCERIES" {
		t.Errorf("T-GROC-JAN spend_detailed = %v, want FOOD_AND_DRINK_GROCERIES", groc.SpendDetailed)
	}
	// Scan alignment: the neighbours on both sides must still be theirs.
	if groc.Description == nil || *groc.Description != "CORNER MARKET" {
		t.Errorf("T-GROC-JAN description = %v (scan alignment)", groc.Description)
	}
	if groc.ValueOutCcy == nil || *groc.ValueOutCcy != "-100" {
		t.Errorf("T-GROC-JAN value = %v, want -100 (scan alignment)", groc.ValueOutCcy)
	}

	// The own-account move keeps its category here: `wealthdb
	// transactions` is the whole ledger, and a row excluded from the
	// spending reports must still say what it was excluded as.
	pay := byID["T-CARDPAY-JAN"]
	if pay.SpendDetailed == nil || *pay.SpendDetailed != "internal_transfer" {
		t.Errorf("T-CARDPAY-JAN spend_detailed = %v, want internal_transfer", pay.SpendDetailed)
	}
	sub := byID["T-SUBSCR-JAN"]
	if sub.SpendDetailed == nil || *sub.SpendDetailed != "investment" {
		t.Errorf("T-SUBSCR-JAN spend_detailed = %v, want investment", sub.SpendDetailed)
	}
}

// TestMigration0042DDLIsRerunnable holds the spending report migration
// to the replay bar, and confirms it leaves web_transactions — which it
// drops and re-creates around the report_transactions_multi swap — in
// place afterwards.
func TestMigration0042DDLIsRerunnable(t *testing.T) {
	db, ctx := openMigrated(t)
	seedSpendingReportFixture(t, db, ctx)

	rerunMigrationDDL(t, db, ctx, "0042_report_spending.sql")

	var n int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM information_schema.columns
		  WHERE table_name = 'web_transactions' AND column_name = 'account_kind'`).Scan(&n); err != nil {
		t.Fatalf("web_transactions.account_kind: %v", err)
	}
	if n != 1 {
		t.Errorf("web_transactions.account_kind columns = %d, want 1", n)
	}
	// Replaying a SUPERSEDED migration is a downgrade, not a no-op: it
	// puts every object it defines back at its own version, and a view
	// a later migration built on a later version stops binding until
	// the chain is replayed forward. So replay the migrations that
	// redefine these macros, which is what Migrate itself would do —
	// and extend this list when another one does.
	for _, later := range []string{
		"0045_spend_investment.sql",        // spending_lines_base
		"0059_spend_labels_in_reports.sql", // the label + issuer columns
	} {
		rerunMigrationDDL(t, db, ctx, later)
	}

	// The macros still answer, including the one the re-run replaced
	// underneath a view that reads it.
	from, to := spendWindow()
	for _, q := range []string{
		`SELECT COUNT(*) FROM report_spending_summary(?, ?, 'USD', 'month')`,
		`SELECT COUNT(*) FROM report_spending_categories(?, ?, 'USD', 'month', 'primary')`,
		`SELECT COUNT(*) FROM report_spending_transactions(?, ?, 'USD')`,
		`SELECT COUNT(*) FROM spending_lines_base(?, ?)`,
	} {
		if err := db.QueryRowContext(ctx, q, from, to).Scan(&n); err != nil {
			t.Errorf("after re-run: %s: %v", q, err)
		}
	}
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM web_spending`).Scan(&n); err != nil {
		t.Errorf("web_spending after the macro re-issue: %v", err)
	}
}

// TestSpendReportColumnsSurviveAReissue pins the column set every
// spending report publishes.
//
// A macro is re-issued whole, so a migration that adds one column has to
// retype every other. Twice now that has silently dropped columns a
// later migration had added — and nothing failed until a caller went
// looking for one. This is the cheap guard: it does not care what a
// column means, only that the report still has it.
func TestSpendReportColumnsSurviveAReissue(t *testing.T) {
	db, ctx := openMigrated(t)
	cols := func(q string) map[string]bool {
		t.Helper()
		rows, err := db.QueryContext(ctx, q)
		if err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		defer rows.Close()
		names, err := rows.Columns()
		if err != nil {
			t.Fatalf("%s columns: %v", q, err)
		}
		out := map[string]bool{}
		for _, n := range names {
			out[n] = true
		}
		return out
	}
	for _, tc := range []struct {
		query string
		want  []string
	}{
		{"SELECT * FROM report_spending_categories(0, 9, 'USD', 'month', 'detailed') LIMIT 0",
			[]string{"period_start", "category", "category_label", "txn_count",
				"spend", "refunds", "net_spend", "share"}},
		{"SELECT * FROM report_spending_categories_multi(0, 9, 'month', 'detailed') LIMIT 0",
			[]string{"period_start", "category", "category_label", "txn_count",
				"spend_usd", "spend_chf", "spend_eur",
				"refunds_usd", "refunds_chf", "refunds_eur",
				"net_spend_usd", "net_spend_chf", "net_spend_eur",
				"share_usd", "share_chf", "share_eur"}},
		{"SELECT * FROM report_spending_transactions(0, 9, 'USD') LIMIT 0",
			[]string{"merchant_name", "spend_primary", "spend_detailed",
				"spend_label", "spend_primary_label", "provenance",
				"provider_spend_detailed", "provider_spend_label", "value_outccy"}},
		{"SELECT * FROM report_spending_transactions_multi(0, 9) LIMIT 0",
			[]string{"merchant_name", "spend_primary", "spend_detailed",
				"spend_label", "spend_primary_label", "provenance",
				"provider_spend_detailed", "provider_spend_label",
				"value_usd", "value_chf", "value_eur"}},
		{"SELECT * FROM web_spending LIMIT 0",
			[]string{"occurred_at", "merchant_name", "spend_primary", "spend_detailed",
				"spend_primary_label", "spend_label", "provider_spend_label",
				"value_usd", "value_chf", "value_eur"}},
		{"SELECT * FROM spending_lines_base(0, 9) LIMIT 0",
			[]string{"spend_detailed", "spend_primary", "spend_label",
				"spend_primary_label", "provider_spend_detailed",
				"provider_spend_primary", "provider_spend_label",
				"provider_spend_primary_label", "provider_category"}},
	} {
		have := cols(tc.query)
		for _, c := range tc.want {
			if !have[c] {
				t.Errorf("%s lost column %q", tc.query, c)
			}
		}
	}
}
