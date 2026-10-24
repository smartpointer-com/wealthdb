package config

import (
	"strconv"
	"strings"
	"testing"
)

// TestCashflowBlockParses pins the shape: one block, three fields, all
// optional, and an accessor for each.
func TestCashflowBlockParses(t *testing.T) {
	c, err := loadWith(t, `"cashflow": {
        "accounts": {"exclude": {"bank": ["ACC1", "ACC2"]}},
        "wrappers": {"529": "household", "trust_grantor": "trusts"},
        "projection": {"lookback_months": 24}
    }`)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	include, exclude := c.CashflowAccountScope()
	if include != nil {
		t.Errorf("the pool has an include map: %v — every account is pooled by default", include)
	}
	if got := exclude["bank"]; len(got) != 2 || got[0] != "ACC1" || got[1] != "ACC2" {
		t.Errorf("pool exclusions = %v, want [ACC1 ACC2]", got)
	}
	if got := c.CashflowWrappers(); len(got) != 2 ||
		got["529"] != "household" || got["trust_grantor"] != "trusts" {
		t.Errorf("wrapper overrides = %v", got)
	}
	if got := c.CashflowLookback(); got != 24 {
		t.Errorf("lookback = %d, want 24", got)
	}
}

// TestCashflowBlockIsOptional pins the absent-block default: every
// account pooled, and the engine's own boundary.
func TestCashflowBlockIsOptional(t *testing.T) {
	c, err := loadWith(t, `"web": {"enabled": false}`)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if include, exclude := c.CashflowAccountScope(); include != nil || exclude != nil {
		t.Errorf("an absent block scopes something: %v / %v", include, exclude)
	}
	if got := c.CashflowWrappers(); got != nil {
		t.Errorf("an absent block overrides a wrapper: %v", got)
	}
	if got := c.CashflowLookback(); got != DefaultCashflowLookbackMonths {
		t.Errorf("an absent block's lookback = %d, want %d", got, DefaultCashflowLookbackMonths)
	}
}

// The plan's look-back is 1 to 36 months, or 0 for the default.
func TestCashflowLookbackValidation(t *testing.T) {
	for _, n := range []int{-1, MaxCashflowLookbackMonths + 1} {
		_, err := loadWith(t, `"cashflow": {"projection": {"lookback_months": `+strconv.Itoa(n)+`}}`)
		if err == nil || !strings.Contains(err.Error(), "cashflow.projection.lookback_months") {
			t.Errorf("lookback_months %d: %v", n, err)
		}
	}
	if _, err := loadWith(t, `"cashflow": {"projection": {"lookback_months": 36}}`); err != nil {
		t.Errorf("lookback_months 36: %v", err)
	}
}

// TestCashflowAccountsHasNoIncludeField pins the decision behind the
// one-sided block: every account is pooled by default, so an `include`
// could fence nothing, and a knob that does nothing is worse than one
// that is absent. The loader disallows unknown fields, so writing it is
// an error rather than a silent no-op.
func TestCashflowAccountsHasNoIncludeField(t *testing.T) {
	_, err := loadWith(t, `"cashflow": {"accounts": {"include": {"bank": ["ACC1"]}}}`)
	if err == nil {
		t.Fatal("cashflow.accounts.include was accepted")
	}
	if !strings.Contains(err.Error(), "include") {
		t.Errorf("error does not name the field: %v", err)
	}
}

// TestCashflowWrapperValidation pins both halves of the vocabulary
// check, and that each error names the entry a reader has to edit.
func TestCashflowWrapperValidation(t *testing.T) {
	for _, tc := range []struct {
		name, body, wants string
	}{
		{"unknown wrapper", `"cashflow": {"wrappers": {"pillar_4": "retirement"}}`, "pillar_4"},
		{"unknown destination", `"cashflow": {"wrappers": {"hsa": "medical"}}`, "medical"},
		// `unpaired` is where a crossing goes when there is no far
		// account to read a wrapper off, so no wrapper can be sent there.
		{"unpaired is not a destination", `"cashflow": {"wrappers": {"hsa": "unpaired"}}`, "unpaired"},
		{"a side is not a destination", `"cashflow": {"wrappers": {"hsa": "vehicle"}}`, "vehicle"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := loadWith(t, tc.body)
			if err == nil {
				t.Fatalf("%s was accepted", tc.body)
			}
			if !strings.Contains(err.Error(), tc.wants) {
				t.Errorf("error %q does not name %q", err, tc.wants)
			}
		})
	}
}

// TestCashflowWrapperDestinationsAreAllAccepted walks the whole
// vocabulary, so a destination added to canonical without a config
// story fails here rather than at the stamp.
func TestCashflowWrapperDestinationsAreAllAccepted(t *testing.T) {
	for _, dest := range []string{"household", "retirement", "education", "health", "trusts", "giving"} {
		if _, err := loadWith(t,
			`"cashflow": {"wrappers": {"hsa": "`+dest+`"}}`); err != nil {
			t.Errorf("destination %q was refused: %v", dest, err)
		}
	}
}

// TestCashflowPoolExclusionValidation pins the checks the account scope
// shares with the two families': the source must be declared, and an id
// may not be listed twice — a repeat would raise a primary-key
// violation mid-load, after every source had already been written.
func TestCashflowPoolExclusionValidation(t *testing.T) {
	if _, err := loadWith(t,
		`"cashflow": {"accounts": {"exclude": {"nosuch": ["ACC1"]}}}`); err == nil {
		t.Error("an undeclared source was accepted")
	}
	if _, err := loadWith(t,
		`"cashflow": {"accounts": {"exclude": {"bank": ["ACC1", "ACC1"]}}}`); err == nil {
		t.Error("a repeated id was accepted")
	}
}
