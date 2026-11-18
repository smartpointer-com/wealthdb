package gold

import (
	"context"
	"database/sql"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/enrichtest"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/yield"
)

// dumpValue writes v in full, in one order at every run: a map by its
// keys' text, a pointer by what it points to, unexported fields too. It
// shows any change yield.Compute makes to what the inputs reach.
func dumpValue(b *strings.Builder, v reflect.Value, depth int) {
	if depth > 40 {
		b.WriteString("…")
		return
	}
	if v.IsValid() && v.Type() == reflect.TypeOf(time.Time{}) && v.CanInterface() {
		fmt.Fprint(b, v.Interface().(time.Time).UTC().Format(time.RFC3339Nano))
		return
	}
	switch v.Kind() {
	case reflect.Invalid:
		b.WriteString("nil")
	case reflect.Pointer, reflect.Interface:
		if v.IsNil() {
			b.WriteString("nil")
			return
		}
		b.WriteString("&")
		dumpValue(b, v.Elem(), depth+1)
	case reflect.Map:
		keys := v.MapKeys()
		text := make([]string, len(keys))
		for i, k := range keys {
			var kb strings.Builder
			dumpValue(&kb, k, depth+1)
			text[i] = kb.String()
		}
		order := make([]int, len(keys))
		for i := range order {
			order[i] = i
		}
		slices.SortFunc(order, func(a, c int) int { return strings.Compare(text[a], text[c]) })
		b.WriteString("map[")
		for _, i := range order {
			b.WriteString(text[i] + ":")
			dumpValue(b, v.MapIndex(keys[i]), depth+1)
			b.WriteString(" ")
		}
		b.WriteString("]")
	case reflect.Slice, reflect.Array:
		b.WriteString("[")
		for i := range v.Len() {
			dumpValue(b, v.Index(i), depth+1)
			b.WriteString(" ")
		}
		b.WriteString("]")
	case reflect.Struct:
		b.WriteString("{")
		for i := range v.NumField() {
			b.WriteString(v.Type().Field(i).Name + ":")
			dumpValue(b, v.Field(i), depth+1)
			b.WriteString(" ")
		}
		b.WriteString("}")
	case reflect.Func, reflect.Chan, reflect.UnsafePointer:
		b.WriteString(v.Kind().String())
	case reflect.Float32, reflect.Float64:
		fmt.Fprintf(b, "%b", v.Float())
	case reflect.Bool:
		fmt.Fprint(b, v.Bool())
	case reflect.String:
		fmt.Fprintf(b, "%q", v.String())
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		fmt.Fprint(b, v.Int())
	default:
		fmt.Fprint(b, v.Uint())
	}
}

// dump is dumpValue of v, as a string.
func dump(v any) string {
	var b strings.Builder
	dumpValue(&b, reflect.ValueOf(v), 0)
	return b.String()
}

// seedYieldBookShared is seedYieldBookFX's book with a fund's
// distribution received. Invented.
func seedYieldBookShared(t *testing.T) (*sql.DB, context.Context, int64) {
	t.Helper()
	db, ctx, at := seedYieldBookFX(t)
	execEach(t, db, `INSERT INTO transactions (silver_source_id, transaction_external_id, occurred_at, account_external_id, kind,
		    currency, net_amount, instrument_external_id) VALUES
		    ('test-src', 'D1', `+itoa(ymd(2026, 3, 16).Unix())+`, 'A', 'dividend', 'USD', 12, 'IDXF')`)
	enrichtest.SeedIncome(t, db, ruleIncome("D1", "sig-fund", "INCOME_DIVIDENDS"))
	return db, ctx, at
}

// The inputs of an instant's currencies share the base's maps, its funds
// and the attribution of its income lines (readYieldBase, attribution).
// yield.Compute reads them and changes none of them; what it does change
// is its own currency's (the copy's indexes, which Index makes afresh).
// So each currency's inputs from the shared read are what a read of that
// currency alone gives.
func TestComputeLeavesTheSharedInputsAlone(t *testing.T) {
	t.Parallel()
	db, ctx, at := seedYieldBookShared(t)
	// A short bond fund's filed sensitivities: Compute flips their sign
	// on a copy of its own, never on the fund the currencies share.
	execEach(t, db,
		`INSERT INTO fund_risk (silver_source_id, fund_ref, as_of, currency, tenor, dv01) VALUES
		    ('ref', 'symbol:BNDF', `+itoa(at-86399)+`, 'USD', '2Y', 0.4), ('ref', 'symbol:BNDF', `+itoa(at-86399)+`,
		    'USD', '10Y', 1.1)`,
		`INSERT INTO fund_characteristics (silver_source_id, fund_ref, as_of, provider, field, label, value) VALUES
		    ('ref', 'symbol:BNDF', `+itoa(at-86399)+`, 'ishares', 'effective_duration', '', -1.5)`)
	base, err := readYieldBase(ctx, db, at, &yieldShared{}, materializeCurrencies)
	if err != nil {
		t.Fatal(err)
	}
	sawFunds, sawPayments := false, false
	for _, ccy := range materializeCurrencies {
		in := base.inputs(ccy)
		sawFunds = sawFunds || in.Funds["symbol:BNDF"] != nil && in.Funds["symbol:BNDF"].Risk != nil
		sawPayments = sawPayments || len(in.Payments) > 0
		shared := func() string {
			return dump(base.in) + dump(base.funds) + dump(base.valued) + dump(base.attributions)
		}
		before := shared()
		yield.Compute(in)
		if after := shared(); after != before {
			t.Errorf("%s: yield.Compute changed the inputs the currencies share", ccy)
		}
		own, err := YieldInputs(ctx, db, at, ccy)
		if err != nil {
			t.Fatal(err)
		}
		for name, pair := range map[string][2]any{"holdings": {in.Holdings, own.Holdings},
			"funds": {in.Funds, own.Funds}, "lines": {in.Lines, own.Lines}, "income": {in.Income, own.Income},
			"account income": {in.AccountIncome, own.AccountIncome}, "payments": {in.Payments, own.Payments},
			"account payments": {in.AccountPayments, own.AccountPayments}, "fx": {in.FXRates, own.FXRates},
			"situs": {in.Situs, own.Situs}, "daily": {in.Daily, own.Daily},
			"former daily": {in.FormerDaily, own.FormerDaily}} {
			if a, b := dump(pair[0]), dump(pair[1]); a != b {
				t.Errorf("%s %s: shared read %s, a read of its own %s", ccy, name, a, b)
			}
		}
		for k, f := range own.Securities {
			if dump(in.Securities[k]) != dump(f) {
				t.Errorf("%s security %s: shared read %s, a read of its own %s", ccy, k, dump(in.Securities[k]),
					dump(f))
			}
		}
	}
	if !sawFunds || !sawPayments {
		t.Fatalf("the fixture holds a fund with a filed sensitivity: %v, pays: %v", sawFunds, sawPayments)
	}
}

// The currencies whose holdings are the same attribute the income lines
// once: their inputs share the payments. A currency no rate converts the
// holdings to holds none of them, and its lines are attributed apart, as
// a read of that currency alone attributes them.
func TestCurrenciesOfOneHoldingsShareTheirPayments(t *testing.T) {
	t.Parallel()
	db, ctx, at := seedYieldBookShared(t)
	execEach(t, db, `DELETE FROM fx_rates WHERE 'GBP' IN (base_currency, quote_currency)`)
	base, err := readYieldBase(ctx, db, at, &yieldShared{}, materializeCurrencies)
	if err != nil {
		t.Fatal(err)
	}
	usd, gbp := base.inputs("USD"), base.inputs("GBP")
	if len(usd.Holdings) == 0 || len(gbp.Holdings) != 0 || len(usd.Payments) == 0 {
		t.Fatalf("%d dollar holdings and %d payments, %d sterling holdings", len(usd.Holdings), len(usd.Payments),
			len(gbp.Holdings))
	}
	for _, ccy := range []string{"CHF", "EUR"} {
		if in := base.inputs(ccy); reflect.ValueOf(in.Payments).UnsafePointer() !=
			reflect.ValueOf(usd.Payments).UnsafePointer() {
			t.Errorf("%s reads payments of its own", ccy)
		}
	}
	if len(base.attributions) != 2 {
		t.Errorf("%d attributions, want the one of the converted holdings and the one of none", len(base.attributions))
	}
	own, err := YieldInputs(ctx, db, at, "GBP")
	if err != nil {
		t.Fatal(err)
	}
	if a, b := dump(gbp.Payments)+dump(gbp.AccountPayments)+dump(gbp.Income)+dump(gbp.AccountIncome),
		dump(own.Payments)+dump(own.AccountPayments)+dump(own.Income)+dump(own.AccountIncome); a != b {
		t.Errorf("sterling: shared read %s, a read of its own %s", a, b)
	}
}
