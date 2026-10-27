package gold

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"os"
	"strings"
)

// WebMaterializeScope is the materialize_inputs row web-materialize
// writes. One row covers every phase: they read the same loads, the
// same config and the same engine.
const WebMaterializeScope = "web"

// MaterializeKey is the inputs one materialize ran from. A later run
// with the same key produces the same tables, so it can skip.
type MaterializeKey struct {
	LoadKey       string
	ConfigHash    string
	EngineVersion string
	// AsOf is the end of the UTC day the tables were built through.
	// The reports depend on it, so a later day is a different key.
	AsOf int64
}

// loadKeyTables are the tables a load stamps from the config and the
// ledgers, the enrichment and the account scopes a load's passes
// rewrite from the config, the verdict stores a categorize writes, the
// symbol resolutions a resolve-symbols writes, and the lot-run log a
// lots rebuild appends, on top of silver_sources. A change in one of
// them moves the load key even when no source watermark does. Every
// load runs the passes, and the enrichment pass stamps each row it
// writes with the run's time, so a load moves the key even when it
// finds nothing new: that stamp covers what else the passes rewrite in
// place, such as a position's class. A lots rebuild records a run row
// in the same write as the basis it fills in, so the run log is the
// stamp for that basis.
var loadKeyTables = []string{
	"silver_sources",
	"holding_aliases",
	"accounts",
	"exposure_proxies",
	"exposure_reference_funds",
	"exposure_size_funds",
	"exposure_declarations",
	"exposure_declaration_leaves",
	"exposure_custody",
	"exposure_settings",
	"yield_curves",
	"yield_withholding",
	"yield_rating_funds",
	"yield_conventions",
	"yield_cycles",
	"yield_settlement_cycles",
	"yield_declared_rates",
	"yield_declared_ratings",
	"yield_declared_dividends",
	"yield_declared_terms",
	"spend_txn_enrichment",
	"income_txn_enrichment",
	"spend_reimbursement_pairs",
	"cash_signed_kinds",
	"spend_account_scope",
	"income_account_scope",
	"cashflow_account_scope",
	"cashflow_wrapper_sides",
	"spend_merchant_categories",
	"income_payer_categories",
	"symbol_resolutions",
	"lot_runs",
}

// LoadKey reads the loads currently in gold: a stamp of each of
// loadKeyTables. Any part moves when a load or a reload does. A stamped
// table that is absent, such as one a later migration drops, reads as
// absent rather than failing the key, so its key differs from any key
// of a gold that has it.
func LoadKey(ctx context.Context, q querier) (string, error) {
	parts := make([]string, 0, len(loadKeyTables))
	for _, table := range loadKeyTables {
		stamp, err := tableStamp(ctx, q, table)
		if err != nil {
			return "", err
		}
		parts = append(parts, stamp)
	}
	return strings.Join(parts, ":"), nil
}

// tableStamp is a hash of every row of table and the row count, or
// "absent".
func tableStamp(ctx context.Context, q querier, table string) (string, error) {
	catalog, err := tableColumns(ctx, q, table)
	if err != nil {
		return "", fmt.Errorf("load key %s: %w", table, err)
	}
	if len(catalog) == 0 {
		return "absent", nil
	}
	cols := make([]string, len(catalog))
	for i, c := range catalog {
		cols[i] = `"` + strings.ReplaceAll(c.name, `"`, `""`) + `"`
	}
	var stamp string
	// The row count beside the hash: a pair of equal rows cancels in
	// the xor.
	err = q.QueryRowContext(ctx, `SELECT COALESCE(CAST(bit_xor(hash(`+strings.Join(cols, ", ")+`)) AS VARCHAR), '0') || '/' || count(*) FROM `+table).Scan(&stamp)
	if err != nil {
		return "", fmt.Errorf("load key %s: %w", table, err)
	}
	return stamp, nil
}

// ReadMaterializeKey is the key stored for scope. ok is false when
// none is.
func ReadMaterializeKey(ctx context.Context, q querier, scope string) (key MaterializeKey, ok bool, err error) {
	err = q.QueryRowContext(ctx, `
SELECT load_key, config_hash, engine_version, as_of FROM materialize_inputs WHERE scope = ?`, scope).
		Scan(&key.LoadKey, &key.ConfigHash, &key.EngineVersion, &key.AsOf)
	if err == sql.ErrNoRows {
		return MaterializeKey{}, false, nil
	}
	if err != nil {
		return MaterializeKey{}, false, fmt.Errorf("read materialize inputs: %w", err)
	}
	return key, true, nil
}

// WriteMaterializeKey records key as the inputs scope was built from.
func WriteMaterializeKey(ctx context.Context, db *sql.DB, scope string, key MaterializeKey) error {
	_, err := db.ExecContext(ctx, `
INSERT INTO materialize_inputs (scope, load_key, config_hash, engine_version, as_of)
VALUES (?, ?, ?, ?, ?)
ON CONFLICT (scope) DO UPDATE SET
    load_key = excluded.load_key,
    config_hash = excluded.config_hash,
    engine_version = excluded.engine_version,
    as_of = excluded.as_of`,
		scope, key.LoadKey, key.ConfigHash, key.EngineVersion, key.AsOf)
	if err != nil {
		return fmt.Errorf("write materialize inputs: %w", err)
	}
	return nil
}

// ClearMaterializeKey removes the key stored for scope, before a run
// rewrites what it covers.
func ClearMaterializeKey(ctx context.Context, db *sql.DB, scope string) error {
	if _, err := db.ExecContext(ctx, `DELETE FROM materialize_inputs WHERE scope = ?`, scope); err != nil {
		return fmt.Errorf("clear materialize inputs: %w", err)
	}
	return nil
}

// HashInputs is the sha256 of the config's bytes and of each other
// file a materialize reads, named in order. The whole config is the
// input: a key that does not affect a table still changes the hash, and
// the run materializes again. A hash of only the keys a phase reads
// would skip a run it should not. Another file that is absent hashes
// apart from an empty one; an empty name is no file.
func HashInputs(config []byte, files ...string) (string, error) {
	h := sha256.New()
	h.Write(config)
	for _, path := range files {
		if path == "" {
			continue
		}
		fmt.Fprintf(h, "\x00%s\x00", path)
		b, err := os.ReadFile(path)
		switch {
		case os.IsNotExist(err):
			h.Write([]byte("absent"))
		case err != nil:
			return "", fmt.Errorf("hash %s: %w", path, err)
		default:
			fmt.Fprintf(h, "%d\x00", len(b))
			h.Write(b)
		}
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
