package main

import (
	"database/sql"
	"fmt"
	"strings"
	"testing"
)

// Every verb that loads stamps the config's yield block into gold and
// says how many entries it stamped; a load without the block leaves the
// built-in conventions and cycles and the default horizon alone.
func TestLoadStampsTheYieldBlock(t *testing.T) {
	t.Parallel()
	for _, args := range [][]string{{"load", "schwab-test"}, {"reload", "schwab-test"}, {"reload", "-a"}} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			t.Parallel()
			cfgPath := setupCLITest(t)
			writeCLIConfig(t, cfgPath, `, "yield": {
                "curves": {"USD": {"3M": "schwab-test:DGS3MO", "10Y": "schwab-test:DGS10"}},
                "withholding": {"US": {"default": 30}},
                "rating_funds": {"isin:XX0000000004": "AAA-A"},
                "conventions": {"USD": {"frequency": 4, "day_count": "ACT/365"}},
                "horizon_months": 24,
                "cycles": {"utilities": "sensitive"}}`)
			if _, se, code := run(t, "-c", cfgPath, "init"); code != 0 {
				t.Fatalf("init failed: %s", se)
			}
			so, se, code := run(t, append([]string{"-c", cfgPath}, args...)...)
			if code != 0 {
				t.Fatalf("exit %d, stderr:\n%s", code, se)
			}
			// Two curve points, a rate, a rating fund, and the config's
			// convention and cycle.
			if want := args[0] + ": 6 yield entr(ies) stamped"; !strings.Contains(so, want) {
				t.Errorf("stdout lacks %q:\n%s", want, so)
			}
			if got := yieldState(t, goldPathFromCfg(cfgPath)); got != "2 curves, 1 rates, 1 funds, 1+1 config, horizon 24" {
				t.Errorf("gold after %s: %s", args[0], got)
			}

			writeCLIConfig(t, cfgPath, "")
			so, se, code = run(t, "-c", cfgPath, "load", "schwab-test")
			if code != 0 || strings.Contains(so, "yield entr(ies)") {
				t.Fatalf("exit %d, stdout:\n%s\nstderr:\n%s", code, so, se)
			}
			if got := yieldState(t, goldPathFromCfg(cfgPath)); got != "0 curves, 0 rates, 0 funds, 0+0 config, horizon 12" {
				t.Errorf("gold after a load without the block: %s", got)
			}
		})
	}
}

// yieldState is what the yield tables hold, in counts: curve points,
// withholding rates, rating funds, conventions and cycles the config
// sets, and the horizon.
func yieldState(t *testing.T, goldPath string) string {
	t.Helper()
	db, err := sql.Open("duckdb", goldPath+"?access_mode=read_only")
	if err != nil {
		t.Fatalf("open gold read-only: %v", err)
	}
	defer db.Close()
	var curves, rates, funds, conventions, cycles, builtin, horizon int
	if err := db.QueryRow(`SELECT
        (SELECT COUNT(*) FROM yield_curves), (SELECT COUNT(*) FROM yield_withholding),
        (SELECT COUNT(*) FROM yield_rating_funds),
        (SELECT COUNT(*) FROM yield_conventions WHERE origin = 'config'),
        (SELECT COUNT(*) FROM yield_cycles WHERE origin = 'config'),
        (SELECT COUNT(*) FROM yield_conventions WHERE origin = 'builtin') +
            (SELECT COUNT(*) FROM yield_cycles WHERE origin = 'builtin'),
        (SELECT horizon_months FROM yield_settings)`).
		Scan(&curves, &rates, &funds, &conventions, &cycles, &builtin, &horizon); err != nil {
		t.Fatalf("read the yield tables: %v", err)
	}
	// The built-in maps fill every row the config does not set: five
	// conventions and eleven cycles in all.
	if conventions+cycles+builtin != 5+11 {
		t.Errorf("%d conventions and cycles in all, want 16", conventions+cycles+builtin)
	}
	return fmt.Sprintf("%d curves, %d rates, %d funds, %d+%d config, horizon %d", curves, rates, funds, conventions,
		cycles, horizon)
}
