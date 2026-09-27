package config

import (
	"strings"
	"testing"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
)

// TestDeclaredAccountsLoad: a declaration becomes an account row under
// the reserved source, with the kind and wrapper it states, sorted by
// id; a rule may name one as the far side of an own-account move, in
// either family.
func TestDeclaredAccountsLoad(t *testing.T) {
	path := writeConfig(t, `{
        "gold_db": "/tmp/x", "default_currency": "USD",
        "silver_sources": [{"id": "bank", "kind": "chase", "path": "/tmp/b.db"}],
        "declared_accounts": {
            "savings-bank": {"account_kind": "cash", "tax_wrapper": "taxable_personal",
                             "nickname": "Example Savings", "currency": "USD"},
            "old-plan":     {"account_kind": "brokerage", "tax_wrapper": "401k"}
        },
        "spending": {"rules": [
            {"match": "EXAMPLE SAVINGS", "category": "internal_transfer", "far": "savings-bank"}
        ]},
        "income": {"rules": [
            {"match": "EXAMPLE SAVINGS", "type": "internal_transfer", "far": "savings-bank"}
        ]}
    }`)
	c, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	rows := c.DeclaredAccountChanges(42)
	if len(rows) != 2 {
		t.Fatalf("declared rows = %d, want 2", len(rows))
	}
	if rows[0].AccountExternalID != "old-plan" || rows[1].AccountExternalID != "savings-bank" {
		t.Errorf("rows are not sorted by id: %q, %q", rows[0].AccountExternalID, rows[1].AccountExternalID)
	}
	sb := rows[1]
	if sb.SilverSourceID != canonical.DeclaredSourceID || sb.AccountKind != canonical.AccountKindCash ||
		sb.TaxWrapper == nil || *sb.TaxWrapper != canonical.TaxWrapperTaxablePersonal ||
		sb.Nickname == nil || *sb.Nickname != "Example Savings" ||
		sb.DisplayName == nil || *sb.DisplayName != "Example Savings" ||
		sb.BaseCurrency == nil || *sb.BaseCurrency != "USD" ||
		sb.FirstSeenAt != 42 || sb.LastSeenAt != 42 {
		t.Errorf("savings-bank row = %+v", sb)
	}
	if op := rows[0]; op.Nickname != nil || op.DisplayName == nil || *op.DisplayName != "old-plan" || op.BaseCurrency != nil {
		t.Errorf("a declaration with no nickname is displayed by its id: %+v", op)
	}
	if got := c.SpendRules()[0].Far; got != "savings-bank" {
		t.Errorf("spending rule far = %q", got)
	}
	if got := c.IncomeRules()[0].Far; got != "savings-bank" {
		t.Errorf("income rule far = %q", got)
	}
}

// TestDeclaredAccountsRejects pins the refusals: a source may not take
// the reserved id, a declaration needs both the kind and the wrapper in
// their enums, and a `far` is admitted beside `internal_transfer`
// alone and only where it names a declaration.
func TestDeclaredAccountsRejects(t *testing.T) {
	base := `"gold_db": "/tmp/x", "default_currency": "USD",
        "silver_sources": [{"id": "bank", "kind": "chase", "path": "/tmp/b.db"}]`
	for _, tc := range []struct{ name, body, want string }{
		{"reserved source id",
			`"gold_db": "/tmp/x", "default_currency": "USD",
             "silver_sources": [{"id": "declared", "kind": "chase", "path": "/tmp/b.db"}]`,
			"reserved"},
		{"missing kind", base + `, "declared_accounts": {"x": {"tax_wrapper": "taxable_personal"}}`,
			"account_kind is required"},
		{"bad kind", base + `, "declared_accounts": {"x": {"account_kind": "wallet", "tax_wrapper": "taxable_personal"}}`,
			"invalid account_kind"},
		{"missing wrapper", base + `, "declared_accounts": {"x": {"account_kind": "cash"}}`,
			"tax_wrapper is required"},
		{"bad wrapper", base + `, "declared_accounts": {"x": {"account_kind": "cash", "tax_wrapper": "offshore"}}`,
			"invalid tax_wrapper"},
		{"bad id", base + `, "declared_accounts": {"my bank": {"account_kind": "cash", "tax_wrapper": "taxable_personal"}}`,
			"id must match"},
		{"bad currency", base + `, "declared_accounts": {"x": {"account_kind": "cash", "tax_wrapper": "taxable_personal", "currency": "dollars"}}`,
			"ISO 4217"},
		{"far beside a category", base + `, "declared_accounts": {"x": {"account_kind": "cash", "tax_wrapper": "taxable_personal"}},
             "spending": {"rules": [{"match": "EXAMPLE", "category": "investment", "far": "x"}]}`,
			"spending.rules[0].far is only meaningful beside category \"internal_transfer\""},
		{"far names nothing", base + `, "income": {"rules": [{"match": "EXAMPLE", "type": "internal_transfer", "far": "x"}]}`,
			"income.rules[0].far \"x\" names no declared_accounts entry"},
	} {
		_, err := Load(writeConfig(t, "{"+tc.body+"}"))
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want it to mention %q", tc.name, err, tc.want)
		}
	}
}
