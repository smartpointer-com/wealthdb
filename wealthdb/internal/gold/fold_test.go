package gold

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
)

// ---- fold ≡ record-by-record equivalence ----------------------------------
//
// The loader's ChangeAccumulator claims: for an entity gold has not
// seen, folding a load's records and upserting once stores exactly
// what upserting record-by-record would. These sweeps drive every
// permutation of small record alphabets through both paths and
// compare the stored rows column-by-column. The alphabets are built
// to hit each guard branch: newer-wins, older-pinned, equal-ts
// (last emission wins), and nil-vs-set on the COALESCE'd columns.

// instrumentAlphabet returns records that vary the timestamp order
// and alternate nil/set across the nullable columns (two disjoint
// column groups, so a fold has to preserve values across records
// rather than take any single record wholesale). FirstSeenAt is
// deliberately distinct from LastSeenAt so a fold that conflated
// the two seen-at columns couldn't slip through the comparison.
func instrumentAlphabet() []canonical.InstrumentChange {
	mk := func(ts int64, group int, label string) canonical.InstrumentChange {
		r := canonical.InstrumentChange{
			SilverSourceID:       "test-src",
			InstrumentExternalID: "INSTR",
			AssetClass:           canonical.AssetClassPublicEquity,
			Vehicle:              canonical.VehicleStock,
			FirstSeenAt:          ts - 5,
			LastSeenAt:           ts,
		}
		switch group {
		case 0: // carries nothing nullable
		case 1: // identity columns set, descriptive nil
			r.ISIN = ptr("US0000000" + label)
			r.CUSIP = ptr("00000" + label)
			r.Vehicle = canonical.VehicleETF
		case 2: // descriptive columns set, identity nil
			r.Symbol = ptr("SYM" + label)
			r.Name = ptr("Name " + label)
			r.Currency = ptr("USD")
			r.Payload = json.RawMessage(`{"v":"` + label + `"}`)
		}
		return r
	}
	var out []canonical.InstrumentChange
	for _, ts := range []int64{10, 30} {
		for g := 0; g < 3; g++ {
			out = append(out, mk(ts, g, fmt.Sprintf("%d%d", ts, g)))
		}
	}
	return out
}

func accountAlphabet() []canonical.AccountChange {
	mk := func(ts int64, group int, label string) canonical.AccountChange {
		r := canonical.AccountChange{
			SilverSourceID:    "test-src",
			AccountExternalID: "ACC",
			AccountKind:       canonical.AccountKindBrokerage,
			FirstSeenAt:       ts - 5,
			LastSeenAt:        ts,
		}
		switch group {
		case 0:
		case 1:
			r.AccountKind = canonical.AccountKindCash
			r.DisplayName = ptr("Display " + label)
			r.BaseCurrency = ptr("CHF")
			r.RelationshipID = ptr("REL" + label)
			r.TaxWrapper = ptr(canonical.TaxWrapperTaxablePersonal)
		case 2:
			r.Nickname = ptr("Nick " + label)
			r.AccountCategory = ptr("Cat " + label)
			r.PortfolioExternalID = ptr("PF" + label)
			r.ManagementStyle = ptr(canonical.ManagementStyleSelfDirected)
			r.Payload = json.RawMessage(`{"v":"` + label + `"}`)
		}
		return r
	}
	var out []canonical.AccountChange
	for _, ts := range []int64{10, 30} {
		for g := 0; g < 3; g++ {
			out = append(out, mk(ts, g, fmt.Sprintf("%d%d", ts, g)))
		}
	}
	return out
}

func portfolioAlphabet() []canonical.PortfolioChange {
	mk := func(ts int64, group int, label string) canonical.PortfolioChange {
		r := canonical.PortfolioChange{
			SilverSourceID:      "test-src",
			PortfolioExternalID: "PF",
			FirstSeenAt:         ts - 5,
			LastSeenAt:          ts,
		}
		switch group {
		case 0:
		case 1:
			r.DisplayName = ptr("Display " + label)
			r.RelationshipID = ptr("REL" + label)
		case 2:
			r.BaseCurrency = ptr("CHF")
			r.Nickname = ptr("Nick " + label)
			r.Payload = json.RawMessage(`{"v":"` + label + `"}`)
		}
		return r
	}
	var out []canonical.PortfolioChange
	for _, ts := range []int64{10, 30} {
		for g := 0; g < 3; g++ {
			out = append(out, mk(ts, g, fmt.Sprintf("%d%d", ts, g)))
		}
	}
	return out
}

// readRow returns every column of the single row matching the
// external id, normalised to strings, so sequences can be compared
// across the two write paths.
func readRow(t *testing.T, ctx context.Context, db *sql.DB, table, idCol, id string) string {
	t.Helper()
	// Casting the whole row struct to VARCHAR renders every column
	// without hand-listing them per table; the caller strips the
	// embedded external id (stripID) before comparing.
	q := fmt.Sprintf(
		`SELECT CAST(t AS VARCHAR) FROM (SELECT * FROM %s WHERE %s = ?) t`,
		table, idCol)
	var row string
	if err := db.QueryRowContext(ctx, q, id).Scan(&row); err != nil {
		t.Fatalf("read %s row %s: %v", table, id, err)
	}
	return row
}

// runSequential upserts records one call at a time (the pre-fold
// write path); runFolded accumulates then flushes once. Both write
// under a distinct external id in the same DB so their rows can be
// compared directly.
func upsertSeq[T any](t *testing.T, ctx context.Context, db *sql.DB, seq []T, up func(*Writer, context.Context, []T) error) {
	t.Helper()
	inTx(t, db, ctx, func(w *Writer) error {
		for i := range seq {
			if err := up(w, ctx, seq[i:i+1]); err != nil {
				return err
			}
		}
		return nil
	})
}

// mustAdd folds a batch, failing the test on a validation error.
func mustAdd(t *testing.T, acc *ChangeAccumulator, b *canonical.SnapshotBatch) {
	t.Helper()
	if err := acc.AddBatch(b); err != nil {
		t.Fatalf("AddBatch: %v", err)
	}
}

// permutationsOfLength streams every sequence of the given length
// over alphabet indices into visit.
func permutationsOfLength(alphabet, length int, visit func(seq []int)) {
	seq := make([]int, length)
	var rec func(pos int)
	rec = func(pos int) {
		if pos == length {
			visit(seq)
			return
		}
		for i := 0; i < alphabet; i++ {
			seq[pos] = i
			rec(pos + 1)
		}
	}
	rec(0)
}

func TestFoldInstrumentMatchesSequentialUpserts(t *testing.T) {
	db, ctx := openMigrated(t)
	alpha := instrumentAlphabet()

	n := 0
	permutationsOfLength(len(alpha), 3, func(seq []int) {
		seqID := fmt.Sprintf("SEQ-%d", n)
		foldID := fmt.Sprintf("FOLD-%d", n)
		n++

		recs := make([]canonical.InstrumentChange, len(seq))
		for i, j := range seq {
			recs[i] = alpha[j]
		}

		byID := func(id string) []canonical.InstrumentChange {
			out := make([]canonical.InstrumentChange, len(recs))
			copy(out, recs)
			for i := range out {
				out[i].InstrumentExternalID = id
			}
			return out
		}

		upsertSeq(t, ctx, db, byID(seqID), (*Writer).UpsertInstruments)

		acc := NewChangeAccumulator()
		for _, r := range byID(foldID) {
			mustAdd(t, acc, &canonical.SnapshotBatch{Instruments: []canonical.InstrumentChange{r}})
		}
		inTx(t, db, ctx, func(w *Writer) error { return acc.Flush(ctx, w) })

		got := readRow(t, ctx, db, "instruments", "instrument_external_id", foldID)
		want := readRow(t, ctx, db, "instruments", "instrument_external_id", seqID)
		// The row strings embed the differing external ids; strip them.
		got = stripID(got, foldID)
		want = stripID(want, seqID)
		if got != want {
			t.Errorf("sequence %v: folded row differs\n fold: %s\n  seq: %s", seq, got, want)
		}
	})
}

func TestFoldAccountMatchesSequentialUpserts(t *testing.T) {
	db, ctx := openMigrated(t)
	alpha := accountAlphabet()

	n := 0
	permutationsOfLength(len(alpha), 3, func(seq []int) {
		seqID := fmt.Sprintf("SEQ-%d", n)
		foldID := fmt.Sprintf("FOLD-%d", n)
		n++

		recs := make([]canonical.AccountChange, len(seq))
		for i, j := range seq {
			recs[i] = alpha[j]
		}
		byID := func(id string) []canonical.AccountChange {
			out := make([]canonical.AccountChange, len(recs))
			copy(out, recs)
			for i := range out {
				out[i].AccountExternalID = id
			}
			return out
		}

		upsertSeq(t, ctx, db, byID(seqID), (*Writer).UpsertAccounts)

		acc := NewChangeAccumulator()
		for _, r := range byID(foldID) {
			mustAdd(t, acc, &canonical.SnapshotBatch{Accounts: []canonical.AccountChange{r}})
		}
		inTx(t, db, ctx, func(w *Writer) error { return acc.Flush(ctx, w) })

		got := stripID(readRow(t, ctx, db, "accounts", "account_external_id", foldID), foldID)
		want := stripID(readRow(t, ctx, db, "accounts", "account_external_id", seqID), seqID)
		if got != want {
			t.Errorf("sequence %v: folded row differs\n fold: %s\n  seq: %s", seq, got, want)
		}
	})
}

func TestFoldPortfolioMatchesSequentialUpserts(t *testing.T) {
	db, ctx := openMigrated(t)
	alpha := portfolioAlphabet()

	n := 0
	permutationsOfLength(len(alpha), 3, func(seq []int) {
		seqID := fmt.Sprintf("SEQ-%d", n)
		foldID := fmt.Sprintf("FOLD-%d", n)
		n++

		recs := make([]canonical.PortfolioChange, len(seq))
		for i, j := range seq {
			recs[i] = alpha[j]
		}
		byID := func(id string) []canonical.PortfolioChange {
			out := make([]canonical.PortfolioChange, len(recs))
			copy(out, recs)
			for i := range out {
				out[i].PortfolioExternalID = id
			}
			return out
		}

		upsertSeq(t, ctx, db, byID(seqID), (*Writer).UpsertPortfolios)

		acc := NewChangeAccumulator()
		for _, r := range byID(foldID) {
			mustAdd(t, acc, &canonical.SnapshotBatch{Portfolios: []canonical.PortfolioChange{r}})
		}
		inTx(t, db, ctx, func(w *Writer) error { return acc.Flush(ctx, w) })

		got := stripID(readRow(t, ctx, db, "portfolios", "portfolio_external_id", foldID), foldID)
		want := stripID(readRow(t, ctx, db, "portfolios", "portfolio_external_id", seqID), seqID)
		if got != want {
			t.Errorf("sequence %v: folded row differs\n fold: %s\n  seq: %s", seq, got, want)
		}
	})
}

// stripID removes the per-path external id from a rendered row so
// the remaining columns compare equal across the two paths.
func stripID(row, id string) string {
	return strings.ReplaceAll(row, id, "")
}

// ---- documented divergence against a pre-existing row ---------------------

// TestChangeAccumulatorPreexistingRow pins the one interleave where
// fold-then-upsert diverges from record-by-record application: gold
// already holds the entity (last_seen 20), and the load emits an
// OLDER record carrying a value (ts 10, name set) followed by a
// NEWER record without it (ts 30, name nil). Record-by-record, the
// ts-10 name would be discarded against the stored ts-20 row and the
// column would keep "Stored"; folded, the ts-10 name survives into
// the merged record (in-load fold sees no ts-20 row) and wins the
// upsert. The fold's outcome is asserted here so a change to it is a
// conscious decision, not an accident. See ChangeAccumulator's doc
// comment.
func TestChangeAccumulatorPreexistingRow(t *testing.T) {
	db, ctx := openMigrated(t)

	seed := canonical.InstrumentChange{
		SilverSourceID:       "test-src",
		InstrumentExternalID: "INSTR",
		AssetClass:           canonical.AssetClassPublicEquity,
		Vehicle:              canonical.VehicleStock,
		Name:                 ptr("Stored"),
		FirstSeenAt:          20,
		LastSeenAt:           20,
	}
	inTx(t, db, ctx, func(w *Writer) error {
		return w.UpsertInstruments(ctx, []canonical.InstrumentChange{seed})
	})

	older := seed
	older.Name = ptr("Backfill")
	older.FirstSeenAt, older.LastSeenAt = 8, 10
	newer := seed
	newer.Name = nil
	newer.FirstSeenAt, newer.LastSeenAt = 25, 30

	acc := NewChangeAccumulator()
	mustAdd(t, acc, &canonical.SnapshotBatch{Instruments: []canonical.InstrumentChange{older}})
	mustAdd(t, acc, &canonical.SnapshotBatch{Instruments: []canonical.InstrumentChange{newer}})
	inTx(t, db, ctx, func(w *Writer) error { return acc.Flush(ctx, w) })

	var name string
	var fs, ls int64
	if err := db.QueryRowContext(ctx, `
        SELECT name, first_seen_at, last_seen_at
          FROM instruments WHERE instrument_external_id='INSTR'
    `).Scan(&name, &fs, &ls); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if name != "Backfill" {
		t.Errorf("name = %q, want %q (the folded outcome)", name, "Backfill")
	}
	if fs != 8 || ls != 30 {
		t.Errorf("seen range = [%d,%d], want [8,30]", fs, ls)
	}
}

// TestChangeAccumulatorRejectsSupersededInvalid pins the validation
// tripwire: an invalid emission fails at AddBatch even though a
// later valid record would supersede it in the fold, matching the
// old write path where every emission hit the upsert's checks.
func TestChangeAccumulatorRejectsSupersededInvalid(t *testing.T) {
	bad := canonical.SnapshotBatch{Instruments: []canonical.InstrumentChange{{
		SilverSourceID:       "test-src",
		InstrumentExternalID: "INSTR",
		AssetClass:           canonical.AssetClassCrypto,
		Vehicle:              canonical.VehicleMortgage, // not an admitted pair
		FirstSeenAt:          10,
		LastSeenAt:           10,
	}}}
	if err := NewChangeAccumulator().AddBatch(&bad); err == nil {
		t.Fatal("expected error for invalid taxonomy pair, got nil")
	}
}

// TestChangeAccumulatorFoldsToOneRowPerEntity asserts the point of
// the fold: many emissions of the same entity leave exactly one
// dimension row, with the seen-at range spanning all of them.
func TestChangeAccumulatorFoldsToOneRowPerEntity(t *testing.T) {
	db, ctx := openMigrated(t)

	acc := NewChangeAccumulator()
	for ts := int64(1); ts <= 100; ts++ {
		mustAdd(t, acc, &canonical.SnapshotBatch{
			Instruments: []canonical.InstrumentChange{{
				SilverSourceID:       "test-src",
				InstrumentExternalID: "INSTR",
				AssetClass:           canonical.AssetClassPublicEquity,
				Vehicle:              canonical.VehicleStock,
				Name:                 ptr(fmt.Sprintf("Name %d", ts)),
				FirstSeenAt:          ts,
				LastSeenAt:           ts,
			}},
		})
	}
	inTx(t, db, ctx, func(w *Writer) error { return acc.Flush(ctx, w) })

	var n int
	var name string
	var fs, ls int64
	if err := db.QueryRowContext(ctx, `
        SELECT COUNT(*), MIN(name), MIN(first_seen_at), MAX(last_seen_at)
          FROM instruments WHERE instrument_external_id='INSTR'
    `).Scan(&n, &name, &fs, &ls); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if n != 1 {
		t.Fatalf("row count = %d, want 1", n)
	}
	if name != "Name 100" || fs != 1 || ls != 100 {
		t.Errorf("got name=%q range=[%d,%d], want Name 100 [1,100]", name, fs, ls)
	}
}

// ---- older observation fills absent columns -------------------------------
//
// The schwab full-rebuild regression: the statement-derived
// tax_wrapper/account_category ride an account record dated at the
// web silver's snapshot, while fresher api records carry neither.
// Under the one-directional guard the newer wrapper-less records
// discarded the older record's attributes on every rebuild, and the
// wrapper silently fell back to the taxable_personal render
// default. Per column, recency
// arbitrates conflicts — absence never wins — in both the fold and
// the record-by-record SQL, in either arrival order.
func TestOlderObservationFillsAbsentColumns(t *testing.T) {
	db, ctx := openMigrated(t)

	older := canonical.AccountChange{
		SilverSourceID:  "test-src",
		AccountKind:     canonical.AccountKindBrokerage,
		AccountCategory: ptr("Contributory IRA"),
		TaxWrapper:      ptr(canonical.TaxWrapperTraditionalIRA),
		FirstSeenAt:     10,
		LastSeenAt:      10,
	}
	newer := canonical.AccountChange{
		SilverSourceID: "test-src",
		AccountKind:    canonical.AccountKindBrokerage,
		Nickname:       ptr("api nickname"),
		FirstSeenAt:    90,
		LastSeenAt:     90,
	}

	check := func(t *testing.T, id string) {
		var wrapper, category, nickname sql.NullString
		err := db.QueryRowContext(ctx, `
			SELECT tax_wrapper, account_category, nickname
			  FROM accounts WHERE account_external_id = ?`, id).
			Scan(&wrapper, &category, &nickname)
		if err != nil {
			t.Fatal(err)
		}
		if wrapper.String != string(canonical.TaxWrapperTraditionalIRA) {
			t.Errorf("tax_wrapper = %q, want traditional_ira", wrapper.String)
		}
		if category.String != "Contributory IRA" {
			t.Errorf("account_category = %q, want the older record's label", category.String)
		}
		if nickname.String != "api nickname" {
			t.Errorf("nickname = %q, want the newer record's", nickname.String)
		}
	}

	withID := func(r canonical.AccountChange, id string) canonical.AccountChange {
		r.AccountExternalID = id
		return r
	}

	// Record-by-record, worst order: newer stored first, older arrives
	// against a fresher stored row.
	upsertSeq(t, ctx, db,
		[]canonical.AccountChange{withID(newer, "SEQ"), withID(older, "SEQ")},
		(*Writer).UpsertAccounts)
	check(t, "SEQ")

	// Folded, same order.
	acc := NewChangeAccumulator()
	mustAdd(t, acc, &canonical.SnapshotBatch{Accounts: []canonical.AccountChange{withID(newer, "FOLD")}})
	mustAdd(t, acc, &canonical.SnapshotBatch{Accounts: []canonical.AccountChange{withID(older, "FOLD")}})
	inTx(t, db, ctx, func(w *Writer) error { return acc.Flush(ctx, w) })
	check(t, "FOLD")
}
