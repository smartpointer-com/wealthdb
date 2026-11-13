package gold

import (
	"slices"
	"strings"
	"testing"
)

// TestSourceTablesNameEachTableOnce: a table listed twice is deleted
// twice by a reset and twice by a windowed delete.
func TestSourceTablesNameEachTableOnce(t *testing.T) {
	t.Parallel()
	seen := map[string]bool{}
	for _, st := range SourceTables {
		if seen[st.Name] {
			t.Errorf("%s is listed twice", st.Name)
		}
		seen[st.Name] = true
		if st.RefKind != "" && st.TimeCol == "" {
			t.Errorf("%s has a record kind and no time column to window it by", st.Name)
		}
	}
}

// TestSourceTablesKeepTheirDeleteOrder: the sweep of an excluded
// portfolio finds its facts through accounts, so accounts comes after
// every other account table. A reset drops portfolios after the
// accounts that name them, and the registration last.
func TestSourceTablesKeepTheirDeleteOrder(t *testing.T) {
	t.Parallel()
	at := map[string]int{}
	lastAccount := ""
	for i, st := range SourceTables {
		at[st.Name] = i
		if st.Account {
			lastAccount = st.Name
		}
	}
	if lastAccount != "accounts" {
		t.Errorf("the last account table is %s, want accounts", lastAccount)
	}
	if at["portfolios"] < at["accounts"] {
		t.Error("portfolios comes before accounts")
	}
	if last := SourceTables[len(SourceTables)-1].Name; last != "silver_sources" {
		t.Errorf("the last table is %s, want silver_sources", last)
	}
}

// survivesReset are the tables with a silver_source_id that a reset
// keeps, each with its reason: the load stamps it from the config
// again, a pass that is not the source's own rebuilds it, or it is a
// materialized read of the rest.
var survivesReset = map[string]string{
	"spend_account_scope":        "config",
	"income_account_scope":       "config",
	"cashflow_account_scope":     "config",
	"holding_aliases":            "config",
	"exposure_custody":           "config",
	"exposure_declarations":      "config",
	"yield_curves":               "config",
	"yield_declared_dividends":   "config",
	"yield_declared_rates":       "config",
	"yield_declared_ratings":     "config",
	"yield_declared_terms":       "config",
	"yield_withholding_accounts": "config",
	"instrument_keys":            "exposure",
	"exposure_situs":             "exposure",
	"instrument_terms":           "yield",
	"account_rates":              "yield",
	"lot_runs":                   "lots",
}

// survivesResetByPrefix are the families of materialized tables a
// reset keeps: the next load rebuilds them from the rest.
var survivesResetByPrefix = []string{"report_", "serving_"}

// TestSourceTablesCoverTheSchema holds the registry to the head schema:
// every table keyed by silver_source_id is a source table or a named
// survivor, so a new table cannot slip past a reset, a windowed delete
// or the exclusion sweep. Each source table has the columns its entry
// names.
func TestSourceTablesCoverTheSchema(t *testing.T) {
	t.Parallel()
	db, _ := openMigrated(t)
	cols := map[string]map[string]bool{}
	for _, tc := range scanStrings(t, db, `
        SELECT c.table_name || '.' || c.column_name
          FROM duckdb_columns() c
          JOIN duckdb_tables() t USING (database_name, schema_name, table_name)`) {
		table, col, _ := strings.Cut(tc, ".")
		if cols[table] == nil {
			cols[table] = map[string]bool{}
		}
		cols[table][col] = true
	}
	registered := map[string]bool{}
	for _, st := range SourceTables {
		registered[st.Name] = true
		have := cols[st.Name]
		if !have["silver_source_id"] {
			t.Errorf("%s has no silver_source_id", st.Name)
		}
		if st.TimeCol != "" && !have[st.TimeCol] {
			t.Errorf("%s has no time column %s", st.Name, st.TimeCol)
		}
		if st.Account && !have["account_external_id"] {
			t.Errorf("%s is an account table without account_external_id", st.Name)
		}
		if _, ok := survivesReset[st.Name]; ok {
			t.Errorf("%s is a source table and a survivor", st.Name)
		}
	}
	for table, have := range cols {
		if !have["silver_source_id"] || registered[table] {
			continue
		}
		if _, ok := survivesReset[table]; ok {
			continue
		}
		if slices.ContainsFunc(survivesResetByPrefix, func(p string) bool { return strings.HasPrefix(table, p) }) {
			continue
		}
		t.Errorf("%s is keyed by silver_source_id, and neither a source table nor a survivor", table)
	}
	for table := range survivesReset {
		if !cols[table]["silver_source_id"] {
			t.Errorf("survivor %s is not keyed by silver_source_id", table)
		}
	}
}

// TestLoadKeyStampsWhatNoWatermarkCovers: a source's facts, references
// and dimensions move with its watermark in silver_sources. A model's
// store, an overlay a pass rewrites, and the lot ledger, whose run log
// is its stamp, do not, so the load key stamps each of them.
func TestLoadKeyStampsWhatNoWatermarkCovers(t *testing.T) {
	t.Parallel()
	want := []string{"silver_sources", "lot_runs"}
	want = append(want, sourceTableNames(ClassStore)...)
	want = append(want, sourceTableNames(ClassOverlay)...)
	for _, table := range want {
		if !slices.Contains(loadKeyTables, table) {
			t.Errorf("%s is not in loadKeyTables", table)
		}
	}
}
