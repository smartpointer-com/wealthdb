package gold

import (
	"encoding/json"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

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

// The inputs of an instant's currencies share the base's maps and the
// funds the first currency to hold one reads (readYieldBase, readFunds).
// yield.Compute reads them and changes none of them; what it does change
// is its own currency's (the copy's indexes, which Index makes afresh).
// So the shared read gives each currency what a read of its own gives.
func TestComputeLeavesTheSharedInputsAlone(t *testing.T) {
	t.Parallel()
	db, ctx, at := seedYieldBookFX(t)
	// A short bond fund's filed sensitivities: Compute flips their sign
	// on a copy of its own, never on the fund the currencies share.
	execEach(t, db,
		`INSERT INTO fund_risk (silver_source_id, fund_ref, as_of, currency, tenor, dv01) VALUES
		    ('ref', 'symbol:BNDF', `+itoa(at-86399)+`, 'USD', '2Y', 0.4), ('ref', 'symbol:BNDF', `+itoa(at-86399)+`,
		    'USD', '10Y', 1.1)`,
		`INSERT INTO fund_characteristics (silver_source_id, fund_ref, as_of, provider, field, label, value) VALUES
		    ('ref', 'symbol:BNDF', `+itoa(at-86399)+`, 'ishares', 'effective_duration', '', -1.5)`)
	base, err := readYieldBase(ctx, db, at, &yieldShared{})
	if err != nil {
		t.Fatal(err)
	}
	sawFunds := false
	for _, ccy := range materializeCurrencies {
		in, err := base.inputs(ctx, db, ccy)
		if err != nil {
			t.Fatal(err)
		}
		sawFunds = sawFunds || in.Funds["symbol:BNDF"] != nil && in.Funds["symbol:BNDF"].Risk != nil
		shared := func() string { return dump(base.in) + dump(base.funds) }
		before := shared()
		yield.Compute(in)
		if after := shared(); after != before {
			t.Errorf("%s: yield.Compute changed the inputs the currencies share", ccy)
		}
		own, err := YieldInputs(ctx, db, at, ccy)
		if err != nil {
			t.Fatal(err)
		}
		for name, pair := range map[string][2]any{"funds": {in.Funds, own.Funds}, "lines": {in.Lines, own.Lines}} {
			a, err := json.Marshal(pair[0])
			if err != nil {
				t.Fatal(err)
			}
			b, err := json.Marshal(pair[1])
			if err != nil {
				t.Fatal(err)
			}
			if string(a) != string(b) {
				t.Errorf("%s %s: shared read %s, a read of its own %s", ccy, name, a, b)
			}
		}
	}
	if !sawFunds {
		t.Fatal("the fixture holds no fund with a filed sensitivity")
	}
}

// A later currency that holds a fund no earlier one did reads that fund
// alone: the funds already read keep their figures, and the lines it
// picks are what a read of its own picks.
func TestALaterCurrencyReadsOnlyTheFundsItAdds(t *testing.T) {
	t.Parallel()
	db, ctx, at := seedYieldBookFX(t)
	base, err := readYieldBase(ctx, db, at, &yieldShared{})
	if err != nil {
		t.Fatal(err)
	}
	full, err := base.inputs(ctx, db, "USD")
	if err != nil {
		t.Fatal(err)
	}
	keys := heldFunds(full)
	if len(keys) < 2 {
		t.Fatalf("the fixture holds %d funds, want two or more", len(keys))
	}
	fresh := &yieldBase{in: base.in, funds: map[string]*fundRead{}}
	first := *full
	first.Funds = map[string]*yield.Fund{}
	first.Holdings = nil
	for _, h := range full.Holdings {
		if h.SecurityID != keys[1] {
			first.Holdings = append(first.Holdings, h)
		}
	}
	if err := fresh.readFunds(ctx, db, &first); err != nil {
		t.Fatal(err)
	}
	if fresh.funds[keys[1]] != nil {
		t.Fatalf("%s read though no holding holds it", keys[1])
	}
	kept := fresh.funds[keys[0]].fund
	later := *full
	later.Funds = map[string]*yield.Fund{}
	if err := fresh.readFunds(ctx, db, &later); err != nil {
		t.Fatal(err)
	}
	if fresh.funds[keys[1]] == nil || later.Funds[keys[0]] != kept {
		t.Errorf("the later read: %s read %v, %s kept %v", keys[1], fresh.funds[keys[1]] != nil, keys[0],
			later.Funds[keys[0]] == kept)
	}
	a, _ := json.Marshal(later.Lines)
	b, _ := json.Marshal(full.Lines)
	if string(a) != string(b) {
		t.Errorf("lines %s, a read of its own %s", a, b)
	}
}
