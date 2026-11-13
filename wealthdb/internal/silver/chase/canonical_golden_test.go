package chase

import (
	"testing"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/silver/silvertest"
)

// TestCanonicalGolden holds every record the adapter yields over the
// shared seed fixture to testdata/canonical.jsonl.
func TestCanonicalGolden(t *testing.T) {
	path, db := newFixture(t)
	seed(t, db)
	seedCard(t, db)
	silvertest.Golden(t, openConn(t, path), "testdata/canonical.jsonl")
}
