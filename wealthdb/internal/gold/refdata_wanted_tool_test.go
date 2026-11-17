package gold

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io/fs"
	"maps"
	"math"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

// TestWantedListOfAGoldIsItsVersion4ListConverted checks a real gold,
// WEALTHDB_GOLD_VERIFY, against the lists engines printed of it beside
// it, and is skipped without the gold. It prints counts only, never a
// reference or a day.
//
//   - refdata-wanted-v4.json, a version 4 list an older engine printed:
//     the list this engine builds as of its generated_at equals it
//     converted (List). The older engine summed a constituent's share in
//     no fixed order, so a share may differ in its last bits: it is
//     compared within a relative 1e-12.
//
//   - refdata-wanted-v5.json, where present, a list this engine printed:
//     the list built as of its generated_at equals it exactly.
//
//     WEALTHDB_GOLD_VERIFY=/path/wealthdb.db wealthdb/wealthdb-test -count=1 \
//     -run TestWantedListOfAGoldIsItsVersion4ListConverted ./internal/gold/
func TestWantedListOfAGoldIsItsVersion4ListConverted(t *testing.T) {
	path := os.Getenv("WEALTHDB_GOLD_VERIFY")
	if path == "" {
		t.Skip("set WEALTHDB_GOLD_VERIFY=<gold.db>, with refdata-wanted-v4.json beside it")
	}
	var old Wanted
	readList(t, filepath.Join(filepath.Dir(path), "refdata-wanted-v4.json"), &old)
	if old.Version != wantedVersion4 {
		t.Fatalf("the old list is version %d, want %d", old.Version, wantedVersion4)
	}
	db, err := Open(path, ModeReadOnly)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	got := builtList(t, db, old.GeneratedAt)
	logWants(t, got)
	compareLists(t, "the old list converted", old.List(), got, 1e-12)

	var printed WantedList
	if !readList(t, filepath.Join(filepath.Dir(path), "refdata-wanted-v5.json"), &printed) {
		return
	}
	if printed.Version != WantedContractVersion {
		t.Fatalf("the printed list is version %d, want %d", printed.Version, WantedContractVersion)
	}
	compareLists(t, "the printed list", &printed, builtList(t, db, printed.GeneratedAt), 0)
}

// readList reads a list's JSON into v; false where a version 5 list is
// absent.
func readList(t *testing.T, path string, v any) bool {
	t.Helper()
	raw, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) && filepath.Base(path) == "refdata-wanted-v5.json" {
		return false
	}
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, v); err != nil {
		t.Fatal(err)
	}
	return true
}

// builtList is the list this engine builds of db as of generatedAt.
func builtList(t *testing.T, db *sql.DB, generatedAt string) *WantedList {
	t.Helper()
	now, err := time.Parse(time.RFC3339, generatedAt)
	if err != nil {
		t.Fatal(err)
	}
	w, err := RefdataWanted(context.Background(), db, now)
	if err != nil {
		t.Fatal(err)
	}
	return w.List()
}

// logWants logs how many funds and securities the list names, and how
// many want each fact.
func logWants(t *testing.T, l *WantedList) {
	t.Logf("funds %d, securities %d", len(l.Funds), len(l.Securities))
	counts := map[string]int{}
	for _, f := range l.Funds {
		for _, x := range f.Wants {
			counts["fund "+x.Fact]++
		}
	}
	for _, s := range l.Securities {
		for _, x := range s.Wants {
			counts["security "+x.Fact]++
		}
	}
	for _, k := range slices.Sorted(maps.Keys(counts)) {
		t.Logf("wants %s: %d", k, counts[k])
	}
}

// compareLists fails where want and got differ as printed, a map the list
// leaves out reading the same whether it is empty or nil. A share of net
// worth may differ by a relative tol.
func compareLists(t *testing.T, what string, want, got *WantedList, tol float64) {
	t.Helper()
	if len(want.Funds) != len(got.Funds) || len(want.Securities) != len(got.Securities) {
		t.Fatalf("%s: %d funds, %d securities; built: %d funds, %d securities", what,
			len(want.Funds), len(want.Securities), len(got.Funds), len(got.Securities))
	}
	same := func(a, b any) bool {
		x, errA := json.Marshal(a)
		y, errB := json.Marshal(b)
		return errA == nil && errB == nil && bytes.Equal(x, y)
	}
	funds := 0
	for i := range got.Funds {
		if !same(want.Funds[i], got.Funds[i]) {
			funds++
		}
	}
	securities, shares := 0, 0
	for i := range got.Securities {
		w, g := want.Securities[i], got.Securities[i]
		if !within(w.SharePct, g.SharePct, tol) {
			shares++
		}
		w.SharePct, g.SharePct = nil, nil
		if !same(w, g) {
			securities++
		}
	}
	t.Logf("%s: funds that differ %d, securities %d, shares beyond a relative %g %d", what, funds, securities, tol,
		shares)
	if funds+securities+shares > 0 || want.Settings != got.Settings || want.AsOf != got.AsOf {
		t.Errorf("the list built differs from %s", what)
	}
}

// within says a and b are both nil, or equal within a relative tol.
func within(a, b *float64, tol float64) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b || math.Abs(*a-*b) <= tol*math.Max(math.Abs(*a), math.Abs(*b))
}
