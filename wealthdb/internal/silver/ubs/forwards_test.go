package ubs

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
)

// The forward rows of one PSN day, in the shapes a forward position
// takes. Every contract id, amount and date is invented.
//
//	QC-01  a swap's far leg, Buy: the trade leg positive, the counter
//	       leg negative
//	QC-02  a forward, Sell: the trade leg negative, the counter leg
//	       positive
//	QC-03  a forward, Sell, with both amounts unsigned
//	QC-04  a spot deal, open
//	QC-05  a forward matured on its position day
//	QC-06  a forward with no maturity date
//	QC-07  Sell, but the trade leg positive: the signs win
//	QC-08  maturing on the snapshot's day, its position day the day
//	       before: open
//	QC-09  maturing on the snapshot's day, with no position day:
//	       matured
//	QC-10  a forward whose two legs are one currency
//	QC-11  a forward with no counter amount
//	QC-12  a forward with no mark currency
func TestSnapshotsForwardLegs(t *testing.T) {
	path, seed := newFixtureSilver(t)
	snap := time.Date(2026, 6, 30, 0, 0, 0, 0, time.UTC).Unix()
	row := func(id, deal, side, trade, tradeCcy, counter, counterCcy, mark, maturity string) string {
		mt := ""
		if maturity != "" {
			mt = fmt.Sprintf(`,"MtrtyDt":%q`, maturity)
		}
		return fmt.Sprintf(`(%d, 'SFTPCHxx', %q, '{"PrtflId":"P1","DealTpDesc":%q,"BuySellInd":%q,"TradeAmt":%q,`+
			`"TradeCcyIsoCd":%q,"CounterCcyAmt":%q,"CounterCcyIsoCd":%q,"MrktValueAmt":%q,"MrktValueCcyIsoCd":"CHF",`+
			`"PosDt":"2026-06-29"%s}')`, snap, id, deal, side, trade, tradeCcy, counter, counterCcy, mark, mt)
	}
	if _, err := seed.Exec(fmt.Sprintf(`
        INSERT INTO dump_runs(snapshot_at, silver_schema_version, run_dir) VALUES (%d, 1, '/x/1');
        INSERT INTO forward_contracts(snapshot_at, relationship_id, contract_external_id, payload) VALUES %s;`,
		snap, strings.Join([]string{
			row("QC-01", "Swap Deal", "Buy", "1000.00", "USD", "-820.00", "CHF", "-20.00", "2026-12-15"),
			row("QC-02", "Forward Deal", "Sell", "-500.00", "EUR", "540.00", "USD", "3.00", "2026-09-30"),
			row("QC-03", "Forward Deal", "Sell", "500.00", "EUR", "540.00", "USD", "3.00", "2026-09-30"),
			row("QC-04", "Spot Deal", "Buy", "100.00", "EUR", "-96.00", "CHF", "0.10", "2026-07-02"),
			row("QC-05", "Forward Deal", "Buy", "100.00", "EUR", "-96.00", "CHF", "0.10", "2026-06-29"),
			row("QC-06", "Forward Deal", "Buy", "100.00", "EUR", "-96.00", "CHF", "0.10", ""),
			row("QC-07", "Forward Deal", "Sell", "100.00", "EUR", "-96.00", "CHF", "0.10", "2026-09-30"),
			row("QC-08", "Forward Deal", "Buy", "100.00", "EUR", "-96.00", "CHF", "0.10", "2026-06-30"),
			strings.Replace(row("QC-09", "Forward Deal", "Buy", "100.00", "EUR", "-96.00", "CHF", "0.10", "2026-06-30"),
				`,"PosDt":"2026-06-29"`, "", 1),
			row("QC-10", "Forward Deal", "Buy", "100.00", "EUR", "-96.00", "EUR", "0.10", "2026-09-30"),
			strings.Replace(row("QC-11", "Forward Deal", "Buy", "100.00", "EUR", "-96.00", "CHF", "0.10", "2026-09-30"),
				`"CounterCcyAmt":"-96.00",`, "", 1),
			strings.Replace(row("QC-12", "Forward Deal", "Buy", "100.00", "EUR", "-96.00", "CHF", "0.10", "2026-09-30"),
				`"MrktValueCcyIsoCd":"CHF",`, "", 1),
		}, ", "))); err != nil {
		t.Fatal(err)
	}
	conn := openAdapter(t, path)
	w, _ := conn.ChangeWindow(context.Background(), -1)
	stream, err := conn.Snapshots(context.Background(), w)
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	batch, _, err := stream.Next(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	// The matured contract emits nothing; every other one its position,
	// and only an open forward its instrument.
	positions := map[string]*string{}
	for _, p := range batch.Positions {
		positions[p.PositionKey] = p.InstrumentExternalID
	}
	for id, want := range map[string]bool{"QC-01": true, "QC-02": true, "QC-03": true, "QC-04": false, "QC-06": false,
		"QC-07": true, "QC-08": true, "QC-10": false, "QC-11": false, "QC-12": false} {
		iid, held := positions[id]
		if !held || (iid != nil) != want || (want && *iid != id) {
			t.Errorf("%s: held %v, instrument %v; want held, instrument %v", id, held, iid, want)
		}
	}
	_, held5 := positions["QC-05"]
	_, held9 := positions["QC-09"]
	if held5 || held9 || len(positions) != 10 {
		t.Errorf("positions = %v, want ten, the matured QC-05 and QC-09 not among them", positions)
	}

	got := map[string]string{}
	for _, i := range batch.Instruments {
		got[i.InstrumentExternalID] = fmt.Sprintf("%s/%s %s %s", i.AssetClass, i.Vehicle, *i.Name, *i.Currency)
	}
	for _, c := range batch.FundCompositions {
		got[c.FundRef] = fmt.Sprintf("%s %s %s %s %s %s %d %v", c.Basis, c.Origin, c.Provider, *c.Currency,
			c.NetAssets, c.WeightSum, c.LineCount, c.ValidUntil != nil)
	}
	for _, l := range batch.FundLines {
		got[l.FundRef+" line"] = fmt.Sprintf("%s %s %s %s %s %s %s",
			*l.AssetType, l.Weight, *l.LongCurrency, l.LongAmount, *l.ShortCurrency, l.ShortAmount, *l.Currency)
		if l.SecurityKey == l.FundRef || l.SecurityKey != canonical.SecurityKey("", "", "", "", *l.Name) {
			t.Errorf("%s: line key %q, want the name key of %q", l.FundRef, l.SecurityKey, *l.Name)
		}
	}
	k1, k2, k3 := canonical.ContractKey("QC-01"), canonical.ContractKey("QC-02"), canonical.ContractKey("QC-03")
	k7, k8 := canonical.ContractKey("QC-07"), canonical.ContractKey("QC-08")
	want := map[string]string{
		"QC-01":      "foreign_exchange/forward FX forward USD/CHF 2026-12-15 CHF",
		"QC-02":      "foreign_exchange/forward FX forward USD/EUR 2026-09-30 CHF",
		"QC-03":      "foreign_exchange/forward FX forward USD/EUR 2026-09-30 CHF",
		k1:           "holdings parsed ubs-psn CHF -20 100 1 false",
		k2:           "holdings parsed ubs-psn CHF 3 100 1 false",
		k3:           "holdings parsed ubs-psn CHF 3 100 1 false",
		k1 + " line": "fx forward 100 USD 1000 CHF -820 CHF",
		k2 + " line": "fx forward 100 USD 540 EUR -500 CHF",
		k3 + " line": "fx forward 100 USD 540 EUR -500 CHF",
		"QC-07":      "foreign_exchange/forward FX forward EUR/CHF 2026-09-30 CHF",
		k7:           "holdings parsed ubs-psn CHF 0.1 100 1 false",
		k7 + " line": "fx forward 100 EUR 100 CHF -96 CHF",
		"QC-08":      "foreign_exchange/forward FX forward EUR/CHF 2026-06-30 CHF",
		k8:           "holdings parsed ubs-psn CHF 0.1 100 1 false",
		k8 + " line": "fx forward 100 EUR 100 CHF -96 CHF",
	}
	if len(got) != len(want) {
		t.Errorf("emitted %d records, want %d: %v", len(got), len(want), got)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %q, want %q", k, got[k], v)
		}
	}
}
