package angellist

import (
	"bytes"
	"context"
	"database/sql"
	"log"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
)

// day is a calendar date as unix seconds at UTC midnight.
func day(t *testing.T, s string) int64 {
	t.Helper()
	tm, err := time.Parse("2006-01-02", s)
	if err != nil {
		t.Fatalf("day(%q): %v", s, err)
	}
	return tm.Unix()
}

// seedInKind builds a book of three SPV stakes, each paired to its K-1
// fund through offerings.fund_name. Every figure is invented.
//
//   - s1: paid in 10,000. Its tax-year 2021 K-1 states 4,000 of property
//     distributed in kind; its 2020 K-1 states none.
//   - s2: paid in 5,000. Its K-1 states a cash distribution only.
//   - s3: paid in 2,000. Its K-1 states 3,000 of property distributed,
//     more than was paid in.
//
// Event dates: 2020-03-01, 2021-06-30, 2021-12-31 (the K-1 period end)
// and 2022-06-30.
func seedInKind(t *testing.T, db *sql.DB) {
	t.Helper()
	if _, err := db.Exec(`
        INSERT INTO dump_runs(snapshot_at, invest_account_slug) VALUES (1700000000, 'acct');
        INSERT INTO offerings(position_external_id, kind, company_name, fund_name) VALUES
            ('s1', 'spv', 'Example Co One',   'Example SPV One, LP'),
            ('s2', 'spv', 'Example Co Two',   'Example SPV Two, LP'),
            ('s3', 'spv', 'Example Co Three', 'Example SPV Three, LP');
        INSERT INTO position_snapshots(position_external_id, as_of_date, event_type,
            is_open, currency, market_value_minor, valuation_basis, contributed_minor,
            snapshot_at) VALUES
            ('s1', strftime('%s', '2020-03-01'), 'investment', 1, 'USD', 1000000, 'cost',      1000000, 1700000000),
            ('s1', strftime('%s', '2021-12-31'), 'statement',  1, 'USD',  700000, 'tax_basis', 1000000, 1700000000),
            ('s1', strftime('%s', '2022-06-30'), 'valuation',  1, 'USD',  900000, 'fmv',       1000000, 1700000000),
            ('s2', strftime('%s', '2020-03-01'), 'investment', 1, 'USD',  500000, 'cost',       500000, 1700000000),
            ('s2', strftime('%s', '2022-06-30'), 'valuation',  1, 'USD',  600000, 'fmv',        500000, 1700000000),
            ('s3', strftime('%s', '2021-06-30'), 'investment', 1, 'USD',  200000, 'cost',       200000, 1700000000),
            ('s3', strftime('%s', '2022-06-30'), 'valuation',  1, 'USD',  100000, 'fmv',        200000, 1700000000);
        INSERT INTO k1_capital_accounts(tax_year, fund_name, cash_distributions_minor,
            property_distributions_minor) VALUES
            (2020, 'Example SPV One, LP',   NULL,   NULL),
            (2021, 'Example SPV One, LP',   NULL,   400000),
            (2021, 'Example SPV Two, LP',   100000, NULL),
            (2021, 'Example SPV Three, LP', NULL,   300000);
    `); err != nil {
		t.Fatal(err)
	}
}

// inKindPositions loads the fixture and returns its positions by date
// and position key.
func inKindPositions(t *testing.T, path string) map[int64]map[string]canonical.PositionChange {
	t.Helper()
	conn := openAdapter(t, path)
	ctx := context.Background()
	w, err := conn.ChangeWindow(ctx, -1)
	if err != nil {
		t.Fatal(err)
	}
	stream, err := conn.Snapshots(ctx, w)
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	out := map[int64]map[string]canonical.PositionChange{}
	for {
		b, more, err := stream.Next(ctx)
		if err != nil {
			t.Fatal(err)
		}
		for _, p := range b.Positions {
			if out[p.SnapshotAt] == nil {
				out[p.SnapshotAt] = map[string]canonical.PositionChange{}
			}
			out[p.SnapshotAt][p.PositionKey] = p
		}
		if !more {
			break
		}
	}
	return out
}

// A K-1's property distribution moves basis out with the asset from the
// K-1's period end on. Before it, and on a K-1 that states only cash
// paid back, the book value is the capital paid in as the portal states
// it. A distribution larger than the capital paid in holds the book
// value at zero.
func TestAPropertyDistributionReducesTheBookValueFromTheK1PeriodEnd(t *testing.T) {
	path, db := newFixtureSilver(t)
	seedInKind(t, db)
	byDay := inKindPositions(t, path)

	stated := paidInBasis
	derived := canonical.Basis{
		Origin: canonical.BasisDerived, Method: canonical.BasisMethodPaidIn, Fees: canonical.BasisFeesIncluded,
	}
	for _, c := range []struct {
		day, pos, book string
		basis          canonical.Basis
	}{
		{"2020-03-01", "s1", "10000.00", stated},
		{"2021-06-30", "s1", "10000.00", stated},
		{"2021-12-31", "s1", "6000.00", derived},
		{"2022-06-30", "s1", "6000.00", derived},
		{"2022-06-30", "s2", "5000.00", stated},
		{"2021-06-30", "s3", "2000.00", stated},
		{"2021-12-31", "s3", "0.00", derived},
		{"2022-06-30", "s3", "0.00", derived},
	} {
		p, ok := byDay[day(t, c.day)][c.pos]
		if !ok {
			t.Errorf("%s %s: no position", c.day, c.pos)
			continue
		}
		if p.BookValue == nil || p.BookValue.StringFixed(2) != c.book {
			t.Errorf("%s %s: book value %v, want %s", c.day, c.pos, p.BookValue, c.book)
		}
		if p.Basis != c.basis {
			t.Errorf("%s %s: stamp %+v, want %+v", c.day, c.pos, p.Basis, c.basis)
		}
	}
	want := `{"paid_in":"10000","property_distributed":"4000","tax_basis_contributed":"10000"}`
	if got := string(byDay[day(t, "2022-06-30")]["s1"].Payload); got != want {
		t.Errorf("s1 payload = %s, want %s", got, want)
	}
	if p := byDay[day(t, "2021-06-30")]["s1"]; p.Payload != nil {
		t.Errorf("s1 payload before the K-1 period end = %s, want none", p.Payload)
	}
}

// A silver older than migration 0008 states no property distributions:
// it still loads, and every book value is the capital paid in.
func TestASilverWithoutPropertyDistributionsKeepsThePaidInBookValue(t *testing.T) {
	path, db := newFixtureSilver(t)
	seedInKind(t, db)
	if _, err := db.Exec(`ALTER TABLE k1_capital_accounts DROP COLUMN property_distributions_minor`); err != nil {
		t.Fatal(err)
	}
	p := inKindPositions(t, path)[day(t, "2022-06-30")]["s1"]
	if p.BookValue == nil || p.BookValue.StringFixed(2) != "10000.00" || p.Basis != paidInBasis {
		t.Errorf("s1 book value %v stamped %+v, want 10000.00 stated paid-in", p.BookValue, p.Basis)
	}
}

// A K-1's period end is a snapshot day even where no position event
// falls on it, so the reduction lands on its own date.
func TestThePropertyDistributionCutLandsOnTheK1PeriodEnd(t *testing.T) {
	path, db := newFixtureSilver(t)
	seedInKind(t, db)
	if _, err := db.Exec(`DELETE FROM position_snapshots WHERE event_type = 'statement'`); err != nil {
		t.Fatal(err)
	}
	at, ok := inKindPositions(t, path)[day(t, "2021-12-31")]
	if !ok {
		t.Fatal("no snapshot on the K-1 period end")
	}
	for pos, want := range map[string]string{"s1": "6000.00", "s2": "5000.00", "s3": "0.00"} {
		if p := at[pos]; p.BookValue == nil || p.BookValue.StringFixed(2) != want {
			t.Errorf("%s book value %v on the period end, want %s", pos, p.BookValue, want)
		}
	}
}

// A fund name two offerings carry does not say which position the K-1
// belongs to, so its property distribution reduces neither, and the load
// names the fund.
func TestAnAmbiguousK1FundReducesNoPosition(t *testing.T) {
	path, db := newFixtureSilver(t)
	seedInKind(t, db)
	if _, err := db.Exec(`UPDATE offerings SET fund_name = 'Example SPV One, LP' WHERE position_external_id = 's2'`); err != nil {
		t.Fatal(err)
	}
	var logged bytes.Buffer
	log.SetOutput(&logged)
	defer log.SetOutput(os.Stderr)
	at := inKindPositions(t, path)[day(t, "2022-06-30")]
	for pos, want := range map[string]string{"s1": "10000.00", "s2": "5000.00"} {
		if p := at[pos]; p.BookValue == nil || p.BookValue.StringFixed(2) != want || p.Basis != paidInBasis {
			t.Errorf("%s book value %v stamped %+v, want %s stated paid-in", pos, p.BookValue, p.Basis, want)
		}
	}
	if got := logged.String(); !strings.Contains(got, "1 K-1 fund(s)") ||
		!strings.Contains(got, "Example SPV One, LP (on 2 offerings)") {
		t.Errorf("log = %q, want the ambiguous fund named", got)
	}
}
