package gold

import (
	"context"
	"database/sql"
	"math"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
)

// The rule takes the first basis that applies, in the order of
// docs/EXPOSURE.md §13.
func TestSitusRuleOrder(t *testing.T) {
	t.Parallel()
	all := situsFacts{receipt: true, share: "NL", isin: "US", domicile: "IE", banked: true, custody: "CH", private: true}
	for _, c := range []struct {
		name              string
		f                 situsFacts
		declared, country string
		situs, basis      string
	}{
		{"a declared situs wins over every other", all, "KY", "DE", "KY", situsDeclared},
		{"a coin's ticker names no issuer", situsFacts{coin: true, isin: "US"}, "", "", "", situsUnknown},
		{"a declared coin", situsFacts{coin: true}, "KY", "", "KY", situsDeclared},
		{"a receipt reads as its share", all, "", "", "NL", situsUnderlying},
		{"a receipt without its share has none, its ISIN notwithstanding",
			situsFacts{receipt: true, isin: "US", domicile: "US"}, "", "", "", situsUnknown},
		{"the ISIN prefix outranks a domicile", situsFacts{isin: "IE", domicile: "LU"}, "", "", "IE", situsISIN},
		{"a domicile serves a holding with no ISIN of a country", situsFacts{domicile: "LU", banked: true, custody: "CH"},
			"", "", "LU", situsDomicile},
		{"cash sits at its custodian", situsFacts{banked: true, custody: "CH"}, "", "", "CH", situsCustody},
		{"a custodian with no country", situsFacts{banked: true}, "", "", "", situsUnknown},
		{"an ETF at a bank is not the bank's", situsFacts{custody: "CH"}, "", "", "", situsUnknown},
		{"a private holding's one declared country", situsFacts{private: true}, "", "DE", "DE", situsCountry},
		{"a listed security's declared country is its risk, not its situs", situsFacts{}, "", "DE", "", situsUnknown},
		{"a private holding no record names", situsFacts{private: true}, "", "", "", situsUnknown},
	} {
		if situs, basis := situsOf(c.f, c.declared, c.country); situs != c.situs || basis != c.basis {
			t.Errorf("%s: %q %s, want %q %s", c.name, situs, basis, c.situs, c.basis)
		}
	}
}

// A declared situs sticks until a later record states another; the
// country rule reads the economic record in force; a reading starts
// only where it changes.
func TestSitusReadings(t *testing.T) {
	t.Parallel()
	day := func(d int64) *int64 { return &d }
	private := situsFacts{private: true}
	text := func(rs []situsReading) string {
		var b []string
		for _, r := range rs {
			at := "-"
			if r.asOf != nil {
				at = strconv.FormatInt(*r.asOf, 10)
			}
			b = append(b, at+":"+r.situs+"/"+r.basis)
		}
		return strings.Join(b, " ")
	}
	for _, c := range []struct {
		name    string
		f       situsFacts
		records []situsRecord
		want    string
	}{
		{"no record", private, nil, "-:/unknown"},
		{"an undated country, then a dated split that states none", private, []situsRecord{
			{country: "CH", economic: true}, {asOf: day(5), economic: true}}, "-:CH/country 5:/unknown"},
		{"a situs-only record leaves the country rule's record in force", private, []situsRecord{
			{country: "CH", economic: true}, {asOf: day(5), situs: "KY"}}, "-:CH/country 5:KY/declared"},
		{"a later record that states no situs keeps the declared one", private, []situsRecord{
			{situs: "KY"}, {asOf: day(5), economic: true}, {asOf: day(9), situs: "LU", economic: true}},
			"-:KY/declared 9:LU/declared"},
		{"a dated situs alone: none before it", private, []situsRecord{{asOf: day(5), situs: "KY"}},
			"-:/unknown 5:KY/declared"},
		{"a day's two records count together", private, []situsRecord{
			{asOf: day(5), economic: true, country: "DE"}, {asOf: day(5), situs: "KY"}}, "-:/unknown 5:KY/declared"},
		{"an economic record that changes nothing starts no reading", situsFacts{isin: "IE"}, []situsRecord{
			{country: "CH", economic: true}, {asOf: day(5), economic: true}}, "-:IE/isin"},
	} {
		if got := text(situsReadings(c.f, c.records)); got != c.want {
			t.Errorf("%s: %s, want %s", c.name, got, c.want)
		}
	}
}

// The situs fixture adds to the exposure fixture a holding for each
// branch of the rule; every name and identifier is invented.
//
//	QADR   a receipt under a US ISIN whose issuer GLEIF ties to a share,
//	       NL00000SHAR7, an equity line of a fund nobody holds
//	QADX   a receipt no share is known for
//	QADF   an ETF whose name says ADR: no receipt
//	QADS   a share under a Finnish ISIN whose name starts ADS-: no
//	       receipt either
//	QXSB   a bond under an XS ISIN with no domicile
//	QEUB   a bond under an EU ISIN: the European Union is no country
//	QCSP   a share known by a CUSIP alone, which the US rule ties to an
//	       ISIN, incorporated in Delaware as EDGAR states it
//	GAPF   a fund under its ticker, domiciled in Ireland by its
//	       attributes
//	QNPF   a fund under its ticker whose latest composition is an SEC
//	       N-PORT filing: a US fund; OLDF has another provider's
//	FUNDX  a fund under its ticker, domiciled in the US by its
//	       attributes, whose lines sit elsewhere
//	cash   kept by a custodian in Switzerland, as is GOLD
//	QPRV   declared Cayman beside its split
//	HOUSE  its one declared country, Switzerland
func seedSitus(t *testing.T, db *sql.DB, ctx context.Context) {
	t.Helper()
	ex := func(q string, args ...any) {
		t.Helper()
		if _, err := db.ExecContext(ctx, q, args...); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	may := exDay(5, 31)
	for _, i := range []struct{ id, class, vehicle, isin, cusip, name, ccy string }{
		{"QADR", "public_equity", "stock", "US00000QADR1", "", "Example Holdings Sponsored ADR", "USD"},
		{"QADX", "public_equity", "stock", "US00000QADX3", "", "Other Example Corp ADR", "USD"},
		{"QADF", "public_equity", "etf", "US00000QADF5", "", "Example Emerging ADR Index ETF", "USD"},
		{"QADS", "public_equity", "stock", "FI00000QADS4", "", "ADS-Example Energy plc", "EUR"},
		{"QXSB", "fixed_income", "bond", "XS00000QXSB5", "", "Example Note 2031", "EUR"},
		{"QEUB", "fixed_income", "bond", "EU00000QEUB2", "", "Example Union Bond 2030", "EUR"},
		{"QCSP", "public_equity", "stock", "", "00000Q105", "Example Cusip Corp", "USD"},
	} {
		ex(`INSERT INTO instruments (silver_source_id, instrument_external_id, asset_class, vehicle, isin, cusip, name, currency,
		    first_seen_at, last_seen_at) VALUES ('test-src', ?, ?, ?, NULLIF(?, ''), NULLIF(?, ''), ?, ?, 0, 0)`,
			i.id, i.class, i.vehicle, i.isin, i.cusip, i.name, i.ccy)
		ex(`INSERT INTO positions (silver_source_id, snapshot_at, account_external_id, position_key, instrument_external_id,
		    asset_class, vehicle, currency, quantity, market_value) VALUES ('test-src', ?, 'A1', ?, ?, ?, ?, ?, 1, 40)`,
			exSnap, i.id, i.id, i.class, i.vehicle, i.ccy)
	}
	ex(`INSERT INTO fund_compositions (silver_source_id, fund_ref, as_of, basis, provider, line_count)
	    VALUES ('test-src', 'symbol:QREF', ?, 'holdings', 'testprov', 1)`, may)
	ex(`INSERT INTO fund_lines (silver_source_id, fund_ref, as_of, basis, line_no, security_key, isin, name, asset_type,
	    weight, currency, country, sector) VALUES ('test-src', 'symbol:QREF', ?, 'holdings', 1, 'isin:NL00000SHAR7',
	    'NL00000SHAR7', 'Example Holdings NV', 'Equity', 100, 'EUR', 'Netherlands', 'Industrials')`, may)
	ex(`INSERT INTO security_facts (silver_source_id, security_key, field, as_of, provider, value)
	    VALUES ('test-src', 'isin:US00000QADR1', 'issuer_isins', ?, 'gleif', 'NL00000SHAR7'),
	           ('test-src', 'cusip:00000Q105', 'incorporation', ?, 'sec-edgar', 'DE')`, may, may)
	ex(`INSERT INTO instruments (silver_source_id, instrument_external_id, asset_class, vehicle, symbol, name, currency,
	    first_seen_at, last_seen_at) VALUES ('test-src', 'QNPF', 'public_equity', 'fund', 'QNPF', 'Example Registered Fund',
	    'USD', 0, 0)`)
	ex(`INSERT INTO positions (silver_source_id, snapshot_at, account_external_id, position_key, instrument_external_id,
	    asset_class, vehicle, currency, quantity, market_value) VALUES ('test-src', ?, 'A1', 'QNPF', 'QNPF', 'public_equity',
	    'fund', 'USD', 1, 40)`, exSnap)
	ex(`INSERT INTO fund_compositions (silver_source_id, fund_ref, as_of, basis, provider, line_count)
	    VALUES ('test-src', 'symbol:QNPF', ?, 'holdings', 'sec-nport', 1)`, may)
	ex(`INSERT INTO fund_lines (silver_source_id, fund_ref, as_of, basis, line_no, security_key, isin, name, asset_type,
	    weight, currency) VALUES ('test-src', 'symbol:QNPF', ?, 'holdings', 1, 'isin:XX0000000001', 'XX0000000001',
	    'Stock A', 'Equity', 100, 'USD')`, may)
	ex(`INSERT INTO fund_attributes (silver_source_id, fund_ref, as_of, domicile)
	    VALUES ('test-src', 'symbol:GAPF', ?, 'IE'), ('test-src', 'symbol:FUNDX', ?, 'US')`, may, may)
	rows := exposureFixtureRows()
	rows.Custody = []canonical.ExposureCustody{{SilverSourceID: "test-src", Custodian: "Example Bank", Country: "CH"}}
	rows.Declarations[1].Situs = "KY"
	stampAndRebuild(t, db, ctx, rows)
}

// stampAndRebuild stamps an exposure block and runs the exposure pass,
// as a load does.
func stampAndRebuild(t *testing.T, db *sql.DB, ctx context.Context, rows canonical.ExposureRows) {
	t.Helper()
	if _, err := SyncExposure(ctx, db, rows); err != nil {
		t.Fatal(err)
	}
	if _, err := RebuildExposure(ctx, db); err != nil {
		t.Fatal(err)
	}
}

// situsAt is the situs and basis of a holding of account A1 at an
// instant, "-" for no situs.
func situsAt(t *testing.T, db *sql.DB, ctx context.Context, at int64, positionKey string) string {
	t.Helper()
	var s string
	if err := db.QueryRowContext(ctx, `SELECT COALESCE(situs, '-') || ' ' || basis FROM exposure_situs_at(?)
	    WHERE silver_source_id = 'test-src' AND account_external_id = 'A1' AND position_key = ?`, at, positionKey).
		Scan(&s); err != nil {
		t.Fatalf("situs of %s: %v", positionKey, err)
	}
	return s
}

// Every branch of the rule, on the invented holdings.
func TestExposureSitusOfEachKindOfHolding(t *testing.T) {
	t.Parallel()
	db, ctx := openExposureFixture(t)
	seedSitus(t, db, ctx)
	for key, want := range map[string]string{
		"QPRV":     "KY declared",
		"QADR":     "NL underlying",
		"QADX":     "- unknown",
		"QADF":     "US isin",
		"QADS":     "FI isin",
		"QEUB":     "- unknown",
		"NOCF":     "IE isin",
		"STKD":     "DE isin",
		"QXSB":     "- unknown",
		"BOND9":    "DE domicile",
		"QCSP":     "US domicile",
		"GAPF":     "IE domicile",
		"QNPF":     "US domicile",
		"OLDF":     "- unknown",
		"STKA":     "- unknown",
		"cash:USD": "CH custody",
		"GOLD":     "CH custody",
		"HOUSE":    "CH country",
		"LOAN":     "- unknown",
		"PE1":      "- unknown",
		"COIN1":    "- unknown",
	} {
		if got := situsAt(t, db, ctx, exAsOf, key); got != want {
			t.Errorf("%s: %s, want %s", key, got, want)
		}
	}
	// No fund is looked through: every leaf of FUNDX sits in the US, where
	// its lines' securities sit elsewhere, and every holding's leaves
	// carry one situs.
	if got := leafText(t, db, ctx, "string_agg(DISTINCT situs || ' ' || situs_basis, ',')",
		`top_key = 'symbol:FUNDX' AND depth > 0`); got != "US domicile" {
		t.Errorf("FUNDX's lines sit in %q, want the fund's US", got)
	}
	var mixed int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM (SELECT src, acct, position_key FROM exposure_leaves(?, 'USD')
	    GROUP BY ALL HAVING COUNT(DISTINCT COALESCE(situs, '-') || situs_basis) > 1)`, exAsOf).Scan(&mixed); err != nil ||
		mixed != 0 {
		t.Errorf("%d holdings split over two situs (%v)", mixed, err)
	}
	// A receipt's sector comes from its share as before.
	if got := leafText(t, db, ctx, "sector", `iid = 'QADR'`); got != "industrials" {
		t.Errorf("QADR's sector = %q, want its share's", got)
	}
}

// The view sums to holdings global, one row per situs with its region,
// the share held in funds and the bases it rests on; funds and coverage
// carry the situs too.
func TestExposureSitusViews(t *testing.T) {
	t.Parallel()
	db, ctx := openExposureFixture(t)
	seedSitus(t, db, ctx)
	g, err := GlobalAsOf(ctx, db, exAsOf, "USD")
	if err != nil {
		t.Fatal(err)
	}
	rows, err := ExposureSitus(ctx, db, exAsOf, "USD")
	if err != nil {
		t.Fatal(err)
	}
	total := 0.0
	by := map[string]ExposureSitusRow{}
	for _, r := range rows {
		total += parse(t, r.Value)
		by[r.Situs] = r
	}
	if want := parse(t, g.TotalValueOutCcy); math.Abs(total-want) > 0.01 {
		t.Errorf("situs sums to %.4f, holdings global is %.4f", total, want)
	}
	// Ireland: NOCF 60 by its ISIN and GAPF 100 by its domicile, both
	// funds.
	ie := by["IE"]
	if math.Abs(parse(t, ie.Value)-160) > 0.005 || ie.Region == nil || *ie.Region != "europe" ||
		ie.FundsPct == nil || math.Abs(*ie.FundsPct-100) > 1e-9 || ie.Quality != "isin=38%,domicile=63%" {
		t.Errorf("IE = %v %v funds %v %q", parse(t, ie.Value), ie.Region, ie.FundsPct, ie.Quality)
	}
	// Switzerland: the house by its declared country, the cash and the
	// gold by their custodian; no fund.
	ch := by["CH"]
	want := leafValue(t, db, ctx, `position_key IN ('HOUSE', 'cash:USD', 'cash:EUR', 'GOLD')`)
	if math.Abs(parse(t, ch.Value)-want) > 0.005 || ch.FundsPct == nil || *ch.FundsPct != 0 ||
		qualityTags(ch.Quality) != "custody,country" {
		t.Errorf("CH = %v, want %v; funds %v %q", parse(t, ch.Value), want, ch.FundsPct, ch.Quality)
	}
	if u := by["unknown"]; u.Region != nil || u.Quality != "unknown=100%" {
		t.Errorf("unknown = %v %q", u.Region, u.Quality)
	}

	funds, err := ExposureFunds(ctx, db, exAsOf, "USD")
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range funds {
		got := strOrDash(f.Situs) + " " + strOrDash(f.SitusBasis)
		want := map[string]string{"symbol:GAPF": "IE domicile", "holding:test-src:QPRV": "KY declared",
			"symbol:FOF": "- unknown", "(other holdings)": "- -"}[f.Fund]
		if want != "" && got != want {
			t.Errorf("funds %s: situs %s, want %s", f.Fund, got, want)
		}
	}

	cov, err := ExposureCoverage(ctx, db, exAsOf, "USD")
	if err != nil {
		t.Fatal(err)
	}
	bases, listed := 0.0, map[string]ExposureCoverageRow{}
	var order []string
	for _, r := range cov {
		if r.Section != "situs" {
			continue
		}
		order = append(order, r.Reason)
		if r.SilverSourceID == nil && r.Reason != "(no value)" {
			bases += parse(t, r.Value)
		} else {
			listed[r.Reason] = r
		}
	}
	if math.Abs(bases-total) > 0.01 {
		t.Errorf("situs bases sum to %.4f, want %.4f", bases, total)
	}
	if len(order) < 2 || order[0] != "declared" {
		t.Errorf("situs section = %v, want the bases first, in the rule's order", order)
	}
	// Each holding of unknown situs, by what a record names; the
	// funds by their own name, not a line's.
	if r, ok := listed["symbol:FOF"]; !ok || r.Detail == nil || *r.Detail != "Fund of Funds" || r.Items != 1 ||
		r.Instrument == nil || *r.Instrument != "FOF" {
		t.Errorf("FOF situs line = %+v", r)
	}
	if _, ok := listed["isin:US00000QADX3"]; !ok {
		t.Errorf("the receipt without its share is not listed: %v", order)
	}
	if _, ok := listed["isin:NL00000SHAR7"]; ok {
		t.Errorf("a fund's line is listed as a holding")
	}
}

// A declared situs holds from its record's day until a later record
// states another; a later record that states none leaves it standing.
// A situs-only record is no record for the economic dimensions: the
// country and currency an earlier record states stay.
func TestExposureSitusOverTime(t *testing.T) {
	t.Parallel()
	db, ctx := openExposureFixture(t)
	seedSitus(t, db, ctx)
	rows := exposureFixtureRows()
	rows.Declarations[1].Situs = "KY"
	apr, may := exDay(4, 30), exDay(5, 31)
	lu := declaration(3, "QPRV", &apr, split{"region", []string{"europe"}, []float64{100}})
	lu.Situs = "LU"
	rows.Declarations = append(rows.Declarations, lu, declaration(4, "QPRV", &may,
		split{"region", []string{"europe"}, []float64{100}}),
		canonical.ExposureDeclaration{Line: 5, SilverSourceID: "test-src", InstrumentExternalID: "HOUSE", AsOf: &may,
			Situs: "AT"})
	stampAndRebuild(t, db, ctx, rows)
	for at, want := range map[int64]string{
		exDay(3, 31): "KY declared", exDay(5, 15): "LU declared", exAsOf: "LU declared",
	} {
		if got := situsAt(t, db, ctx, at, "QPRV"); got != want {
			t.Errorf("QPRV at %s: %s, want %s", time.Unix(at, 0).UTC().Format(time.DateOnly), got, want)
		}
	}
	if got := situsAt(t, db, ctx, exDay(5, 15), "HOUSE"); got != "CH country" {
		t.Errorf("HOUSE before its situs: %s", got)
	}
	if got := situsAt(t, db, ctx, exAsOf, "HOUSE"); got != "AT declared" {
		t.Errorf("HOUSE from its situs: %s", got)
	}
	// The house keeps its declared country and currency.
	if got := leafText(t, db, ctx, "how || '/' || country || '/' || currency", `iid = 'HOUSE'`); got != "declared/CH/CHF" {
		t.Errorf("HOUSE = %q", got)
	}
	// A record that states no situs and no new split adds no reading.
	var n int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM exposure_situs WHERE position_key = 'QPRV'`).Scan(&n); err != nil ||
		n != 2 {
		t.Errorf("QPRV has %d readings (%v), want 2", n, err)
	}
}

// A record that states only a situs declares nothing else: the holding
// reads as undeclared in every other view, and a coin keeps its class
// rules.
func TestExposureSitusOnlyRecord(t *testing.T) {
	t.Parallel()
	db, ctx := openExposureFixture(t)
	rows := exposureFixtureRows()
	rows.Declarations = append(rows.Declarations,
		canonical.ExposureDeclaration{Line: 3, SilverSourceID: "test-src", InstrumentExternalID: "PE1", Situs: "DE"},
		canonical.ExposureDeclaration{Line: 4, SilverSourceID: "test-src", InstrumentExternalID: "COIN1", Situs: "SG"},
		canonical.ExposureDeclaration{Line: 5, SilverSourceID: "test-src", InstrumentExternalID: "QPRV", Situs: "KY"})
	stampAndRebuild(t, db, ctx, rows)
	for key, want := range map[string]string{"PE1": "DE declared", "COIN1": "SG declared", "QPRV": "KY declared"} {
		if got := situsAt(t, db, ctx, exAsOf, key); got != want {
			t.Errorf("%s: %s, want %s", key, got, want)
		}
	}
	if f := leafFlags(t, db, ctx, `iid = 'PE1'`); f != "opaque,undeclared" {
		t.Errorf("PE1 flags = %q, want it undeclared still", f)
	}
	if got := leafText(t, db, ctx, "how || '/' || currency", `iid = 'COIN1'`); got != "asset_class/BTC" {
		t.Errorf("COIN1 = %q", got)
	}
	// QPRV's undated split stands beside its undated situs.
	if v := leafValue(t, db, ctx, `iid = 'QPRV' AND region = 'other'`); math.Abs(v-30) > 1e-6 {
		t.Errorf("QPRV's rest of the world = %v, want its split", v)
	}
	funds, err := ExposureFunds(ctx, db, exAsOf, "USD")
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range funds {
		switch f.Fund {
		case "holding:test-src:PE1":
			if f.Via != nil || f.Quality != "no_composition" || strOrDash(f.Situs) != "DE" {
				t.Errorf("PE1 in funds: via %v, quality %q, situs %s", f.Via, f.Quality, strOrDash(f.Situs))
			}
		case "holding:test-src:QPRV":
			if strOrDash(f.Via) != "declared" || strOrDash(f.DeclaredDimensions) != "region,currency,asset_class" {
				t.Errorf("QPRV in funds: via %s, declared %s", strOrDash(f.Via), strOrDash(f.DeclaredDimensions))
			}
		}
	}
}

// The situs reads a holding's key as the look-through does: a holding
// alias moves it to the security it names.
func TestExposureSitusFollowsAHoldingAlias(t *testing.T) {
	t.Parallel()
	db, ctx := openExposureFixture(t)
	if got := situsAt(t, db, ctx, exAsOf, "UNK"); got != "- unknown" {
		t.Fatalf("UNK before the alias: %s", got)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO holding_aliases (silver_source_id, account_external_id, from_key, to_key)
	    VALUES ('test-src', 'A1', 'UNK', 'STKD')`); err != nil {
		t.Fatal(err)
	}
	if _, err := RebuildExposure(ctx, db); err != nil {
		t.Fatal(err)
	}
	if got := situsAt(t, db, ctx, exAsOf, "UNK"); got != "DE isin" {
		t.Errorf("UNK aliased to STKD: %s", got)
	}
	if got := leafText(t, db, ctx, "top_key || ' ' || situs", `position_key = 'UNK'`); got != "isin:DE0000000007 DE" {
		t.Errorf("UNK's leaf = %q", got)
	}
}

func TestMigration0129DDLIsRerunnable(t *testing.T) {
	t.Parallel()
	db, ctx := openExposureFixture(t)
	seedSitus(t, db, ctx)
	rerunMigrationDDL(t, db, ctx, "0129_exposure_situs.sql")
	if _, err := RebuildExposure(ctx, db); err != nil {
		t.Fatal(err)
	}
	if got := situsAt(t, db, ctx, exAsOf, "QPRV"); got != "KY declared" {
		t.Errorf("after replay: QPRV %s", got)
	}
	if rows, err := ExposureSitus(ctx, db, exAsOf, "USD"); err != nil || len(rows) == 0 {
		t.Errorf("situs after replay: %d rows, %v", len(rows), err)
	}
}

// qualityTags is a quality cell's tags without their shares.
func qualityTags(q string) string {
	return regexp.MustCompile(`=[<0-9]+%`).ReplaceAllString(q, "")
}

func strOrDash(s *string) string {
	if s == nil {
		return "-"
	}
	return *s
}
