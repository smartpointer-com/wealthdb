package silver

import (
	"context"
	"database/sql"
	"path/filepath"
	"reflect"
	"testing"
)

func TestRemovalsAnswerPerHoldingAndTime(t *testing.T) {
	r := Removals{"a": 2000, "b": 3000, "c": 2000}
	if r.Removed("a", 1999) {
		t.Error("a is still listed before its removal")
	}
	if !r.Removed("a", 2000) || !r.Removed("a", 5000) {
		t.Error("a is out from its removal on")
	}
	if r.Removed("zzz", 5000) {
		t.Error("a holding never removed stays")
	}
	if got := r.Times(); !reflect.DeepEqual(got, []int64{2000, 3000}) {
		t.Errorf("Times = %v, want the distinct removal times ascending", got)
	}
	if got := r.Reach(2500); got != 3000 {
		t.Errorf("Reach(2500) = %d, want the later removal", got)
	}
	if got := r.Reach(4000); got != 4000 {
		t.Errorf("Reach(4000) = %d, want the later instant", got)
	}
	if got := (Removals{}).Reach(-1); got != -1 {
		t.Errorf("Reach of none = %d, want the instant unchanged", got)
	}
	if got := (Removals{}).Times(); len(got) != 0 {
		t.Errorf("Times of none = %v, want none", got)
	}
}

func TestQueryRemovalsScansKeyAndTime(t *testing.T) {
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	if _, err := db.ExecContext(ctx, `CREATE TABLE r (k TEXT, at INTEGER);
        INSERT INTO r VALUES ('x', 100), ('y', 200);`); err != nil {
		t.Fatal(err)
	}
	got, err := QueryRemovals(ctx, db, "test", `SELECT k, at FROM r WHERE at >= ?`, 150)
	if err != nil {
		t.Fatal(err)
	}
	want := Removals{"y": 200}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("QueryRemovals = %v, want %v", got, want)
	}
	if _, err := QueryRemovals(ctx, db, "test", `SELECT nope FROM r`); err == nil {
		t.Error("a failing query must surface its error")
	}
}
