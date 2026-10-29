package gold

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
)

// The exposure fixture: one invented brokerage account holding a fund of
// each kind the look-through handles, a stock, a property, a private
// fund, gold, coins and cash, as of 2026-06-30, and a second account
// with a position no rate converts. Every name and identifier is
// invented.
//
//	FUNDX   holdings: STKA 50, BBB 20, CNG 10, cash 18 (a 2% remainder)
//	        and a keyless future at 0; CNG is labelled "Conglomerates",
//	        which no map knows
//	FOF     a fund of funds: FUNDX 60, a government bond 40
//	LEV3    no composition; the config proxies it by IDX1 at leverage 3
//	        under its ISIN, and at leverage 2 under its ticker
//	IDX1    holdings: STKA 100
//	GAPF    compositions 450 days before and 92 days after: the later
//	        one, flagged forward
//	QHDG     a EUR stock, hedged into USD
//	SWP     a swap fund: a collateral basket and the index lines, one
//	        of them with no asset type
//	QFXF     an EUR stock and a forward long EUR 500 against USD, net
//	        assets 1000
//	OLDF    compositions at Jan 31 and May 31
//	QT1     a chain of funds four deep: QT1 holds QT2, QT2 QT3, QT3
//	        QT4, QT4 STKA; GLEIF places QT4 in Luxembourg
//	NOCF    a fund with no composition, domiciled in Ireland
//	STKD    a stock nothing describes but its ISIN
//	HOUSE   declared: Switzerland, Swiss francs
//	QPRV    a private fund declared by region (north_america 55, europe
//	        30, rest 15), asset class (private_equity 72, infrastructure
//	        28) and currency (USD 60, EUR 40)
var (
	exAsOf = time.Date(2026, 6, 30, 23, 59, 59, 0, time.UTC).Unix()
	exSnap = time.Date(2026, 6, 30, 0, 0, 0, 0, time.UTC).Unix()
)

func exDay(m time.Month, d int, y ...int) int64 {
	year := 2026
	if len(y) > 0 {
		year = y[0]
	}
	return time.Date(year, m, d, 0, 0, 0, 0, time.UTC).Unix()
}

func seedExposureFixture(t *testing.T, db *sql.DB, ctx context.Context) {
	t.Helper()
	mustExec(t, db, `INSERT INTO accounts (silver_source_id, account_external_id, account_kind, display_name, base_currency, first_seen_at, last_seen_at)
	    VALUES ('test-src', 'A1', 'brokerage', 'Brokerage', 'USD', 0, 0)`)
	mustExec(t, db, `INSERT INTO fx_rates (silver_source_id, snapshot_at, base_currency, quote_currency, mid_rate)
	    VALUES ('test-src', ?, 'USD', 'EUR', 1.1), ('test-src', ?, 'USD', 'CHF', 1.25)`, exDay(1, 1), exDay(1, 1))
	type inst struct {
		id, class, vehicle, isin, symbol, name, ccy string
		mv                                          int
	}
	for _, i := range []inst{
		{"FUNDX", "public_equity", "etf", "", "FUNDX", "Fund X", "USD", 1000},
		{"FOF", "multi_asset", "etf", "", "FOF", "Fund of Funds", "USD", 500},
		{"LEV3", "public_equity", "etf", "XX00000000L3", "LEV3", "Leveraged Three", "USD", 200},
		{"GAPF", "public_equity", "etf", "", "GAPF", "Gap Fund", "USD", 100},
		{"QHDG", "public_equity", "fund", "", "QHDG", "Hedged Fund", "USD", 300},
		{"SWP", "public_equity", "etf", "", "SWP", "Swap Fund", "USD", 400},
		{"QFXF", "public_equity", "fund", "", "QFXF", "Forward Fund", "USD", 100},
		{"OLDF", "public_equity", "etf", "", "OLDF", "Old Fund", "USD", 100},
		{"STKA", "public_equity", "stock", "XX0000000001", "", "Stock A", "USD", 250},
		{"UNK", "public_equity", "stock", "", "UNK", "Unknown Corp", "USD", 50},
		{"HOUSE", "real_estate", "physical", "", "", "House", "CHF", 1000},
		{"PE1", "private_equity", "fund", "", "", "Private Fund", "USD", 80},
		{"GOLD", "metal", "physical", "", "", "Gold bars", "USD", 70},
		{"LOAN", "real_estate", "mortgage", "", "", "Home loan", "CHF", -600},
		{"BOND9", "fixed_income", "bond", "XX0000000009", "", "Bond Nine", "EUR", 100},
		{"QT1", "public_equity", "etf", "", "QT1", "Chain One", "USD", 40},
		{"NOCF", "public_equity", "etf", "IE0000000005", "NOCF", "No Composition Fund", "USD", 60},
		{"STKD", "public_equity", "stock", "DE0000000007", "", "Stock D", "USD", 20},
		{"COIN1", "crypto", "physical", "", "BTC", "Bitcoin", "USD", 30},
		{"QBTP", "crypto", "etf", "", "QBTP", "Example Bitcoin ETP", "USD", 15},
		{"QPRV", "private_equity", "fund", "", "", "Private Fund Two", "USD", 200},
	} {
		mustExec(t, db, `INSERT INTO instruments (silver_source_id, instrument_external_id, asset_class, vehicle, isin, symbol, name, currency, first_seen_at, last_seen_at)
		    VALUES ('test-src', ?, ?, ?, NULLIF(?, ''), NULLIF(?, ''), ?, ?, 0, 0)`, i.id, i.class, i.vehicle, i.isin, i.symbol, i.name, i.ccy)
		mustExec(t, db, `INSERT INTO positions (silver_source_id, snapshot_at, account_external_id, position_key, instrument_external_id,
		    asset_class, vehicle, currency, quantity, market_value) VALUES ('test-src', ?, 'A1', ?, ?, ?, ?, ?, 1, ?)`,
			exSnap, i.id, i.id, i.class, i.vehicle, i.ccy, i.mv)
	}
	mustExec(t, db, `INSERT INTO cash_balances (silver_source_id, snapshot_at, account_external_id, currency, balance_kind, amount)
	    VALUES ('test-src', ?, 'A1', 'USD', 'current', 1000), ('test-src', ?, 'A1', 'EUR', 'current', 100)`, exSnap, exSnap)
	// A2: no rate converts its one position, so holdings global leaves the
	// whole account out, its cash too.
	mustExec(t, db, `INSERT INTO accounts (silver_source_id, account_external_id, account_kind, display_name, base_currency, first_seen_at, last_seen_at)
	    VALUES ('test-src', 'A2', 'brokerage', 'Second', 'USD', 0, 0)`)
	mustExec(t, db, `INSERT INTO positions (silver_source_id, snapshot_at, account_external_id, position_key, instrument_external_id,
	    asset_class, vehicle, currency, quantity, market_value) VALUES ('test-src', ?, 'A2', 'QJPY', 'QJPY', 'public_equity', 'stock', 'JPY', 1, 5000)`,
		exSnap)
	mustExec(t, db, `INSERT INTO cash_balances (silver_source_id, snapshot_at, account_external_id, currency, balance_kind, amount)
	    VALUES ('test-src', ?, 'A2', 'USD', 'current', 100)`, exSnap)

	comp := func(ref string, asOf int64, basis, ccy string, net any, lines ...[]any) {
		t.Helper()
		mustExec(t, db, `INSERT INTO fund_compositions (silver_source_id, fund_ref, as_of, basis, provider, line_count, currency, net_assets)
		    VALUES ('test-src', ?, ?, ?, 'testprov', ?, ?, ?)`, ref, asOf, basis, len(lines), ccy, net)
		for n, l := range lines {
			args := append([]any{ref, asOf, basis, n + 1}, l...)
			mustExec(t, db, `INSERT INTO fund_lines (silver_source_id, fund_ref, as_of, basis, line_no, security_key, isin, ticker, name,
			    asset_type, weight, currency, country, sector, long_currency, long_amount, short_currency)
			    VALUES ('test-src', ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, args...)
		}
	}
	line := func(key, isin, ticker, name, assetType string, w float64, ccy, country, sector string) []any {
		return []any{nullableEnumValue(key), nullableEnumValue(isin), nullableEnumValue(ticker), name,
			nullableEnumValue(assetType), w, ccy, nullableEnumValue(country), nullableEnumValue(sector), nil, nil, nil}
	}
	may := exDay(5, 31)
	comp("symbol:FUNDX", may, "holdings", "USD", nil,
		line("isin:XX0000000001", "XX0000000001", "AAA", "Stock A", "Equity", 50, "USD", "United States", "Information Technology"),
		line("symbol:BBB", "", "BBB", "Stock B", "Equity", 20, "EUR", "Germany", "Health Care"),
		line("symbol:CNG", "", "CNG", "Conglomerate C", "Equity", 10, "USD", "United States", "Conglomerates"),
		line("", "", "", "USD Cash", "Cash", 18, "USD", "", ""),
		line("", "", "", "Index Future", "Futures", 0, "USD", "", ""))
	comp("symbol:FOF", may, "holdings", "USD", nil,
		line("symbol:FUNDX", "", "FUNDX", "Fund X", "Fund", 60, "USD", "", ""),
		line("isin:XX0000000003", "XX0000000003", "", "Gov Bond", "Fixed Income", 40, "USD", "United States", "Treasury"))
	comp("symbol:IDX1", may, "holdings", "USD", nil,
		line("isin:XX0000000001", "XX0000000001", "AAA", "Stock A", "Equity", 100, "USD", "United States", "Information Technology"))
	comp("symbol:GAPF", exDay(4, 1, 2025), "holdings", "USD", nil,
		line("symbol:OLD", "", "OLD", "Old Co", "Equity", 100, "USD", "Canada", "Energy"))
	comp("symbol:GAPF", exDay(9, 30), "holdings", "USD", nil,
		line("symbol:NEW", "", "NEW", "New Co", "Equity", 100, "USD", "Japan", "Energy"))
	comp("symbol:QHDG", may, "holdings", "EUR", nil,
		line("symbol:QEUS", "", "QEUS", "Euro Stock", "Equity", 100, "EUR", "France", "Industrials"))
	mustExec(t, db, `INSERT INTO fund_attributes (silver_source_id, fund_ref, as_of, hedged_currency) VALUES ('test-src', 'symbol:QHDG', ?, 'USD')`, may)
	comp("symbol:SWP", may, "collateral", "USD", nil,
		line("symbol:BSK", "", "BSK", "Basket Co", "Equity", 100, "USD", "United States", "Financials"))
	comp("symbol:SWP", may, "index", "USD", nil,
		line("symbol:QEMA", "", "QEMA", "EM Co A", "Equity", 60, "USD", "Brazil", "Materials"),
		line("symbol:QEMB", "", "QEMB", "EM Co B", "", 40, "USD", "India", "Technology"))
	comp("symbol:QFXF", may, "holdings", "USD", 1000,
		line("symbol:QEUS", "", "QEUS", "Euro Stock", "Equity", 100, "EUR", "France", "Industrials"),
		[]any{"symbol:FWD", nil, nil, "EUR/USD forward", "FX Forward", 0, "EUR", nil, nil, "EUR", 500, "USD"})
	comp("symbol:OLDF", exDay(1, 31), "holdings", "USD", nil,
		line("symbol:JAN", "", "JAN", "January Co", "Equity", 100, "USD", "United States", "Energy"))
	comp("symbol:OLDF", may, "holdings", "USD", nil,
		line("symbol:MAY", "", "MAY", "May Co", "Equity", 100, "USD", "United States", "Utilities"))
	comp("symbol:QT1", may, "holdings", "USD", nil, line("symbol:QT2", "", "QT2", "Chain Two", "Fund", 100, "USD", "", ""))
	comp("symbol:QT2", may, "holdings", "USD", nil, line("symbol:QT3", "", "QT3", "Chain Three", "Fund", 100, "USD", "", ""))
	comp("symbol:QT3", may, "holdings", "USD", nil, line("symbol:QT4", "", "QT4", "Chain Four", "Fund", 100, "USD", "", ""))
	comp("symbol:QT4", may, "holdings", "USD", nil,
		line("isin:XX0000000001", "XX0000000001", "AAA", "Stock A", "Equity", 100, "USD", "United States", "Information Technology"))
	// The funds are US funds: their lines list on US exchanges, so a
	// line's ticker names the US symbol.
	mustExec(t, db, `UPDATE fund_lines SET exchange = 'NYSE' WHERE ticker IS NOT NULL`)
	mustExec(t, db, `INSERT INTO security_facts (silver_source_id, security_key, field, as_of, provider, value)
	    VALUES ('test-src', 'isin:XX0000000009', 'legal_country', ?, 'gleif', 'DE'),
	           ('test-src', 'isin:IE0000000005', 'legal_country', ?, 'gleif', 'IE'),
	           ('test-src', 'isin:XX0000000001', 'risk_country', ?, 'otherprov', 'IE'),
	           ('test-src', 'symbol:BBB', 'risk_country', ?, 'otherprov', 'NL'),
	           ('test-src', 'symbol:QT4', 'legal_country', ?, 'gleif', 'LU')`, may, may, may, may, may)
	if _, err := SyncExposure(ctx, db, exposureFixtureRows()); err != nil {
		t.Fatal(err)
	}
	if _, err := RebuildExposure(ctx, db); err != nil {
		t.Fatal(err)
	}
}

// exposureFixtureRows is the fixture's exposure block, for a test to
// extend and stamp again.
func exposureFixtureRows() canonical.ExposureRows {
	return canonical.ExposureRows{
		Proxies: []canonical.ExposureProxy{
			{FundRef: "isin:XX00000000L3", ProxyRef: "symbol:IDX1", Leverage: 3},
			{FundRef: "symbol:LEV3", ProxyRef: "symbol:IDX1", Leverage: 2},
		},
		Declarations: []canonical.ExposureDeclaration{
			declaration(1, "HOUSE", nil, split{"country", []string{"CH"}, []float64{100}},
				split{"currency", []string{"CHF"}, []float64{100}}),
			declaration(2, "QPRV", nil,
				split{"region", []string{"north_america", "europe", "other"}, []float64{55, 30, 15}},
				split{"currency", []string{"USD", "EUR"}, []float64{60, 40}},
				split{"asset_class", []string{"private_equity", "infrastructure"}, []float64{72, 28}}),
		},
		Settings: defaultExposureSettings(),
	}
}

// stampDeclarations stamps the fixture's exposure block with ds added to
// its declarations.
func stampDeclarations(t *testing.T, db *sql.DB, ctx context.Context, ds ...canonical.ExposureDeclaration) {
	t.Helper()
	rows := exposureFixtureRows()
	rows.Declarations = append(rows.Declarations, ds...)
	if _, err := SyncExposure(ctx, db, rows); err != nil {
		t.Fatal(err)
	}
}

// defaultExposureSettings are the exposure settings an exposure block
// that states none reads as.
func defaultExposureSettings() canonical.ExposureSettings {
	return canonical.ExposureSettings{MaxGapDays: canonical.DefaultMaxGapDays, AgedDays: canonical.DefaultAgedDays,
		PartialPct: canonical.DefaultPartialPct}
}

// rebuildExposureWithDefaults stamps an exposure block of the default
// settings alone and rebuilds the exposure tables.
func rebuildExposureWithDefaults(t *testing.T, db *sql.DB, ctx context.Context) {
	t.Helper()
	if _, err := SyncExposure(ctx, db, canonical.ExposureRows{Settings: defaultExposureSettings()}); err != nil {
		t.Fatal(err)
	}
	if _, err := RebuildExposure(ctx, db); err != nil {
		t.Fatal(err)
	}
}

// split is one dimension of a declaration: its values and their shares
// in percent.
type split struct {
	dim    string
	values []string
	pcts   []float64
}

// declaration is a record of the declarations ledger for a test-src
// holding, its splits crossed into leaves as the config's reader crosses
// them. The splits come in the order of canonical.DeclarationDimensions.
func declaration(line int, iid string, asOf *int64, splits ...split) canonical.ExposureDeclaration {
	d := canonical.ExposureDeclaration{Line: line, SilverSourceID: "test-src", InstrumentExternalID: iid, AsOf: asOf}
	leaves := []canonical.ExposureDeclaredLeaf{{Share: 1}}
	for _, s := range splits {
		d.Dimensions = append(d.Dimensions, s.dim)
		var next []canonical.ExposureDeclaredLeaf
		for _, l := range leaves {
			for i, v := range s.values {
				n := l
				n.Share *= s.pcts[i] / 100
				n.Set(s.dim, v)
				next = append(next, n)
			}
		}
		leaves = next
	}
	d.Leaves = leaves
	return d
}

// exposureImage is the seeded and rebuilt fixture, built once per
// process and copied per test, as OpenFresh copies the fresh image.
var exposureImage struct {
	once sync.Once
	data []byte
}

// openExposureFixture opens a copy of the fixture of its own.
func openExposureFixture(t *testing.T) (*sql.DB, context.Context) {
	t.Helper()
	exposureImage.once.Do(func() {
		db, ctx := openMigrated(t)
		seedExposureFixture(t, db, ctx)
		var path string
		if err := db.QueryRowContext(ctx, `SELECT path FROM duckdb_databases() WHERE database_name = current_database()`).
			Scan(&path); err != nil {
			t.Fatal(err)
		}
		if _, err := db.ExecContext(ctx, `CHECKPOINT`); err != nil {
			t.Fatal(err)
		}
		if err := db.Close(); err != nil {
			t.Fatal(err)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		exposureImage.data = data
	})
	if exposureImage.data == nil {
		t.Fatal("the exposure fixture failed to build")
	}
	path := filepath.Join(t.TempDir(), "gold.db")
	if err := os.WriteFile(path, exposureImage.data, 0o600); err != nil {
		t.Fatal(err)
	}
	db, err := Open(path, ModeReadWrite)
	if err != nil {
		t.Fatalf("open the exposure fixture: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db, context.Background()
}

func parse(t *testing.T, s *string) float64 {
	t.Helper()
	if s == nil {
		return math.NaN()
	}
	v, err := strconv.ParseFloat(*s, 64)
	if err != nil {
		t.Fatalf("parse %q: %v", *s, err)
	}
	return v
}

// The invariant: every view sums to `holdings global`, in every output
// currency.
func TestExposureViewsSumToHoldingsGlobal(t *testing.T) {
	t.Parallel()
	db, ctx := openExposureFixture(t)
	for _, ccy := range []string{"USD", "CHF"} {
		g, err := GlobalAsOf(ctx, db, exAsOf, ccy)
		if err != nil {
			t.Fatal(err)
		}
		want := parse(t, g.TotalValueOutCcy)
		sum := func(name string, err error, n int, value func(i int) *string) {
			t.Helper()
			if err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			total := 0.0
			for i := range n {
				total += parse(t, value(i))
			}
			if math.Abs(total-want) > 0.01 {
				t.Errorf("%s %s sums to %.4f, holdings global is %.4f", ccy, name, total, want)
			}
		}
		classes, err := ExposureClasses(ctx, db, exAsOf, ccy)
		sum("classes", err, len(classes), func(i int) *string { return classes[i].Value })
		sectors, err := ExposureSectors(ctx, db, exAsOf, ccy)
		sum("sectors", err, len(sectors), func(i int) *string { return sectors[i].Value })
		sizes, err := ExposureSizes(ctx, db, exAsOf, ccy)
		sum("sizes", err, len(sizes), func(i int) *string { return sizes[i].Value })
		for _, mode := range []string{"risk", "domicile"} {
			countries, err := ExposureCountries(ctx, db, exAsOf, ccy, mode)
			sum("countries/"+mode, err, len(countries), func(i int) *string { return countries[i].Value })
		}
		currencies, err := ExposureCurrencies(ctx, db, exAsOf, ccy)
		sum("currencies", err, len(currencies), func(i int) *string { return currencies[i].Value })
		situs, err := ExposureSitus(ctx, db, exAsOf, ccy)
		sum("situs", err, len(situs), func(i int) *string { return situs[i].Value })
		for name, read := range map[string]func(context.Context, *sql.DB, int64, string) ([]ExposureBucketRow, error){
			"credit": ExposureCredit, "maturities": ExposureMaturities, "coupons": ExposureCoupons,
			"cycles": ExposureCycles} {
			rows, err := read(ctx, db, exAsOf, ccy)
			sum(name, err, len(rows), func(i int) *string { return rows[i].Value })
		}
		issuers, err := ExposureIssuers(ctx, db, exAsOf, ccy)
		sum("issuers", err, len(issuers), func(i int) *string { return issuers[i].Value })
		securities, err := ExposureSecurities(ctx, db, exAsOf, ccy, "risk")
		sum("securities", err, len(securities), func(i int) *string { return securities[i].Value })
		funds, err := ExposureFunds(ctx, db, exAsOf, ccy)
		sum("funds", err, len(funds), func(i int) *string { return funds[i].Value })
		coverage, err := ExposureCoverage(ctx, db, exAsOf, ccy)
		var how, bases []*string
		for _, r := range coverage {
			switch {
			case r.Section == "how":
				how = append(how, r.Value)
			case r.Section == "situs" && r.SilverSourceID == nil && r.Reason != "(no value)":
				bases = append(bases, r.Value)
			}
		}
		sum("coverage how", err, len(how), func(i int) *string { return how[i] })
		sum("coverage situs", err, len(bases), func(i int) *string { return bases[i] })
	}
}

// leafValue sums the USD leaves matching a filter.
func leafValue(t *testing.T, db *sql.DB, ctx context.Context, where string, args ...any) float64 {
	t.Helper()
	var v sql.NullFloat64
	q := `SELECT SUM(value) FROM exposure_leaves(?, 'USD') WHERE ` + where
	if err := db.QueryRowContext(ctx, q, append([]any{exAsOf}, args...)...).Scan(&v); err != nil {
		t.Fatalf("%s: %v", where, err)
	}
	return v.Float64
}

// closeTo fails the test where got is not want within 1e-6.
func closeTo(t *testing.T, name string, got, want float64) {
	t.Helper()
	if math.Abs(got-want) > 1e-6 {
		t.Errorf("%s = %.6f, want %.6f", name, got, want)
	}
}

func leafFlags(t *testing.T, db *sql.DB, ctx context.Context, where string) string {
	t.Helper()
	var s sql.NullString
	if err := db.QueryRowContext(ctx, `SELECT array_to_string(list_sort(list_distinct(flatten(list(flags)))), ',')
	    FROM exposure_leaves(?, 'USD') WHERE `+where,
		exAsOf).Scan(&s); err != nil {
		t.Fatalf("%s: %v", where, err)
	}
	return s.String
}

// leafText reads one column of the one USD leaf matching a filter, ""
// for NULL.
func leafText(t *testing.T, db *sql.DB, ctx context.Context, col, where string) string {
	t.Helper()
	var s sql.NullString
	if err := db.QueryRowContext(ctx, `SELECT CAST(`+col+` AS VARCHAR) FROM exposure_leaves(?, 'USD') WHERE `+where,
		exAsOf).Scan(&s); err != nil {
		t.Fatalf("%s of %s: %v", col, where, err)
	}
	return s.String
}

func TestExposureLooksThroughEachKindOfFund(t *testing.T) {
	t.Parallel()
	db, ctx := openExposureFixture(t)
	// FUNDX 1000: half in Stock A, which a fund line ties to the ISIN the
	// stock is held under; the 2% its weights leave is a remainder.
	closeTo(t, "FUNDX's Stock A", leafValue(t, db, ctx, `top_key = 'symbol:FUNDX' AND key = 'isin:XX0000000001'`), 500)
	closeTo(t, "FUNDX's remainder", leafValue(t, db, ctx, `top_key = 'symbol:FUNDX' AND kind = 'remainder'`), 20)
	// FOF 500: 60% is FUNDX, looked through again.
	closeTo(t, "FOF's Stock A", leafValue(t, db, ctx, `top_key = 'symbol:FOF' AND key = 'isin:XX0000000001' AND depth = 2`), 150)
	// LEV3 200 at leverage 3 of IDX1: 600 of Stock A, minus 400 of cash.
	closeTo(t, "LEV3's Stock A", leafValue(t, db, ctx, `top_key = 'isin:XX00000000L3' AND key = 'isin:XX0000000001'`), 600)
	closeTo(t, "LEV3's cash leg", leafValue(t, db, ctx, `top_key = 'isin:XX00000000L3' AND kind = 'leverage'`), -400)
	if f := leafFlags(t, db, ctx, `top_key = 'isin:XX00000000L3'`); !strings.Contains(f, "proxy") {
		t.Errorf("LEV3 flags = %q, want proxy", f)
	}
	// GAPF: the later composition, the earlier one past max_gap_days.
	closeTo(t, "GAPF's later line", leafValue(t, db, ctx, `top_key = 'symbol:GAPF' AND key = 'symbol:NEW'`), 100)
	if f := leafFlags(t, db, ctx, `top_key = 'symbol:GAPF'`); f != "forward" {
		t.Errorf("GAPF flags = %q, want forward", f)
	}
	// QHDG: a EUR stock hedged into USD.
	closeTo(t, "QHDG in USD", leafValue(t, db, ctx, `top_key = 'symbol:QHDG' AND currency = 'USD'`), 300)
	// SWP: the index lines, never the basket.
	closeTo(t, "SWP's basket", leafValue(t, db, ctx, `top_key = 'symbol:SWP' AND key = 'symbol:BSK'`), 0)
	closeTo(t, "SWP's index", leafValue(t, db, ctx, `top_key = 'symbol:SWP' AND key IN ('symbol:QEMA', 'symbol:QEMB')`), 400)
	// QFXF 100, net assets 1000: the forward's long leg is 500 EUR at 1.1,
	// 55% of the fund in EUR; the short leg is the rest of the line, in
	// the short currency although the line states the long one.
	closeTo(t, "QFXF's long EUR leg", leafValue(t, db, ctx, `top_key = 'symbol:QFXF' AND key = 'symbol:FWD' AND currency = 'EUR'`), 55)
	closeTo(t, "QFXF's short USD leg", leafValue(t, db, ctx, `top_key = 'symbol:QFXF' AND key = 'symbol:FWD' AND currency = 'USD'`), -55)
	// The house is declared; the private fund and the unknown stock are
	// not. Nothing looks through the private fund: it reads diversified.
	if f := leafFlags(t, db, ctx, `iid = 'PE1'`); f != "opaque,undeclared" {
		t.Errorf("PE1 flags = %q, want opaque,undeclared", f)
	}
	closeTo(t, "PE1 diversified", leafValue(t, db, ctx, `iid = 'PE1' AND sector = 'diversified'`), 80)
	if f := leafFlags(t, db, ctx, `iid = 'UNK'`); f != "unmatched" {
		t.Errorf("UNK flags = %q, want unmatched", f)
	}
	closeTo(t, "the house in CH", leafValue(t, db, ctx, `iid = 'HOUSE' AND country = 'CH'`), 1250)
	// Gold counts in XAU; a bond takes its domicile from GLEIF.
	closeTo(t, "gold in XAU", leafValue(t, db, ctx, `iid = 'GOLD' AND currency = 'XAU' AND sector = 'metal'`), 70)
	closeTo(t, "the bond in DE", leafValue(t, db, ctx, `iid = 'BOND9' AND country = 'DE'`), 110)
	// A2 is left out whole, as holdings global leaves it out.
	closeTo(t, "A2", leafValue(t, db, ctx, `acct = 'A2'`), 0)
	// QT1: three levels expand; QT4, a fund the third level names, stays
	// a leaf, flagged partial.
	closeTo(t, "QT4 at depth 3", leafValue(t, db, ctx, `top_key = 'symbol:QT1' AND depth = 3 AND key = 'symbol:QT4'`), 40)
	if how := leafText(t, db, ctx, "how", `top_key = 'symbol:QT1'`); how != "truncated" {
		t.Errorf("QT1's leaf is %q, want truncated", how)
	}
	if f := leafFlags(t, db, ctx, `top_key = 'symbol:QT1'`); f != "partial" {
		t.Errorf("QT1 flags = %q, want partial", f)
	}
	if c := leafText(t, db, ctx, "country", `top_key = 'symbol:QT1'`); c != "" {
		t.Errorf("QT4 counts in %q, want no country", c)
	}
	// A fund no composition describes has no country, though GLEIF states
	// its domicile; a stock only its ISIN places is not described.
	for _, iid := range []string{"NOCF", "STKD"} {
		if how := leafText(t, db, ctx, "how", `iid = '`+iid+`'`); how != "unknown" {
			t.Errorf("%s is %q, want unknown", iid, how)
		}
	}
	if c := leafText(t, db, ctx, "country", `iid = 'NOCF'`); c != "" {
		t.Errorf("NOCF country = %q, want none", c)
	}
	// A coin in a wallet is its symbol, one in a product the coin its
	// name states.
	for _, iid := range []string{"COIN1", "QBTP"} {
		if got := leafText(t, db, ctx, "currency || '|' || currency_kind", `iid = '`+iid+`'`); got != "BTC|crypto" {
			t.Errorf("%s currency = %q, want BTC|crypto", iid, got)
		}
	}
}

// The temporal join: the latest composition on or before the day, else
// the earliest after, flagged aged once older than aged_days.
func TestExposureTemporalJoin(t *testing.T) {
	t.Parallel()
	db, ctx := openExposureFixture(t)
	pick := func(asOf int64) (int64, bool, bool) {
		t.Helper()
		var at int64
		var aged, forward bool
		if err := db.QueryRowContext(ctx, `SELECT as_of, aged, forward FROM exposure_funds_at(?) WHERE fund_ref = 'symbol:OLDF'`,
			asOf).Scan(&at, &aged, &forward); err != nil {
			t.Fatal(err)
		}
		return at, aged, forward
	}
	if at, aged, _ := pick(exAsOf); at != exDay(5, 31) || aged {
		t.Errorf("at June 30: %d aged=%v, want May 31 and fresh", at, aged)
	}
	if at, aged, _ := pick(exDay(4, 15)); at != exDay(1, 31) || !aged {
		t.Errorf("at April 15: %d aged=%v, want January 31 and aged", at, aged)
	}
	if at, _, forward := pick(exDay(1, 1)); at != exDay(1, 31) || !forward {
		t.Errorf("at January 1: %d forward=%v, want January 31 and forward", at, forward)
	}
	// Days count whole: at the end of the 35th day after May 31 the
	// composition is not yet aged; a day later it is.
	if _, aged, _ := pick(exDay(7, 5) + 86399); aged {
		t.Errorf("35 days on: aged, want fresh")
	}
	if _, aged, _ := pick(exDay(7, 6) + 86399); !aged {
		t.Errorf("36 days on: fresh, want aged")
	}
}

// A label no map knows reaches coverage, by provider.
func TestExposureCoverageNamesUnmappedLabels(t *testing.T) {
	t.Parallel()
	db, ctx := openExposureFixture(t)
	rows, err := ExposureCoverage(ctx, db, exAsOf, "USD")
	if err != nil {
		t.Fatal(err)
	}
	found := map[string]bool{}
	for _, r := range rows {
		found[r.Section+"|"+r.Reason] = true
		if r.Section == "unmapped" && r.Reason == "sector: Conglomerates" && (r.Detail == nil || *r.Detail != "testprov") {
			t.Errorf("unmapped row = %+v, want the provider named", r)
		}
	}
	for _, want := range []string{"unmapped|sector: Conglomerates", "how|looked_through", "how|proxied", "how|declared",
		"how|cash", "how|asset_class", "how|security", "how|unknown", "how|remainder", "unmatched|symbol:UNK"} {
		if !found[want] {
			t.Errorf("coverage lacks %s", want)
		}
	}
}

// A declaration marks a private holding declared whatever it states, and
// a private company's stock is private, not a listed security; holdings
// and labels with no value share one coverage line each.
func TestExposureDeclarationsAndZeroValueCoverage(t *testing.T) {
	t.Parallel()
	db, ctx := openExposureFixture(t)
	for _, i := range []struct {
		id, class, vehicle string
		mv                 int
	}{{"PE2", "private_equity", "fund", 90}, {"PSTK", "private_equity", "stock", 20},
		{"MEMO1", "other", "other", 0}, {"MEMO2", "other", "other", 0}} {
		for _, q := range []string{
			`INSERT INTO instruments (silver_source_id, instrument_external_id, asset_class, vehicle, name, currency,
			 first_seen_at, last_seen_at) VALUES ('test-src', ?, ?, ?, ?, 'USD', 0, 0)`,
			`INSERT INTO positions (silver_source_id, snapshot_at, account_external_id, position_key, instrument_external_id,
			 asset_class, vehicle, currency, quantity, market_value) VALUES ('test-src', ?, 'A1', ?, ?, ?, ?, 'USD', 1, ?)`,
		} {
			args := []any{i.id, i.class, i.vehicle, i.id}
			if strings.Contains(q, "positions") {
				args = []any{exSnap, i.id, i.id, i.class, i.vehicle, i.mv}
			}
			if _, err := db.ExecContext(ctx, q, args...); err != nil {
				t.Fatal(err)
			}
		}
	}
	stampDeclarations(t, db, ctx, declaration(3, "PE2", nil,
		split{"sector", []string{"unknown"}, []float64{100}}))
	if f := leafFlags(t, db, ctx, `iid = 'PE2'`); f != "declared,opaque" {
		t.Errorf("declared PE2 flags = %q, want declared,opaque", f)
	}
	if got := leafText(t, db, ctx, "how || '|' || COALESCE(sector, '-')", `iid = 'PE2'`); got != "declared|-" {
		t.Errorf("declared PE2 is %q, want declared with no sector", got)
	}
	if f := leafFlags(t, db, ctx, `iid = 'PSTK'`); f != "undeclared" {
		t.Errorf("a private company's stock flags %q, want undeclared", f)
	}
	// A label no map knows, on a line of no weight.
	if _, err := db.ExecContext(ctx, `INSERT INTO fund_lines (silver_source_id, fund_ref, as_of, basis, line_no,
	    security_key, name, asset_type, weight, currency, sector) VALUES ('test-src', 'symbol:FUNDX', ?, 'holdings', 99,
	    'symbol:QZRO', 'Zero Line', 'Equity', 0, 'USD', 'Quasi Sector')`, exDay(5, 31)); err != nil {
		t.Fatal(err)
	}
	if _, err := RebuildExposure(ctx, db); err != nil {
		t.Fatal(err)
	}
	cov, err := ExposureCoverage(ctx, db, exAsOf, "USD")
	if err != nil {
		t.Fatal(err)
	}
	none := map[string]int64{}
	for _, r := range cov {
		if strings.Contains(r.Reason, "MEMO") || strings.Contains(r.Reason, "Quasi") {
			t.Errorf("an item with no value has a line of its own: %+v", r)
		}
		if r.Reason == "(no value)" {
			none[r.Section] = r.Items
		}
	}
	// The fixture's zero-weight future adds to the unmapped count.
	if none["undeclared"] != 2 || none["unmapped"] < 1 {
		t.Errorf("(no value) lines = %v, want 2 undeclared and the unmapped zero lines", none)
	}
	// A private holding with no declaration is listed by what a record
	// names; a listed one no source describes stays unmatched. Both are
	// of unknown situs too, which the situs section lists apart.
	var pe1, unk *ExposureCoverageRow
	for i, r := range cov {
		if r.Section == "situs" {
			continue
		}
		switch r.Reason {
		case "holding:test-src:PE1":
			pe1 = &cov[i]
		case "symbol:UNK":
			unk = &cov[i]
		}
	}
	if pe1 == nil || pe1.Section != "undeclared" || pe1.SilverSourceID == nil || *pe1.SilverSourceID != "test-src" ||
		pe1.Instrument == nil || *pe1.Instrument != "PE1" {
		t.Errorf("PE1 coverage = %+v", pe1)
	}
	if unk == nil || unk.Section != "unmatched" {
		t.Errorf("UNK coverage = %+v", unk)
	}
	// A holding at two sources has a line for each, as a ledger row each.
	for _, q := range []string{
		`INSERT INTO accounts (silver_source_id, account_external_id, account_kind, display_name, base_currency,
		 first_seen_at, last_seen_at) VALUES ('test-src2', 'B1', 'brokerage', 'Other', 'USD', 0, 0)`,
		`INSERT INTO instruments (silver_source_id, instrument_external_id, asset_class, vehicle, symbol, name, currency,
		 first_seen_at, last_seen_at) VALUES ('test-src2', 'UNK', 'public_equity', 'stock', 'UNK', 'Unknown Corp', 'USD', 0, 0)`,
		`INSERT INTO positions (silver_source_id, snapshot_at, account_external_id, position_key, instrument_external_id,
		 asset_class, vehicle, currency, quantity, market_value) VALUES ('test-src2', ?, 'B1', 'UNK', 'UNK',
		 'public_equity', 'stock', 'USD', 1, 30)`,
	} {
		if _, err := db.ExecContext(ctx, q, slices.Repeat([]any{exSnap}, strings.Count(q, "?"))...); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := RebuildExposure(ctx, db); err != nil {
		t.Fatal(err)
	}
	cov, err = ExposureCoverage(ctx, db, exAsOf, "USD")
	if err != nil {
		t.Fatal(err)
	}
	var srcs []string
	for _, r := range cov {
		if r.Section == "unmatched" && r.Reason == "symbol:UNK" {
			srcs = append(srcs, *r.SilverSourceID)
		}
	}
	if !slices.Equal(srcs, []string{"test-src", "test-src2"}) {
		t.Errorf("UNK lines by source = %v, want one each", srcs)
	}
}

// An SEC code that says nothing of the business (a depositary receipt's
// registrant) gives no sector, though a broad range holds it.
func TestExposureSICOfADepositaryGivesNoSector(t *testing.T) {
	t.Parallel()
	db, ctx := openMigrated(t)
	if _, err := db.ExecContext(ctx, `INSERT INTO security_facts (silver_source_id, security_key, field, as_of, provider,
	    value, scheme) VALUES ('refdata', 'symbol:QDEP', 'sic', 0, 'sec-edgar', '8880', 'sic'),
	    ('refdata', 'symbol:QENG', 'sic', 0, 'sec-edgar', '8711', 'sic')`); err != nil {
		t.Fatal(err)
	}
	if _, err := RebuildExposure(ctx, db); err != nil {
		t.Fatal(err)
	}
	for k, want := range map[string]string{"symbol:QDEP": "", "symbol:QENG": "industrials"} {
		var got sql.NullString
		if err := db.QueryRowContext(ctx, `SELECT sector FROM securities WHERE security_key = ?`, k).Scan(&got); err != nil {
			t.Fatal(err)
		}
		if got.String != want {
			t.Errorf("%s sector = %q, want %q", k, got.String, want)
		}
	}
}

// A ticker a fund line states beside an ISIN reads as that ISIN, and a
// CUSIP no source ties to an ISIN reads as the one the US rule gives.
func TestExposureAliases(t *testing.T) {
	t.Parallel()
	db, ctx := openExposureFixture(t)
	if _, err := db.ExecContext(ctx, `INSERT INTO instruments (silver_source_id, instrument_external_id, asset_class, vehicle, cusip, first_seen_at, last_seen_at)
	    VALUES ('test-src', 'C1', 'public_equity', 'stock', '00000AA01', 0, 0)`); err != nil {
		t.Fatal(err)
	}
	if _, err := RebuildExposure(ctx, db); err != nil {
		t.Fatal(err)
	}
	for alias, want := range map[string]string{
		"symbol:AAA":      "isin:XX0000000001",
		"cusip:00000AA01": "isin:US00000AA01" + string(rune('0'+canonical.ISINCheckDigit("US00000AA01"))),
	} {
		var got string
		if err := db.QueryRowContext(ctx, `SELECT security_key FROM security_aliases WHERE alias = ?`, alias).Scan(&got); err != nil {
			t.Fatalf("%s: %v", alias, err)
		}
		if got != want {
			t.Errorf("%s reads as %s, want %s", alias, got, want)
		}
	}
	var sector, origin string
	if err := db.QueryRowContext(ctx, `SELECT sector, sector_origin FROM securities WHERE security_key = 'isin:XX0000000001'`).
		Scan(&sector, &origin); err != nil {
		t.Fatal(err)
	}
	if sector != "technology" || origin != "fund_line:testprov" {
		t.Errorf("Stock A sector = %s from %s", sector, origin)
	}
	// The country of risk: a fund line's wins over a provider's.
	for key, want := range map[string]string{
		"symbol:BBB":        "DE|fund_line:testprov",
		"isin:XX0000000001": "US|fund_line:testprov",
	} {
		var got string
		if err := db.QueryRowContext(ctx, `SELECT risk_country || '|' || risk_country_origin FROM securities WHERE security_key = ?`,
			key).Scan(&got); err != nil {
			t.Fatalf("%s: %v", key, err)
		}
		if got != want {
			t.Errorf("%s risk country = %s, want %s", key, got, want)
		}
	}
}

// Where sources tie one ticker to two ISINs, an account's own instrument
// wins over lookups, however many state the other. A bank abroad's local
// ticker ties nothing: in New York it is another company's.
func TestExposureAliasPrecedence(t *testing.T) {
	t.Parallel()
	db, ctx := openMigrated(t)
	for _, q := range []string{
		`INSERT INTO instruments (silver_source_id, instrument_external_id, asset_class, vehicle, symbol, isin, currency,
		 first_seen_at, last_seen_at) VALUES ('test-src', 'C2', 'public_equity', 'stock', 'QQZ', 'XX0000000021', 'USD', 0, 0),
		 ('test-src', 'C3', 'public_equity', 'stock', 'QVN', 'AT0000000023', 'EUR', 0, 0)`,
		`INSERT INTO security_identities (silver_source_id, security_key, as_of, provider, isin, ticker)
		 VALUES ('test-src', 'symbol:QQZ', 0, 'p1', 'XX0000000022', 'QQZ'), ('test-src', 'symbol:QQZ', 0, 'p2', 'XX0000000022', 'QQZ')`,
	} {
		if _, err := db.ExecContext(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := RebuildExposure(ctx, db); err != nil {
		t.Fatal(err)
	}
	var got string
	if err := db.QueryRowContext(ctx, `SELECT security_key FROM security_aliases WHERE alias = 'symbol:QQZ'`).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != "isin:XX0000000021" {
		t.Errorf("symbol:QQZ reads as %s, want the instrument's ISIN", got)
	}
	var n int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM security_aliases WHERE alias = 'symbol:QVN'`).Scan(&n); err != nil || n != 0 {
		t.Errorf("a local ticker abroad ties %d aliases, %v", n, err)
	}
}

// A broker's symbol and CUSIP fill each other's gaps. A CUSIP reads as
// the ISIN a source states for it; a CINS code, else, as its symbol's,
// never a US one, the lowest of two equally new; a US CUSIP as its
// symbol's only where that ISIN holds the CUSIP (a Canadian one), else
// as the rule's, so a reused ticker leaves it alone.
// A symbol no source names reads as its CUSIP, keeping what a lookup
// found under it; a symbol priced abroad takes no part. No alias reads
// as another alias.
func TestExposureABrokersSymbolAndCUSIPFillEachOther(t *testing.T) {
	t.Parallel()
	db, ctx := openMigrated(t)
	isin := func(body string) string { return body + string(rune('0'+canonical.ISINCheckDigit(body))) }
	ca := isin("CA00000AB07")
	for _, q := range []string{
		`INSERT INTO instruments (silver_source_id, instrument_external_id, asset_class, vehicle, symbol, cusip, currency,
		 first_seen_at, last_seen_at) VALUES ('test-src', 'QCS1', 'public_equity', 'stock', 'QCS1', '00000AB02', 'USD', 0, 0),
		 ('test-src', 'QCS2', 'public_equity', 'stock', 'QCS2', '00000AB03', 'USD', 0, 0),
		 ('test-src', 'QCS3', 'public_equity', 'stock', 'QCS3', 'Q0000AB04', 'USD', 0, 0),
		 ('test-src', 'QCS4', 'public_equity', 'stock', 'QCS4', '00000AB05', 'USD', 0, 0),
		 ('test-src', 'QCS5', 'public_equity', 'stock', 'QCS5', '00000AB06', 'USD', 0, 0),
		 ('test-src', 'QCS6', 'public_equity', 'stock', 'QCS6', '00000AB07', 'USD', 0, 0),
		 ('test-src', 'QCS8', 'public_equity', 'stock', 'QCS8', 'Q0000AB08', 'USD', 0, 1),
		 ('test-src', 'QCS9', 'public_equity', 'stock', 'QCS9', 'Q0000AB08', 'USD', 0, 2),
		 ('test-src', 'QLCL', 'public_equity', 'stock', 'QLCL', '00000AB10', 'CHF', 0, 0),
		 ('test-src', 'QCSA', 'public_equity', 'stock', 'QCSA', 'Q0000AB11', 'USD', 0, 0),
		 ('test-src', 'QCSB', 'public_equity', 'stock', 'QCSB', 'Q0000AB12', 'USD', 0, 5),
		 ('test-src', 'QCSC', 'public_equity', 'stock', 'QCSC', 'Q0000AB12', 'USD', 0, 5)`,
		`INSERT INTO fund_lines (silver_source_id, fund_ref, as_of, basis, line_no, security_key, isin, cusip, ticker, exchange, currency, name, weight)
		 VALUES ('test-src', 'symbol:FUNDX', 0, 'holdings', 80, 'isin:XX0000000051', 'XX0000000051', '00000AB03', NULL, NULL, 'USD', 'Cusip Co', 0),
		        ('test-src', 'symbol:FUNDX', 0, 'holdings', 81, 'isin:XX0000000054', 'XX0000000054', NULL, 'QCS4', 'New York Stock Exchange', 'USD', 'New Co', 0),
		        ('test-src', 'symbol:FUNDX', 0, 'holdings', 82, 'isin:XX0000000056', 'XX0000000056', '00000AB06', NULL, NULL, 'USD', 'Receipt Co', 0),
		        ('test-src', 'symbol:FUNDX', 0, 'holdings', 83, 'isin:` + ca + `', '` + ca + `', NULL, 'QCS6', 'New York Stock Exchange', 'USD', 'Toronto Co', 0),
		        ('test-src', 'symbol:FUNDX', 0, 'holdings', 84, 'isin:US0000000611', 'US0000000611', NULL, 'QCSA', 'NASDAQ', 'USD', 'Other Co', 0)`,
		`INSERT INTO security_identities (silver_source_id, security_key, as_of, provider, isin, ticker)
		 VALUES ('test-src', 'symbol:QCS2', 0, 'p1', 'XX0000000052', 'QCS2'),
		        ('test-src', 'symbol:QCS9', 0, 'p1', 'XX0000000059', 'QCS9'),
		        ('test-src', 'symbol:QCSB', 0, 'p1', 'XX0000000622', 'QCSB'),
		        ('test-src', 'symbol:QCSC', 0, 'p1', 'XX0000000621', 'QCSC')`,
		`INSERT INTO security_facts (silver_source_id, security_key, field, as_of, provider, value)
		 VALUES ('test-src', 'symbol:QCS3', 'legal_country', 0, 'gleif', 'DE')`,
	} {
		if _, err := db.ExecContext(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := RebuildExposure(ctx, db); err != nil {
		t.Fatal(err)
	}
	for alias, want := range map[string]string{
		"symbol:QCS1": "isin:" + isin("US00000AB02"), "cusip:00000AB02": "isin:" + isin("US00000AB02"),
		"symbol:QCS2": "isin:XX0000000052", "cusip:00000AB03": "isin:XX0000000051",
		"symbol:QCS3": "cusip:Q0000AB04",
		"symbol:QCS4": "isin:XX0000000054", "cusip:00000AB05": "isin:" + isin("US00000AB05"),
		"symbol:QCS5": "isin:XX0000000056", "cusip:00000AB06": "isin:XX0000000056",
		"symbol:QCS6": "isin:" + ca, "cusip:00000AB07": "isin:" + ca,
		"symbol:QCS8": "isin:XX0000000059", "cusip:Q0000AB08": "isin:XX0000000059",
		"symbol:QLCL": "",
		"symbol:QCSA": "isin:US0000000611", "cusip:Q0000AB11": "",
		"cusip:Q0000AB12": "isin:XX0000000621",
	} {
		var got sql.NullString
		err := db.QueryRowContext(ctx, `SELECT security_key FROM security_aliases WHERE alias = ?`, alias).Scan(&got)
		if err != nil && err != sql.ErrNoRows {
			t.Fatalf("%s: %v", alias, err)
		}
		if got.String != want {
			t.Errorf("%s reads as %q, want %q", alias, got.String, want)
		}
	}
	var chained int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM security_aliases a JOIN security_aliases b ON b.alias = a.security_key`).
		Scan(&chained); err != nil || chained != 0 {
		t.Errorf("%d aliases read as another alias, %v", chained, err)
	}
	var domicile sql.NullString
	if err := db.QueryRowContext(ctx, `SELECT domicile FROM securities WHERE security_key = 'cusip:Q0000AB04'`).Scan(&domicile); err != nil {
		t.Fatal(err)
	}
	if domicile.String != "DE" {
		t.Errorf("the CUSIP's domicile = %q, want the lookup's under its symbol", domicile.String)
	}
}

// A fund line that names no security is a row of its own in securities,
// named by its fund and its name, never by the holding above it.
func TestExposureSecuritiesNameKeylessLines(t *testing.T) {
	t.Parallel()
	db, ctx := openExposureFixture(t)
	rows, err := ExposureSecurities(ctx, db, exAsOf, "USD", "risk")
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, r := range rows {
		if r.Security == "line:symbol:FUNDX:Index Future" {
			found = true
		}
		if strings.HasPrefix(r.Security, "holding:test-src:FUNDX") {
			t.Errorf("a FUNDX line reads as the holding: %+v", r)
		}
	}
	if !found {
		t.Error("securities lacks the keyless future")
	}
}

// EDGAR's codes read through EDGAR's own map: DE is Delaware, so the
// United States, never Germany. Another provider's DE is Germany.
func TestExposureEdgarCodesReadAsEdgarStatesThem(t *testing.T) {
	t.Parallel()
	db, ctx := openMigrated(t)
	if _, err := db.ExecContext(ctx, `INSERT INTO security_facts (silver_source_id, security_key, field, as_of, provider, value)
	    VALUES ('test-src', 'isin:XX0000000031', 'incorporation', 0, 'sec-edgar', 'DE'),
	           ('test-src', 'isin:XX0000000032', 'incorporation', 0, 'sec-edgar', 'E9'),
	           ('test-src', 'isin:XX0000000032', 'hq_country', 0, 'sec-edgar', 'K3'),
	           ('test-src', 'isin:XX0000000033', 'legal_country', 0, 'gleif', 'DE')`); err != nil {
		t.Fatal(err)
	}
	if _, err := RebuildExposure(ctx, db); err != nil {
		t.Fatal(err)
	}
	for key, want := range map[string]string{"isin:XX0000000031": "US|", "isin:XX0000000032": "KY|HK",
		"isin:XX0000000033": "DE|"} {
		var got string
		if err := db.QueryRowContext(ctx, `SELECT COALESCE(domicile, '') || '|' || COALESCE(risk_country, '')
		    FROM securities WHERE security_key = ?`, key).Scan(&got); err != nil {
			t.Fatalf("%s: %v", key, err)
		}
		if got != want {
			t.Errorf("%s domicile|risk = %s, want %s", key, got, want)
		}
	}
}

// A fund line's ticker reads as a US symbol only where the line names a
// US listing: a US ISIN, a US exchange, or no exchange and a US-dollar
// price. A local ticker elsewhere is another company's.
func TestExposureLocalTickersAreNoUSSymbols(t *testing.T) {
	t.Parallel()
	db, ctx := openMigrated(t)
	if _, err := db.ExecContext(ctx, `INSERT INTO fund_lines (silver_source_id, fund_ref, as_of, basis, line_no, security_key, isin, ticker, exchange, currency, name, weight)
	    VALUES ('test-src', 'symbol:FUNDX', ?, 'holdings', 90, 'isin:XX0000000041', 'XX0000000041', 'QLOC', 'SIX Swiss Exchange', 'CHF', 'Local Co', 0),
	           ('test-src', 'symbol:FUNDX', ?, 'holdings', 91, 'isin:XX0000000042', 'XX0000000042', 'QPAR', 'Nyse Euronext - Euronext Paris', 'EUR', 'Paris Co', 0),
	           ('test-src', 'symbol:FUNDX', ?, 'holdings', 92, 'isin:XX0000000043', 'XX0000000043', 'QNYC', 'New York Stock Exchange', 'USD', 'Listed Co', 0),
	           ('test-src', 'symbol:FUNDX', ?, 'holdings', 93, 'isin:US0000000044', 'US0000000044', 'QUSA', NULL, 'USD', 'Home Co', 0),
	           ('test-src', 'symbol:FUNDX', ?, 'holdings', 94, 'isin:XX0000000045', 'XX0000000045', 'QADR', NULL, 'USD', 'Dollar Co', 0),
	           ('test-src', 'symbol:FUNDX', ?, 'holdings', 95, 'isin:XX0000000046', 'XX0000000046', 'QZRH', NULL, 'CHF', 'Franc Co', 0),
	           ('test-src', 'symbol:FUNDX', ?, 'holdings', 96, 'isin:XX0000000047', 'XX0000000047', 'QTOR', 'Cboe Canada', 'CAD', 'Toronto Co', 0)`,
		exDay(5, 31), exDay(5, 31), exDay(5, 31), exDay(5, 31), exDay(5, 31), exDay(5, 31), exDay(5, 31)); err != nil {
		t.Fatal(err)
	}
	if _, err := RebuildExposure(ctx, db); err != nil {
		t.Fatal(err)
	}
	for alias, want := range map[string]string{"symbol:QLOC": "", "symbol:QPAR": "", "symbol:QNYC": "isin:XX0000000043",
		"symbol:QUSA": "isin:US0000000044", "symbol:QADR": "isin:XX0000000045", "symbol:QZRH": "", "symbol:QTOR": ""} {
		var got sql.NullString
		err := db.QueryRowContext(ctx, `SELECT security_key FROM security_aliases WHERE alias = ?`, alias).Scan(&got)
		if err != nil && err != sql.ErrNoRows {
			t.Fatal(err)
		}
		if got.String != want {
			t.Errorf("%s reads as %q, want %q", alias, got.String, want)
		}
	}
}

func TestMigration0126DDLIsRerunnable(t *testing.T) {
	t.Parallel()
	db, ctx := openExposureFixture(t)
	rerunMigrationDDL(t, db, ctx, "0126_exposure_report.sql")
	n, err := RebuildExposure(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	if n.Securities == 0 || n.Aliases == 0 {
		t.Errorf("after replay: rebuilt %+v", n)
	}
	if f := leafFlags(t, db, ctx, `iid = 'PE1'`); f != "opaque,undeclared" {
		t.Errorf("after replay: PE1 flags = %q", f)
	}
	if rows, err := ExposureSizes(ctx, db, exAsOf, "USD"); err != nil || len(rows) == 0 {
		t.Errorf("sizes after replay: %d rows, %v", len(rows), err)
	}
	if rows, err := ExposureCoverage(ctx, db, exAsOf, "USD"); err != nil || len(rows) == 0 {
		t.Errorf("coverage after replay: %d rows, %v", len(rows), err)
	}
	if rows, err := ExposureCredit(ctx, db, exAsOf, "USD"); err != nil || len(rows) == 0 {
		t.Errorf("credit after replay: %d rows, %v", len(rows), err)
	}
	// Only the current views read what nothing describes as unknown.
	sectors, err := ExposureSectors(ctx, db, exAsOf, "USD")
	if err != nil {
		t.Fatal(err)
	}
	var labels []string
	for _, r := range sectors {
		labels = append(labels, r.Sector)
	}
	if !slices.Contains(labels, canonical.Unknown) || slices.Contains(labels, "unclassified") {
		t.Errorf("sectors after replay: %v, want unknown and no unclassified", labels)
	}
}

// divergenceLines rebuilds the exposure fixture with stmts applied, each
// `?` the fixture's snapshot, and returns the coverage's divergence
// lines.
func divergenceLines(t *testing.T, stmts ...string) []string {
	t.Helper()
	db, ctx := openExposureFixture(t)
	for _, q := range stmts {
		if _, err := db.ExecContext(ctx, q, slices.Repeat([]any{exSnap}, strings.Count(q, "?"))...); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	if _, err := RebuildExposure(ctx, db); err != nil {
		t.Fatal(err)
	}
	return coverageDivergence(t, ctx, db)
}

// coverageDivergence returns the coverage's divergence lines, each as
// its reason and detail.
func coverageDivergence(t *testing.T, ctx context.Context, db *sql.DB) []string {
	t.Helper()
	cov, err := ExposureCoverage(ctx, db, exAsOf, "USD")
	if err != nil {
		t.Fatal(err)
	}
	var div []string
	for _, r := range cov {
		if r.Section == "divergence" {
			div = append(div, r.Reason+": "+*r.Detail)
		}
	}
	return div
}

// heldReportFunds returns the statements that hold report fund QRP<n>
// for each suffix n, proxied by QPX<n>, each with a composition on the
// fixture's snapshot.
func heldReportFunds(ns ...string) []string {
	var ins, pos, prox, comps []string
	for _, n := range ns {
		ins = append(ins, fmt.Sprintf("('test-src', 'QRP%s', 'public_equity', 'fund', 'QRP%s', 'Report Fund %s', 'USD', 0, 0)", n, n, n))
		pos = append(pos, fmt.Sprintf("('test-src', ?, 'A1', 'QRP%s', 'QRP%s', 'public_equity', 'fund', 'USD', 1, 100)", n, n))
		prox = append(prox, fmt.Sprintf("('symbol:QRP%s', 'symbol:QPX%s', FALSE, 1)", n, n))
		comps = append(comps, fmt.Sprintf("('test-src', 'symbol:QRP%s', ?, 'holdings', 'swissfunddata', 'report', 2, 'USD'), "+
			"('test-src', 'symbol:QPX%s', ?, 'holdings', 'testprov', 'issuer', 2, 'USD')", n, n))
	}
	return []string{
		`INSERT INTO instruments (silver_source_id, instrument_external_id, asset_class, vehicle, symbol, name, currency,
		 first_seen_at, last_seen_at) VALUES ` + strings.Join(ins, ", "),
		`INSERT INTO positions (silver_source_id, snapshot_at, account_external_id, position_key, instrument_external_id,
		 asset_class, vehicle, currency, quantity, market_value) VALUES ` + strings.Join(pos, ", "),
		`INSERT INTO exposure_proxies (fund_ref, proxy_ref, always, leverage) VALUES ` + strings.Join(prox, ", "),
		`INSERT INTO fund_compositions (silver_source_id, fund_ref, as_of, basis, provider, source_kind, line_count, currency)
		 VALUES ` + strings.Join(comps, ", "),
	}
}

// A dimension one side states for no line is left out: a report whose
// lines state no country, and that nothing places, compares on sectors
// alone, though its proxy's file labels a country.
func TestExposureDivergenceSkipsWhatTheReportLeavesOut(t *testing.T) {
	t.Parallel()
	div := divergenceLines(t, append(heldReportFunds("3"),
		`INSERT INTO fund_lines (silver_source_id, fund_ref, as_of, basis, line_no, security_key, isin, name, asset_type,
		 weight, currency, country, sector) VALUES
		 ('test-src', 'symbol:QRP3', ?, 'holdings', 1, 'symbol:QRF1', NULL, 'QLAND ONE', 'Equity', 60, 'JPY', NULL, 'Materials'),
		 ('test-src', 'symbol:QRP3', ?, 'holdings', 2, 'symbol:QRF2', NULL, 'QLAND TWO', 'Equity', 40, 'JPY', NULL, 'Industrials'),
		 ('test-src', 'symbol:QPX3', ?, 'holdings', 1, 'symbol:QPF1', NULL, 'QLAND ONE', 'Equity', 50, 'JPY', 'Japan', 'Materials'),
		 ('test-src', 'symbol:QPX3', ?, 'holdings', 2, 'symbol:QPF2', NULL, 'QLAND THREE', 'Equity', 50, 'JPY', 'Japan', 'Utilities')`)...)
	// Materials 60/50, industrials 40/0, utilities 0/50: 50 points.
	if want := []string{"symbol:QRP3: sector 50.0 points from symbol:QPX3"}; !slices.Equal(div, want) {
		t.Errorf("divergence = %v, want %v", div, want)
	}
}

// A dimension the proxy's file labels on none of its lines is left out,
// though its securities' own facts place some of them: a mix from those
// alone would read as a divergence. A label counts on a line of positive
// weight, in the country or the risk country column. A proxy whose file
// labels neither sector nor country is not listed.
func TestExposureDivergenceSkipsWhatTheProxyFileLeavesOut(t *testing.T) {
	t.Parallel()
	// Each report holds QLAND ONE 60 and QLAND TWO 40, both Japanese.
	var report []string
	for _, n := range []string{"4", "5", "6", "7"} {
		report = append(report, fmt.Sprintf(
			"('test-src', 'symbol:QRP%s', ?, 'holdings', 1, 'isin:JP0000000G01', 'JP0000000G01', 'QLAND ONE', 'Equity', 60, 'JPY', 'Japan', 'Materials'), "+
				"('test-src', 'symbol:QRP%s', ?, 'holdings', 2, 'isin:JP0000000G02', 'JP0000000G02', 'QLAND TWO', 'Equity', 40, 'JPY', 'Japan', 'Industrials')", n, n))
	}
	div := divergenceLines(t, append(heldReportFunds("4", "5", "6", "7"),
		`INSERT INTO security_facts (silver_source_id, security_key, field, as_of, provider, value) VALUES
		 ('refdata', 'isin:US0000000G03', 'sector', ?, 'testfact', 'Utilities'),
		 ('refdata', 'isin:US0000000G03', 'risk_country', ?, 'testfact', 'US')`,
		`INSERT INTO fund_lines (silver_source_id, fund_ref, as_of, basis, line_no, security_key, isin, name, asset_type,
		 weight, currency, country, sector) VALUES `+strings.Join(report, ", "),
		// QPX4 labels sectors, no country; QPX5 neither; QPX6 a risk
		// country on one line; QPX7 a country on a line of no weight.
		`INSERT INTO fund_lines (silver_source_id, fund_ref, as_of, basis, line_no, security_key, isin, name, asset_type,
		 weight, currency, country, risk_country, sector) VALUES
		 ('test-src', 'symbol:QPX4', ?, 'holdings', 1, 'isin:JP0000000G01', 'JP0000000G01', 'QLAND ONE', 'Equity', 50, 'JPY', NULL, NULL, 'Materials'),
		 ('test-src', 'symbol:QPX4', ?, 'holdings', 2, 'isin:US0000000G03', 'US0000000G03', 'QLAND THREE', 'Equity', 50, 'USD', NULL, NULL, 'Utilities'),
		 ('test-src', 'symbol:QPX5', ?, 'holdings', 1, 'isin:JP0000000G01', 'JP0000000G01', 'QLAND ONE', 'Equity', 50, 'JPY', NULL, NULL, NULL),
		 ('test-src', 'symbol:QPX5', ?, 'holdings', 2, 'isin:US0000000G03', 'US0000000G03', 'QLAND THREE', 'Equity', 50, 'USD', NULL, NULL, NULL),
		 ('test-src', 'symbol:QPX6', ?, 'holdings', 1, 'isin:JP0000000G01', 'JP0000000G01', 'QLAND ONE', 'Equity', 50, 'JPY', NULL, 'Japan', NULL),
		 ('test-src', 'symbol:QPX6', ?, 'holdings', 2, 'isin:US0000000G03', 'US0000000G03', 'QLAND THREE', 'Equity', 50, 'USD', NULL, NULL, NULL),
		 ('test-src', 'symbol:QPX7', ?, 'holdings', 1, 'isin:JP0000000G01', 'JP0000000G01', 'QLAND ONE', 'Equity', 50, 'JPY', NULL, NULL, NULL),
		 ('test-src', 'symbol:QPX7', ?, 'holdings', 2, 'isin:US0000000G03', 'US0000000G03', 'QLAND THREE', 'Equity', 50, 'USD', NULL, NULL, NULL),
		 ('test-src', 'symbol:QPX7', ?, 'holdings', 3, 'isin:US0000000G04', 'US0000000G04', 'QLAND FOUR', 'Equity', 0, 'USD', 'United States', NULL, NULL)`)...)
	// Materials 60/50, industrials 40/0, utilities 0/50: 50 points. The
	// countries, Japan 100 against Japan 50 and the US 50, are compared
	// only for QPX6, whose file labels a country.
	want := []string{"symbol:QRP4: sector 50.0 points from symbol:QPX4",
		"symbol:QRP6: country 50.0 points from symbol:QPX6"}
	if slices.Sort(div); !slices.Equal(div, want) {
		t.Errorf("divergence = %v, want %v", div, want)
	}
}
