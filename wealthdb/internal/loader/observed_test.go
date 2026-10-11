package loader_test

import (
	"testing"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/loader"
)

// Every load stamps when the silver last observed the source, a load that
// finds no new data too: that a run found nothing new is what a
// freshness reader needs. A watermark the silver does not know is NULL in
// source_observed.
func TestEveryLoadStampsTheObservedWatermarks(t *testing.T) {
	l, db := lotsGold(t)
	observed := func() (snaps, ledger *int64) {
		t.Helper()
		if err := db.QueryRow(`SELECT observed_snapshots_through, observed_transactions_through
		                         FROM source_observed WHERE silver_source_id = 'lots-src'`).Scan(&snaps, &ledger); err != nil {
			t.Fatal(err)
		}
		return
	}
	lotsState.changeNumber, lotsState.observed = 1, [2]int64{1000, 900}
	lotsState.window = canonical.Window{Start: 1000, End: 1000, NewChangeNumber: 1, HasChanges: true}
	lotsState.snapshots, lotsState.realized = canonical.SnapshotBatch{Accounts: accountsAt(1000, "A")}, nil
	lotsLoad(t, l, loader.SourceSpec{})
	if s, x := observed(); s == nil || *s != 1000 || x == nil || *x != 900 {
		t.Errorf("after a load: %v %v", s, x)
	}

	// The same change number: no new data, a later observation.
	lotsState.observed = [2]int64{2000, 1900}
	if res := lotsLoad(t, l, loader.SourceSpec{}); !res.AlreadyUpToDate {
		t.Fatalf("expected a no-op load: %+v", res)
	}
	if s, x := observed(); s == nil || *s != 2000 || x == nil || *x != 1900 {
		t.Errorf("after a no-op load: %v %v", s, x)
	}

	lotsState.observed = [2]int64{0, -1}
	lotsLoad(t, l, loader.SourceSpec{})
	if s, x := observed(); s != nil || x != nil {
		t.Errorf("unknown watermarks: %v %v, want NULL", s, x)
	}
}
