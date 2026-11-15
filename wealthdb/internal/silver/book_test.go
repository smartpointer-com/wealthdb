package silver

import (
	"errors"
	"math"
	"reflect"
	"testing"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
)

// everything is the window of a source's whole history.
var everything = canonical.Window{Start: math.MinInt64, End: math.MaxInt64, HasChanges: true, Full: true}

// heldKeys is an Emit that emits one position per held holding, keyed on
// its position, and records the held holdings' keys per time.
func heldKeys(seen map[int64][]string) func(int64, []Holding) (canonical.SnapshotBatch, error) {
	return func(t int64, held []Holding) (canonical.SnapshotBatch, error) {
		var b canonical.SnapshotBatch
		seen[t] = []string{}
		for _, h := range held {
			seen[t] = append(seen[t], h.Key)
			b.Positions = append(b.Positions, canonical.PositionChange{
				SnapshotAt: t, AccountExternalID: "acct", PositionKey: h.position()})
		}
		return b, nil
	}
}

func account(t int64) canonical.AccountChange {
	return CustodyAccount("acct", t)
}

func TestABookForwardFillsEachHoldingToItsLatestState(t *testing.T) {
	seen := map[int64][]string{}
	batches, err := Book{
		States: []Holding{
			{Key: "a", At: 100, Open: true},
			{Key: "b", At: 200, Open: true},
			{Key: "a", At: 300, Open: true},
			{Key: "b", At: 400, Open: false},
			{Key: "c", At: 500, Open: false}, // never held: a date, no holding
		},
		Extra:   []int64{250, 50, 250},
		Account: account,
		Emit:    heldKeys(seen),
	}.Batches(everything)
	if err != nil {
		t.Fatal(err)
	}
	if len(batches) != 7 {
		t.Fatalf("batches = %d, want one per state date and extra time", len(batches))
	}
	want := map[int64][]string{
		50:  {},
		100: {"a"},
		200: {"a", "b"},
		250: {"a", "b"},
		300: {"b", "a"}, // in the order of the latest states
		400: {"a"},
		500: {"a"},
	}
	if !reflect.DeepEqual(seen, want) {
		t.Errorf("held = %v, want %v", seen, want)
	}
	for _, b := range batches[1:] {
		if len(b.Accounts) != 1 || b.Accounts[0].FirstSeenAt != b.Positions[0].SnapshotAt {
			t.Errorf("batch at %d accounts = %+v, want the account as of its time", b.Positions[0].SnapshotAt, b.Accounts)
		}
	}
	if len(batches[0].Accounts) != 0 || len(batches[0].Positions) != 0 {
		t.Errorf("batch at 50 = %+v, want it empty", batches[0])
	}
}

// TestTheLatestStateIsTheLastInStatesOrder pins the pick for a source
// whose states replace one another in an order of their own (an event
// sequence): the last state dated on or before a time wins, even where
// an earlier one in that order carries a later date.
func TestTheLatestStateIsTheLastInStatesOrder(t *testing.T) {
	seen := map[int64][]string{}
	var open []bool
	_, err := Book{
		States: []Holding{
			{Key: "a", At: 100, Open: true},
			{Key: "a", At: 300, Open: true},
			{Key: "a", At: 200, Open: false},
		},
		Emit: func(t int64, held []Holding) (canonical.SnapshotBatch, error) {
			open = append(open, len(held) > 0)
			return heldKeys(seen)(t, held)
		},
	}.Batches(everything)
	if err != nil {
		t.Fatal(err)
	}
	if want := []bool{true, false, false}; !reflect.DeepEqual(open, want) {
		t.Errorf("held at 100/200/300 = %v, want %v", open, want)
	}
}

func TestARemovalEndsItsPositionAtTheDump(t *testing.T) {
	seen := map[int64][]string{}
	_, err := Book{
		States: []Holding{
			{Key: "lot1", Position: "p", At: 100, Open: true},
			{Key: "lot2", Position: "p", At: 100, Open: true},
			{Key: "q", At: 100, Open: true},
		},
		Removals: Removals{"p": 700},
		Account:  account,
		Emit:     heldKeys(seen),
	}.Batches(everything)
	if err != nil {
		t.Fatal(err)
	}
	want := map[int64][]string{100: {"lot1", "lot2", "q"}, 700: {"q"}}
	if !reflect.DeepEqual(seen, want) {
		t.Errorf("held = %v, want the removal time a snapshot time without p's lots: %v", seen, want)
	}
}

func TestABookThatEmptiesEmitsTheClosureMarker(t *testing.T) {
	states := []Holding{
		{Key: "a", At: 100, Open: true},
		{Key: "a", At: 200, Open: false},
		{Key: "b", At: 50, Open: false}, // before the first holding: no marker
		{Key: "b", At: 300, Open: false},
	}
	batches, err := Book{States: states, Account: account, Emit: heldKeys(map[int64][]string{}), Closure: true}.Batches(everything)
	if err != nil {
		t.Fatal(err)
	}
	if len(batches) != 4 {
		t.Fatalf("batches = %d, want 50/100/200/300", len(batches))
	}
	if len(batches[0].Positions) != 0 || len(batches[0].Accounts) != 0 {
		t.Errorf("batch at 50 = %+v, want no marker before the first holding", batches[0])
	}
	marker := batches[2]
	if len(marker.Positions) != 1 || marker.Positions[0].SnapshotAt != 200 ||
		!marker.Positions[0].MarketValue.IsZero() || string(marker.Positions[0].Payload) != string(ClosureMarkerPayload) {
		t.Errorf("batch at 200 = %+v, want a's position replayed at zero", marker)
	}
	if len(marker.Accounts) != 1 || marker.Accounts[0].FirstSeenAt != 200 {
		t.Errorf("marker accounts = %+v, want the account as of the exit", marker.Accounts)
	}
	if len(batches[3].Positions) != 0 {
		t.Errorf("batch at 300 = %+v, want the marker once", batches[3])
	}

	plain, err := Book{States: states, Emit: heldKeys(map[int64][]string{})}.Batches(everything)
	if err != nil {
		t.Fatal(err)
	}
	if b := plain[2]; len(b.Positions) != 0 || len(b.Accounts) != 0 {
		t.Errorf("batch at 200 without Closure = %+v, want it empty", b)
	}
}

func TestStampValuedAtValuesAPositionAtItsLatestState(t *testing.T) {
	emit := heldKeys(map[int64][]string{})
	states := []Holding{
		{Key: "lot1", Position: "p", At: 100, Open: true},
		{Key: "lot2", Position: "p", At: 300, Open: true},
		{Key: "q", At: 200, Open: true},
	}
	batches, err := Book{States: states, Emit: emit, StampValuedAt: true}.Batches(everything)
	if err != nil {
		t.Fatal(err)
	}
	got := map[int64]map[string]int64{}
	for _, b := range batches {
		for _, p := range b.Positions {
			if got[p.SnapshotAt] == nil {
				got[p.SnapshotAt] = map[string]int64{}
			}
			got[p.SnapshotAt][p.PositionKey] = p.ValuedAt
		}
	}
	want := map[int64]map[string]int64{
		100: {"p": 100},
		200: {"p": 100, "q": 200},
		300: {"p": 300, "q": 200},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ValuedAt = %v, want %v", got, want)
	}

	plain, err := Book{States: states, Emit: emit}.Batches(everything)
	if err != nil {
		t.Fatal(err)
	}
	for _, b := range plain {
		for _, p := range b.Positions {
			if p.ValuedAt != 0 {
				t.Errorf("ValuedAt without the stamp = %d, want it left to Emit", p.ValuedAt)
			}
		}
	}
}

func TestABookEmitsOnlyTheTimesInsideTheWindow(t *testing.T) {
	seen := map[int64][]string{}
	_, err := Book{
		States:   []Holding{{Key: "a", At: 100, Open: true}, {Key: "a", At: 900, Open: true}},
		Extra:    []int64{50, 400},
		Removals: Removals{"a": 950},
		Emit:     heldKeys(seen),
	}.Batches(canonical.Window{Start: 100, End: 800, HasChanges: true})
	if err != nil {
		t.Fatal(err)
	}
	if want := (map[int64][]string{100: {"a"}, 400: {"a"}}); !reflect.DeepEqual(seen, want) {
		t.Errorf("held = %v, want the times inside the window: %v", seen, want)
	}
}

func TestAnEmitErrorEndsTheWalk(t *testing.T) {
	boom := errors.New("boom")
	_, err := Book{
		States: []Holding{{Key: "a", At: 100, Open: true}},
		Emit:   func(int64, []Holding) (canonical.SnapshotBatch, error) { return canonical.SnapshotBatch{}, boom },
	}.Batches(everything)
	if !errors.Is(err, boom) {
		t.Errorf("err = %v, want Emit's", err)
	}
}
