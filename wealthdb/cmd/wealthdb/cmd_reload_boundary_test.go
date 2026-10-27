package main

import (
	"strings"
	"testing"
)

// The load that first stamps the cash-flow boundary says that every
// earlier cash flow report was wrong. A rebuild of a gold that carried a
// boundary is no first pass and says nothing; a rebuild of one that
// never had a boundary still is, and says so.
func TestReloadFreshCallsItselfTheFirstPassOnlyWithoutABoundary(t *testing.T) {
	t.Parallel()
	const notice = "FIRST pass to stamp the boundary"

	cfg := setupCLITest(t)
	if _, _, code := run(t, "-c", cfg, "init"); code != 0 {
		t.Fatal("init failed")
	}
	so, se, code := run(t, "-c", cfg, "reload", "-a")
	if code != 0 {
		t.Fatalf("reload -a failed: code=%d stderr=%s", code, se)
	}
	if !strings.Contains(so, notice) {
		t.Errorf("a rebuild of a gold with no boundary did not call itself the first pass:\n%s", so)
	}

	so, se, code = run(t, "-c", cfg, "reload", "-a")
	if code != 0 {
		t.Fatalf("second reload -a failed: code=%d stderr=%s", code, se)
	}
	if strings.Contains(so, notice) {
		t.Errorf("a rebuild of a gold with a boundary called itself the first pass:\n%s", so)
	}
	if strings.Contains(se, "did not carry across") {
		t.Errorf("the boundary did not carry across:\n%s", se)
	}
}
