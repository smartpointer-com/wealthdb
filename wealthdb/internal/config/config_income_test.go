package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeIncomeCfg writes a config holding the given `income` block and
// loads it, returning the config or the load error.
func writeIncomeCfg(t *testing.T, body string) (*Config, error) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "wealthdb.cfg")
	cfg := `{"gold_db": "/tmp/g.db", "default_currency": "USD", ` +
		`"silver_sources": [{"id": "bank", "kind": "chase", "path": "/tmp/s.db"}], ` + body + `}`
	if err := os.WriteFile(path, []byte(cfg), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return Load(path)
}

// TestIncomeBlockParses pins the shape of `income`: its own account
// scope, rules whose value field is `type`, a pins path, and a
// categorization block.
func TestIncomeBlockParses(t *testing.T) {
	c, err := writeIncomeCfg(t, `"income": {
        "accounts": {"exclude": {"bank": ["ACC1"]}},
        "rules": [{"match": "EXAMPLE EMPLOYER", "type": "INCOME_SALARY"}],
        "pins": "/tmp/income_pins.csv",
        "categorization": {"context": "payer", "model": {"name": "m", "baseUrl": "http://localhost:1/v1", "api": "openai"}}
    }`)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	_, exclude := c.IncomeAccountScope()
	if got := exclude["bank"]; len(got) != 1 || got[0] != "ACC1" {
		t.Errorf("income exclude = %v, want [ACC1]", got)
	}
	rules := c.IncomeRules()
	if len(rules) != 1 || rules[0].Category != "INCOME_SALARY" {
		t.Fatalf("income rules = %+v, want one INCOME_SALARY rule", rules)
	}
	if !rules[0].Match.MatchString("example employer ag") {
		t.Error("the income rule did not compile case-insensitively")
	}
	if c.IncomePins() != "/tmp/income_pins.csv" {
		t.Errorf("IncomePins() = %q", c.IncomePins())
	}
	// `payer` is the income spelling of the narrowest level and
	// resolves to the one constant the pass compares against.
	if got := c.IncomeCategorization().ContextLevel(); got != SpendContextMerchant {
		t.Errorf("ContextLevel() = %q, want the narrowest level", got)
	}
}

// TestIncomeRuleTypeMustBeAnIncomeValue pins the fence between the two
// vocabularies at the one place a person's typo is still cheap to fix.
func TestIncomeRuleTypeMustBeAnIncomeValue(t *testing.T) {
	for _, tc := range []struct{ name, value, want string }{
		{"a spending value", "FOOD_AND_DRINK_GROCERIES", "income.rules[0].type"},
		{"a spending-only delta", "cash_withdrawal", "income.rules[0].type"},
		{"nonsense", "NOT_A_TYPE", "income.rules[0].type"},
		{"wrong case", "income_salary", "income.rules[0].type"},
	} {
		_, err := writeIncomeCfg(t, `"income": {"rules": [{"match": "X", "type": "`+tc.value+`"}]}`)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want one naming %s", tc.name, err, tc.want)
		}
	}
	// ...and the values that ARE income's, including the deltas both
	// families read.
	for _, value := range []string{"INCOME_SALARY", "INCOME_RENTAL", "INCOME_ALIMONY", "gift", "internal_transfer", "capital_return"} {
		if _, err := writeIncomeCfg(t, `"income": {"rules": [{"match": "X", "type": "`+value+`"}]}`); err != nil {
			t.Errorf("income rule %q rejected: %v", value, err)
		}
	}
}

// TestRetiredRuleTypeNamesWhatToWrite pins the refusal of a spelling an
// earlier taxonomy held. It fails the load like any other value outside
// the vocabulary, so a stale rule never writes a value no report shows,
// and the error names what replaced it, so the fix is one edit.
func TestRetiredRuleTypeNamesWhatToWrite(t *testing.T) {
	// The whole message once, at an index past the first, so the entry
	// can be found in a list of several.
	_, err := writeIncomeCfg(t, `"income": {"rules": [
        {"match": "A", "type": "INCOME_SALARY"},
        {"match": "B", "type": "INCOME_WAGES"}]}`)
	const want = `config: income.rules[1].type "INCOME_WAGES" is retired: ` +
		`use INCOME_SALARY, or INCOME_GIG_ECONOMY for gig-platform pay`
	if err == nil || err.Error() != want {
		t.Errorf("err = %v, want %q", err, want)
	}

	for _, tc := range []struct{ value, want string }{
		// A rename names the one value that replaced it.
		{"INCOME_RENT", `"INCOME_RENT" is retired: use INCOME_RENTAL`},
		{"INCOME_OTHER_INCOME", `"INCOME_OTHER_INCOME" is retired: use INCOME_OTHER`},
		{"INCOME_SELF_EMPLOYMENT", `"INCOME_SELF_EMPLOYMENT" is retired: use INCOME_CONTRACTOR`},
		// A split names each half with the case it covers: the old
		// spelling did not say which one a payer was, and a rule must.
		{"INCOME_ALIMONY_AND_CHILD_SUPPORT", `"INCOME_ALIMONY_AND_CHILD_SUPPORT" is retired: ` +
			`use INCOME_CHILD_SUPPORT for child support, or INCOME_ALIMONY for maintenance from a former partner`},
	} {
		_, err := writeIncomeCfg(t, `"income": {"rules": [{"match": "X", "type": "`+tc.value+`"}]}`)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want one containing %q", tc.value, err, tc.want)
		}
	}

	// The advice stays inside the family. A retired income spelling in
	// a SPENDING rule is not a spending value under either name, so it
	// takes the ordinary refusal rather than a pointer to an income
	// value the rule could not use either.
	_, err = writeIncomeCfg(t, `"spending": {"rules": [{"match": "X", "category": "INCOME_WAGES"}]}`)
	if err == nil || !strings.Contains(err.Error(), `category "INCOME_WAGES" is not a spend_detailed value`) {
		t.Errorf("a spending rule naming a retired income value: err = %v, want the ordinary refusal", err)
	}
}

// TestIncomeCategorizationInherits pins the one resolution: absent, the
// income model tier IS the spending one.
func TestIncomeCategorizationInherits(t *testing.T) {
	const spendBlock = `"spending": {"categorization": {"context": "descriptor",
        "model": {"name": "shared", "baseUrl": "http://localhost:1/v1", "api": "openai"}}}`

	// No income block at all.
	c, err := writeIncomeCfg(t, spendBlock)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got := c.IncomeCategorization().CategorizationModel(); got == nil || got.Name != "shared" {
		t.Error("income did not inherit the spending model")
	}
	if got := c.IncomeCategorizationKey(); got != "spending.categorization" {
		t.Errorf("error key = %q, want the block a reader would have to edit", got)
	}

	// An income block with no categorization inside it inherits too.
	c, err = writeIncomeCfg(t, spendBlock+`, "income": {"pins": "/tmp/p.csv"}`)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got := c.IncomeCategorization().CategorizationModel(); got == nil || got.Name != "shared" {
		t.Error("an income block without a categorization did not inherit")
	}
	if got := c.IncomeCategorization().ContextLevel(); got != SpendContextDescriptor {
		t.Errorf("inheritance is whole-block: ContextLevel() = %q, want descriptor", got)
	}

	// Its own block wins, whole.
	c, err = writeIncomeCfg(t, spendBlock+`, "income": {"categorization":
        {"model": {"name": "own", "baseUrl": "http://localhost:2/v1", "api": "openai"}}}`)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got := c.IncomeCategorization().CategorizationModel(); got == nil || got.Name != "own" {
		t.Error("income's own block did not win")
	}
	if got := c.IncomeCategorization().ContextLevel(); got != DefaultSpendContext {
		t.Errorf("ContextLevel() = %q: its own block is taken whole, not merged", got)
	}
	if got := c.IncomeCategorizationKey(); got != "income.categorization" {
		t.Errorf("error key = %q", got)
	}

	// Neither: nothing to run on, and the accessor says so.
	c, err = writeIncomeCfg(t, `"income": {"pins": "/tmp/p.csv"}`)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if c.IncomeCategorization() != nil {
		t.Error("IncomeCategorization() is not nil with neither block configured")
	}
}

// TestPersonNameFenceDefaultsOnAndIsInherited pins the one setting that
// decides whether a person's name may leave the machine.
//
// Three states, and the middle one is the whole reason the field is a
// pointer: absent must mean ON, and an explicit `false` must survive
// both the parse and a round-trip through the config writer, which a
// plain bool with omitempty would drop.
func TestPersonNameFenceDefaultsOnAndIsInherited(t *testing.T) {
	const model = `"model": {"name": "m", "baseUrl": "http://localhost:1/v1", "api": "openai"}`

	// Absent: on. A missing configuration must never be the thing that
	// sends a name to a third party.
	c, err := writeIncomeCfg(t, `"spending": {"categorization": {`+model+`}}`)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if !c.SpendCategorization().PersonNameFence() {
		t.Error("an absent fence_person_names resolved to off")
	}
	// ...and a nil block answers the same way, since the accessor is
	// what every caller reads.
	if !(*SpendingCategorization)(nil).PersonNameFence() {
		t.Error("a nil categorization block resolved to off")
	}

	// Explicit false: off, and it round-trips rather than being read
	// as absent.
	c, err = writeIncomeCfg(t, `"spending": {"categorization": {`+model+`, "fence_person_names": false}}`)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if c.SpendCategorization().PersonNameFence() {
		t.Error("an explicit fence_person_names=false was read as absent")
	}

	// Explicit true: on.
	c, err = writeIncomeCfg(t, `"spending": {"categorization": {`+model+`, "fence_person_names": true}}`)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if !c.SpendCategorization().PersonNameFence() {
		t.Error("an explicit fence_person_names=true was read as off")
	}

	// Income inherits it with the rest of the block. One household, one
	// answer to what may leave the machine — which is the point of
	// inheriting whole rather than per field.
	c, err = writeIncomeCfg(t, `"spending": {"categorization": {`+model+`, "fence_person_names": false}},
        "income": {"pins": "/tmp/p.csv"}`)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if c.IncomeCategorization().PersonNameFence() {
		t.Error("income did not inherit fence_person_names=false")
	}

	// ...and an income block of its own is taken whole, so it falls
	// back to the DEFAULT rather than to spending's value. That is the
	// same rule ContextLevel is held to above, and it errs safe here:
	// a half-inherited block cannot quietly turn the fence off.
	c, err = writeIncomeCfg(t, `"spending": {"categorization": {`+model+`, "fence_person_names": false}},
        "income": {"categorization": {`+model+`}}`)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if !c.IncomeCategorization().PersonNameFence() {
		t.Error("an income block of its own inherited spending's fence_person_names per field")
	}
}

// TestIncomeContextAcceptsBothSpellings is the reason ValidIncomeContext
// exists: a spending block inherited whole must validate under the
// income family's checker without being rewritten in income's words.
func TestIncomeContextAcceptsBothSpellings(t *testing.T) {
	for _, level := range []string{"payer", "merchant", "descriptor", "transaction", ""} {
		if !ValidIncomeContext(level) {
			t.Errorf("ValidIncomeContext(%q) = false", level)
		}
	}
	for _, level := range []string{"payers", "Merchant", "everything"} {
		if ValidIncomeContext(level) {
			t.Errorf("ValidIncomeContext(%q) = true", level)
		}
	}
	// `payer` is income's alone: a spending block may not borrow it,
	// since nothing on the outflow side has payers.
	if ValidSpendContext(SpendContextPayer) {
		t.Error("ValidSpendContext accepted the income spelling")
	}
}

// TestIncomeHasNoMatcherKnobs pins decision 8 at the config surface:
// there is one matcher, and a config that tries to give income its own
// fails rather than being quietly ignored.
func TestIncomeHasNoMatcherKnobs(t *testing.T) {
	for _, body := range []string{
		`"income": {"internal_transfer_matching": {"window_days": 9}}`,
		`"income": {"transfer_overrides": "/tmp/o.csv"}`,
	} {
		if _, err := writeIncomeCfg(t, body); err == nil {
			t.Errorf("%s was accepted; income has no matcher of its own", body)
		}
	}
}
