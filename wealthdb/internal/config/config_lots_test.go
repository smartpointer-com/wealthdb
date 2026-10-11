package config

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/lots"
)

func TestLotsBlock(t *testing.T) {
	base := func(lotsBlock string) *Config {
		t.Helper()
		c := &Config{GoldDB: "/tmp/g.db", DefaultCurrency: "USD",
			SilverSources: []SilverSource{{ID: "ct", Kind: "cointracking", Path: "/tmp/ct.db"}}}
		if lotsBlock != "" {
			c.Lots = &LotsConfig{}
			mustUnmarshal(t, lotsBlock, c.Lots)
		}
		return c
	}
	c := base(`{"method": "hifo", "missing_basis": "zero",
	            "sources": {"ct": {"method": "lifo", "mode": "shadow", "grain": "account"}},
	            "portfolios": {"ct": {"P1": {"method": "lofo"}}},
	            "accounts": {"ct": {"A1": {"method": "average"}}},
	            "basis": [{"source": "ct", "instrument": "XYZ", "per_unit": 0.25, "acquired": "2020-01-15"},
	                      {"source": "ct", "account": "A1", "instrument": "XYZ", "per_unit": 0.5}]}`)
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	lc, err := c.LotsEngine()
	if err != nil {
		t.Fatal(err)
	}
	if lc.Method != lots.HIFO || *lc.Sources["ct"].Method != lots.LIFO || *lc.Sources["ct"].Mode != lots.Shadow ||
		*lc.Sources["ct"].Grain != lots.GrainAccount || lc.Portfolios["ct"]["P1"] != lots.LOFO ||
		lc.Accounts["ct"]["A1"] != lots.Average {
		t.Errorf("parsed %+v", lc)
	}
	if c.MissingBasis() != lots.MissingZero {
		t.Errorf("missing basis %q", c.MissingBasis())
	}
	if b := lc.BasisFor("ct", "A2", "XYZ"); b == nil || b.PerUnit != 0.25 || b.AcqDay != lots.DayOf(1579046400) {
		t.Errorf("source-wide basis %+v", b)
	}
	if b := lc.BasisFor("ct", "A1", "XYZ"); b == nil || b.PerUnit != 0.5 || b.AcqDay != lots.NoDay {
		t.Errorf("account basis %+v", b)
	}
	if b := lc.BasisFor("ct", "A1", "ABC"); b != nil {
		t.Errorf("no basis for another instrument: %+v", b)
	}
	if def := base(""); def.MissingBasis() != lots.MissingIgnore {
		t.Error("an absent block reads a missing basis as ignore")
	}

	for block, want := range map[string]string{
		`{"method": "random"}`:                                                                     "unknown lot method",
		`{"missing_basis": "half"}`:                                                                "lots.missing_basis",
		`{"sources": {"nope": {"method": "fifo"}}}`:                                                "no silver_sources[].id matches",
		`{"sources": {"ct": {"mode": "maybe"}}}`:                                                   "unknown lots mode",
		`{"sources": {"ct": {"grain": "wallet"}}}`:                                                 "unknown lots grain",
		`{"accounts": {"ct": {"A1": {"method": "newest"}}}}`:                                       "unknown lot method",
		`{"portfolios": {"other": {"P": {"method": "fifo"}}}}`:                                     "no silver_sources[].id matches",
		`{"basis": [{"source": "nope", "instrument": "X", "per_unit": 1}]}`:                        "no silver_sources[].id matches source",
		`{"basis": [{"source": "ct", "per_unit": 1}]}`:                                             "instrument is required",
		`{"basis": [{"source": "ct", "instrument": "X", "per_unit": -1}]}`:                         "0 or more",
		`{"basis": [{"source": "ct", "instrument": "X", "per_unit": 1, "acquired": "yesterday"}]}`: "lots.basis[0].acquired",
		`{"basis": [{"source": "ct", "instrument": "X", "per_unit": 1}, {"source": "ct", "instrument": "X", "per_unit": 2}]}`: "a second basis",
	} {
		if err := base(block).Validate(); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v, want %q", block, err, want)
		}
	}
}

func mustUnmarshal(t *testing.T, s string, v any) {
	t.Helper()
	if err := json.Unmarshal([]byte(s), v); err != nil {
		t.Fatalf("unmarshal %s: %v", s, err)
	}
}
