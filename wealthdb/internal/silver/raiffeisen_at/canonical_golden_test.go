package raiffeisenat

import (
	"testing"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/silver/silvertest"
)

// TestCanonicalGolden holds every record the adapter yields over the
// shared seed fixture to testdata/canonical.jsonl, with a second account
// whose details state a credit and a debit rate.
func TestCanonicalGolden(t *testing.T) {
	path, db := newFixture(t)
	seed(t, db)
	if _, err := db.Exec(`INSERT INTO accounts VALUES (?,?,?,?,?,?,?,?)`, loadUnix, "ATkkBBBBBKKKKKKK5678",
		"Sparkonto", "", "…5678", "EUR", 200.00,
		`{"details": {"interest_credit": "0,125% ab 01.01.2026", "interest_debit": "6,5% ab 01.01.2026"}}`); err != nil {
		t.Fatal(err)
	}
	silvertest.Golden(t, openConn(t, path), "testdata/canonical.jsonl")
}
