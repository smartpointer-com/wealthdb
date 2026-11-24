package main

import (
	"strings"
	"testing"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/gold"
)

// The load that first stamps the cash-flow boundary says that every
// earlier cash flow report was wrong, when gold held transactions a
// report could have read. A rebuild of a gold that carried a boundary
// is no first pass and says nothing; a rebuild of one that held data
// and never had a boundary still is, and says so. A first load into an
// empty gold says nothing: no report read anything before it.
func TestReloadFreshCallsItselfTheFirstPassOnlyWithoutABoundary(t *testing.T) {
	t.Parallel()
	const notice = "FIRST pass to stamp the boundary"

	cfg := setupCLITest(t)
	freshGoldFor(t, cfg)
	so, se, code := run(t, "-c", cfg, "reload", "-a")
	if code != 0 {
		t.Fatalf("reload -a failed: code=%d stderr=%s", code, se)
	}
	if strings.Contains(so, notice) {
		t.Errorf("a rebuild of an empty gold called itself the first pass:\n%s", so)
	}

	// Gold holds data and no boundary: the state between the cashflow
	// migrations and the first load after them.
	db, err := gold.Open(goldPathFromCfg(cfg), gold.ModeReadWrite)
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`DELETE FROM cashflow_wrapper_sides`,
		`INSERT INTO transactions (silver_source_id, transaction_external_id, occurred_at,
		     account_external_id, kind, currency, net_amount, description)
		 VALUES ('schwab-test', 'T-1', 1000, 'ACC1', 'deposit', 'USD', 10, 'a deposit')`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	so, se, code = run(t, "-c", cfg, "reload", "-a")
	if code != 0 {
		t.Fatalf("reload -a failed: code=%d stderr=%s", code, se)
	}
	if !strings.Contains(so, notice) {
		t.Errorf("a rebuild of a gold with data and no boundary did not call itself the first pass:\n%s", so)
	}

	so, se, code = run(t, "-c", cfg, "reload", "-a")
	if code != 0 {
		t.Fatalf("third reload -a failed: code=%d stderr=%s", code, se)
	}
	if strings.Contains(so, notice) {
		t.Errorf("a rebuild of a gold with a boundary called itself the first pass:\n%s", so)
	}
	if strings.Contains(se, "did not carry across") {
		t.Errorf("the boundary did not carry across:\n%s", se)
	}
}
