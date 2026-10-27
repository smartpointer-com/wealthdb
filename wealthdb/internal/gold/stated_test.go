package gold

import (
	"slices"
	"testing"
	"time"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
)

// TestReconcileStatedPeriods sets a document's stated months against the
// engine's monthly account rows: a month the ledger agrees with, one whose
// stated flow the ledger lacks, one before the account's history, and a
// month a second account states in its own currency, EUR.
func TestReconcileStatedPeriods(t *testing.T) {
	db, ctx := openMigrated(t)
	seedReturnsSource(t, db, ctx, "bank", "fidelity")
	monthEnd := func(y int, m time.Month) int64 {
		return time.Date(y, m+1, 0, 0, 0, 0, 0, time.UTC).Unix()
	}
	first := func(y int, m time.Month) int64 { return time.Date(y, m, 1, 0, 0, 0, 0, time.UTC).Unix() }
	seedAcct(t, db, ctx, "bank", "ACCT-1", canonical.AccountKindBrokerage, nil,
		[]snap{{monthEnd(2024, time.January), 1000}, {monthEnd(2024, time.February), 1600},
			{monthEnd(2024, time.March), 1700}, {monthEnd(2024, time.June), 1700}},
		[]txn{{dy(2024, time.February, 10), canonical.TxKindDeposit, 500}})
	eur := "EUR"
	inTx(t, db, ctx, func(w *Writer) error {
		if err := w.UpsertAccounts(ctx, []canonical.AccountChange{{
			SilverSourceID: "bank", AccountExternalID: "ACCT-2", AccountKind: canonical.AccountKindBrokerage,
			BaseCurrency: &eur, FirstSeenAt: monthEnd(2024, time.January), LastSeenAt: monthEnd(2024, time.February),
		}}); err != nil {
			return err
		}
		var pos []canonical.PositionChange
		for _, s := range []snap{{monthEnd(2024, time.January), 2000}, {monthEnd(2024, time.February), 2300}} {
			pos = append(pos, canonical.PositionChange{
				SilverSourceID: "bank", SnapshotAt: s.at, AccountExternalID: "ACCT-2",
				PositionKey: "P", AssetClass: canonical.AssetClassPublicEquity, Vehicle: canonical.VehicleStock,
				Currency: "EUR", MarketValue: decp(s.val),
			})
		}
		if err := w.InsertPositions(ctx, pos); err != nil {
			return err
		}
		return w.InsertTransactions(ctx, []canonical.TransactionChange{{
			SilverSourceID: "bank", TransactionExternalID: "ACCT-2-tx0", OccurredAt: dy(2024, time.February, 12),
			AccountExternalID: "ACCT-2", Kind: canonical.TxKindDeposit, Currency: "EUR", NetAmount: decp(200),
		}})
	})

	stated := func(m time.Month, additions, ending int64) canonical.StatedPeriodChange {
		return canonical.StatedPeriodChange{SilverSourceID: "bank", AccountExternalID: "ACCT-1",
			AsOf: monthEnd(2024, m), PeriodStart: first(2024, m), Currency: "USD",
			NetAdditions: decp(additions), EndingValue: decp(ending), SourceSHA256: "doc"}
	}
	inTx(t, db, ctx, func(w *Writer) error {
		return w.InsertReference(ctx, &canonical.SnapshotBatch{StatedPeriods: []canonical.StatedPeriodChange{
			stated(time.February, 500, 1600),
			stated(time.March, 50, 1710),
			{SilverSourceID: "bank", AccountExternalID: "ACCT-2", AsOf: monthEnd(2024, time.February),
				PeriodStart: first(2024, time.February), Currency: "EUR",
				NetAdditions: decp(200), EndingValue: decp(2300), SourceSHA256: "doc"},
			{SilverSourceID: "bank", AccountExternalID: "ACCT-1", AsOf: monthEnd(2023, time.December),
				PeriodStart: first(2023, time.December), Currency: "USD", NetAdditions: decp(0), SourceSHA256: "doc"},
		}})
	})

	p := params("accounts", 0, time.Date(2024, time.December, 31, 0, 0, 0, 0, time.UTC).Unix())
	rows, err := ReconcileStatedPeriods(ctx, db, p)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 4 {
		t.Fatalf("rows = %d, want 4: %+v", len(rows), rows)
	}
	get := func(s *string) string {
		if s == nil {
			return "<nil>"
		}
		d, err := canonical.NewDecimalFromString(*s)
		if err != nil {
			t.Fatalf("decimal %q: %v", *s, err)
		}
		return d.StringFixed(2)
	}
	dec, feb, mar, eurFeb := rows[0], rows[1], rows[2], rows[3]
	// The month before the history reads no_engine_row, then the
	// account's since-inception tags, which say why.
	engine := p
	engine.Period, engine.Method, engine.FromEpoch = "monthly", "twr", 0
	usdRows, err := RunReturns(ctx, db, engine)
	if err != nil {
		t.Fatal(err)
	}
	summary, ok := summaryFor(usdRows, "ACCT-1")
	if !ok {
		t.Fatal("no since-inception row for ACCT-1")
	}
	if want := append([]string{"no_engine_row"}, summary.Quality...); dec.Period != "2023-12" ||
		!slices.Equal(dec.Quality, want) || dec.NetFlow != nil || dec.FlowGap != nil {
		t.Errorf("before history: quality %v, want %v; %+v", dec.Quality, want, dec)
	}
	if feb.Period != "2024-02" || get(feb.NetFlow) != "500.00" || get(feb.FlowGap) != "0.00" || get(feb.ValueGap) != "0.00" {
		t.Errorf("February: net %s flow gap %s value gap %s", get(feb.NetFlow), get(feb.FlowGap), get(feb.ValueGap))
	}
	if get(feb.StartValue) != "1000.00" || get(feb.EndValue) != "1600.00" {
		t.Errorf("February values: %s %s", get(feb.StartValue), get(feb.EndValue))
	}
	if mar.Period != "2024-03" || get(mar.NetFlow) != "0.00" || get(mar.FlowGap) != "50.00" || get(mar.ValueGap) != "10.00" {
		t.Errorf("March: net %s flow gap %s value gap %s", get(mar.NetFlow), get(mar.FlowGap), get(mar.ValueGap))
	}
	if eurFeb.AccountExternalID != "ACCT-2" || eurFeb.Currency != "EUR" || get(eurFeb.NetFlow) != "200.00" ||
		get(eurFeb.FlowGap) != "0.00" || get(eurFeb.EndValue) != "2300.00" || get(eurFeb.ValueGap) != "0.00" {
		t.Errorf("EUR February: %s net %s flow gap %s end %s value gap %s", eurFeb.Currency,
			get(eurFeb.NetFlow), get(eurFeb.FlowGap), get(eurFeb.EndValue), get(eurFeb.ValueGap))
	}
	if mar.StatedBeginning != nil {
		t.Errorf("a figure the document does not state reads %v", *mar.StatedBeginning)
	}

	// A window that starts mid-February still sets the whole stated
	// February against the engine's whole February.
	mid := p
	mid.FromEpoch = time.Date(2024, time.February, 15, 0, 0, 0, 0, time.UTC).Unix()
	later, err := ReconcileStatedPeriods(ctx, db, mid)
	if err != nil {
		t.Fatal(err)
	}
	if len(later) != 3 || get(later[0].StartValue) != "1000.00" || get(later[0].NetFlow) != "500.00" {
		t.Errorf("mid-month window: %d rows, February start %s net %s", len(later), get(later[0].StartValue), get(later[0].NetFlow))
	}

	// A period inside one month that starts after its first day is still
	// that month; a period longer than a month sums its months' flows.
	if got := statedPeriodLabel(time.Date(2024, time.May, 2, 0, 0, 0, 0, time.UTC).Unix(), monthEnd(2024, time.May)); got != "2024-05" {
		t.Errorf("a month from its first business day reads %q", got)
	}
	inTx(t, db, ctx, func(w *Writer) error {
		return w.InsertReference(ctx, &canonical.SnapshotBatch{StatedPeriods: []canonical.StatedPeriodChange{{
			SilverSourceID: "bank", AccountExternalID: "ACCT-1", AsOf: monthEnd(2024, time.March),
			PeriodStart: first(2024, time.February), Currency: "USD", NetAdditions: decp(550), SourceSHA256: "doc2"},
			{SilverSourceID: "bank", AccountExternalID: "ACCT-1", AsOf: monthEnd(2024, time.May),
				PeriodStart: first(2024, time.April), Currency: "USD", NetAdditions: decp(0), SourceSHA256: "doc3"},
		}})
	})
	rows, err = ReconcileStatedPeriods(ctx, db, p)
	if err != nil {
		t.Fatal(err)
	}
	var q, gap StatedPeriodRow
	for _, r := range rows {
		switch r.SourceSHA256 {
		case "doc2":
			q = r
		case "doc3":
			gap = r
		}
	}
	// April and May have no snapshot: a period over both carries each
	// month's tags once.
	if want := []string{"empty_bucket", "carried_forward"}; !slices.Equal(gap.Quality, want) {
		t.Errorf("snapshot-less period quality %v, want %v", gap.Quality, want)
	}
	if q.Period != "2024-02-01..2024-03-31" || get(q.NetFlow) != "500.00" || get(q.FlowGap) != "50.00" || get(q.StartValue) != "1000.00" {
		t.Errorf("two-month period: %s net %s gap %s start %s", q.Period, get(q.NetFlow), get(q.FlowGap), get(q.StartValue))
	}
}

// sumDecimals is nil when any figure is missing or does not parse, and
// subDecimals when either side is; a gap is never read off a partial sum.
func TestStatedDecimalHelpers(t *testing.T) {
	s := func(v string) *string { return &v }
	if got := sumDecimals(s("1.10"), s("2.205")); got == nil || *got != "3.305" {
		t.Errorf("sum = %v, want 3.305", got)
	}
	if got := sumDecimals(s("1"), nil); got != nil {
		t.Errorf("sum with a nil = %q, want nil", *got)
	}
	if got := sumDecimals(s("1"), s("n/a")); got != nil {
		t.Errorf("sum with a bad figure = %q, want nil", *got)
	}
	if got := subDecimals(s("5"), nil); got != nil {
		t.Errorf("5 − nil = %q, want nil", *got)
	}
	if got := subDecimals(s("5"), s("x")); got != nil {
		t.Errorf("5 − x = %q, want nil", *got)
	}
	if got := subDecimals(s("5"), s("1.5")); got == nil || *got != "3.5" {
		t.Errorf("5 − 1.5 = %v, want 3.5", got)
	}
}
