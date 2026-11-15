package loader_test

import (
	"bytes"
	"context"
	"log"
	"os"
	"strings"
	"testing"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/loader"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/silver"
)

// adviceKind is a kind of this test's own: no real adapter registers it.
const adviceKind = "advice-test"

// adviceNotes is what the fake connection's silver lacks.
var adviceNotes []silver.SchemaNote

type adviceConn struct{ lotsConn }

func (adviceConn) SchemaAdvice() []silver.SchemaNote { return adviceNotes }

func init() {
	silver.Register(silver.Source{Kind: adviceKind,
		Open: func(context.Context, silver.OpenSpec) (silver.Connection, error) { return adviceConn{}, nil }})
}

// A silver older than its adapter loads, and the load prints one line per
// silver that lacks a part, naming the file, the parts and both versions;
// a current silver prints nothing.
func TestLoadAdvisesTheCollectorsLoad(t *testing.T) {
	l, _ := lotsGold(t)
	var out bytes.Buffer
	log.SetOutput(&out)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	load := func() {
		t.Helper()
		if _, err := l.Load(context.Background(), loader.SourceSpec{ID: "old", Kind: adviceKind}); err != nil {
			t.Fatalf("Load: %v", err)
		}
	}
	adviceNotes = []silver.SchemaNote{{Silver: "old.db", Version: 10, Reads: 15,
		Missing: []string{"open_lots", "open_lots.cusip", "closed_lots.security_name", "stated_periods"}}}
	load()
	want := "old loader: old.db is at silver schema 10 and the adapter reads 15; open_lots, open_lots.cusip, " +
		"closed_lots.security_name and stated_periods load empty until its collector's load applies the migrations\n"
	if got := out.String(); !strings.HasSuffix(got, want) || strings.Count(got, "\n") != 1 {
		t.Errorf("log = %q, want the one line %q", got, want)
	}
	out.Reset()
	adviceNotes = nil
	load()
	if out.Len() != 0 {
		t.Errorf("a current silver logged %q", out.String())
	}
}
