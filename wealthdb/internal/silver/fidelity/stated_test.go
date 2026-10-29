package fidelity

import (
	"context"
	"testing"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/silver/silvertest"
)

// TestStatedPeriodsLoadAsTheyAre covers the stated_periods table (silver
// migration 0015): each row reaches the stream once, a NULL figure as
// nil, and the window widens to a period outside the dumps' span.
func TestStatedPeriodsLoadAsTheyAre(t *testing.T) {
	path, seed := newFixtureSilver(t)
	if _, err := seed.Exec(`
        INSERT INTO dump_runs(snapshot_at, silver_schema_version, run_dir) VALUES (5000000, 4, '/x/1');
        INSERT INTO accounts(snapshot_at, account_external_id, portfolio_external_id, nickname, payload) VALUES
            (5000000, 'ACC1', NULL, NULL, '{}');
        INSERT INTO stated_periods VALUES
            (2592000, 5097600, 'ACC1', 1000, 1250.5, 2, -100, 350.5, 12, 3, 'USD', 'doc', '{"Date":"1970-01-31"}'),
            (5184000, 7689600, 'ACC1', 1250.5, NULL, NULL, 0, NULL, NULL, NULL, 'USD', 'doc', '{}');
    `); err != nil {
		t.Fatal(err)
	}
	conn := openAdapter(t, path)
	ctx := context.Background()
	w, err := conn.ChangeWindow(ctx, -1)
	if err != nil {
		t.Fatal(err)
	}
	if w.Start > 5097600 || w.End < 7689600 {
		t.Fatalf("window [%d, %d] does not reach the stated periods", w.Start, w.End)
	}
	got := silvertest.Merged(t, conn, w).StatedPeriods
	if len(got) != 2 {
		t.Fatalf("stated periods = %d, want 2", len(got))
	}
	a, b := got[0], got[1]
	if a.AsOf != 5097600 || a.PeriodStart != 2592000 || a.AccountExternalID != "ACC1" || a.Currency != "USD" || a.SourceSHA256 != "doc" {
		t.Errorf("first period keys: %+v", a)
	}
	if a.NetAdditions == nil || a.NetAdditions.String() != "-100" || a.EndingValue.String() != "1250.5" || a.Fees.String() != "3" {
		t.Errorf("first period figures: %+v", a)
	}
	if b.EndingValue != nil || b.NetGain != nil || b.NetAdditions == nil || !b.NetAdditions.IsZero() {
		t.Errorf("a NULL figure must read nil and a stated zero zero: %+v", b)
	}
	if string(a.Payload) != `{"Date":"1970-01-31"}` {
		t.Errorf("payload = %s", a.Payload)
	}
}
