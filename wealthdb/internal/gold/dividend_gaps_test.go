package gold

import (
	"context"
	"database/sql"
	"strconv"
	"strings"
	"testing"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/yield"
)

// rebuildGaps rebuilds dividend_gaps from what gold holds now.
func rebuildGaps(t *testing.T, db *sql.DB) {
	t.Helper()
	ctx := context.Background()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback() //nolint:errcheck — no-op after Commit
	if err := writeDividendGaps(ctx, tx); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
}

// A payment the calendar dates no distribution of takes its record day
// from its issuer's learned gap. The book is invented: every holding
// holds 400 at 50 a unit from 2025 on, and is paid 240 on Thursday 11 June
// 2026, which no row of the calendar states:
//
//   - QEDG, a company's share, sells 200 on 1 June, inside its market's
//     lag window. Its filings state two dividends, each paid 20 business
//     days after its record day: the payment's record day is 14 May, its
//     ex day too under T+1, and it rates on the 400 held then.
//   - QED2, the same filer's other class, states one dividend of its own,
//     20 business days apart as well: the classes learn together.
//   - QONE states one dividend alone, QOUT two paid 120 days after their
//     record days: neither teaches a gap, so the lag's window drops their
//     readings, as with no row.
//   - QFAM and QFA2, funds of one family, sell 200 on 10 June. Their
//     issuer states two distributions each, paid 3 business days after
//     the record day, one over Easter's closing days: the record day is 8
//     June, and each rates on the 400 held then, where the pay day's
//     quantity would read 200.
//   - QPOL, a fund of the United States with no row of its own, takes the
//     gap of the country's funds, 3 business days: it rates on its 400.
//   - QNON, a fund quoted in yen, has neither: it keeps the pay day's
//     quantity.
func TestARecordDayFromTheIssuersGap(t *testing.T) {
	t.Parallel()
	early, pay := ymd(2025, 1, 1), ymd(2026, 6, 11)
	full := []bookSeg{{early, 400, 20000}}
	half := []bookSeg{{early, 400, 20000}, {ymd(2026, 6, 30), 200, 10000}}
	ids := []string{"QEDG", "QED2", "QONE", "QOUT", "QFAM", "QFA2", "QPOL", "QNON"}
	var insts []bookInstrument
	var lines []bookLine
	var txns []bookTxn
	for i, id := range ids {
		isin := "US00000002" + strconv.Itoa(i+1) + "0"
		if id == "QNON" {
			isin = "JP0000000280"
		}
		insts = append(insts, bookInstrument{"src", id, isin, "", id})
		segs := full
		switch id {
		case "QEDG", "QONE", "QOUT":
			segs = half
			txns = append(txns, bookTxn{"src", "B1", id, ymd(2026, 6, 1), "sell", 10000, -200})
		case "QFAM", "QFA2", "QPOL":
			segs = half
			txns = append(txns, bookTxn{"src", "B1", id, ymd(2026, 6, 10), "sell", 10000, -200})
		}
		lines = append(lines, bookLine{"src", "B1", id, segs})
		txns = append(txns, bookTxn{"src", "B1", id, pay, "dividend", 240, 0})
	}
	in, db, ctx := dividendBook{instruments: insts, lines: lines, txns: txns,
		currencies: map[string]string{"QNON": "JPY"}, after: []string{
			`INSERT INTO fx_rates (silver_source_id, snapshot_at, base_currency, quote_currency, mid_rate)
			    VALUES ('fx', 0, 'USD', 'JPY', 0.007)`,
			`UPDATE positions SET vehicle = 'etf' WHERE position_key IN ('QFAM', 'QFA2', 'QPOL', 'QNON')`,
			`UPDATE instruments SET vehicle = 'etf' WHERE instrument_external_id IN ('QFAM', 'QFA2', 'QPOL', 'QNON')`,
			`INSERT INTO fund_attributes (silver_source_id, fund_ref, as_of) VALUES
			    ('ref', 'isin:US0000000250', 0), ('ref', 'isin:US0000000260', 0)`,
			`INSERT INTO security_identities (silver_source_id, security_key, as_of, provider, isin, payload) VALUES
			    ('ref', 'isin:US0000000210', 0, 'sec-edgar', 'US0000000210', '{"cik": "9001"}'),
			    ('ref', 'isin:US0000000220', 0, 'sec-edgar', 'US0000000220', '{"cik": "9001"}')`,
			`INSERT INTO dividend_calendar (silver_source_id, ref, provider, event_id, as_of, kind, ex_day, record_day,
			    pay_day, currency) VALUES
			    ('ref', 'isin:US0000000210', 'sec-edgar', 'a', 0, 'dividend', NULL, '2025-11-03', '2025-12-01', 'USD'),
			    ('ref', 'isin:US0000000210', 'sec-edgar', 'b', 0, 'dividend', NULL, '2026-02-02', '2026-03-02', 'USD'),
			    ('ref', 'isin:US0000000220', 'sec-edgar', 'c', 0, 'dividend', NULL, '2025-08-01', '2025-08-29', 'USD'),
			    ('ref', 'isin:US0000000230', 'sec-edgar', 'd', 0, 'dividend', NULL, '2026-02-02', '2026-03-02', 'USD'),
			    ('ref', 'isin:US0000000240', 'sec-edgar', 'e', 0, 'dividend', NULL, '2025-08-01', '2025-11-29', 'USD'),
			    ('ref', 'isin:US0000000240', 'sec-edgar', 'f', 0, 'dividend', NULL, '2025-11-03', '2026-03-03', 'USD'),
			    ('ref', 'isin:US0000000250', 'issuer', 'g', 0, 'dividend', NULL, '2026-03-02', '2026-03-05', 'USD'),
			    ('ref', 'isin:US0000000250', 'issuer', 'h', 0, 'dividend', NULL, '2026-04-01', '2026-04-08', 'USD'),
			    ('ref', 'isin:US0000000260', 'issuer', 'g', 0, 'dividend', NULL, '2026-03-02', '2026-03-05', 'USD'),
			    ('ref', 'isin:US0000000260', 'issuer', 'h', 0, 'dividend', NULL, '2026-04-01', '2026-04-08', 'USD')`,
		}}.inputs(t)
	var got []string
	rows, err := db.QueryContext(ctx, `SELECT key, issuer, fund, market, days, near, far, events FROM dividend_gaps
 ORDER BY key`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var key, issuer, market string
		var fund bool
		var days, near, far, events int
		if err := rows.Scan(&key, &issuer, &fund, &market, &days, &near, &far, &events); err != nil {
			t.Fatal(err)
		}
		got = append(got, strings.Join([]string{key, issuer, strconv.FormatBool(fund), market, strconv.Itoa(days),
			strconv.Itoa(near), strconv.Itoa(far), strconv.Itoa(events)}, " "))
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"isin:US0000000210 cik:9001 false US 20 20 20 3",
		"isin:US0000000220 cik:9001 false US 20 20 20 3",
		"isin:US0000000250 isin:US0000000250 true US 3 3 3 2",
		"isin:US0000000260 isin:US0000000260 true US 3 3 3 2",
		"pool:US pool:US true US 3 3 3 2",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("dividend_gaps\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	b := yield.Compute(in)
	for _, c := range []struct{ iid, want string }{
		{"QEDG", "learned 28 400"}, {"QED2", "learned 28 400"},
		{"QONE", "lag 30 400"}, {"QOUT", "lag 30 400"},
		{"QFAM", "learned 3 400"}, {"QFA2", "learned 3 400"}, {"QPOL", "learned 3 400"},
		{"QNON", " 0 -"},
	} {
		k := yield.HoldingKey{Source: "src", Account: "B1", PositionKey: c.iid}
		ps := in.Payments[k]
		if len(ps) != 1 {
			t.Errorf("%s: payments %+v", c.iid, ps)
			continue
		}
		p := ps[0]
		if got := string(p.RecordSource) + " " + strconv.Itoa(p.LagDays) + " " + fmtFloatPtr(p.Record); got != c.want {
			t.Errorf("%s: %q, want %q", c.iid, got, c.want)
		}
		if p.RecordSource == yield.RecordLearned && (p.Least == nil || *p.Least != 400 || *p.Most != 400) {
			t.Errorf("%s: window %s to %s, want the 400 of its gap's days", c.iid, fmtFloatPtr(p.Least),
				fmtFloatPtr(p.Most))
		}
	}
	got = nil
	for _, r := range b.Coverage() {
		if r.Section == "record_day" {
			got = append(got, r.Reason+" "+strconv.Itoa(r.Payments))
		}
	}
	if want := "learned 5|lag 2|none 1|dropped 2"; strings.Join(got, "|") != want {
		t.Errorf("coverage's record_day lines %q, want %q", strings.Join(got, "|"), want)
	}
}

// The gap pass names each security as the reader does. The book is
// invented, each holding paid once on 11 June 2026:
//
//   - QEU1 and QEU2, funds quoted in euros, state two distributions each,
//     3 business days apart over Easter's closing days. QEU1 is a fund by
//     a fund table, QEU2 by its instrument alone: both are funds, and their
//     pool, the European funds', learns 3 days in TARGET2's. QEUP, a fund
//     of Luxembourg with no row of its own, takes it in its own market's:
//     its record day is Monday 8 June, its ex day Friday 5 June under T+2.
//   - QCKU and QCKC are one filer's shares quoted in dollars and in francs:
//     they learn apart, 20 business days in the United States and 2 in
//     Switzerland over 1 May, when SIX SIS is closed.
//   - QJPS, a Japanese share, is named by its JPX code alone: its gap
//     joins its own key, in Japan's market.
func TestTheGapPassNamesEachSecurityAsTheReaderDoes(t *testing.T) {
	t.Parallel()
	early, pay := ymd(2025, 1, 1), ymd(2026, 6, 11)
	ids := map[string]string{"QEU1": "IE0000000410", "QEU2": "IE0000000420", "QEUP": "LU0000000430",
		"QCKU": "US0000000440", "QCKC": "CH0000000450", "QJPS": "JP0000000460"}
	var insts []bookInstrument
	var lines []bookLine
	var txns []bookTxn
	for _, id := range []string{"QEU1", "QEU2", "QEUP", "QCKU", "QCKC", "QJPS"} {
		insts = append(insts, bookInstrument{"src", id, ids[id], "", id})
		lines = append(lines, bookLine{"src", "B1", id, []bookSeg{{early, 400, 20000}}})
		txns = append(txns, bookTxn{"src", "B1", id, pay, "dividend", 240, 0})
	}
	in, db, ctx := dividendBook{instruments: insts, lines: lines, txns: txns,
		currencies: map[string]string{"QEU1": "EUR", "QEU2": "EUR", "QEUP": "EUR", "QCKC": "CHF", "QJPS": "JPY"},
		after: []string{
			`INSERT INTO fx_rates (silver_source_id, snapshot_at, base_currency, quote_currency, mid_rate)
			    VALUES ('fx', 0, 'USD', 'EUR', 1.1), ('fx', 0, 'USD', 'CHF', 1.1), ('fx', 0, 'USD', 'JPY', 0.007)`,
			`UPDATE positions SET vehicle = 'etf' WHERE position_key IN ('QEU1', 'QEU2', 'QEUP')`,
			`UPDATE instruments SET vehicle = 'etf' WHERE instrument_external_id IN ('QEU1', 'QEU2', 'QEUP')`,
			`INSERT INTO fund_attributes (silver_source_id, fund_ref, as_of) VALUES ('ref', 'isin:IE0000000410', 0)`,
			`INSERT INTO security_identities (silver_source_id, security_key, as_of, provider, isin, exchange, payload) VALUES
			    ('ref', 'isin:US0000000440', 0, 'sec-edgar', 'US0000000440', NULL, '{"cik": "9002"}'),
			    ('ref', 'isin:CH0000000450', 0, 'sec-edgar', 'CH0000000450', NULL, '{"cik": "9002"}'),
			    ('ref', 'isin:JP0000000460', 0, 'openfigi', 'JP0000000460', 'JT', '{"local_ticker": "1Q06"}')`,
			`INSERT INTO dividend_calendar (silver_source_id, ref, provider, event_id, as_of, kind, ex_day, record_day,
			    pay_day, currency) VALUES
			    ('ref', 'isin:IE0000000410', 'issuer', 'a', 0, 'dividend', NULL, '2026-03-02', '2026-03-05', 'EUR'),
			    ('ref', 'isin:IE0000000410', 'issuer', 'b', 0, 'dividend', NULL, '2026-04-01', '2026-04-08', 'EUR'),
			    ('ref', 'isin:IE0000000420', 'issuer', 'a', 0, 'dividend', NULL, '2026-03-02', '2026-03-05', 'EUR'),
			    ('ref', 'isin:IE0000000420', 'issuer', 'b', 0, 'dividend', NULL, '2026-04-01', '2026-04-08', 'EUR'),
			    ('ref', 'isin:US0000000440', 'sec-edgar', 'c', 0, 'dividend', NULL, '2025-11-03', '2025-12-01', 'USD'),
			    ('ref', 'isin:US0000000440', 'sec-edgar', 'd', 0, 'dividend', NULL, '2026-02-02', '2026-03-02', 'USD'),
			    ('ref', 'isin:CH0000000450', 'six', 'e', 0, 'dividend', NULL, '2026-04-21', '2026-04-23', 'CHF'),
			    ('ref', 'isin:CH0000000450', 'six', 'f', 0, 'dividend', NULL, '2025-04-29', '2025-05-02', 'CHF'),
			    ('ref', 'jpx:1Q06', 'jpx', 'g', 0, 'dividend', '2025-09-29', '2025-09-30', '2025-12-05', 'JPY'),
			    ('ref', 'jpx:1Q06', 'jpx', 'h', 0, 'dividend', '2026-03-30', '2026-03-31', '2026-06-08', 'JPY'),
			    ('ref', 'jpx:1Q06', 'jpx', 'i', 0, 'other', NULL, '2026-01-30', '2026-02-02', 'USD')`,
		}}.inputs(t)
	got := map[string]string{}
	rows, err := db.QueryContext(ctx, `SELECT key, issuer, fund, market, days, events FROM dividend_gaps`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var key, issuer, market string
		var fund bool
		var days, events int
		if err := rows.Scan(&key, &issuer, &fund, &market, &days, &events); err != nil {
			t.Fatal(err)
		}
		got[key] = strings.Join([]string{issuer, strconv.FormatBool(fund), market, strconv.Itoa(days),
			strconv.Itoa(events)}, " ")
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	for key, want := range map[string]string{
		"isin:IE0000000410": "isin:IE0000000410 true IE 3 2",
		"isin:IE0000000420": "isin:IE0000000420 true IE 3 2",
		"pool:EU":           "pool:EU true  3 2",
		"isin:US0000000440": "cik:9002 false US 20 2",
		"isin:CH0000000450": "cik:9002 false CH 2 2",
	} {
		if got[key] != want {
			t.Errorf("%s: %q, want %q", key, got[key], want)
		}
	}
	if g := got["isin:JP0000000460"]; !strings.HasPrefix(g, "isin:JP0000000460 false JP ") || !strings.HasSuffix(g, " 2") {
		t.Errorf("the JPX-named share's row %q", g)
	}
	if len(got) != 6 {
		t.Errorf("%d rows, want 6: %v", len(got), got)
	}
	ps := in.Payments[yield.HoldingKey{Source: "src", Account: "B1", PositionKey: "QEUP"}]
	if len(ps) != 1 || ps[0].RecordSource != yield.RecordLearned || ps[0].LagDays != 6 {
		t.Errorf("QEUP: payments %+v, want one on its pool's gap", ps)
	}
}

// A share takes its own gap or none; a fund its own, else its pool's in
// its own market's business days, also where it has no key; a fund of no
// market, or of a pool with no gap, takes none.
func TestGapOf(t *testing.T) {
	t.Parallel()
	own := yield.Lag{Days: 20, Business: true, Near: 19, Far: 21, Market: "US"}
	eu := yield.Lag{Days: 3, Business: true, Near: 2, Far: 4}
	b := &yieldBase{gaps: dividendGaps{security: map[string]yield.Lag{"isin:X": own}, pool: map[string]yield.Lag{"EU": eu}}}
	ch := eu
	ch.Market = "CH"
	for _, c := range []struct {
		security string
		share    bool
		market   string
		want     yield.Lag
	}{
		{"isin:X", true, "US", own}, {"isin:X", false, "CH", own}, {"isin:Y", false, "CH", ch},
		{"isin:Y", true, "CH", yield.Lag{}}, {"", false, "CH", ch}, {"isin:Y", false, "", yield.Lag{}},
		{"isin:Y", false, "JP", yield.Lag{}},
	} {
		if got := b.gapOf(c.security, c.share, c.market); got != c.want {
			t.Errorf("gapOf(%q, %v, %q) = %+v, want %+v", c.security, c.share, c.market, got, c.want)
		}
	}
}

// Migration 0148 replays over a gold that holds learned gaps: its table
// keeps its rows. The share and its two dividends are invented.
func TestMigration0148DDLIsRerunnable(t *testing.T) {
	t.Parallel()
	db, ctx := openMigrated(t)
	execEach(t, db, `INSERT INTO dividend_calendar (silver_source_id, ref, provider, event_id, as_of, kind, ex_day,
	    record_day, pay_day, currency) VALUES
	    ('ref', 'isin:US0000000510', 'issuer', 'a', 0, 'dividend', NULL, '2025-11-03', '2025-12-01', 'USD'),
	    ('ref', 'isin:US0000000510', 'issuer', 'b', 0, 'dividend', NULL, '2026-02-02', '2026-03-02', 'USD')`)
	rebuildGaps(t, db)
	before := countRows(t, db, ctx, "dividend_gaps")
	if before == 0 {
		t.Fatal("no gap learned")
	}
	for range 2 {
		rerunMigrationDDL(t, db, ctx, "0148_dividend_gaps.sql")
	}
	if after := countRows(t, db, ctx, "dividend_gaps"); after != before {
		t.Errorf("rows %d, before %d", after, before)
	}
}
