package canonical

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

// ReferenceRows counts one record of each reference slice, every slice
// of a batch from FundCompositions on, and none of the slices before. An
// entry of Reference counts as its rows.
func TestReferenceRowsCountsEveryReferenceSlice(t *testing.T) {
	t.Parallel()
	var b SnapshotBatch
	v := reflect.ValueOf(&b).Elem()
	reference, want := false, 0
	for i := range v.NumField() {
		f := v.Field(i)
		reference = reference || v.Type().Field(i).Name == "FundCompositions"
		if f.Kind() != reflect.Slice {
			continue
		}
		f.Set(reflect.MakeSlice(f.Type(), 1, 1))
		if reference {
			want++
		}
	}
	// A reference table's entry counts its rows.
	b.Reference[0].Rows = [][]any{{}}
	if got := b.ReferenceRows(); want == 0 || got != want {
		t.Errorf("ReferenceRows = %d, want %d", got, want)
	}
}

// TestSetSourceReachesEverySlice: a batch's SetSource sets the source id
// on a record of every slice the batch has. A slice added to a batch
// without its line in SetSource fails here, rather than loading rows
// with no source.
func TestSetSourceReachesEverySlice(t *testing.T) {
	t.Parallel()
	var snap SnapshotBatch
	var txs TransactionBatch
	for _, c := range []struct {
		batch any
		set   func()
	}{
		{&snap, func() { snap.SetSource("src") }},
		{&txs, func() { txs.SetSource("src") }},
	} {
		v := reflect.ValueOf(c.batch).Elem()
		var fields []int
		for i := range v.NumField() {
			if f := v.Field(i); f.Kind() == reflect.Slice {
				f.Set(reflect.MakeSlice(f.Type(), 1, 1))
				fields = append(fields, i)
			}
		}
		if len(fields) == 0 {
			t.Fatalf("%s has no slice", v.Type().Name())
		}
		c.set()
		for _, i := range fields {
			id := v.Field(i).Index(0).FieldByName("SilverSourceID")
			if !id.IsValid() || id.String() != "src" {
				t.Errorf("%s.%s: SetSource leaves SilverSourceID %v, want src", v.Type().Name(), v.Type().Field(i).Name, id)
			}
		}
	}
}

// An action is the empty one or a name the lot engine reads; a JSON
// record leaves the empty one out.
func TestTxActionValid(t *testing.T) {
	for _, a := range []TxAction{"", TxActionIgnore, TxActionThirdCurrencyFee, TxActionCashOnly} {
		if !a.Valid() {
			t.Errorf("%q is not valid", a)
		}
	}
	for _, a := range []TxAction{"Ignore", "buy", "none"} {
		if a.Valid() {
			t.Errorf("%q is valid", a)
		}
	}
	b, err := json.Marshal(TransactionChange{})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), `"Action"`) {
		t.Errorf("an empty action is written: %s", b)
	}
}
