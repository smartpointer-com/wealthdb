package gold

import (
	"slices"
	"strings"
	"testing"
	"time"
)

// A bond held directly that no source gives a sector reads its broker's
// description: a municipal bond is municipal, US, USD and its leaf a
// described security; a sovereign's is government in its country. A
// lookup's type wins over the description, a country a source states
// stays, and two brokers whose descriptions read otherwise leave the
// bond undescribed, while one that reads nothing does not stop the
// other. A Canadian bond is no municipal one. The wanted list still asks
// a lookup for the sector.
func TestExposureReadsABondByItsDescription(t *testing.T) {
	db, ctx := openMigrated(t)
	day := time.Date(2026, 6, 30, 0, 0, 0, 0, time.UTC).Unix()
	for _, q := range []string{
		`INSERT INTO accounts (silver_source_id, account_external_id, account_kind, display_name, base_currency,
		 first_seen_at, last_seen_at) VALUES ('test-src', 'A', 'brokerage', 'Brokerage', 'USD', 0, 0),
		 ('other-src', 'B', 'brokerage', 'Other', 'USD', 0, 0)`,
		`INSERT INTO instruments (silver_source_id, instrument_external_id, asset_class, vehicle, isin, cusip, name,
		 currency, first_seen_at, last_seen_at) VALUES
		 ('test-src', 'QMUNI', 'fixed_income', 'bond', NULL, '00000000C', 'QLAND CNTY GO BDS 03.33000% 07/01/2041', 'USD', 0, 0),
		 ('test-src', 'QSOV', 'fixed_income', 'bond', 'XX0000000051', NULL, 'FINLAND 3.33% 15.09.2041', 'EUR', 0, 0),
		 ('test-src', 'QTYPE', 'fixed_income', 'bond', 'US00000000D5', NULL, 'QLAND AUTH REV BDS 3.17% 2043', 'USD', 0, 0),
		 ('test-src', 'QMIX', 'fixed_income', 'bond', 'US00000000E3', NULL, 'QLAND AUTH REV BDS 3.17% 2044', 'USD', 0, 0),
		 ('other-src', 'QMIX', 'fixed_income', 'bond', 'US00000000E3', NULL, 'QORVANT INC 3.21% 2044', 'USD', 0, 0),
		 ('test-src', 'QHQ', 'fixed_income', 'bond', 'US00000000F1', NULL, 'QLAND CNTY GO BDS 3.33% 2042', 'USD', 0, 0),
		 ('test-src', 'QTWO', 'fixed_income', 'bond', 'US00000000G9', NULL, 'QLAND 3.33% 2043', 'USD', 0, 0),
		 ('other-src', 'QTWO', 'fixed_income', 'bond', 'US00000000G9', NULL, 'QLAND AUTH REV BDS 3.33% 2043', 'USD', 0, 0),
		 ('test-src', 'QCAN', 'fixed_income', 'bond', 'CA00000000H7', '00000000H', 'QLAND HSG TR PUB WTR 3.33% 2041', 'CAD', 0, 0)`,
		`INSERT INTO positions (silver_source_id, snapshot_at, account_external_id, position_key, instrument_external_id,
		 asset_class, vehicle, currency, market_value) VALUES
		 ('test-src', ?, 'A', 'QMUNI', 'QMUNI', 'fixed_income', 'bond', 'USD', 1),
		 ('test-src', ?, 'A', 'QSOV', 'QSOV', 'fixed_income', 'bond', 'EUR', 1),
		 ('test-src', ?, 'A', 'QTYPE', 'QTYPE', 'fixed_income', 'bond', 'USD', 1),
		 ('test-src', ?, 'A', 'QMIX', 'QMIX', 'fixed_income', 'bond', 'USD', 1),
		 ('other-src', ?, 'B', 'QMIX', 'QMIX', 'fixed_income', 'bond', 'USD', 1),
		 ('test-src', ?, 'A', 'QHQ', 'QHQ', 'fixed_income', 'bond', 'USD', 1),
		 ('test-src', ?, 'A', 'QTWO', 'QTWO', 'fixed_income', 'bond', 'USD', 1),
		 ('test-src', ?, 'A', 'QCAN', 'QCAN', 'fixed_income', 'bond', 'CAD', 1)`,
		`INSERT INTO security_facts (silver_source_id, security_key, field, as_of, provider, value)
		 VALUES ('refdata', 'isin:US00000000F1', 'hq_country', ?, 'gleif', 'CA')`,
		`INSERT INTO security_identities (silver_source_id, security_key, as_of, provider, security_type, payload)
		 VALUES ('refdata', 'isin:US00000000D5', ?, 'openfigi', 'Corp', '{"found": true}')`,
	} {
		if _, err := db.ExecContext(ctx, q, slices.Repeat([]any{day}, strings.Count(q, "?"))...); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	if _, err := RebuildExposure(ctx, db); err != nil {
		t.Fatal(err)
	}
	var muni string
	if err := db.QueryRowContext(ctx, `SELECT COALESCE(sa.security_key, ik.security_key) FROM instrument_keys ik
	  LEFT JOIN security_aliases sa ON sa.alias = ik.security_key
	  WHERE ik.instrument_external_id = 'QMUNI'`).Scan(&muni); err != nil {
		t.Fatal(err)
	}
	for key, want := range map[string]string{
		muni:                "municipal description US description USD",
		"isin:XX0000000051": "government description FI description -",
		"isin:US00000000D5": "corporate type:openfigi - - -",
		"isin:US00000000E3": "- - - - -",
		"isin:US00000000F1": "municipal description CA fact:gleif USD",
		"isin:US00000000G9": "municipal description US description USD",
		"isin:CA00000000H7": "- - - - -",
	} {
		var got string
		if err := db.QueryRowContext(ctx, `SELECT concat_ws(' ', COALESCE(sector, '-'), COALESCE(sector_origin, '-'),
		  COALESCE(risk_country, '-'), COALESCE(risk_country_origin, '-'), COALESCE(currency, '-'))
		  FROM securities WHERE security_key = ?`, key).Scan(&got); err != nil {
			t.Fatalf("%s: %v", key, err)
		}
		if got != want {
			t.Errorf("%s = %s, want %s", key, got, want)
		}
	}
	var how, flags string
	if err := db.QueryRowContext(ctx, `SELECT how, array_to_string(flags, ',') FROM exposure_leaves(?, 'USD')
	  WHERE top_key = ?`, day+86400, muni).Scan(&how, &flags); err != nil {
		t.Fatal(err)
	}
	if how != "security" || flags != "" {
		t.Errorf("the municipal bond's leaf = %s %q, want a described security with no flag", how, flags)
	}
	w, err := RefdataWanted(ctx, db, time.Unix(day+86400, 0))
	if err != nil {
		t.Fatal(err)
	}
	for _, sec := range w.Securities {
		if sec.Key == muni && !slices.Contains(sec.Missing, "sector") {
			t.Errorf("the municipal bond's missing = %v, want the sector still asked of a lookup", sec.Missing)
		}
	}
}
