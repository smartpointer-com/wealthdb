package lots

import "slices"

// book is one key's open lots and their running totals. Its structure
// depends on the method:
//
//   - fifo and lifo keep the dated lots ordered by acquisition date,
//     then by the order they were opened, and relieve from the front or
//     the back. An acquisition lands at the back in O(1); a lot that
//     arrives with an older date is inserted in place, and lots that
//     arrive together (a move, a restored blip) merge in one pass
//     (addMany). The undated lots (seeds, receipts nothing explains)
//     sort before every dated one, in a queue of their own in the order
//     they opened.
//   - hifo and lofo keep a binary heap on unit cost; a lot without a
//     cost sorts last. A partly relieved lot keeps its unit cost, so it
//     stays where it is.
//   - average keeps two pools, the costed and the uncosted quantity.
//
// The totals let an observation read the book in O(1). seeds stacks the
// seed lots of unknown cost in the order they opened, so an implied
// disposal finds the latest without a scan; a closed one leaves the
// stack when it reaches the top.
type book struct {
	method Method
	ord    []int32
	head   int
	und    []int32
	uhead  int
	heap   []int32
	pool   [2]int32
	seeds  []int32

	qty, known, unknown float64
	n                   int32

	pending []pendingPart
	flight  int32
}

func (b *book) init(m Method) {
	b.method = m
	b.pool = [2]int32{-1, -1}
}

// reset empties the book after every lot has been closed.
func (b *book) reset() {
	b.ord, b.head, b.heap = b.ord[:0], 0, b.heap[:0]
	b.und, b.uhead = b.und[:0], 0
	b.pool = [2]int32{-1, -1}
	b.seeds = b.seeds[:0]
	b.qty, b.known, b.unknown, b.n = 0, 0, 0, 0
	b.pending = b.pending[:0]
}

// lots lists the open lots: in date order for fifo and lifo, so adding
// them back in that order appends.
func (b *book) lots() []int32 {
	switch b.method {
	case FIFO, LIFO:
		return append(slices.Clone(b.und[b.uhead:]), b.ord[b.head:]...)
	case HIFO, LOFO:
		return slices.Clone(b.heap)
	}
	var out []int32
	for _, li := range b.pool {
		if li >= 0 {
			out = append(out, li)
		}
	}
	return out
}

// count adds lot li to the book's running totals.
func (b *book) count(e *engine, li int32) {
	l := &e.res.Lots[li]
	b.qty += l.rem
	if l.CostKnown {
		b.known += l.remCost
	} else {
		b.unknown += l.rem
	}
	b.n++
	if l.seed && !l.CostKnown && b.method != Average {
		b.seeds = append(b.seeds, li)
	}
}

// insertOrdered puts li into the ordered list q (live from head), at
// the back in O(1) when it is the latest, else in place.
func (e *engine) insertOrdered(q []int32, head int, li int32) []int32 {
	live := q[head:]
	if len(live) == 0 || !e.dateBefore(li, live[len(live)-1]) {
		return append(q, li)
	}
	i, _ := slices.BinarySearchFunc(live, li, func(x, t int32) int {
		if e.dateBefore(t, x) {
			return 1
		}
		return -1
	})
	return slices.Insert(q, head+i, li)
}

// add puts lot li into the book.
func (b *book) add(e *engine, li int32) {
	b.count(e, li)
	switch b.method {
	case FIFO, LIFO:
		if e.res.Lots[li].AcqDay == NoDay {
			b.und = e.insertOrdered(b.und, b.uhead, li)
			return
		}
		b.ord = e.insertOrdered(b.ord, b.head, li)
	case HIFO, LOFO:
		b.heap = append(b.heap, li)
		b.up(e, len(b.heap)-1)
	case Average:
		if e.res.Lots[li].CostKnown {
			b.pool[0] = li
		} else {
			b.pool[1] = li
		}
	}
}

// addMany puts lots that arrive together into the book: a fifo or lifo
// book merges the dated ones, in date order, with its own in one pass
// rather than inserting each in place. An average book is not asked:
// its lots merge into the pools (engine.place).
func (b *book) addMany(e *engine, lis []int32) {
	if b.method != FIFO && b.method != LIFO {
		for _, li := range lis {
			b.add(e, li)
		}
		return
	}
	var dated []int32
	for _, li := range lis {
		if e.res.Lots[li].AcqDay == NoDay {
			b.add(e, li)
			continue
		}
		b.count(e, li)
		dated = append(dated, li)
	}
	slices.SortFunc(dated, func(x, y int32) int {
		switch {
		case e.dateBefore(x, y):
			return -1
		case e.dateBefore(y, x):
			return 1
		}
		return 0
	})
	live := b.ord[b.head:]
	if len(dated) == 0 || len(live) == 0 || !e.dateBefore(dated[0], live[len(live)-1]) {
		b.ord = append(b.ord, dated...)
		return
	}
	out := make([]int32, 0, len(live)+len(dated))
	i, j := 0, 0
	for i < len(live) && j < len(dated) {
		if e.dateBefore(dated[j], live[i]) {
			out = append(out, dated[j])
			j++
		} else {
			out = append(out, live[i])
			i++
		}
	}
	out = append(append(out, live[i:]...), dated[j:]...)
	b.ord, b.head = out, 0
}

// lastSeed is the latest open seed lot of unknown cost, -1 when none.
// An average book's uncosted pool stands for its seeds.
func (b *book) lastSeed(e *engine) int32 {
	if b.method == Average {
		return b.pool[1]
	}
	for n := len(b.seeds); n > 0; n = len(b.seeds) {
		if li := b.seeds[n-1]; e.res.Lots[li].ClosedAt == 0 && e.res.Lots[li].rem > 0 {
			return li
		}
		b.seeds = b.seeds[:n-1]
	}
	return -1
}

// next is the lot to relieve first, -1 when the book is empty.
func (b *book) next() int32 {
	switch b.method {
	case FIFO:
		if b.uhead < len(b.und) {
			return b.und[b.uhead]
		}
		if b.head < len(b.ord) {
			return b.ord[b.head]
		}
	case LIFO:
		if b.head < len(b.ord) {
			return b.ord[len(b.ord)-1]
		}
		if b.uhead < len(b.und) {
			return b.und[len(b.und)-1]
		}
	case HIFO, LOFO:
		if len(b.heap) > 0 {
			return b.heap[0]
		}
	}
	return -1
}

// remove takes a used-up lot out of the book. Relief uses up the lot
// next returns, which fifo, lifo and the heap remove in O(1) or
// O(log n); a lot elsewhere (a seed an implied disposal takes first) is
// found by a scan. Closing a whole book goes through drain instead.
func (b *book) remove(e *engine, li int32) {
	b.n--
	if b.n == 0 {
		b.qty, b.known, b.unknown = 0, 0, 0
	}
	switch b.method {
	case FIFO, LIFO:
		if e.res.Lots[li].AcqDay == NoDay {
			b.und, b.uhead = removeOrdered(b.und, b.uhead, li)
		} else {
			b.ord, b.head = removeOrdered(b.ord, b.head, li)
		}
	case HIFO, LOFO:
		i := 0
		if len(b.heap) == 0 || b.heap[0] != li {
			i = slices.Index(b.heap, li)
			if i < 0 {
				return
			}
		}
		last := len(b.heap) - 1
		b.heap[i] = b.heap[last]
		b.heap = b.heap[:last]
		if i < last {
			b.down(e, i)
			b.up(e, i)
		}
	case Average:
		for i, p := range b.pool {
			if p == li {
				b.pool[i] = -1
			}
		}
	}
}

// removeOrdered takes li out of the ordered list q (live from head): in
// O(1) from either end, else by a scan. It compacts a list whose dead
// front outgrows its live part.
func removeOrdered(q []int32, head int, li int32) ([]int32, int) {
	switch {
	case head < len(q) && q[head] == li:
		head++
		if head > 64 && head*2 > len(q) {
			q, head = append(q[:0], q[head:]...), 0
		}
	case len(q) > head && q[len(q)-1] == li:
		q = q[:len(q)-1]
	default:
		if i := slices.Index(q[head:], li); i >= 0 {
			q = slices.Delete(q, head+i, head+i+1)
		}
	}
	if head == len(q) {
		q, head = q[:0], 0
	}
	return q, head
}

// dateBefore orders lots for fifo and lifo: by acquisition date, then
// by the order they were opened. An unknown date sorts first: a seed
// stands for holdings older than any trade the history shows, and a
// receipt nothing dates sorts with it.
func (e *engine) dateBefore(a, b int32) bool {
	la, lb := &e.res.Lots[a], &e.res.Lots[b]
	if la.AcqDay != lb.AcqDay {
		return la.AcqDay < lb.AcqDay
	}
	return la.seq < lb.seq
}

// costBefore orders the heap: a costed lot before an uncosted one, then
// by unit cost (descending for hifo, ascending for lofo), then by date.
func (b *book) costBefore(e *engine, x, y int32) bool {
	lx, ly := &e.res.Lots[x], &e.res.Lots[y]
	if lx.CostKnown != ly.CostKnown {
		return lx.CostKnown
	}
	if lx.CostKnown {
		ux, uy := lx.Cost/lx.Qty, ly.Cost/ly.Qty
		if ux != uy {
			if b.method == HIFO {
				return ux > uy
			}
			return ux < uy
		}
	}
	return e.dateBefore(x, y)
}

func (b *book) up(e *engine, i int) {
	for i > 0 {
		p := (i - 1) / 2
		if !b.costBefore(e, b.heap[i], b.heap[p]) {
			return
		}
		b.heap[i], b.heap[p] = b.heap[p], b.heap[i]
		i = p
	}
}

func (b *book) down(e *engine, i int) {
	n := len(b.heap)
	for {
		l, best := 2*i+1, i
		if l < n && b.costBefore(e, b.heap[l], b.heap[best]) {
			best = l
		}
		if r := l + 1; r < n && b.costBefore(e, b.heap[r], b.heap[best]) {
			best = r
		}
		if best == i {
			return
		}
		b.heap[i], b.heap[best] = b.heap[best], b.heap[i]
		i = best
	}
}
