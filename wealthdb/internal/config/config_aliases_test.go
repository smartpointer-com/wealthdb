package config

import (
	"strings"
	"testing"
)

// A holding alias loads as a gold row; an empty account means every
// account of the source.
func TestHoldingAliasesLoad(t *testing.T) {
	path := writeConfig(t, `{
        "gold_db": "/tmp/x", "default_currency": "USD",
        "silver_sources": [{"id": "bank", "kind": "ubs", "path": "/tmp/b.db"}],
        "holding_aliases": [
            {"source": "bank", "account": "ACC1", "key": "PM-LINE", "as": "ISIN0000001", "note": "the overview line"},
            {"source": "bank", "key": "OLD", "as": "NEW"}
        ]
    }`)
	c, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	rows := c.HoldingAliasRows()
	if len(rows) != 2 || rows[0].AccountExternalID != "ACC1" || rows[0].FromKey != "PM-LINE" || rows[0].ToKey != "ISIN0000001" ||
		rows[1].AccountExternalID != "" || rows[1].FromKey != "OLD" || rows[1].ToKey != "NEW" {
		t.Errorf("rows = %+v", rows)
	}
}

func TestHoldingAliasesRejects(t *testing.T) {
	for body, want := range map[string]string{
		`[{"source": "nope", "key": "A", "as": "B"}]`:                                            "no silver_sources[].id matches",
		`[{"source": "bank", "key": "", "as": "B"}]`:                                             "key and as are required",
		`[{"source": "bank", "key": "A", "as": "A"}]`:                                            "its own alias",
		`[{"source": "bank", "key": "A", "as": "B"}, {"source": "bank", "key": "A", "as": "C"}]`: "aliased twice",
		`[{"source": "bank", "key": "A", "as": "B"}, {"source": "bank", "key": "B", "as": "C"}]`: "itself an aliased key",
	} {
		path := writeConfig(t, `{
            "gold_db": "/tmp/x", "default_currency": "USD",
            "silver_sources": [{"id": "bank", "kind": "ubs", "path": "/tmp/b.db"}],
            "holding_aliases": `+body+`}`)
		if _, err := Load(path); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: err %v, want %q", body, err, want)
		}
	}
}
