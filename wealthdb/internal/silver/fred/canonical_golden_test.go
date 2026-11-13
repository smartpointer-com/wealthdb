package fred

import (
	"testing"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/silver/silvertest"
)

// TestCanonicalGolden holds every record the adapter yields over the
// shared seed fixture to testdata/canonical.jsonl.
func TestCanonicalGolden(t *testing.T) {
	path, db := newFixtureSilver(t)
	seed(t, db)
	seedRates(t, db)
	silvertest.Golden(t, openAdapter(t, path), "testdata/canonical.jsonl")
}
