// Package loader is the IVM-style bridge that runs the
// silver→gold load sequence described in docs/DESIGN.md §8.1. It
// is the single place that touches both the silver Adapter
// interface and the gold *sql.DB; cmd/wealthdb's subcommands call
// in here for `wealthdb load` and `wealthdb reset`.
package loader

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"sort"
	"strings"
	"time"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/gold"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/silver"
)

// SourceSpec identifies one silver source. Comes from the wealthdb
// config file's silver_sources[] entries.
type SourceSpec struct {
	ID   string // user-defined silver_source_id
	Kind string // adapter kind ("schwab", "ubs", "swissquote", ...)
	Path string // filesystem path to the silver SQLite (single-file adapters)
	// Subsources, when set, replaces Path for adapters that merge
	// several backing silvers under one logical source (UBS =
	// ubs-web + ubs-psn). Passed through to silver.OpenSpec.
	Subsources []silver.Subsource
	// Relationships pairs cross-subsource entity identities. Used
	// by the UBS adapter to align web banking_relationship_id with
	// PSN SFTP relationship_id under a single label.
	Relationships []silver.RelationshipPair
	// Overrides is the per-account_external_id override map for
	// this source — nickname / category values from the
	// config-file `account_overrides` block. Loader applies these
	// after adapters have stamped their own values; config wins on
	// overlap. nil or empty entries are no-ops.
	Overrides map[string]AccountOverride
	// PortfolioOverrides is the per-portfolio_external_id override
	// map for this source. Applied to every account whose
	// PortfolioExternalID matches, BEFORE the per-account
	// Overrides — so an account-level override always wins over
	// a portfolio-level one on the same column.
	PortfolioOverrides map[string]PortfolioOverride
	// TaxableWrapper rewrites `taxable_personal` on this source's
	// accounts, BEFORE both override maps above; see
	// config.SilverSource.TaxableWrapper.
	TaxableWrapper string
	// InstrumentOverrides is the per-instrument_external_id
	// override map for this source — asset_class values from the
	// config-file `instrument_overrides` block. Applied after the
	// adapter has classified, to the instrument dimension and to
	// every position row referencing the instrument.
	InstrumentOverrides map[string]InstrumentOverride
	// TransactionInstruments links a row the adapter could not
	// resolve, keyed by the token it looked up and failed on
	// (canonical.TransactionChange.InstrumentHint, and the realized
	// lot's field of the same name).
	TransactionInstruments map[string]string
	// Supersession ends this source's account at a date because
	// another source carries it from there (config `supersession`),
	// as Unix seconds at UTC midnight per account_external_id. Rows
	// dated on or after it are dropped before they reach gold —
	// positions, lots, cash balances and transactions alike — so the two
	// sources tile instead of double-counting. Dropping alone would
	// not end the series: gold carries a key forward until something
	// supersedes it, so one zero row is also written AT the date for
	// every key the account still held at its last pre-handover
	// snapshot (see supersessionClosing). Empty is a no-op.
	//
	// The drop is of what the ADAPTER read. A TransferLedger row past
	// the date is refused instead of dropped, because it is written by
	// hand rather than read from a statement — see
	// rejectSupersededTransfers.
	Supersession map[string]int64
	// TransferLedger holds this source's rows from the optional
	// equity-transfer ledger (config `equity_transfers`). The loader
	// injects each as a canonical transfer_in/transfer_out transaction
	// after the adapter's own transactions, replacing any previously
	// injected ledger rows. Empty ⇒ nothing to inject.
	TransferLedger []TransferEntry
}

// AccountOverride is the loader's view of one config-file
// account_overrides entry. An empty string means "don't override
// that column". TaxWrapper and ManagementStyle must be valid
// canonical enum values when non-empty — config validation
// catches bad values upstream.
type AccountOverride struct {
	Nickname        string
	Category        string
	TaxWrapper      string
	ManagementStyle string
	// Exclude drops the account and every fact keyed to it. See the
	// config package's AccountOverride for what it is for and why
	// nothing infers it.
	Exclude bool
}

// PortfolioOverride is the loader's view of one config-file
// portfolio_overrides entry. TaxWrapper is the only dimension
// wired through today; empty = no override.
type PortfolioOverride struct {
	TaxWrapper string
	// Exclude drops the portfolio and every account inside it. See
	// the config package's PortfolioOverride for what it is for.
	Exclude bool
}

// InstrumentOverride is the loader's view of one config-file
// instrument_overrides entry: the 2-D taxonomy pair (exposure +
// vehicle), validated upstream at config-load time.
type InstrumentOverride struct {
	AssetClass string
	Vehicle    string
}

// LoadResult summarises one Load call. Populated even when no
// changes were applied (AlreadyUpToDate == true) so callers can
// report progress for `wealthdb status` / `wealthdb load` output.
type LoadResult struct {
	SourceID           string
	AlreadyUpToDate    bool
	ChangeNumberBefore int64 // -1 on first-ever load
	ChangeNumberAfter  int64
	Window             canonical.Window
	SnapshotsLoaded    int
	TransactionsLoaded int
	// RealizedLotsLoaded counts the realized_lots rows the load wrote;
	// it is not part of load_audit, which records the windowed streams.
	RealizedLotsLoaded int
}

// Loader holds the gold *sql.DB. One Loader per process; safe to
// reuse across multiple Load calls (each opens its own transaction).
type Loader struct {
	gold *sql.DB
}

// New returns a Loader that writes into the given gold DB.
func New(goldDB *sql.DB) *Loader { return &Loader{gold: goldDB} }

// ErrSilverWentBackwards is returned by Load when the silver
// source reports a LatestChangeNumber strictly less than the
// stored high_watermark. Per DESIGN.md §8.5 `wealthdb reset <id>`
// must be run before re-loading.
var ErrSilverWentBackwards = errors.New("silver went backwards relative to stored watermark; run `wealthdb reset` to re-sync")

// Load runs the full IVM sequence for one silver source. See
// docs/DESIGN.md §8.1 for the step-by-step. Everything happens
// inside a single gold transaction; failures roll back cleanly.
func (l *Loader) Load(ctx context.Context, spec SourceSpec) (*LoadResult, error) {
	adapter, err := silver.Get(spec.Kind)
	if err != nil {
		return nil, fmt.Errorf("Load(%s): %w", spec.ID, err)
	}
	conn, err := adapter.Open(ctx, silver.OpenSpec{
		Path:          spec.Path,
		Subsources:    spec.Subsources,
		Relationships: spec.Relationships,
	})
	if err != nil {
		return nil, fmt.Errorf("Load(%s): open silver: %w", spec.ID, err)
	}
	defer conn.Close()

	status, err := conn.Status(ctx)
	if err != nil {
		return nil, fmt.Errorf("Load(%s): silver Status: %w", spec.ID, err)
	}

	tx, err := l.gold.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("Load(%s): begin gold tx: %w", spec.ID, err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()

	// Two clocks: seconds-grain for silver_sources columns
	// (matches every other timestamp in gold) and nanoseconds-
	// grain for load_audit.loaded_at (so the PK can't collide
	// across back-to-back loads in the same wall-clock second).
	nowSec := time.Now().UTC().Unix()
	nowNano := time.Now().UTC().UnixNano()
	watermark, err := upsertSilverSource(ctx, tx, spec, nowSec)
	if err != nil {
		return nil, fmt.Errorf("Load(%s): upsert silver_sources: %w", spec.ID, err)
	}

	res := &LoadResult{
		SourceID:           spec.ID,
		ChangeNumberBefore: watermark,
		ChangeNumberAfter:  watermark,
	}

	switch {
	case status.LatestChangeNumber == watermark:
		// Nothing to do; commit so silver_sources.last_loaded_at
		// still advances (proof we ran).
		res.AlreadyUpToDate = true
		if err := tx.Commit(); err != nil {
			return nil, fmt.Errorf("Load(%s): commit no-op: %w", spec.ID, err)
		}
		committed = true
		return res, nil

	case status.LatestChangeNumber < watermark:
		// Don't commit; the rollback in defer cleans up. The error
		// signals to the caller that user intervention is needed.
		return nil, fmt.Errorf("Load(%s): silver change number %d < watermark %d: %w",
			spec.ID, status.LatestChangeNumber, watermark, ErrSilverWentBackwards)
	}

	window, err := conn.ChangeWindow(ctx, watermark)
	if err != nil {
		return nil, fmt.Errorf("Load(%s): ChangeWindow: %w", spec.ID, err)
	}
	res.Window = window
	res.ChangeNumberAfter = window.NewChangeNumber

	if window.HasChanges {
		if err := deleteWindow(ctx, tx, spec.ID, window); err != nil {
			return nil, fmt.Errorf("Load(%s): delete window: %w", spec.ID, err)
		}

		nSnap, err := applySnapshots(ctx, tx, conn, window, spec)
		if err != nil {
			return nil, fmt.Errorf("Load(%s): apply snapshots: %w", spec.ID, err)
		}
		res.SnapshotsLoaded = nSnap

		nTx, err := applyTransactions(ctx, tx, conn, window, spec)
		if err != nil {
			return nil, fmt.Errorf("Load(%s): apply transactions: %w", spec.ID, err)
		}
		res.TransactionsLoaded = nTx

		nLots, err := replaceRealizedLots(ctx, tx, conn, spec)
		if err != nil {
			return nil, fmt.Errorf("Load(%s): realized lots: %w", spec.ID, err)
		}
		res.RealizedLotsLoaded = nLots

		// Excluded accounts and portfolios are swept once every stream
		// has drained — see deleteExcluded for why that is the only
		// point at which the sweep can see what it has to remove.
		if err := deleteExcluded(ctx, tx, spec.ID,
			configExclusions(spec.Overrides, spec.PortfolioOverrides)); err != nil {
			return nil, fmt.Errorf("Load(%s): %w", spec.ID, err)
		}

		// A ledger row dated past a superseded account's handover is
		// refused rather than dropped the way the adapter's rows are, and
		// refused before any of the ledger is written — see
		// rejectSupersededTransfers.
		if err := rejectSupersededTransfers(ctx, tx, spec); err != nil {
			return nil, fmt.Errorf("Load(%s): %w", spec.ID, err)
		}

		// Inject the optional equity-transfer ledger as canonical transfer
		// transactions, replacing this source's prior ledger rows. The
		// delete+insert spans the whole ledger (rows are dated outside the
		// change window), so any load that has changes — and every `reload` —
		// re-applies edits to the CSV.
		nLedger, err := applyTransferLedger(ctx, tx, spec.ID, spec.TransferLedger)
		if err != nil {
			return nil, fmt.Errorf("Load(%s): %w", spec.ID, err)
		}
		res.TransactionsLoaded += nLedger

		if err := insertLoadAudit(ctx, tx, spec.ID, nowNano, watermark, res); err != nil {
			return nil, fmt.Errorf("Load(%s): insert load_audit: %w", spec.ID, err)
		}
	}

	if err := updateWatermark(ctx, tx, spec.ID, window.NewChangeNumber, nowSec); err != nil {
		return nil, fmt.Errorf("Load(%s): update watermark: %w", spec.ID, err)
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("Load(%s): commit: %w", spec.ID, err)
	}
	committed = true
	return res, nil
}

// upsertSilverSource returns the existing high_watermark for the
// source, or inserts a fresh row (with watermark = -1) for a
// first-ever load. Always refreshes silver_path / silver_kind to
// the values from the current config so a moved/renamed silver is
// picked up automatically.
func upsertSilverSource(ctx context.Context, tx *sql.Tx, spec SourceSpec, now int64) (int64, error) {
	var existing int64
	err := tx.QueryRowContext(ctx,
		`SELECT high_watermark FROM silver_sources WHERE silver_source_id = ?`,
		spec.ID,
	).Scan(&existing)

	if err == sql.ErrNoRows {
		_, err := tx.ExecContext(ctx, `
            INSERT INTO silver_sources(
                silver_source_id, silver_kind, silver_path,
                high_watermark, first_loaded_at, last_loaded_at
            ) VALUES (?, ?, ?, ?, ?, ?)
        `, spec.ID, spec.Kind, spec.Path, int64(-1), now, now)
		return -1, err
	}
	if err != nil {
		return 0, err
	}

	// Existing row — refresh path/kind in case the silver
	// path moved. last_loaded_at is bumped by updateWatermark at the
	// end of the transaction.
	if _, err := tx.ExecContext(ctx, `
        UPDATE silver_sources
           SET silver_kind = ?, silver_path = ?
         WHERE silver_source_id = ?
    `, spec.Kind, spec.Path, spec.ID); err != nil {
		return 0, err
	}
	return existing, nil
}

// deleteWindow removes any existing gold rows whose key time
// (snapshot_at for snapshot tables, occurred_at for transactions)
// falls inside the window. Required before re-inserting the new
// change records.
func deleteWindow(ctx context.Context, tx *sql.Tx, sourceID string, w canonical.Window) error {
	tables := []struct {
		name, timeCol string
	}{
		{"positions", "snapshot_at"},
		{"position_lots", "snapshot_at"},
		{"cash_balances", "snapshot_at"},
		{"fx_rates", "snapshot_at"},
		{"transactions", "occurred_at"},
	}
	for _, t := range tables {
		q := fmt.Sprintf(
			`DELETE FROM %s WHERE silver_source_id = ? AND %s BETWEEN ? AND ?`,
			t.name, t.timeCol,
		)
		if _, err := tx.ExecContext(ctx, q, sourceID, w.Start, w.End); err != nil {
			return fmt.Errorf("delete from %s: %w", t.name, err)
		}
	}
	return nil
}

// applySnapshots drains conn.Snapshots into the gold writer.
// Fact rows (positions, lots, cash, fx) are written per batch, plus a
// closing set once the stream drains (below); dimension rows
// (portfolios, accounts, instruments) fold into a
// gold.ChangeAccumulator and upsert once after the stream drains —
// adapters re-emit dimension rows alongside every snapshot, so
// folding them to one record per entity removes the bulk of the
// gold statements a load runs. Gold declares no FKs (DuckDB can't
// defer), so facts landing before their dimensions is fine within
// the transaction. Returns the total count of fact rows written.
//
// The spec's config statements (any may be empty) are applied after
// stamping, widest scope first so the narrower always wins on the same
// column: the source-wide taxable wrapper, then portfolio overrides,
// then per-account ones, all to AccountChange records; instrument
// overrides to InstrumentChange and PositionChange records. See
// applyTaxableWrapper, applyPortfolioOverrides, applyAccountOverrides
// and applyInstrumentOverrides. Then dropSuperseded removes the rows
// of an account another source carries from a date on — after the
// rest, so it drops finished records rather than leaving half-dressed
// ones behind. Last, once the stream drains, comes the closing write:
// one zero row at each superseded account's handover date for every
// key it still held going in, so the old mark cannot carry forward
// past the handover (see supersessionClosing). Those rows are
// synthesised rather than read from silver, and count toward the
// total returned; deleteClosingRows clears the previous load's set
// first, because the delete that opens the load may not reach them.
func applySnapshots(ctx context.Context, tx *sql.Tx, conn silver.Connection, w canonical.Window, spec SourceSpec) (int, error) {
	stream, err := conn.Snapshots(ctx, w)
	if err != nil {
		return 0, err
	}
	defer stream.Close()

	writer := gold.NewWriter(tx)
	dims := gold.NewChangeAccumulator()
	closing := newSupersessionClosing(spec.Supersession)
	total := 0
	for {
		batch, more, err := stream.Next(ctx)
		if err != nil {
			return total, err
		}

		// Stamp the silver_source_id on every record before write.
		stampSnapshotBatch(&batch, spec.ID)
		// Config-file statements go on top of whatever the adapter
		// emitted; see DESIGN.md §13.9. Widest scope first, so a named
		// account still overrules the blanket rule on the same column.
		applyTaxableWrapper(batch.Accounts, spec.TaxableWrapper)
		applyPortfolioOverrides(batch.Accounts, spec.PortfolioOverrides)
		applyAccountOverrides(batch.Accounts, spec.Overrides)
		applyInstrumentOverrides(batch.Instruments, batch.Positions, spec.InstrumentOverrides)
		// Before the drop, record what each superseded account held
		// going into its handover — read after the statements above, so
		// the zero row carries the same dressed keys as the row it ends.
		closing.observe(batch.Positions, batch.CashBalances)
		// Last of the per-batch statements, so it drops rows the ones
		// above have finished dressing rather than leaving half-applied
		// ones behind.
		batch.Positions = dropSuperseded(batch.Positions, spec.Supersession,
			func(p canonical.PositionChange) (string, int64) {
				return p.AccountExternalID, p.SnapshotAt
			})
		batch.PositionLots = dropSuperseded(batch.PositionLots, spec.Supersession,
			func(l canonical.PositionLotChange) (string, int64) {
				return l.AccountExternalID, l.SnapshotAt
			})
		batch.CashBalances = dropSuperseded(batch.CashBalances, spec.Supersession,
			func(c canonical.CashBalanceChange) (string, int64) {
				return c.AccountExternalID, c.SnapshotAt
			})

		if err := dims.AddBatch(&batch); err != nil {
			return total, err
		}
		if err := writer.InsertPositions(ctx, batch.Positions); err != nil {
			return total, err
		}
		if err := writer.InsertPositionLots(ctx, batch.PositionLots); err != nil {
			return total, err
		}
		if err := writer.InsertCashBalances(ctx, batch.CashBalances); err != nil {
			return total, err
		}
		if err := writer.InsertFxRates(ctx, batch.FxRates); err != nil {
			return total, err
		}

		total += len(batch.Positions) + len(batch.PositionLots) + len(batch.CashBalances) + len(batch.FxRates)

		if !more {
			positions, balances := closing.rows(spec.ID)
			if err := deleteClosingRows(ctx, tx, spec.ID, positions, balances); err != nil {
				return total, err
			}
			if err := writer.InsertPositions(ctx, positions); err != nil {
				return total, err
			}
			if err := writer.InsertCashBalances(ctx, balances); err != nil {
				return total, err
			}
			total += len(positions) + len(balances)
			return total, dims.Flush(ctx, writer)
		}
	}
}

// deleteClosingRows removes the rows this load is about to rewrite at
// a handover date, ahead of writing that date's zeros again.
//
// The window delete that opens a load spans what silver itself
// reports, and a superseded source's data can stop before the date
// another source takes the account over — so the zeros the last load
// wrote at the handover can sit outside every window this source will
// ever report, and re-inserting them would collide on the primary key.
// Nothing else of this source's can be dated there: dropSuperseded has
// already cut everything from the handover on.
//
// It deletes exactly what this load recomputed, per account and per
// table, never the whole configured set. The closing rows are a
// function of the source's PRE-handover history, not of the change
// window, so an incremental window that opens after the handover
// recomputes nothing and must leave the standing zeros alone — wiping
// them there would let the last pre-handover mark carry forward again,
// which is the double-count the feature exists to prevent. Per-table
// scoping matters for the same reason: a source whose cash series ends
// before its positions produces position zeros and no balance zeros in
// one load, and the standing cash zero must survive it. Retiring a zero
// for an account that has left silver altogether is `reload`'s job.
func deleteClosingRows(
	ctx context.Context,
	tx *sql.Tx,
	sourceID string,
	positions []canonical.PositionChange,
	balances []canonical.CashBalanceChange,
) error {
	if err := deleteClosing(ctx, tx, sourceID, "positions", positions,
		func(p canonical.PositionChange) (string, int64) {
			return p.AccountExternalID, p.SnapshotAt
		}); err != nil {
		return err
	}
	return deleteClosing(ctx, tx, sourceID, "cash_balances", balances,
		func(b canonical.CashBalanceChange) (string, int64) {
			return b.AccountExternalID, b.SnapshotAt
		})
}

// deleteClosing issues one DELETE per (account, snapshot) pair the rows
// name, deduped so the table is hit once per pair.
//
// Generic over the fact kinds for the same reason dropSuperseded is:
// both tables are addressed by the same pair of fields, and `at` reads
// that pair off a row. The dedup key mirrors the DELETE predicate
// rather than the caller's per-account cutoff, so it stays correct by
// local inspection if a caller ever names more than one date per
// account.
func deleteClosing[T any](ctx context.Context, tx *sql.Tx, sourceID, table string,
	rows []T, at func(T) (string, int64)) error {
	q := fmt.Sprintf(
		`DELETE FROM %s WHERE silver_source_id = ? AND account_external_id = ? AND snapshot_at = ?`,
		table,
	)
	seen := map[[2]any]bool{}
	for _, row := range rows {
		account, when := at(row)
		k := [2]any{account, when}
		if seen[k] {
			continue
		}
		seen[k] = true
		if _, err := tx.ExecContext(ctx, q, sourceID, account, when); err != nil {
			return fmt.Errorf("delete closing rows from %s: %w", table, err)
		}
	}
	return nil
}

// supersessionClosing builds the zero rows that END a superseded
// account's series.
//
// Dropping an account's rows from the handover on is only half of it.
// Gold carries a key forward until something supersedes it, and a
// source that simply stops reporting supersedes nothing — so the last
// mark before the handover would linger for up to hist_carry_days,
// double-counting against whatever carries the account now. An explicit
// zero ends a key immediately, which is what an adapter emits on
// closure, so that is what this synthesises: at the handover date, one
// zero for every key the account still held going into it.
//
// "Still held" is read off the account's LAST snapshot before the
// handover, not off everything it ever held — a key already gone by
// then has already ended, and re-zeroing it would resurrect it for a
// day.
//
// Positions and cash keep separate clocks: gold resolves the two on
// their own tables, so a cash row dated after the last positions
// snapshot ends no position, and a position row after the last cash
// snapshot ends no balance. One clock would let either silence the
// other's zeros.
type supersessionClosing struct {
	ends     map[string]int64
	atKeys   map[string]int64                                      // account -> its last positions snapshot before the handover
	atBals   map[string]int64                                      // account -> its last cash snapshot before the handover
	keys     map[string]map[string]canonical.PositionChange        // account -> position key -> the row to zero
	balances map[string]map[balanceKey]canonical.CashBalanceChange // account -> currency+kind -> the row to zero
}

// balanceKey is gold's identity for a cash series minus the account:
// cash_balances is keyed on (source, snapshot, account, currency,
// balance_kind), and the carry-forward runs per currency. Collecting by
// kind alone would keep one currency of an account and leave every
// other one carrying past the handover.
type balanceKey struct {
	currency string
	kind     canonical.BalanceKind
}

func newSupersessionClosing(ends map[string]int64) *supersessionClosing {
	return &supersessionClosing{
		ends:     ends,
		atKeys:   map[string]int64{},
		atBals:   map[string]int64{},
		keys:     map[string]map[string]canonical.PositionChange{},
		balances: map[string]map[balanceKey]canonical.CashBalanceChange{},
	}
}

// observe records what each superseded account held at the latest
// snapshot it has been seen at so far — per fact kind, each on its own
// clock — discarding an earlier one when a later (but still
// pre-handover) snapshot arrives.
func (c *supersessionClosing) observe(positions []canonical.PositionChange,
	balances []canonical.CashBalanceChange) {
	if len(c.ends) == 0 {
		return
	}
	for _, p := range positions {
		account := p.AccountExternalID
		if !c.advance(c.atKeys, account, p.SnapshotAt, func() {
			c.keys[account] = map[string]canonical.PositionChange{}
		}) {
			continue
		}
		c.keys[account][p.PositionKey] = p
	}
	for _, b := range balances {
		account := b.AccountExternalID
		if !c.advance(c.atBals, account, b.SnapshotAt, func() {
			c.balances[account] = map[balanceKey]canonical.CashBalanceChange{}
		}) {
			continue
		}
		c.balances[account][balanceKey{b.Currency, b.BalanceKind}] = b
	}
}

// advance reports whether a row belongs to the account's newest
// pre-handover snapshot on the given clock, calling reset to discard
// what was collected for an older one.
func (c *supersessionClosing) advance(clock map[string]int64, account string, at int64, reset func()) bool {
	cutoff, superseded := c.ends[account]
	if !superseded || at >= cutoff {
		return false
	}
	seen, ok := clock[account]
	if ok && at < seen {
		return false
	}
	if !ok || at > seen {
		clock[account] = at
		reset()
	}
	return true
}

// supersessionMarkerPayload replaces the payload of the row a zero was
// built from. Every other value-bearing field is zeroed or dropped, and
// a payload restating the pre-handover holding would be the one place
// the row still claimed it; the marker also makes it recognisable in
// gold as synthesised rather than read. Mirrors silver's closure marker.
const supersessionMarkerPayload = `{"supersession_marker": true}`

// rows returns the zeroed positions and balances to write at each
// superseded account's handover date. An account the stream never
// carried contributes nothing — there is no series to end.
func (c *supersessionClosing) rows(sourceID string) ([]canonical.PositionChange, []canonical.CashBalanceChange) {
	var positions []canonical.PositionChange
	var balances []canonical.CashBalanceChange
	zero := canonical.NewDecimalFromInt(0)
	for account, cutoff := range c.ends {
		for _, p := range c.keys[account] {
			p.SilverSourceID = sourceID
			p.SnapshotAt = cutoff
			q, v := zero, zero
			p.Quantity, p.MarketValue = &q, &v
			p.SetBookValue(nil, canonical.Basis{})
			p.AccruedInterest, p.AcquisitionDate = nil, nil
			p.Payload = json.RawMessage(supersessionMarkerPayload)
			positions = append(positions, p)
		}
		for _, b := range c.balances[account] {
			b.SilverSourceID = sourceID
			b.SnapshotAt = cutoff
			b.Amount = zero
			b.Payload = json.RawMessage(supersessionMarkerPayload)
			balances = append(balances, b)
		}
	}
	return positions, balances
}

// applyTransactions drains conn.Transactions into the gold writer.
func applyTransactions(ctx context.Context, tx *sql.Tx, conn silver.Connection, w canonical.Window, spec SourceSpec) (int, error) {
	stream, err := conn.Transactions(ctx, w)
	if err != nil {
		return 0, err
	}
	defer stream.Close()

	writer := gold.NewWriter(tx)
	total := 0
	for {
		batch, more, err := stream.Next(ctx)
		if err != nil {
			return total, err
		}
		stampTransactionBatch(&batch, spec.ID)
		// Config-file overrides go on top of whatever the adapter
		// emitted, as they do on the snapshot side; see DESIGN.md §13.9.
		applyTransactionInstruments(batch.Transactions, spec.TransactionInstruments)
		batch.Transactions = dropSuperseded(batch.Transactions, spec.Supersession,
			func(x canonical.TransactionChange) (string, int64) {
				return x.AccountExternalID, x.OccurredAt
			})
		if err := writer.InsertTransactions(ctx, batch.Transactions); err != nil {
			return total, err
		}
		total += len(batch.Transactions)
		if !more {
			return total, nil
		}
	}
}

// replaceRealizedLots rewrites the source's realized_lots rows from the
// adapter's RealizedLots, when the adapter offers them. The table is
// not windowed (silver.RealizedLotReader says why), so the previous
// rows go first, whole. The loader's statements apply as they do to
// transactions: a config link names an instrument the adapter could
// only hint at, and a lot disposed on or after a superseded account's
// handover is the successor's to state. A lot dated by neither its
// disposal nor its settlement stands at the start of its tax year.
func replaceRealizedLots(ctx context.Context, tx *sql.Tx, conn silver.Connection, spec SourceSpec) (int, error) {
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM realized_lots WHERE silver_source_id = ?`, spec.ID); err != nil {
		return 0, fmt.Errorf("clear prior: %w", err)
	}
	reader, ok := conn.(silver.RealizedLotReader)
	if !ok {
		return 0, nil
	}
	lots, err := reader.RealizedLots(ctx)
	if err != nil {
		return 0, err
	}
	for i := range lots {
		r := &lots[i]
		r.SilverSourceID = spec.ID
		linkHint(&r.InstrumentExternalID, r.InstrumentHint, spec.TransactionInstruments)
	}
	lots = dropSuperseded(lots, spec.Supersession,
		func(r canonical.RealizedLotChange) (string, int64) {
			switch {
			case r.DisposalDate != nil:
				return r.AccountExternalID, r.DisposalDate.Unix()
			case r.SettlementDate != nil:
				return r.AccountExternalID, r.SettlementDate.Unix()
			}
			return r.AccountExternalID, time.Date(r.TaxYear, 1, 1, 0, 0, 0, 0, time.UTC).Unix()
		})
	if err := gold.NewWriter(tx).InsertRealizedLots(ctx, lots); err != nil {
		return 0, err
	}
	return len(lots), nil
}

func insertLoadAudit(ctx context.Context, tx *sql.Tx, sourceID string, now, watermarkBefore int64, res *LoadResult) error {
	var beforeArg any = watermarkBefore
	if watermarkBefore < 0 {
		// First-ever load gets NULL change_number_before per
		// the schema in DESIGN.md §7.2.
		beforeArg = nil
	}
	_, err := tx.ExecContext(ctx, `
        INSERT INTO load_audit(
            silver_source_id, loaded_at,
            change_number_before, change_number_after,
            window_start, window_end,
            snapshots_loaded, transactions_loaded
        ) VALUES (?, ?, ?, ?, ?, ?, ?, ?)
    `,
		sourceID, now,
		beforeArg, res.ChangeNumberAfter,
		res.Window.Start, res.Window.End,
		res.SnapshotsLoaded, res.TransactionsLoaded,
	)
	return err
}

func updateWatermark(ctx context.Context, tx *sql.Tx, sourceID string, newWatermark, now int64) error {
	_, err := tx.ExecContext(ctx, `
        UPDATE silver_sources
           SET high_watermark = ?, last_loaded_at = ?
         WHERE silver_source_id = ?
    `, newWatermark, now, sourceID)
	return err
}

// stampSnapshotBatch fills in the SilverSourceID on every record
// in a batch. Adapters intentionally leave this empty so they
// don't need to know their own registered name; the loader is
// authoritative.
func stampSnapshotBatch(b *canonical.SnapshotBatch, sourceID string) {
	for i := range b.Portfolios {
		b.Portfolios[i].SilverSourceID = sourceID
	}
	for i := range b.Accounts {
		b.Accounts[i].SilverSourceID = sourceID
	}
	for i := range b.Instruments {
		b.Instruments[i].SilverSourceID = sourceID
	}
	for i := range b.Positions {
		b.Positions[i].SilverSourceID = sourceID
	}
	for i := range b.PositionLots {
		b.PositionLots[i].SilverSourceID = sourceID
	}
	for i := range b.CashBalances {
		b.CashBalances[i].SilverSourceID = sourceID
	}
	for i := range b.FxRates {
		b.FxRates[i].SilverSourceID = sourceID
	}
}

func stampTransactionBatch(b *canonical.TransactionBatch, sourceID string) {
	for i := range b.Transactions {
		b.Transactions[i].SilverSourceID = sourceID
	}
}

// exclusions is what the config removes from gold for one source:
// accounts named outright (config.AccountOverride.Exclude) and whole
// portfolios (config.PortfolioOverride.Exclude), whose member accounts
// the data names rather than the config.
type exclusions struct {
	accounts   []string
	portfolios []string
}

func (e exclusions) empty() bool { return len(e.accounts) == 0 && len(e.portfolios) == 0 }

// configExclusions collects both grains, sorted so the SQL the sweep
// builds is stable across runs (Go map order is not).
func configExclusions(accounts map[string]AccountOverride, portfolios map[string]PortfolioOverride) exclusions {
	var e exclusions
	for id, ov := range accounts {
		if ov.Exclude {
			e.accounts = append(e.accounts, id)
		}
	}
	for id, ov := range portfolios {
		if ov.Exclude {
			e.portfolios = append(e.portfolios, id)
		}
	}
	sort.Strings(e.accounts)
	sort.Strings(e.portfolios)
	return e
}

// deleteExcluded removes an excluded account or portfolio from gold —
// the dimension rows and every fact keyed to them — and logs what went.
//
// It sweeps gold after the streams have drained rather than filtering
// them, and both grains do, because neither is decidable earlier. A
// portfolio's membership lives in the account dimension: a position, a
// lot, a cash balance and a transaction each name an account and never
// a portfolio, and an adapter may emit its facts before the dimension
// rows that would place them. An account is decidable per row, but a
// filter reaches only the rows this load happens to write — an account
// already in gold when the exclusion is added would sit there
// untouched, since gold's dimensions are upserted and never expire.
// Sweeping covers both, and covers the grains identically.
//
// The dimension and its facts must go together: a fact whose account
// gold has no record of is an orphan, outside every account-scoped
// filter and attached to no portfolio — the state this mechanism exists
// to remove, not to create. So order is load-bearing: a portfolio's
// facts are found THROUGH the accounts table and the accounts must
// still be there when they are deleted, and the portfolio row goes
// last.
func deleteExcluded(ctx context.Context, tx *sql.Tx, sourceID string, e exclusions) error {
	if e.empty() {
		return nil
	}
	// Two predicates over the same column: accounts named by the
	// config, and accounts the data places inside an excluded
	// portfolio. Either may be empty, so each is contributed only when
	// it has ids behind it — an `IN ()` is a syntax error, and an
	// always-false stand-in would read as a deliberate no-op.
	var terms []string
	args := []any{sourceID}
	if len(e.accounts) > 0 {
		terms = append(terms, "account_external_id IN ("+placeholders(len(e.accounts))+")")
		args = append(args, ids(e.accounts)...)
	}
	if len(e.portfolios) > 0 {
		terms = append(terms, `account_external_id IN (
                   SELECT account_external_id FROM accounts
                    WHERE silver_source_id = ?
                      AND portfolio_external_id IN (`+placeholders(len(e.portfolios))+`))`)
		args = append(args, sourceID)
		args = append(args, ids(e.portfolios)...)
	}
	factWhere := "silver_source_id = ? AND (" + strings.Join(terms, " OR ") + ")"

	total := 0
	exec := func(q string, a ...any) error {
		res, err := tx.ExecContext(ctx, q, a...)
		if err != nil {
			return err
		}
		// A driver that cannot count leaves the log short rather than
		// failing a load over a number nothing reads back.
		if n, err := res.RowsAffected(); err == nil {
			total += int(n)
		}
		return nil
	}

	for _, table := range []string{"positions", "position_lots", "cash_balances", "transactions", "realized_lots"} {
		if err := exec("DELETE FROM "+table+" WHERE "+factWhere, args...); err != nil {
			return fmt.Errorf("delete excluded from %s: %w", table, err)
		}
	}
	if err := exec("DELETE FROM accounts WHERE "+factWhere, args...); err != nil {
		return fmt.Errorf("delete excluded accounts: %w", err)
	}
	if len(e.portfolios) > 0 {
		pfArgs := append([]any{sourceID}, ids(e.portfolios)...)
		if err := exec(`DELETE FROM portfolios
             WHERE silver_source_id = ? AND portfolio_external_id IN (`+
			placeholders(len(e.portfolios))+`)`, pfArgs...); err != nil {
			return fmt.Errorf("delete excluded portfolios: %w", err)
		}
	}
	if total > 0 {
		log.Printf("%s loader: dropped %d row(s) on %d excluded account(s) and %d excluded portfolio(s) (account_overrides / portfolio_overrides)",
			sourceID, total, len(e.accounts), len(e.portfolios))
	}
	return nil
}

// placeholders is `?, ?, …` for an IN list of n ids.
func placeholders(n int) string {
	return strings.TrimSuffix(strings.Repeat("?, ", n), ", ")
}

// ids widens a string slice to the `any` slice ExecContext takes.
func ids(in []string) []any {
	out := make([]any, len(in))
	for i, v := range in {
		out[i] = v
	}
	return out
}

// applyPortfolioOverrides patches each AccountChange whose
// PortfolioExternalID appears in the overrides map. Today only
// tax_wrapper is wired through. Accounts without a
// PortfolioExternalID (the orphan / standalone-account form most
// non-portfolio-shaped adapters use) are skipped — no key to
// match against.
//
// Called BEFORE applyAccountOverrides so per-account overrides on
// the same column win.
func applyPortfolioOverrides(accounts []canonical.AccountChange, overrides map[string]PortfolioOverride) {
	if len(overrides) == 0 {
		return
	}
	for i := range accounts {
		if accounts[i].PortfolioExternalID == nil {
			continue
		}
		ov, ok := overrides[*accounts[i].PortfolioExternalID]
		if !ok {
			continue
		}
		if ov.TaxWrapper != "" {
			w := canonical.TaxWrapper(ov.TaxWrapper)
			accounts[i].TaxWrapper = &w
		}
	}
}

// dropSuperseded removes the rows of an account another source carries
// from a date on — everything dated on or AFTER that date, keeping what
// precedes it, so a statement or a dump straddling the handover still
// contributes its earlier half.
//
// Generic over the fact kinds because all three are filtered the same
// way and by the same pair of fields; `at` reads that pair off a row.
// Returns the slice unchanged when nothing is configured, and when the
// configured accounts appear in no row — an entry matching nothing is a
// no-op, not an error, since a source that stops emitting the account
// on its own is the outcome this was for.
func dropSuperseded[T any](rows []T, from map[string]int64, at func(T) (string, int64)) []T {
	if len(from) == 0 || len(rows) == 0 {
		return rows
	}
	kept := rows[:0]
	for _, row := range rows {
		account, when := at(row)
		if cutoff, ok := from[account]; ok && when >= cutoff {
			continue
		}
		kept = append(kept, row)
	}
	return kept
}

// rejectSupersededTransfers fails the load when an equity-transfer
// ledger row falls on or after the handover of the account it names.
//
// The adapter's rows are DROPPED there: a statement straddling the
// handover is expected, and its later half is the successor's to state.
// A ledger row is the opposite — it is written by hand, one row at a
// time, asserting a capital flow nothing else in silver carries.
// Dropping one silently would delete a real flow, which the returns
// engine then reads as performance inside the account; the row belongs
// under the source that carries the account from the handover on, and
// saying so is the only outcome that gets it there.
//
// Resolution is the ledger's own (account_external_id or nickname,
// gold.NewAccountResolver), because supersession is keyed on the
// resolved id.
func rejectSupersededTransfers(ctx context.Context, tx *sql.Tx, spec SourceSpec) error {
	if len(spec.Supersession) == 0 || len(spec.TransferLedger) == 0 {
		return nil
	}
	resolve, err := gold.NewAccountResolver(ctx, tx, spec.ID)
	if err != nil {
		return fmt.Errorf("equity_transfers: %w", err)
	}
	return checkSupersededTransfers(spec.ID, spec.TransferLedger, spec.Supersession, resolve)
}

// checkSupersededTransfers is rejectSupersededTransfers' decision, split
// off from the gold read so it can be exercised without one.
func checkSupersededTransfers(sourceID string, entries []TransferEntry,
	ends map[string]int64, resolve gold.AccountResolver) error {
	day := func(t int64) string { return time.Unix(t, 0).UTC().Format("2006-01-02") }
	for _, e := range entries {
		account, err := resolve(e.Account)
		if err != nil {
			return fmt.Errorf("equity_transfers: %w", err)
		}
		cutoff, superseded := ends[account]
		if !superseded || e.OccurredAt < cutoff {
			continue
		}
		return fmt.Errorf(
			"equity_transfers: %s is superseded in source %s from %s, so its row dated %s belongs under the source that carries it from there",
			account, sourceID, day(cutoff), day(e.OccurredAt))
	}
	return nil
}

// applyTaxableWrapper restates which taxable wrapper this source's
// taxable accounts sit in.
//
// ONLY `taxable_personal` moves. That is the adapter's generic answer
// for "a taxable account", given a bank feed says what a product is and
// never who holds it; every other wrapper is something the adapter had
// positive evidence for, and a blanket rule has no business touching
// it. An account with no wrapper at all is also left alone — absent is
// not the same as taxable, and guessing there is the error the
// coverage canary exists to report.
func applyTaxableWrapper(accounts []canonical.AccountChange, wrapper string) {
	if wrapper == "" {
		return
	}
	w := canonical.TaxWrapper(wrapper)
	for i := range accounts {
		if accounts[i].TaxWrapper != nil && *accounts[i].TaxWrapper == canonical.TaxWrapperTaxablePersonal {
			v := w
			accounts[i].TaxWrapper = &v
		}
	}
}

// applyAccountOverrides patches each AccountChange whose
// account_external_id appears in the overrides map. Non-empty
// override fields replace the adapter's value (Nickname /
// AccountCategory / TaxWrapper / ManagementStyle); empty fields
// are left as-is. Overrides for account_external_ids not in the
// batch are silently ignored — overrides may be configured
// for accounts that happen not to be in this snapshot
// window.
func applyAccountOverrides(accounts []canonical.AccountChange, overrides map[string]AccountOverride) {
	if len(overrides) == 0 {
		return
	}
	for i := range accounts {
		ov, ok := overrides[accounts[i].AccountExternalID]
		if !ok {
			continue
		}
		if ov.Nickname != "" {
			n := ov.Nickname
			accounts[i].Nickname = &n
		}
		if ov.Category != "" {
			c := ov.Category
			accounts[i].AccountCategory = &c
		}
		if ov.TaxWrapper != "" {
			w := canonical.TaxWrapper(ov.TaxWrapper)
			accounts[i].TaxWrapper = &w
		}
		if ov.ManagementStyle != "" {
			s := canonical.ManagementStyle(ov.ManagementStyle)
			accounts[i].ManagementStyle = &s
		}
	}
}

// applyInstrumentOverrides patches each InstrumentChange whose
// instrument_external_id appears in the overrides map, and every
// PositionChange referencing such an instrument — positions carry
// their own classification copy, so both must move together or the
// dimension and the fact rows would disagree. Overrides for
// instruments not in the batch are silently ignored (the position
// may not be held in this snapshot window).
func applyInstrumentOverrides(instruments []canonical.InstrumentChange, positions []canonical.PositionChange, overrides map[string]InstrumentOverride) {
	if len(overrides) == 0 {
		return
	}
	for i := range instruments {
		ov, ok := overrides[instruments[i].InstrumentExternalID]
		if !ok {
			continue
		}
		applyInstrumentOverrideTo(&instruments[i].AssetClass, &instruments[i].Vehicle, ov)
	}
	for i := range positions {
		if positions[i].InstrumentExternalID == nil {
			continue
		}
		ov, ok := overrides[*positions[i].InstrumentExternalID]
		if !ok {
			continue
		}
		applyInstrumentOverrideTo(&positions[i].AssetClass, &positions[i].Vehicle, ov)
	}
}

// applyTransactionInstruments links a row whose feed named an
// instrument nothing in the product could resolve, keyed by the token
// the adapter looked up and failed on — every row of the source that
// states it, in any account and of any kind.
//
// One key for every source, because the adapter states the token rather
// than the loader digging it out of a payload whose shape differs per
// feed. A row that already resolved is never touched: config closes the
// tail, it does not second-guess the adapter — and a token still listed
// after the adapter learned to resolve it is a silent no-op rather than
// an error, because that is what success looks like.
//
// The IDENTITY only. What the instrument is remains
// `instrument_overrides`' question; the two compose. A row's own
// taxonomy pair is dropped as the link lands — it was the adapter's
// answer for a row nothing could name, and the instrument now named
// answers better and stays current as the dimension is reclassified.
func applyTransactionInstruments(txns []canonical.TransactionChange, links map[string]string) {
	if len(links) == 0 {
		return
	}
	for i := range txns {
		if linkHint(&txns[i].InstrumentExternalID, txns[i].InstrumentHint, links) {
			txns[i].AssetClass, txns[i].Vehicle = "", ""
		}
	}
}

// linkHint sets an unresolved row's instrument from the config link for
// the token the adapter failed on, and reports whether it did. A row the
// adapter resolved, or whose token no link names, is left alone.
func linkHint(instrument **string, hint string, links map[string]string) bool {
	if *instrument != nil || hint == "" {
		return false
	}
	id, ok := links[hint]
	if !ok || id == "" {
		return false
	}
	*instrument = &id
	return true
}

func applyInstrumentOverrideTo(ac *canonical.AssetClass, veh *canonical.Vehicle, ov InstrumentOverride) {
	if ov.AssetClass != "" {
		*ac = canonical.AssetClass(ov.AssetClass)
	}
	if ov.Vehicle != "" {
		*veh = canonical.Vehicle(ov.Vehicle)
	}
}
