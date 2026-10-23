package loader

import (
	"reflect"
	"testing"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
)

// TestStampReachesEverySlice: a batch's stamp sets the source id on a
// record of every slice the batch has. A slice added to a batch without
// its stamp loop fails here, rather than loading rows with no source.
func TestStampReachesEverySlice(t *testing.T) {
	t.Parallel()
	var snap canonical.SnapshotBatch
	var txs canonical.TransactionBatch
	for _, c := range []struct {
		batch any
		stamp func()
	}{
		{&snap, func() { stampSnapshotBatch(&snap, "src") }},
		{&txs, func() { stampTransactionBatch(&txs, "src") }},
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
		c.stamp()
		for _, i := range fields {
			id := v.Field(i).Index(0).FieldByName("SilverSourceID")
			if !id.IsValid() || id.String() != "src" {
				t.Errorf("%s.%s: the stamp leaves SilverSourceID %v, want src", v.Type().Name(), v.Type().Field(i).Name, id)
			}
		}
	}
}
