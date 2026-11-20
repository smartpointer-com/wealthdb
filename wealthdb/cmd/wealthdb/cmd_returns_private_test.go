package main

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/gold"
)

// setupPrivateGold is a gold with one private-markets account: a fund
// called twice and marked once, and an SPV that exited in kind.
func setupPrivateGold(t *testing.T) string {
	t.Helper()
	goldPath := filepath.Join(t.TempDir(), "wealthdb.db")
	db, err := gold.OpenFresh(goldPath)
	if err != nil {
		t.Fatalf("open gold: %v", err)
	}
	ctx := context.Background()
	if _, err := db.ExecContext(ctx, `
		INSERT INTO silver_sources(silver_source_id, silver_kind, silver_path, high_watermark, first_loaded_at, last_loaded_at)
		VALUES ('carta', 'carta', '/tmp/carta.db', -1, 0, 0)`); err != nil {
		t.Fatalf("seed source: %v", err)
	}
	day := func(y int, m time.Month, dd int) int64 { return time.Date(y, m, dd, 12, 0, 0, 0, time.UTC).Unix() }
	d := func(n int64) *canonical.Decimal { v := canonical.NewDecimalFromInt(n); return &v }
	usd := "USD"
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	w := gold.NewWriter(tx)
	if err := w.UpsertAccounts(ctx, []canonical.AccountChange{{SilverSourceID: "carta", AccountExternalID: "INV000123456",
		AccountKind: canonical.AccountKindCustody, DisplayName: strp("Investor account"), BaseCurrency: &usd,
		FirstSeenAt: day(2024, time.January, 2), LastSeenAt: day(2024, time.December, 31)}}); err != nil {
		t.Fatal(err)
	}
	var txs []canonical.TransactionChange
	for _, l := range []struct {
		id, inst string
		at       int64
		kind     canonical.TxKind
		amount   int64
	}{
		{"c1", "FUND", day(2024, time.January, 10), canonical.TxKindContribution, -40000},
		{"c2", "FUND", day(2024, time.June, 10), canonical.TxKindContribution, -40000},
		{"s1", "SPV", day(2024, time.February, 1), canonical.TxKindContribution, -10000},
		{"s2", "SPV", day(2024, time.August, 1), canonical.TxKindTransferOut, -25000},
	} {
		inst := l.inst
		txs = append(txs, canonical.TransactionChange{SilverSourceID: "carta", TransactionExternalID: l.id,
			OccurredAt: l.at, AccountExternalID: "INV000123456", InstrumentExternalID: &inst, Kind: l.kind,
			Currency: "USD", NetAmount: d(l.amount)})
	}
	if err := w.InsertTransactions(ctx, txs); err != nil {
		t.Fatal(err)
	}
	line := func(inst string, at, valued, value int64) canonical.PositionChange {
		i := inst
		return canonical.PositionChange{SilverSourceID: "carta", AccountExternalID: "INV000123456", SnapshotAt: at,
			PositionKey: inst, InstrumentExternalID: &i, AssetClass: canonical.AssetClassPrivateEquity,
			Vehicle: canonical.VehicleFund, Currency: "USD", ValuedAt: valued, MarketValue: d(value)}
	}
	mar31, dec31 := day(2024, time.March, 31), day(2024, time.December, 31)
	if err := w.InsertPositions(ctx, []canonical.PositionChange{line("FUND", mar31, mar31, 40000),
		line("SPV", mar31, mar31, 12000), line("FUND", dec31, day(2024, time.September, 30), 91234)}); err != nil {
		t.Fatal(err)
	}
	if err := w.UpsertInstruments(ctx, []canonical.InstrumentChange{
		{SilverSourceID: "carta", InstrumentExternalID: "FUND", Name: strp("Example Ventures Fund I"),
			AssetClass: canonical.AssetClassPrivateEquity, Vehicle: canonical.VehicleFund, Currency: &usd},
	}); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close gold: %v", err)
	}
	return writeGoldCfg(t, goldPath)
}

// withPrivateLegs adds a capital call on every holding of a fixture's
// gold, so the private view lists each one, its account's name with it.
func withPrivateLegs(t *testing.T, cfg string) {
	t.Helper()
	db, err := gold.Open(goldPathFromCfg(cfg), gold.ModeReadWrite)
	if err != nil {
		t.Fatalf("open gold: %v", err)
	}
	defer db.Close()
	if _, err := db.Exec(`
		INSERT INTO transactions (silver_source_id, transaction_external_id, occurred_at,
		                          account_external_id, instrument_external_id, kind, currency, net_amount)
		SELECT silver_source_id, 'call-' || row_number() OVER (ORDER BY silver_source_id, account_external_id, instrument_external_id), first_at, account_external_id,
		       instrument_external_id, 'contribution', currency, -100
		  FROM (SELECT silver_source_id, account_external_id, instrument_external_id, currency, min(snapshot_at) AS first_at
		          FROM positions WHERE instrument_external_id IS NOT NULL GROUP BY ALL)`); err != nil {
		t.Fatalf("seed calls: %v", err)
	}
}

// The default columns are the design's; a holding reads by its name, the
// total last.
func TestPrivateView(t *testing.T) {
	t.Parallel()
	cfg := setupPrivateGold(t)
	out, se, code := runToday(t, "-c", cfg, "returns", "private", "-f", "csv")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, se)
	}
	header, _, _ := strings.Cut(out, "\n")
	if want := "silver_source,account,holding,asset_class,vehicle,status,first_call,paid_in_USD,distributed_USD," +
		"nav_USD,marked_at,dpi,tvpi,irr_pct,quality"; header != want {
		t.Errorf("header = %s", header)
	}
	rows := csvRows(t, out)
	if len(rows) != 3 {
		t.Fatalf("%d rows:\n%s", len(rows), out)
	}
	fund, spv, total := rows[0], rows[1], rows[2]
	if fund["holding"] != "Example Ventures Fund I" || fund["paid_in_USD"] != "80000.00" || fund["nav_USD"] != "91234.00" ||
		fund["marked_at"] != "2024-09-30" || fund["tvpi"] != "1.14" || fund["irr_pct"] == "" {
		t.Errorf("fund row %v", fund)
	}
	if spv["holding"] != "SPV" || spv["distributed_USD"] != "25000.00" || spv["nav_USD"] != "0.00" ||
		spv["dpi"] != "2.50" || spv["marked_at"] != "2024-03-31" || spv["quality"] != "exited" ||
		spv["status"] != "exited" || spv["asset_class"] != "private_equity" {
		t.Errorf("spv row %v", spv)
	}
	if fund["status"] != "held" || fund["vehicle"] != "fund" || total["status"] != "" {
		t.Errorf("fund %v, total %v", fund, total)
	}
	if total["holding"] != "Total" || total["paid_in_USD"] != "90000.00" || total["tvpi"] != "1.29" {
		t.Errorf("total row %v", total)
	}
	// The view reads since the first call: a window is refused.
	if _, se, code := runToday(t, "-c", cfg, "returns", "private", "2024"); code == 0 ||
		!strings.Contains(se, "takes no window") {
		t.Errorf("a window passed: exit %d: %s", code, se)
	}
}

// -d reads the view as of a day: the legs up to it and the NAV of the
// last snapshot on or before it. The grains take no -d.
func TestPrivateViewAsOf(t *testing.T) {
	t.Parallel()
	cfg := setupPrivateGold(t)
	out, se, code := runToday(t, "-c", cfg, "returns", "private", "-d", "2024-06-30", "-f", "csv")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, se)
	}
	rows := csvRows(t, out)
	if len(rows) != 3 {
		t.Fatalf("%d rows:\n%s", len(rows), out)
	}
	fund, spv := rows[0], rows[1]
	if fund["paid_in_USD"] != "80000.00" || fund["nav_USD"] != "40000.00" || fund["marked_at"] != "2024-03-31" {
		t.Errorf("fund on June 30 %v", fund)
	}
	if spv["distributed_USD"] != "0.00" || spv["nav_USD"] != "12000.00" || spv["status"] != "held" {
		t.Errorf("spv on June 30 %v: its exit is in August", spv)
	}
	if _, se, code := runToday(t, "-c", cfg, "returns", "accounts", "-d", "2024-06-30"); code == 0 ||
		!strings.Contains(se, "-d") {
		t.Errorf("-d passed on accounts: exit %d: %s", code, se)
	}
}

// -p redacts the money and the account id; the holdings' names, the
// multiples and the IRR stay.
func TestPrivateViewPrivacy(t *testing.T) {
	t.Parallel()
	cfg := setupPrivateGold(t)
	args := []string{"-c", cfg, "returns", "private", "-f", "csv", "-C", "+account_id"}
	plain, se, code := runToday(t, args...)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, se)
	}
	out, se, code := runToday(t, append(args, "-p")...)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, se)
	}
	if strings.Contains(out, "91234") || strings.Contains(out, "INV000123456") {
		t.Errorf("-p shows an amount or an id:\n%s", out)
	}
	want, got := csvRows(t, plain), csvRows(t, out)
	for i := range want {
		for _, col := range []string{"holding", "dpi", "tvpi", "irr_pct", "marked_at"} {
			if got[i][col] != want[i][col] {
				t.Errorf("row %d: %s reads %q under -p, %q without", i, col, got[i][col], want[i][col])
			}
		}
	}
}

// MCP reads the CLI's view: the same rows, and the same refusals.
func TestPrivateViewInMCP(t *testing.T) {
	t.Parallel()
	s := onFixtureToday(newTestMCP(setupPrivateGold(t), false))
	res, err := callTool(t, s, "returns", map[string]any{"view": "private", "currency": "USD"})
	if err != nil {
		t.Fatalf("returns private: %v", err)
	}
	if !strings.Contains(res, "Example Ventures Fund I") || !strings.Contains(res, "tvpi") {
		t.Errorf("result:\n%s", res)
	}
	res, err = callTool(t, s, "returns", map[string]any{"view": "private", "as_of": "2024-06-30", "format": "csv"})
	if err != nil || !strings.Contains(res, "as of 2024-06-30") || !strings.Contains(res, "40000.00") {
		t.Errorf("returns private as of June 30 (%v):\n%s", err, res)
	}
	// asset_class keeps the holdings of that class. The total is no
	// holding's: the filter drops it and never recomputes it.
	res, err = callTool(t, s, "returns", map[string]any{"view": "private", "asset_class": "private_equity", "format": "csv"})
	if err != nil || !strings.Contains(res, "Example Ventures Fund I") || !strings.Contains(res, "SPV") ||
		strings.Contains(res, "Total") {
		t.Errorf("returns private of private_equity (%v):\n%s", err, res)
	}
	res, err = callTool(t, s, "returns", map[string]any{"view": "private", "asset_class": "private_debt", "format": "csv"})
	if err != nil || !strings.Contains(res, "0 rows") || strings.Contains(res, "Example Ventures Fund I") {
		t.Errorf("returns private of private_debt (%v):\n%s", err, res)
	}
	if _, err := callTool(t, s, "returns", map[string]any{"view": "accounts", "as_of": "2024-06-30"}); err == nil ||
		!strings.Contains(err.Error(), "as_of belongs to view=private") {
		t.Errorf("as_of passed on accounts through MCP: %v", err)
	}
	if _, err := callTool(t, s, "returns", map[string]any{"view": "private", "from": "2024"}); err == nil ||
		!strings.Contains(err.Error(), "takes no window") {
		t.Errorf("a window passed through MCP: %v", err)
	}
	if _, err := callTool(t, s, "returns", map[string]any{"view": "private", "period": "annual"}); err == nil ||
		!strings.Contains(err.Error(), "private takes no period") {
		t.Errorf("period passed through MCP: %v", err)
	}
	if _, err := callTool(t, s, "returns", map[string]any{"view": "private", "benchmark": "spx"}); err == nil ||
		!strings.Contains(err.Error(), "private takes no benchmark") {
		t.Errorf("benchmark passed through MCP: %v", err)
	}
}
