package silver

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
)

// TestJoinText pins the transaction-text composition rule the adapters
// share: trim each part, drop the empty ones, join the rest with "; ",
// and yield "" — never a stray separator — when nothing is left.
func TestJoinText(t *testing.T) {
	for _, c := range []struct {
		name  string
		parts []string
		want  string
	}{
		{"no parts", nil, ""},
		{"every part blank", []string{"", "   ", "\t"}, ""},
		{"one part is trimmed", []string{"  PAYMENT ORDER  "}, "PAYMENT ORDER"},
		{"parts are joined", []string{"PAYMENT ORDER", "EXAMPLE SHOP"},
			"PAYMENT ORDER; EXAMPLE SHOP"},
		{"blank parts drop out", []string{"", "PAYMENT ORDER", "  ", "EXAMPLE SHOP"},
			"PAYMENT ORDER; EXAMPLE SHOP"},
		{"an already-separated part is not re-split", []string{"A; B", "C"}, "A; B; C"},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := JoinText(c.parts...); got != c.want {
				t.Errorf("JoinText(%q) = %q, want %q", c.parts, got, c.want)
			}
		})
	}
}

// TestPayloadWith pins the payload-merge rule the card adapters share: the
// collector's own JSON survives, the adapter's annotations are laid over it,
// and a payload that is not a JSON object never costs the annotations.
func TestPayloadWith(t *testing.T) {
	for _, c := range []struct {
		name    string
		payload string
		extra   map[string]any
		want    string
	}{
		{"no annotations passes the payload through untouched",
			`{"a":1}`, nil, `{"a":1}`},
		{"no annotations does not even reformat",
			`not json at all`, nil, `not json at all`},
		{"annotations are merged in",
			`{"a":1}`, map[string]any{"b": "x"}, `{"a":1,"b":"x"}`},
		{"an annotation overrides the collector's key",
			`{"a":1}`, map[string]any{"a": 2}, `{"a":2}`},
		{"an undecodable payload yields the annotations alone",
			`not json at all`, map[string]any{"b": "x"}, `{"b":"x"}`},
		{"a JSON non-object yields the annotations alone",
			`[1,2]`, map[string]any{"b": "x"}, `{"b":"x"}`},
		{"a null payload yields the annotations alone",
			`null`, map[string]any{"b": "x"}, `{"b":"x"}`},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := string(PayloadWith(c.payload, c.extra)); got != c.want {
				t.Errorf("PayloadWith(%q, %v) = %s, want %s",
					c.payload, c.extra, got, c.want)
			}
		})
	}
}

// TestHasTables pins the probe the adapters share for tables a newer
// silver migration adds: true only when every named table exists, and a
// view or an index of that name does not count.
func TestHasTables(t *testing.T) {
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "silver.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`
        CREATE TABLE a (x INTEGER);
        CREATE TABLE b (x INTEGER);
        CREATE VIEW v AS SELECT x FROM a;
        CREATE INDEX i ON a (x);`); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		names []string
		want  bool
	}{
		{[]string{"a"}, true},
		{[]string{"a", "b"}, true},
		{[]string{"a", "c"}, false},
		{[]string{"c"}, false},
		{[]string{"v"}, false},
		{[]string{"i"}, false},
		{nil, true},
	} {
		got, err := HasTables(context.Background(), db, c.names...)
		if err != nil {
			t.Fatalf("HasTables(%q): %v", c.names, err)
		}
		if got != c.want {
			t.Errorf("HasTables(%q) = %v, want %v", c.names, got, c.want)
		}
	}
}

func TestLotHelpers(t *testing.T) {
	if d := ISODate(" 2024-03-05 "); d == nil || d.Format(time.DateOnly) != "2024-03-05" {
		t.Errorf("ISODate = %v", d)
	}
	for _, s := range []string{"", "Various", "03/05/2024"} {
		if ISODate(s) != nil {
			t.Errorf("ISODate(%q) parsed", s)
		}
	}
	neg := canonical.NewDecimalFromInt(-7)
	if a := AbsPtr(&neg); a == nil || a.String() != "7" || neg.String() != "-7" {
		t.Errorf("AbsPtr = %v (input %v)", a, neg)
	}
	if AbsPtr(nil) != nil {
		t.Error("AbsPtr(nil) not nil")
	}
	d := time.Date(2023, 12, 29, 0, 0, 0, 0, time.UTC)
	if TaxYearOf(nil, &d) != 2023 || TaxYearOf() != 0 {
		t.Error("TaxYearOf")
	}
}

func TestWithAccrued(t *testing.T) {
	clean, accrued := canonical.NewDecimalFromInt(1000), canonical.NewDecimalFromFloat(12.5)
	if v := WithAccrued(&clean, &accrued); v == nil || v.String() != "1012.5" || clean.String() != "1000" {
		t.Errorf("WithAccrued = %v (clean %v)", v, clean)
	}
	if v := WithAccrued(&clean, nil); v != &clean {
		t.Error("no accrued figure changed the value")
	}
	if WithAccrued(nil, &accrued) != nil {
		t.Error("a nil value gained one")
	}
}
