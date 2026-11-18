package gold

import (
	"math"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/yield"
)

// Interest a bond pays is a coupon, but a bill's discount is interest,
// as the projection expects it; so is a bond's whose zero coupon only a
// ledger record states.
func TestABillsDiscountIsInterest(t *testing.T) {
	t.Parallel()
	db, ctx := openMigrated(t)
	execEach(t, db,
		`INSERT INTO accounts (silver_source_id, account_external_id, account_kind, display_name, base_currency,
		    first_seen_at, last_seen_at) VALUES ('src', 'B', 'brokerage', 'Brokerage', 'USD', 0, 0)`,
		`INSERT INTO instruments (silver_source_id, instrument_external_id, asset_class, vehicle, name, currency,
		    first_seen_at, last_seen_at) VALUES ('src', 'QB', 'fixed_income', 'bond', 'EXAMPLE NOTE', 'USD', 0, 0),
		    ('src', 'QBILL', 'fixed_income', 'bond', 'EXAMPLE DISCOUNT NOTE', 'USD', 0, 0),
		    ('src', 'QZD', 'fixed_income', 'bond', 'EXAMPLE DEEP NOTE', 'USD', 0, 0)`,
		`INSERT INTO instrument_terms (silver_source_id, instrument_external_id, vehicle, coupon_pct, coupon_kind)
		    VALUES ('src', 'QB', 'bond', 4.5, 'fixed'), ('src', 'QBILL', 'bond', 0, 'zero')`,
		`INSERT INTO yield_declared_terms (line_no, silver_source_id, instrument_external_id, coupon_pct)
		    VALUES (1, 'src', 'QZD', 0)`,
		`INSERT INTO transactions (silver_source_id, transaction_external_id, occurred_at, account_external_id, kind,
		    currency, net_amount, instrument_external_id) VALUES
		    ('src', 'T1', `+itoa(ymd(2026, 3, 15).Unix())+`, 'B', 'interest', 'USD', 225, 'QB'),
		    ('src', 'T2', `+itoa(ymd(2026, 4, 30).Unix())+`, 'B', 'interest', 'USD', 120, 'QBILL'),
		    ('src', 'T3', `+itoa(ymd(2026, 5, 15).Unix())+`, 'B', 'interest', 'USD', 300, 'QZD')`,
		`INSERT INTO income_txn_enrichment (silver_source_id, transaction_external_id, payer_signature,
		    signature_version, income_detailed, provenance, assigned_at) VALUES
		    ('src', 'T1', 'sig-note', 1, 'INCOME_INTEREST_EARNED', 'rule', 100),
		    ('src', 'T2', 'sig-bill', 1, 'INCOME_INTEREST_EARNED', 'rule', 100),
		    ('src', 'T3', 'sig-deep', 1, 'INCOME_INTEREST_EARNED', 'rule', 100)`,
	)
	got := map[string]yield.IncomeType{}
	if err := incomeLinesIn(ctx, db, ymd(2026, 1, 1).Unix(), ymd(2026, 6, 30).Unix(), []string{"USD"},
		func(l incomeLine, _ []*float64) {
			got[l.Instrument] = l.Type
		}); err != nil {
		t.Fatal(err)
	}
	if got["QB"] != yield.TypeCoupons || got["QBILL"] != yield.TypeInterest || got["QZD"] != yield.TypeInterest {
		t.Errorf("types %v", got)
	}
}

// Every detailed type incomeTypeSQL reads an income line by is one of
// the income taxonomy's, and the cash flow projection's other income
// leaves out exactly those (projectedIncomeDetailed), so it counts none
// twice and drops none.
func TestIncomeTypesReadTheTaxonomy(t *testing.T) {
	t.Parallel()
	read := regexp.MustCompile(`income_detailed = '(\w+)'`).FindAllStringSubmatch(incomeTypeSQL, -1)
	if len(read) == 0 {
		t.Fatal("incomeTypeSQL reads no detailed type")
	}
	var values []string
	for _, m := range read {
		if !canonical.ValidIncomeDetailed(m[1]) {
			t.Errorf("%s is no detailed income type", m[1])
		}
		values = append(values, m[1])
	}
	slices.Sort(values)
	values = slices.Compact(values)
	if want := slices.Sorted(slices.Values(projectedIncomeDetailed)); !slices.Equal(values, want) {
		t.Errorf("incomeTypeSQL reads %v; projectedIncomeDetailed lists %v", values, want)
	}
}

// A broker books a Treasury's coupon as the account's interest, naming
// no instrument; its description names the bond by coupon and maturity.
// Such a line is no cash's interest. It is the coupon of the one bond the
// account holds that it names. Where the account holds none, two, or one
// whose terms state no maturity, it is the account's coupons, and so is a
// coupon whose line names a bond no longer held. A line that names its
// bond keeps it, and a deposit's interest that states a rate and an end
// is still interest. The book, the descriptions and the terms are
// invented, in a broker's shapes.
func TestACouponBookedAsInterestIsTheBonds(t *testing.T) {
	t.Parallel()
	db, ctx := openMigrated(t)
	start, snap := ymd(2025, 6, 30).Unix(), ymd(2026, 6, 30).Unix()
	var txns, kinds []string
	for i, l := range []struct {
		acct, instrument, desc, amt string
		day                         time.Time
	}{{"B", "", "US TREASU NT 9.875%09/99UST NOTE DUE 09/09/99", "98.75", ymd(2026, 3, 9)},
		{"B", "000000001", "US TREASURY 9.75%08/98UST BOND DUE 08/08/98", "1", ymd(2026, 3, 10)},
		{"B", "", "US TREASURY 9.75%08/98UST BOND DUE 08/08/98", "97.50", ymd(2026, 2, 9)},
		{"B", "", "EXAMPLE BANK CD 9.25% DUE 09/09/99", "4", ymd(2026, 4, 1)},
		{"B", "", "SCHWAB1 INT 01/01-02/01", "30", ymd(2026, 6, 25)},
		{"B", "000000009", "", "11.25", ymd(2026, 5, 1)},
		{"C", "", "US TREASU NT 1.125%07/97UST NOTE DUE 07/07/97", "11.25", ymd(2026, 1, 7)},
		{"D", "", "US TREASU NT 9.875%09/99UST NOTE DUE 09/09/99", "9.87", ymd(2026, 3, 9)},
		{"E", "", "US TREASU NT 9.5%05/98UST NOTE DUE 05/05/98", "9.50", ymd(2026, 5, 5)}} {
		id := "T" + itoa(int64(i))
		instrument := "NULL"
		if l.instrument != "" {
			instrument = "'" + l.instrument + "'"
		}
		txns = append(txns, "('src', '"+id+"', "+itoa(l.day.Unix())+", '"+l.acct+"', 'interest', 'USD', "+l.amt+
			", '"+l.desc+"', "+instrument+")")
		kinds = append(kinds, "('src', '"+id+"', 'sig-"+id+"', 1, 'INCOME_INTEREST_EARNED', 'rule', 100)")
	}
	var positions []string
	for _, at := range []int64{start, snap} {
		for _, p := range [][2]string{{"B", "000000001"}, {"C", "000000003"}, {"C", "000000004"}, {"E", "000000005"}} {
			positions = append(positions, "('src', "+itoa(at)+", '"+p[0]+"', '"+p[1]+"', '"+p[1]+
				"', 'fixed_income', 'bond', 'USD', 1000, 990)")
		}
	}
	execEach(t, db,
		`INSERT INTO accounts (silver_source_id, account_external_id, account_kind, display_name, base_currency,
		    first_seen_at, last_seen_at) VALUES ('src', 'B', 'brokerage', 'Brokerage', 'USD', 0, 0),
		    ('src', 'C', 'brokerage', 'Brokerage C', 'USD', 0, 0), ('src', 'D', 'brokerage', 'Brokerage D', 'USD', 0, 0),
		    ('src', 'E', 'brokerage', 'Brokerage E', 'USD', 0, 0)`,
		`INSERT INTO instruments (silver_source_id, instrument_external_id, asset_class, vehicle, name, currency,
		    first_seen_at, last_seen_at) VALUES
		    ('src', '000000001', 'fixed_income', 'bond', 'US TREASURY NOTE 9.875% 09/09/99', 'USD', 0, 0),
		    ('src', '000000003', 'fixed_income', 'bond', 'US TREASURY NOTE 1.125% 07/07/97', 'USD', 0, 0),
		    ('src', '000000004', 'fixed_income', 'bond', 'US TREASURY NOTE 1.125% 07/07/97', 'USD', 0, 0),
		    ('src', '000000005', 'fixed_income', 'bond', 'US TREASURY NOTE 9.5%', 'USD', 0, 0),
		    ('src', '000000009', 'fixed_income', 'bond', 'US TREASURY NOTE 1.375% 03/03/97', 'USD', 0, 0)`,
		`INSERT INTO positions (silver_source_id, snapshot_at, account_external_id, position_key, instrument_external_id,
		    asset_class, vehicle, currency, quantity, market_value) VALUES `+strings.Join(positions, ", "),
		`INSERT INTO cash_balances (silver_source_id, snapshot_at, account_external_id, currency, balance_kind, amount)
		    VALUES ('src', `+itoa(start)+`, 'B', 'USD', 'current', 2000), ('src', `+itoa(snap)+`, 'B', 'USD', 'current', 2000)`,
		`INSERT INTO transactions (silver_source_id, transaction_external_id, occurred_at, account_external_id, kind,
		    currency, net_amount, description, instrument_external_id) VALUES `+strings.Join(txns, ", "),
		`INSERT INTO income_txn_enrichment (silver_source_id, transaction_external_id, payer_signature,
		    signature_version, income_detailed, provenance, assigned_at) VALUES `+strings.Join(kinds, ", "))
	rebuildExposureWithDefaults(t, db, ctx)
	if _, err := RebuildYield(ctx, db); err != nil {
		t.Fatal(err)
	}
	in, err := YieldInputs(ctx, db, snap+86399, "USD")
	if err != nil {
		t.Fatal(err)
	}
	ac := func(acct string) yield.Received {
		return in.AccountIncome[yield.AccountCurrency{AccountKey: yield.AccountKey{Source: "src", Account: acct},
			Currency: "USD"}]
	}
	bond := yield.HoldingKey{Source: "src", Account: "B", PositionKey: "000000001"}
	if in.Income[bond][yield.TypeCoupons] != 99.75 || len(in.Payments[bond]) != 2 ||
		ac("B")[yield.TypeInterest] != 34 || ac("B")[yield.TypeCoupons] != 97.5+11.25 ||
		ac("C")[yield.TypeCoupons] != 11.25 || ac("D")[yield.TypeCoupons] != 9.87 || ac("E")[yield.TypeCoupons] != 9.5 {
		t.Errorf("the bond's %v %v; the accounts' %v %v %v %v", in.Income[bond], in.Payments[bond], ac("B"), ac("C"),
			ac("D"), ac("E"))
	}
	if p := in.AccountPayments; len(p) != 2 || p[0].Type != yield.TypeInterest || p[1].Type != yield.TypeInterest {
		t.Errorf("account payments %+v", p)
	}
	// The cash's realized yield is its own interest's and the deposit's:
	// 34 on 2,000.
	b := yield.Compute(in)
	cash := position(b, yield.HoldingKey{Source: "src", Account: "B", PositionKey: "cash:USD"})
	if cash.Realized == nil || math.Abs(*cash.Realized-1.7) > 1e-9 {
		t.Errorf("the cash's realized yield %v", cash.Realized)
	}
	lines, err := incomeReceived(ctx, db, in, ymd(2026, 1, 1).Unix(), snap+86399)
	if err != nil {
		t.Fatal(err)
	}
	var keys []string
	for _, l := range lines {
		keys = append(keys, l.Holding.Account+"/"+l.Holding.PositionKey+"="+string(l.Type))
	}
	if got, want := strings.Join(keys, " "), "C/income:coupons:USD=coupons B/income:coupons:USD=coupons "+
		"B/000000001=coupons D/income:coupons:USD=coupons B/000000001=coupons B/cash:USD=interest "+
		"B/income:coupons:USD=coupons E/income:coupons:USD=coupons B/cash:USD=interest"; got != want {
		t.Errorf("received\n%s\nwant\n%s", got, want)
	}
}

// A source may key one stock by its CUSIP and by its ticker
// (instrument_aliases): the lines and the holdings meet under either.
// Each stock pays 0.5 a share every quarter and is priced at 50. The book
// is invented:
//
//   - B1 holds QCSP under its CUSIP id, and is paid under its ticker;
//   - B1 holds QTKR under its ticker, and is paid under its CUSIP id;
//   - H holds QSLD under its ticker, and S held 300 under its CUSIP id
//     until it sold them on 1 April, paid under that id: S's lines are its
//     sold position's, under the ticker, which pools with H's holding.
func TestALineMeetsItsHoldingUnderEitherID(t *testing.T) {
	t.Parallel()
	early := ymd(2025, 1, 1)
	held := []bookSeg{{early, 100, 5000}}
	var txns []bookTxn
	for i, d := range []time.Time{ymd(2025, 9, 10), ymd(2025, 12, 10), ymd(2026, 3, 10), ymd(2026, 6, 10)} {
		txns = append(txns, bookTxn{"src", "B1", "QCSP", d, "dividend", 50, 0},
			bookTxn{"src", "B1", "000000AC2", d, "dividend", 50, 0}, bookTxn{"src", "H", "QSLD", d, "dividend", 50, 0})
		if i < 3 {
			txns = append(txns, bookTxn{"src", "S", "000000AD3", d, "dividend", 150, 0})
		}
	}
	txns = append(txns, bookTxn{"src", "S", "000000AD3", ymd(2026, 4, 1), "sell", 15000, -300})
	in, db, ctx := dividendBook{instruments: []bookInstrument{
		{"src", "000000AB1", "", "000000AB1", "QCSP"}, {"src", "QCSP", "", "", "QCSP"},
		{"src", "QTKR", "", "", "QTKR"}, {"src", "000000AC2", "", "000000AC2", "QTKR"},
		{"src", "QSLD", "", "", "QSLD"}, {"src", "000000AD3", "", "000000AD3", "QSLD"},
	}, lines: []bookLine{
		{"src", "B1", "000000AB1", held}, {"src", "B1", "QTKR", held}, {"src", "H", "QSLD", held},
		{"src", "S", "000000AD3", []bookSeg{{early, 300, 15000}, {ymd(2026, 4, 30), 0, 0}}},
	}, txns: txns}.inputs(t)
	b := yield.Compute(in)
	key := func(acct, id string) yield.HoldingKey {
		return yield.HoldingKey{Source: "src", Account: acct, PositionKey: id}
	}
	for _, k := range []yield.HoldingKey{key("B1", "000000AB1"), key("B1", "QTKR")} {
		ps := in.Payments[k]
		if len(ps) != 4 || ps[3].Quantity == nil || *ps[3].Quantity != 100 || in.Income[k].Total() != 200 {
			t.Errorf("%s: payments %+v, income %v", k.PositionKey, ps, in.Income[k])
		}
		p := position(b, k)
		if p.DividendYield == nil || math.Abs(*p.DividendYield-4) > 1e-9 || p.Realized == nil ||
			math.Abs(*p.Realized-4) > 1e-9 {
			t.Errorf("%s: dividend yield %s, realized %s", k.PositionKey, fmtFloatPtr(p.DividendYield),
				fmtFloatPtr(p.Realized))
		}
	}
	if got := in.Held[yield.InstrumentKey{Source: "src", Instrument: "QSLD"}]; !slices.Equal(got, []string{"H", "S"}) {
		t.Errorf("QSLD held by %v", got)
	}
	sold := key("S", "QSLD")
	if len(in.Payments[sold]) != 3 || in.Income[sold].Total() != 450 {
		t.Errorf("S's sold position: payments %+v, income %v", in.Payments[sold], in.Income[sold])
	}
	// S held 15,000 dollars of it to the snapshot of 30 April, which ends
	// its observation.
	mean := (5000*365 + 15000*303.0) / 365
	p := position(b, key("H", "QSLD"))
	if p.DividendYield == nil || math.Abs(*p.DividendYield-4) > 1e-9 || p.Realized == nil ||
		math.Abs(*p.Realized-650/mean*100) > 1e-9 {
		t.Errorf("QSLD: dividend yield %s, realized %s, want %v", fmtFloatPtr(p.DividendYield),
			fmtFloatPtr(p.Realized), 650/mean*100)
	}
	got, err := incomeReceived(ctx, db, in, ymd(2025, 7, 1).Unix(), ymd(2026, 6, 30).Unix()+86399)
	if err != nil {
		t.Fatal(err)
	}
	named := map[yield.HoldingKey]int{}
	for _, l := range got {
		named[l.Holding]++
	}
	if named[sold] != 3 || named[key("B1", "000000AB1")] != 4 || named[key("B1", "QTKR")] != 4 {
		t.Errorf("received lines %v", named)
	}
}

// A bank may book a dividend in another portfolio than the custody
// account's. A line of an account whose portfolio never held the
// instrument, where one account of its source alone did, is that
// account's sold position, and pools with the security's holdings. The
// book is invented: CUST, in portfolio P1 at the bank, held 100 of an
// ISIN until it sold them on 15 March; its dividends went to CSH, in P2.
// BRK at the broker holds 100 of the same ISIN, priced at 50. Each pays
// 0.5 a share a quarter: BRK's realized yield is what both received over
// the amounts both held.
func TestALineBookedInAnotherPortfolio(t *testing.T) {
	t.Parallel()
	early := ymd(2025, 1, 1)
	var txns []bookTxn
	for i, d := range []time.Time{ymd(2025, 9, 10), ymd(2025, 12, 10), ymd(2026, 3, 10), ymd(2026, 6, 10)} {
		txns = append(txns, bookTxn{"broker", "BRK", "YBRK", d, "dividend", 50, 0})
		if i < 3 {
			txns = append(txns, bookTxn{"bank", "CSH", "XBNK", d, "dividend", 50, 0})
		}
	}
	txns = append(txns, bookTxn{"bank", "CUST", "XBNK", ymd(2026, 3, 15), "sell", 5000, -100})
	in, db, ctx := dividendBook{instruments: []bookInstrument{
		{"bank", "XBNK", "XX0000000002", "", "XBNK"}, {"bank", "QPEER", "", "", "QPEER"},
		{"broker", "YBRK", "XX0000000002", "", "YBRK"},
	}, lines: []bookLine{
		{"bank", "CUST", "XBNK", []bookSeg{{early, 100, 5000}, {ymd(2026, 3, 31), 0, 0}}},
		{"bank", "PEER", "QPEER", []bookSeg{{early, 10, 100}}},
		{"broker", "BRK", "YBRK", []bookSeg{{early, 100, 5000}}},
	}, txns: txns, portfolios: map[string]string{"CUST": "P1", "PEER": "P1", "CSH": "P2"}}.inputs(t)
	sold := yield.HoldingKey{Source: "bank", Account: "CUST", PositionKey: "XBNK"}
	if k, ok := in.Sold("bank", "CSH", "XBNK"); !ok || k != sold {
		t.Errorf("CSH's line: %v %v", k, ok)
	}
	if ps := in.Payments[sold]; len(ps) != 3 || ps[0].Quantity == nil || *ps[0].Quantity != 100 ||
		in.Income[sold].Total() != 150 {
		t.Errorf("CUST's sold position: payments %+v, income %v", ps, in.Income[sold])
	}
	custody := ymd(2026, 3, 31).Sub(ymd(2025, 7, 1)).Hours() / 24
	mean := (5000*365 + 5000*custody) / 365
	p := position(yield.Compute(in), yield.HoldingKey{Source: "broker", Account: "BRK", PositionKey: "YBRK"})
	if p.Realized == nil || math.Abs(*p.Realized-350/mean*100) > 1e-9 {
		t.Errorf("realized %s, want %v", fmtFloatPtr(p.Realized), 350/mean*100)
	}
	got, err := incomeReceived(ctx, db, in, ymd(2025, 7, 1).Unix(), ymd(2026, 6, 30).Unix()+86399)
	if err != nil {
		t.Fatal(err)
	}
	named := 0
	for _, l := range got {
		if l.Holding == sold {
			named++
		}
	}
	if named != 3 {
		t.Errorf("%d received lines name CUST's sold position: %+v", named, got)
	}
}
