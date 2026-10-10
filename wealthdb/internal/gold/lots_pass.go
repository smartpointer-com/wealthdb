package gold

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/lots"
)

// lotsEngineVersion enters every input fingerprint. A change to what the
// engine, the feed or a registered policy makes of the same input bumps
// it, so the next pass replays.
const lotsEngineVersion = 1

// LotsOptions configure a lot pass.
type LotsOptions struct {
	Config lots.Config
	// Force replays even when no input changed since the last pass.
	Force bool
}

// LotsSourceSummary is one source's line of a pass.
type LotsSourceSummary struct {
	SourceID, Mode, Grain, Methods, Fees       string
	DatedBySettlement                          bool
	Events, Lots, Disposals, RealizedLots      int
	PositionsFilled, Seeds, ImpliedDisposals   int
	Blips, Anchors, AnchorsKept, SeedsResolved int
	FeeUnvalued, KeysSkipped                   int
}

// LotsSummary is what a pass did.
type LotsSummary struct {
	BuildID int64
	// Unchanged is a pass that found every input as the last pass left
	// it, and replayed nothing.
	Unchanged bool
	Sources   []LotsSourceSummary
	Took      time.Duration
}

// RebuildLots runs the lot engine over every source whose policy is not
// off (docs/LOTS.md §2): it reads gold into events, replays them, and
// rewrites the ledger, the rebuilt cost basis on positions and the
// engine's realized lots in one transaction. It replays nothing when
// every source's inputs, the config and the engine are as the last pass
// found them, unless opt.Force.
func RebuildLots(ctx context.Context, db *sql.DB, opt LotsOptions) (*LotsSummary, error) {
	start := time.Now()
	f, err := newLotFeed(ctx, db, opt.Config)
	if err != nil {
		return nil, err
	}
	prints, err := lotFingerprints(ctx, db, f, opt.Config)
	if err != nil {
		return nil, err
	}
	if !opt.Force {
		same, err := lotInputsUnchanged(ctx, db, prints)
		if err != nil {
			return nil, err
		}
		if same {
			return &LotsSummary{Unchanged: true, Took: time.Since(start)}, nil
		}
	}
	if err := f.read(ctx, db); err != nil {
		return nil, err
	}
	lots.SortEvents(f.events)
	res := lots.Run(f.keys, f.events)

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("lots: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	var build int64
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(build_id), 0) + 1 FROM lot_runs`).Scan(&build); err != nil {
		return nil, fmt.Errorf("lots: next build: %w", err)
	}
	w := &lotWriter{tx: tx, f: f, res: &res, build: build, sums: map[string]*LotsSourceSummary{}}
	for _, id := range f.ids {
		s := f.sources[id]
		w.sums[id] = &LotsSourceSummary{SourceID: id, Mode: s.pol.Mode.String(), Grain: s.pol.Grain.String(),
			Fees: s.pol.Fees.String(), KeysSkipped: f.skipped[id]}
	}
	if err := w.write(ctx, prints); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("lots: commit: %w", err)
	}
	out := &LotsSummary{BuildID: build, Took: time.Since(start)}
	for _, id := range f.ids {
		out.Sources = append(out.Sources, *w.sums[id])
	}
	return out, nil
}

// lotFingerprints is one fingerprint per replayed source: its rows in
// every table the feed reads, the rows that tie sources together (the
// rates, the portfolios), its policy and methods, and the engine
// version. A row the engine itself wrote does not count.
func lotFingerprints(ctx context.Context, db *sql.DB, f *lotFeed, cfg lots.Config) (map[string]string, error) {
	out := map[string]string{}
	if len(f.ids) == 0 {
		return out, nil
	}
	rows, err := db.QueryContext(ctx, `
WITH ids AS (SELECT unnest(?::VARCHAR[]) id),
     t AS (SELECT silver_source_id s, count(*) n, bit_xor(hash(transaction_external_id, occurred_at, account_external_id,
                  instrument_external_id, instrument_hint, kind, currency, quantity, net_amount, description, payload::VARCHAR)) h
             FROM transactions WHERE silver_source_id IN (SELECT id FROM ids) GROUP BY 1),
     p AS (SELECT silver_source_id s, count(*) n, bit_xor(hash(snapshot_at, account_external_id, position_key,
                  instrument_external_id, currency, quantity, asset_class, vehicle,
                  CASE WHEN stated_basis(book_value, basis_origin) THEN book_value END)) h
             FROM positions WHERE silver_source_id IN (SELECT id FROM ids) GROUP BY 1),
     l AS (SELECT silver_source_id s, count(*) n, bit_xor(hash(snapshot_at, account_external_id, position_key, lot_key,
                  quantity, book_value, acquisition_date)) h
             FROM position_lots WHERE silver_source_id IN (SELECT id FROM ids) GROUP BY 1),
     r AS (SELECT silver_source_id s, count(*) n, bit_xor(hash(realized_lot_external_id, account_external_id,
                  instrument_external_id, instrument_hint, description, quantity, book_value, disposal_date,
                  settlement_date, acquisition_date, tax_year, is_primary)) h
             FROM realized_lots WHERE silver_source_id IN (SELECT id FROM ids) GROUP BY 1),
     i AS (SELECT silver_source_id s, bit_xor(hash(instrument_external_id, cusip, symbol)) h
             FROM instruments WHERE silver_source_id IN (SELECT id FROM ids) GROUP BY 1),
     a AS (SELECT src s, bit_xor(hash(acct, pid)) h FROM portfolio_acct_map() GROUP BY 1),
     x AS (SELECT count(*) n, bit_xor(hash(from_ccy, to_ccy, day, rate)) h FROM fx_daily)
SELECT ids.id, concat_ws('/', t.n, t.h, p.n, p.h, l.n, l.h, r.n, r.h, i.h, a.h, x.n, x.h)
  FROM ids
  LEFT JOIN t ON t.s = ids.id LEFT JOIN p ON p.s = ids.id LEFT JOIN l ON l.s = ids.id
  LEFT JOIN r ON r.s = ids.id LEFT JOIN i ON i.s = ids.id LEFT JOIN a ON a.s = ids.id, x`, f.ids)
	if err != nil {
		return nil, fmt.Errorf("lots: fingerprint: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id, content string
		if err := rows.Scan(&id, &content); err != nil {
			return nil, err
		}
		s := f.sources[id]
		pol, _ := json.Marshal(struct {
			Mode, Grain string
			Method      string
			Source      any
			Portfolios  any
			Accounts    any
		}{s.pol.Mode.String(), s.pol.Grain.String(), cfg.Method.String(), cfg.Sources[id], cfg.Portfolios[id], cfg.Accounts[id]})
		out[id] = fmt.Sprintf("v%d|%s|%s", lotsEngineVersion, pol, content)
	}
	return out, rows.Err()
}

// lotInputsUnchanged reports whether the last pass replayed exactly
// these sources with exactly these fingerprints.
func lotInputsUnchanged(ctx context.Context, db *sql.DB, prints map[string]string) (bool, error) {
	rows, err := db.QueryContext(ctx, `SELECT silver_source_id, input_fingerprint FROM lot_sources()`)
	if err != nil {
		return false, fmt.Errorf("lots: last run: %w", err)
	}
	defer rows.Close()
	last := map[string]string{}
	for rows.Next() {
		var id, fp string
		if err := rows.Scan(&id, &fp); err != nil {
			return false, err
		}
		last[id] = fp
	}
	if err := rows.Err(); err != nil {
		return false, err
	}
	if len(last) != len(prints) {
		return false, nil
	}
	for id, fp := range prints {
		if last[id] != fp {
			return false, nil
		}
	}
	// A reload rewrites a source's rows unchanged and drops what the last
	// pass wrote for it (loader.Reset): the ledger, the engine's realized
	// lots and the filled positions must all still be there.
	var intact bool
	err = db.QueryRowContext(ctx, `
WITH last AS (SELECT * FROM lot_sources())
SELECT (SELECT count(*) FROM lots) = (SELECT COALESCE(SUM(lots), 0) FROM last)
   AND (SELECT count(*) FROM lot_realized) = (SELECT COALESCE(SUM(realized_lots), 0) FROM last)
   AND (SELECT count(*) FROM positions WHERE book_value_known IS NOT NULL)
         = (SELECT COALESCE(SUM(positions_filled), 0) FROM last)`).Scan(&intact)
	return intact, err
}
