package gold

import (
	"testing"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
)

// The exposure tables hold exactly the config's block after each sync,
// and the settings row is always there.
func TestSyncExposure(t *testing.T) {
	t.Parallel()
	db, ctx := openMigrated(t)
	asOf := int64(1719705600) // 2024-06-30
	rows := canonical.ExposureRows{
		Proxies: []canonical.ExposureProxy{
			{FundRef: "symbol:LEV3", ProxyRef: "symbol:IDX1", Leverage: 3},
			{FundRef: "isin:XX0000000001", ProxyRef: "isin:XX0000000002", Always: true, Leverage: 1},
		},
		ReferenceFunds: []string{"symbol:IDX9"},
		SizeFunds:      []canonical.ExposureSizeFund{{FundRef: "symbol:ALL9", CoveragePct: 100}},
		Declarations: []canonical.ExposureDeclaration{{Line: 1, SilverSourceID: "manual", InstrumentExternalID: "house-1",
			Dimensions: []string{"country", "currency"}, Note: "the house",
			Leaves: []canonical.ExposureDeclaredLeaf{{Share: 1, Country: "CH", Currency: "CHF"}}},
			{Line: 2, SilverSourceID: "manual", InstrumentExternalID: "fund-1", AsOf: &asOf, Dimensions: []string{"region"},
				Source: `{"page":4}`, Leaves: []canonical.ExposureDeclaredLeaf{{Share: 0.6, Region: "europe"},
					{Share: 0.4, Region: "unknown"}}}},
		Custody:  []canonical.ExposureCustody{{SilverSourceID: "bank", Custodian: "Example Bank AG"}},
		Settings: defaultExposureSettings(),
	}
	if n, err := SyncExposure(ctx, db, rows); err != nil || n != 7 {
		t.Fatalf("sync: %d %v", n, err)
	}
	var records, leaves string
	if err := db.QueryRowContext(ctx, `SELECT
        (SELECT string_agg(line_no || ' ' || instrument_external_id || ' ' || COALESCE(CAST(as_of AS VARCHAR), '-') || ' ' ||
                dimensions || ' ' || COALESCE(CAST(source AS VARCHAR), '-') || ' ' || COALESCE(note, '-'), '; '
                ORDER BY line_no) FROM exposure_declarations),
        (SELECT string_agg(line_no || '.' || leaf_no || ' ' || share || ' ' || COALESCE(region, '-') || ' ' ||
                COALESCE(country, '-') || ' ' || COALESCE(currency, '-'), '; ' ORDER BY line_no, leaf_no)
           FROM exposure_declaration_leaves)`).Scan(&records, &leaves); err != nil {
		t.Fatal(err)
	}
	if records != `1 house-1 - country,currency - the house; 2 fund-1 1719705600 region {"page":4} -` {
		t.Errorf("declarations = %q", records)
	}
	if leaves != "1.1 1.0 - CH CHF; 2.1 0.6 europe - -; 2.2 0.4 unknown - -" {
		t.Errorf("leaves = %q", leaves)
	}
	rows.Proxies = rows.Proxies[:1]
	rows.Declarations, rows.Custody = nil, nil
	rows.Settings.MaxGapDays = 200
	if n, err := SyncExposure(ctx, db, rows); err != nil || n != 3 {
		t.Fatalf("resync: %d %v", n, err)
	}
	var sizeFunds string
	if err := db.QueryRowContext(ctx, `SELECT string_agg(fund_ref || ':' || coverage_pct, ',') FROM exposure_size_funds`).
		Scan(&sizeFunds); err != nil || sizeFunds != "symbol:ALL9:100.0" {
		t.Errorf("size funds = %q (%v)", sizeFunds, err)
	}
	var proxies, declared, custody, settings string
	if err := db.QueryRowContext(ctx, `SELECT
        (SELECT string_agg(fund_ref || '>' || proxy_ref || ':' || leverage, ',') FROM exposure_proxies),
        (SELECT (SELECT COUNT(*) FROM exposure_declarations) || '/' || COUNT(*) FROM exposure_declaration_leaves),
        (SELECT COALESCE(string_agg(custodian, ','), '') FROM exposure_custody),
        (SELECT string_agg(max_gap_days || '/' || aged_days || '/' || partial_pct, ',') FROM exposure_settings)`).
		Scan(&proxies, &declared, &custody, &settings); err != nil {
		t.Fatal(err)
	}
	if proxies != "symbol:LEV3>symbol:IDX1:3.0" || declared != "0/0" || custody != "" || settings != "200/35/3.0" {
		t.Errorf("tables after resync: proxies %q declarations %q custody %q settings %q", proxies, declared, custody,
			settings)
	}
}

// A gold whose settings were never stamped reads the defaults the config
// stamps: the macro's fallbacks are canonical's.
func TestExposureSettingsRowDefaultsAreCanonicals(t *testing.T) {
	t.Parallel()
	db, ctx := openMigrated(t)
	var maxGap, aged int
	var partial float64
	if err := db.QueryRowContext(ctx, `SELECT max_gap_days, aged_days, partial_pct FROM exposure_settings_row()`).
		Scan(&maxGap, &aged, &partial); err != nil {
		t.Fatal(err)
	}
	if maxGap != canonical.DefaultMaxGapDays || aged != canonical.DefaultAgedDays || partial != canonical.DefaultPartialPct {
		t.Errorf("exposure_settings_row() = %d, %d, %v; canonical's defaults are %d, %d, %v", maxGap, aged, partial,
			canonical.DefaultMaxGapDays, canonical.DefaultAgedDays, canonical.DefaultPartialPct)
	}
}
