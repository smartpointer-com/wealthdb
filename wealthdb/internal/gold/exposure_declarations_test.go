package gold

import (
	"database/sql"
	"math"
	"strings"
	"testing"

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
	// 0129, 0131, 0133 and 0137 re-create the look-through and its
	// views, so they are replayed after 0128, as Migrate would.
	rerunMigrationDDL(t, db, ctx, "0128_exposure_declarations.sql")
	rerunMigrationDDL(t, db, ctx, "0129_exposure_situs.sql")
	rerunMigrationDDL(t, db, ctx, "0131_exposure_unknown.sql")
	rerunMigrationDDL(t, db, ctx, "0133_deterministic_labels.sql")
	rerunMigrationDDL(t, db, ctx, "0137_exposure_yield_views.sql")
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

// A record whose holding no source states is listed by its line.
func TestDeclarationsNamingNoHolding(t *testing.T) {
	t.Parallel()
	db, ctx := openExposureFixture(t)
	stampDeclarations(t, db, ctx,
		declaration(7, "HOUS3", nil, split{"country", []string{"CH"}, []float64{100}}))
	if lines, err := DeclarationsNamingNoHolding(ctx, db); err != nil || len(lines) != 1 || lines[0] != 7 {
		t.Errorf("lines = %v, %v; want [7]", lines, err)
	}
}
