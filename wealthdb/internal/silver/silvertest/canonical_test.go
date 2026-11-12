package silvertest

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/silver"
)

// fakeConn yields fixed batches and records which streams were read.
type fakeConn struct {
	window    canonical.Window
	snapshots []canonical.SnapshotBatch
	txs       []canonical.TransactionBatch
	calls     []string
}

func (c *fakeConn) Close() error { return nil }

func (c *fakeConn) Status(context.Context) (canonical.Status, error) {
	return canonical.UnknownStatus(), nil
}

func (c *fakeConn) ChangeWindow(context.Context, int64) (canonical.Window, error) {
	return c.window, nil
}

func (c *fakeConn) Snapshots(context.Context, canonical.Window) (silver.SnapshotStream, error) {
	c.calls = append(c.calls, "snapshots")
	return silver.NewSnapshotStream(c.snapshots), nil
}

func (c *fakeConn) Transactions(context.Context, canonical.Window) (silver.TransactionStream, error) {
	c.calls = append(c.calls, "transactions")
	return &txStream{batches: c.txs}, nil
}

type txStream struct{ batches []canonical.TransactionBatch }

func (s *txStream) Next(context.Context) (canonical.TransactionBatch, bool, error) {
	if len(s.batches) == 0 {
		return canonical.TransactionBatch{}, false, nil
	}
	b := s.batches[0]
	s.batches = s.batches[1:]
	return b, len(s.batches) > 0, nil
}

func (s *txStream) Close() error { return nil }

// fakeLotConn is a fakeConn that also offers realized lots.
type fakeLotConn struct {
	fakeConn
	lots []canonical.RealizedLotChange
}

func (c *fakeLotConn) RealizedLots(context.Context) ([]canonical.RealizedLotChange, error) {
	c.calls = append(c.calls, "realized")
	return c.lots, nil
}

func dump(t *testing.T, conn silver.Connection) string {
	t.Helper()
	out, err := DumpCanonical(context.Background(), conn, 0)
	if err != nil {
		t.Fatal(err)
	}
	return string(out)
}

// The dump groups records by type, sorts them within the group whatever
// batch they came in, and re-encodes payloads with their keys sorted.
func TestDumpCanonicalSortsAndNormalizes(t *testing.T) {
	conn := &fakeLotConn{fakeConn: fakeConn{
		window: canonical.Window{Start: 1, End: 2, NewChangeNumber: 2, HasChanges: true},
		snapshots: []canonical.SnapshotBatch{
			{Accounts: []canonical.AccountChange{{AccountExternalID: "b", Payload: json.RawMessage(`{"z": 1, "a": 2}`)}}},
			{Accounts: []canonical.AccountChange{{AccountExternalID: "a"}}},
		},
		txs: []canonical.TransactionBatch{
			{Transactions: []canonical.TransactionChange{{TransactionExternalID: "t2"}}},
			{Transactions: []canonical.TransactionChange{{TransactionExternalID: "t1"}}},
		},
	}, lots: []canonical.RealizedLotChange{{RealizedLotExternalID: "r1"}}}
	got := dump(t, conn)

	var headers []string
	for _, l := range strings.Split(got, "\n") {
		if strings.HasPrefix(l, "== ") {
			headers = append(headers, l)
		}
	}
	if want := "== status 1|== window 1|== accounts 2|== transactions 2|== realized_lots 1"; strings.Join(headers, "|") != want {
		t.Errorf("headers = %q, want %q", strings.Join(headers, "|"), want)
	}
	if a, b := strings.Index(got, `"AccountExternalID":"a"`), strings.Index(got, `"AccountExternalID":"b"`); a < 0 || b < a {
		t.Errorf("accounts not sorted:\n%s", got)
	}
	if t1, t2 := strings.Index(got, `"t1"`), strings.Index(got, `"t2"`); t1 < 0 || t2 < t1 {
		t.Errorf("transactions not sorted:\n%s", got)
	}
	if !strings.Contains(got, `"Payload":{"a":2,"z":1}`) {
		t.Errorf("payload keys not sorted:\n%s", got)
	}
	if again := dump(t, conn); again != got {
		t.Error("a second dump differs")
	}
}

// A window without changes reads nothing past the clocks, as a load
// would not.
func TestDumpCanonicalStopsAtAQuietWindow(t *testing.T) {
	conn := &fakeLotConn{lots: []canonical.RealizedLotChange{{RealizedLotExternalID: "r1"}}}
	got := dump(t, conn)
	if strings.Count(got, "== ") != 2 || len(conn.calls) != 0 {
		t.Errorf("dump = %q, calls = %v; want the status and window alone", got, conn.calls)
	}
}

func TestLineDiffCountsRepeats(t *testing.T) {
	aOnly, bOnly := lineDiff("x\nx\ny", "x\nz")
	if strings.Join(aOnly, ",") != "x,y" || strings.Join(bOnly, ",") != "z" {
		t.Errorf("lineDiff = %q, %q", aOnly, bOnly)
	}
}
