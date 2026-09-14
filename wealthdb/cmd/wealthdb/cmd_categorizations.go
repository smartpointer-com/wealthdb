package main

import (
	"context"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"

	"github.com/ptu-gh/wealthdb/wealthdb/internal/config"
	"github.com/ptu-gh/wealthdb/wealthdb/internal/errs"
	"github.com/ptu-gh/wealthdb/wealthdb/internal/gold"
	"github.com/ptu-gh/wealthdb/wealthdb/internal/output"
	"github.com/ptu-gh/wealthdb/wealthdb/internal/pathmode"
)

func init() {
	register("categorizations", cmdCategorizations)
}

// signatureList is the repeatable --forget flag: every occurrence
// appends one merchant signature, verbatim. A signature named twice
// is kept once, so it is reported once as removed rather than once as
// removed and once as missing.
type signatureList []string

func (l *signatureList) String() string { return strings.Join(*l, ", ") }

func (l *signatureList) Set(v string) error {
	if v == "" {
		return errors.New("a signature cannot be empty")
	}
	for _, have := range *l {
		if have == v {
			return nil
		}
	}
	*l = append(*l, v)
	return nil
}

// cmdCategorizations dumps the spend_merchant_categories table — the
// model-derived merchant verdicts the spending report macros COALESCE
// in behind the per-transaction ones — and, with --forget, removes
// verdicts from it. The dump is read-only, the resolutions
// counterpart for the spending overlay; --forget is the one write,
// and the only way short of hand-editing gold to undo a verdict the
// model got wrong at merchant scope.
//
// The table has no source column and takes no source filter: a
// merchant is the same merchant whichever card met it, which is the
// deliberate keying difference from symbol_resolutions.
func cmdCategorizations(ctx context.Context, g globalFlags, subargs []string, _ io.Reader, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("wealthdb categorizations", flag.ContinueOnError)
	fs.SetOutput(stderr)
	format := fs.String("f", "table", "output format: table | csv | csv_plain | json")
	fs.StringVar(format, "format", "table", "output format: table | csv | csv_plain | json")
	categoryFilter := fs.String("d", "", "filter to one taxonomy value (spend_detailed or income_detailed)")
	fs.StringVar(categoryFilter, "detailed", "", "filter to one taxonomy value (spend_detailed or income_detailed)")
	var forget signatureList
	fs.Var(&forget, "forget", "remove the verdict stored at this signature (repeatable)")
	dryRun := fs.Bool("n", false, "with --forget: print what would be removed, write nothing")
	fs.BoolVar(dryRun, "dry-run", false, "with --forget: print what would be removed, write nothing")
	privacy := fs.Bool("p", false, "redact the signature and name columns in the dump")
	fs.BoolVar(privacy, "privacy", false, "redact the signature and name columns in the dump")
	fs.Usage = func() {
		fmt.Fprintln(stderr, `usage: wealthdb categorizations [spending | income] [-d DETAILED] [-f FORMAT] [-p]
       wealthdb categorizations [spending | income] --forget SIGNATURE [--forget SIGNATURE ...] [-n | --dry-run]

Dump every row in the verdict stores — spend_merchant_categories and
income_payer_categories, the merchant and payer verdicts 'wealthdb
categorize' bought from the configured model, which each family's
report macros apply to every transaction carrying the matching
signature. The positional selects one family; with none, BOTH are
dumped, told apart by a 'family' column.

The stores are GLOBAL: each is keyed by signature alone, with no
silver source, because a merchant is the same merchant whichever card
met it and a payer the same payer whichever account it paid. That is
why there is no -s flag here and one on 'resolutions'.

--forget removes the verdict stored at a signature, given exactly as
the dump's signature column shows it (quote it: a signature is
upper-cased tokens separated by spaces). With no family it removes
from BOTH stores — one counterparty can sit in each with a different
verdict — and a family narrows it to that store. It is how a wrong
counterparty-scope verdict is undone: the next 'wealthdb categorize'
run re-asks a forgotten signature, because the backlog is whatever the
store does not cover. A signature with no verdict is reported and is
not an error. It needs write access to the gold database; --dry-run
prints what would be removed and writes nothing.

Flags:
  -d, --detailed VALUE    filter the dump to one taxonomy value —
                          spend_detailed or income_detailed; a value of
                          one family simply matches nothing in the other
  -f, --format FORMAT     table | csv | csv_plain | json (default: table)
  -p, --privacy           redact the signature and name columns — the raw
                          narrative's fold and the name taken off it, both free text
                          (the cell masks whole). The category, version, date and
                          model stay legible. --forget needs the verbatim signature,
                          so run the dump without -p to read one
      --forget SIGNATURE  remove the verdict at this signature (repeatable)
  -n, --dry-run           with --forget: print what would be removed, write nothing

The flags may appear before or after the family positional.

signature_version records which revision of the signature
normalisation produced the key. When that version is bumped the
enrichment pass carries a verdict onto the new key only if every row
that carried the old key moved to the same new one; a verdict whose
rows split across several new keys is left behind, unreachable, and
'load' reports the count. Forgetting such a key BEFORE the bump is the
clean way to keep it from carrying anywhere; forgetting it after
merely tidies the store. model_name is the model that emitted the
verdict.`)
	}
	if err := fs.Parse(reorderFlagsFirst(subargs, categorizeValueFlags)); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return errs.Newf(2, "categorizations: bad flags")
	}
	// The optional family positional. More than one is a typo rather
	// than a selection, and an unknown one is rejected by the resolver
	// below — the old blanket "no positionals" guard predates the
	// selector and would have made it unreachable.
	if fs.NArg() > 1 {
		fs.Usage()
		return errs.Newf(2, "categorizations: unexpected positional argument %q", fs.Arg(1))
	}

	if len(forget) > 0 {
		// The dump's flags have no meaning for a removal; refusing them
		// is cheaper than guessing which of the two was meant.
		var dumpFlag string
		fs.Visit(func(f *flag.Flag) {
			switch f.Name {
			case "d", "detailed", "f", "format", "p", "privacy":
				if dumpFlag == "" {
					dumpFlag = "-" + f.Name
				}
			}
		})
		if dumpFlag != "" {
			return errs.Newf(2, "categorizations: %s applies to the dump, not to --forget", dumpFlag)
		}
		forgetFamilies, ok := resolveCategorizeFamilies(fs.Arg(0))
		if !ok {
			fs.Usage()
			return errs.Newf(2, "categorizations: unknown family %q (want spending | income, or neither for both)",
				strings.Join(fs.Args(), " "))
		}
		cfg, err := config.Load(g.ConfigPath)
		if err != nil {
			return err
		}
		return forgetCategorizations(ctx, g, cfg, forgetFamilies, forget, *dryRun, stdout)
	}
	if *dryRun {
		return errs.Newf(2, "categorizations: --dry-run applies to --forget only")
	}

	fmtChoice, err := output.Parse(*format)
	if err != nil {
		return errs.Newf(2, "categorizations: %s", err.Error())
	}

	families, ok := resolveCategorizeFamilies(fs.Arg(0))
	if !ok {
		fs.Usage()
		return errs.Newf(2, "categorizations: unknown family %q (want spending | income, or neither for both)",
			strings.Join(fs.Args(), " "))
	}

	cfg, err := config.Load(g.ConfigPath)
	if err != nil {
		return err
	}

	db, err := openGoldForRead(g, cfg, "gold database %q does not exist. Run 'wealthdb init' first.")
	if err != nil {
		return err
	}
	defer db.Close()

	// One dump over both stores, each row saying which family it came
	// from. A signature can be in BOTH — one counterparty can be a
	// merchant and a payer — and the family column is what tells the
	// two rows apart.
	var dump []categorizationRow
	for _, fam := range families {
		q := `SELECT ` + fam.signatureColumn + `, ` + fam.storeNameColumn + `, ` + fam.valueColumn + `,
                 signature_version, assigned_at, model_name
            FROM ` + fam.storeTable
		args := []any{}
		if *categoryFilter != "" {
			q += ` WHERE ` + fam.valueColumn + ` = ?`
			args = append(args, *categoryFilter)
		}
		q += ` ORDER BY ` + fam.signatureColumn

		rows, err := db.QueryContext(ctx, q, args...)
		if err != nil {
			return fmt.Errorf("categorizations: %w", err)
		}
		for rows.Next() {
			r := categorizationRow{Family: fam.name}
			if err := rows.Scan(&r.Signature, &r.Name, &r.Detailed,
				&r.Version, &r.AssignedAt, &r.ModelName); err != nil {
				rows.Close()
				return fmt.Errorf("categorizations scan: %w", err)
			}
			dump = append(dump, r)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return err
		}
		rows.Close()
	}
	return writeFormatted(stdout, fmtChoice,
		rowsToTable(dump, categorizationColumns(), *privacy, fmtChoice))
}

// categorizationRow is one verdict-store row as the dump renders it.
type categorizationRow struct {
	// Family is which store the row came from: `spending` or
	// `income`. One counterparty can be in both, with two different
	// verdicts, and this is what says which is which.
	Family     string
	Signature  string
	Name       string
	Detailed   string
	Version    int
	AssignedAt int64
	ModelName  string
}

// categorizationColumns is the dump's column registry, so the stores
// redact under -p by the same classes the report views use.
//
// The three leading columns are named for neither family. One dump now
// carries both stores, and a header saying `merchant_signature` over a
// payer's row would be wrong on half the output; `family` is what says
// which vocabulary a row's value belongs to.
//
// `signature` is a fold of the raw statement narrative and carries
// whatever that narrative carried — a creditor's postal address is
// deliberately kept in it — so it takes the free-text class, which
// masks the cell whole. `name` goes with it: the fence gates what may
// reach these stores, but they are append-only across signature
// revisions and across widenings of the fence itself, so a name bought
// while the fence was narrower is still here. What stays legible is
// vocabulary — the category, the version, the date and the model.
func categorizationColumns() []columnSpec[categorizationRow] {
	return []columnSpec[categorizationRow]{
		{Name: "family", Align: output.AlignLeft,
			Extract: func(r categorizationRow) string { return r.Family }},
		{Name: "signature", Align: output.AlignLeft, Privacy: PrivacyFreeText,
			Extract: func(r categorizationRow) string { return r.Signature }},
		{Name: "name", Align: output.AlignLeft, Privacy: PrivacyFreeText,
			Extract: func(r categorizationRow) string { return r.Name }},
		{Name: "detailed", Align: output.AlignLeft,
			Extract: func(r categorizationRow) string { return r.Detailed }},
		{Name: "signature_version", Align: output.AlignRight,
			Extract: func(r categorizationRow) string { return fmt.Sprintf("%d", r.Version) }},
		{Name: "assigned_at", Align: output.AlignLeft,
			Extract: func(r categorizationRow) string { return formatDate(r.AssignedAt) }},
		{Name: "model_name", Align: output.AlignLeft,
			Extract: func(r categorizationRow) string { return r.ModelName }},
	}
}

// rowQuerier is the one read the removal makes, satisfied by the
// database on a dry run and by the transaction on a real one.
type rowQuerier interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// forgetCategorizations removes the named verdicts from the merchant
// store, matched by exact signature, in one transaction: the list is
// removed whole or not at all. Its gate and its dry-run shape follow
// resolve-symbols and categorize — the read-only rejection names the
// dry run, and the dry run opens gold read-only so it takes no write
// lock. A miss is reported on stdout beside the removals and is not an
// error: a run may be clearing a key an earlier --forget already
// emptied, or one the last re-key moved on from.
func forgetCategorizations(ctx context.Context, g globalFlags, cfg *config.Config, families []categorizeFamily, signatures []string, dryRun bool, stdout io.Writer) error {
	dec, err := pathmode.Detect(cfg.GoldDB, g.ForceReadOnly, false)
	if err != nil {
		return errs.Wrap(errs.ExitOpenFailed, err)
	}
	if !dec.DBExists {
		return errs.Newf(errs.ExitMissingDB, "gold database %q does not exist. Run 'wealthdb init' first.", cfg.GoldDB)
	}
	if !dryRun && dec.Mode != pathmode.ModeReadWrite {
		return errs.Newf(errs.ExitRWNeeded,
			"'categorizations --forget' requires write access to the gold database, but '%s' is read-only (detected: %s). "+
				"Pass --dry-run if you only want to see what would be removed.", cfg.GoldDB, dec.Reason)
	}
	openMode := gold.ModeReadWrite
	if dryRun {
		openMode = gold.ModeReadOnly
	}
	// Only the real removal takes the gold write mutex; the dry run
	// stays a pure read, as its rejection message promises. Without
	// it, a removal committed while 'compact' or 'reload -a' is
	// rebuilding lands in the inode the swap unlinks and is reported
	// as done.
	if !dryRun {
		lock, err := lockGoldForWrite(cfg.GoldDB, "categorizations --forget")
		if err != nil {
			return err
		}
		defer lock.unlock()
	}
	db, err := gold.Open(cfg.GoldDB, openMode)
	if err != nil {
		return errs.Wrap(errs.ExitOpenFailed, err)
	}
	defer db.Close()

	var (
		q  rowQuerier = db
		tx *sql.Tx
	)
	if !dryRun {
		if tx, err = db.BeginTx(ctx, nil); err != nil {
			return fmt.Errorf("categorizations: begin: %w", err)
		}
		defer func() { _ = tx.Rollback() }() // a no-op after Commit
		q = tx
	}

	// A signature names a COUNTERPARTY, and one counterparty can be in
	// both stores with two different verdicts. With no family named,
	// forgetting removes it from both and says so per store; with one
	// named, only from that store.
	removed, missing := 0, 0
	for _, sig := range signatures {
		found := false
		for _, fam := range families {
			var name, detailed string
			err := q.QueryRowContext(ctx, `
            SELECT `+fam.storeNameColumn+`, `+fam.valueColumn+` FROM `+fam.storeTable+`
             WHERE `+fam.signatureColumn+` = ?`, sig).Scan(&name, &detailed)
			switch {
			case errors.Is(err, sql.ErrNoRows):
				continue
			case err != nil:
				return fmt.Errorf("categorizations: look up %q in the %s store: %w", sig, fam.name, err)
			}
			found = true
			verb := "forgot"
			if dryRun {
				verb = "would forget"
			} else if _, err := tx.ExecContext(ctx,
				`DELETE FROM `+fam.storeTable+` WHERE `+fam.signatureColumn+` = ?`, sig); err != nil {
				return fmt.Errorf("categorizations: forget %q from the %s store: %w", sig, fam.name, err)
			}
			removed++
			fmt.Fprintf(stdout, "categorizations: %s %q — %s [%s] (%s)\n", verb, sig, name, detailed, fam.name)
		}
		if !found {
			missing++
			fmt.Fprintf(stdout, "categorizations: no verdict stored at %q\n", sig)
		}
	}

	if dryRun {
		fmt.Fprintf(stdout, "categorizations: dry-run — %d verdict(s) would be removed, %d not found; nothing written\n",
			removed, missing)
		return nil
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("categorizations: commit: %w", err)
	}
	fmt.Fprintf(stdout, "categorizations: %d verdict(s) removed, %d not found; the next 'categorize' run re-asks the removed signatures\n",
		removed, missing)
	return nil
}
