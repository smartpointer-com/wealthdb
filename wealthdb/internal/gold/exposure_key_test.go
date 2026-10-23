package gold

import "testing"

// TestExposureKeyBindsTheCallersColumn pins migration 0142: an
// unqualified security_key passed to exposure_key() in a WHERE clause or a
// join condition binds to the caller's row, so its alias resolves there as
// it does in a select list, and a caller's qualified column binds there
// too.
func TestExposureKeyBindsTheCallersColumn(t *testing.T) {
	db, ctx := openMigrated(t)
	if _, err := db.ExecContext(ctx, `
        INSERT INTO security_aliases (alias, security_key, origin) VALUES ('symbol:EXMP', 'isin:XX0000000001', 'lookup');
        CREATE TEMP TABLE keys AS SELECT * FROM (VALUES ('symbol:EXMP'), ('isin:XX0000000002')) v(security_key);
    `); err != nil {
		t.Fatal(err)
	}
	var where, selected int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM keys WHERE exposure_key(security_key) = 'isin:XX0000000001'`).Scan(&where); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FILTER (WHERE k = 'isin:XX0000000001') FROM (SELECT exposure_key(security_key) AS k FROM keys)`).
		Scan(&selected); err != nil {
		t.Fatal(err)
	}
	var joined, aliased int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM keys JOIN (VALUES ('isin:XX0000000001')) v(k)
	    ON exposure_key(security_key) = v.k`).Scan(&joined); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM keys a WHERE exposure_key(a.security_key) = 'isin:XX0000000001'`).Scan(&aliased); err != nil {
		t.Fatal(err)
	}
	if where != 1 || selected != 1 || joined != 1 || aliased != 1 {
		t.Errorf("the alias resolves in a WHERE clause %d time(s), a select list %d, a join %d, qualified %d; want 1 each",
			where, selected, joined, aliased)
	}
	rerunMigrationDDL(t, db, ctx, "0142_exposure_key_columns.sql")
}
