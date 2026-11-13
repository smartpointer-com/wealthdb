package main

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/gold"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/spending"
)

// ---- persistCategorizations --------------------------------------------------

// openMerchantStore is a fresh migrated gold, which is all the
// merchant store needs: it is keyed by signature alone and joins to
// nothing.
func openMerchantStore(t *testing.T) (*sql.DB, context.Context) {
	t.Helper()
	db, err := gold.OpenFresh(filepath.Join(t.TempDir(), "gold.db"))
	if err != nil {
		t.Fatalf("open gold: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	ctx := context.Background()
	return db, ctx
}

type storedVerdict struct {
	name, detailed, model string
	sigVersion            int
	assignedAt            int64
}

func readVerdict(t *testing.T, db *sql.DB, ctx context.Context, sig string) storedVerdict {
	t.Helper()
	var v storedVerdict
	if err := db.QueryRowContext(ctx, `
        SELECT merchant_name, spend_detailed, model_name, signature_version, assigned_at
          FROM spend_merchant_categories WHERE merchant_signature = ?`, sig).
		Scan(&v.name, &v.detailed, &v.model, &v.sigVersion, &v.assignedAt); err != nil {
		t.Fatalf("read verdict %q: %v", sig, err)
	}
	return v
}

// TestPersistCategorizationsUpsertsBySignature pins the store's key: a
// re-run with a better model overwrites in place rather than accumulating
// a second answer for the same merchant, and the count returned is the
// table's, not the write's.
func TestPersistCategorizationsUpsertsBySignature(t *testing.T) {
	t.Parallel()
	db, ctx := openMerchantStore(t)

	total, err := persistCategorizations(ctx, db, spendingCategorizeFamily, []categorization{
		{"BLUE HARBOUR CAFE", "Blue Harbour Cafe", "FOOD_AND_DRINK_COFFEE"},
		{"NORTHWIND HARDWARE", "Northwind Hardware", "HOME_IMPROVEMENT_HARDWARE"},
	}, 1_700_000_000, "first-model")
	if err != nil || total != 2 {
		t.Fatalf("first write = (%d, %v), want (2, nil)", total, err)
	}
	if got := readVerdict(t, db, ctx, "BLUE HARBOUR CAFE"); got.sigVersion != spending.SignatureVersion {
		t.Errorf("signature_version = %d, want the version that produced the key (%d)",
			got.sigVersion, spending.SignatureVersion)
	}

	// Same signature, a different verdict from a different model.
	total, err = persistCategorizations(ctx, db, spendingCategorizeFamily, []categorization{
		{"BLUE HARBOUR CAFE", "Blue Harbour Coffee", "FOOD_AND_DRINK_RESTAURANT"},
	}, 1_700_009_999, "second-model")
	if err != nil || total != 2 {
		t.Fatalf("re-write = (%d, %v), want (2, nil) — the store is keyed by signature", total, err)
	}
	want := storedVerdict{"Blue Harbour Coffee", "FOOD_AND_DRINK_RESTAURANT", "second-model",
		spending.SignatureVersion, 1_700_009_999}
	if got := readVerdict(t, db, ctx, "BLUE HARBOUR CAFE"); got != want {
		t.Errorf("verdict = %+v, want %+v", got, want)
	}
	// The merchant nobody re-asked about is untouched.
	if got := readVerdict(t, db, ctx, "NORTHWIND HARDWARE"); got.model != "first-model" {
		t.Errorf("unrelated verdict model = %q, want it left alone", got.model)
	}
}

// TestPersistCategorizationsWritesNothingWhenGoldRefuses pins the
// contract batchStore's hold depends on: a write that cannot go through
// reports it and leaves the store exactly as it was, so the caller can
// safely keep the verdicts and try again. Read-only is the real case —
// gold is one read-write handle or many read-only ones, so a concurrent
// reader is enough to refuse the reopen.
func TestPersistCategorizationsWritesNothingWhenGoldRefuses(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "gold.db")
	db, err := gold.OpenFresh(path)
	if err != nil {
		t.Fatalf("open gold: %v", err)
	}
	ctx := context.Background()
	if _, err := persistCategorizations(ctx, db, spendingCategorizeFamily, []categorization{
		{"BLUE HARBOUR CAFE", "Blue Harbour Cafe", "FOOD_AND_DRINK_COFFEE"},
	}, 1_700_000_000, "first-model"); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close gold: %v", err)
	}

	ro, err := gold.Open(path, gold.ModeReadOnly)
	if err != nil {
		t.Fatalf("reopen read-only: %v", err)
	}
	defer ro.Close()
	if _, err := persistCategorizations(ctx, ro, spendingCategorizeFamily, []categorization{
		{"NORTHWIND HARDWARE", "Northwind Hardware", "HOME_IMPROVEMENT_HARDWARE"},
	}, 1_700_009_999, "second-model"); err == nil {
		t.Fatal("a write gold refused must report, not report success")
	}
	var n int
	if err := ro.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM spend_merchant_categories`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 1 {
		t.Errorf("%d row(s) after the refused write, want the 1 that was there before", n)
	}
}
