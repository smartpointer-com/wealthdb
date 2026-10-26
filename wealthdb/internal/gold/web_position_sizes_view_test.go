package gold

import (
	"database/sql"
	"testing"
)

// TestWebPositionSizesNamesAShareSize pins how a position label of a
// public-equity stock finds its share's size, through the keys the
// look-through reads: the instrument's own, a holding alias (the
// account's, else one for every account), an instrument alias (a CUSIP
// and its ticker), and a security alias. A label whose sized securities
// differ has none, while an unsized one beside a sized one leaves the
// size; a key the look-through expands is no share. A private share and
// a fund have no row.
func TestWebPositionSizesNamesAShareSize(t *testing.T) {
	db, ctx := openMigrated(t)
	for _, q := range []string{
		`INSERT INTO serving_positions_history BY NAME
		 SELECT 'test-src' AS silver_source_id, acct AS account_external_id, iid AS instrument_external_id,
		        iid AS position_key, symbol, symbol || ' Example' AS name, class AS asset_class, vehicle,
		        day * 86400 AS as_of_day
		   FROM (VALUES ('A1', 'I-QDIR', 'QDIR', 'public_equity', 'stock', 1),
		                ('A1', 'I-QDIR', 'QDIR', 'public_equity', 'stock', 2),
		                ('A2', 'I-QDIR', 'QDIR', 'public_equity', 'stock', 2),
		                ('A1', 'I-QHAL', 'QHAL', 'public_equity', 'stock', 1),
		                ('A2', 'I-QHDA', 'QHDA', 'public_equity', 'stock', 1),
		                ('A1', '000000AA0', 'QCUS', 'public_equity', 'stock', 1),
		                ('A1', 'I-QSAL', 'QSAL', 'public_equity', 'stock', 1),
		                ('A1', 'I-QNOK', 'QNOK', 'public_equity', 'stock', 1),
		                ('A1', 'I-QTWO', 'QTWO', 'public_equity', 'stock', 1),
		                ('A2', 'I-QTWO-B', 'QTWO', 'public_equity', 'stock', 1),
		                ('A1', 'I-QOLD', 'QNEW', 'public_equity', 'stock', 1),
		                ('A1', 'I-QNEW', 'QNEW', 'public_equity', 'stock', 2),
		                ('A1', 'I-QXPD', 'QXPD', 'public_equity', 'stock', 1),
		                ('A1', 'I-QPRV', 'QPRV', 'private_equity', 'stock', 1),
		                ('A1', 'I-QETF', 'QETF', 'public_equity', 'etf', 1))
		        AS t(acct, iid, symbol, class, vehicle, day)`,
		`INSERT INTO instrument_keys VALUES ('test-src', 'I-QDIR', 'isin:XX0000000701'),
		     ('test-src', 'I-QHAL-REAL', 'isin:XX0000000702'), ('test-src', 'I-QSAL', 'symbol:QSAL'),
		     ('test-src', 'I-QTWO', 'isin:XX0000000704'), ('test-src', 'I-QTWO-B', 'isin:XX0000000705'),
		     ('test-src', 'I-QETF', 'isin:XX0000000706'), ('test-src', 'I-QHDA-REAL', 'isin:XX0000000707'),
		     ('test-src', 'QCUS', 'isin:XX0000000708'), ('test-src', 'I-QOLD', 'isin:XX0000000709'),
		     ('test-src', 'I-QNEW', 'isin:XX0000000710'), ('test-src', 'I-QXPD', 'isin:XX0000000711'),
		     ('test-src', 'I-QPRV', 'isin:XX0000000712')`,
		`INSERT INTO holding_aliases (silver_source_id, account_external_id, from_key, to_key)
		     VALUES ('test-src', 'A1', 'I-QHAL', 'I-QHAL-REAL'), ('test-src', '', 'I-QHDA', 'I-QHDA-REAL')`,
		`INSERT INTO instruments (silver_source_id, instrument_external_id, asset_class, cusip, symbol,
		                         first_seen_at, last_seen_at)
		     VALUES ('test-src', '000000AA0', 'public_equity', '000000AA0', 'QCUS', 0, 0),
		            ('test-src', 'QCUS', 'public_equity', NULL, 'QCUS', 0, 0)`,
		`INSERT INTO fund_attributes (silver_source_id, fund_ref, as_of) VALUES ('test-src', 'isin:XX0000000711', 0)`,
		`INSERT INTO security_aliases VALUES ('symbol:QSAL', 'isin:XX0000000703', 'lookup')`,
		`INSERT INTO securities (security_key, size) VALUES ('isin:XX0000000701', 'large'),
		     ('isin:XX0000000702', 'mid'), ('isin:XX0000000703', 'small'), ('isin:XX0000000704', 'large'),
		     ('isin:XX0000000705', 'small'), ('isin:XX0000000706', 'large'), ('isin:XX0000000707', 'mid'),
		     ('isin:XX0000000708', 'small'), ('isin:XX0000000710', 'mid'), ('isin:XX0000000711', 'large'),
		     ('isin:XX0000000712', 'small')`,
	} {
		if _, err := db.ExecContext(ctx, q); err != nil {
			t.Fatalf("seed: %v\n%s", err, q)
		}
	}

	got := map[string]string{}
	if err := scanAll(ctx, db, `SELECT symbol, size FROM web_position_sizes`, func(rows *sql.Rows) error {
		var symbol string
		var size sql.NullString
		if err := rows.Scan(&symbol, &size); err != nil {
			return err
		}
		got[symbol] = size.String
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"QDIR": "large", "QHAL": "mid", "QHDA": "mid", "QCUS": "small", "QSAL": "small",
		"QNOK": "", "QTWO": "", "QNEW": "mid", "QXPD": ""}
	if len(got) != len(want) {
		t.Errorf("rows %v, want %v", got, want)
	}
	for symbol, size := range want {
		if s, ok := got[symbol]; !ok || s != size {
			t.Errorf("%s: size %q (row %v), want %q", symbol, s, ok, size)
		}
	}
}
