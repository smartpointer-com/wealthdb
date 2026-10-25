package gold

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMaterializeInputsRoundTrip(t *testing.T) {
	db, err := Open(":memory:", ModeReadWrite)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	ctx := context.Background()

	key, err := LoadKey(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	if key == "" || !strings.HasPrefix(key, "0/0:") {
		t.Fatalf("empty gold load key = %q", key)
	}
	got, ok, err := ReadMaterializeKey(ctx, db, WebMaterializeScope)
	if err != nil || ok {
		t.Fatalf("missing row: ok=%v err=%v key=%+v", ok, err, got)
	}
	want := MaterializeKey{LoadKey: key, ConfigHash: "abc", EngineVersion: "test", AsOf: 100}
	if err := WriteMaterializeKey(ctx, db, WebMaterializeScope, want); err != nil {
		t.Fatal(err)
	}
	got, ok, err = ReadMaterializeKey(ctx, db, WebMaterializeScope)
	if err != nil || !ok || got != want {
		t.Fatalf("stored %+v ok=%v err=%v, want %+v", got, ok, err, want)
	}
	want.EngineVersion = "next"
	if err := WriteMaterializeKey(ctx, db, WebMaterializeScope, want); err != nil {
		t.Fatal(err)
	}
	got, ok, err = ReadMaterializeKey(ctx, db, WebMaterializeScope)
	if err != nil || !ok || got != want {
		t.Fatalf("replaced %+v ok=%v err=%v, want %+v", got, ok, err, want)
	}
}

func TestMigration0155DDLIsRerunnable(t *testing.T) {
	db, err := Open(":memory:", ModeReadWrite)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	ctx := context.Background()
	want := MaterializeKey{LoadKey: "0/0", ConfigHash: "abc", EngineVersion: "test", AsOf: 100}
	if err := WriteMaterializeKey(ctx, db, WebMaterializeScope, want); err != nil {
		t.Fatal(err)
	}
	rerunMigrationDDL(t, db, ctx, "0155_materialize_inputs.sql")
	got, ok, err := ReadMaterializeKey(ctx, db, WebMaterializeScope)
	if err != nil || !ok || got != want {
		t.Fatalf("after a replay %+v ok=%v err=%v, want %+v", got, ok, err, want)
	}
}

func TestLoadKeyMovesWithEachStampedTable(t *testing.T) {
	db, err := Open(":memory:", ModeReadWrite)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	ctx := context.Background()
	if _, err := db.ExecContext(ctx, `
INSERT INTO silver_sources (silver_source_id, silver_kind, silver_path, high_watermark, first_loaded_at, last_loaded_at)
VALUES ('a', 'manual', '/tmp/a.db', 5, 0, 10),
       ('b', 'manual', '/tmp/b.db', 7, 0, 10)`); err != nil {
		t.Fatal(err)
	}
	first, err := LoadKey(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE silver_sources SET observed_snapshots_through = 20 WHERE silver_source_id = 'a'`); err != nil {
		t.Fatal(err)
	}
	second, err := LoadKey(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	if first == second {
		t.Fatal("an observation did not move the load key")
	}
	if _, err := db.ExecContext(ctx, `
INSERT INTO spend_merchant_categories (merchant_signature, merchant_name, spend_detailed, signature_version, assigned_at, model_name)
VALUES ('CORNER', 'Corner', 'groceries', 1, 1, 'm')`); err != nil {
		t.Fatal(err)
	}
	third, err := LoadKey(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	if third == second {
		t.Fatal("a merchant verdict did not move the load key")
	}
	if _, err := db.ExecContext(ctx, `
INSERT INTO income_payer_categories (payer_signature, payer_name, income_detailed, signature_version, assigned_at, model_name)
VALUES ('ACME', 'Acme', 'salary', 1, 1, 'm')`); err != nil {
		t.Fatal(err)
	}
	fourth, err := LoadKey(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	if fourth == third {
		t.Fatal("a payer verdict did not move the load key")
	}
	if _, err := db.ExecContext(ctx, `
INSERT INTO lot_runs (build_id, silver_source_id, built_at, mode, grain, methods, fees, events, lots, disposals,
    realized_lots, positions_filled, seeds, implied_disposals, blips, anchors, anchors_kept, seeds_resolved,
    fee_unvalued, keys_skipped, dated_by_settlement, input_fingerprint)
VALUES (1, 'a', 1, 'rebuild', 'position', 'fifo', 'excluded', 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, false, 'fp')`); err != nil {
		t.Fatal(err)
	}
	fifth, err := LoadKey(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	if fifth == fourth {
		t.Fatal("a lot rebuild did not move the load key")
	}
	if _, err := db.ExecContext(ctx, `
INSERT INTO symbol_resolutions (silver_source_id, lookup_kind, lookup_value, symbol, resolved_at, model_name)
VALUES ('a', 'name', 'Example Growth Fund', 'EGF', 1, 'm')`); err != nil {
		t.Fatal(err)
	}
	sixth, err := LoadKey(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	if sixth == fifth {
		t.Fatal("a symbol resolution did not move the load key")
	}
}

// A gold that lacks a stamped table, such as one a later migration
// drops, still has a load key, and that key is not the key of the same
// gold with the table.
func TestLoadKeyReadsAnAbsentTable(t *testing.T) {
	db, err := Open(":memory:", ModeReadWrite)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	ctx := context.Background()
	with, err := LoadKey(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `DROP TABLE yield_settlement_cycles`); err != nil {
		t.Fatal(err)
	}
	without, err := LoadKey(ctx, db)
	if err != nil {
		t.Fatalf("load key without a stamped table: %v", err)
	}
	if without == with || !strings.Contains(without, "absent") {
		t.Fatalf("load key without the table = %q, with it = %q", without, with)
	}
}

func TestHashInputsCoversEveryFile(t *testing.T) {
	dir := t.TempDir()
	ledger := filepath.Join(dir, "transfer-overrides.csv")
	write := func(path, body string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	var config []byte
	hash := func(files ...string) string {
		t.Helper()
		h, err := HashInputs(config, files...)
		if err != nil {
			t.Fatal(err)
		}
		return h
	}
	config = []byte(`{}`)
	alone := hash()
	if hash() != alone {
		t.Fatal("one config hashed twice gave two hashes")
	}
	if hash("") != alone {
		t.Fatal("an empty file name changed the hash")
	}
	absent := hash(ledger)
	write(ledger, "")
	empty := hash(ledger)
	write(ledger, "verb\n")
	edited := hash(ledger)
	if absent == alone || empty == absent || edited == empty {
		t.Fatalf("hashes alone %s absent %s empty %s edited %s", alone, absent, empty, edited)
	}
	config = []byte(`{"x": 1}`)
	if hash(ledger) == edited {
		t.Fatal("a config edit kept the hash")
	}
}

func TestMigrationsPending(t *testing.T) {
	db, err := Open(":memory:", ModeReadWrite)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	ctx := context.Background()
	if pending, err := MigrationsPending(ctx, db); err != nil || pending {
		t.Fatalf("a migrated gold: pending=%v err=%v", pending, err)
	}
	if _, err := db.ExecContext(ctx, `DELETE FROM schema_meta WHERE gold_schema_version = (SELECT max(gold_schema_version) FROM schema_meta)`); err != nil {
		t.Fatal(err)
	}
	if pending, err := MigrationsPending(ctx, db); err != nil || !pending {
		t.Fatalf("a gold one migration behind: pending=%v err=%v", pending, err)
	}
}

// Swapping two sources' FX priority, an account scope, and a pair of
// equal rows (whose hashes cancel in the xor) each move the key.
func TestLoadKeySeesASwapAScopeAndAPair(t *testing.T) {
	db, err := Open(":memory:", ModeReadWrite)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	ctx := context.Background()
	key := func() string {
		t.Helper()
		k, err := LoadKey(ctx, db)
		if err != nil {
			t.Fatal(err)
		}
		return k
	}
	exec := func(stmt string) {
		t.Helper()
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			t.Fatal(err)
		}
	}
	exec(`INSERT INTO silver_sources (silver_source_id, silver_kind, silver_path, high_watermark, first_loaded_at, last_loaded_at, fx_priority)
VALUES ('a', 'manual', '/tmp/a.db', 1, 0, 10, 1), ('b', 'manual', '/tmp/b.db', 1, 0, 10, 2)`)
	before := key()
	exec(`UPDATE silver_sources SET fx_priority = 3 - fx_priority`)
	swapped := key()
	if swapped == before {
		t.Fatal("swapping two sources' fx priority kept the load key")
	}
	exec(`INSERT INTO spend_account_scope VALUES ('a', 'ACCT-1', 'exclude')`)
	if key() == swapped {
		t.Fatal("a rewritten account scope kept the load key")
	}
	exec(`CREATE TABLE pair_probe (x INTEGER)`)
	empty, err := tableStamp(ctx, db, "pair_probe")
	if err != nil {
		t.Fatal(err)
	}
	exec(`INSERT INTO pair_probe VALUES (7), (7)`)
	pair, err := tableStamp(ctx, db, "pair_probe")
	if err != nil {
		t.Fatal(err)
	}
	if pair == empty {
		t.Fatalf("a pair of equal rows kept the stamp %q", pair)
	}
}
