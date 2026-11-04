package gold

import (
	"context"
	"database/sql"
	"fmt"
)

// BenchmarkNames are the benchmarks gold knows, in name order.
func BenchmarkNames(ctx context.Context, db *sql.DB) ([]string, error) {
	var out []string
	err := scanAll(ctx, db, `SELECT DISTINCT benchmark FROM benchmark_funds ORDER BY 1`, func(rows *sql.Rows) error {
		var n string
		if err := rows.Scan(&n); err != nil {
			return err
		}
		out = append(out, n)
		return nil
	})
	return out, err
}

// RunReturnsBenchmark is RunReturns at the global grain with the
// benchmark `name` beside each row: its buckets on the bucket rows, and
// on the summary row its figures over the days both sides have.
func RunReturnsBenchmark(ctx context.Context, db *sql.DB, p ReturnParams, name string) ([]ReturnRow, error) {
	fx, err := loadFxBounds(ctx, db)
	if err != nil {
		return nil, err
	}
	ds, err := loadReturnsDataset(ctx, db, p.OutCcy, fx, p.PolicyOverrides, p.TransferMatching)
	if err != nil {
		return nil, err
	}
	benchmarks, err := LoadBenchmarks(ctx, db, fx, []string{p.OutCcy})
	if err != nil {
		return nil, err
	}
	var b *Benchmark
	for _, c := range benchmarks {
		if c.Name == name {
			b = c
		}
	}
	if b == nil {
		return nil, fmt.Errorf("no benchmark named %q; 'wealthdb help benchmarks' lists them", name)
	}
	rows := computeReturns(ds, p)
	bucket := map[int64]*BenchmarkRow{}
	buckets := benchmarkBuckets(b, p.OutCcy, p.Period, rows)
	for i := range buckets {
		bucket[buckets[i].EndDay] = &buckets[i]
	}
	total, _ := benchmarkTotal(newBenchmarkWindows(ds), b, p, 0)
	for i := range rows {
		rows[i].Benchmark = bucket[rows[i].EndDay]
		if rows[i].IsSummary {
			rows[i].Benchmark = &total
		}
		if rows[i].Benchmark == nil {
			rows[i].Benchmark = &BenchmarkRow{Benchmark: name, Quality: []string{}}
		}
	}
	return rows, nil
}
