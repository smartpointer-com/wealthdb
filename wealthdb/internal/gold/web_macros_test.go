package gold

import (
	"database/sql"
	"math"
	"math/rand/v2"
	"strconv"
	"strings"
	"testing"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/yield"
)

// The macros the dashboards read a figure through, each held to the Go
// function that computes the same figure for the CLI, on random inputs.

// nearPtr says two figures agree within rel of the larger, or both are none.
func nearPtr(a, b *float64, rel float64) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b || math.Abs(*a-*b) <= rel*math.Max(math.Abs(*a), math.Abs(*b))
}

func nullFloat(n sql.NullFloat64) *float64 {
	if !n.Valid {
		return nil
	}
	return &n.Float64
}

func TestYieldPartIncomeIsPartIncome(t *testing.T) {
	t.Parallel()
	db, ctx := openEmpty(t)
	r := rand.New(rand.NewPCG(1, 2))
	// A coin flip between a value and zero, so the roll's two arms and
	// the floors at zero are each met.
	maybe := func(scale float64) float64 {
		if r.IntN(2) == 0 {
			return 0
		}
		return (r.Float64()*2 - 0.5) * scale
	}
	for i := range 2000 {
		p := yield.Part{Fixed: maybe(500), Principal: maybe(1e6), Spread: maybe(2),
			RollConstant: maybe(800), RollForward: maybe(800), RollPrincipal: maybe(2e5)}
		path := yield.Paths[r.IntN(len(yield.Paths))]
		rate := (r.Float64()*8 - 1) * float64(r.IntN(2))
		shift := int64(r.IntN(1001) - 500)
		want := p.Income(path, rate, float64(shift))
		var got float64
		if err := db.QueryRowContext(ctx, `SELECT yield_part_income(?::DOUBLE, ?::DOUBLE, ?::DOUBLE, ?::DOUBLE,
            ?::DOUBLE, ?::DOUBLE, ?::DOUBLE, ?, ?::INTEGER)`, p.Fixed, p.Principal, p.Spread, rate,
			p.RollConstant, p.RollForward, p.RollPrincipal, string(path), shift).Scan(&got); err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Fatalf("part %d %+v under %s at %v%% %+dbp: yield_part_income %v, Part.Income %v",
				i, p, path, rate, shift, got, want)
		}
	}
}

func TestYieldRateAndEarningValueAreSummarize(t *testing.T) {
	t.Parallel()
	db, ctx := openEmpty(t)
	if _, err := db.ExecContext(ctx, `CREATE TABLE g (grp INTEGER, value DOUBLE, weighs BOOLEAN,
        implied DOUBLE, income DOUBLE)`); err != nil {
		t.Fatal(err)
	}
	r := rand.New(rand.NewPCG(3, 4))
	opt := func(scale float64) *float64 {
		if r.IntN(3) == 0 {
			return nil
		}
		return ptr(r.Float64() * scale)
	}
	const groups = 300
	books := make([][]yield.Position, groups)
	for g := range groups {
		for range 1 + r.IntN(6) {
			p := yield.Position{Holding: yield.Holding{Value: (r.Float64() - 0.25) * 1e5},
				Measures: yield.Measures{Kind: yieldKinds[r.IntN(len(yieldKinds))], Implied: opt(9), Income12: opt(4e3)}}
			if r.IntN(8) == 0 {
				p.Value = 0
			}
			books[g] = append(books[g], p)
			if _, err := db.ExecContext(ctx, `INSERT INTO g VALUES (?, ?, ?, ?, ?)`, g, p.Value, p.Weighs(),
				p.Implied, p.Income12); err != nil {
				t.Fatal(err)
			}
		}
	}
	got := map[int][2]*float64{}
	if err := scanAll(ctx, db, `SELECT grp, yield_rate(implied, value, weighs),
           100 * sum(income) / NULLIF(yield_earning_value(value, weighs), 0)
      FROM g GROUP BY grp`, func(rows *sql.Rows) error {
		var g int
		var rate, income sql.NullFloat64
		if err := rows.Scan(&g, &rate, &income); err != nil {
			return err
		}
		got[g] = [2]*float64{nullFloat(rate), nullFloat(income)}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	for g, ps := range books {
		rows := (&yield.Book{Positions: ps}).Aggregate(yield.GrainAll, true)
		if len(rows) != 1 {
			t.Fatalf("group %d: %d rows", g, len(rows))
		}
		if !nearPtr(got[g][0], rows[0].Implied, 1e-12) {
			t.Errorf("group %d: yield_rate %v, summarize %v", g, deref(got[g][0]), deref(rows[0].Implied))
		}
		if !nearPtr(got[g][1], rows[0].IncomeYield, 1e-12) {
			t.Errorf("group %d: income over yield_earning_value %v, summarize %v", g, deref(got[g][1]),
				deref(rows[0].IncomeYield))
		}
	}
}

func deref(f *float64) any {
	if f == nil {
		return nil
	}
	return *f
}

func TestFeeRatePctIsFeeRate(t *testing.T) {
	t.Parallel()
	db, ctx := openEmpty(t)
	r := rand.New(rand.NewPCG(5, 6))
	for i := range 2000 {
		fees := (r.Float64() - 0.1) * 5e3
		days := int64(1 + r.IntN(400))
		mean := (r.Float64() - 0.05) * 1e6
		if i%50 == 0 {
			mean = 0
		}
		var got sql.NullFloat64
		if err := db.QueryRowContext(ctx, `SELECT fee_rate_pct(?::DOUBLE, ?::DOUBLE) / 100`, fees,
			mean*float64(days)).Scan(&got); err != nil {
			t.Fatal(err)
		}
		if want := feeRate(fees, mean, days); !nearPtr(nullFloat(got), want, 1e-12) {
			t.Fatalf("fees %v on a mean of %v over %d days: fee_rate_pct %v, feeRate %v", fees, mean, days,
				deref(nullFloat(got)), deref(want))
		}
	}
}

func TestChainReturnsIsChainMonths(t *testing.T) {
	t.Parallel()
	db, ctx := openEmpty(t)
	r := rand.New(rand.NewPCG(7, 8))
	for i := range 500 {
		var months []ReturnRow
		var vals []string
		for m := range r.IntN(30) {
			row := ReturnRow{EndDay: int64(m)}
			switch r.IntN(12) {
			case 0:
				vals = append(vals, "NULL")
			case 1:
				row.TWR = ptr(-1 - r.Float64()*float64(r.IntN(2)))
			default:
				row.TWR = ptr((r.Float64() - 0.45) * 0.3)
			}
			if row.TWR != nil {
				vals = append(vals, strconv.FormatFloat(*row.TWR, 'g', -1, 64))
			}
			months = append(months, row)
		}
		want, _ := chainMonths(months, feeBucket{start: 0, end: 1 << 40})
		var got sql.NullFloat64
		q := `SELECT chain_returns(twr) FROM (SELECT NULL::DOUBLE AS twr WHERE FALSE`
		for _, v := range vals {
			q += ` UNION ALL SELECT ` + v + `::DOUBLE`
		}
		if err := db.QueryRowContext(ctx, q+`)`).Scan(&got); err != nil {
			t.Fatal(err)
		}
		if !nearPtr(nullFloat(got), want, 1e-12) {
			t.Fatalf("months %d [%s]: chain_returns %v, chainMonths %v", i, strings.Join(vals, " "),
				deref(nullFloat(got)), deref(want))
		}
	}
}
