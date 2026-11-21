package carta

import (
	"testing"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/silver/silvertest"
)

// TestCanonicalGolden holds every record the adapter yields over the
// shared seed fixture to testdata/canonical.jsonl, the fund's partner
// metrics stating its capital account.
func TestCanonicalGolden(t *testing.T) {
	path, db := newFixtureSilver(t)
	seed(t, db)
	silvertest.Exec(t, db, `UPDATE fund_metrics SET commitment = '250000.00', called_capital = '100000.00',
    distributions = '2500.00' WHERE entity_external_id = 200`)
	silvertest.Golden(t, openAdapter(t, path), "testdata/canonical.jsonl")
}
