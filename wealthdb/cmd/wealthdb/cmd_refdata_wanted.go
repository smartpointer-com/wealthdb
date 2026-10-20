package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"time"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/config"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/gold"
)

func init() {
	register("refdata-wanted", cmdRefdataWanted)
}

// cmdRefdataWanted prints the wanted list, the JSON document the refdata
// collector reads to learn which funds and securities gold wants
// reference data for (gold.Wanted; docs/adapters/refdata.md §5). It is
// the one way the collector reads gold. Read-only, and hidden from
// `wealthdb help`, like mcp-config: the collector runs it as
// `wealthdb -r refdata-wanted`.
func cmdRefdataWanted(ctx context.Context, g globalFlags, subargs []string, _ io.Reader, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("wealthdb refdata-wanted", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		fmt.Fprintln(stderr, `usage: wealthdb -r refdata-wanted

Print the wanted list the refdata collector reads, as JSON (contract
version `+fmt.Sprint(gold.WantedContractVersion)+`). It names:
  - every fund gold wants: a held, proxy, reference or size fund, or
    one a held fund holds. Each lists the days it is wanted for, the
    days gold has a composition for and the days gold has each fact
    for. A held fund also lists the facts it is wanted for.
  - every directly held security, with the attributes still missing.
  - the securities the funds held today reach that lack a sector or a
    country, or, for a stock, recent earnings, dividends or financials.`)
	}
	if err := parseFlags(fs, subargs, "refdata-wanted"); err != nil {
		return err
	}
	if err := noPositionals(fs, "refdata-wanted"); err != nil {
		return err
	}
	cfg, err := config.Load(g.ConfigPath)
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
	return enc.Encode(w)
}
