package gold

import (
	"context"
	"database/sql"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
)

func execEach(t *testing.T, db *sql.DB, stmts ...string) {
	t.Helper()
	for _, q := range stmts {
		if _, err := db.ExecContext(context.Background(), q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
}

func scanStrings(t *testing.T, db *sql.DB, q string, args ...any) []string {
	t.Helper()
	var out []string
	if err := scanAll(context.Background(), db, q, func(rows *sql.Rows) error {
		var s string
		if err := rows.Scan(&s); err != nil {
			return err
		}
		out = append(out, s)
		return nil
	}, args...); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	return out
}

// termsText is instrument_terms_at on a day, one line per instrument:
// each field and its origin.
const termsText = `SELECT concat_ws(' ', instrument_external_id,
    COALESCE(coupon_pct::VARCHAR || '@' || coupon_origin, '-'), COALESCE(maturity::VARCHAR || '@' || maturity_origin, '-'),
    COALESCE(frequency::VARCHAR || '@' || frequency_origin, '-'), COALESCE(coupon_kind || '@' || coupon_kind_origin, '-'),
    COALESCE(day_count || '@' || day_count_origin, '-'), COALESCE(call_date::VARCHAR || '@' || call_origin, '-'),
    CASE WHEN make_whole THEN 'make_whole' ELSE '-' END, COALESCE(rating || '@' || rating_origin, '-'),
    CASE WHEN in_default THEN 'in_default' ELSE '-' END, COALESCE(fixed_until::VARCHAR, '-'),
    COALESCE(duration::VARCHAR, '-'), COALESCE(yield_pct::VARCHAR, '-'), COALESCE(terms_as_of::VARCHAR, '-'))
  FROM instrument_terms_at(?) ORDER BY instrument_external_id`

// A bond's terms come field by field from the best source that states
// them: the source's own figures, the description, OpenFIGI's ticker;
// the frequency and the day count else from the market's convention, a
// government bond's and a bill's their own. On a day, the ledger's
// record in force (the latest dated on or before it, else the undated
// one) lays its fields over them; it names a fund's factsheet figures
// too. A stock no record names has none.
func TestYieldPassReadsInstrumentTerms(t *testing.T) {
	t.Parallel()
	db, ctx := openMigrated(t)
	execEach(t, db,
		`INSERT INTO instruments (silver_source_id, instrument_external_id, asset_class, vehicle, isin, cusip, name,
		 currency, payload, first_seen_at, last_seen_at) VALUES
		 ('test-src', 'QMUNI', 'fixed_income', 'bond', NULL, '00000000C', 'QLAND CNTY GO BDS 06/15/31 FIXED COUPON '
		  || 'MOODYS Aa2 S&P AA- SEMIANNUALLY CALL ON 06/15/2027 100.00 CUSIP: 00000000C', 'USD', NULL, 0, 0),
		 ('test-src', 'QSOV', 'fixed_income', 'bond', 'XX0000000051', NULL, 'QLAND 1.915% 10.05.2034', 'EUR', NULL, 0, 0),
		 ('test-src', 'QBILL', 'fixed_income', 'bond', NULL, '00000000D', 'EXAMPLE TREAS BILLS ZERO CPN', 'USD', NULL, 0, 0),
		 ('test-src', 'QEBILL', 'fixed_income', 'bond', NULL, NULL, 'QLAND TREASURY BILL ZERO CPN', 'EUR', NULL, 0, 0),
		 ('test-src', 'QGOV', 'fixed_income', 'bond', 'US00000000G9', NULL, 'UNITED STATES 3.875% 02/15/2034', 'USD',
		  NULL, 0, 0),
		 ('test-src', 'QMW', 'fixed_income', 'bond', NULL, NULL, 'QORVANT CORP 5% 03/01/2033 CALL 03/01/2028 MAKE WHOLE '
		  || 'S&P D', 'USD', NULL, 0, 0),
		 ('test-src', 'QNOTE', 'private_equity', 'convertible_note', NULL, NULL, 'Qorvant note', 'USD',
		  '{"principal": 1000, "interest_rate": 6, "maturity_date": "2027-03-31"}', 0, 0),
		 ('test-src', 'QDECL', 'fixed_income', 'bond', NULL, NULL, 'QORVANT CORP 4.5% 01/15/2030 MOODYS A2', 'USD',
		  NULL, 0, 0),
		 ('test-src', 'QFUND', 'fixed_income', 'fund', NULL, NULL, 'Example Stable Value Sleeve', 'USD', NULL, 0, 0),
		 ('test-src', 'QSTK', 'public_equity', 'stock', NULL, NULL, 'QORVANT CORP', 'USD', NULL, 0, 0)`,
		`INSERT INTO instrument_keys (silver_source_id, instrument_external_id, security_key)
		 VALUES ('test-src', 'QMUNI', 'cusip:00000000C')`,
		`INSERT INTO security_identities (silver_source_id, security_key, as_of, provider, security_type, payload)
		 VALUES ('refdata', 'cusip:00000000C', 0, 'openfigi', 'Muni', '{"local_ticker": "QLAND 4.25 06/15/31"}')`,
		// Undated, then dated 2023-11-14 and 2025-06-15.
		`INSERT INTO yield_declared_terms (line_no, silver_source_id, instrument_external_id, as_of, coupon_pct,
		 frequency, call_date, fixed_until, coupon_kind) VALUES
		 (1, 'test-src', 'QDECL', NULL, 4.25, 1, NULL, NULL, NULL),
		 (2, 'test-src', 'QDECL', 1700000000, 4.75, 2, DATE '2028-01-15', NULL, NULL),
		 (3, 'test-src', 'QDECL', 1750000000, 4.5, 4, DATE '2029-01-15', DATE '2028-12-31', 'floating')`,
		`INSERT INTO yield_declared_terms (line_no, silver_source_id, instrument_external_id, as_of, duration, yield_pct)
		 VALUES (4, 'test-src', 'QFUND', 1750000000, 6.1, 4.4)`,
		`INSERT INTO yield_declared_ratings (line_no, silver_source_id, instrument_external_id, rating)
		 VALUES (5, 'test-src', 'QDECL', 'BBB')`)
	n, err := RebuildYield(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	if n.Instruments != 8 || n.WithMaturity != 6 {
		t.Errorf("counts %+v", n)
	}
	later := time.Date(2026, 6, 30, 0, 0, 0, 0, time.UTC).Unix()
	got := scanStrings(t, db, termsText, later)
	want := []string{
		"QBILL 0.0@description - 0@convention zero@description ACT/360@convention - - - - - - - -",
		// The newest record in force wins, field by field over the sources.
		"QDECL 4.5@declared 2030-01-15@description 4@declared floating@declared 30/360@convention " +
			"2029-01-15@declared - BBB@declared - 2028-12-31 - - 1750000000",
		// A euro bill: no euro bill convention, so no coupon on ACT/360.
		"QEBILL 0.0@description - 0@convention zero@description ACT/360@convention - - - - - - - -",
		"QFUND - - - - - - - - - - 6.1 4.4 1750000000",
		// The description reads a sovereign's bond as government.
		"QGOV 3.875@description 2034-02-15@description 2@convention fixed@convention ACT/ACT@convention - - - - - - - -",
		"QMUNI 4.25@openfigi 2031-06-15@description 2@description fixed@description 30/360@convention " +
			"2027-06-15@description - AA@description - - - - -",
		"QMW 5.0@description 2033-03-01@description 2@convention fixed@convention 30/360@convention - make_whole " +
			"B@description in_default - - - -",
		"QNOTE 6.0@payload 2027-03-31@payload - - - - - - - - - - -",
		"QSOV 1.915@description 2034-05-10@description 1@convention fixed@convention 30/360@convention - - - - - - - -",
	}
	if !slices.Equal(got, want) {
		t.Errorf("terms:\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	// Between the two dated records the older one holds; before both,
	// the undated one, which states a coupon and a frequency.
	between := scanStrings(t, db, termsText, time.Date(2024, 6, 30, 0, 0, 0, 0, time.UTC).Unix())
	before := scanStrings(t, db, termsText, time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC).Unix())
	if !strings.HasPrefix(between[1], "QDECL 4.75@declared 2030-01-15@description 2@declared fixed@convention") ||
		!strings.HasPrefix(before[1], "QDECL 4.25@declared 2030-01-15@description 1@declared fixed@convention") {
		t.Errorf("the record in force:\n%s\n%s", between[1], before[1])
	}
	if slices.ContainsFunc(got, func(s string) bool { return strings.HasPrefix(s, "QSTK") }) {
		t.Error("a stock no record names has terms")
	}
}

// An account's stated rates come from the shape each source writes: a
// cash balance's tiers with their limits, per snapshot; an account's
// details (a comma or a dot decimal) or its strategy; and the ledger's
// rate records for the accounts a source states. The rates here are
// invented.
func TestYieldPassReadsAccountRates(t *testing.T) {
	t.Parallel()
	db, ctx := openMigrated(t)
	day := time.Date(2026, 6, 30, 0, 0, 0, 0, time.UTC).Unix()
	q := `INSERT INTO cash_balances (silver_source_id, snapshot_at, account_external_id, currency, balance_kind, amount,
	 payload) VALUES
	 ('test-src', ?, 'CUR', 'CHF', 'closing', 1000, '{"CdtIntrstRate1": "0.333", "CdtLmtAmt1": "77777",
	   "CdtIntrstRate2": "0.111", "CdtLmtAmt2": "99999999999999999.99", "CdtIntrstRate3": "0", "CdtLmtAmt3": "0",
	   "CdtIntrstVldFrmDt": "2026-01-01", "DbtIntrstRate1": "7.777", "DbtLmtAmt1": "0", "DbtIntrstRate2": "0",
	   "DbtIntrstVldFrmDt": "2025-07-01", "CalIntrstBasisDesc": "act/360", "EndOfDurtn": "9999-12-31"}'),
	 ('test-src', ?, 'CUR', 'CHF', 'available', 900, '{"CdtIntrstRate1": "0.333", "CdtLmtAmt1": "77777",
	   "CalIntrstBasisDesc": "act/360"}'),
	 ('test-src', ?, 'CUR', 'CHF', 'closing', 1000, '{"CdtIntrstRate1": "0.222", "CalIntrstBasisDesc": "30E/360"}'),
	 ('test-src', ?, 'TERM', 'USD', 'closing', 5000, '{"CdtIntrstRate1": "3.333", "CdtLmtAmt1": "0",
	   "CalIntrstBasisDesc": "ACT / 365", "EndOfDurtn": "2027-03-31"}')`
	if _, err := db.ExecContext(ctx, q, day, day, day-86400*30, day); err != nil {
		t.Fatal(err)
	}
	execEach(t, db,
		`INSERT INTO accounts (silver_source_id, account_external_id, account_kind, display_name, base_currency,
		 first_seen_at, last_seen_at, payload) VALUES
		 ('test-src', 'GIRO', 'cash', 'Giro', 'EUR', 0, 0,
		  '{"details": {"interest_credit": "0,111% ab 01.01.2024", "interest_debit": "7,777% ab 01.07.2024"}}'),
		 ('test-src', 'DOT', 'cash', 'Dot', 'EUR', 0, 0, '{"details": {"interest_credit": "0.222% ab 01.01.2025"}}'),
		 ('test-src', 'PILLAR', 'brokerage', 'Pillar', 'CHF', 0, 0, '{"strategy": {"interestRate": 0.111}}'),
		 ('test-src', 'NONE', 'cash', 'None', 'CHF', 0, 0, '{"details": {"interest_credit": "--"}}'),
		 ('test-src', 'SAVE', 'cash', 'Save', 'CHF', 0, 0, NULL)`,
		`INSERT INTO yield_declared_rates (line_no, silver_source_id, account_external_id, as_of, currency, rate_pct)
		 VALUES (7, 'test-src', 'SAVE', 1767225600, 'CHF', 0.444), (8, 'test-src', 'GONE', 1767225600, 'CHF', 0.555)`)
	n, err := RebuildYield(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	if n.RatedAccounts != 6 {
		t.Errorf("rated accounts %d, want 6", n.RatedAccounts)
	}
	got := scanStrings(t, db, `SELECT concat_ws(' ', account_external_id, COALESCE(currency, '-'), side, tier,
	    rate_pct, COALESCE(up_to::VARCHAR, '-'), COALESCE(valid_from::VARCHAR, '-'), COALESCE(term_end::VARCHAR, '-'),
	    COALESCE(day_count, '-'), origin, COALESCE(field, '-'), COALESCE(line_no::VARCHAR, '-'),
	    COALESCE(strftime(to_timestamp(observed_at) AT TIME ZONE 'UTC', '%m-%d'), '-'))
	  FROM account_rates ORDER BY account_external_id, observed_at, side, tier`)
	want := []string{
		// The older snapshot, and the newer one with its tiers: a second
		// tier only after a capped first, a third after an open second
		// never. One row a snapshot whatever its balance kinds.
		"CUR CHF credit 1 0.222 - - - 30E/360 stated CdtIntrstRate1 - 05-31",
		"CUR CHF credit 1 0.333 77777.0 2026-01-01 - ACT/360 stated CdtIntrstRate1 - 06-30",
		"CUR CHF credit 2 0.111 - 2026-01-01 - ACT/360 stated CdtIntrstRate2 - 06-30",
		"CUR CHF debit 1 7.777 - 2025-07-01 - ACT/360 stated DbtIntrstRate1 - 06-30",
		"DOT EUR credit 1 0.222 - 2025-01-01 - - stated details.interest_credit - -",
		"GIRO EUR credit 1 0.111 - 2024-01-01 - - stated details.interest_credit - -",
		"GIRO EUR debit 1 7.777 - 2024-07-01 - - stated details.interest_debit - -",
		"PILLAR CHF credit 1 0.111 - - - - stated strategy.interestRate - -",
		// A record of an account no source states declares nothing.
		"SAVE CHF credit 1 0.444 - 2026-01-01 - - declared - 7 -",
		"TERM USD credit 1 3.333 - - 2027-03-31 ACT/365 stated CdtIntrstRate1 - 06-30",
	}
	if !slices.Equal(got, want) {
		t.Errorf("account rates:\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

// A ledger's yield records go to their tables; one that states nothing
// else describes no exposure. A rate naming no account and a rating
// naming no holding are lines that declare nothing.
func TestSyncExposureWritesYieldRecords(t *testing.T) {
	t.Parallel()
	db, ctx := openMigrated(t)
	execEach(t, db, `INSERT INTO instruments (silver_source_id, instrument_external_id, asset_class, vehicle, name,
	  currency, first_seen_at, last_seen_at) VALUES ('test-src', 'QB', 'fixed_income', 'bond', 'Qorvant bond', 'USD', 0, 0)`)
	coupon, freq := 4.25, 2
	until := time.Date(2026, 12, 31, 0, 0, 0, 0, time.UTC)
	rows := canonical.ExposureRows{Declarations: []canonical.ExposureDeclaration{
		{Line: 1, SilverSourceID: "test-src", AccountExternalID: "NOPE",
			Rate: &canonical.DeclaredRate{Pct: 0.5, Until: &until}},
		{Line: 2, SilverSourceID: "test-src", InstrumentExternalID: "QB",
			Rating: &canonical.DeclaredRating{Bucket: canonical.RatingAA}, Terms: &canonical.DeclaredTerms{Coupon: &coupon,
				Frequency: &freq, CouponKind: canonical.CouponFixed}},
		{Line: 3, SilverSourceID: "test-src", InstrumentExternalID: "QGONE",
			Dividend: &canonical.DeclaredDividend{Amount: 0.42, Frequency: 4}},
	}}
	if n, err := SyncExposure(ctx, db, rows); err != nil || n != 3 {
		t.Fatalf("SyncExposure = %d, %v", n, err)
	}
	for table, want := range map[string]int{"exposure_declarations": 0, "yield_declared_rates": 1,
		"yield_declared_ratings": 1, "yield_declared_terms": 1, "yield_declared_dividends": 1} {
		if got := countRows(t, db, ctx, table); got != want {
			t.Errorf("%s holds %d rows, want %d", table, got, want)
		}
	}
	lines, err := DeclarationsNamingNoHolding(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(lines, []int{1, 3}) {
		t.Errorf("lines naming nothing = %v, want [1 3]", lines)
	}
}

// Migration 0135 replays over a gold that holds its tables filled: the
// derived rows and the ledger's stay, and its seeds add nothing to a
// convention table a load has stamped. A fresh gold reads the built-in
// conventions, cycles and horizon before any load.
func TestMigration0135DDLIsRerunnable(t *testing.T) {
	db, ctx := openMigrated(t)
	var conv []string
	for _, c := range canonical.BuiltinConventions() {
		conv = append(conv, strings.Join([]string{c.Market, string(rune('0' + c.Frequency)), string(c.DayCount),
			string(c.Origin)}, " "))
	}
	var cyc []string
	for _, c := range canonical.BuiltinCycles() {
		cyc = append(cyc, strings.Join([]string{string(c.Sector), string(c.Cycle), string(c.Origin)}, " "))
	}
	for q, want := range map[string][]string{
		`SELECT concat_ws(' ', market, frequency, day_count, origin) FROM yield_conventions ORDER BY market`: conv,
		`SELECT concat_ws(' ', sector, cycle, origin) FROM yield_cycles`:                                     cyc,
		`SELECT horizon_months::VARCHAR FROM yield_settings`:                                                 {"12"},
	} {
		got := scanStrings(t, db, q)
		slices.Sort(got)
		slices.Sort(want)
		if !slices.Equal(got, want) {
			t.Errorf("%s:\n got %v\nwant %v", q, got, want)
		}
	}
	if _, err := SyncYield(ctx, db, canonical.YieldRows{
		Conventions: []canonical.YieldConvention{{Market: "GBP", Frequency: 2, DayCount: canonical.DayCountActAct,
			Origin: canonical.OriginConfig}},
		Settings: canonical.YieldSettings{HorizonMonths: 24}}); err != nil {
		t.Fatal(err)
	}
	execEach(t, db,
		`INSERT INTO instrument_terms (silver_source_id, instrument_external_id, coupon_pct) VALUES ('s', 'i', 4)`,
		`INSERT INTO account_rates (silver_source_id, account_external_id, side, tier, rate_pct, origin)
		 VALUES ('s', 'a', 'credit', 1, 0.111, 'stated')`,
		`INSERT INTO yield_declared_ratings (line_no, silver_source_id, instrument_external_id, rating)
		 VALUES (1, 's', 'i', 'AA')`)

	rerunMigrationDDL(t, db, ctx, "0135_yield_terms.sql")

	for table, want := range map[string]int{"instrument_terms": 1, "account_rates": 1, "yield_declared_ratings": 1,
		"yield_conventions": 1, "yield_settings": 1} {
		if got := countRows(t, db, ctx, table); got != want {
			t.Errorf("%s holds %d rows after a replay, want %d", table, got, want)
		}
	}
	if got := scanStrings(t, db, `SELECT rating FROM instrument_terms_at(0)`); !slices.Equal(got, []string{"AA"}) {
		t.Errorf("terms after a replay: %v", got)
	}
}
