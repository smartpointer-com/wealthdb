package canonical

import (
	"reflect"
	"testing"
)

// ReferenceRows counts one record of each reference slice, every slice
// of a batch from FundCompositions on, and none of the slices before.
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
	if got := b.ReferenceRows(); want == 0 || got != want {
		t.Errorf("ReferenceRows = %d, want %d", got, want)
	}
}
