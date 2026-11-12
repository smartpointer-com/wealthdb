package manual

import (
	"database/sql"
	"flag"
	"testing"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/silver/silvertest"
)

var update = flag.Bool("update", false, "rewrite testdata/canonical_*.jsonl")

// TestCanonicalGolden holds every record the adapter yields over each of
// the two shared seed fixtures, the book without accounts and the book
// with declared accounts, to a golden file of its own.
func TestCanonicalGolden(t *testing.T) {
	for _, c := range []struct {
		name string
		seed func(*testing.T, *sql.DB)
	}{
		{"book", seed},
		{"accounts", seedAccounts},
	} {
		t.Run(c.name, func(t *testing.T) {
			path, db := newFixtureSilver(t)
			c.seed(t, db)
			silvertest.Golden(t, openAdapter(t, path), "testdata/canonical_"+c.name+".jsonl", *update)
		})
	}
}
