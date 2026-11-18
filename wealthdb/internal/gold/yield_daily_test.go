package gold

import (
	"database/sql"
	"math"
	"math/rand"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/yield"
)

// A holding's daily amounts are what positions_at and cash_chosen read on
// each day of the year, at the day's last second and on the instant's day
// at the instant: its account's active snapshot at it, a day not held as
// zero. The book is invented: snapshots at random instants over two
// years, two on one day, one at a day's first second and one at its
// last, one after the instant; positions in and out of them, one of no
// value; cash in two balance kinds and in zero; a stale source of one
// old snapshot; and a balance held from the 5th to the 25th of every
// month, which every month end reads as zero.
func TestDailyAmountsAreEachDaysHoldings(t *testing.T) {
	t.Parallel()
	db, ctx := openMigrated(t)
	asOf := ymd(2026, 6, 30).Unix() + 86399
	first := ymd(2025, 7, 1).Unix() + 86399
	rng := rand.New(rand.NewSource(19))
	snaps := []int64{first, first + 1, ymd(2025, 7, 1).Unix(), ymd(2025, 11, 3).Unix() + 36000,
		ymd(2025, 11, 3).Unix() + 54000, asOf, asOf + 1}
	for range 150 {
		snaps = append(snaps, ymd(2024, 6, 1).Unix()+rng.Int63n(ymd(2026, 7, 15).Unix()-ymd(2024, 6, 1).Unix()))
	}
	var positions, balances []string
	for _, at := range snaps {
		for _, k := range []string{"A1/QP1", "A1/QP2", "A2/QP3", "A2/QP4"} {
			if rng.Intn(10) < 7 {
				acct, iid, _ := strings.Cut(k, "/")
				value := itoa(100 + rng.Int63n(10000))
				if iid == "QP4" && rng.Intn(3) == 0 {
					value = "NULL"
				}
				positions = append(positions, "('a', "+itoa(at)+", '"+acct+"', '"+iid+"', '"+iid+
					"', 'public_equity', 'stock', 'USD', 1, "+value+")")
			}
		}
	}
	for range 120 {
		at := ymd(2024, 6, 1).Unix() + rng.Int63n(ymd(2026, 7, 15).Unix()-ymd(2024, 6, 1).Unix())
		for _, b := range []struct{ acct, ccy, kind string }{{"C1", "USD", "current"}, {"C1", "USD", "available"},
			{"C1", "EUR", "current"}, {"C2", "USD", "available"}} {
			if rng.Intn(10) < 7 {
				amount := itoa(rng.Int63n(5000))
				if rng.Intn(5) == 0 {
					amount = "0"
				}
				balances = append(balances, "('a', "+itoa(at)+", '"+b.acct+"', '"+b.ccy+"', '"+b.kind+"', "+amount+")")
			}
		}
	}
	for m := 0; m < 19; m++ {
		balances = append(balances,
			"('m', "+itoa(ymd(2025, 1+m, 5).Unix()+43200)+", 'M', 'USD', 'current', 5000)",
			"('m', "+itoa(ymd(2025, 1+m, 25).Unix()+43200)+", 'M', 'USD', 'current', 0)")
	}
	positions = append(positions, "('b', "+itoa(ymd(2024, 1, 1).Unix())+
		", 'B1', 'QOLD', 'QOLD', 'public_equity', 'stock', 'USD', 1, 777)")
	execEach(t, db,
		`INSERT INTO positions (silver_source_id, snapshot_at, account_external_id, position_key, instrument_external_id,
		    asset_class, vehicle, currency, quantity, market_value) VALUES `+strings.Join(positions, ", "),
		`INSERT INTO cash_balances (silver_source_id, snapshot_at, account_external_id, currency, balance_kind, amount)
		    VALUES `+strings.Join(balances, ", "))

	b := &yieldBase{in: &yield.Inputs{AsOf: asOf, Day: ymd(2026, 6, 30)}}
	if err := b.readAmounts(ctx, db); err != nil {
		t.Fatal(err)
	}
	const days = 365
	want := map[yield.HoldingKey][]float64{}
	add := func(k yield.HoldingKey, day int, v float64) {
		if want[k] == nil {
			want[k] = make([]float64, days)
		}
		want[k][day] += v
	}
	for day := range days {
		at := first + int64(day)*86400
		if day == days-1 {
			at = asOf
		}
		if err := scanAll(ctx, db, `
SELECT silver_source_id, account_external_id, position_key, SUM(market_value)::DOUBLE
  FROM positions_at([?::BIGINT]) GROUP BY ALL
 UNION ALL
SELECT silver_source_id, account_external_id, 'cash:' || currency, amount::DOUBLE FROM cash_chosen(?)`,
			func(rows *sql.Rows) error {
				var k yield.HoldingKey
				var v sql.NullFloat64
				if err := rows.Scan(&k.Source, &k.Account, &k.PositionKey, &v); err != nil {
					return err
				}
				add(k, day, v.Float64)
				return nil
			}, at, at); err != nil {
			t.Fatal(err)
		}
	}
	// Each holding of this book is valued in one currency.
	amounts := map[yield.HoldingKey]*yield.Daily{}
	for k, d := range b.amounts {
		amounts[k.HoldingKey] = d
	}
	for k, d := range amounts {
		if want[k] == nil {
			want[k] = make([]float64, days)
		}
		if d.Days != days {
			t.Errorf("%v: %d days", k, d.Days)
		}
	}
	if len(want) != 9 {
		t.Errorf("%d holdings: %v", len(want), want)
	}
	for k, w := range want {
		got := make([]float64, days)
		d := amounts[k]
		if d != nil {
			for _, s := range d.Spans {
				for i := s.From; i < s.To; i++ {
					got[i] += s.Amount
				}
			}
		}
		sum := 0.0
		for i := range days {
			sum += w[i]
			if got[i] != w[i] {
				t.Errorf("%v on day %d: %v, positions_at %v", k, i, got[i], w[i])
				break
			}
		}
		if d != nil && math.Abs(d.Mean()-sum/days) > 1e-9 {
			t.Errorf("%v: mean %v, day by day %v", k, d.Mean(), sum/days)
		}
	}
	// Zero at every month end, held two thirds of the year.
	m := amounts[yield.HoldingKey{Source: "m", Account: "M", PositionKey: "cash:USD"}]
	if m == nil || m.Mean() < 5000*0.6 || m.Mean() > 5000*0.7 {
		t.Errorf("between the month ends: %+v", m)
	}
	for _, at := range monthEndsTo(asOf, 12) {
		var n int
		if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM cash_chosen(?) WHERE silver_source_id = 'm'`, at).
			Scan(&n); err != nil || n != 0 {
			t.Errorf("held at the month end %d: %d %v", at, n, err)
		}
	}
	if old := amounts[yield.HoldingKey{Source: "b", Account: "B1", PositionKey: "QOLD"}]; old == nil ||
		old.Mean() != 777 || len(old.Spans) != 1 {
		t.Errorf("a stale source %+v", old)
	}
}

// A holding's daily amounts in another currency than its own convert at
// the instant's rates, its own as they are; where no rate converts them
// they count nothing. The holding and its amounts are invented: valued in
// francs for its first hundred days, in kronor, which no rate converts,
// for the next hundred, and in dollars since.
func TestADailyAmountNoRateConverts(t *testing.T) {
	t.Parallel()
	k := yield.HoldingKey{Source: "src", Account: "A", PositionKey: "QSTK"}
	in := &yield.Inputs{Currency: "USD", FXRates: map[string]float64{"CHF": 1.25},
		Holdings: []yield.Holding{{HoldingKey: k, Currency: "USD"}}, Daily: map[yield.HoldingKey]yield.Daily{},
		FormerDaily: map[yield.InstrumentKey][]yield.Daily{}}
	b := &yieldBase{amounts: map[amountKey]*yield.Daily{
		{k, "CHF"}: {Days: 365, Currency: "CHF", Spans: []yield.Span{{From: 0, To: 100, Amount: 4000}}},
		{k, "SEK"}: {Days: 365, Currency: "SEK", Spans: []yield.Span{{From: 100, To: 200, Amount: 50000}}},
		{k, "USD"}: {Days: 365, Currency: "USD", Spans: []yield.Span{{From: 200, To: 365, Amount: 5000}}},
	}}
	b.daily(in)
	want := []yield.Span{{From: 0, To: 100, Amount: 5000}, {From: 200, To: 365, Amount: 5000}}
	if d := in.Daily[k]; d.Days != 365 || d.Currency != "USD" || !slices.Equal(d.Spans, want) {
		t.Errorf("daily %+v, want spans %+v", d, want)
	}
}

// cashKindRank orders the balance kinds as cash_chosen does, so a
// holding's daily cash reads the balance its holdings read.
func TestCashKindRankIsCashChosens(t *testing.T) {
	t.Parallel()
	var body string
	for _, s := range mustDerived(t).stmts {
		if s.name == "cash_chosen" {
			body = s.sql
		}
	}
	got := regexp.MustCompile(`(?s)CASE cb\.balance_kind\s.*?\sEND`).FindString(body)
	if got == "" {
		t.Fatal("cash_chosen orders by no CASE cb.balance_kind")
	}
	if norm := func(s string) string { return strings.Join(strings.Fields(s), " ") }; norm(got) != norm(cashKindRank) {
		t.Errorf("cash_chosen orders by\n%s\nnot cashKindRank\n%s", norm(got), norm(cashKindRank))
	}
}
