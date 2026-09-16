package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeCashflowCfg writes a config holding the given `cashflow` block
// and loads it, returning the config or the load error.
func writeCashflowCfg(t *testing.T, body string) (*Config, error) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "wealthdb.cfg")
	cfg := `{"gold_db": "/tmp/g.db", "default_currency": "USD", ` +
		`"silver_sources": [{"id": "bank", "kind": "chase", "path": "/tmp/s.db"}], ` + body + `}`
	if err := os.WriteFile(path, []byte(cfg), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return Load(path)
}

// TestCashflowBlockParses pins the shape: one block, two fields, both
// optional, and an accessor for each.
func TestCashflowBlockParses(t *testing.T) {
	c, err := writeCashflowCfg(t, `"cashflow": {
        "accounts": {"exclude": {"bank": ["ACC1", "ACC2"]}},
        "wrappers": {"529": "household", "trust_grantor": "trusts"}
    }`)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	include, exclude := c.CashflowAccountScope()
	if include != nil {
		t.Errorf("the pool has an include map: %v — every account is pooled by default", include)
	}
	if got := exclude["bank"]; len(got) != 2 || got[0] != "ACC1" || got[1] != "ACC2" {
		t.Errorf("pool exclusions = %v, want [ACC1 ACC2]", got)
	}
	if got := c.CashflowWrappers(); len(got) != 2 ||
		got["529"] != "household" || got["trust_grantor"] != "trusts" {
		t.Errorf("wrapper overrides = %v", got)
	}
}

// TestCashflowBlockIsOptional pins the absent-block default: every
// account pooled, and the engine's own boundary.
func TestCashflowBlockIsOptional(t *testing.T) {
	c, err := writeCashflowCfg(t, `"web": {"enabled": false}`)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if include, exclude := c.CashflowAccountScope(); include != nil || exclude != nil {
		t.Errorf("an absent block scopes something: %v / %v", include, exclude)
	}
	if got := c.CashflowWrappers(); got != nil {
		t.Errorf("an absent block overrides a wrapper: %v", got)
	}
}

// TestCashflowAccountsHasNoIncludeField pins the decision behind the
// one-sided block: every account is pooled by default, so an `include`
// could fence nothing, and a knob that does nothing is worse than one
// that is absent. The loader disallows unknown fields, so writing it is
// an error rather than a silent no-op.
func TestCashflowAccountsHasNoIncludeField(t *testing.T) {
	_, err := writeCashflowCfg(t, `"cashflow": {"accounts": {"include": {"bank": ["ACC1"]}}}`)
	if err == nil {
		t.Fatal("cashflow.accounts.include was accepted")
	}
	if !strings.Contains(err.Error(), "include") {
		t.Errorf("error does not name the field: %v", err)
	}
}

// TestCashflowWrapperValidation pins both halves of the vocabulary
// check, and that each error names the entry a reader has to edit.
func TestCashflowWrapperValidation(t *testing.T) {
	for _, tc := range []struct {
		name, body, wants string
	}{
		{"unknown wrapper", `"cashflow": {"wrappers": {"pillar_4": "retirement"}}`, "pillar_4"},
		{"unknown destination", `"cashflow": {"wrappers": {"hsa": "medical"}}`, "medical"},
		// `untracked` is where a crossing goes when there is no far
		// account to read a wrapper off, so no wrapper can be sent there.
		{"untracked is not a destination", `"cashflow": {"wrappers": {"hsa": "untracked"}}`, "untracked"},
		{"a side is not a destination", `"cashflow": {"wrappers": {"hsa": "vehicle"}}`, "vehicle"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := writeCashflowCfg(t, tc.body)
			if err == nil {
				t.Fatalf("%s was accepted", tc.body)
			}
			if !strings.Contains(err.Error(), tc.wants) {
				t.Errorf("error %q does not name %q", err, tc.wants)
			}
		})
	}
}

// TestCashflowWrapperDestinationsAreAllAccepted walks the whole
// vocabulary, so a destination added to canonical without a config
// story fails here rather than at the stamp.
func TestCashflowWrapperDestinationsAreAllAccepted(t *testing.T) {
	for _, dest := range []string{"household", "retirement", "education", "health", "trusts", "giving"} {
		if _, err := writeCashflowCfg(t,
			`"cashflow": {"wrappers": {"hsa": "`+dest+`"}}`); err != nil {
			t.Errorf("destination %q was refused: %v", dest, err)
		}
	}
}

// TestCashflowPoolExclusionValidation pins the checks the account scope
// shares with the two families': the source must be declared, and an id
// may not be listed twice — a repeat would raise a primary-key
// violation mid-load, after every source had already been written.
func TestCashflowPoolExclusionValidation(t *testing.T) {
	if _, err := writeCashflowCfg(t,
		`"cashflow": {"accounts": {"exclude": {"nosuch": ["ACC1"]}}}`); err == nil {
		t.Error("an undeclared source was accepted")
	}
	if _, err := writeCashflowCfg(t,
		`"cashflow": {"accounts": {"exclude": {"bank": ["ACC1", "ACC1"]}}}`); err == nil {
		t.Error("a repeated id was accepted")
	}
}
