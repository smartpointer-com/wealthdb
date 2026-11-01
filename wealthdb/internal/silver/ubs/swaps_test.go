package ubs

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
)

// A hedge rolled every year by an FX swap, as the netbanking ledger and
// the statements book it. Every reference, account, amount and date is
// invented.
//
//	QA-00001  a far leg alone; its near leg is a statement-era FX deal,
//	          and another account books a deal of the same shape
//	QA-00002  a near leg and its far leg
//	QA-00003  a near leg whose far leg matures after the PSN feed's
//	          first forward day
//	QA-00004  a near leg with no far leg yet
//	QA-00005  a near leg on a day the PSN feed states
//
// The portfolio's statement snapshots are quarter ends, and one month
// end holds cash alone. The PSN feed states its first forward on
// 2033-11-15.
const (
	swapCHF, swapUSD = "XX00SWAPCHF", "XX00SWAPUSD"
	swapPortfolio    = "P1"
	swapSafekeeping  = "SAFE1"
)

func swapDay(y int, m time.Month, d int) int64 {
	return time.Date(y, m, d, 0, 0, 0, 0, time.UTC).Unix()
}

// seedSwapLeg books one leg of a swap on both cash accounts, as the
// export does: the narrative names the leg and the reference, and
// states both amounts.
func seedSwapLeg(t *testing.T, r *webReader, ref, leg string, day int64, chf, usd string) {
	t.Helper()
	// A near leg sells the francs and buys the dollars; a far leg buys
	// them back.
	kind, desc1 := "Sale from FX Swap", fmt.Sprintf("You sold CHF (near); You bought USD; FX %s", ref)
	desc3 := fmt.Sprintf("You sold: -%s CHF; You bought: %s USD; Dealt rate: 0.9 CHF/USD", chf, usd)
	rows := []struct{ acct, ccy, debit, credit string }{{swapCHF, "CHF", "-" + chf, ""}, {swapUSD, "USD", "", usd}}
	if leg == "far" {
		kind, desc1 = "Purchase from FX Swap", fmt.Sprintf("You bought CHF (far); You sold USD; FX %s", ref)
		desc3 = fmt.Sprintf("You bought: %s CHF; You sold: -%s USD; Dealt rate: 0.9 CHF/USD", chf, usd)
		rows = []struct{ acct, ccy, debit, credit string }{{swapCHF, "CHF", "", chf}, {swapUSD, "USD", "-" + usd, ""}}
	}
	for _, row := range rows {
		if _, err := r.db.Exec(`INSERT INTO transactions (transaction_external_id, account_external_id, snapshot_at, value_date,
		    currency_iso, amount_debit, amount_credit, description_kind, payload) VALUES (?, ?, 0, ?, ?, NULLIF(?, ''), NULLIF(?, ''), ?, ?)`,
			ref+leg+row.ccy, row.acct, day, row.ccy, row.debit, row.credit, kind,
			fmt.Sprintf(`{"Description1": %q, "Description2": %q, "Description3": %q}`, desc1, kind, desc3)); err != nil {
			t.Fatal(err)
		}
	}
}

// seedStatementFX books one statement-era FX row, its deal number first
// under it.
func seedStatementFX(t *testing.T, r *webReader, id, acct, ccy, kind string, day int64, debit, credit, deal string) {
	t.Helper()
	if _, err := r.db.Exec(`INSERT INTO transactions (transaction_external_id, account_external_id, snapshot_at, value_date,
	    currency_iso, amount_debit, amount_credit, description_kind, payload) VALUES (?, ?, 0, ?, ?, NULLIF(?, ''), NULLIF(?, ''), ?, ?)`,
		statementIDPrefix+id, acct, day, ccy, debit, credit, kind,
		fmt.Sprintf(`{"booking_type": %q, "continuation": [%q, "EXAMPLE"], "source": "account_statement_pdf"}`, kind, deal)); err != nil {
		t.Fatal(err)
	}
}

// newSwapFixture is the web and PSN silvers of the hedge. statement
// says which statement-era deals to book besides the decoys: each its
// day, deal number and dollar amount.
func newSwapFixture(t *testing.T, statement ...[3]string) (*webReader, *psnReader) {
	t.Helper()
	web := newWebFixture(t)
	if _, err := web.db.Exec(`CREATE TABLE transactions (
	    transaction_external_id TEXT, account_external_id TEXT, snapshot_at INTEGER, trade_date INTEGER,
	    booking_date INTEGER, value_date INTEGER, currency_iso TEXT, amount_debit REAL, amount_credit REAL,
	    counterparty TEXT, description_kind TEXT, payload TEXT)`); err != nil {
		t.Fatal(err)
	}
	// The netbanking ledger starts on 2031-10-01.
	if _, err := web.db.Exec(`INSERT INTO transactions (transaction_external_id, account_external_id, snapshot_at, value_date,
	    currency_iso, amount_credit, description_kind, payload) VALUES ('X1', ?, 0, ?, 'CHF', 5, 'Interest', '{}')`,
		swapCHF, swapDay(2031, 10, 1)); err != nil {
		t.Fatal(err)
	}
	seedSwapLeg(t, web, "QA-00001", "far", swapDay(2032, 3, 10), "1000.50", "1150.00")
	seedSwapLeg(t, web, "QA-00002", "near", swapDay(2032, 3, 10), "1000", "1100.00")
	seedSwapLeg(t, web, "QA-00002", "far", swapDay(2033, 3, 10), "1000", "1180.00")
	seedSwapLeg(t, web, "QA-00003", "near", swapDay(2033, 3, 10), "1000", "1090.00")
	seedSwapLeg(t, web, "QA-00003", "far", swapDay(2034, 3, 10), "1000", "1170.00")
	seedSwapLeg(t, web, "QA-00004", "near", swapDay(2033, 5, 2), "500", "560.00")
	seedSwapLeg(t, web, "QA-00005", "near", swapDay(2034, 3, 10), "1000", "1095.00")
	// Decoys, none the near leg of QA-00001: a deal selling another
	// amount, one buying the francs back, and one of a single row.
	seedStatementFX(t, web, "d1", swapCHF, "CHF", "FOREX PURCHASE", swapDay(2031, 4, 20), "999", "", "0000011/000000011")
	seedStatementFX(t, web, "d2", swapUSD, "USD", "FOREX SALE", swapDay(2031, 4, 20), "", "1110", "0000011/000000011")
	seedStatementFX(t, web, "d3", swapCHF, "CHF", "FOREX SALE", swapDay(2031, 5, 20), "", "1000", "0000012/000000012")
	seedStatementFX(t, web, "d4", swapUSD, "USD", "FOREX PURCHASE", swapDay(2031, 5, 20), "1120", "", "0000012/000000012")
	seedStatementFX(t, web, "d5", swapCHF, "CHF", "FOREX PURCHASE", swapDay(2031, 6, 20), "1000.5", "", "0000013/000000013")
	seedStatementFX(t, web, "d6", "XX00OTHERCHF", "CHF", "FOREX PURCHASE", swapDay(2031, 7, 20), "1000.5", "", "0000014/000000014")
	seedStatementFX(t, web, "d7", "XX00OTHERUSD", "USD", "FOREX SALE", swapDay(2031, 7, 20), "", "1130", "0000014/000000014")
	for i, s := range statement {
		day, err := time.Parse(time.DateOnly, s[0])
		if err != nil {
			t.Fatal(err)
		}
		seedStatementFX(t, web, fmt.Sprintf("n%d-chf", i), swapCHF, "CHF", "FOREX PURCHASE", day.Unix(), "1000.5", "", s[1])
		seedStatementFX(t, web, fmt.Sprintf("n%d-usd", i), swapUSD, "USD", "FOREX SALE", day.Unix(), "", s[2], s[1])
	}
	quarterEnds := []int64{}
	for y := 2031; y <= 2033; y++ {
		for _, m := range []time.Month{3, 6, 9, 12} {
			quarterEnds = append(quarterEnds, time.Date(y, m+1, 0, 0, 0, 0, 0, time.UTC).Unix())
		}
	}
	for _, at := range quarterEnds {
		if _, err := web.db.Exec(`INSERT INTO historical_position_snapshots (as_of_date, portfolio_external_id, account_external_id,
		    instrument_isin, currency_iso, units, market_value, market_value_currency, source_doc_token, payload)
		    VALUES (?, ?, '', 'XX0000000001', 'CHF', 1, 100, 'CHF', 'tok', '{}')`, at, swapPortfolio); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := web.db.Exec(`INSERT INTO historical_cash_balances (period_end, account_external_id, currency_iso, period_start,
	    opening_balance, closing_balance, source_doc_token, payload) VALUES (?, ?, 'CHF', ?, 1, 1, 'tok', '{}')`,
		swapDay(2032, 4, 30), swapCHF, swapDay(2032, 4, 1)); err != nil {
		t.Fatal(err)
	}

	_, db := newFixtureSilver(t)
	if _, err := db.Exec(`INSERT INTO cash_accounts (snapshot_at, relationship_id, account_external_id, portfolio_external_id, payload)
	    VALUES (1, 'R1', ?, ?, '{}'), (1, 'R1', ?, ?, '{}')`, swapCHF, swapPortfolio, swapUSD, swapPortfolio); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO forward_contracts (snapshot_at, relationship_id, contract_external_id, payload)
	    VALUES (?, 'R1', 'QC-01', '{}'), (?, 'R1', 'QC-01', '{}')`, swapDay(2033, 11, 15)+3600, swapDay(2033, 11, 16)); err != nil {
		t.Fatal(err)
	}
	return web, &psnReader{db: db}
}

// inferredForwardRecords runs the statement stream over the fixture and
// returns its forward records as text, per day.
func inferredForwardRecords(t *testing.T, web *webReader, psn *psnReader) map[string][]string {
	t.Helper()
	ctx := context.Background()
	first, err := psn.firstForwardDay(ctx)
	if err != nil {
		t.Fatal(err)
	}
	swaps, err := web.inferredSwaps(ctx, first)
	if err != nil {
		t.Fatal(err)
	}
	portfolioOf, err := psn.portfolioByCashAccount(ctx)
	if err != nil {
		t.Fatal(err)
	}
	w := canonical.Window{Start: 0, End: swapDay(2040, 1, 1), HasChanges: true}
	stream, err := web.snapshotsHistorical(ctx, w, map[string]string{swapPortfolio: swapSafekeeping}, nil, nil,
		inferredForwards{source: "test-ubs", swaps: swaps, portfolioOf: portfolioOf, until: first})
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	got := map[string][]string{}
	for {
		batch, more, err := stream.Next(ctx)
		if err != nil {
			t.Fatal(err)
		}
		for _, p := range batch.Positions {
			if p.Vehicle != canonical.VehicleForward {
				continue
			}
			day, iid := time.Unix(p.SnapshotAt, 0).UTC().Format(time.DateOnly), "-"
			if p.InstrumentExternalID != nil {
				iid = *p.InstrumentExternalID
			}
			got[day] = append(got[day], fmt.Sprintf("%s %s %s %s %s %s", p.AccountExternalID, p.PositionKey, iid,
				p.Currency, p.MarketValue, p.Payload))
		}
		for _, i := range batch.Instruments {
			if i.Vehicle == canonical.VehicleForward {
				day := time.Unix(i.FirstSeenAt, 0).UTC().Format(time.DateOnly)
				got[day] = append(got[day], fmt.Sprintf("%s %s/%s %s %s", i.InstrumentExternalID, i.AssetClass, i.Vehicle, *i.Name, *i.Currency))
			}
		}
		for _, c := range batch.FundCompositions {
			day := time.Unix(c.AsOf, 0).UTC().Format(time.DateOnly)
			got[day] = append(got[day], fmt.Sprintf("%s %s %s %s %s %s %s %d %s", c.FundRef, c.Basis, c.Origin, c.Provider, c.SourceKind,
				*c.Currency, c.NetAssets, c.LineCount, c.Payload))
		}
		for _, l := range batch.FundLines {
			day := time.Unix(l.AsOf, 0).UTC().Format(time.DateOnly)
			got[day] = append(got[day], fmt.Sprintf("%s line %s %s %s %s %s %s", l.FundRef, *l.AssetType, *l.LongCurrency, l.LongAmount,
				*l.ShortCurrency, l.ShortAmount, l.MarketValue))
		}
		for _, a := range batch.Accounts {
			if a.AccountKind == canonical.AccountKindOverlay {
				day := time.Unix(a.FirstSeenAt, 0).UTC().Format(time.DateOnly)
				got[day] = append(got[day], fmt.Sprintf("%s %s %s", a.AccountExternalID, *a.DisplayName, *a.PortfolioExternalID))
			}
		}
		if !more {
			break
		}
	}
	return got
}

// swapRecords is what a day holding the forward of ref states.
func swapRecords(ref, name string, long, short string, payload string) []string {
	id := "fx-swap " + ref
	key := canonical.ContractKey("test-ubs", id)
	return []string{
		"P1:overlay " + id + " " + id + " USD 0 " + payload,
		id + " foreign_exchange/forward " + name + " USD",
		key + " holdings parsed ubs-web daily_file USD 0 1 " + payload,
		key + " line fx forward CHF " + long + " USD " + short + " 0",
	}
}

func TestInferredForwardsFollowTheSwaps(t *testing.T) {
	const inferred = `{"inferred":"fx_swap"}`
	overlay := "P1:overlay Portfolio overlay P1"
	s1 := swapRecords("QA-00001", "FX forward CHF/USD 2032-03-10", "1000.5", "-1150", inferred)
	s2 := swapRecords("QA-00002", "FX forward CHF/USD 2033-03-10", "1000", "-1180", inferred)
	s3 := swapRecords("QA-00003", "FX forward CHF/USD 2034-03-10", "1000", "-1170", inferred)
	s4 := swapRecords("QA-00004", "FX forward CHF/USD open", "500", "-560", inferred)
	join := func(parts ...[]string) []string {
		out := []string{overlay}
		for _, p := range parts {
			out = append(out, p...)
		}
		return out
	}
	paired := map[string][]string{
		// QA-00001 opens on its statement-era near leg, 2031-03-12.
		"2031-03-31": join(s1), "2031-06-30": join(s1), "2031-09-30": join(s1), "2031-12-31": join(s1),
		// QA-00002 from its near leg's day to its far leg's.
		"2032-03-31": join(s2), "2032-06-30": join(s2), "2032-09-30": join(s2), "2032-12-31": join(s2),
		// QA-00003 and QA-00004 until the PSN feed's first forward day;
		// 2033-12-31 and QA-00005 fall on the feed's days.
		"2033-03-31": join(s3), "2033-06-30": join(s3, s4), "2033-09-30": join(s3, s4),
	}
	beforeLedger := func(ref string) map[string][]string {
		s := swapRecords(ref, "FX forward CHF/USD 2032-03-10", "1000.5", "-1150", `{"inferred":"fx_swap","opened":"before_ledger"}`)
		out := map[string][]string{}
		for day, records := range paired {
			out[day] = records
		}
		// The netbanking ledger starts on 2031-10-01.
		delete(out, "2031-03-31")
		delete(out, "2031-06-30")
		delete(out, "2031-09-30")
		out["2031-12-31"] = join(s)
		return out
	}
	for _, tc := range []struct {
		name      string
		statement [][3]string
		want      map[string][]string
	}{
		{"near leg in the statements", [][3]string{{"2031-03-12", "0000001/000000001", "1120"}}, paired},
		{"no near leg", nil, beforeLedger("QA-00001")},
		{"two candidate near legs", [][3]string{{"2031-03-12", "0000001/000000001", "1120"},
			{"2031-03-13", "0000002/000000002", "1121"}}, beforeLedger("QA-00001")},
		{"near leg more than a year before", [][3]string{{"2031-03-08", "0000001/000000001", "1120"}}, beforeLedger("QA-00001")},
		{"near leg on the ledger's first day", [][3]string{{"2031-10-01", "0000001/000000001", "1120"}}, beforeLedger("QA-00001")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			web, psn := newSwapFixture(t, tc.statement...)
			checkForwardRecords(t, inferredForwardRecords(t, web, psn), tc.want)
		})
	}
}

// A swap that matures with none open after it: the overlay states it
// at zero, without its instrument, on the next statement day, and
// nothing after.
func TestInferredForwardsCloseWhenNoSwapIsOpen(t *testing.T) {
	web, psn := newSwapFixture(t, [3]string{"2031-03-12", "0000001/000000001", "1120"})
	if _, err := web.db.Exec(`DELETE FROM transactions WHERE transaction_external_id >= 'QA-00003'
	    AND transaction_external_id < 'QA-00006'`); err != nil {
		t.Fatal(err)
	}
	got := inferredForwardRecords(t, web, psn)
	if !strings.Contains(strings.Join(got["2032-12-31"], "\n"), "fx-swap QA-00002") {
		t.Errorf("2032-12-31 lacks QA-00002: %v", got["2032-12-31"])
	}
	checkForwardRecords(t, map[string][]string{"2033-03-31": got["2033-03-31"], "2033-06-30": got["2033-06-30"],
		"2033-09-30": got["2033-09-30"], "2033-12-31": got["2033-12-31"]}, map[string][]string{
		"2033-03-31": {"P1:overlay Portfolio overlay P1", `P1:overlay fx-swap QA-00002 - USD 0 {"closure_marker": true}`},
	})
}

// checkForwardRecords compares the records of each day, as sets.
func checkForwardRecords(t *testing.T, got, want map[string][]string) {
	t.Helper()
	for day, want := range want {
		want = slices.Sorted(slices.Values(want))
		if slices.Sort(got[day]); strings.Join(got[day], "\n") != strings.Join(want, "\n") {
			t.Errorf("%s:\n got %s\nwant %s", day, strings.Join(got[day], "\n     "), strings.Join(want, "\n     "))
		}
	}
	for day := range got {
		if _, ok := want[day]; !ok && len(got[day]) > 0 {
			t.Errorf("%s: unexpected forward records %v", day, got[day])
		}
	}
}
