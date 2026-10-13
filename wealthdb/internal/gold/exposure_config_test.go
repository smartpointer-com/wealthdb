package gold

import (
	"testing"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
)

// The exposure tables hold exactly the config's block after each sync,
// and the settings row is always there.
func TestSyncExposure(t *testing.T) {
	db, ctx := openMigrated(t)
	rows := canonical.ExposureRows{
		Proxies: []canonical.ExposureProxy{
			{FundRef: "symbol:LEV3", ProxyRef: "symbol:IDX1", Leverage: 3},
			{FundRef: "isin:XX0000000001", ProxyRef: "isin:XX0000000002", Always: true, Leverage: 1},
		},
		ReferenceFunds: []string{"symbol:TOTL"},
		Tags:           []canonical.ExposureTag{{SilverSourceID: "manual", InstrumentExternalID: "house-1", Country: "CH", Currency: "CHF"}},
		Custody:        []canonical.ExposureCustody{{SilverSourceID: "bank", Custodian: "Example Bank AG"}},
		Settings:       canonical.ExposureSettings{MaxGapDays: 400, AgedDays: 35, PartialPct: 3},
	}
	if n, err := SyncExposure(ctx, db, rows); err != nil || n != 5 {
		t.Fatalf("sync: %d %v", n, err)
	}
	rows.Proxies = rows.Proxies[:1]
	rows.Tags, rows.Custody = nil, nil
	rows.Settings.MaxGapDays = 200
	if n, err := SyncExposure(ctx, db, rows); err != nil || n != 2 {
		t.Fatalf("resync: %d %v", n, err)
	}
	var proxies, tags, custody, settings string
	if err := db.QueryRowContext(ctx, `SELECT
        (SELECT string_agg(fund_ref || '>' || proxy_ref || ':' || leverage, ',') FROM exposure_proxies),
        (SELECT COALESCE(string_agg(instrument_external_id, ','), '') FROM exposure_tags),
        (SELECT COALESCE(string_agg(custodian, ','), '') FROM exposure_custody),
        (SELECT string_agg(max_gap_days || '/' || aged_days || '/' || partial_pct, ',') FROM exposure_settings)`).
		Scan(&proxies, &tags, &custody, &settings); err != nil {
		t.Fatal(err)
	}
	if proxies != "symbol:LEV3>symbol:IDX1:3.0" || tags != "" || custody != "" || settings != "200/35/3.0" {
		t.Errorf("tables after resync: proxies %q tags %q custody %q settings %q", proxies, tags, custody, settings)
	}
}
