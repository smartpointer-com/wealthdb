package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// TestEveryKeyFixtureSetsEveryKey holds testdata/every_key.cfg, the
// config other packages edit in their tests, to loading and to setting
// every member a config may hold.
func TestEveryKeyFixtureSetsEveryKey(t *testing.T) {
	path := filepath.Join("testdata", "every_key.cfg")
	if _, err := Load(path); err != nil {
		t.Fatalf("Load: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var members map[string]json.RawMessage
	if err := json.Unmarshal(data, &members); err != nil {
		t.Fatal(err)
	}
	for _, k := range TopLevelKeys() {
		if _, ok := members[k]; !ok {
			t.Errorf("every_key.cfg does not set %q", k)
		}
	}
	if len(members) != len(TopLevelKeys()) {
		t.Errorf("every_key.cfg sets %d members, the config knows %d", len(members), len(TopLevelKeys()))
	}
}

func TestTopLevelKeys(t *testing.T) {
	keys := TopLevelKeys()
	for _, want := range []string{"gold_db", "default_currency", "silver_sources", "web", "mcp", "yield"} {
		if !slices.Contains(keys, want) {
			t.Errorf("TopLevelKeys lacks %q: %v", want, keys)
		}
	}
	if keys[0] != "gold_db" {
		t.Errorf("TopLevelKeys starts %q, want the struct's order", keys[0])
	}
}

func TestInspect(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	for _, tc := range []struct {
		name, body string
		want       State
		errHas     string
	}{
		{"empty", "", StateAbsent, ""},
		{"blank", " \n\t\n", StateAbsent, ""},
		{"no currency", `{"gold_db": "/tmp/g.db"}`, StateUnfinished, ""},
		{"no gold", `{"default_currency": "USD", "silver_sources": []}`, StateUnfinished, ""},
		{"object only", `{}`, StateUnfinished, ""},
		{"valid", `{"gold_db": "/tmp/g.db", "default_currency": "USD"}`, StateValid, ""},
		{"bad json", `{"gold_db": `, StateRefused, "parse"},
		{"unknown member", `{"gold_db": "/tmp/g.db", "colour": 1}`, StateRefused, "unknown field"},
		{"bad currency", `{"gold_db": "/tmp/g.db", "default_currency": "usd"}`, StateRefused, "ISO 4217"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st, cfg, err := Inspect(write(tc.name+".cfg", tc.body))
			if st != tc.want {
				t.Fatalf("state = %d, want %d (err %v)", st, tc.want, err)
			}
			if (st == StateValid) != (cfg != nil) {
				t.Errorf("config = %v for state %d", cfg, st)
			}
			if tc.errHas == "" && err != nil || tc.errHas != "" && (err == nil || !strings.Contains(err.Error(), tc.errHas)) {
				t.Errorf("err = %v, want one naming %q", err, tc.errHas)
			}
		})
	}
	if st, _, err := Inspect(filepath.Join(dir, "missing.cfg")); st != StateAbsent || err != nil {
		t.Errorf("missing file: state %d, err %v; want absent", st, err)
	}
}
