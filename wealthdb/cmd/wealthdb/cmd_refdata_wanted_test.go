package main

import (
	"encoding/json"
	"testing"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/gold"
)

// refdata-wanted reads gold read-only and prints the wanted list under
// its contract version; it takes no positional argument.
// TestUsageListsEverySubcommand keeps it out of the help listing.
func TestRefdataWantedPrintsTheWantedList(t *testing.T) {
	t.Parallel()
	cfg := setupExposureGold(t)
	so, se, code := run(t, "-r", "-c", cfg, "refdata-wanted")
	if code != 0 {
		t.Fatalf("exit=%d stderr=%s", code, se)
	}
	var w gold.Wanted
	if err := json.Unmarshal([]byte(so), &w); err != nil {
		t.Fatalf("not the wanted list: %v\n%s", err, so)
	}
	if w.Version != gold.WantedContractVersion || len(w.Funds) == 0 {
		t.Errorf("version %d with %d funds, want version %d and the held fund", w.Version, len(w.Funds),
			gold.WantedContractVersion)
	}
	if _, _, code := run(t, "-r", "-c", cfg, "refdata-wanted", "extra"); code != 2 {
		t.Errorf("a positional argument: exit=%d, want 2", code)
	}
}
