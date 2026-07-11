// Package loader is the IVM-style bridge that runs the
// silver→gold load sequence described in docs/DESIGN.md §8.1. It
// is the single place that touches both the silver Adapter
// interface and the gold *sql.DB; cmd/wealthdb's subcommands call
// in here for `wealthdb load` and `wealthdb reset`.
package loader

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/ptu-gh/wealthdb/wealthdb/internal/canonical"
	"github.com/ptu-gh/wealthdb/wealthdb/internal/gold"
	"github.com/ptu-gh/wealthdb/wealthdb/internal/silver"
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
	// InstrumentOverrides is the per-instrument_external_id
	// override map for this source — asset_class values from the
	// config-file `instrument_overrides` block. Applied after the
	// adapter has classified, to the instrument dimension and to
	// every position row referencing the instrument.
	InstrumentOverrides map[string]InstrumentOverride
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
}

// PortfolioOverride is the loader's view of one config-file
// portfolio_overrides entry. TaxWrapper is the only dimension
// wired through today; empty = no override.
type PortfolioOverride struct {
	TaxWrapper string
}

// InstrumentOverride is the loader's view of one config-file
// instrument_overrides entry. AssetClass (legacy) and the 2-D pair
// AssetClassNew + Vehicle are validated upstream at config-load time;
// the loader trusts them. AssetClassNew/Vehicle are empty when the
// entry only overrides the legacy column.
type InstrumentOverride struct {
	AssetClass    string
	AssetClassNew string
	Vehicle       string
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

		nSnap, err := applySnapshots(ctx, tx, spec.ID, conn, window, spec.Overrides, spec.PortfolioOverrides, spec.InstrumentOverrides)
		if err != nil {
			return nil, fmt.Errorf("Load(%s): apply snapshots: %w", spec.ID, err)
		}
		res.SnapshotsLoaded = nSnap

		nTx, err := applyTransactions(ctx, tx, spec.ID, conn, window)
		if err != nil {
			return nil, fmt.Errorf("Load(%s): apply transactions: %w", spec.ID, err)
		}
		res.TransactionsLoaded = nTx

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
// Returns the total count of snapshot-grain rows written. The
// override maps (any may be nil) are applied after stamping:
// account/portfolio overrides to AccountChange records
// (portfolio_overrides go first so per-account overrides win on
// overlap), instrument overrides to InstrumentChange and
// PositionChange records. See applyAccountOverrides,
// applyPortfolioOverrides and applyInstrumentOverrides.
func applySnapshots(ctx context.Context, tx *sql.Tx, sourceID string, conn silver.Connection, w canonical.Window, overrides map[string]AccountOverride, portfolioOverrides map[string]PortfolioOverride, instrumentOverrides map[string]InstrumentOverride) (int, error) {
	stream, err := conn.Snapshots(ctx, w)
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

		// Stamp the silver_source_id on every record before write.
		stampSnapshotBatch(&batch, sourceID)
		// Config-file overrides go on top of whatever the adapter
		// emitted; see DESIGN.md §13.9. Portfolio overrides apply
		// first (broader scope); per-account overrides override on
		// the same column (narrower scope wins).
		applyPortfolioOverrides(batch.Accounts, portfolioOverrides)
		applyAccountOverrides(batch.Accounts, overrides)
		applyInstrumentOverrides(batch.Instruments, batch.Positions, instrumentOverrides)

		// Portfolios first so the FK semantics on
		// accounts.portfolio_external_id are satisfied (though
		// gold doesn't declare the FK because DuckDB can't defer).
		if err := writer.UpsertPortfolios(ctx, batch.Portfolios); err != nil {
			return total, err
		}
		if err := writer.UpsertAccounts(ctx, batch.Accounts); err != nil {
			return total, err
		}
		if err := writer.UpsertInstruments(ctx, batch.Instruments); err != nil {
			return total, err
		}
		if err := writer.InsertPositions(ctx, batch.Positions); err != nil {
			return total, err
		}
		if err := writer.InsertCashBalances(ctx, batch.CashBalances); err != nil {
			return total, err
		}
		if err := writer.InsertFxRates(ctx, batch.FxRates); err != nil {
			return total, err
		}

		total += len(batch.Positions) + len(batch.CashBalances) + len(batch.FxRates)

		if !more {
			return total, nil
		}
	}
}

// applyTransactions drains conn.Transactions into the gold writer.
func applyTransactions(ctx context.Context, tx *sql.Tx, sourceID string, conn silver.Connection, w canonical.Window) (int, error) {
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
		stampTransactionBatch(&batch, sourceID)
		if err := writer.InsertTransactions(ctx, batch.Transactions); err != nil {
			return total, err
		}
		total += len(batch.Transactions)
		if !more {
			return total, nil
		}
	}
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
// may not be held in this snapshot window). The legacy asset_class
// and the 2-D (asset_class_new, vehicle) pair are patched
// independently: an entry may override either or both.
func applyInstrumentOverrides(instruments []canonical.InstrumentChange, positions []canonical.PositionChange, overrides map[string]InstrumentOverride) {
	if len(overrides) == 0 {
		return
	}
	for i := range instruments {
		ov, ok := overrides[instruments[i].InstrumentExternalID]
		if !ok {
			continue
		}
		applyInstrumentOverrideTo(&instruments[i].AssetClass, &instruments[i].AssetClassNew, &instruments[i].Vehicle, ov)
	}
	for i := range positions {
		if positions[i].InstrumentExternalID == nil {
			continue
		}
		ov, ok := overrides[*positions[i].InstrumentExternalID]
		if !ok {
			continue
		}
		applyInstrumentOverrideTo(&positions[i].AssetClass, &positions[i].AssetClassNew, &positions[i].Vehicle, ov)
	}
}

func applyInstrumentOverrideTo(ac *canonical.AssetClass, acNew *canonical.AssetClass, veh *canonical.Vehicle, ov InstrumentOverride) {
	if ov.AssetClass != "" {
		*ac = canonical.AssetClass(ov.AssetClass)
	}
	if ov.AssetClassNew != "" {
		*acNew = canonical.AssetClass(ov.AssetClassNew)
		*veh = canonical.Vehicle(ov.Vehicle)
	}
}
