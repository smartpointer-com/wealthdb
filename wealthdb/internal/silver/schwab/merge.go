package schwab

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/silver"
)

// Connection orchestrates the Schwab subsources: api, web or both.
//
// Composition rules (api + web configured):
//
//	Snapshots
//	  - Live positions, cash balances, fx rates, and instrument
//	    dimensions come from api.
//	  - Web emits AccountChange rows (rewritten to api hashValue)
//	    so its `nickname` flows into gold via per-column upsert.
//	  - Web's historical position snapshots (with their open
//	    lots) and historical cash balances stream alongside
//	    api's live ones — different
//	    snapshot timestamps (statement period-ends vs live dump
//	    times) so they coexist under the gold PK.
//
//	Transactions
//	  - Hard cut at per-account api-coverage-start
//	    (apiCoverageStart). Web emits only timestamps strictly
//	    below that cutoff, api only those at or above it.
//	    INTEROP §2 documents why a per-row merge across the
//	    boundary isn't safe.
//
//	RealizedLots
//	  - Web only: the year-end documents' lots, every tax year,
//	    with no cut at the api's coverage start (realized.go).
//
// Web alone (no api configured): the web silver's statement-date
// positions, cash balances and lots, every transaction with no cut,
// and the year-end lots, each account keyed by its full account
// number (webOnlyBridge). No live positions: those are the api's.
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

	// bridge resolves the web account_external_id (suffix) to the
	// gold account id: the api hashValue, or with web alone the full
	// account number. Built lazily on the first Snapshots/Transactions
	// call.
	bridge      map[string]string
	bridgeBuilt bool
	bridgeErr   error

	// apiStartByHash caches where each api account's history
	// begins (apiCoverageStart) for the transactions splice. Same
	// lazy-build pattern as bridge.
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

// Close releases both readers. Safe to call multiple times, and on a
// Connection Open is still assembling.
func (c *Connection) Close() error {
	var apiErr, webErr error
	if c.api != nil {
		apiErr = c.api.Close()
		c.api = nil
	}
	if c.web != nil {
		webErr = c.web.Close()
		c.web = nil
	}
	return errors.Join(apiErr, webErr)
}

// feed is a configured subsource, named for error messages, with what
// the orchestrator asks of each.
type feed struct {
	name  string
	clock interface {
		Status(context.Context) (canonical.Status, error)
		ChangeWindow(context.Context, int64) (canonical.Window, error)
	}
}

// feeds lists the configured subsources, api first.
func (c *Connection) feeds() []feed {
	var out []feed
	if c.api != nil {
		out = append(out, feed{"api", c.api})
	}
	if c.web != nil {
		out = append(out, feed{"web", c.web})
	}
	return out
}

// Status merges the per-subsource Status (silver.MergeStatuses).
func (c *Connection) Status(ctx context.Context) (canonical.Status, error) {
	var statuses []canonical.Status
	for _, f := range c.feeds() {
		s, err := f.clock.Status(ctx)
		if err != nil {
			return canonical.Status{}, fmt.Errorf("schwab %s status: %w", f.name, err)
		}
		statuses = append(statuses, s)
	}
	// Each feed reads every account of the source: it is observed
	// through the later of the two.
	return silver.MergeStatuses(statuses, true), nil
}

// ChangeWindow merges the per-subsource windows past sinceN
// (silver.MergeWindows).
func (c *Connection) ChangeWindow(ctx context.Context, sinceN int64) (canonical.Window, error) {
	var windows []canonical.Window
	for _, f := range c.feeds() {
		w, err := f.clock.ChangeWindow(ctx, sinceN)
		if err != nil {
			return canonical.Window{}, fmt.Errorf("schwab %s ChangeWindow: %w", f.name, err)
		}
		windows = append(windows, w)
	}
	return silver.MergeWindows(sinceN, windows), nil
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

	hist, err := c.web.snapshotsHistorical(ctx, w, bridge, c.api == nil)
	if err != nil {
		return nil, fmt.Errorf("schwab web Snapshots (historical): %w", err)
	}
	streams = append(streams, hist)
	// api's Snapshots also surfaces accounts. The per-column upsert in
	// gold merges web's nickname (emitted below) with api's other
	// account columns.
	if c.api != nil {
		s, err := c.api.Snapshots(ctx, w)
		if err != nil {
			return nil, fmt.Errorf("schwab api Snapshots: %w", err)
		}
		streams = append(streams, s)
	}
	dims, err := c.snapshotsWebDimensions(ctx, w, bridge)
	if err != nil {
		return nil, fmt.Errorf("schwab web Snapshots (dimensions): %w", err)
	}
	streams = append(streams, dims)
	return silver.NewConcatSnapshotStream(streams), nil
}

// snapshotsWebDimensions emits only the AccountChange rows from
// the web side (with bridged ids). The web silver carries no live
// positions or cash; those come from api.
func (c *Connection) snapshotsWebDimensions(
	ctx context.Context,
	w canonical.Window,
	bridge map[string]string,
) (silver.SnapshotStream, error) {
	if !w.HasChanges {
		return silver.NewSnapshotStream(nil), nil
	}
	times, err := dumpTimesInWindow(ctx, c.web.db, w)
	if err != nil {
		return nil, fmt.Errorf("schwab web: %w", err)
	}
	byTime := silver.NewBatches(times)
	if err := c.web.snapshotsDimensions(ctx, w, byTime, bridge, c.api == nil); err != nil {
		return nil, err
	}
	batches := slices.DeleteFunc(byTime.Sorted(), func(b canonical.SnapshotBatch) bool {
		return len(b.Accounts) == 0
	})
	return silver.NewSnapshotStream(batches), nil
}

// Transactions emits the api transactions alone when web is not
// configured. With both, each account's history splits at its
// api-coverage-start: web emits the rows strictly before it, api
// the rows from it on. With web alone, every web row is emitted,
// with no cut.
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
	webTxs, err := c.web.transactionsBeforeAPIStart(ctx, w, bridge, apiStart, symbolBridge)
	if err != nil {
		return nil, fmt.Errorf("schwab web Transactions: %w", err)
	}
	streams = append(streams, webTxs)
	if c.api != nil {
		s, err := c.api.Transactions(ctx, w)
		if err != nil {
			return nil, fmt.Errorf("schwab api Transactions: %w", err)
		}
		streams = append(streams, &apiFromCoverageStart{inner: s, start: apiStart})
	}
	return silver.NewConcatTransactionStream(streams), nil
}

// apiFromCoverageStart drops the api rows dated before their account's
// coverage start: the strays apiCoverageStart set aside, whose period
// the web side books. Where an account has no strays its start is its
// first row, so nothing is dropped.
type apiFromCoverageStart struct {
	inner silver.TransactionStream
	start map[string]int64
}

func (s *apiFromCoverageStart) Next(ctx context.Context) (canonical.TransactionBatch, bool, error) {
	batch, more, err := s.inner.Next(ctx)
	if err != nil {
		return batch, more, err
	}
	kept := batch.Transactions[:0]
	for _, tx := range batch.Transactions {
		if start, ok := s.start[tx.AccountExternalID]; ok && tx.OccurredAt < start {
			continue
		}
		kept = append(kept, tx)
	}
	batch.Transactions = kept
	return batch, more, nil
}

func (s *apiFromCoverageStart) Close() error { return s.inner.Close() }

// ensureBridge builds the web suffix → account id map on first use and
// caches the result. The id is the api hashValue, or with web alone the
// full account number. Returns the cached value on
// subsequent calls; returns the cached error if the build failed.
func (c *Connection) ensureBridge(ctx context.Context) (map[string]string, error) {
	if c.bridgeBuilt {
		return c.bridge, c.bridgeErr
	}
	c.bridgeBuilt = true
	if c.web == nil {
		c.bridge = map[string]string{}
		return c.bridge, nil
	}
	if c.api == nil {
		c.bridge, c.bridgeErr = webOnlyBridge(ctx, c.web.db)
	} else {
		c.bridge, c.bridgeErr = buildAccountBridge(ctx, c.api.db, c.web.db)
	}
	return c.bridge, c.bridgeErr
}

// ensureSymbolBridge builds the web ticker → api CUSIP map on
// first use and caches the result.
func (c *Connection) ensureSymbolBridge(ctx context.Context) (map[string]string, error) {
	if c.symbolBridgeBuilt {
		return c.symbolToCUSIP, c.symbolBridgeErr
	}
	c.symbolBridgeBuilt = true
	if c.api == nil {
		// Web alone: no CUSIPs to resolve a ticker to, so a web row keeps
		// its own key, as the historical positions do.
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

// webInstrument is the gold instrument id of a web-side key: the api
// CUSIP when the bridge knows the ticker, so the row lands on the
// instruments row the api side registered (and its symbol, name and
// asset class join in), else the key as stored. A CUSIP or an option
// contract passes through unchanged.
func webInstrument(key string, symbolToCUSIP map[string]string) string {
	if cusip, ok := symbolToCUSIP[key]; ok {
		return cusip
	}
	return key
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

	suffixes, fulls, err := webFullNumbers(ctx, web)
	if err != nil {
		return nil, err
	}

	bridge := make(map[string]string, len(suffixes))
	for _, suffix := range suffixes {
		if full := digitsOnly(fulls[suffix]); full != "" {
			// A full number that doesn't end in the account's
			// own suffix can only be a mis-parsed statement
			// header; refuse rather than rebridge the account's
			// history onto the wrong api hashValue.
			if !strings.HasSuffix(full, digitsOnly(suffix)) {
				return nil, fmt.Errorf("schwab bridge: full account number for web suffix %q does not end in that suffix (suspect statement parse); refusing to bridge", suffix)
			}
			var matches []string
			for hash, num := range apiAccts {
				if digitsOnly(num) == full {
					matches = append(matches, hash)
				}
			}
			if len(matches) == 1 {
				bridge[suffix] = matches[0]
				continue
			}
			if len(matches) > 1 {
				return nil, fmt.Errorf("schwab bridge: web account number for suffix %q matches %d api accounts, so the web account cannot be bridged", suffix, len(matches))
			}
			// No exact counterpart — fall through to the suffix
			// heuristic.
		}
		var matches []string
		for hash, num := range apiAccts {
			if strings.HasSuffix(num, suffix) {
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
			bridge[suffix] = matches[0]
		default:
			return nil, fmt.Errorf("schwab bridge: web suffix %q matches %d api accounts, so the web account cannot be bridged", suffix, len(matches))
		}
	}
	return bridge, nil
}

// webOnlyBridge maps each web suffix to its full account number, the
// account's id when the web silver is configured alone. The newest
// non-empty `account_number_full` wins, as in buildAccountBridge, and a
// full number that does not end in its suffix is refused for the same
// reason. A suffix with no full number has no id to key it by, so it
// fails the load rather than reaching gold under its last digits.
func webOnlyBridge(ctx context.Context, web *sql.DB) (map[string]string, error) {
	suffixes, fulls, err := webFullNumbers(ctx, web)
	if err != nil {
		return nil, err
	}
	bridge := make(map[string]string, len(suffixes))
	for _, suffix := range slices.Sorted(slices.Values(suffixes)) {
		number := digitsOnly(fulls[suffix])
		switch {
		case number == "":
			return nil, fmt.Errorf("schwab: web account %q states no full account number, so it has no id without the api silver", suffix)
		case !strings.HasSuffix(number, digitsOnly(suffix)):
			return nil, fmt.Errorf("schwab: full account number for web suffix %q does not end in that suffix (suspect statement parse)", suffix)
		}
		bridge[suffix] = number
	}
	return bridge, nil
}

// webFullNumbers reads every account the web silver has seen, in the
// order it first appears, and the full account number each states: the
// newest non-empty `account_number_full`, empty where none states one.
// Both bridges read it, so they agree on which number an account has.
func webFullNumbers(ctx context.Context, web *sql.DB) ([]string, map[string]string, error) {
	rows, err := web.QueryContext(ctx, `
SELECT account_external_id,
       COALESCE(json_extract(payload, '$.account_number_full'), '')
  FROM accounts
 ORDER BY snapshot_at, rowid`)
	if err != nil {
		return nil, nil, fmt.Errorf("schwab bridge web scan: %w", err)
	}
	defer rows.Close()
	var suffixes []string
	fulls := map[string]string{}
	for rows.Next() {
		var suffix, full string
		if err := rows.Scan(&suffix, &full); err != nil {
			return nil, nil, err
		}
		if _, seen := fulls[suffix]; !seen {
			suffixes = append(suffixes, suffix)
			fulls[suffix] = full
		} else if full != "" {
			fulls[suffix] = full
		}
	}
	return suffixes, fulls, rows.Err()
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

// ensureAPIStart caches, per api account hash, where the api history
// begins (apiCoverageStart). Web transactions from that instant on are
// dropped (the api side covers them); web transactions before it are
// the backfill, and api rows before it are dropped in their favour.
//
// Accounts with no api transactions at all (start absent from the
// map) get NO web-side filter — web rows pass through unfiltered.
func (c *Connection) ensureAPIStart(ctx context.Context) (map[string]int64, error) {
	if c.apiStartBuilt {
		return c.apiStartByHash, c.apiStartErr
	}
	c.apiStartBuilt = true
	c.apiStartByHash = map[string]int64{}
	if c.api == nil {
		return c.apiStartByHash, nil // web alone: no cut
	}
	apiTimes, err := accountTimestamps(ctx, c.api.db, nil)
	if err != nil {
		c.apiStartErr = fmt.Errorf("schwab apiStartByHash: %w", err)
		return nil, c.apiStartErr
	}
	webTimes := map[string][]int64{}
	if c.web != nil {
		bridge, err := c.ensureBridge(ctx)
		if err != nil {
			c.apiStartErr = err
			return nil, err
		}
		if webTimes, err = accountTimestamps(ctx, c.web.db, bridge); err != nil {
			c.apiStartErr = fmt.Errorf("schwab apiStartByHash: %w", err)
			return nil, c.apiStartErr
		}
	}
	for hash, times := range apiTimes {
		c.apiStartByHash[hash] = apiCoverageStart(times, webTimes[hash])
	}
	return c.apiStartByHash, nil
}

// accountTimestamps reads every transaction timestamp of a silver,
// ascending per account. With a bridge the web suffixes are translated
// to api hashes and an unbridged account is skipped.
func accountTimestamps(ctx context.Context, db *sql.DB, bridge map[string]string) (map[string][]int64, error) {
	rows, err := db.QueryContext(ctx, `
SELECT account_external_id, timestamp
  FROM transactions
 ORDER BY account_external_id, timestamp`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string][]int64{}
	for rows.Next() {
		var account string
		var ts int64
		if err := rows.Scan(&account, &ts); err != nil {
			return nil, err
		}
		if bridge != nil {
			hash, ok := bridge[account]
			if !ok {
				continue
			}
			account = hash
		}
		out[account] = append(out[account], ts)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// Several web suffixes can bridge to one hash; each arrived sorted
	// on its own.
	for account := range out {
		slices.Sort(out[account])
	}
	return out, nil
}

// apiStrayGap is how long an api account can fall silent after its
// first rows before those rows are suspected of predating its history.
const apiStrayGap = 30 * 86400

// apiCoverageStart returns where an api account's history begins,
// given its api and web transaction timestamps, both ascending.
//
// Normally that is its first row. The api can also return a row from
// well before its history proper — one trade from months earlier, then
// silence until the account's rows begin in earnest — and taken as the
// start, that stray hides every web row in between, which the api does
// not have. So a leading row followed by more than apiStrayGap of api
// silence, during which the web books the account as active, is a
// stray, and the history starts at the next row. The web's activity is
// the evidence: an account that was simply quiet shows none, and keeps
// its first row as its start. The evidence stops at the start of the
// next row's day, because the web stamps a row at midnight: its copy of
// that very row would otherwise count as activity in the silence.
func apiCoverageStart(api, web []int64) int64 {
	i := 0
	for i+1 < len(api) && api[i+1]-api[i] > apiStrayGap &&
		anyBetween(web, api[i], api[i+1]-api[i+1]%86400) {
		i++
	}
	return api[i]
}

// anyBetween reports whether a sorted slice holds a value strictly
// between lo and hi.
func anyBetween(sorted []int64, lo, hi int64) bool {
	j, _ := slices.BinarySearch(sorted, lo+1)
	return j < len(sorted) && sorted[j] < hi
}
