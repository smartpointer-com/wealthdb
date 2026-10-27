package gold

import (
	"context"
	"database/sql"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/yield"
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
// too. A rating record clears the description's default, and its day is
// no terms record's. A stock no record names has none.
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
		 ('test-src', 'QRCV', 'fixed_income', 'bond', NULL, NULL, 'QORVANT CORP 7% 05/01/2032 S&P D', 'USD', NULL, 0, 0),
		 ('test-src', 'QLFT', 'fixed_income', 'bond', NULL, NULL, 'QORVANT CORP 4.2% 03/15/2031', 'USD', NULL, 0, 0),
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
		`INSERT INTO yield_declared_terms (line_no, silver_source_id, instrument_external_id, coupon_kind)
		 VALUES (7, 'test-src', 'QLFT', 'floating')`,
		`INSERT INTO yield_declared_ratings (line_no, silver_source_id, instrument_external_id, as_of, rating)
		 VALUES (5, 'test-src', 'QDECL', NULL, 'BBB'), (6, 'test-src', 'QRCV', 1700000000, 'BB')`)
	n, err := RebuildYield(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	if n.Instruments != 10 || n.WithMaturity != 8 {
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
		// A record that makes a bond floating drops its market's
		// frequency: it resets quarterly.
		"QLFT 4.2@description 2031-03-15@description - floating@declared 30/360@convention - - - - - - - -",
		"QMUNI 4.25@openfigi 2031-06-15@description 2@description fixed@description 30/360@convention " +
			"2027-06-15@description - AA@description - - - - -",
		"QMW 5.0@description 2033-03-01@description 2@convention fixed@convention 30/360@convention - make_whole " +
			"B@description in_default - - - -",
		"QNOTE 6.0@payload 2027-03-31@payload - - - - - - - - - - -",
		"QRCV 7.0@description 2032-05-01@description 2@convention fixed@convention 30/360@convention - - " +
			"BB@declared - - - - -",
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

// A source's own figures (canonical.PayloadTermsKeys) come from the
// instrument's payload, else from the newest position whose payload
// states one: a statement's holding outlives a live feed that describes
// the instrument later without them. They outrank the description field
// by field. The grades give the worst bucket and say whether the bond is
// in default. An unrated grade counts only where no origin grades the
// bond. A UBS position's Rating is a grade too, read off its newest
// position. A stated call date clears the description's make-whole call.
// The figures are invented, in the shapes the statements print.
func TestYieldPassReadsPayloadTerms(t *testing.T) {
	t.Parallel()
	db, ctx := openMigrated(t)
	execEach(t, db,
		`INSERT INTO instruments (silver_source_id, instrument_external_id, asset_class, vehicle, name, currency,
		 payload, first_seen_at, last_seen_at) VALUES
		 ('test-src', 'QSTMT', 'fixed_income', 'bond', 'QLAND CNTY GO BDS 06/15/31 FIXED COUPON MOODYS A2 S&P A '
		  || 'QUARTERLY CUSIP: 00000000E', 'USD', '{"securityDescription": "QLAND CNTY GO BDS"}', 0, 0),
		 ('test-src', 'QSVB', 'fixed_income', 'bond', 'EXAMPLE CORP NOTE CALL MAKE WHOLE', 'USD', NULL, 0, 0),
		 ('test-src', 'QCONT', 'fixed_income', 'bond', 'QORVANT CORP 5% 03/01/2033 CALL 03/01/2028', 'USD', NULL, 0, 0),
		 ('test-src', 'QDFLT', 'fixed_income', 'bond', 'QORVANT CORP 6% 01/15/2029 S&P AA', 'USD', NULL, 0, 0),
		 ('test-src', 'QCURED', 'fixed_income', 'bond', 'QORVANT CORP 4% 01/15/2031 S&P D', 'USD', NULL, 0, 0),
		 ('test-src', 'QZERO', 'fixed_income', 'bond', 'QLAND CNTY SCHOOL BDS', 'USD', NULL, 0, 0),
		 ('test-src', 'QWORD', 'fixed_income', 'bond', 'QORVANT CORP NOTE', 'USD', NULL, 0, 0),
		 ('test-src', 'QOWN', 'fixed_income', 'bond', 'QORVANT CORP NOTE', 'USD', '{"rating_sp": "A"}', 0, 0),
		 ('test-src', 'QNR', 'fixed_income', 'bond', 'QORVANT CORP 4% 01/15/2031 S&P D', 'USD', NULL, 0, 0),
		 ('test-ubs', 'XX0000000061', 'fixed_income', 'bond', 'Example Ltd Notes', 'CHF', NULL, 0, 0),
		 ('test-ubs', 'XX0000000062', 'fixed_income', 'bond', 'Example Ltd Notes', 'CHF', NULL, 0, 0),
		 ('test-ubs', 'XX0000000063', 'fixed_income', 'bond', 'Example Ltd Notes', 'CHF', NULL, 0, 0)`,
		`INSERT INTO positions (silver_source_id, snapshot_at, account_external_id, position_key, instrument_external_id,
		 asset_class, vehicle, currency, quantity, market_value, payload) VALUES
		 ('test-src', 100, 'ACC1', 'QSTMT', 'QSTMT', 'fixed_income', 'bond', 'USD', 1000, 1000,
		  '{"rating_moodys": "Baa1", "coupon_frequency": "ANNUALLY"}'),
		 ('test-src', 200, 'ACC1', 'QSTMT', 'QSTMT', 'fixed_income', 'bond', 'USD', 1000, 1000,
		  '{"coupon_kind": "FIXED", "rating_moodys": "Aa2", "rating_sp": "AA-", "coupon_frequency": "SEMIANNUALLY",
		    "interest_rate": 4.0}'),
		 ('test-src', 300, 'ACC1', 'QSTMT', 'QSTMT', 'fixed_income', 'bond', 'USD', 1000, 1000,
		  '{"securityDescription": "QLAND CNTY GO BDS"}'),
		 ('test-src', 100, 'ACC1', 'QSVB', 'QSVB', 'fixed_income', 'bond', 'USD', 1000, 1000,
		  '{"interest_rate": 3.0, "maturity_date": "06/30/2027", "rating_moodys": "Aaa", "rating_sp": "NR",
		    "coupon_frequency": "SEMI-ANNUAL", "coupon_dates": "JUN 30, DEC 30", "call": "CALLABLE ON 06/30/2025",
		    "call_date": "06/30/2025"}'),
		 ('test-src', 100, 'ACC1', 'QCONT', 'QCONT', 'fixed_income', 'bond', 'USD', 1000, 1000,
		  '{"interest_rate": 5, "rating_sp": "NR", "call": "CONTINUOUSLY CALLABLE"}'),
		 ('test-src', 100, 'ACC1', 'QDFLT', 'QDFLT', 'fixed_income', 'bond', 'USD', 1000, 1000,
		  '{"rating_moodys": "Ca", "rating_sp": "D"}'),
		 ('test-src', 100, 'ACC1', 'QCURED', 'QCURED', 'fixed_income', 'bond', 'USD', 1000, 1000,
		  '{"rating_sp": "BBB"}'),
		 ('test-src', 100, 'ACC1', 'QZERO', 'QZERO', 'fixed_income', 'bond', 'USD', 1000, 1000,
		  '{"coupon_kind": "ZERO", "coupon_frequency": "AT MATURITY", "maturity_date": "2030-12-01"}'),
		 ('test-src', 100, 'ACC1', 'QWORD', 'QWORD', 'fixed_income', 'bond', 'USD', 1000, 1000,
		  '{"interest_rate": "4.5%", "coupon_frequency": "WEEKLY"}'),
		 ('test-src', 100, 'ACC1', 'QOWN', 'QOWN', 'fixed_income', 'bond', 'USD', 1000, 1000,
		  '{"rating_sp": "BB", "coupon_kind": "FLOATING"}'),
		 ('test-src', 100, 'ACC1', 'QNR', 'QNR', 'fixed_income', 'bond', 'USD', 1000, 1000,
		  '{"rating_moodys": "WR", "rating_sp": "NR"}'),
		 ('test-ubs', 100, 'SAFE1', 'XX0000000061', 'XX0000000061', 'fixed_income', 'bond', 'CHF', 1000, 1000,
		  '{"web": {"Rating": "A+"}}'),
		 ('test-ubs', 200, 'SAFE1', 'XX0000000061', 'XX0000000061', 'fixed_income', 'bond', 'CHF', 1000, 1000,
		  '{"web": {"Rating": "AA-"}}'),
		 ('test-ubs', 100, 'SAFE1', 'XX0000000062', 'XX0000000062', 'fixed_income', 'bond', 'CHF', 1000, 1000,
		  '{"web": {"Rating": "No rating av."}}'),
		 ('test-ubs', 100, 'SAFE1', 'XX0000000063', 'XX0000000063', 'fixed_income', 'bond', 'CHF', 1000, 1000,
		  '{"web": {"Rating": "BBB"}}'),
		 ('test-ubs', 200, 'SAFE1', 'XX0000000063', 'XX0000000063', 'fixed_income', 'bond', 'CHF', 1000, 1000,
		  '{"web": {"Rating": "No rating av."}}')`)
	if _, err := RebuildYield(ctx, db); err != nil {
		t.Fatal(err)
	}
	got := scanStrings(t, db, termsText, time.Date(2026, 6, 30, 0, 0, 0, 0, time.UTC).Unix())
	want := []string{
		// Continuously callable with no date: the call is the description's.
		"QCONT 5.0@payload 2033-03-01@description 2@convention fixed@convention 30/360@convention " +
			"2028-03-01@description - unrated@payload - - - - -",
		// Not in default, though the description grades it D: the source's
		// own grade wins.
		"QCURED 4.0@description 2031-01-15@description 2@convention fixed@convention 30/360@convention - - " +
			"BBB@payload - - - - -",
		"QDFLT 6.0@description 2029-01-15@description 2@convention fixed@convention 30/360@convention - - " +
			"B@payload in_default - - - -",
		// An agency's word that it rates nothing yields to the
		// description's grade, which says whether the bond is in default.
		"QNR 4.0@description 2031-01-15@description 2@convention fixed@convention 30/360@convention - - " +
			"B@description in_default - - - -",
		// The instrument's own payload wins key by key over its position's.
		// A floating note takes no frequency by convention: it resets
		// quarterly where nothing states one.
		"QOWN - - - floating@payload 30/360@convention - - A@payload - - - - -",
		// The statement's terms, not the live feed's newer payload or an
		// older statement's, and over the description's grades and frequency.
		"QSTMT 4.0@payload 2031-06-15@description 2@payload fixed@payload 30/360@convention - - AA@payload - - - - -",
		"QSVB 3.0@payload 2027-06-30@payload 2@payload fixed@convention 30/360@convention 2025-06-30@payload - " +
			"AAA@payload - - - - -",
		// An unknown frequency word states none.
		"QWORD 4.5@payload - 2@convention fixed@convention 30/360@convention - - - - - - - -",
		// AT MATURITY states no frequency: a zero is a bond, not a bill.
		"QZERO 0.0@payload 2030-12-01@payload 2@convention zero@payload 30/360@convention - - - - - - - -",
		"XX0000000061 - - 1@convention - 30/360@convention - - AA@payload - - - - -",
		// Text that is no grade is no rating, also where an older position
		// stated one.
		"XX0000000062 - - 1@convention - 30/360@convention - - - - - - - -",
		"XX0000000063 - - 1@convention - 30/360@convention - - - - - - - -",
	}
	if !slices.Equal(got, want) {
		t.Errorf("terms:\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	// The derived table states no frequency for the floater either, and
	// no kind where only the convention gives one.
	if got := scanStrings(t, db, `SELECT concat_ws(' ', instrument_external_id, COALESCE(frequency::VARCHAR, '-'),
	    COALESCE(coupon_kind, '-')) FROM instrument_terms WHERE instrument_external_id IN ('QOWN', 'QWORD')
	    ORDER BY 1`); strings.Join(got, ", ") != "QOWN - floating, QWORD 2 -" {
		t.Errorf("instrument_terms: %v", got)
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

// A rate a payload states under the canonical keys is every source's:
// a flat rate on an account, stated once; tiers on a cash balance,
// observed at its snapshot; a credit and a debit side on one account; a
// rate that holds from a later day, the older rate in force until then.
// An account that states none has no row.
func TestYieldPassReadsTheStatedRateKeys(t *testing.T) {
	t.Parallel()
	db, ctx := openMigrated(t)
	day := time.Date(2026, 6, 30, 0, 0, 0, 0, time.UTC).Unix()
	q := `INSERT INTO cash_balances (silver_source_id, snapshot_at, account_external_id, currency, balance_kind, amount,
	 payload) VALUES
	 ('test-src', ?, 'TIER', 'USD', 'closing', 1000, '{"interest_tiers": [{"rate": 0.5, "up_to": 10000},
	   {"rate": "1.25%", "up_to": null}], "interest_valid_from": "2026-06-01"}'),
	 ('test-src', ?, 'TIER', 'USD', 'current', 1000, '{"interest_rate": 9.99}'),
	 ('test-src', ?, 'TIER', 'USD', 'opening', 1000, '{"basis": "statement"}'),
	 ('test-src', ?, 'BOTH', 'EUR', 'closing', 500, '{"interest_conditions": [
	   {"interest_rate": 0.1, "interest_side": "credit"}, {"interest_rate": "6.5", "interest_side": "debit"}]}'),
	 ('test-src', ?, 'LATER', 'USD', 'closing', 700, '{"interest_rate": 2, "interest_valid_from": "2026-09-01"}'),
	 ('test-src', ?, 'LATER', 'USD', 'closing', 700, '{"interest_rate": 1.5}'),
	 ('test-src', ?, 'BAD', 'USD', 'closing', 50, '{"interest_rate": "n/a", "interest_tiers": [{"rate": 1}],
	   "interest_side": "sideways"}')`
	if _, err := db.ExecContext(ctx, q, day, day-86400, day, day, day, day-86400, day); err != nil {
		t.Fatal(err)
	}
	execEach(t, db,
		`INSERT INTO accounts (silver_source_id, account_external_id, account_kind, display_name, base_currency,
		 first_seen_at, last_seen_at, payload) VALUES
		 ('test-src', 'FLAT', 'cash', 'Flat', 'CHF', 0, 0, '{"interest_rate": "0.75"}'),
		 ('test-src', 'TIER', 'cash', 'Tier', 'USD', 0, 0, NULL),
		 ('test-src', 'BOTH', 'cash', 'Both', 'EUR', 0, 0, NULL),
		 ('test-src', 'LATER', 'cash', 'Later', 'USD', 0, 0, NULL),
		 ('test-src', 'NONE', 'cash', 'None', 'CHF', 0, 0, '{"basis": "statement"}'),
		 ('test-src', 'BAD', 'cash', 'Bad', 'USD', 0, 0, NULL)`)
	n, err := RebuildYield(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	if n.RatedAccounts != 4 {
		t.Errorf("rated accounts %d, want 4", n.RatedAccounts)
	}
	got := scanStrings(t, db, `SELECT concat_ws(' ', account_external_id, COALESCE(currency, '-'), side, tier,
	    rate_pct, COALESCE(up_to::VARCHAR, '-'), COALESCE(valid_from::VARCHAR, '-'), origin, COALESCE(field, '-'),
	    COALESCE(strftime(to_timestamp(observed_at) AT TIME ZONE 'UTC', '%m-%d'), '-'))
	  FROM account_rates ORDER BY account_external_id, observed_at, side, tier`)
	want := []string{
		"BOTH EUR credit 1 0.1 - - stated interest_conditions[0].interest_rate 06-30",
		"BOTH EUR debit 1 6.5 - - stated interest_conditions[1].interest_rate 06-30",
		"FLAT CHF credit 1 0.75 - - stated interest_rate -",
		"LATER USD credit 1 1.5 - - stated interest_rate 06-29",
		"LATER USD credit 1 2.0 - 2026-09-01 stated interest_rate 06-30",
		// The day before, the current balance's own; on the day, of the
		// snapshot's balances that state a rate, the closing one.
		"TIER USD credit 1 9.99 - - stated interest_rate 06-29",
		"TIER USD credit 1 0.5 10000.0 2026-06-01 stated interest_tiers 06-30",
		"TIER USD credit 2 1.25 - 2026-06-01 stated interest_tiers 06-30",
	}
	if !slices.Equal(got, want) {
		t.Errorf("account rates:\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	rates := func(asOf int64, acct string) []string {
		in, err := YieldInputs(ctx, db, asOf, "USD")
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, r := range in.Rates[yield.AccountKey{Source: "test-src", Account: acct}] {
			out = append(out, strconv.FormatFloat(r.Pct, 'f', -1, 64)+map[bool]string{true: "c", false: "d"}[r.Credit])
		}
		return out
	}
	later := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC).Unix()
	// Until its day, the rate an older snapshot states stays in force.
	if got := rates(day+3600, "LATER"); !slices.Equal(got, []string{"1.5c"}) {
		t.Errorf("a rate before its day %v", got)
	}
	if got := rates(later, "LATER"); !slices.Equal(got, []string{"2c"}) {
		t.Errorf("a rate from its day %v", got)
	}
	if got := rates(later, "BOTH"); !slices.Equal(got, []string{"0.1c", "6.5d"}) {
		t.Errorf("both sides %v", got)
	}
	if got := rates(later, "NONE"); got != nil {
		t.Errorf("an account that states no rate %v", got)
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
// conventions and cycles before any load.
func TestMigration0135DDLIsRerunnable(t *testing.T) {
	db, ctx := openMigrated(t)
	var conv []string
	for _, c := range canonical.BuiltinConventions() {
		conv = append(conv, strings.Join([]string{c.Market, strconv.Itoa(c.Frequency), string(c.DayCount),
			string(c.Origin)}, " "))
	}
	var cyc []string
	for _, c := range canonical.BuiltinCycles() {
		cyc = append(cyc, strings.Join([]string{string(c.Sector), string(c.Cycle), string(c.Origin)}, " "))
	}
	for q, want := range map[string][]string{
		`SELECT concat_ws(' ', market, frequency, day_count, origin) FROM yield_conventions ORDER BY market`: conv,
		`SELECT concat_ws(' ', sector, cycle, origin) FROM yield_cycles`:                                     cyc,
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
			Origin: canonical.OriginConfig}}}); err != nil {
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
		"yield_conventions": 1} {
		if got := countRows(t, db, ctx, table); got != want {
			t.Errorf("%s holds %d rows after a replay, want %d", table, got, want)
		}
	}
	if got := scanStrings(t, db, `SELECT rating FROM instrument_terms_at(0)`); !slices.Equal(got, []string{"AA"}) {
		t.Errorf("terms after a replay: %v", got)
	}
}

// A bond whose instrument states its terms in its payload, with no name,
// reads them with origin payload. The bond is invented.
func TestABondsPayloadTermsAloneGiveItsTerms(t *testing.T) {
	t.Parallel()
	db, ctx := openMigrated(t)
	execEach(t, db, `INSERT INTO instruments (silver_source_id, instrument_external_id, asset_class, vehicle, cusip,
	    currency, payload, first_seen_at, last_seen_at) VALUES ('schwab', '000000007', 'fixed_income', 'bond',
	    '000000007', 'USD', '{"interest_rate":"9.75","maturity_date":"2098-08-08"}', 0, 0)`)
	if _, err := RebuildYield(ctx, db); err != nil {
		t.Fatal(err)
	}
	got := strings.Join(scanStrings(t, db, `SELECT concat_ws(' ', coupon_pct, coupon_origin, maturity, maturity_origin)
	    FROM instrument_terms_at(?)`, 1), "|")
	if got != "9.75 payload 2098-08-08 payload" {
		t.Errorf("terms %q", got)
	}
}
