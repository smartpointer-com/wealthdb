package silvertest

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/silver"
)

// DumpCanonical writes everything conn yields as text that two runs of
// the same adapter over the same silver reproduce byte for byte: the
// Status, the change window since since, and, where the window has
// changes, every snapshot record, every transaction and every realized
// lot. Each record is one JSON line with its keys sorted, payloads
// included, so a payload compares as parsed JSON. The lines are grouped
// by record type under a "== name count" header, and sorted within the
// group, so the order of batches and of records in a batch does not
// show. An empty group is left out.
//
// It reads what a load reads and nothing more: Snapshots, Transactions
// and RealizedLots are called only when the window has changes.
func DumpCanonical(ctx context.Context, conn silver.Connection, since int64) ([]byte, error) {
	var out bytes.Buffer
	status, err := conn.Status(ctx)
	if err != nil {
		return nil, fmt.Errorf("conn.Status: %w", err)
	}
	w, err := conn.ChangeWindow(ctx, since)
	if err != nil {
		return nil, fmt.Errorf("conn.ChangeWindow: %w", err)
	}
	if err := writeGroup(&out, "status", []canonical.Status{status}); err != nil {
		return nil, err
	}
	if err := writeGroup(&out, "window", []canonical.Window{w}); err != nil {
		return nil, err
	}
	if !w.HasChanges {
		return out.Bytes(), nil
	}

	snaps, err := conn.Snapshots(ctx, w)
	if err != nil {
		return nil, fmt.Errorf("conn.Snapshots: %w", err)
	}
	var all canonical.SnapshotBatch
	av := reflect.ValueOf(&all).Elem()
	for more := true; more; {
		var b canonical.SnapshotBatch
		b, more, err = snaps.Next(ctx)
		if err != nil {
			snaps.Close()
			return nil, fmt.Errorf("conn.Snapshots: next: %w", err)
		}
		bv := reflect.ValueOf(b)
		for i := range av.NumField() {
			av.Field(i).Set(reflect.AppendSlice(av.Field(i), bv.Field(i)))
		}
	}
	if err := snaps.Close(); err != nil {
		return nil, fmt.Errorf("conn.Snapshots: close: %w", err)
	}
	for i := range av.NumField() {
		if err := writeGroup(&out, snakeCase(av.Type().Field(i).Name), av.Field(i).Interface()); err != nil {
			return nil, err
		}
	}

	txs, err := conn.Transactions(ctx, w)
	if err != nil {
		return nil, fmt.Errorf("conn.Transactions: %w", err)
	}
	var tx []canonical.TransactionChange
	for more := true; more; {
		var b canonical.TransactionBatch
		b, more, err = txs.Next(ctx)
		if err != nil {
			txs.Close()
			return nil, fmt.Errorf("conn.Transactions: next: %w", err)
		}
		tx = append(tx, b.Transactions...)
	}
	if err := txs.Close(); err != nil {
		return nil, fmt.Errorf("conn.Transactions: close: %w", err)
	}
	if err := writeGroup(&out, "transactions", tx); err != nil {
		return nil, err
	}

	if r, ok := conn.(silver.RealizedLotReader); ok {
		lots, err := r.RealizedLots(ctx)
		if err != nil {
			return nil, fmt.Errorf("conn.RealizedLots: %w", err)
		}
		if err := writeGroup(&out, "realized_lots", lots); err != nil {
			return nil, err
		}
	}
	return out.Bytes(), nil
}

// writeGroup writes the records of the slice records under one header,
// one sorted line each.
func writeGroup(out *bytes.Buffer, name string, records any) error {
	v := reflect.ValueOf(records)
	if v.Len() == 0 {
		return nil
	}
	lines := make([]string, v.Len())
	for i := range lines {
		line, err := canonicalJSON(v.Index(i).Interface())
		if err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
		lines[i] = line
	}
	slices.Sort(lines)
	fmt.Fprintf(out, "== %s %d\n", name, len(lines))
	for _, l := range lines {
		out.WriteString(l)
		out.WriteByte('\n')
	}
	return nil
}

// canonicalJSON is record as JSON with every object's keys sorted: it
// is encoded, decoded into maps with its numbers kept verbatim, and
// encoded again.
func canonicalJSON(record any) (string, error) {
	raw, err := json.Marshal(record)
	if err != nil {
		return "", err
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var tree any
	if err := dec.Decode(&tree); err != nil {
		return "", err
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(tree); err != nil {
		return "", err
	}
	return strings.TrimSuffix(buf.String(), "\n"), nil
}

// snakeCase is a Go field name as a lower-case group name:
// "PositionLots" is "position_lots".
func snakeCase(s string) string {
	var b strings.Builder
	for i, r := range s {
		if 'A' <= r && r <= 'Z' {
			if i > 0 {
				b.WriteByte('_')
			}
			r += 'a' - 'A'
		}
		b.WriteRune(r)
	}
	return b.String()
}

// updateCanonical is the -update-canonical flag of every test binary
// that imports this package: with it set, Golden rewrites its file. The
// name is its own, so a tree-wide run rewrites no other golden file.
var updateCanonical = flag.Bool("update-canonical", false, "rewrite the canonical golden files")

// Golden compares conn's canonical dump since change number 0 with the
// file golden, a path relative to the test's package. Under
// -update-canonical it writes the file instead. A mismatch lists the
// lines found on one side only, a few of each, and the command that
// rewrites the file.
func Golden(t testing.TB, conn silver.Connection, golden string) {
	t.Helper()
	compareGolden(t, conn, golden, *updateCanonical)
}

// compareGolden is Golden with the flag's value passed in.
func compareGolden(t testing.TB, conn silver.Connection, golden string, update bool) {
	t.Helper()
	got, err := DumpCanonical(context.Background(), conn, 0)
	if err != nil {
		t.Fatalf("DumpCanonical: %v", err)
	}
	if update {
		if err := os.MkdirAll(filepath.Dir(golden), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(golden, got, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatalf("%v (run %s to write it)", err, rerun(t))
	}
	if bytes.Equal(got, want) {
		return
	}
	gotOnly, wantOnly := lineDiff(string(got), string(want))
	t.Errorf("canonical dump differs from %s (run %s to rewrite it)\nonly in the dump:\n%s\nonly in the golden file:\n%s",
		golden, rerun(t), firstLines(gotOnly), firstLines(wantOnly))
}

// rerun is the wealthdb-test command that rewrites the golden files of
// t's top-level test: the test's package directory relative to the
// module root, which is the directory a test runs in.
func rerun(t testing.TB) string {
	pkg := "."
	if wd, err := os.Getwd(); err == nil {
		if root := moduleRoot(wd); root != "" {
			if rel, err := filepath.Rel(root, wd); err == nil {
				pkg = "./" + filepath.ToSlash(rel)
			}
		}
	}
	name, _, _ := strings.Cut(t.Name(), "/")
	return fmt.Sprintf("wealthdb/wealthdb-test %s -run '^%s$' -update-canonical", pkg, name)
}

// moduleRoot is the nearest directory at or above dir that holds a
// go.mod, or "" when there is none.
func moduleRoot(dir string) string {
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return ""
		}
		dir = parent
	}
}

// lineDiff is the lines of a that b lacks and those of b that a lacks,
// counting a repeated line as often as it repeats.
func lineDiff(a, b string) (aOnly, bOnly []string) {
	count := map[string]int{}
	for _, l := range strings.Split(b, "\n") {
		count[l]++
	}
	for _, l := range strings.Split(a, "\n") {
		if count[l] > 0 {
			count[l]--
		} else {
			aOnly = append(aOnly, l)
		}
	}
	for _, l := range strings.Split(b, "\n") {
		if count[l] > 0 {
			count[l]--
			bOnly = append(bOnly, l)
		}
	}
	return aOnly, bOnly
}

// firstLines is up to five of lines, one per row, with a count of the
// rest.
func firstLines(lines []string) string {
	const limit = 5
	if len(lines) == 0 {
		return "  (none)"
	}
	var b strings.Builder
	for i, l := range lines {
		if i == limit {
			fmt.Fprintf(&b, "  … %d more\n", len(lines)-limit)
			break
		}
		b.WriteString("  " + l + "\n")
	}
	return strings.TrimSuffix(b.String(), "\n")
}
