package jsonedit

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
)

// documents are the texts every operation runs over: the demo
// household's config, a config that sets every member, and a few
// layouts the files in the wild use.
func documents(t *testing.T) map[string][]byte {
	t.Helper()
	out := map[string][]byte{
		"inline":  []byte(`{"gold_db": "/x/g.db", "silver_sources": [{"id": "a", "kind": "viac", "path": "/x/a.db"}], "web": {"enabled": false}}`),
		"tabs":    []byte("{\n\t\"default_currency\": \"EUR\",\n\t\"silver_sources\": [\n\t\t{\n\t\t\t\"id\": \"a\",\n\t\t\t\"kind\": \"viac\",\n\t\t\t\"path\": \"/x/a.db\"\n\t\t}\n\t],\n\t\"mcp\": {\n\t\t\"enabled\": true,\n\t\t\"auth\": \"token\"\n\t}\n}\n"),
		"aligned": []byte("{\n    \"gold_db\":          \"/x/g.db\",\n    \"default_currency\": \"USD\",\n    \"silver_sources\": [\n        {\"id\": \"a\", \"kind\": \"ubs\", \"subsources\": [\n            {\"kind\": \"ubs-web\", \"path\": \"/x/w.db\"}\n        ]}\n    ]\n}\n"),
		"empties": []byte("{\n  \"silver_sources\": [],\n  \"web\": {},\n  \"note\": \"a & b <c>\"\n}"),
	}
	for name, path := range map[string]string{
		"demo":      filepath.Join("testdata", "demo.cfg"),
		"every_key": filepath.Join("..", "config", "testdata", "every_key.cfg"),
	} {
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		out[name] = b
	}
	return out
}

// generic is the document's value, numbers kept as written.
func generic(t *testing.T, b []byte) any {
	t.Helper()
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		t.Fatalf("result is not JSON: %v\n%s", err, b)
	}
	return v
}

// normal is v as generic would read it back.
func normal(t *testing.T, v any) any {
	t.Helper()
	b, err := marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return generic(t, b)
}

// at walks a generic value along path.
func at(v any, path []any) any {
	for _, s := range path {
		switch k := s.(type) {
		case string:
			v = v.(map[string]any)[k]
		case int:
			v = v.([]any)[k]
		}
	}
	return v
}

// window is the span [lo, hi) of the original text an operation may
// change; everything outside it must come back byte for byte.
type window struct{ lo, hi int }

// checkUntouched fails unless before and after differ only inside w.
func checkUntouched(t *testing.T, before, after []byte, w window) {
	t.Helper()
	if !bytes.Equal(before[:w.lo], after[:min(w.lo, len(after))]) {
		t.Errorf("a byte before offset %d changed", w.lo)
	}
	tail := before[w.hi:]
	if len(after) < len(tail) || !bytes.Equal(tail, after[len(after)-len(tail):]) {
		t.Errorf("a byte after offset %d changed", w.hi)
	}
}

// itemWindow is what removing item i of the container n may change: the
// item and the separator between it and a neighbour.
func itemWindow(n *node, i int) window {
	var spans [][2]int
	for _, m := range n.members {
		spans = append(spans, [2]int{m.keyStart, m.val.end})
	}
	for _, e := range n.elems {
		spans = append(spans, [2]int{e.start, e.end})
	}
	switch {
	case len(spans) == 1:
		return window{n.start + 1, n.end - 1}
	case i > 0:
		return window{spans[i-1][1], spans[i][1]}
	default:
		return window{spans[0][0], spans[1][0]}
	}
}

// tailWindow is what adding an item to the container n may change: from
// its last item's end, or its opening bracket, to its end.
func tailWindow(n *node) window {
	switch {
	case len(n.members) > 0:
		return window{n.members[len(n.members)-1].val.end, n.end}
	case len(n.elems) > 0:
		return window{n.elems[len(n.elems)-1].end, n.end}
	}
	return window{n.start, n.end}
}

func parse(t *testing.T, b []byte) *Doc {
	t.Helper()
	d, err := Parse(b)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

// containers are the paths of every object and array in d, depth first.
func containers(d *Doc) [][]any {
	var out [][]any
	var walk func(n *node, path []any)
	walk = func(n *node, path []any) {
		if n.kind == kindScalar {
			return
		}
		out = append(out, slices.Clone(path))
		for _, m := range n.members {
			walk(m.val, append(slices.Clone(path), m.key))
		}
		for i, e := range n.elems {
			walk(e, append(slices.Clone(path), i))
		}
	}
	walk(d.root, nil)
	return out
}

func TestRemoveEveryItem(t *testing.T) {
	for name, src := range documents(t) {
		for _, cpath := range containers(parse(t, src)) {
			n := parse(t, src).lookup(cpath)
			count := len(n.members) + len(n.elems)
			for i := range count {
				d := parse(t, src)
				n := d.lookup(cpath)
				var step any = i
				if n.kind == kindObject {
					step = n.members[i].key
				}
				path := append(slices.Clone(cpath), step)
				t.Run(fmt.Sprintf("%s/%s", name, pathString(path)), func(t *testing.T) {
					w := itemWindow(n, i)
					want := generic(t, src)
					switch c := at(want, cpath).(type) {
					case map[string]any:
						delete(c, step.(string))
					case []any:
						c = slices.Delete(c, i, i+1)
						want = replaceAt(want, cpath, c)
					}
					if err := d.Remove(path...); err != nil {
						t.Fatal(err)
					}
					if got := generic(t, d.Bytes()); !reflect.DeepEqual(got, want) {
						t.Errorf("value = %v, want %v", got, want)
					}
					checkUntouched(t, src, d.Bytes(), w)
				})
			}
		}
	}
}

// replaceAt sets the value at path in a generic value, returning the
// root.
func replaceAt(root any, path []any, v any) any {
	if len(path) == 0 {
		return v
	}
	parent := at(root, path[:len(path)-1])
	switch k := path[len(path)-1].(type) {
	case string:
		parent.(map[string]any)[k] = v
	case int:
		parent.([]any)[k] = v
	}
	return root
}

func TestAddToEveryContainer(t *testing.T) {
	value := Object{{Key: "id", Value: "new"}, {Key: "kind", Value: "viac"},
		{Key: "path", Value: "${WEALTHDB_DATA_ROOT}/viac/viac.db"}}
	for name, src := range documents(t) {
		for _, cpath := range containers(parse(t, src)) {
			d := parse(t, src)
			n := d.lookup(cpath)
			t.Run(fmt.Sprintf("%s/%s", name, pathString(cpath)), func(t *testing.T) {
				w := tailWindow(n)
				want := generic(t, src)
				var err error
				if n.kind == kindObject {
					want = replaceAt(want, append(slices.Clone(cpath), "zz_added"), normal(t, value))
					err = d.Set(value, append(slices.Clone(cpath), "zz_added")...)
				} else {
					want = replaceAt(want, cpath, append(at(want, cpath).([]any), normal(t, value)))
					err = d.Append(value, cpath...)
				}
				if err != nil {
					t.Fatal(err)
				}
				if got := generic(t, d.Bytes()); !reflect.DeepEqual(got, want) {
					t.Errorf("value = %v, want %v", got, want)
				}
				checkUntouched(t, src, d.Bytes(), w)
				checkIndentFollows(t, d)
			})
		}
	}
}

// checkIndentFollows fails when a line of d starts with an indentation
// that mixes tabs and spaces, the sign of an edit that ignored the
// document's own unit.
func checkIndentFollows(t *testing.T, d *Doc) {
	t.Helper()
	for i, line := range strings.Split(string(d.Bytes()), "\n") {
		ind := line[:len(line)-len(strings.TrimLeft(line, " \t"))]
		if strings.Contains(ind, " ") && strings.Contains(ind, "\t") {
			t.Errorf("line %d mixes tabs and spaces: %q", i+1, line)
		}
	}
}

func TestReplaceEveryMember(t *testing.T) {
	for name, src := range documents(t) {
		for _, cpath := range containers(parse(t, src)) {
			n := parse(t, src).lookup(cpath)
			for i := range len(n.members) + len(n.elems) {
				d := parse(t, src)
				n := d.lookup(cpath)
				var step any = i
				var span window
				if n.kind == kindObject {
					step = n.members[i].key
					span = window{n.members[i].val.start, n.members[i].val.end}
				} else {
					span = window{n.elems[i].start, n.elems[i].end}
				}
				path := append(slices.Clone(cpath), step)
				t.Run(fmt.Sprintf("%s/%s", name, pathString(path)), func(t *testing.T) {
					value := Object{{Key: "enabled", Value: true}, {Key: "port", Value: 3100}}
					want := replaceAt(generic(t, src), path, normal(t, value))
					if err := d.Set(value, path...); err != nil {
						t.Fatal(err)
					}
					if got := generic(t, d.Bytes()); !reflect.DeepEqual(got, want) {
						t.Errorf("value = %v, want %v", got, want)
					}
					checkUntouched(t, src, d.Bytes(), span)
				})
			}
		}
	}
}

func TestSetSameScalarChangesNothing(t *testing.T) {
	for name, src := range documents(t) {
		d := parse(t, src)
		for _, k := range d.Keys() {
			raw, _ := d.Raw(k)
			var v any
			if err := json.Unmarshal(raw, &v); err != nil {
				t.Fatal(err)
			}
			if _, scalar := v.(string); !scalar {
				continue
			}
			if err := d.Set(v, k); err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(d.Bytes(), src) {
				t.Errorf("%s: setting %s to its own value changed the text", name, k)
			}
		}
	}
}

func TestSetCreatesTheWayDown(t *testing.T) {
	d := parse(t, []byte("{\n  \"gold_db\": \"/x/g.db\"\n}\n"))
	if err := d.Set(true, "web", "enabled"); err != nil {
		t.Fatal(err)
	}
	want := "{\n  \"gold_db\": \"/x/g.db\",\n  \"web\": {\n    \"enabled\": true\n  }\n}\n"
	if got := string(d.Bytes()); got != want {
		t.Errorf("got\n%s\nwant\n%s", got, want)
	}
	if err := d.Set(3100, "web", "port"); err != nil {
		t.Fatal(err)
	}
	want = "{\n  \"gold_db\": \"/x/g.db\",\n  \"web\": {\n    \"enabled\": true,\n    \"port\": 3100\n  }\n}\n"
	if got := string(d.Bytes()); got != want {
		t.Errorf("got\n%s\nwant\n%s", got, want)
	}
}

func TestEmptyDocumentBecomesIndented(t *testing.T) {
	for _, src := range []string{"", "  \n"} {
		d := parse(t, []byte(src))
		if !d.Empty() {
			t.Fatalf("%q is not empty", src)
		}
		if err := d.Set("CHF", "default_currency"); err != nil {
			t.Fatal(err)
		}
		if err := d.Set("${WEALTHDB_DATA_ROOT}/wealthdb.db", "gold_db"); err != nil {
			t.Fatal(err)
		}
		if err := d.Append(Object{{Key: "id", Value: "viac"}}, "silver_sources"); err != nil {
			t.Fatal(err)
		}
		want := `{
    "default_currency": "CHF",
    "gold_db": "${WEALTHDB_DATA_ROOT}/wealthdb.db",
    "silver_sources": [
        {
            "id": "viac"
        }
    ]
}
`
		if got := string(d.Bytes()); got != want {
			t.Errorf("got\n%s\nwant\n%s", got, want)
		}
	}
}

func TestAppendKeepsTheSiblingsIndent(t *testing.T) {
	src := "{\n  \"silver_sources\": [\n    {\n      \"id\": \"a\"\n    }\n  ]\n}\n"
	d := parse(t, []byte(src))
	if err := d.Append(Object{{Key: "id", Value: "b"}, {Key: "subsources", Value: []any{Object{{Key: "kind", Value: "ubs-web"}}}}}, "silver_sources"); err != nil {
		t.Fatal(err)
	}
	if err := d.Append(Object{{Key: "kind", Value: "ubs-psn"}}, "silver_sources", 1, "subsources"); err != nil {
		t.Fatal(err)
	}
	want := `{
  "silver_sources": [
    {
      "id": "a"
    },
    {
      "id": "b",
      "subsources": [
        {
          "kind": "ubs-web"
        },
        {
          "kind": "ubs-psn"
        }
      ]
    }
  ]
}
`
	if got := string(d.Bytes()); got != want {
		t.Errorf("got\n%s\nwant\n%s", got, want)
	}
}

func TestRemoveMissingIsNoChange(t *testing.T) {
	src := []byte(`{"a": [1, 2], "b": {"c": 1}}`)
	d := parse(t, src)
	for _, path := range [][]any{{"zz"}, {"a", 5}, {"b", "zz"}, {"zz", "y"}, {"a", "x"}} {
		if err := d.Remove(path...); err != nil {
			t.Errorf("%v: %v", path, err)
		}
	}
	if !bytes.Equal(d.Bytes(), src) {
		t.Errorf("text changed: %s", d.Bytes())
	}
}

func TestRefusals(t *testing.T) {
	if _, err := Parse([]byte(`{"a": `)); err == nil {
		t.Error("Parse took a truncated document")
	}
	if _, err := Parse([]byte(`{} {}`)); err == nil {
		t.Error("Parse took two documents")
	}
	d := parse(t, []byte(`{"a": [1], "s": "x"}`))
	for _, tc := range []struct {
		name string
		err  error
	}{
		{"index past the end", d.Set(2, "a", 3)},
		{"key into an array", d.Set(2, "a", "k")},
		{"key into a string", d.Set(2, "s", "k")},
		{"append to a string", d.Append(2, "s")},
		{"empty path", d.Set(1)},
	} {
		if tc.err == nil {
			t.Errorf("%s: no error", tc.name)
		}
	}
	if got := string(d.Bytes()); got != `{"a": [1], "s": "x"}` {
		t.Errorf("a refused edit changed the text: %s", got)
	}
}

func TestReaders(t *testing.T) {
	d := parse(t, []byte(`{"a": [1, {"b": "x"}], "c": {"d": true, "e": null}, "a2": "A"}`))
	if got := d.Keys(); !slices.Equal(got, []string{"a", "c", "a2"}) {
		t.Errorf("Keys = %v", got)
	}
	if got := d.Len("a"); got != 2 {
		t.Errorf("Len(a) = %d", got)
	}
	if raw, ok := d.Raw("a", 1, "b"); !ok || string(raw) != `"x"` {
		t.Errorf("Raw = %s, %v", raw, ok)
	}
	var s string
	if ok, err := d.Decode(&s, "a2"); !ok || err != nil || s != "A" {
		t.Errorf("Decode = %q, %v, %v", s, ok, err)
	}
	if d.Has("c", "zz") || !d.Has("c", "e") {
		t.Error("Has is wrong")
	}
}
