package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"time"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/gold"
)

func init() {
	register("refdata-wanted", cmdRefdataWanted)
}

// cmdRefdataWanted prints the wanted list, the JSON document the refdata
// collector reads to learn which funds and securities gold wants
// reference data for (gold.WantedList; docs/adapters/refdata.md §5). It is
// the one way the collector reads gold. Read-only, and hidden from
// `wealthdb help`, like config-env: the collector runs it as
// `wealthdb -r refdata-wanted`.
func cmdRefdataWanted(ctx context.Context, g globalFlags, subargs []string, _ io.Reader, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("wealthdb refdata-wanted", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		fmt.Fprintln(stderr, `usage: wealthdb -r refdata-wanted

Print the wanted list the refdata collector reads, as JSON (contract
version `+fmt.Sprint(gold.WantedContractVersion)+`). Each subject it names lists what it wants,
one entry per fact, with the days gold holds it for. It names:
  - every fund gold wants: a held, proxy, reference, size or benchmark
    fund, or one a held fund holds. Each wants its composition over the
    days it is wanted for. A held fund also wants its facts, its
    dividend calendar among them; a benchmark's fund its monthly
    returns and characteristics.
  - every directly held security: its lookup, the attributes still
    missing, and the dividend calendar a stock or a listed REIT is
    wanted for; and whether gold reads it as a depositary receipt.
  - the securities the funds held today reach that lack a sector or a
    country, or, for a stock, recent earnings, dividends or financials.`)
	}
	if err := parseFlags(fs, subargs, "refdata-wanted"); err != nil {
		return err
	}
	if err := noPositionals(fs, "refdata-wanted"); err != nil {
		return err
	}
	cfg, err := g.loadConfig()
	if err != nil {
		return err
	}
	db, err := openGoldForRead(g, cfg)
	if err != nil {
		return err
	}
	defer db.Close()
	w, err := gold.RefdataWanted(ctx, db, time.Now())
	if err != nil {
		return err
	}
	enc := json.NewEncoder(stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(w.List())
}
