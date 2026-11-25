package main

import (
	"context"
	"database/sql"
	"fmt"
	"io"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/config"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/gold"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/silver"
)

// fxSourceName is the display name of the collector the setup offers
// for exchange rates, as the catalogue states it; empty when no adapter
// registers it.
func fxSourceName() string {
	c, _ := silver.CollectorNamed("fred")
	return c.Display
}

// unconvertibleMoney are the currencies of money gold holds amounts in
// that no exchange rate connects to target: gold.Unconvertible, less
// every code that is no currency of money (canonical.IsMoneyCurrency).
// A token with no price is another condition: a source of exchange
// rates has no rate for it, so the notice does not name it. A target
// that is no currency of money gives none either.
func unconvertibleMoney(ctx context.Context, db *sql.DB, target string) ([]string, error) {
	if !canonical.IsMoneyCurrency(target) {
		return nil, nil
	}
	all, err := gold.Unconvertible(ctx, db, target)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, c := range all {
		if canonical.IsMoneyCurrency(c) {
			out = append(out, c)
		}
	}
	return out, nil
}

// fxNotice says that gold holds amounts in the currencies missing with
// no exchange rate to target (unconvertibleMoney), so every figure in
// target leaves them out, and names the two ways out: the setup command
// of the config at configPath (setupCommand), which adds a source of
// exchange rates, and a report in a currency of the amounts. Empty for
// no missing currency. Every command that says this builds the sentence
// here, and puts its own prefix before it.
func fxNotice(configPath string, missing []string, target string) string {
	if len(missing) == 0 {
		return ""
	}
	from := joinAnd(missing)
	add := "Add exchange rates with '" + setupCommand(configPath) + " banks'"
	if name := fxSourceName(); name != "" {
		add += " (" + name + ")"
	}
	other := fmt.Sprintf("or report in %s with -x %s", missing[0], missing[0])
	if len(missing) > 1 {
		other = fmt.Sprintf("or report in one of them, such as %s with -x %s", missing[0], missing[0])
	}
	return fmt.Sprintf("no exchange rate from %s to %s: amounts in %s are missing from every figure in %s. %s, %s.",
		from, target, from, target, add, other)
}

// warnUnconvertible writes fxNotice for gold and target to w as a line
// that starts with prefix, when gold holds a currency of money that
// does not convert. A check that fails says nothing: the command it rides on
// reports what is wrong with the database.
func warnUnconvertible(ctx context.Context, db *sql.DB, configPath, target, prefix string, w io.Writer) {
	missing, err := unconvertibleMoney(ctx, db, target)
	if err != nil {
		return
	}
	if notice := fxNotice(configPath, missing, target); notice != "" {
		fmt.Fprintf(w, "%s: %s\n", prefix, notice)
	}
}

// openGoldForReport is openGoldForRead for a report in the output
// currency: once gold is open, it says on stderr which currencies gold
// holds amounts in that do not convert to that currency. A report opens
// gold once, so the line is said once.
func openGoldForReport(ctx context.Context, g globalFlags, cfg *config.Config, currency string, stderr io.Writer) (*sql.DB, error) {
	db, err := openGoldForRead(g, cfg)
	if err == nil {
		warnUnconvertible(ctx, db, g.ConfigPath, currency, "wealthdb", stderr)
	}
	return db, err
}

// unconvertibleNote is the same fact for the header of an MCP result,
// in the words of a call: the currency parameter in place of -x, and no
// command, which the model cannot run.
func unconvertibleNote(missing []string, target string) string {
	if len(missing) == 0 {
		return ""
	}
	from := joinAnd(missing)
	shows := "currency=" + missing[0] + " shows them"
	if len(missing) > 1 {
		shows = "a call in one of them, such as currency=" + missing[0] + ", shows its amounts"
	}
	return fmt.Sprintf("no exchange rate from %s to %s: amounts in %s are missing from every figure in %s (%s)",
		from, target, from, target, shows)
}
