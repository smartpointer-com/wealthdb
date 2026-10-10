package lots

import (
	"math/rand/v2"
	"runtime"
	"strconv"
	"testing"
	"time"
)

// synthetic is an algotrader's history: n events over keys keys, three
// buys for every two sells, a snapshot of every key every tenth of the
// stream. It leaves about n/10 lots open at the end.
func synthetic(n, keys int, m Method) ([]KeySpec, []Event) {
	r := rand.New(rand.NewPCG(1, 2))
	ks := make([]KeySpec, keys)
	for i := range ks {
		ks[i] = KeySpec{ID: "k" + strconv.Itoa(i), Method: m}
	}
	held := make([]float64, keys)
	evs := make([]Event, 0, n+n/10)
	at := int64(1_600_000_000)
	for i := 0; len(evs) < n; i++ {
		at += 7
		k := int32(r.IntN(keys))
		q := float64(1 + r.IntN(100))
		txn := "t" + strconv.Itoa(i)
		if r.IntN(5) < 3 || held[k] < q {
			evs = append(evs, Event{At: at, Kind: Acquire, Key: k, Txn: txn, Qty: q, Value: q * (50 + 50*r.Float64()),
				HasValue: true, AcqDay: DayOf(at), Origin: OriginBuy, CostOrigin: CostTrade})
			held[k] += q
		} else {
			// Sells take a little less than the buys bring, so lots pile up.
			q *= 0.75
			evs = append(evs, Event{At: at, Kind: Dispose, Key: k, Txn: txn, Qty: q, Value: q * 80, HasValue: true, Disposal: DisposeSell})
			held[k] -= q
		}
		if i%(n/10) == n/10-1 {
			for j := range keys {
				evs = append(evs, Event{At: at, Kind: Observe, Key: int32(j), Txn: "obs", Qty: held[j], Obs: int32(j)})
			}
		}
	}
	SortEvents(evs)
	return ks, evs
}

func benchmarkRun(b *testing.B, m Method) {
	keys, evs := synthetic(1_000_000, 1000, m)
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		r := Run(keys, evs)
		if len(r.Lots) == 0 {
			b.Fatal("no lots")
		}
	}
}

func BenchmarkRunFIFO(b *testing.B)    { benchmarkRun(b, FIFO) }
func BenchmarkRunLIFO(b *testing.B)    { benchmarkRun(b, LIFO) }
func BenchmarkRunHIFO(b *testing.B)    { benchmarkRun(b, HIFO) }
func BenchmarkRunAverage(b *testing.B) { benchmarkRun(b, Average) }

// TestMillionEventsBudget holds the engine to its budget: a million
// events, a hundred thousand lots open at the end, in under ten seconds
// and a gigabyte. It skips under -short.
func TestMillionEventsBudget(t *testing.T) {
	if testing.Short() {
		t.Skip("budget test")
	}
	for _, m := range []Method{FIFO, HIFO} {
		keys, evs := synthetic(1_000_000, 1000, m)
		var before, after runtime.MemStats
		runtime.GC()
		runtime.ReadMemStats(&before)
		start := time.Now()
		r := Run(keys, evs)
		took := time.Since(start)
		runtime.ReadMemStats(&after)
		open := 0
		for _, l := range r.Lots {
			if l.ClosedAt == 0 {
				open++
			}
		}
		alloc := (after.TotalAlloc - before.TotalAlloc) >> 20
		if open < 100_000 {
			t.Errorf("%s: %d lots open, the budget wants at least 100000", m, open)
		}
		if took > 10*time.Second || alloc > 1024 {
			t.Errorf("%s: %v and %d MB, budget 10s and 1024 MB", m, took, alloc)
		}
		t.Logf("%s: %d events, %d lots (%d open), %d disposals: %v, %d MB allocated",
			m, len(evs), len(r.Lots), open, len(r.Disposals), took.Round(time.Millisecond), alloc)
	}
}
