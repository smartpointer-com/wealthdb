package gold

import (
	"math"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/yield"
)

// Migration 0147 seeds the built-in settlement cycles, which a sync of
// the config's yield rows replaces with the config's markets over them.
// Replaying it adds nothing.
func TestTheSettlementCyclesSeedTheBuiltIns(t *testing.T) {
	t.Parallel()
	db, ctx := openMigrated(t)
	text := func() []string {
		return scanStrings(t, db, `SELECT market || ' ' || COALESCE(CAST(from_day AS VARCHAR), '-') || ' ' || days || ' ' ||
		    origin FROM yield_settlement_cycles ORDER BY market, from_day NULLS FIRST`)
	}
	builtin := func(cs []canonical.SettlementCycle) []string {
		var out []string
		for _, c := range cs {
			from := "-"
			if c.From != nil {
				from = c.From.Format(time.DateOnly)
			}
			out = append(out, c.Market+" "+from+" "+strconv.Itoa(c.Days)+" "+string(c.Origin))
		}
		return out
	}
	if got, want := text(), builtin(canonical.BuiltinSettlementCycles()); !slices.Equal(got, want) {
		t.Errorf("seeded cycles:\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	rerunMigrationDDL(t, db, ctx, "0147_dividend_calendar.sql")
	if n := countRows(t, db, ctx, "yield_settlement_cycles"); n != len(canonical.BuiltinSettlementCycles()) {
		t.Errorf("cycles after a replay = %d", n)
	}
	rows := []canonical.SettlementCycle{{Market: "HK", Days: 2, Origin: canonical.OriginConfig},
		{Market: "US", Days: 1, Origin: canonical.OriginBuiltin}}
	if n, err := SyncYield(ctx, db, canonical.YieldRows{SettlementCycles: rows}); err != nil || n != 1 {
		t.Fatalf("sync: %d %v", n, err)
	}
	if got := text(); !slices.Equal(got, builtin(rows)) {
		t.Errorf("synced cycles = %q", got)
	}
}

// A dividend's record day is the one the dividend calendar states, else
// the one its ex day derives, else its market's lag (YIELD.md §4). Each
// stock or fund pays once and is priced at 50 a unit; the book is
// invented:
//
//   - QCAL, a share of the United States, sells half of its 400 on 1 June,
//     inside the record window its lag gives, ten days before it pays 0.6
//     a share. Its calendar states a record day of 28 May, its ex day too
//     under T+1: it rates on the 400 held then, the window that one day.
//     QNOC, alike with only a split on those days in its calendar, which
//     states no dividend's days, rates nothing: its window drops the
//     reading.
//   - QFND, a fund, alike, which reads its pay day's quantity with no
//     calendar row: its stated ex day of 29 May rates it.
//   - QCHF, a Swiss share, sells 100 of its 300 on 30 July 2025, before its
//     ex day of Thursday 31 July, and pays on 6 August. Its record day
//     derives from the ex day by the two-day cycle, stepping over 1
//     August, when SIX SIS is closed, to Monday 4 August: it rates on
//     the 200 held as the ex day began, where its lag's window would drop
//     the reading.
//   - QHKD, a share of Hong Kong, has an ex day and no record day: Hong
//     Kong derives none, so its market's lag of 30 days gives the record
//     day, as with no calendar row.
func TestARecordDayFromTheCalendar(t *testing.T) {
	t.Parallel()
	early, pay := ymd(2025, 1, 1), ymd(2026, 6, 11)
	half := []bookSeg{{early, 400, 20000}, {ymd(2026, 6, 30), 200, 10000}}
	in, _, _ := dividendBook{instruments: []bookInstrument{
		{"src", "QCAL", "US0000000101", "", "QCAL"}, {"src", "QNOC", "US0000000102", "", "QNOC"},
		{"src", "QFND", "US0000000105", "", "QFND"}, {"src", "QCHF", "CH0000000103", "", "QCHF"},
		{"src", "QHKD", "HK0000000104", "", "QHKD"},
	}, lines: []bookLine{
		{"src", "B1", "QCAL", half}, {"src", "B1", "QNOC", half}, {"src", "B1", "QFND", half},
		{"src", "B1", "QCHF", []bookSeg{{early, 300, 15000}, {ymd(2025, 7, 31), 200, 10000}}},
		{"src", "B1", "QHKD", []bookSeg{{early, 100, 5000}}},
	}, txns: []bookTxn{
		{"src", "B1", "QCAL", ymd(2026, 6, 1), "sell", 10000, -200}, {"src", "B1", "QCAL", pay, "dividend", 240, 0},
		{"src", "B1", "QNOC", ymd(2026, 6, 1), "sell", 10000, -200}, {"src", "B1", "QNOC", pay, "dividend", 240, 0},
		{"src", "B1", "QFND", ymd(2026, 6, 1), "sell", 10000, -200}, {"src", "B1", "QFND", pay, "dividend", 240, 0},
		{"src", "B1", "QCHF", ymd(2025, 7, 30), "sell", 5000, -100},
		{"src", "B1", "QCHF", ymd(2025, 8, 6), "dividend", 0.5 * 200, 0},
		{"src", "B1", "QHKD", pay, "dividend", 50, 0},
	}, currencies: map[string]string{"QCHF": "CHF", "QHKD": "HKD"}, after: []string{
		`INSERT INTO fx_rates (silver_source_id, snapshot_at, base_currency, quote_currency, mid_rate)
		    VALUES ('fx', 0, 'USD', 'CHF', 1.1), ('fx', 0, 'USD', 'HKD', 0.13)`,
		`UPDATE positions SET vehicle = 'etf' WHERE position_key = 'QFND'`,
		`UPDATE instruments SET vehicle = 'etf' WHERE instrument_external_id = 'QFND'`,
		`INSERT INTO dividend_calendar (silver_source_id, ref, provider, event_id, as_of, kind, ex_day, record_day,
		    pay_day) VALUES
		    ('ref', 'isin:US0000000101', 'sec-edgar', 'a', 0, 'dividend', NULL, '2026-05-28', '2026-06-11'),
		    ('ref', 'isin:US0000000105', 'issuer', 'b', 0, 'dividend', '2026-05-29', '2026-05-29', '2026-06-11'),
		    ('ref', 'isin:CH0000000103', 'six', 'c', 0, 'dividend', '2025-07-31', NULL, '2025-08-06'),
		    ('ref', 'isin:HK0000000104', 'other', 'd', 0, 'dividend', '2026-05-20', NULL, NULL),
		    ('ref', 'isin:US0000000102', 'finra', 'e', 0, 'other', '2026-05-28', '2026-05-28', '2026-06-11')`,
	}}.inputs(t)
	b := yield.Compute(in)
	for _, c := range []struct {
		iid    string
		source yield.RecordSource
		lag    int
		record float64
		pct    float64
	}{
		{"QCAL", yield.RecordStated, 14, 400, 1.2}, {"QNOC", yield.RecordLagged, yield.RecordLagUSShare.Days, 400, 0},
		{"QFND", yield.RecordStated, 13, 400, 1.2}, {"QCHF", yield.RecordDerived, 6, 200, 1.0},
		{"QHKD", yield.RecordLagged, 30, 100, 1.0},
	} {
		k := yield.HoldingKey{Source: "src", Account: "B1", PositionKey: c.iid}
		ps := in.Payments[k]
		if len(ps) != 1 || ps[0].RecordSource != c.source || ps[0].LagDays != c.lag ||
			(ps[0].Record == nil) != (c.record == 0) || (c.record != 0 && *ps[0].Record != c.record) {
			t.Errorf("%s: payments %+v", c.iid, ps)
			continue
		}
		if c.source == yield.RecordStated || c.source == yield.RecordDerived {
			if ps[0].Least == nil || *ps[0].Least != c.record || *ps[0].Most != c.record {
				t.Errorf("%s: window %s to %s, want its record day's %v alone", c.iid, fmtFloatPtr(ps[0].Least),
					fmtFloatPtr(ps[0].Most), c.record)
			}
		}
		p := position(b, k)
		if (p.DividendYield == nil) != (c.pct == 0) || (c.pct != 0 && math.Abs(*p.DividendYield-c.pct) > 1e-9) {
			t.Errorf("%s: dividend yield %s, want %v", c.iid, fmtFloatPtr(p.DividendYield), c.pct)
		}
	}
	var got []string
	for _, r := range b.Coverage() {
		if r.Section == "record_day" {
			got = append(got, r.Reason+" "+strconv.Itoa(r.Payments)+" "+strconv.Itoa(r.Holdings))
		}
	}
	if want := "stated 2 2|derived 1 1|lag 2 2|dropped 1 1"; strings.Join(got, "|") != want {
		t.Errorf("coverage's record_day lines %q, want %q", strings.Join(got, "|"), want)
	}
}

// A calendar's day gives the quantity held as the ex day began, by trade
// date: a trade on the ex day earns nothing. The book is invented; each
// share is priced at 50 a unit:
//
//   - QCHE and QCHR, Swiss shares, each sell 100 of their 300 on their ex
//     day, Thursday 31 July 2025, and are paid 150 on Tuesday 5 August,
//     0.5 on the 300. QCHE's row states the ex day, QCHR's the record day
//     of Monday 4 August alone, whose ex day the two-day cycle gives over
//     1 August, when SIX SIS is closed. Both rate on the 300, the window
//     the ex day alone. QCHA, alike, sells all of its 300 on the ex day:
//     the window holds the 300 though the record day holds none.
//   - QUS1, a share of the United States, sells half of its 400 on 28 May
//     2026, its ex day and its record day under T+1: it rates on the 400.
//   - QPAY, alike, sells 100 of its 400 two days before its pay day. Its
//     row states the pay day alone, as a filing may: its market's lag
//     rates it on the 400, as with no row.
//   - QEST's estimate states a record day of 20 May and the payment's pay
//     day, the final row 28 May and a pay day two days before: the final
//     one wins.
//   - QALS's row names it by another class's ticker, an alias of its own
//     security: it joins.
//   - QFNW, a fund, has a row with no pay day 80 days before its payment,
//     past a fund's 63 days: it is no payment's, and a fund fixes no lag.
//   - QNOK, a share quoted in Norwegian kroner, derives its record day by
//     the two-day cycle. QMIX, alike, is held by a second account valued
//     in Canadian dollars at the same instants: its market is unknown, so
//     nothing derives it.
func TestACalendarDayReadsTheExDay(t *testing.T) {
	t.Parallel()
	early, pay := ymd(2025, 1, 1), ymd(2026, 6, 11)
	swiss := []bookSeg{{early, 300, 15000}, {ymd(2025, 7, 31), 200, 10000}}
	full := []bookSeg{{early, 400, 20000}}
	in, _, _ := dividendBook{instruments: []bookInstrument{
		{"src", "QCHE", "CH0000000111", "", "QCHE"}, {"src", "QCHR", "CH0000000112", "", "QCHR"},
		{"src", "QCHA", "CH0000000120", "", "QCHA"},
		{"src", "QUS1", "US0000000113", "", "QUS1"}, {"src", "QPAY", "US0000000114", "", "QPAY"},
		{"src", "QEST", "US0000000115", "", "QEST"}, {"src", "QALS", "US0000000116", "", "QALS"},
		{"src", "QFNW", "US0000000117", "", "QFNW"}, {"src", "QNOK", "NO0000000118", "", "QNOK"},
		{"src", "QMIX", "NO0000000119", "", "QMIX"},
	}, lines: []bookLine{
		{"src", "B1", "QCHE", swiss}, {"src", "B1", "QCHR", swiss},
		{"src", "B1", "QCHA", []bookSeg{{early, 300, 15000}, {ymd(2025, 7, 31), 0, 0}}},
		{"src", "B1", "QUS1", []bookSeg{{early, 400, 20000}, {ymd(2026, 5, 31), 200, 10000}}},
		{"src", "B1", "QPAY", []bookSeg{{early, 400, 20000}, {ymd(2026, 6, 30), 300, 15000}}},
		{"src", "B1", "QEST", full}, {"src", "B1", "QALS", full}, {"src", "B1", "QFNW", full},
		{"src", "B1", "QNOK", full}, {"src", "B1", "QMIX", full}, {"src", "B2", "QMIX", full},
	}, txns: []bookTxn{
		{"src", "B1", "QCHE", ymd(2025, 7, 31), "sell", 5000, -100},
		{"src", "B1", "QCHE", ymd(2025, 8, 5), "dividend", 150, 0},
		{"src", "B1", "QCHR", ymd(2025, 7, 31), "sell", 5000, -100},
		{"src", "B1", "QCHR", ymd(2025, 8, 5), "dividend", 150, 0},
		{"src", "B1", "QCHA", ymd(2025, 7, 31), "sell", 15000, -300},
		{"src", "B1", "QCHA", ymd(2025, 8, 5), "dividend", 150, 0},
		{"src", "B1", "QUS1", ymd(2026, 5, 28), "sell", 10000, -200}, {"src", "B1", "QUS1", pay, "dividend", 240, 0},
		{"src", "B1", "QPAY", ymd(2026, 6, 9), "sell", 5000, -100}, {"src", "B1", "QPAY", pay, "dividend", 240, 0},
		{"src", "B1", "QEST", pay, "dividend", 200, 0}, {"src", "B1", "QALS", pay, "dividend", 200, 0},
		{"src", "B1", "QFNW", pay, "dividend", 200, 0}, {"src", "B1", "QNOK", pay, "dividend", 200, 0},
		{"src", "B1", "QMIX", pay, "dividend", 200, 0},
	}, currencies: map[string]string{"QCHE": "CHF", "QCHR": "CHF", "QCHA": "CHF", "QNOK": "NOK",
		"QMIX": "NOK"}, after: []string{
		`INSERT INTO fx_rates (silver_source_id, snapshot_at, base_currency, quote_currency, mid_rate)
		    VALUES ('fx', 0, 'USD', 'CHF', 1.1), ('fx', 0, 'USD', 'NOK', 0.1), ('fx', 0, 'USD', 'CAD', 0.7)`,
		`UPDATE positions SET vehicle = 'etf' WHERE position_key = 'QFNW'`,
		`UPDATE instruments SET vehicle = 'etf' WHERE instrument_external_id = 'QFNW'`,
		`UPDATE instruments SET currency = NULL WHERE instrument_external_id = 'QMIX'`,
		`UPDATE positions SET currency = 'CAD' WHERE account_external_id = 'B2' AND position_key = 'QMIX'`,
		`INSERT INTO security_aliases (alias, security_key, origin) VALUES ('symbol:QALT', 'isin:US0000000116', 'lookup')`,
		`INSERT INTO dividend_calendar (silver_source_id, ref, provider, event_id, as_of, kind, ex_day, record_day,
		    pay_day, status) VALUES
		    ('ref', 'isin:CH0000000111', 'six', 'a', 0, 'dividend', '2025-07-31', NULL, '2025-08-05', NULL),
		    ('ref', 'isin:CH0000000112', 'issuer', 'b', 0, 'dividend', NULL, '2025-08-04', '2025-08-05', NULL),
		    ('ref', 'isin:CH0000000120', 'six', 'a', 0, 'dividend', '2025-07-31', NULL, '2025-08-05', NULL),
		    ('ref', 'isin:US0000000113', 'issuer', 'c', 0, 'dividend', '2026-05-28', '2026-05-28', '2026-06-11', NULL),
		    ('ref', 'isin:US0000000114', 'sec-edgar', 'd', 0, 'dividend', NULL, NULL, '2026-06-11', 'final'),
		    ('ref', 'isin:US0000000115', 'finra', 'e1', 0, 'dividend', NULL, '2026-05-20', '2026-06-11', 'estimated'),
		    ('ref', 'isin:US0000000115', 'finra', 'e2', 0, 'dividend', NULL, '2026-05-28', '2026-06-09', 'final'),
		    ('ref', 'symbol:QALT', 'issuer', 'f', 0, 'dividend', NULL, '2026-05-28', '2026-06-11', NULL),
		    ('ref', 'isin:US0000000117', 'issuer', 'g', 0, 'dividend', NULL, '2026-03-23', NULL, NULL),
		    ('ref', 'isin:NO0000000118', 'other', 'h', 0, 'dividend', '2026-05-28', NULL, '2026-06-11', NULL),
		    ('ref', 'isin:NO0000000119', 'other', 'i', 0, 'dividend', '2026-05-28', NULL, '2026-06-11', NULL)`,
	}}.inputs(t)
	b := yield.Compute(in)
	for _, c := range []struct {
		iid, want string
		pct       float64
	}{
		{"QCHE", "derived 5 300", 1.0}, {"QCHR", "stated 5 300", 1.0}, {"QCHA", "derived 5 300", 0},
		{"QUS1", "stated 14 400", 1.2},
		{"QPAY", "lag 30 400", 1.2}, {"QEST", "stated 14 400", 1.0}, {"QALS", "stated 14 400", 1.0},
		{"QFNW", " 0 -", 0}, {"QNOK", "derived 14 400", 0}, {"QMIX", " 0 -", 0},
	} {
		k := yield.HoldingKey{Source: "src", Account: "B1", PositionKey: c.iid}
		ps := in.Payments[k]
		if len(ps) != 1 {
			t.Errorf("%s: payments %+v", c.iid, ps)
			continue
		}
		got := string(ps[0].RecordSource) + " " + strconv.Itoa(ps[0].LagDays) + " " + fmtFloatPtr(ps[0].Record)
		if got != c.want {
			t.Errorf("%s: %q, want %q", c.iid, got, c.want)
		}
		if ps[0].Record != nil && ps[0].RecordSource != yield.RecordLagged &&
			(ps[0].Least == nil || *ps[0].Least != *ps[0].Record || *ps[0].Most != *ps[0].Record) {
			t.Errorf("%s: window %s to %s, want the ex day's %s alone", c.iid, fmtFloatPtr(ps[0].Least),
				fmtFloatPtr(ps[0].Most), fmtFloatPtr(ps[0].Record))
		}
		if c.pct == 0 {
			continue
		}
		if p := position(b, k); p.DividendYield == nil || math.Abs(*p.DividendYield-c.pct) > 1e-9 {
			t.Errorf("%s: dividend yield %s, want %v", c.iid, fmtFloatPtr(p.DividendYield), c.pct)
		}
	}
}

// A JPX row names a Japanese share by its Tokyo Stock Exchange code. It
// joins the share a lookup lists by that code on a Japanese exchange,
// OpenFIGI's local ticker, a five-character code read as its four, and a
// ticker read without the exchange it adds; a row whose code no lookup
// on a Japanese exchange states stays unmatched. The shares and their
// codes are invented, quoted in yen, which fixes no lag, so a share's
// event lies within 105 days before its payment: QJPN's stated record
// day, 85 days before its pay day, matches, and its ex day a day earlier
// gives its quantity; so does QJPT's. QJPX's row is unmatched, and
// QJPO's only row lies 120 days back: their payments know no record day.
func TestAJPXRowJoinsThroughItsTSECode(t *testing.T) {
	t.Parallel()
	early, pay := ymd(2025, 1, 1), ymd(2026, 6, 24)
	in, _, _ := dividendBook{instruments: []bookInstrument{
		{"src", "QJPN", "JP0000000106", "", "QJPN"}, {"src", "QJPX", "JP0000000107", "", "QJPX"},
		{"src", "QJPO", "JP0000000108", "", "QJPO"}, {"src", "QJPT", "JP0000000109", "", "QJPT"},
	}, lines: []bookLine{
		{"src", "B1", "QJPN", []bookSeg{{early, 100, 500000}}}, {"src", "B1", "QJPX", []bookSeg{{early, 100, 500000}}},
		{"src", "B1", "QJPO", []bookSeg{{early, 100, 500000}}}, {"src", "B1", "QJPT", []bookSeg{{early, 100, 500000}}},
	}, txns: []bookTxn{
		{"src", "B1", "QJPN", pay, "dividend", 3000, 0}, {"src", "B1", "QJPX", pay, "dividend", 3000, 0},
		{"src", "B1", "QJPO", pay, "dividend", 3000, 0}, {"src", "B1", "QJPT", pay, "dividend", 3000, 0},
	}, currencies: map[string]string{"QJPN": "JPY", "QJPX": "JPY", "QJPO": "JPY", "QJPT": "JPY"}, after: []string{
		`INSERT INTO fx_rates (silver_source_id, snapshot_at, base_currency, quote_currency, mid_rate)
		    VALUES ('fx', 0, 'USD', 'JPY', 0.007)`,
		`INSERT INTO security_identities (silver_source_id, security_key, as_of, provider, isin, exchange, ticker, payload)
		    VALUES ('ref', 'isin:JP0000000106', 0, 'openfigi', 'JP0000000106', 'JT', NULL, '{"local_ticker": "1Q01"}'),
		           ('ref', 'isin:JP0000000107', 0, 'openfigi', 'JP0000000107', 'LN', NULL, '{"local_ticker": "1Q02"}'),
		           ('ref', 'isin:JP0000000108', 0, 'openfigi', 'JP0000000108', 'JT', NULL, '{"local_ticker": "1Q04"}'),
		           ('ref', 'isin:JP0000000109', 0, 'openfigi', 'JP0000000109', 'XTKS', '1q05.t', NULL)`,
		`INSERT INTO dividend_calendar (silver_source_id, ref, provider, event_id, as_of, kind, ex_day, record_day)
		    VALUES ('ref', 'jpx:1Q010', 'jpx', 'a', 0, 'dividend', '2026-03-30', '2026-03-31'),
		           ('ref', 'jpx:1Q02', 'jpx', 'b', 0, 'dividend', '2026-03-30', '2026-03-31'),
		           ('ref', 'jpx:1Q03', 'jpx', 'c', 0, 'dividend', '2026-03-30', '2026-03-31'),
		           ('ref', 'jpx:1Q04', 'jpx', 'd', 0, 'dividend', '2026-02-23', '2026-02-24'),
		           ('ref', 'jpx:1Q05', 'jpx', 'e', 0, 'dividend', '2026-03-30', '2026-03-31')`,
	}}.inputs(t)
	for iid, want := range map[string]string{"QJPN": "stated 86 100", "QJPX": " 0 -", "QJPO": " 0 -",
		"QJPT": "stated 86 100"} {
		ps := in.Payments[yield.HoldingKey{Source: "src", Account: "B1", PositionKey: iid}]
		if len(ps) != 1 {
			t.Errorf("%s: payments %+v", iid, ps)
			continue
		}
		if got := string(ps[0].RecordSource) + " " + strconv.Itoa(ps[0].LagDays) + " " + fmtFloatPtr(ps[0].Record); got != want {
			t.Errorf("%s: %q, want %q", iid, got, want)
		}
	}
}
