package gold

import (
	"errors"
	"testing"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
)

// TestNewAccountResolver pins the one resolution rule every
// account-keyed ledger shares: an id resolves to itself, a nickname to
// its account, a shared nickname is refused, and an unknown reference
// is ErrUnknownAccount so a caller can tell "not loaded yet" from a
// configuration fault.
func TestNewAccountResolver(t *testing.T) {
	db, ctx := openMigrated(t)
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()

	every, shared, usd := "Everyday", "Shared", "USD"
	if err := NewWriter(tx).UpsertAccounts(ctx, []canonical.AccountChange{
		{SilverSourceID: "bank", AccountExternalID: "HASH1", AccountKind: canonical.AccountKindCash,
			Nickname: &every, BaseCurrency: &usd, FirstSeenAt: 1, LastSeenAt: 2},
		{SilverSourceID: "bank", AccountExternalID: "HASH2", AccountKind: canonical.AccountKindCash,
			Nickname: &shared, BaseCurrency: &usd, FirstSeenAt: 1, LastSeenAt: 2},
		{SilverSourceID: "bank", AccountExternalID: "HASH3", AccountKind: canonical.AccountKindCash,
			Nickname: &shared, BaseCurrency: &usd, FirstSeenAt: 1, LastSeenAt: 2},
		{SilverSourceID: "other", AccountExternalID: "HASH9", AccountKind: canonical.AccountKindCash,
			Nickname: &every, BaseCurrency: &usd, FirstSeenAt: 1, LastSeenAt: 2},
	}); err != nil {
		t.Fatalf("seed accounts: %v", err)
	}

	resolve, err := NewAccountResolver(ctx, tx, "bank")
	if err != nil {
		t.Fatalf("NewAccountResolver: %v", err)
	}
	for in, want := range map[string]string{"HASH1": "HASH1", "Everyday": "HASH1", "HASH2": "HASH2"} {
		if got, err := resolve(in); err != nil || got != want {
			t.Errorf("resolve(%q) = (%q, %v), want %q", in, got, err, want)
		}
	}
	if _, err := resolve("Shared"); err == nil || errors.Is(err, ErrUnknownAccount) {
		t.Errorf("a shared nickname must be refused as ambiguous, got %v", err)
	}
	// The other source's account is not visible: resolution is per source.
	if _, err := resolve("HASH9"); !errors.Is(err, ErrUnknownAccount) {
		t.Errorf("another source's account must be unknown here, got %v", err)
	}
	if _, err := resolve("Nope"); !errors.Is(err, ErrUnknownAccount) {
		t.Errorf("an unknown reference must be ErrUnknownAccount, got %v", err)
	}

	// A source with no accounts at all resolves nothing, without erroring
	// at construction: that is what "not loaded yet" looks like.
	none, err := NewAccountResolver(ctx, tx, "unloaded")
	if err != nil {
		t.Fatalf("NewAccountResolver over no accounts: %v", err)
	}
	if _, err := none("Anything"); !errors.Is(err, ErrUnknownAccount) {
		t.Errorf("an unloaded source must resolve nothing, got %v", err)
	}
}
