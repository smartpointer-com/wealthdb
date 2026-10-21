package gold

import (
	"context"
	"database/sql"
	"math"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
)

// seedYieldExposure is an invented book of 6,000 USD on 2026-06-30: a
// bond its description rates and dates (2,000), cash at a custodian
// (1,000), and three funds of 1,000 each: one of stocks and a floating
// note, one sliced by rating in the config, one whose page breaks its
// debt down by credit quality. The bond and a stock share a LEI.
func seedYieldExposure(t *testing.T) (*sql.DB, context.Context, int64) {
	t.Helper()
	db, ctx := openMigrated(t)
	snap := time.Date(2026, 6, 30, 0, 0, 0, 0, time.UTC).Unix()
	ex := func(q string, args ...any) {
		t.Helper()
		if _, err := db.ExecContext(ctx, q, args...); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	ex(`INSERT INTO accounts (silver_source_id, account_external_id, account_kind, display_name, base_currency,
	    first_seen_at, last_seen_at) VALUES ('test-src', 'A', 'brokerage', 'Brokerage', 'USD', 0, 0)`)
	for _, i := range []struct {
		id, class, vehicle, cusip, symbol, name string
		mv                                      float64
	}{
		{"QB", "fixed_income", "bond", "00000000B", "", "QORVANT CORP 4.5% 01/15/2030 MOODYS A2 S&P BBB+", 2000},
		{"IDXF", "public_equity", "etf", "", "IDXF", "Example Index Fund", 1000},
		{"BNDF", "fixed_income", "etf", "", "BNDF", "Example A Bond Fund", 1000},
		{"BKF", "fixed_income", "etf", "", "BKF", "Example Short Bond Fund", 1000},
	} {
		ex(`INSERT INTO instruments (silver_source_id, instrument_external_id, asset_class, vehicle, cusip, symbol, name,
		    currency, first_seen_at, last_seen_at) VALUES ('test-src', ?, ?, ?, NULLIF(?, ''), NULLIF(?, ''), ?, 'USD', 0, 0)`,
			i.id, i.class, i.vehicle, i.cusip, i.symbol, i.name)
		ex(`INSERT INTO positions (silver_source_id, snapshot_at, account_external_id, position_key, instrument_external_id,
		    asset_class, vehicle, currency, quantity, market_value) VALUES ('test-src', ?, 'A', ?, ?, ?, ?, 'USD', 1, ?)`,
			snap, i.id, i.id, i.class, i.vehicle, i.mv)
	}
	ex(`INSERT INTO cash_balances (silver_source_id, snapshot_at, account_external_id, currency, balance_kind, amount)
	    VALUES ('test-src', ?, 'A', 'USD', 'current', 1000)`, snap)
	type ln struct {
		key, name, assetType string
		w                    float64
		sector, maturity     string
		coupon               any
		kind, lei            string
	}
	comp := func(ref string, lines ...ln) {
		ex(`INSERT INTO fund_compositions (silver_source_id, fund_ref, as_of, basis, provider, line_count, currency)
		    VALUES ('test-src', ?, ?, 'holdings', 'testprov', ?, 'USD')`, ref, snap, len(lines))
		for n, l := range lines {
			payload := "{}"
			if l.lei != "" {
				payload = `{"lei": "` + l.lei + `"}`
			}
			ex(`INSERT INTO fund_lines (silver_source_id, fund_ref, as_of, basis, line_no, security_key, name, asset_type,
			    weight, currency, sector, maturity, coupon, coupon_kind, lei, payload)
			    VALUES ('test-src', ?, ?, 'holdings', ?, ?, ?, ?, ?, 'USD', NULLIF(?, ''), CAST(NULLIF(?, '') AS DATE), ?,
			    NULLIF(?, ''), NULLIF(?, ''), ?)`, ref, snap, n+1, l.key, l.name, l.assetType, l.w, l.sector, l.maturity,
				l.coupon, l.kind, l.lei, payload)
		}
	}
	comp("symbol:IDXF",
		ln{"symbol:AAAA", "Qorvant Corp", "Equity", 50, "Information Technology", "", nil, "", "QQQQ00000000000000Q1"},
		ln{"symbol:BBBB", "Beta Health Co", "Equity", 30, "Health Care", "", nil, "", ""},
		ln{"cusip:00000000N", "EXAMPLE FRN 06/30/2028", "Fixed Income", 20, "", "2028-06-30", 4.0, "", ""})
	comp("symbol:BNDF", ln{"cusip:00000000L", "EXAMPLE LONG BOND 2040", "Fixed Income", 100, "", "2040-01-01", 3.0,
		"Fixed", ""})
	comp("symbol:BKF", ln{"cusip:00000000K", "EXAMPLE SHORT NOTE 2027", "Fixed Income", 100, "", "2027-01-01", 2.0,
		"Fixed", ""})
	ex(`INSERT INTO fund_characteristics (silver_source_id, fund_ref, as_of, provider, field, label, value) VALUES
	    ('ref', 'symbol:BKF', ?, 'ishares', 'credit_quality', 'AAA', 60),
	    ('ref', 'symbol:BKF', ?, 'ishares', 'credit_quality', 'BB', 40),
	    ('ref', 'symbol:BKF', ?, 'ishares', 'credit_quality', 'Cash and/or Derivatives', 10)`, snap, snap, snap)
	ex(`INSERT INTO security_identities (silver_source_id, security_key, as_of, provider, lei, name)
	    VALUES ('ref', 'cusip:00000000B', 0, 'gleif', 'QQQQ00000000000000Q1', 'Qorvant Corporation')`)
	if _, err := SyncExposure(ctx, db, canonical.ExposureRows{
		Custody:  []canonical.ExposureCustody{{SilverSourceID: "test-src", Custodian: "Example Bank AG", Country: "CH"}},
		Settings: canonical.ExposureSettings{MaxGapDays: canonical.DefaultMaxGapDays, AgedDays: 35, PartialPct: 3},
	}); err != nil {
		t.Fatal(err)
	}
	ex(`INSERT INTO yield_rating_funds (fund_ref, rating, bucket_high, bucket_low) VALUES ('symbol:BNDF', 'AA-A', 'AA', 'A')`)
	if _, err := RebuildExposure(ctx, db); err != nil {
		t.Fatal(err)
	}
	if _, err := RebuildYield(ctx, db); err != nil {
		t.Fatal(err)
	}
	return db, ctx, snap + 86399
}

// bucketsOf is a bucket view as "bucket=value" in its order.
func bucketsOf(t *testing.T, rows []ExposureBucketRow) []string {
	t.Helper()
	var out []string
	sum := 0.0
	for _, r := range rows {
		v, _ := strconv.ParseFloat(*r.Value, 64)
		sum += v
		s := r.Bucket + "=" + strconv.FormatFloat(v, 'f', 0, 64)
		if r.Grade != nil {
			s += "/" + *r.Grade
		}
		out = append(out, s)
	}
	if sum < 5999.99 || sum > 6000.01 {
		t.Errorf("the view sums to %v, not net worth", sum)
	}
	return out
}

// Each of the four bucket views sums to net worth. A bond's rating comes
// from its description, a fund line's from its rating fund (the range's
// worse end), else from its fund's page, split by the breakdown's shares
// of its rated debt; a line with none reads unrated, unknown in quality.
func TestExposureYieldViews(t *testing.T) {
	t.Parallel()
	db, ctx, at := seedYieldExposure(t)
	credit, err := ExposureCredit(ctx, db, at, "USD")
	if err != nil {
		t.Fatal(err)
	}
	if got, want := bucketsOf(t, credit), []string{"AAA=600/IG", "A=1000/IG", "BBB=2000/IG", "BB=400/HY",
		"unrated=200", "cash=1000", "(none)=800"}; !slices.Equal(got, want) {
		t.Errorf("credit %v, want %v", got, want)
	}
	if q := credit[4].Quality; !strings.Contains(q, "unknown=100%") {
		t.Errorf("unrated quality %q", q)
	}
	mat, err := ExposureMaturities(ctx, db, at, "USD")
	if err != nil {
		t.Fatal(err)
	}
	if got, want := bucketsOf(t, mat), []string{"under 1y=2000", "1-3y=200", "3-5y=2000", "10-20y=1000",
		"(none)=800"}; !slices.Equal(got, want) {
		t.Errorf("maturities %v, want %v", got, want)
	}
	cpn, err := ExposureCoupons(ctx, db, at, "USD")
	if err != nil {
		t.Fatal(err)
	}
	// The note's name says it floats; the bond's coupon is fixed by
	// convention.
	if got, want := bucketsOf(t, cpn), []string{"fixed=4000", "floating=200", "(none)=1800"}; !slices.Equal(got, want) {
		t.Errorf("coupons %v, want %v", got, want)
	}
	cyc, err := ExposureCycles(ctx, db, at, "USD")
	if err != nil {
		t.Fatal(err)
	}
	if got, want := bucketsOf(t, cyc), []string{"defensive=300", "sensitive=500", "(none)=5200"}; !slices.Equal(got, want) {
		t.Errorf("cycles %v, want %v", got, want)
	}
	iss, err := ExposureIssuers(ctx, db, at, "USD")
	if err != nil {
		t.Fatal(err)
	}
	top := iss[0]
	// The bond and the stock of one LEI are one issuer; the cash is its
	// custodian's.
	if top.Issuer != "Qorvant Corporation" || *top.Value != "2500" || *top.EquityPct != 20 || *top.DebtPct != 80 ||
		*top.CumulativeSharePct < 41.66 || *top.CumulativeSharePct > 41.67 {
		t.Errorf("top issuer %s %v %v %v %v", top.Issuer, *top.Value, *top.EquityPct, *top.DebtPct, *top.CumulativeSharePct)
	}
	var names []string
	for _, r := range iss {
		names = append(names, r.Issuer)
	}
	if !slices.Contains(names, "Example Bank AG") || len(iss) != 6 {
		t.Errorf("issuers %v", names)
	}
}

// The labels a page or a description writes map to buckets, an agency's
// grade to the one canonical.RatingOfGrade gives it, a band to its lower
// bound's bucket, a maturity exactly N years on to the longer bucket, and
// a name to the text the issuers view groups it by.
func TestExposureYieldLabels(t *testing.T) {
	t.Parallel()
	db, ctx := openMigrated(t)
	check := func(q string, cases map[string]string) {
		t.Helper()
		for in, want := range cases {
			var got sql.NullString
			if err := db.QueryRowContext(ctx, q, in).Scan(&got); err != nil {
				t.Fatalf("%s %q: %v", q, in, err)
			}
			if got.String != want {
				t.Errorf("%s %q: %q, want %q", q, in, got.String, want)
			}
		}
	}
	check(`SELECT exposure_rating_label(?)`, map[string]string{"AAA": "AAA", "AA+": "AA", "Aa2": "AA", "Baa1": "BBB",
		"(P)A2": "A", "AA Rated": "AA", "BB-": "BB", "Below B": "B", "B and below": "B", "BB and below": "B",
		"CCC+": "B", "RD": "B", "Not Rated": "unrated", "N/A": "unrated", "Cash and/or Derivatives": "cash",
		"Government": "", "aA": "AA", "bBB-": "BBB", "notRated": "unrated", "belowB": "B",
		"cashand/orDerivatives": "cash", "cCCandbelow": "B", "bBandbelow": "B", "aA1": "AA", "aAA Rated": "AAA",
		"AA1": "AA", "BAA2": "BBB", "A": "A", "BB": "BB"})
	// Every agency's grade, its provisional and starred forms, the grades
	// that rate nothing, and grades of no bucket: the short-term ones.
	grades := map[string]string{}
	for _, g := range []string{"Aaa", "Aa1", "Aa2", "Aa3", "Aa", "A1", "A2", "A3", "Baa1", "Baa2", "Baa3", "Baa",
		"Ba1", "Ba2", "Ba3", "Ba", "B1", "B2", "B3", "Caa1", "Caa2", "Caa3", "Caa", "Ca", "C", "AAA", "AA+", "AA",
		"AA-", "A+", "A", "A-", "BBB+", "BBB", "BBB-", "BB+", "BB", "BB-", "B+", "B", "B-", "CCC+", "CCC", "CCC-",
		"CC", "D", "SD", "RD", "DDD", "DD", "NR", "WR", "WD", "N/R", "Aa1*", "(P)Aa2", "(P)Baa3", "BBB+*", "P-1",
		"A-1+", "SP-1", "F1", "", "GOOD"} {
		b, ok := canonical.RatingOfGrade(g)
		grades[g] = map[bool]string{true: string(b)}[ok]
	}
	check(`SELECT exposure_rating_label(?)`, grades)
	check(`SELECT exposure_maturity_label(?)`, map[string]string{"0 - 1 Years": "under 1y", "1 - 2 Years": "1-3y",
		"2-3 Years": "1-3y", "3 - 5 Years": "3-5y", "7 - 10 Years": "5-10y", "15 - 20 Years": "10-20y",
		"20+ Years": "over 20y", "10+ Years": "", "6-12 Months": "under 1y", "Cash": "cash", "Other": "",
		"> 30 Years": "over 20y", "> 10 Years": "", "Under 1 Year": "under 1y", "< 3 Years": "",
		"5to10Years": "5-10y", "over20Years": "over 20y", "cashand/orDerivatives": "cash", "0 - 1 Year": "under 1y",
		"0-1 Year": "under 1y", "5-10 Years": "5-10y", "1 - 5 Years": "", "10 - 30 Years": "", "1 to 3 Years": "1-3y",
		"10 Years+": "", "20 Years+": "over 20y", "10PlusYears": "", "10 Years and over": "",
		"20 Years and over": "over 20y", "lessthan1Year": "under 1y", "upto1Year": "under 1y",
		"morethan20Years": "over 20y", "greaterthan20Years": "over 20y"})
	for _, day := range []string{"2026-06-30", "2027-03-01"} {
		cases := map[string]string{}
		for _, b := range []struct {
			years int
			at    string
		}{{1, "1-3y"}, {3, "3-5y"}, {5, "5-10y"}, {10, "10-20y"}, {20, "over 20y"}, {100, "perpetual"}} {
			cases[strconv.Itoa(b.years)] = b.at
		}
		check(`SELECT exposure_maturity_bucket(DATE '`+day+`', CAST(DATE '`+day+`' + to_years(CAST(? AS INTEGER)) AS DATE))`,
			cases)
		check(`SELECT exposure_maturity_bucket(DATE '`+day+`', CAST(DATE '`+day+`' + to_years(CAST(? AS INTEGER))
		       - INTERVAL 1 DAY AS DATE))`, map[string]string{"1": "under 1y", "100": "over 20y"})
	}
	check(`SELECT exposure_issuer_name(?)`, map[string]string{"Bérénice Foods SA": "BERENICE FOODS",
		"BERENICE FOODS SA-REG": "BERENICE FOODS", "Østby Straße AG": "OSTBY STRASSE",
		"US TREASURY N/B 9.99% 01/01/2099": "US TREASURY", "UNITED STATES TREASURY NOTE/BOND": "US TREASURY",
		"U.S. TREASURY NOTE": "US TREASURY", "UNITED STATES TREAS BILLS": "US TREASURY",
		"EXAMPLE BANCORP MTN 4% 01/15/2031": "EXAMPLE BANCORP", "QORVANT INC SR UNSECURED 03/30 4.5": "QORVANT",
		"QORVANT HOLDINGS INC CL A": "QORVANT", "Example Co A": "EXAMPLE A", "Example Co B": "EXAMPLE B",
		"THE": "", "THE QORVANT COMPANY": "QORVANT", "Stock Yards Example": "STOCK YARDS EXAMPLE",
		"New Example Fields": "NEW EXAMPLE FIELDS", "Q&R": "Q&R", "9Q CO": "9Q"})
}

// The edges of the views on an invented book: a rating fund named twice
// through an alias counts once; one held with no composition reads its
// range; a page's label no map knows reads unrated, flagged; a fund
// nothing looks through splits by its maturity breakdown; a term's cash
// matures at its term's end; an N-PORT inflation line typed Fixed is
// inflation-linked; a bank's deposits, its time deposit and its bond are
// one issuer; a nameless line keeps a row of its own; a declared private
// holding's parts are no issuer's; the (none) row carries no LEI; a
// fund's name that says perpetual says nothing of its lines.
func TestExposureYieldEdges(t *testing.T) {
	t.Parallel()
	db, ctx := openMigrated(t)
	snap := time.Date(2026, 6, 30, 0, 0, 0, 0, time.UTC).Unix()
	ex := func(q string, args ...any) {
		t.Helper()
		if _, err := db.ExecContext(ctx, q, args...); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	ex(`INSERT INTO accounts (silver_source_id, account_external_id, account_kind, display_name, base_currency,
	    first_seen_at, last_seen_at) VALUES ('bank', 'A', 'brokerage', 'Brokerage', 'USD', 0, 0)`)
	for _, i := range []struct {
		id, class, vehicle, cusip, symbol, name string
		mv                                      float64
	}{
		{"RTGF", "fixed_income", "etf", "", "RTGF", "Example BBB Fund", 1000},
		{"ALF", "fixed_income", "etf", "", "ALF", "Example Aliased Fund", 1000},
		{"PGF", "fixed_income", "etf", "", "PGF", "Example Page Fund", 1000},
		{"TIPF", "fixed_income", "etf", "", "TIPF", "Example Inflation Fund", 1000},
		{"EBB", "fixed_income", "bond", "00000000E", "", "EXAMPLE BANK AG 3% 01/15/2031", 300},
		{"TD1", "cash", "time_deposit", "", "", "Term deposit", 500},
		{"SPV1", "private_equity", "spv", "", "", "Example Feeder SPV", 1000},
		{"TIEF", "fixed_income", "etf", "", "TIEF", "Example Two Lines Fund", 1000},
		{"PERF", "fixed_income", "etf", "", "PERF", "Example Perpetual Income Fund", 1000},
	} {
		ex(`INSERT INTO instruments (silver_source_id, instrument_external_id, asset_class, vehicle, cusip, symbol, name,
		    currency, first_seen_at, last_seen_at) VALUES ('bank', ?, ?, ?, NULLIF(?, ''), NULLIF(?, ''), ?, 'USD', 0, 0)`,
			i.id, i.class, i.vehicle, i.cusip, i.symbol, i.name)
		ex(`INSERT INTO positions (silver_source_id, snapshot_at, account_external_id, position_key, instrument_external_id,
		    asset_class, vehicle, currency, quantity, market_value) VALUES ('bank', ?, 'A', ?, ?, ?, ?, 'USD', 1, ?)`,
			snap, i.id, i.id, i.class, i.vehicle, i.mv)
	}
	ex(`INSERT INTO cash_balances (silver_source_id, snapshot_at, account_external_id, currency, balance_kind, amount)
	    VALUES ('bank', ?, 'A', 'USD', 'current', 1000)`, snap)
	line := func(ref string, n int, key, name string, w float64, maturity, kind, lei string, real any) {
		ex(`INSERT INTO fund_lines (silver_source_id, fund_ref, as_of, basis, line_no, security_key, name, asset_type,
		    weight, currency, maturity, coupon, coupon_kind, lei, real_yield)
		    VALUES ('bank', ?, ?, 'holdings', ?, NULLIF(?, ''), NULLIF(?, ''), 'Fixed Income', ?, 'USD',
		    CAST(NULLIF(?, '') AS DATE), 2.0, NULLIF(?, ''), NULLIF(?, ''), ?)`,
			ref, snap, n, key, name, w, maturity, kind, lei, real)
	}
	for _, c := range []struct {
		ref   string
		lines int
	}{{"symbol:ALF", 1}, {"symbol:TIPF", 2}, {"symbol:TIEF", 2}, {"symbol:PERF", 1}} {
		ex(`INSERT INTO fund_compositions (silver_source_id, fund_ref, as_of, basis, provider, line_count, currency)
		    VALUES ('bank', ?, ?, 'holdings', 'testprov', ?, 'USD')`, c.ref, snap, c.lines)
	}
	line("symbol:ALF", 1, "cusip:00000000A", "EXAMPLE NOTE 2030", 100, "2030-06-30", "Fixed", "", nil)
	line("symbol:TIPF", 1, "cusip:00000000T", "UNITED STATES TREASURY INFLATION INDEXED BONDS", 60, "2032-01-15",
		"Fixed", "", nil)
	line("symbol:TIPF", 2, "", "", 40, "2029-01-15", "Fixed", "QQQQ0000000000000NUL", nil)
	line("symbol:TIEF", 1, "cusip:000000AL3", "ALPHA ISSUER 3% 2030", 50, "2030-06-30", "Fixed", "QQQQ000000000000ALPH", nil)
	line("symbol:TIEF", 2, "cusip:000000AL4", "ALPHA ISSUER INC 4% 2032", 50, "2032-06-30", "Fixed",
		"QQQQ000000000000ALPH", nil)
	line("symbol:PERF", 1, "", "", 100, "", "Fixed", "", nil)
	ex(`INSERT INTO fund_characteristics (silver_source_id, fund_ref, as_of, provider, field, label, value) VALUES
	    ('ref', 'symbol:PGF', ?, 'ishares', 'credit_quality', 'Government', 60),
	    ('ref', 'symbol:PGF', ?, 'ishares', 'credit_quality', 'AA Rated', 40),
	    ('ref', 'symbol:PGF', ?, 'ishares', 'maturity_breakdown', '0 - 1 Years', 50),
	    ('ref', 'symbol:PGF', ?, 'ishares', 'maturity_breakdown', '3 - 5 Years', 50),
	    ('ref', 'symbol:RTGF', ?, 'ishares', 'credit_quality', 'AA', 100)`, snap, snap, snap, snap, snap)
	ex(`INSERT INTO security_identities (silver_source_id, security_key, as_of, provider, lei, name) VALUES
	    ('ref', 'cusip:00000000E', 0, 'gleif', 'QQQQ0000000000000EB1', 'Example Bank AG')`)
	spv := declaration(1, "SPV1", nil, split{"asset_class", []string{"public_equity", "fixed_income"}, []float64{60, 40}})
	spv.SilverSourceID = "bank"
	if _, err := SyncExposure(ctx, db, canonical.ExposureRows{
		Custody:      []canonical.ExposureCustody{{SilverSourceID: "bank", Custodian: "Example Bank AG", Country: "CH"}},
		Declarations: []canonical.ExposureDeclaration{spv},
		Settings:     canonical.ExposureSettings{MaxGapDays: canonical.DefaultMaxGapDays, AgedDays: 35, PartialPct: 3},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := RebuildExposure(ctx, db); err != nil {
		t.Fatal(err)
	}
	if _, err := RebuildYield(ctx, db); err != nil {
		t.Fatal(err)
	}
	// After the passes: the term's rate as the yield pass leaves it, the
	// alias and the two rating keys of one fund.
	ex(`INSERT INTO account_rates (silver_source_id, account_external_id, currency, side, tier, rate_pct, term_end,
	    origin) VALUES ('bank', 'A', 'USD', 'credit', 1, 3.0, '2028-06-30', 'stated')`)
	ex(`INSERT OR REPLACE INTO security_aliases (alias, security_key, origin) VALUES ('isin:XX00000000AL', 'symbol:ALF', 'lookup')`)
	ex(`INSERT INTO yield_rating_funds (fund_ref, rating, bucket_high, bucket_low) VALUES
	    ('symbol:ALF', 'A', 'A', 'A'), ('isin:XX00000000AL', 'AA-BBB', 'AA', 'BBB'), ('symbol:RTGF', 'BBB', 'BBB', 'BBB')`)
	at := snap + 86399
	total := 8800.0
	sumOf := func(name string, rows []ExposureBucketRow, err error) map[string]float64 {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		out, sum := map[string]float64{}, 0.0
		for _, r := range rows {
			v, _ := strconv.ParseFloat(*r.Value, 64)
			out[r.Bucket] = v
			sum += v
		}
		if math.Abs(sum-total) > 0.01 {
			t.Errorf("%s sums to %v, not %v: %v", name, sum, total, out)
		}
		return out
	}
	view := func(name string, read func(context.Context, *sql.DB, int64, string) ([]ExposureBucketRow, error)) map[string]float64 {
		t.Helper()
		rows, err := read(ctx, db, at, "USD")
		return sumOf(name, rows, err)
	}
	// ALF's range under its two keys reads its worse end, BBB, once;
	// RTGF its own over its page's AA; PGF's page: AA 40%, its Government
	// 60% unrated; the cash and the time deposit cash.
	if c := view("credit", ExposureCredit); c["BBB"] != 2000 || c["AA"] != 400 || c["cash"] != 1500 ||
		c["unrated"] != 600+1000+300+400+1000+1000 {
		t.Errorf("credit %v", c)
	}
	// ALF's note (four years on) and the bond mature in 3-5 years, PGF
	// half under a year and half in 3-5, the cash and the time deposit at
	// their account's term end two years on in 1-3.
	if m := view("maturities", ExposureMaturities); m["under 1y"] != 500 || m["1-3y"] != 400+1000+500 ||
		m["3-5y"] != 1000+500+300+500 || m["5-10y"] != 600+500 || m["unknown"] != 1000+400+1000 {
		t.Errorf("maturities %v", m)
	}
	if k := view("coupons", ExposureCoupons); k["inflation_linked"] != 600 {
		t.Errorf("coupons %v", k)
	}
	iss, err := ExposureIssuers(ctx, db, at, "USD")
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]ExposureIssuerRow{}
	for _, r := range iss {
		byName[r.Issuer] = r
	}
	if b := byName["Example Bank AG"]; *b.Value != "1800" || *b.CashPct < 83.3 || *b.CashPct > 83.4 || b.LEI == nil {
		t.Errorf("the bank %+v", b)
	}
	if n := byName["(none)"]; n.LEI != nil {
		t.Errorf("(none) has an LEI: %+v", n)
	}
	if _, ok := byName["QQQQ0000000000000NUL"]; !ok {
		t.Errorf("the nameless line has no row: %v", byName)
	}
	// Two lines of one LEI print one name, whatever their order.
	if a := byName["ALPHA ISSUER"]; a.Value == nil || *a.Value != "1000" {
		t.Errorf("one LEI's lines: %v", byName)
	}
	// The parts a declaration splits the SPV into are no issuer's.
	if _, ok := byName["Example Feeder SPV"]; ok || *byName["(none)"].Value != "3000" {
		t.Errorf("the declared SPV: %+v", byName)
	}
}

// Migration 0137 replays over a gold whose fund lines it has read: the
// LEI a payload states as text fills the column, once; any other reads
// none.
func TestMigration0137DDLIsRerunnable(t *testing.T) {
	t.Parallel()
	db, ctx := openMigrated(t)
	for i, payload := range []string{`{"lei":"QQQQ00000000000000L1"}`, `{"lei":""}`, `{"lei":12}`, `{"l":1}`, ``} {
		if _, err := db.ExecContext(ctx, `INSERT INTO fund_lines (silver_source_id, fund_ref, as_of, basis, line_no,
		    weight, payload) VALUES ('s', 'symbol:F', 0, 'holdings', ?, 1, NULLIF(?, '')::JSON)`, i+1, payload); err != nil {
			t.Fatal(err)
		}
	}
	for range 2 {
		rerunMigrationDDL(t, db, ctx, "0137_exposure_yield_views.sql")
	}
	if got := scanStrings(t, db, `SELECT COALESCE(lei, '-') FROM fund_lines ORDER BY line_no`); strings.Join(got, " ") !=
		"QQQQ00000000000000L1 - - - -" {
		t.Errorf("lei %v", got)
	}
}

// The dashboard's debt and issuer tables are the views': in every
// reporting currency, each dimension's buckets at an instant are the CLI
// view's, and the issuers its rows, in rank order with their shares.
func TestMaterializeExposureDebtMatchesTheViews(t *testing.T) {
	t.Parallel()
	db, ctx, at := seedYieldBookFX(t)
	if _, err := MaterializeExposure(ctx, db, at, 42); err != nil {
		t.Fatal(err)
	}
	for _, ccy := range materializeCurrencies {
		for dim, read := range map[string]func(context.Context, *sql.DB, int64, string) ([]ExposureBucketRow, error){
			debtDimensionCredit: ExposureCredit, debtDimensionMaturity: ExposureMaturities,
			debtDimensionCoupon: ExposureCoupons, debtDimensionCycle: ExposureCycles} {
			rows, err := read(ctx, db, at, ccy)
			if err != nil {
				t.Fatal(err)
			}
			var want []string
			for _, r := range rows {
				v, _ := strconv.ParseFloat(*r.Value, 64)
				want = append(want, r.Bucket+"="+strconv.FormatFloat(v, 'f', 4, 64))
			}
			slices.Sort(want)
			got := scanStrings(t, db, `SELECT bucket || '=' || printf('%.4f', SUM(value)) FROM report_exposure_debt
			    WHERE as_of = ? AND currency = ? AND dimension = ? GROUP BY bucket ORDER BY 1`, at, ccy, dim)
			if strings.Join(got, " ") != strings.Join(want, " ") {
				t.Errorf("%s %s: %v, want %v", ccy, dim, got, want)
			}
		}
		iss, err := ExposureIssuers(ctx, db, at, ccy)
		if err != nil {
			t.Fatal(err)
		}
		got := scanStrings(t, db, `SELECT issuer || '=' || printf('%.4f', value) || '/' || printf('%.2f', share_pct) || '/'
		    || COALESCE(printf('%.2f', cumulative_share_pct), '-') || '/' || printf('%.1f', debt_pct)
		    FROM report_exposure_issuer_ranks WHERE as_of = ? AND currency = ? ORDER BY rank`, at, ccy)
		var want []string
		for _, r := range iss {
			v, _ := strconv.ParseFloat(*r.Value, 64)
			cum := "-"
			if r.CumulativeSharePct != nil {
				cum = strconv.FormatFloat(*r.CumulativeSharePct, 'f', 2, 64)
			}
			want = append(want, r.Issuer+"="+strconv.FormatFloat(v, 'f', 4, 64)+"/"+
				strconv.FormatFloat(*r.SharePct, 'f', 2, 64)+"/"+cum+"/"+strconv.FormatFloat(*r.DebtPct, 'f', 1, 64))
		}
		if strings.Join(got, " ") != strings.Join(want, " ") {
			t.Errorf("%s issuers %v, want %v", ccy, got, want)
		}
	}
}

// A fund line of a kind the yield report reads as floating
// (floatingLineKinds) is floating in the coupons view too, and one it
// does not is not. The fund is invented.
func TestFloatingLineKindsAgree(t *testing.T) {
	t.Parallel()
	db, ctx := openMigrated(t)
	snap := time.Date(2026, 6, 30, 0, 0, 0, 0, time.UTC).Unix()
	kinds := append(slices.Clone(floatingLineKinds), "Fixed")
	q := []string{
		`INSERT INTO accounts (silver_source_id, account_external_id, account_kind, display_name, base_currency,
		    first_seen_at, last_seen_at) VALUES ('test-src', 'A', 'brokerage', 'Brokerage', 'USD', 0, 0)`,
		`INSERT INTO instruments (silver_source_id, instrument_external_id, asset_class, vehicle, symbol, name,
		    currency, first_seen_at, last_seen_at)
		    VALUES ('test-src', 'QFL', 'fixed_income', 'etf', 'QFL', 'Example Note Fund', 'USD', 0, 0)`,
		`INSERT INTO positions (silver_source_id, snapshot_at, account_external_id, position_key, instrument_external_id,
		    asset_class, vehicle, currency, quantity, market_value)
		    VALUES ('test-src', ` + itoa(snap) + `, 'A', 'QFL', 'QFL', 'fixed_income', 'etf', 'USD', 1, 3000)`,
		`INSERT INTO fund_compositions (silver_source_id, fund_ref, as_of, basis, provider, line_count, currency)
		    VALUES ('test-src', 'symbol:QFL', ` + itoa(snap) + `, 'holdings', 'testprov', ` + strconv.Itoa(len(kinds)) +
			`, 'USD')`,
	}
	for i, k := range kinds {
		q = append(q, `INSERT INTO fund_lines (silver_source_id, fund_ref, as_of, basis, line_no, security_key, name,
		    asset_type, weight, currency, maturity, coupon, coupon_kind) VALUES ('test-src', 'symbol:QFL', `+itoa(snap)+
			`, 'holdings', `+strconv.Itoa(i+1)+`, 'cusip:0000000`+strconv.Itoa(i)+`Z', 'EXAMPLE NOTE', 'Fixed Income', `+
			strconv.FormatFloat(100/float64(len(kinds)), 'f', -1, 64)+`, 'USD', DATE '2030-01-15', 4, '`+k+`')`)
	}
	execEach(t, db, q...)
	if _, err := SyncExposure(ctx, db, canonical.ExposureRows{
		Settings: canonical.ExposureSettings{MaxGapDays: canonical.DefaultMaxGapDays, AgedDays: 35, PartialPct: 3},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := RebuildExposure(ctx, db); err != nil {
		t.Fatal(err)
	}
	got := scanStrings(t, db, `SELECT line_coupon_kind || ' ' || coupon_bucket FROM exposure_yield_leaves(?, 'USD')
	    WHERE depth > 0 AND line_coupon_kind IS NOT NULL ORDER BY 1`, snap+86399)
	var want []string
	for _, k := range kinds {
		bucket := "fixed"
		if floatingLine(k) {
			bucket = "floating"
		}
		want = append(want, k+" "+bucket)
	}
	slices.Sort(want)
	if strings.Join(got, ", ") != strings.Join(want, ", ") || floatingLine("Fixed") {
		t.Errorf("line kinds %v, want %v", got, want)
	}
}
