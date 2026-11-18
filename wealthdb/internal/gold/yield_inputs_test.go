package gold

import (
	"cmp"
	"context"
	"database/sql"
	"maps"
	"math"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/yield"
)

func ymd(y, m, d int) time.Time { return time.Date(y, time.Month(m), d, 0, 0, 0, 0, time.UTC) }

// An account with no stated wrapper reads taxable_personal, marked as the
// default, so the withholding by wrapper passes it by.
func TestYieldAccountsReadTheDefaultWrapper(t *testing.T) {
	t.Parallel()
	db, ctx := openMigrated(t)
	execEach(t, db, `INSERT INTO accounts (silver_source_id, account_external_id, account_kind, tax_wrapper,
	    first_seen_at, last_seen_at) VALUES ('src', 'R', 'brokerage', 'roth_ira', 0, 0),
	    ('src', 'U', 'brokerage', NULL, 0, 0)`)
	in := &yield.Inputs{Accounts: map[yield.AccountKey]yield.Account{}}
	if err := readYieldAccounts(ctx, db, in); err != nil {
		t.Fatal(err)
	}
	if a := in.Accounts[yield.AccountKey{Source: "src", Account: "R"}]; a.TaxWrapper != "roth_ira" || a.DefaultWrapper {
		t.Errorf("R = %+v, want its stated roth_ira", a)
	}
	if a := in.Accounts[yield.AccountKey{Source: "src", Account: "U"}]; a.TaxWrapper != "taxable_personal" || !a.DefaultWrapper {
		t.Errorf("U = %+v, want the default taxable_personal", a)
	}
}

// YieldInputs reads from gold: a holding's amount with its accrual and
// the key exposure names it by; the newest rate schedule that holds,
// every tier of it, the latest snapshot's on or before the day, and a
// ledger rate dated on or before the day over an undated one, which
// leaves the account's stated debit rate; a fund's newest filed rate
// sensitivity with that filing's net assets; the ledger's dividend in
// force; the payments a holding and an account received; the
// daily amounts over the year; reported figures by their periods' ends,
// a restated one as restated. The book is invented.
func TestYieldInputsReadsGold(t *testing.T) {
	t.Parallel()
	db, ctx := openMigrated(t)
	snap := ymd(2026, 6, 30).Unix()
	execEach(t, db,
		`INSERT INTO accounts (silver_source_id, account_external_id, account_kind, display_name, base_currency,
		    first_seen_at, last_seen_at) VALUES ('src', 'B', 'brokerage', 'Brokerage', 'USD', 0, 0),
		    ('src', 'S', 'cash', 'Savings', 'USD', 0, 0), ('src', 'D', 'cash', 'Declared', 'USD', 0, 0)`,
		`INSERT INTO instruments (silver_source_id, instrument_external_id, asset_class, vehicle, symbol, name, currency,
		    first_seen_at, last_seen_at) VALUES ('src', 'QB', 'fixed_income', 'bond', NULL, 'EXAMPLE NOTE', 'USD', 0, 0),
		    ('src', 'QSTK', 'public_equity', 'stock', 'QSTK', 'Example Co', 'USD', 0, 0),
		    ('src', 'QMF', 'cash', 'fund', 'QMF', 'Example Money Fund', 'USD', 0, 0),
		    ('src', 'QOF', 'fixed_income', 'fund', 'QOF', 'Example Old Fund', 'USD', 0, 0)`,
		`INSERT INTO positions (silver_source_id, snapshot_at, account_external_id, position_key, instrument_external_id,
		    asset_class, vehicle, currency, quantity, market_value, accrued_interest) VALUES
		    ('src', `+itoa(snap)+`, 'B', 'QB', 'QB', 'fixed_income', 'bond', 'USD', 10000, 10125, 125),
		    ('src', `+itoa(snap)+`, 'B', 'QSTK', 'QSTK', 'public_equity', 'stock', 'USD', 10, 1000, NULL),
		    ('src', `+itoa(snap)+`, 'B', 'QMF', 'QMF', 'cash', 'fund', 'USD', 2000, 2000, NULL),
		    ('src', `+itoa(snap)+`, 'B', 'QOF', 'QOF', 'fixed_income', 'fund', 'USD', 100, 1000, NULL)`,
		`INSERT INTO cash_balances (silver_source_id, snapshot_at, account_external_id, currency, balance_kind, amount)
		    VALUES ('src', `+itoa(snap)+`, 'S', 'USD', 'current', 5000), ('src', `+itoa(snap)+`, 'D', 'USD', 'current', 100)`,
		`INSERT INTO transactions (silver_source_id, transaction_external_id, occurred_at, account_external_id, kind,
		    currency, net_amount, instrument_external_id) VALUES
		    ('src', 'T1', `+itoa(ymd(2026, 3, 15).Unix())+`, 'B', 'interest', 'USD', 250, 'QB'),
		    ('src', 'T2', `+itoa(ymd(2026, 4, 30).Unix())+`, 'S', 'interest', 'USD', 12, NULL),
		    ('src', 'T3', `+itoa(ymd(2026, 5, 1).Unix())+`, 'S', 'deposit', 'USD', 700, NULL),
		    ('src', 'T4', `+itoa(ymd(2026, 2, 1).Unix())+`, 'B', 'interest', 'USD', 80, 'QOLD')`,
		`INSERT INTO income_txn_enrichment (silver_source_id, transaction_external_id, payer_signature,
		    signature_version, income_detailed, provenance, assigned_at) VALUES
		    ('src', 'T1', 'sig-note', 1, 'INCOME_INTEREST_EARNED', 'rule', 100),
		    ('src', 'T2', 'sig-bank', 1, 'INCOME_INTEREST_EARNED', 'rule', 100),
		    ('src', 'T3', 'sig-tenant', 1, 'INCOME_RENTAL', 'rule', 100),
		    ('src', 'T4', 'sig-old', 1, 'INCOME_INTEREST_EARNED', 'rule', 100)`,
		`INSERT INTO fund_characteristics (silver_source_id, fund_ref, as_of, provider, field, label, value) VALUES
		    ('ref', 'symbol:QMF', `+itoa(ymd(2026, 6, 10).Unix())+`, 'sec', 'weighted_average_maturity', '', 36.525),
		    ('ref', 'symbol:QMF', `+itoa(ymd(2026, 6, 25).Unix())+`, 'inbox', 'weighted_average_maturity_years', '', 0.2)`,
		`INSERT INTO rate_observations (silver_source_id, series_id, as_of, value) VALUES
		    ('rates', 'USD-3M', `+itoa(ymd(2026, 6, 28).Unix())+`, 4.0),
		    ('rates', 'USD-3M', `+itoa(ymd(2026, 5, 20).Unix())+`, 4.5),
		    ('rates', 'USD-1Y', `+itoa(ymd(2026, 5, 1).Unix())+`, 3.0)`,
		`INSERT INTO security_period_facts (silver_source_id, security_key, field, as_of, period_start, provider, value,
		    filed) VALUES
		    ('ref', 'symbol:QSTK', 'eps_diluted', `+itoa(ymd(2025, 6, 30).Unix())+`, '2025-04-01', 'sec', 1, '2026-07-15'),
		    ('ref', 'symbol:QSTK', 'eps_diluted', `+itoa(ymd(2025, 9, 30).Unix())+`, '2025-07-01', 'sec', 1, '2025-11-01'),
		    ('ref', 'symbol:QSTK', 'eps_diluted', `+itoa(ymd(2025, 12, 31).Unix())+`, '2025-10-01', 'sec', 1, '2026-02-01'),
		    ('ref', 'symbol:QSTK', 'eps_diluted', `+itoa(ymd(2026, 3, 31).Unix())+`, '2026-01-01', 'sec', 1, '2026-05-01'),
		    ('ref', 'symbol:QSTK', 'eps_diluted', `+itoa(ymd(2026, 6, 30).Unix())+`, '2026-04-01', 'sec', 5, '2026-08-01')`,
	)
	rebuildExposureWithDefaults(t, db, ctx)
	if _, err := RebuildYield(ctx, db); err != nil {
		t.Fatal(err)
	}
	// The rates as the yield pass and the ledger leave them, and the
	// curve the config names.
	execEach(t, db,
		`INSERT INTO yield_curves (currency, tenor, tenor_months, silver_source_id, series_id) VALUES
		    ('USD', '3M', 3, 'rates', 'USD-3M'), ('USD', '1Y', 12, 'rates', 'USD-1Y')`,
		`INSERT INTO account_rates (silver_source_id, account_external_id, currency, side, tier, rate_pct, valid_from,
		    origin) VALUES ('src', 'S', 'USD', 'credit', 1, 1.0, '2026-01-01', 'stated'),
		    ('src', 'S', 'USD', 'credit', 1, 1.5, '2026-05-01', 'stated'),
		    ('src', 'S', 'USD', 'credit', 2, 0.2, '2026-01-01', 'stated'),
		    ('src', 'S', 'USD', 'credit', 1, 9.0, '2026-09-01', 'stated'),
		    ('src', 'D', 'USD', 'credit', 1, 0.5, '2026-01-01', 'stated')`,
		`INSERT INTO account_rates (silver_source_id, account_external_id, currency, side, tier, rate_pct, valid_from,
		    origin, line_no) VALUES ('src', 'D', NULL, 'credit', 1, 2.0, NULL, 'declared', 1),
		    ('src', 'D', NULL, 'credit', 1, 2.5, '2026-03-01', 'declared', 2),
		    ('src', 'D', NULL, 'credit', 1, 3.0, '2026-08-01', 'declared', 3)`,
		`INSERT INTO account_rates (silver_source_id, account_external_id, currency, side, tier, rate_pct, origin)
		    VALUES ('src', 'D', 'USD', 'debit', 1, 8.0, 'stated')`,
		// Three snapshots of a tiered rate: a month before the day, the day
		// before, and after it.
		`INSERT INTO account_rates (silver_source_id, account_external_id, currency, side, tier, rate_pct, up_to,
		    origin, observed_at) VALUES
		    ('src', 'U', 'USD', 'credit', 1, 0.3, NULL, 'stated', `+itoa(snap-30*86400)+`),
		    ('src', 'U', 'USD', 'credit', 1, 0.4, 50000, 'stated', `+itoa(snap-86400)+`),
		    ('src', 'U', 'USD', 'credit', 2, 0.2, NULL, 'stated', `+itoa(snap-86400)+`),
		    ('src', 'U', 'USD', 'credit', 1, 9.9, NULL, 'stated', `+itoa(snap+5*86400)+`)`,
		// Three filings of the money fund's rate sensitivity: an older one,
		// the newest before the day with its net assets, filed under a key
		// the security master reads as the fund's, and one after. The old
		// fund's one filing and its one figure lie beyond max_gap_days.
		`INSERT INTO security_aliases (alias, security_key, origin) VALUES ('figi:QQQQQMF00001', 'symbol:QMF', 'lookup')`,
		`INSERT INTO fund_risk (silver_source_id, fund_ref, as_of, currency, tenor, dv01) VALUES
		    ('ref', 'symbol:QMF', `+itoa(snap-60*86400)+`, 'USD', '3M', 99),
		    ('ref', 'figi:QQQQQMF00001', `+itoa(snap-10*86400)+`, 'USD', '3M', 5),
		    ('ref', 'figi:QQQQQMF00001', `+itoa(snap-10*86400)+`, 'USD', '1Y', 2),
		    ('ref', 'symbol:QMF', `+itoa(snap+10*86400)+`, 'USD', '3M', 77),
		    ('ref', 'symbol:QOF', `+itoa(snap-(canonical.DefaultMaxGapDays+1)*86400)+`, 'USD', '5Y', 3)`,
		`INSERT INTO fund_compositions (silver_source_id, fund_ref, as_of, basis, provider, net_assets, line_count)
		    VALUES ('ref', 'symbol:QMF', `+itoa(snap-60*86400)+`, 'holdings', 'sec', 1, 0),
		    ('ref', 'figi:QQQQQMF00001', `+itoa(snap-10*86400)+`, 'holdings', 'sec', 1000000, 0)`,
		`INSERT INTO fund_characteristics (silver_source_id, fund_ref, as_of, provider, field, label, value) VALUES
		    ('ref', 'figi:QQQQQMF00001', `+itoa(ymd(2026, 6, 20).Unix())+`, 'sec', 'seven_day_yield', '', 4.2),
		    ('ref', 'symbol:QOF', `+itoa(snap-(canonical.DefaultMaxGapDays+1)*86400)+`, 'inbox', 'sec_yield', '', 3)`,
		// Dividends filed under a key the security master reads as the
		// stock's.
		`INSERT INTO security_aliases (alias, security_key, origin) VALUES ('figi:QQQQSTK0001', 'symbol:QSTK', 'lookup')`,
		`INSERT INTO security_period_facts (silver_source_id, security_key, field, as_of, period_start, provider, value,
		    filed) VALUES
		    ('ref', 'figi:QQQQSTK0001', 'dividends_per_share', `+itoa(ymd(2025, 9, 30).Unix())+`, '2025-07-01', 'sec', 0.25, '2025-11-01'),
		    ('ref', 'figi:QQQQSTK0001', 'dividends_per_share', `+itoa(ymd(2025, 12, 31).Unix())+`, '2025-10-01', 'sec', 0.25, '2026-02-01'),
		    ('ref', 'figi:QQQQSTK0001', 'dividends_per_share', `+itoa(ymd(2026, 3, 31).Unix())+`, '2026-01-01', 'sec', 0.25, '2026-05-01'),
		    ('ref', 'figi:QQQQSTK0001', 'dividends_per_share', `+itoa(ymd(2026, 6, 30).Unix())+`, '2026-04-01', 'sec', 0.25, '2026-08-01')`,
		// The ledger's dividends: undated, dated before the day, and after.
		`INSERT INTO yield_declared_dividends (line_no, silver_source_id, instrument_external_id, as_of, amount,
		    frequency, currency) VALUES (1, 'src', 'QSTK', NULL, 0.1, 4, NULL),
		    (2, 'src', 'QSTK', `+itoa(snap-30*86400)+`, 0.2, 2, 'CHF'),
		    (3, 'src', 'QSTK', `+itoa(snap+30*86400)+`, 0.3, 1, NULL)`,
	)
	in, err := YieldInputs(ctx, db, snap+86399, "USD")
	if err != nil {
		t.Fatal(err)
	}
	var bond, stock yield.Holding
	for _, h := range in.Holdings {
		switch h.Instrument {
		case "QB":
			bond = h
		case "QSTK":
			stock = h
		}
	}
	if bond.Amount != 10125 || bond.Accrued == nil || *bond.Accrued != 125 || bond.Key != HoldingKeyPrefix+"src:QB" {
		t.Errorf("bond %+v", bond)
	}
	if stock.Key != "symbol:QSTK" || stock.SecurityID != "symbol:QSTK" {
		t.Errorf("stock keys %q %q", stock.Key, stock.SecurityID)
	}
	rates := func(acct string) []float64 {
		var out []float64
		for _, r := range in.Rates[yield.AccountKey{Source: "src", Account: acct}] {
			out = append(out, r.Pct)
		}
		return out
	}
	if got := rates("S"); len(got) != 1 || got[0] != 1.5 {
		t.Errorf("stated rates %v", got)
	}
	if got := rates("D"); !slices.Equal(got, []float64{2.5, 8}) {
		t.Errorf("declared rates %v", got)
	}
	if got := rates("U"); !slices.Equal(got, []float64{0.4, 0.2}) {
		t.Errorf("snapshot rates %v", got)
	}
	if f := in.Funds["symbol:QMF"]; f == nil || f.Risk["USD"]["3M"] != 5 || f.Risk["USD"]["1Y"] != 2 ||
		f.RiskAsOf == nil || f.RiskAsOf.Unix() != snap-10*86400 || f.NetAssets == nil || *f.NetAssets != 1e6 ||
		f.SevenDayYield == nil || f.SevenDayYield.Value != 4.2 {
		t.Errorf("fund risk and figures %+v", f)
	}
	if f := in.Funds["symbol:QOF"]; f == nil || f.Risk != nil || f.SECYield != nil {
		t.Errorf("a fund's figures beyond max_gap_days %+v", f)
	}
	if d := in.Dividends[yield.InstrumentKey{Source: "src", Instrument: "QSTK"}]; d.Amount != 0.2 ||
		d.Frequency != 2 || d.Currency != "CHF" {
		t.Errorf("dividend %+v", d)
	}
	bk := yield.HoldingKey{Source: "src", Account: "B", PositionKey: "QB"}
	if p := in.Payments[bk]; len(p) != 1 || p[0].Amount != 250 || p[0].Type != yield.TypeCoupons {
		t.Errorf("payments %+v", p)
	}
	if p := in.AccountPayments; len(p) != 2 || p[0].Amount != 12 || p[0].Type != yield.TypeInterest ||
		p[1].Amount != 700 || p[1].Type != yield.TypeRent {
		t.Errorf("account payments %+v", p)
	}
	// Held on the instant's day alone: one of the year's 365 days.
	if d := in.Daily[bk]; d.Days != 365 || math.Abs(d.Mean()-10125.0/365) > 1e-9 {
		t.Errorf("daily %+v", d)
	}
	// The newer of two fields that fill one figure; the curve's newest
	// point of each tenor within 31 days, its benchmark the 3M.
	if f := in.Funds["symbol:QMF"]; f == nil || f.MaturityYears == nil || f.MaturityYears.Value != 0.2 {
		t.Errorf("fund %+v", f)
	}
	if c := in.Curves["USD"]; c == nil || c.Benchmark != 4 || c.BenchmarkMonths != 3 {
		t.Errorf("curve %+v", c)
	}
	// Each received line is named by its holding: a held one's, the
	// account's cash for its interest, `income:<type>:<CCY>` for its rent,
	// its own instrument for one sold.
	lines, err := incomeReceived(ctx, db, in, ymd(2026, 1, 1).Unix(), snap+86399)
	if err != nil {
		t.Fatal(err)
	}
	var keys []string
	for _, l := range lines {
		keys = append(keys, l.Holding.Account+"/"+l.Holding.PositionKey+"="+string(l.Type))
	}
	if got := strings.Join(keys, " "); got != "B/QOLD=interest B/QB=coupons S/cash:USD=interest S/income:rent:USD=rent" {
		t.Errorf("received %s", got)
	}
	// The four quarters to the day's: one restated in a later filing, one
	// filed after the day.
	if f := in.Securities["symbol:QSTK"]; f.EPS == nil || *f.EPS != 1+1+1+5 || f.DPS == nil || *f.DPS != 1 {
		t.Errorf("eps and dps %+v", f)
	}
}

func itoa(v int64) string { return strconv.FormatInt(v, 10) }

// bookInstrument is an instrument of an invented book: a stock, by the
// ids a source gives it ("" for none).
type bookInstrument struct{ src, iid, isin, cusip, symbol string }

// bookLine is a position of an invented book, keyed by its instrument:
// from each segment's day on, its quantity and value at every month end
// (none at zero), in dollars.
type bookLine struct {
	src, acct, iid string
	segs           []bookSeg
}

type bookSeg struct {
	from       time.Time
	qty, value float64
}

// bookTxn is a transaction of an invented book: a dividend, a buy or a
// sell.
type bookTxn struct {
	src, acct, iid string
	day            time.Time
	kind           string
	amount, qty    float64
}

// dividendBook is an invented book for the dividend tests: its
// instruments, positions and transactions; the portfolio of each account
// that has one, by account; the currency of each instrument quoted in
// another than the dollar, by instrument; and statements run after the
// passes, on what the inputs read as it stands.
type dividendBook struct {
	instruments []bookInstrument
	lines       []bookLine
	txns        []bookTxn
	portfolios  map[string]string
	currencies  map[string]string
	after       []string
}

// inputs writes the book into a migrated gold: each line's positions at
// the month ends from 30 June 2025 to 30 June 2026, at noon; the
// transactions, a dividend's as one; the instruments, listed in their
// currency; and a brokerage account for each account named. Amounts are
// in dollars, or in their instrument's currency. It runs the exposure
// and yield passes, then after, and reads the inputs at the last second
// of 30 June 2026.
func (bk dividendBook) inputs(t *testing.T) (*yield.Inputs, *sql.DB, context.Context) {
	t.Helper()
	db, ctx := openMigrated(t)
	str := func(s string) string {
		if s == "" {
			return "NULL"
		}
		return "'" + s + "'"
	}
	ccy := func(iid string) string { return cmp.Or(bk.currencies[iid], "USD") }
	var insts, positions, rows, kinds []string
	accounts := map[[2]string]bool{}
	for _, i := range bk.instruments {
		insts = append(insts, "('"+i.src+"', '"+i.iid+"', 'public_equity', 'stock', "+str(i.isin)+", "+str(i.cusip)+
			", "+str(i.symbol)+", 'Example "+i.iid+"', '"+ccy(i.iid)+"', 0, 0)")
	}
	for _, l := range bk.lines {
		accounts[[2]string{l.src, l.acct}] = true
		for m := 0; m <= 12; m++ {
			d := ymd(2025, 7+m, 1).AddDate(0, 0, -1)
			var s bookSeg
			for _, x := range l.segs {
				if !x.from.After(d) {
					s = x
				}
			}
			if s.qty != 0 {
				positions = append(positions, "('"+l.src+"', "+itoa(d.Unix()+43200)+", '"+l.acct+"', '"+l.iid+"', '"+
					l.iid+"', 'public_equity', 'stock', '"+ccy(l.iid)+"', "+ftoa(s.qty)+", "+ftoa(s.value)+")")
			}
		}
	}
	for i, x := range bk.txns {
		accounts[[2]string{x.src, x.acct}] = true
		id := "T" + strconv.Itoa(i)
		rows = append(rows, "('"+x.src+"', '"+id+"', "+itoa(x.day.Unix()+36000)+", '"+x.acct+"', '"+x.kind+
			"', '"+ccy(x.iid)+"', "+ftoa(x.amount)+", '"+x.iid+"', "+ftoa(x.qty)+")")
		if x.kind == "dividend" {
			kinds = append(kinds, "('"+x.src+"', '"+id+"', 'sig-"+x.iid+"', 1, 'INCOME_DIVIDENDS', 'rule', 100)")
		}
	}
	var accts []string
	for _, k := range slices.SortedFunc(maps.Keys(accounts), func(a, b [2]string) int {
		return cmp.Or(cmp.Compare(a[0], b[0]), cmp.Compare(a[1], b[1]))
	}) {
		accts = append(accts, "('"+k[0]+"', '"+k[1]+"', 'brokerage', '"+k[1]+"', 'USD', 0, 0, "+
			str(bk.portfolios[k[1]])+")")
	}
	execEach(t, db,
		`INSERT INTO accounts (silver_source_id, account_external_id, account_kind, display_name, base_currency,
		    first_seen_at, last_seen_at, portfolio_external_id) VALUES `+strings.Join(accts, ", "),
		`INSERT INTO instruments (silver_source_id, instrument_external_id, asset_class, vehicle, isin, cusip, symbol,
		    name, currency, first_seen_at, last_seen_at) VALUES `+strings.Join(insts, ", "),
		`INSERT INTO positions (silver_source_id, snapshot_at, account_external_id, position_key, instrument_external_id,
		    asset_class, vehicle, currency, quantity, market_value) VALUES `+strings.Join(positions, ", "),
		`INSERT INTO transactions (silver_source_id, transaction_external_id, occurred_at, account_external_id, kind,
		    currency, net_amount, instrument_external_id, quantity) VALUES `+strings.Join(rows, ", "),
		`INSERT INTO income_txn_enrichment (silver_source_id, transaction_external_id, payer_signature,
		    signature_version, income_detailed, provenance, assigned_at) VALUES `+strings.Join(kinds, ", "))
	rebuildForYield(t, db)
	execEach(t, db, bk.after...)
	// The calendar rows an after statement adds teach their gaps, as the
	// yield pass's last step does.
	rebuildGaps(t, db)
	in, err := YieldInputs(ctx, db, ymd(2026, 6, 30).Unix()+86399, "USD")
	if err != nil {
		t.Fatal(err)
	}
	return in, db, ctx
}

// A security moved from one source to another pools what the first paid
// and held: its sold positions there, under their own instrument. The
// book is invented: A at source old held 100 of an ISIN until it sold
// them on 20 May, paid 0.5 a share each quarter to 15 May; N at source
// new holds 100 of the same ISIN from 31 May, priced at 50, and has been
// paid nothing yet. N's holding pays 0.5 a share four times a year, and
// its realized yield is what A received over the amounts A and N held:
// some source held it every day.
func TestASecurityMovedBetweenSources(t *testing.T) {
	t.Parallel()
	early := ymd(2025, 1, 1)
	var txns []bookTxn
	for _, d := range []time.Time{ymd(2025, 8, 15), ymd(2025, 11, 15), ymd(2026, 2, 15), ymd(2026, 5, 15)} {
		txns = append(txns, bookTxn{"old", "A", "XMOV", d, "dividend", 50, 0})
	}
	txns = append(txns, bookTxn{"old", "A", "XMOV", ymd(2026, 5, 20), "sell", 5000, -100})
	in, _, _ := dividendBook{instruments: []bookInstrument{
		{"old", "XMOV", "XX0000000001", "", "XMOV"}, {"old", "QPEER", "", "", "QPEER"},
		{"new", "YMOV", "XX0000000001", "", "YMOV"},
	}, lines: []bookLine{
		{"old", "A", "XMOV", []bookSeg{{early, 100, 5000}, {ymd(2026, 5, 31), 0, 0}}},
		{"old", "A2", "QPEER", []bookSeg{{early, 10, 100}}},
		{"new", "N", "YMOV", []bookSeg{{ymd(2026, 5, 31), 100, 5000}}},
	}, txns: txns}.inputs(t)
	moved := yield.InstrumentKey{Source: "old", Instrument: "XMOV"}
	if s := in.HeldSecurity[moved]; s != "isin:XX0000000001" {
		t.Errorf("XMOV's security %q", s)
	}
	if d := in.FormerDaily[moved]; len(d) != 1 || d[0].Currency != "USD" || len(d[0].Spans) != 1 ||
		d[0].Spans[0].From != 0 {
		t.Errorf("XMOV's amounts %+v", d)
	}
	p := position(yield.Compute(in), yield.HoldingKey{Source: "new", Account: "N", PositionKey: "YMOV"})
	if p.DividendYield == nil || math.Abs(*p.DividendYield-0.5*4/50*100) > 1e-9 || !p.Tags[yield.TagIndicated] {
		t.Errorf("dividend yield %s, tags %v", fmtFloatPtr(p.DividendYield), p.Tags)
	}
	if p.Realized == nil || math.Abs(*p.Realized-200.0/5000*100) > 1e-9 || p.Tags[yield.TagPartial] {
		t.Errorf("realized %s, tags %v", fmtFloatPtr(p.Realized), p.Tags)
	}
}

// The config's holding aliases fold too, as the gains reports read them
// (holdingAliasJoins): an account's own alias, else its source's, before
// the instrument aliases. Each stock pays 0.5 a share every quarter and is
// priced at 50, its quantity read under the alias too. The book is
// invented:
//
//   - B1 holds QHAL and is paid under the CUSIP id 000000AE4, which its
//     source also keys by its ticker QSHD: the source's holding alias to
//     QHAL wins over the ticker.
//   - DEPOT keyed its holding by an overview key, XOVR, until January,
//     and by QDEP, a share of the United States, since. Its alias reads
//     XOVR as QDEP for DEPOT alone, over the source's alias of XOVR to
//     QOTH. CASH, beside it in portfolio BANK, is paid under XOVR: the
//     lines are DEPOT's holding's, on what DEPOT held under both keys on
//     their record days, and its realized yield pools the amounts under
//     both.
//   - H's statement valued its holding in francs under ASTM until
//     January; its feed values it in dollars under QFED since. The
//     statement's amounts convert at the franc's rate, not the holding's.
//     M's one position key, QMIX, reads the same way in both currencies.
//     No instrument states a listing currency: each reads the dollars its
//     newest snapshot values it in, though ASTM sorts before QFED.
func TestAHoldingAliasFolds(t *testing.T) {
	t.Parallel()
	early, feb := ymd(2025, 1, 1), ymd(2026, 1, 31)
	var txns []bookTxn
	for _, d := range []time.Time{ymd(2025, 9, 10), ymd(2025, 12, 10), ymd(2026, 3, 10), ymd(2026, 6, 10)} {
		txns = append(txns, bookTxn{"src", "B1", "000000AE4", d, "dividend", 50, 0},
			bookTxn{"src", "CASH", "XOVR", d, "dividend", 50, 0}, bookTxn{"src", "H", "QFED", d, "dividend", 50, 0},
			bookTxn{"src", "M", "QMIX", d, "dividend", 50, 0})
	}
	in, _, _ := dividendBook{instruments: []bookInstrument{
		{"src", "000000AE4", "", "000000AE4", "QSHD"}, {"src", "QSHD", "", "", "QSHD"}, {"src", "QHAL", "", "", "QHAL"},
		{"src", "XOVR", "", "", ""}, {"src", "QDEP", "US0000000008", "", "QDEP"}, {"src", "ASTM", "", "", ""},
		{"src", "QFED", "", "", "QFED"}, {"src", "QMIX", "", "", "QMIX"},
	}, lines: []bookLine{
		{"src", "B1", "QHAL", []bookSeg{{early, 100, 5000}}},
		{"src", "DEPOT", "XOVR", []bookSeg{{early, 100, 5000}, {feb, 0, 0}}},
		{"src", "DEPOT", "QDEP", []bookSeg{{feb, 100, 5000}}},
		{"src", "H", "ASTM", []bookSeg{{early, 100, 4000}, {feb, 0, 0}}},
		{"src", "H", "QFED", []bookSeg{{feb, 100, 5000}}},
		{"src", "M", "QMIX", []bookSeg{{early, 100, 4000}, {feb, 100, 5000}}},
	}, txns: txns, portfolios: map[string]string{"DEPOT": "BANK", "CASH": "BANK"}, after: []string{
		`INSERT INTO holding_aliases (silver_source_id, account_external_id, from_key, to_key) VALUES
		    ('src', '', '000000AE4', 'QHAL'), ('src', 'DEPOT', 'XOVR', 'QDEP'), ('src', '', 'XOVR', 'QOTH'),
		    ('src', '', 'ASTM', 'QFED')`,
		`UPDATE positions SET currency = 'CHF' WHERE position_key = 'ASTM'
		    OR (position_key = 'QMIX' AND snapshot_at < ` + itoa(feb.Unix()) + `)`,
		`UPDATE instruments SET currency = NULL WHERE instrument_external_id IN ('ASTM', 'QFED', 'QMIX')`,
		`INSERT INTO fx_rates (silver_source_id, snapshot_at, base_currency, quote_currency, mid_rate)
		    VALUES ('fx', 0, 'USD', 'CHF', 1.25)`,
	}}.inputs(t)
	for k, want := range map[yield.AliasKey]string{
		{Source: "src", ID: "000000AE4"}: "QHAL", {Source: "src", Account: "DEPOT", ID: "XOVR"}: "QDEP",
		{Source: "src", ID: "XOVR"}: "QOTH", {Source: "src", ID: "ASTM"}: "QFED",
	} {
		if got := in.Aliases[k]; got != want {
			t.Errorf("%v reads as %q, want %q", k, got, want)
		}
	}
	if f := in.FormerDaily[yield.InstrumentKey{Source: "src", Instrument: "QFED"}]; len(f) != 1 ||
		f[0].Currency != "CHF" || in.FXRates["CHF"] != 1.25 {
		t.Errorf("QFED's former amounts %+v, the franc at %v", f, in.FXRates["CHF"])
	}
	b := yield.Compute(in)
	for _, k := range []yield.HoldingKey{{Source: "src", Account: "B1", PositionKey: "QHAL"},
		{Source: "src", Account: "DEPOT", PositionKey: "QDEP"}, {Source: "src", Account: "H", PositionKey: "QFED"},
		{Source: "src", Account: "M", PositionKey: "QMIX"}} {
		ps := in.Payments[k]
		if len(ps) != 4 || in.Income[k].Total() != 200 {
			t.Errorf("%s: payments %+v, income %v", k.PositionKey, ps, in.Income[k])
			continue
		}
		// Each is a share quoted in dollars: its record days read what its
		// account held under every key, QDEP's what DEPOT held.
		for _, x := range ps {
			if x.Quantity == nil || *x.Quantity != 100 || x.LagDays != yield.RecordLagUSShare.Days || x.Record == nil ||
				*x.Record != 100 || x.Before != nil {
				t.Errorf("%s on %s: quantity %s, record %s, before %s, lag %d", k.PositionKey,
					x.Day.Format(time.DateOnly), fmtFloatPtr(x.Quantity), fmtFloatPtr(x.Record), fmtFloatPtr(x.Before),
					x.LagDays)
			}
		}
		p := position(b, k)
		if p.DividendYield == nil || math.Abs(*p.DividendYield-4) > 1e-9 || p.Realized == nil ||
			math.Abs(*p.Realized-4) > 1e-9 || p.Tags[yield.TagPartial] {
			t.Errorf("%s: dividend yield %s, realized %s, tags %v", k.PositionKey, fmtFloatPtr(p.DividendYield),
				fmtFloatPtr(p.Realized), p.Tags)
		}
	}
}
