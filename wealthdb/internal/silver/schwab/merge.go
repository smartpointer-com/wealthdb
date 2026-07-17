package schwab

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"github.com/ptu-gh/wealthdb/wealthdb/internal/canonical"
	"github.com/ptu-gh/wealthdb/wealthdb/internal/silver"
)

// Connection orchestrates one or both Schwab subsources. Each
// reader (api / web) is non-nil only when its subsource is
// configured; the merge layer here handles whichever combination
// is present.
//
// Composition rules (api + web configured):
//
//	Snapshots
//	  - Live positions, cash balances, fx rates, and instrument
//	    dimensions come from api.
//	  - Web emits AccountChange rows (rewritten to api hashValue)
//	    so its `nickname` flows into gold via per-column upsert.
//	  - Web's historical position snapshots and historical cash
//	    balances stream alongside api's live ones — different
//	    snapshot timestamps (statement period-ends vs live dump
//	    times) so they coexist under the gold PK.
//
//	Transactions
//	  - Hard cut at per-account api-coverage-start. Web emits
//	    only timestamps strictly below that cutoff. api emits
//	    unfiltered above. INTEROP §2 documents why a per-row
//	    merge across the boundary isn't safe.
//
// Account identity: web stores the 3-to-5-digit account suffix;
// api stores Schwab's opaque hashValue. The orchestrator builds
// a suffix → hashValue bridge by exact digits-only equality when
// web's payload carries the full account number, else by matching
// the suffix against the trailing digits of every api
// accountNumber. Ambiguity in either tier raises a clear error
// rather than silently mismapping.
type Connection struct {
	api *apiReader
	web *webReader

	// bridge resolves web account_external_id (suffix) → api
	// account_external_id (hashValue). Built lazily on first
	// Status/Snapshots/Transactions call.
	bridge      map[string]string
	bridgeBuilt bool
	bridgeErr   error

	// apiStartByHash caches per-api-account MIN(timestamp) for
	// the transactions splice. Same lazy-build pattern as bridge.
	apiStartByHash map[string]int64
	apiStartBuilt  bool
	apiStartErr    error

	// symbolToCUSIP resolves a ticker (the schwab-web silver's
	// instrument_key) to the matching CUSIP (the schwab-api
	// silver's preferredInstrumentKey). Lets web transactions
	// land their InstrumentExternalID on the same gold
	// instruments row the api side registered — so the symbol /
	// name / asset_class columns populate via the LEFT JOIN
	// without a second instrument-emit on the web side.
	symbolToCUSIP     map[string]string
	symbolBridgeBuilt bool
	symbolBridgeErr   error
}

// Close releases both readers. Safe to call multiple times.
func (c *Connection) Close() error {
	var firstErr error
	if c.api != nil {
		if err := c.api.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
		c.api = nil
	}
	if c.web != nil {
		if err := c.web.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
		c.web = nil
	}
	return firstErr
}

// Status combines per-subsource Status. Sentinel-aware (-1 means
// "no observable state"). Matches the UBS orchestrator.
func (c *Connection) Status(ctx context.Context) (canonical.Status, error) {
	combine := func(a, b int64, op func(int64, int64) int64) int64 {
		switch {
		case a == -1:
			return b
		case b == -1:
			return a
		default:
			return op(a, b)
		}
	}
	min := func(a, b int64) int64 {
		if a < b {
			return a
		}
		return b
	}
	max := func(a, b int64) int64 {
		if a > b {
			return a
		}
		return b
	}
	out := canonical.Status{
		OldestSnapshotAt:    -1,
		LatestSnapshotAt:    -1,
		OldestTransactionAt: -1,
		LatestTransactionAt: -1,
		LatestChangeNumber:  -1,
	}
	if c.api != nil {
		s, err := c.api.Status(ctx)
		if err != nil {
			return canonical.Status{}, fmt.Errorf("schwab api status: %w", err)
		}
		out.OldestSnapshotAt = combine(out.OldestSnapshotAt, s.OldestSnapshotAt, min)
		out.LatestSnapshotAt = combine(out.LatestSnapshotAt, s.LatestSnapshotAt, max)
		out.OldestTransactionAt = combine(out.OldestTransactionAt, s.OldestTransactionAt, min)
		out.LatestTransactionAt = combine(out.LatestTransactionAt, s.LatestTransactionAt, max)
		out.LatestChangeNumber = combine(out.LatestChangeNumber, s.LatestChangeNumber, max)
	}
	if c.web != nil {
		s, err := c.web.Status(ctx)
		if err != nil {
			return canonical.Status{}, fmt.Errorf("schwab web status: %w", err)
		}
		out.OldestSnapshotAt = combine(out.OldestSnapshotAt, s.OldestSnapshotAt, min)
		out.LatestSnapshotAt = combine(out.LatestSnapshotAt, s.LatestSnapshotAt, max)
		out.OldestTransactionAt = combine(out.OldestTransactionAt, s.OldestTransactionAt, min)
		out.LatestTransactionAt = combine(out.LatestTransactionAt, s.LatestTransactionAt, max)
		out.LatestChangeNumber = combine(out.LatestChangeNumber, s.LatestChangeNumber, max)
	}
	return out, nil
}

// ChangeWindow yields a window covering whichever subsource has
// advanced past sinceN. NewChangeNumber is max across subsources.
// HasChanges is true when at least one subsource reports changes.
func (c *Connection) ChangeWindow(ctx context.Context, sinceN int64) (canonical.Window, error) {
	out := canonical.Window{NewChangeNumber: sinceN}
	if c.api != nil {
		w, err := c.api.ChangeWindow(ctx, sinceN)
		if err != nil {
			return canonical.Window{}, fmt.Errorf("schwab api ChangeWindow: %w", err)
		}
		if w.HasChanges {
			if !out.HasChanges {
				out.Start, out.End = w.Start, w.End
			} else {
				out.Start = min(out.Start, w.Start)
				out.End = max(out.End, w.End)
			}
			out.HasChanges = true
		}
		out.NewChangeNumber = max(out.NewChangeNumber, w.NewChangeNumber)
	}
	if c.web != nil {
		w, err := c.web.ChangeWindow(ctx, sinceN)
		if err != nil {
			return canonical.Window{}, fmt.Errorf("schwab web ChangeWindow: %w", err)
		}
		if w.HasChanges {
			if !out.HasChanges {
				out.Start, out.End = w.Start, w.End
			} else {
				out.Start = min(out.Start, w.Start)
				out.End = max(out.End, w.End)
			}
			out.HasChanges = true
		}
		out.NewChangeNumber = max(out.NewChangeNumber, w.NewChangeNumber)
	}
	return out, nil
}

// Snapshots emits the merged stream. When only api is configured,
// the orchestrator is a thin passthrough.
func (c *Connection) Snapshots(ctx context.Context, w canonical.Window) (silver.SnapshotStream, error) {
	if c.web == nil {
		// api-only — thin passthrough.
		return c.api.Snapshots(ctx, w)
	}
	bridge, err := c.ensureBridge(ctx)
	if err != nil {
		return nil, err
	}

	streams := make([]silver.SnapshotStream, 0, 3)

	if c.web != nil {
		hist, err := c.web.snapshotsHistorical(ctx, w, bridge)
		if err != nil {
			return nil, fmt.Errorf("schwab web Snapshots (historical): %w", err)
		}
		streams = append(streams, hist)
	}
	if c.api != nil {
		// api's Snapshots also surfaces accounts. We rely on the
		// per-column upsert in gold to merge web's nickname (emitted
		// below) with api's other account columns.
		s, err := c.api.Snapshots(ctx, w)
		if err != nil {
			return nil, fmt.Errorf("schwab api Snapshots: %w", err)
		}
		streams = append(streams, s)
	}
	if c.web != nil {
		s, err := c.snapshotsWebDimensions(ctx, w, bridge)
		if err != nil {
			return nil, fmt.Errorf("schwab web Snapshots (dimensions): %w", err)
		}
		streams = append(streams, s)
	}
	return silver.NewConcatSnapshotStream(streams), nil
}

// snapshotsWebDimensions emits only the AccountChange rows from
// the web side (with bridged hashValues). The web silver doesn't
// carry instruments or live positions/cash; those come from api.
func (c *Connection) snapshotsWebDimensions(
	ctx context.Context,
	w canonical.Window,
	bridge map[string]string,
) (silver.SnapshotStream, error) {
	if !w.HasChanges || c.web == nil {
		return silver.NewSnapshotStream(nil), nil
	}
	const q = `SELECT snapshot_at FROM dump_runs WHERE snapshot_at BETWEEN ? AND ? ORDER BY snapshot_at`
	rows, err := c.web.db.QueryContext(ctx, q, w.Start, w.End)
	if err != nil {
		return nil, fmt.Errorf("schwab web dump_runs in window: %w", err)
	}
	defer rows.Close()
	byTime := make(map[int64]*canonical.SnapshotBatch)
	times := make([]int64, 0)
	for rows.Next() {
		var t int64
		if err := rows.Scan(&t); err != nil {
			return nil, err
		}
		byTime[t] = &canonical.SnapshotBatch{}
		times = append(times, t)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if err := c.web.snapshotsDimensions(ctx, w, byTime, bridge); err != nil {
		return nil, err
	}
	batches := make([]canonical.SnapshotBatch, 0, len(times))
	for _, t := range times {
		b := byTime[t]
		if len(b.Accounts) == 0 {
			continue
		}
		batches = append(batches, *b)
	}
	return silver.NewSnapshotStream(batches), nil
}

// Transactions emits api transactions unfiltered and (when web
// is configured) web transactions strictly older than each
// account's api-coverage-start.
func (c *Connection) Transactions(ctx context.Context, w canonical.Window) (silver.TransactionStream, error) {
	if c.web == nil {
		return c.api.Transactions(ctx, w)
	}
	bridge, err := c.ensureBridge(ctx)
	if err != nil {
		return nil, err
	}
	apiStart, err := c.ensureAPIStart(ctx)
	if err != nil {
		return nil, err
	}
	symbolBridge, err := c.ensureSymbolBridge(ctx)
	if err != nil {
		return nil, err
	}

	streams := make([]silver.TransactionStream, 0, 2)
	if c.web != nil {
		s, err := c.web.transactionsBeforeAPIStart(ctx, w, bridge, apiStart, symbolBridge)
		if err != nil {
			return nil, fmt.Errorf("schwab web Transactions: %w", err)
		}
		streams = append(streams, s)
	}
	if c.api != nil {
		s, err := c.api.Transactions(ctx, w)
		if err != nil {
			return nil, fmt.Errorf("schwab api Transactions: %w", err)
		}
		streams = append(streams, s)
	}
	return silver.NewConcatTransactionStream(streams), nil
}

// ensureBridge builds the web suffix → api hashValue map on first
// use and caches the result. Returns the cached value on
// subsequent calls; returns the cached error if the build failed.
func (c *Connection) ensureBridge(ctx context.Context) (map[string]string, error) {
	if c.bridgeBuilt {
		return c.bridge, c.bridgeErr
	}
	c.bridgeBuilt = true
	if c.api == nil || c.web == nil {
		c.bridge = map[string]string{}
		return c.bridge, nil
	}
	c.bridge, c.bridgeErr = buildAccountBridge(ctx, c.api.db, c.web.db)
	return c.bridge, c.bridgeErr
}

// ensureSymbolBridge builds the web ticker → api CUSIP map on
// first use and caches the result. Empty when api isn't
// configured (web-only mode falls back to using the ticker as
// the instrument id, with no api row to join against).
func (c *Connection) ensureSymbolBridge(ctx context.Context) (map[string]string, error) {
	if c.symbolBridgeBuilt {
		return c.symbolToCUSIP, c.symbolBridgeErr
	}
	c.symbolBridgeBuilt = true
	if c.api == nil {
		c.symbolToCUSIP = map[string]string{}
		return c.symbolToCUSIP, nil
	}
	c.symbolToCUSIP, c.symbolBridgeErr = buildSymbolToCUSIPBridge(ctx, c.api.db)
	return c.symbolToCUSIP, c.symbolBridgeErr
}

// buildSymbolToCUSIPBridge scans schwab-api silver positions for
// (symbol, cusip) pairs. When a symbol maps unambiguously to one
// CUSIP, it's added to the bridge. Symbol→CUSIP conflicts (the
// same ticker reused by Schwab for different securities over
// time, which is rare but possible) are skipped — the web side
// then falls back to the ticker, leaving the symbol column
// blank rather than mismapping.
func buildSymbolToCUSIPBridge(ctx context.Context, api *sql.DB) (map[string]string, error) {
	const q = `
SELECT DISTINCT json_extract(payload,'$.instrument.symbol') AS sym,
                json_extract(payload,'$.instrument.cusip')  AS cusip
  FROM positions
 WHERE sym  IS NOT NULL AND sym  != ''
   AND cusip IS NOT NULL AND cusip != ''`
	rows, err := api.QueryContext(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("schwab symbol bridge: %w", err)
	}
	defer rows.Close()
	candidates := map[string]map[string]struct{}{}
	for rows.Next() {
		var sym, cusip string
		if err := rows.Scan(&sym, &cusip); err != nil {
			return nil, err
		}
		if candidates[sym] == nil {
			candidates[sym] = map[string]struct{}{}
		}
		candidates[sym][cusip] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := make(map[string]string, len(candidates))
	for sym, cusips := range candidates {
		if len(cusips) == 1 {
			for c := range cusips {
				out[sym] = c
			}
		}
	}
	return out, nil
}

// buildAccountBridge reads (hashValue, accountNumber) from api
// and account suffixes from web, then resolves each web account
// in two tiers:
//
//  1. Exact — when the web payload carries a non-empty
//     `account_number_full` (the number as printed on statement
//     PDFs, e.g. "1234-5678"), both sides are reduced to their
//     digit sequences and compared for equality. The UI suffix
//     is by construction the trailing digits of the account
//     number, so a full number that doesn't end in the
//     account's own suffix can only be a mis-parsed statement
//     header and fails loudly. Two api accounts can't share a
//     full number, but the case is guarded with the same loud
//     failure as the suffix tier. A full number matching no api
//     account falls through to the suffix tier — the api roster
//     may lag the statement side.
//  2. Suffix — the web suffix is matched against the trailing
//     digits of the api accountNumbers. Fails loudly on
//     ambiguity.
//
// Uses the latest snapshot per account on the api side so a
// closed account that no longer appears in the most-recent dump
// still participates if it was historically present; on the web
// side every account ever seen participates, with the newest
// non-empty `account_number_full` winning.
func buildAccountBridge(ctx context.Context, api, web *sql.DB) (map[string]string, error) {
	const qAPI = `
SELECT a.account_external_id, a.account_number
  FROM accounts a
  JOIN (SELECT account_external_id, MAX(snapshot_at) AS s
          FROM accounts GROUP BY account_external_id) m
    ON a.account_external_id = m.account_external_id
   AND a.snapshot_at = m.s
 WHERE a.account_number IS NOT NULL`
	rows, err := api.QueryContext(ctx, qAPI)
	if err != nil {
		return nil, fmt.Errorf("schwab bridge api scan: %w", err)
	}
	defer rows.Close()
	apiAccts := make(map[string]string) // hashValue → accountNumber
	for rows.Next() {
		var hash, num string
		if err := rows.Scan(&hash, &num); err != nil {
			return nil, err
		}
		apiAccts[hash] = num
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	const qWeb = `
SELECT account_external_id,
       COALESCE(json_extract(payload, '$.account_number_full'), '')
  FROM accounts
 ORDER BY snapshot_at`
	wrows, err := web.QueryContext(ctx, qWeb)
	if err != nil {
		return nil, fmt.Errorf("schwab bridge web scan: %w", err)
	}
	defer wrows.Close()
	type webAcct struct {
		suffix string
		full   string
	}
	var webAccts []webAcct
	idx := make(map[string]int) // suffix → index into webAccts
	for wrows.Next() {
		var suffix, full string
		if err := wrows.Scan(&suffix, &full); err != nil {
			return nil, err
		}
		if i, ok := idx[suffix]; ok {
			if full != "" {
				webAccts[i].full = full
			}
			continue
		}
		idx[suffix] = len(webAccts)
		webAccts = append(webAccts, webAcct{suffix: suffix, full: full})
	}
	if err := wrows.Err(); err != nil {
		return nil, err
	}

	bridge := make(map[string]string, len(webAccts))
	for _, wa := range webAccts {
		if full := digitsOnly(wa.full); full != "" {
			// A full number that doesn't end in the account's
			// own suffix can only be a mis-parsed statement
			// header; refuse rather than rebridge the account's
			// history onto the wrong api hashValue.
			if !strings.HasSuffix(full, digitsOnly(wa.suffix)) {
				return nil, fmt.Errorf("schwab bridge: full account number for web suffix %q does not end in that suffix (suspect statement parse); refusing to bridge", wa.suffix)
			}
			var matches []string
			for hash, num := range apiAccts {
				if digitsOnly(num) == full {
					matches = append(matches, hash)
				}
			}
			if len(matches) == 1 {
				bridge[wa.suffix] = matches[0]
				continue
			}
			if len(matches) > 1 {
				return nil, fmt.Errorf("schwab bridge: web account number for suffix %q matches %d api accounts (ambiguous); add an explicit override", wa.suffix, len(matches))
			}
			// No exact counterpart — fall through to the suffix
			// heuristic.
		}
		var matches []string
		for hash, num := range apiAccts {
			if strings.HasSuffix(num, wa.suffix) {
				matches = append(matches, hash)
			}
		}
		switch len(matches) {
		case 0:
			// Web-only account (e.g. closed or web-only-enrolled).
			// Skip silently — rows referencing this suffix will be
			// dropped by the readers' bridge lookup, with no orphan
			// gold rows.
		case 1:
			bridge[wa.suffix] = matches[0]
		default:
			return nil, fmt.Errorf("schwab bridge: web suffix %q matches %d api accounts (ambiguous); add an explicit override", wa.suffix, len(matches))
		}
	}
	return bridge, nil
}

// digitsOnly reduces an account number to its digit sequence,
// dropping separators and whitespace ("1234-5678" → "12345678")
// so numbers compare equal regardless of print formatting.
func digitsOnly(s string) string {
	var b strings.Builder
	for _, r := range s {
		if '0' <= r && r <= '9' {
			b.WriteByte(byte(r))
		}
	}
	return b.String()
}

// ensureAPIStart caches the per-account-hash MIN(timestamp) from
// api.transactions. Web transactions newer than this cutoff are
// dropped (the api side covers them); web transactions older
// than this cutoff are the backfill.
//
// Accounts with no api transactions at all (cutoff absent from
// the map) get NO web-side filter — web rows pass through
// unfiltered.
func (c *Connection) ensureAPIStart(ctx context.Context) (map[string]int64, error) {
	if c.apiStartBuilt {
		return c.apiStartByHash, c.apiStartErr
	}
	c.apiStartBuilt = true
	if c.api == nil {
		c.apiStartByHash = map[string]int64{}
		return c.apiStartByHash, nil
	}
	const q = `
SELECT account_external_id, MIN(timestamp)
  FROM transactions
 GROUP BY account_external_id`
	rows, err := c.api.db.QueryContext(ctx, q)
	if err != nil {
		c.apiStartErr = fmt.Errorf("schwab apiStartByHash: %w", err)
		return nil, c.apiStartErr
	}
	defer rows.Close()
	c.apiStartByHash = map[string]int64{}
	for rows.Next() {
		var hash string
		var minTs sql.NullInt64
		if err := rows.Scan(&hash, &minTs); err != nil {
			c.apiStartErr = err
			return nil, err
		}
		if minTs.Valid {
			c.apiStartByHash[hash] = minTs.Int64
		}
	}
	c.apiStartErr = rows.Err()
	return c.apiStartByHash, c.apiStartErr
}
