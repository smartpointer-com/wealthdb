package silver

import (
	"slices"
	"sort"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
)

// Holding is one dated state of one holding: what the source says the
// holding is from At on, until the holding's next state.
type Holding struct {
	// Key names the holding. Its states replace one another.
	Key string
	// Position is the position key the holding is part of, the key
	// Removals reads; "" is Key. Several holdings can make one position
	// (the lots of a company).
	Position string
	// At is the state's date, unix seconds.
	At int64
	// Open says the holding is held from At on.
	Open bool
	// Row holds the adapter's own columns.
	Row any
}

func (h Holding) position() string {
	if h.Position != "" {
		return h.Position
	}
	return h.Key
}

// Book forward-fills a source that states events per holding into the
// complete portfolios gold reads. Gold reads a snapshot as the account's
// whole state on its date, so every snapshot time carries every holding
// still held then, each at its latest state.
//
// The snapshot times are the state dates, the Extra times and the
// removal times inside the window. At each time, a holding's latest
// state is its last state in States order dated on or before it. The
// holding is held there when that state is open and the portal had not
// stopped listing its position by then (Removals). Emit builds the
// positions and instruments of the held holdings, and a batch with a
// position gets the Account record.
//
// Where the last position leaves, a Book that sets Closure emits the
// exit-day zero snapshot (ClosureMarkerBatch) instead of an empty
// batch. Gold cannot see an empty batch and would carry the last marks
// forward as phantom value.
type Book struct {
	// States are every state of every holding, read once. A holding's
	// states are in the order they replace one another, oldest first.
	// The held holdings reach Emit in the order of their latest states.
	States []Holding
	// Extra are snapshot times that need not be a state's date: the
	// days a book value or a carried value changes.
	Extra []int64
	// Removals end a position at the first complete dump that stopped
	// listing it. Each removal time is a snapshot time.
	Removals Removals
	// Account is the book's one account as of t. Every batch with a
	// position carries it, and so does the closure marker. A nil
	// Account adds none, and Emit states the accounts itself.
	Account func(t int64) canonical.AccountChange
	// Emit builds the batch at t from the holdings held at t: their
	// positions, lots and instruments.
	Emit func(t int64, held []Holding) (canonical.SnapshotBatch, error)
	// StampValuedAt values each position at the latest date among the
	// latest states of its holdings (canonical.PositionChange.ValuedAt).
	// Without it, Emit states ValuedAt or leaves it unset.
	StampValuedAt bool
	// Closure emits the closure marker where the book empties.
	Closure bool
}

// Batches are the book's snapshot batches inside w, oldest first.
//
// The walk tracks the held→empty transition within w, so the closure
// marker depends on w spanning the whole history: a window that starts
// between the last held date and the exit would miss the transition.
func (b Book) Batches(w canonical.Window) ([]canonical.SnapshotBatch, error) {
	times := b.times(w)
	// byAt is the states in date order: the walk takes each state in
	// once its date is reached.
	byAt := make([]int, len(b.States))
	for i := range byAt {
		byAt[i] = i
	}
	sort.SliceStable(byAt, func(i, j int) bool { return b.States[byAt[i]].At < b.States[byAt[j]].At })

	out := make([]canonical.SnapshotBatch, 0, len(times))
	latest := map[string]int{} // holding key → index of its latest state
	next := 0
	var lastHeld canonical.SnapshotBatch
	prevHeld := false
	for _, t := range times {
		for ; next < len(byAt) && b.States[byAt[next]].At <= t; next++ {
			i := byAt[next]
			if j, ok := latest[b.States[i].Key]; !ok || i > j {
				latest[b.States[i].Key] = i
			}
		}
		held := b.held(latest, t)
		batch, err := b.Emit(t, held)
		if err != nil {
			return nil, err
		}
		holds := len(batch.Positions) > 0
		switch {
		case holds:
			if b.StampValuedAt {
				stampValuedAt(batch.Positions, held)
			}
			if b.Account != nil {
				batch.Accounts = append(batch.Accounts, b.Account(t))
			}
			lastHeld = batch
		case prevHeld && b.Closure:
			batch = ClosureMarkerBatch(lastHeld, t, b.Account(t))
		}
		prevHeld = holds
		out = append(out, batch)
	}
	return out, nil
}

// times are the distinct state dates, Extra times and removal times
// inside w, ascending.
func (b Book) times(w canonical.Window) []int64 {
	var out []int64
	add := func(t int64) {
		if t >= w.Start && t <= w.End {
			out = append(out, t)
		}
	}
	for _, h := range b.States {
		add(h.At)
	}
	for _, t := range b.Extra {
		add(t)
	}
	for _, t := range b.Removals.Times() {
		add(t)
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// held are the holdings held at t, in the order of their latest states.
func (b Book) held(latest map[string]int, t int64) []Holding {
	idx := make([]int, 0, len(latest))
	for _, i := range latest {
		if h := b.States[i]; h.Open && !b.Removals.Removed(h.position(), t) {
			idx = append(idx, i)
		}
	}
	slices.Sort(idx)
	out := make([]Holding, len(idx))
	for k, i := range idx {
		out[k] = b.States[i]
	}
	return out
}

// stampValuedAt values each position at the latest date among the held
// holdings that make it.
func stampValuedAt(positions []canonical.PositionChange, held []Holding) {
	at := make(map[string]int64, len(held))
	for _, h := range held {
		if a, ok := at[h.position()]; !ok || h.At > a {
			at[h.position()] = h.At
		}
	}
	for i := range positions {
		if a, ok := at[positions[i].PositionKey]; ok {
			positions[i].ValuedAt = a
		}
	}
}
