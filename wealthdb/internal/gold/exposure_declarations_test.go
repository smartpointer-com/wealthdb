package gold

import (
	"context"
	"database/sql"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
)

// A split declaration expands into the cross product of its dimensions.
// Every marginal shows the declared split exactly, a part's class is the
// one it states, and the parts carry no country where a region is
// declared.
func TestExposureDeclaredSplit(t *testing.T) {
	t.Parallel()
	db, ctx := openExposureFixture(t)
	var parts int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM exposure_leaves(?, 'USD') WHERE iid = 'QPRV'`, exAsOf).
		Scan(&parts); err != nil || parts != 12 {
		t.Errorf("QPRV has %d parts (%v), want 3 regions x 2 currencies x 2 classes", parts, err)
	}
	closeTo(t, "QPRV", leafValue(t, db, ctx, `iid = 'QPRV'`), 200)
	for region, want := range map[string]float64{"north_america": 110, "europe": 60, "other": 30} {
		closeTo(t, "QPRV in "+region, leafValue(t, db, ctx, `iid = 'QPRV' AND region = ?`, region), want)
	}
	for ccy, want := range map[string]float64{"USD": 120, "EUR": 80} {
		closeTo(t, "QPRV in "+ccy, leafValue(t, db, ctx, `iid = 'QPRV' AND currency = ?`, ccy), want)
	}
	closeTo(t, "QPRV's private equity", leafValue(t, db, ctx, `iid = 'QPRV' AND asset_class = 'private_equity'`), 144)
	closeTo(t, "QPRV's infrastructure", leafValue(t, db, ctx, `iid = 'QPRV' AND asset_class = 'infrastructure'`), 56)
	closeTo(t, "one part", leafValue(t, db, ctx, `iid = 'QPRV' AND region = 'europe' AND currency = 'EUR'
	    AND asset_class = 'infrastructure'`), 200*0.30*0.4*0.28)
	if got := leafText(t, db, ctx, "string_agg(DISTINCT how || '/' || COALESCE(country, '-'), ',')", `iid = 'QPRV'`); got != "declared/-" {
		t.Errorf("QPRV parts read %q, want declared with no country", got)
	}
	// Private equity nothing looks through stays opaque and diversified;
	// the infrastructure part is neither.
	if f := leafFlags(t, db, ctx, `iid = 'QPRV' AND asset_class = 'private_equity'`); f != "declared,opaque" {
		t.Errorf("private equity part flags = %q", f)
	}
	if f := leafFlags(t, db, ctx, `iid = 'QPRV' AND asset_class = 'infrastructure'`); f != "declared" {
		t.Errorf("infrastructure part flags = %q", f)
	}
	closeTo(t, "diversified", leafValue(t, db, ctx, `iid = 'QPRV' AND sector = 'diversified'`), 144)

	// The classes view shows the fund in both classes.
	classes, err := ExposureClasses(ctx, db, exAsOf, "USD")
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range classes {
		if r.AssetClass == "infrastructure" && (math.Abs(parse(t, r.Value)-56) > 0.005 || r.Quality != "declared=100%") {
			t.Errorf("infrastructure = %v %q", parse(t, r.Value), r.Quality)
		}
	}
	// countries: a declared region is a row of its own under unknown.
	countries, err := ExposureCountries(ctx, db, exAsOf, "USD", "risk")
	if err != nil {
		t.Fatal(err)
	}
	byRegion := map[string]float64{}
	for _, r := range countries {
		if r.Country == "unknown" && r.Region != nil {
			byRegion[*r.Region] = parse(t, r.Value)
		}
	}
	if math.Abs(byRegion["north_america"]-110)+math.Abs(byRegion["europe"]-60)+math.Abs(byRegion["other"]-30) > 0.015 {
		t.Errorf("unknown countries by region = %v", byRegion)
	}
	// securities: the parts are one row, the holding's.
	secs, err := ExposureSecurities(ctx, db, exAsOf, "USD", "risk")
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, r := range secs {
		if strings.Contains(r.Security, "QPRV") {
			n++
			if r.Security != "holding:test-src:QPRV" || parse(t, r.Value) != 200 {
				t.Errorf("QPRV security row = %s %v", r.Security, parse(t, r.Value))
			}
		}
	}
	if n != 1 {
		t.Errorf("QPRV has %d security rows, want 1", n)
	}
}

// Precedence: a fund's own composition, then a declaration, then a proxy.
// A declaration never stands in for a fund a composition of its own
// describes, even where an always-proxy replaces that composition.
func TestExposureDeclarationPrecedence(t *testing.T) {
	t.Parallel()
	db, ctx := openExposureFixture(t)
	rows := exposureFixtureRows()
	country := func(line int, iid, code string) canonical.ExposureDeclaration {
		return declaration(line, iid, nil, split{"country", []string{code}, []float64{100}})
	}
	rows.Declarations = append(rows.Declarations, country(3, "FUNDX", "AT"), country(4, "LEV3", "LU"),
		country(5, "NOCF", "IE"), country(6, "OLDF", "AT"))
	rows.Proxies = append(rows.Proxies, canonical.ExposureProxy{FundRef: "symbol:OLDF", ProxyRef: "symbol:IDX1",
		Always: true, Leverage: 1})
	if _, err := SyncExposure(ctx, db, rows); err != nil {
		t.Fatal(err)
	}
	// FUNDX has its own composition: looked through, not declared.
	if v := leafValue(t, db, ctx, `top_key = 'symbol:FUNDX' AND depth > 0`); math.Abs(v-1000) > 1e-6 {
		t.Errorf("FUNDX through its lines = %v, want 1000", v)
	}
	if f := leafFlags(t, db, ctx, `top_key = 'symbol:FUNDX'`); strings.Contains(f, "declared") {
		t.Errorf("FUNDX flags %q: a declaration stood in for its composition", f)
	}
	// OLDF has its own composition too, which its always-proxy replaces.
	if f := leafFlags(t, db, ctx, `top_key = 'symbol:OLDF'`); f != "proxy" {
		t.Errorf("OLDF flags %q, want proxy", f)
	}
	// LEV3 has only a proxy: the declaration wins.
	if got := leafText(t, db, ctx, "depth || '|' || how || '|' || country || '|' || value", `iid = 'LEV3'`); got != "0|declared|LU|200.0" {
		t.Errorf("LEV3 leaf = %q, want its declaration", got)
	}
	if v := leafValue(t, db, ctx, `top_key = 'isin:XX00000000L3' AND depth > 0`); v != 0 {
		t.Errorf("LEV3 is still looked through its proxy: %v", v)
	}
	// NOCF has nothing else.
	if got := leafText(t, db, ctx, "how || '|' || country", `iid = 'NOCF'`); got != "declared|IE" {
		t.Errorf("NOCF leaf = %q", got)
	}
	// The funds view says what describes each.
	funds, err := ExposureFunds(ctx, db, exAsOf, "USD")
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, f := range funds {
		got[f.Fund] = fmtStrPtr(f.Via) + "|" + fmtStrPtr(f.Proxy) + "|" + fmtStrPtr(f.DeclaredDimensions) + "|" +
			strings.Join(keepFlags(strings.Split(f.Quality, ","), "declared", "no_composition", "proxy"), ",")
	}
	for fund, want := range map[string]string{
		"symbol:FUNDX":         "direct|-|-|",
		"symbol:OLDF":          "proxy|symbol:IDX1|-|proxy",
		"isin:XX00000000L3":    "declared|-|country|declared",
		"isin:IE0000000005":    "declared|-|country|declared",
		"holding:test-src:PE1": "-|-|-|no_composition",
		// The declared house is no fund.
		"(other holdings)": "-|-|-|",
	} {
		if got[fund] != want {
			t.Errorf("funds %s = %q, want %q", fund, got[fund], want)
		}
	}
}

// The record that describes a holding is the latest dated on or before
// the day, else one without a date; a day before every record leaves the
// holding undeclared. No record is too old.
func TestExposureDeclarationAsOf(t *testing.T) {
	t.Parallel()
	db, ctx := openExposureFixture(t)
	old, jul, sep := exDay(1, 1, 2020), exDay(7, 1), exDay(9, 1)
	country := func(line int, asOf *int64, code string) canonical.ExposureDeclaration {
		return declaration(line, "PE1", asOf, split{"country", []string{code}, []float64{100}})
	}
	// The fixture holds its positions from June 30 on.
	read := func(at int64) string {
		t.Helper()
		var s sql.NullString
		if err := db.QueryRowContext(ctx, `SELECT how || '|' || COALESCE(country, '-') || '|' ||
		    array_to_string(flags, ',') FROM exposure_leaves(?, 'USD') WHERE iid = 'PE1'`, at).Scan(&s); err != nil {
			t.Fatal(err)
		}
		return s.String
	}
	stampDeclarations(t, db, ctx, country(3, nil, "CH"), country(4, &old, "DE"), country(5, &jul, "AT"), country(6, &sep, "FR"))
	for at, want := range map[int64]string{
		exAsOf:            "declared|DE|declared,opaque", // the 2020 record, however old
		exDay(8, 15):      "declared|AT|declared,opaque",
		exDay(12, 31) - 1: "declared|FR|declared,opaque",
	} {
		if got := read(at); got != want {
			t.Errorf("at %d: %q, want %q", at, got, want)
		}
	}
	stampDeclarations(t, db, ctx, country(3, nil, "CH"), country(5, &jul, "AT"))
	if got := read(exAsOf); got != "declared|CH|declared,opaque" {
		t.Errorf("before the dated record: %q, want the record without a date", got)
	}
	stampDeclarations(t, db, ctx, country(5, &jul, "AT"))
	if got := read(exAsOf); got != "unknown|-|opaque,undeclared" {
		t.Errorf("before the only record: %q, want undeclared", got)
	}
}

// A record that declares a currency and a sector leaves the rest as the
// holding reads it: a listed stock keeps its security's country, a
// private holding its position's currency.
func TestExposureDeclarationLeavesTheRest(t *testing.T) {
	t.Parallel()
	db, ctx := openExposureFixture(t)
	stka := `iid = 'STKA' AND depth = 0`
	before := leafText(t, db, ctx, "sector || '|' || country || '|' || currency", stka)
	stampDeclarations(t, db, ctx,
		declaration(3, "STKA", nil, split{"sector", []string{"energy"}, []float64{100}}),
		declaration(4, "PE1", nil, split{"country", []string{"US", "unknown"}, []float64{64, 36}}))
	_, rest, _ := strings.Cut(before, "|")
	if got := leafText(t, db, ctx, "sector || '|' || country || '|' || currency", stka); got != "energy|"+rest {
		t.Errorf("STKA = %q, was %q: want the declared sector and the rest as before", got, before)
	}
	if got := leafText(t, db, ctx, "string_agg(COALESCE(country, '-') || ':' || round(value, 6) || ':' || currency, ',' "+
		"ORDER BY value)", `iid = 'PE1'`); got != "-:28.8:USD,US:51.2:USD" {
		t.Errorf("PE1 = %q, want 64%% US, the rest unknown, both in its own currency", got)
	}
}

func TestMigration0128DDLIsRerunnable(t *testing.T) {
	t.Parallel()
	db, ctx := openExposureFixture(t)
	rerunMigrationDDL(t, db, ctx, "0128_exposure_declarations.sql")
	if _, err := SyncExposure(ctx, db, exposureFixtureRows()); err != nil {
		t.Fatal(err)
	}
	if v := leafValue(t, db, ctx, `iid = 'QPRV' AND region = 'other'`); math.Abs(v-30) > 1e-6 {
		t.Errorf("after replay: QPRV's rest of the world = %v", v)
	}
	if f := leafFlags(t, db, ctx, `iid = 'PE1'`); f != "opaque,undeclared" {
		t.Errorf("after replay: PE1 flags = %q", f)
	}
}

// keepFlags keeps the flags of s named in keep, with or without a share.
func keepFlags(s []string, keep ...string) []string {
	var out []string
	for _, e := range s {
		for _, k := range keep {
			if e == k || strings.HasPrefix(e, k+"=") {
				out = append(out, e)
			}
		}
	}
	return out
}

// A declaration yields to a fund's own composition within max_gap_days,
// a forward one too; beyond the bound, the declaration describes it.
func TestExposureDeclarationAndTheGapBound(t *testing.T) {
	t.Parallel()
	db, ctx := openExposureFixture(t)
	rows := exposureFixtureRows()
	rows.Declarations = append(rows.Declarations,
		declaration(3, "GAPF", nil, split{"country", []string{"FR"}, []float64{100}}))
	if _, err := SyncExposure(ctx, db, rows); err != nil {
		t.Fatal(err)
	}
	// GAPF's later composition lies 92 days after the day: within 400.
	if f := leafFlags(t, db, ctx, `top_key = 'symbol:GAPF'`); f != "forward" {
		t.Errorf("GAPF flags = %q, want its own forward composition", f)
	}
	rows.Settings.MaxGapDays = 30
	if _, err := SyncExposure(ctx, db, rows); err != nil {
		t.Fatal(err)
	}
	if got := leafText(t, db, ctx, "depth || '|' || how || '|' || country", `iid = 'GAPF'`); got != "0|declared|FR" {
		t.Errorf("GAPF beyond the bound = %q, want its declaration", got)
	}
}

// A declared fund that another fund also holds: the holding reads its
// declaration, the line its proxy.
func TestExposureDeclaredFundReachedByALine(t *testing.T) {
	t.Parallel()
	db, ctx := openExposureFixture(t)
	for _, q := range []string{
		`INSERT INTO instruments (silver_source_id, instrument_external_id, asset_class, vehicle, symbol, name, currency,
		 first_seen_at, last_seen_at) VALUES ('test-src', 'QFOL', 'public_equity', 'etf', 'QFOL', 'Fund of LEV3', 'USD', 0, 0)`,
		`INSERT INTO positions (silver_source_id, snapshot_at, account_external_id, position_key, instrument_external_id,
		 asset_class, vehicle, currency, quantity, market_value) VALUES ('test-src', ?, 'A1', 'QFOL', 'QFOL',
		 'public_equity', 'etf', 'USD', 1, 50)`,
		`INSERT INTO fund_compositions (silver_source_id, fund_ref, as_of, basis, provider, line_count, currency)
		 VALUES ('test-src', 'symbol:QFOL', ?, 'holdings', 'testprov', 1, 'USD')`,
		`INSERT INTO fund_lines (silver_source_id, fund_ref, as_of, basis, line_no, security_key, isin, name, asset_type,
		 weight, currency) VALUES ('test-src', 'symbol:QFOL', ?, 'holdings', 1, 'isin:XX00000000L3', 'XX00000000L3',
		 'Leveraged Three', 'Fund', 100, 'USD')`,
	} {
		args := []any{}
		if strings.Contains(q, "?") {
			args = append(args, exSnap)
		}
		if _, err := db.ExecContext(ctx, q, args...); err != nil {
			t.Fatal(err)
		}
	}
	stampDeclarations(t, db, ctx,
		declaration(3, "LEV3", nil, split{"country", []string{"LU"}, []float64{100}}))
	if _, err := RebuildExposure(ctx, db); err != nil {
		t.Fatal(err)
	}
	if got := leafText(t, db, ctx, "how || '|' || country || '|' || value", `iid = 'LEV3'`); got != "declared|LU|200.0" {
		t.Errorf("LEV3 held directly = %q, want its declaration", got)
	}
	// Through QFOL, 50 of LEV3 at leverage 3 of IDX1: 150 of Stock A,
	// minus 100 of cash.
	if v := leafValue(t, db, ctx, `top_key = 'symbol:QFOL' AND key = 'isin:XX0000000001'`); math.Abs(v-150) > 1e-6 {
		t.Errorf("QFOL's Stock A = %v, want 150 through the proxy", v)
	}
	if v := leafValue(t, db, ctx, `top_key = 'symbol:QFOL'`); math.Abs(v-50) > 1e-6 {
		t.Errorf("QFOL = %v, want 50", v)
	}
	if f := leafFlags(t, db, ctx, `top_key = 'symbol:QFOL'`); strings.Contains(f, "declared") {
		t.Errorf("QFOL's line read a declaration: %q", f)
	}
}

// A part of class cash or crypto follows its class: cash reads the
// sector cash, a coin its fund's name does not state reads the declared
// currency. The parts stay one securities row, the holding's, apart from
// the cash balances.
func TestExposureDeclaredCashAndCryptoParts(t *testing.T) {
	t.Parallel()
	db, ctx := openExposureFixture(t)
	stampDeclarations(t, db, ctx, declaration(3, "PE1", nil,
		split{"currency", []string{"BTC", "unknown"}, []float64{45, 55}},
		split{"asset_class", []string{"crypto", "cash"}, []float64{40, 60}}))
	got := leafText(t, db, ctx, "string_agg(asset_class || '/' || COALESCE(sector, '-') || '/' || COALESCE(currency, '-') "+
		"|| '/' || round(value, 6), ' ' ORDER BY asset_class, currency NULLS LAST)", `iid = 'PE1'`)
	if want := "cash/cash/BTC/21.6 cash/cash/-/26.4 crypto/crypto/BTC/14.4 crypto/crypto/-/17.6"; got != want {
		t.Errorf("PE1 parts = %q, want %q", got, want)
	}
	secs, err := ExposureSecurities(ctx, db, exAsOf, "USD", "risk")
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range secs {
		if r.Security == "holding:test-src:PE1" && math.Abs(parse(t, r.Value)-80) > 1e-6 {
			t.Errorf("PE1's row = %v, want all 80", parse(t, r.Value))
		}
		if r.Security == "" || r.Security == "cash:" {
			t.Errorf("a part with no currency made the row %q", r.Security)
		}
	}
}

// What a dimension's shares take beyond 100 is a negative unknown
// part, and the holding still sums to its value.
func TestExposureDeclaredNegativePart(t *testing.T) {
	t.Parallel()
	db, ctx := openExposureFixture(t)
	stampDeclarations(t, db, ctx, declaration(3, "PE1", nil,
		split{"country", []string{"US", "unknown"}, []float64{100.3, -0.3}}))
	if v := leafValue(t, db, ctx, `iid = 'PE1'`); math.Abs(v-80) > 1e-9 {
		t.Errorf("PE1 = %v, want 80", v)
	}
	if v := leafValue(t, db, ctx, `iid = 'PE1' AND country IS NULL`); math.Abs(v+0.24) > 1e-9 {
		t.Errorf("PE1's unknown part = %v, want -0.24", v)
	}
}

// The funds view gives a declared fund the record's date, and its own
// attributes, as it gives them under a composition.
func TestExposureFundsShowADeclaration(t *testing.T) {
	t.Parallel()
	db, ctx := openExposureFixture(t)
	if _, err := db.ExecContext(ctx, `INSERT INTO fund_attributes (silver_source_id, fund_ref, as_of, domicile)
	    VALUES ('test-src', 'isin:XX00000000L3', ?, 'IE')`, exDay(5, 31)); err != nil {
		t.Fatal(err)
	}
	may := exDay(5, 1)
	stampDeclarations(t, db, ctx, declaration(3, "LEV3", &may,
		split{"region", []string{"europe"}, []float64{100}}, split{"sector", []string{"technology"}, []float64{100}}))
	funds, err := ExposureFunds(ctx, db, exAsOf, "USD")
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range funds {
		if f.Fund != "isin:XX00000000L3" {
			continue
		}
		if f.DeclarationAsOf == nil || *f.DeclarationAsOf != may || f.DeclaredDimensions == nil ||
			*f.DeclaredDimensions != "region,sector" || f.Domicile == nil || *f.Domicile != "IE" || f.Provider != nil {
			t.Errorf("LEV3 funds row = %+v", f)
		}
		return
	}
	t.Error("no funds row for LEV3")
}

// A ledger record whose holding no source states is listed by its line.
// A parsed record of such a holding is no ledger line, so it is not.
func TestDeclarationsNamingNoHolding(t *testing.T) {
	t.Parallel()
	db, ctx := openExposureFixture(t)
	if _, err := db.ExecContext(ctx, `INSERT INTO exposure_reports (silver_source_id, instrument_external_id, as_of,
	    dimensions) VALUES ('test-src', 'NOSUCH9', ?, '{"country":{"FR":100}}')`, exDay(6, 30)); err != nil {
		t.Fatal(err)
	}
	stampDeclarations(t, db, ctx,
		declaration(7, "HOUS3", nil, split{"country", []string{"CH"}, []float64{100}}))
	if lines, err := DeclarationsNamingNoHolding(ctx, db); err != nil || len(lines) != 1 || lines[0] != 7 {
		t.Errorf("lines = %v, %v; want [7]", lines, err)
	}
}

// parseReport stands in for a source's parsed split of PE1: a row of
// exposure_reports, which the next SyncExposure merges as a parsed
// record.
func parseReport(t *testing.T, db *sql.DB, ctx context.Context, asOf int64, dims string) {
	t.Helper()
	if _, err := db.ExecContext(ctx, `INSERT INTO exposure_reports (silver_source_id, instrument_external_id, as_of,
	    dimensions, note) VALUES ('test-src', 'PE1', ?, ?, 'Example Feeder Fund II')`, asOf, dims); err != nil {
		t.Fatal(err)
	}
}

// A parsed record joins the hand ledger's. On a day the latest dated
// record on or before it wins; on an equal date the ledger's does, so a
// parsed record never outranks a ledger record of the same or a later
// date. An undated ledger record still serves the days before the first
// dated one.
func TestExposureParsedDeclarationsMerge(t *testing.T) {
	t.Parallel()
	db, ctx := openExposureFixture(t)
	jun, jul, aug, sep := exDay(6, 30), exDay(7, 1), exDay(8, 1), exDay(9, 1)
	country := func(line int, asOf *int64, code string) canonical.ExposureDeclaration {
		return declaration(line, "PE1", asOf, split{"country", []string{code}, []float64{100}})
	}
	at := func(day int64) string {
		t.Helper()
		var s sql.NullString
		if err := db.QueryRowContext(ctx, `SELECT string_agg(DISTINCT COALESCE(country, '-'), ',')
		    FROM exposure_leaves(?, 'USD') WHERE iid = 'PE1'`, day+86399).Scan(&s); err != nil {
			t.Fatal(err)
		}
		return s.String
	}
	parseReport(t, db, ctx, jul, `{"country":{"FR":100}}`)
	parseReport(t, db, ctx, sep, `{"country":{"IT":100}}`)
	stampDeclarations(t, db, ctx, country(3, nil, "CH"), country(4, &jul, "AT"), country(5, &aug, "DE"))
	for day, want := range map[int64]string{
		jun:           "CH", // the undated ledger record, before every dated one
		jul:           "AT", // the ledger wins the equal date
		aug:           "DE", // a ledger record later than the parsed one
		sep:           "IT", // a parsed record newer than every ledger record
		exDay(12, 30): "IT",
	} {
		if got := at(day); got != want {
			t.Errorf("on %s: %q, want %q", time.Unix(day, 0).UTC().Format(time.DateOnly), got, want)
		}
	}
	var origins string
	if err := db.QueryRowContext(ctx, `SELECT string_agg(origin || ':' || line_no, ',' ORDER BY origin, line_no)
	    FROM exposure_declarations WHERE instrument_external_id = 'PE1'`).Scan(&origins); err != nil {
		t.Fatal(err)
	}
	if origins != "ledger:3,ledger:4,ledger:5,parsed:-2,parsed:-1" {
		t.Errorf("origins = %q", origins)
	}
	// A parsed record is no ledger line, so it is never one that names no
	// holding.
	if lines, err := DeclarationsNamingNoHolding(ctx, db); err != nil || len(lines) != 0 {
		t.Errorf("DeclarationsNamingNoHolding = %v (%v), want none", lines, err)
	}
}

// A parsed split crosses its dimensions into parts as a ledger record's
// do, each dimension scaled to sum to one.
func TestExposureParsedDeclarationParts(t *testing.T) {
	t.Parallel()
	db, ctx := openExposureFixture(t)
	parseReport(t, db, ctx, exDay(6, 30),
		`{"region":{"europe":30,"north_america":70},"sector":{"technology":50.0001,"industrials":49.9999}}`)
	stampDeclarations(t, db, ctx)
	var parts int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM exposure_leaves(?, 'USD') WHERE iid = 'PE1'`, exAsOf).
		Scan(&parts); err != nil || parts != 4 {
		t.Errorf("PE1 has %d parts (%v), want 2 regions x 2 sectors", parts, err)
	}
	total := leafValue(t, db, ctx, `iid = 'PE1'`)
	closeTo(t, "PE1 in europe", leafValue(t, db, ctx, `iid = 'PE1' AND region = 'europe'`), 0.3*total)
	closeTo(t, "PE1 in technology", leafValue(t, db, ctx, `iid = 'PE1' AND sector = 'technology'`), 0.500001*total)
	if got := leafText(t, db, ctx, "string_agg(DISTINCT how, ',')", `iid = 'PE1'`); got != "declared" {
		t.Errorf("PE1 parts read %q, want declared", got)
	}
}

func TestMigration0164DDLIsRerunnable(t *testing.T) {
	t.Parallel()
	db, ctx := openExposureFixture(t)
	parseReport(t, db, ctx, exDay(7, 1), `{"country":{"FR":100}}`)
	stampDeclarations(t, db, ctx, declaration(3, "PE1", nil, split{"country", []string{"CH"}, []float64{100}}))
	rerunMigrationDDL(t, db, ctx, "0164_parsed_declarations.sql")
	var n int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM exposure_reports`).Scan(&n); err != nil || n != 1 {
		t.Errorf("after replay: %d parsed reports (%v), want the one kept", n, err)
	}
	var origins string
	if err := db.QueryRowContext(ctx, `SELECT string_agg(origin, ',' ORDER BY line_no) FROM exposure_declarations
	    WHERE instrument_external_id = 'PE1'`).Scan(&origins); err != nil || origins != "parsed,ledger" {
		t.Errorf("after replay: origins %q (%v), want each kept", origins, err)
	}
	if got := leafText(t, db, ctx, "string_agg(DISTINCT country, ',')", `iid = 'PE1'`); got != "CH" {
		t.Errorf("after replay: PE1 reads %q before the parsed date, want the undated record", got)
	}
}

// A parsed record takes each dimension it does not state from the ledger
// record in force on its day, crossed with what it states; a dimension
// it states is always its own.
func TestExposureParsedDeclarationCompletedFromLedger(t *testing.T) {
	t.Parallel()
	jul, aug, sep := exDay(7, 1), exDay(8, 1), exDay(9, 1)
	region := `{"region":{"europe":40,"north_america":60}}`
	hand := func(line int, asOf *int64) canonical.ExposureDeclaration {
		return declaration(line, "PE1", asOf,
			split{"currency", []string{"EUR", "unknown"}, []float64{50, 50}},
			split{"asset_class", []string{"infrastructure", "private_equity"}, []float64{75, 25}})
	}
	// parts reads PE1 on day as region/class/currency=share of its 80. A
	// currency the ledger leaves unknown reads -, where an undeclared one
	// reads the holding's own USD.
	parts := func(t *testing.T, db *sql.DB, ctx context.Context, day int64) string {
		t.Helper()
		var s sql.NullString
		if err := db.QueryRowContext(ctx, `SELECT string_agg(COALESCE(region, '-') || '/' || asset_class || '/'
		    || COALESCE(currency, '-') || '=' || round(value / 80, 4), ' '
		    ORDER BY region, asset_class, currency)
		    FROM exposure_leaves(?, 'USD') WHERE iid = 'PE1'`, day+86399).Scan(&s); err != nil {
			t.Fatal(err)
		}
		return s.String
	}
	crossed := "europe/infrastructure/EUR=0.15 europe/infrastructure/-=0.15 europe/private_equity/EUR=0.05 " +
		"europe/private_equity/-=0.05 north_america/infrastructure/EUR=0.225 " +
		"north_america/infrastructure/-=0.225 north_america/private_equity/EUR=0.075 " +
		"north_america/private_equity/-=0.075"
	handOnly := "-/infrastructure/EUR=0.375 -/infrastructure/-=0.375 -/private_equity/EUR=0.125 " +
		"-/private_equity/-=0.125"
	for _, tc := range []struct {
		name   string
		ledger []canonical.ExposureDeclaration
		want   map[int64]string
	}{
		{"a dated ledger record before the report", []canonical.ExposureDeclaration{hand(3, &jul)},
			map[int64]string{jul: handOnly, sep: crossed}},
		{"an undated ledger record", []canonical.ExposureDeclaration{hand(3, nil)},
			map[int64]string{jul: handOnly, sep: crossed}},
		{"a ledger record dated after the report wins whole", []canonical.ExposureDeclaration{
			declaration(3, "PE1", &sep, split{"asset_class", []string{"infrastructure"}, []float64{100}})},
			map[int64]string{aug: "europe/private_equity/USD=0.4 north_america/private_equity/USD=0.6",
				sep: "-/infrastructure/USD=1.0"}},
		{"a dated ledger record before the report, beside an undated one", []canonical.ExposureDeclaration{
			hand(3, &jul), declaration(4, "PE1", nil, split{"asset_class", []string{"real_estate"}, []float64{100}})},
			map[int64]string{jul: handOnly, sep: crossed}},
		{"a dimension both state is the report's", []canonical.ExposureDeclaration{
			declaration(3, "PE1", &jul, split{"region", []string{"asia_pacific"}, []float64{100}},
				split{"currency", []string{"EUR", "unknown"}, []float64{50, 50}})},
			map[int64]string{jul: "asia_pacific/private_equity/EUR=0.5 asia_pacific/private_equity/-=0.5",
				sep: "europe/private_equity/EUR=0.2 europe/private_equity/-=0.2 " +
					"north_america/private_equity/EUR=0.3 north_america/private_equity/-=0.3"}},
		{"no ledger record, or only a situs", []canonical.ExposureDeclaration{
			{Line: 3, SilverSourceID: "test-src", InstrumentExternalID: "PE1", AsOf: &jul, Situs: "LU"}},
			map[int64]string{sep: "europe/private_equity/USD=0.4 north_america/private_equity/USD=0.6"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			db, ctx := openExposureFixture(t)
			parseReport(t, db, ctx, aug, region)
			stampDeclarations(t, db, ctx, tc.ledger...)
			for day, want := range tc.want {
				if got := parts(t, db, ctx, day); got != want {
					t.Errorf("on %s:\n got %s\nwant %s", time.Unix(day, 0).UTC().Format(time.DateOnly), got, want)
				}
			}
		})
	}
}
