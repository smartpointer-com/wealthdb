package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/gold"
)

// setupGainsGold seeds a gold DB with what the gains views read: a
// holding at two snapshots with a cost basis and open lots, a sale's
// realized lots in two documents, a sell, and a USD→CHF rate. Every id
// and figure is invented.
func setupGainsGold(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	goldPath := filepath.Join(dir, "wealthdb.db")
	db, err := gold.OpenFresh(goldPath)
	if err != nil {
		t.Fatalf("open gold: %v", err)
	}
	if _, err := db.ExecContext(context.Background(), `
        INSERT INTO silver_sources(silver_source_id, silver_kind, silver_path, high_watermark, first_loaded_at, last_loaded_at)
            VALUES ('brk', 'schwab', '/tmp/brk.db', -1, 0, 0);
        INSERT INTO fx_rates (silver_source_id, snapshot_at, base_currency, quote_currency, mid_rate)
            VALUES ('brk', 0, 'USD', 'CHF', 0.9);
        INSERT INTO accounts (silver_source_id, account_external_id, account_kind, display_name, tax_wrapper,
                              first_seen_at, last_seen_at)
            VALUES ('brk', 'ACC1', 'brokerage', 'Brokerage', 'taxable_joint', 1, 1);
        INSERT INTO instruments (silver_source_id, instrument_external_id, asset_class, symbol, name,
                                 currency, first_seen_at, last_seen_at) VALUES
            ('brk', 'AAA', 'public_equity', 'AAA', 'Alpha', 'USD', 1, 1),
            ('brk', 'BBB', 'public_equity', 'BBB', 'Beta',  'USD', 1, 1);
        INSERT INTO positions (silver_source_id, snapshot_at, account_external_id, position_key,
                               instrument_external_id, asset_class, vehicle, currency, quantity,
                               market_value, book_value, basis_origin, basis_method, basis_fees) VALUES
            ('brk', 1000,   'ACC1', 'AAA', 'AAA', 'public_equity', 'stock', 'USD', 10, 1000, 600, 'stated', 'lots', 'included'),
            ('brk', 1000,   'ACC1', 'BBB', 'BBB', 'public_equity', 'stock', 'USD', 5,  500,  400, 'stated', 'lots', 'included'),
            ('brk', 200000, 'ACC1', 'AAA', 'AAA', 'public_equity', 'stock', 'USD', 10, 1300, 600, 'stated', 'lots', 'included');
        INSERT INTO position_lots (silver_source_id, snapshot_at, account_external_id, position_key, lot_key,
                                   currency, quantity, book_value, acquisition_date, term, basis_origin) VALUES
            ('brk', 200000, 'ACC1', 'AAA', 'L1', 'USD', 4, 200, DATE '1969-01-01', 'long',  'stated'),
            ('brk', 200000, 'ACC1', 'AAA', 'L2', 'USD', 6, 400, DATE '1969-12-01', 'short', 'stated');
        INSERT INTO realized_lots (silver_source_id, realized_lot_external_id, account_external_id,
                                   instrument_external_id, description, document_kind, tax_year,
                                   acquired_various, disposal_date, currency, quantity, proceeds,
                                   book_value, realized_gain_loss, term, basis_origin, basis_method,
                                   basis_fees, is_primary) VALUES
            ('brk', 'R1', 'ACC1', 'BBB', 'BETA', 'form_1099b',       1970, FALSE, DATE '1970-01-02', 'USD', 5, 550, 400, NULL, 'short', 'stated', 'lots', 'included', TRUE),
            ('brk', 'R2', 'ACC1', 'BBB', 'BETA', 'year_end_summary', 1970, FALSE, DATE '1970-01-02', 'USD', 5, 550, 400, 150,  'short', 'stated', 'lots', 'included', FALSE);
        INSERT INTO transactions (silver_source_id, transaction_external_id, occurred_at,
                                  account_external_id, instrument_external_id, kind, currency, net_amount)
            VALUES ('brk', 'S1', 90000, 'ACC1', 'BBB', 'sell', 'USD', 550);
    `); err != nil {
		t.Fatalf("seed gains gold: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close gold: %v", err)
	}
	cfg := filepath.Join(dir, "wealthdb.cfg")
	body := fmt.Sprintf(`{"gold_db": %q, "default_currency": "USD", "silver_sources": []}`, goldPath)
	if err := os.WriteFile(cfg, []byte(body), 0o644); err != nil {
		t.Fatalf("write cfg: %v", err)
	}
	return cfg
}

// gainsCases runs every gains view over the fixture's year through both
// front-ends, plus the realized view's own options.
func gainsCases() []goldenCase {
	var out []goldenCase
	for _, v := range gainsViews {
		out = append(out, goldenCase{"gains " + v, []string{"gains", v, "1970", "--period", "total"}, "gains",
			map[string]any{"view": v, "from": "1970", "to": "1970"}})
	}
	return append(out,
		goldenCase{"gains summary by quarter in CHF", []string{"gains", "summary", "1970", "--period", "quarterly", "-x", "CHF"}, "gains",
			map[string]any{"view": "summary", "from": "1970", "to": "1970", "period": "quarterly", "currency": "CHF"}},
		goldenCase{"gains realized, every document, newest first", []string{"gains", "realized", "1970", "--documents", "all", "-r"}, "gains",
			map[string]any{"view": "realized", "from": "1970", "to": "1970", "documents": "all", "newest_first": true}})
}

func TestGainsCLIEndToEnd(t *testing.T) {
	t.Parallel()
	cfg := setupGainsGold(t)

	for _, view := range gainsViews {
		so, se, code := run(t, "-c", cfg, "gains", view, "-", "today", "-x", "CHF")
		if code != 0 {
			t.Errorf("gains %s: exit=%d stderr=%s", view, code, se)
			continue
		}
		if !strings.Contains(so, "silver_source") && !strings.Contains(so, "period") {
			t.Errorf("gains %s: no header row:\n%s", view, so)
		}
	}
	if so, se, code := run(t, "-c", cfg, "gains", "realized", "--documents", "all", "-r", "-", "today"); code != 0 {
		t.Errorf("realized --documents all -r: exit=%d stderr=%s out=%s", code, se, so)
	}

	for _, c := range []struct {
		args []string
		want string
	}{
		{[]string{"gains", "summary", "--documents", "all"}, "belong to the realized view"},
		{[]string{"gains", "lots", "-r"}, "belong to the realized view"},
		{[]string{"gains", "realized", "--documents", "some"}, "invalid --documents"},
		{[]string{"gains", "summary", "--period", "hourly"}, "invalid --period"},
		{[]string{"gains", "nope"}, "unknown view"},
		{[]string{"gains"}, "a view subcommand is required"},
	} {
		_, se, code := run(t, append([]string{"-c", cfg}, c.args...)...)
		if code != 2 || !strings.Contains(se, c.want) {
			t.Errorf("%v: exit=%d stderr=%q, want 2 and %q", c.args, code, se, c.want)
		}
	}
}

// TestGainsViewsResolveTheirDefaults pins each view's default column
// set against its registry, and the aggregate views' reconciling
// grains against the usage text.
func TestGainsViewsResolveTheirDefaults(t *testing.T) {
	t.Parallel()
	usage := gainsUsage()
	for _, view := range gainsViews {
		rep := gainsReport(request{view: view, currency: "USD", period: "monthly"})
		if _, err := rep.pick("default"); err != nil {
			t.Errorf("%s defaults: %v", view, err)
		}
		if !strings.Contains(usage, "\n  "+view+" ") {
			t.Errorf("usage does not list the %s view", view)
		}
	}
}

// TestReportHeadersAreIdentifiers holds every report's column names and
// headers to letters, digits and underscores, so a JSON key or CSV
// header never needs quoting: a percentage is _pct, never _%.
func TestReportHeadersAreIdentifiers(t *testing.T) {
	t.Parallel()
	ident := regexp.MustCompile(`^[A-Za-z0-9_]+$`)
	reports := map[string]*report{
		"transactions": transactionsReport(request{currency: "USD"}),
		"returns":      returnsReport(request{currency: "USD", method: "both"}, nil),
	}
	for _, v := range holdingsFamily.views {
		reports["holdings "+v] = holdingsReport(request{view: v, currency: "USD"})
	}
	for _, v := range spendingFamily.views {
		reports["spending "+v] = spendingReport(request{view: v, currency: "USD", period: "monthly"})
	}
	for _, v := range incomeFamily.views {
		reports["income "+v] = incomeReport(request{view: v, currency: "USD", period: "monthly"})
	}
	for _, v := range cashflowFamily.views {
		reports["cashflow "+v] = cashflowReport(request{view: v, currency: "USD", period: "monthly", level: "group"})
	}
	for _, v := range gainsViews {
		reports["gains "+v] = gainsReport(request{view: v, currency: "USD", period: "monthly"})
	}
	for name, rep := range reports {
		for _, c := range rep.columns {
			if !ident.MatchString(c.name) || !ident.MatchString(c.header) {
				t.Errorf("%s: column %q header %q has a special character", name, c.name, c.header)
			}
			// A twin prints its currency, never the registry's suffix.
			if strings.Contains(c.header, "outccy") {
				t.Errorf("%s: column %q prints as %q", name, c.name, c.header)
			}
		}
	}
}
