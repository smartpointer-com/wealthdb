// Package jsonedit edits JSON text in place. It sets, removes and
// appends values by path and leaves every other byte of the document
// as it was: the order of the members, the spacing, the indentation and
// the line breaks of everything an edit does not touch.
//
// A path is a list of steps: a string names an object member, an int an
// array element. A new member or element takes the indentation the
// document already uses: the indentation of its last sibling, or one
// unit deeper than its parent's line when it has none. The unit is the
// one the document's own nesting shows, four spaces in a document that
// shows none. A line an edit adds ends in a line feed, also in a
// document whose lines end in CR LF.
//
// Of a key written twice in one object, the last member is the value at
// its path, as encoding/json reads it: Set replaces that one, and Remove
// removes that one, which leaves the earlier member as the value.
package jsonedit

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
)

// defaultUnit is the indent unit of a document whose nesting shows none.
const defaultUnit = "    "

type kind int

const (
	kindScalar kind = iota
	kindObject
	kindArray
)

// node is one value of the document: its kind, the span [start, end)
// of its text, and its members or elements.
type node struct {
	kind       kind
	start, end int
	members    []member
	elems      []*node
}

// member is one member of an object: its key, the offset of the key's
// opening quote, and its value.
type member struct {
	key      string
	keyStart int
	val      *node
}

// Doc is a JSON document as text, with the spans of its values. The
// zero Doc is an empty document.
type Doc struct {
	src  []byte
	root *node
	unit string
}

// Parse reads src. A src of whitespace alone, or none, is an empty
// document, which the first Set turns into an object.
func Parse(src []byte) (*Doc, error) {
	d := &Doc{}
	if err := d.reset(src); err != nil {
		return nil, err
	}
	return d, nil
}

// reset makes src the document's text and parses it.
func (d *Doc) reset(src []byte) error {
	if len(bytes.TrimSpace(src)) == 0 {
		d.src, d.root, d.unit = src, nil, defaultUnit
		return nil
	}
	if !json.Valid(src) {
		var v any
		err := json.Unmarshal(src, &v)
		if err == nil {
			err = fmt.Errorf("not one JSON value")
		}
		return err
	}
	p := parser{src: src}
	p.ws()
	root := p.value()
	d.src, d.root = src, root
	d.unit = d.detectUnit()
	return nil
}

// Bytes is the document's text. The caller must not change it.
func (d *Doc) Bytes() []byte { return d.src }

// Raw is the text of the value at path, and whether there is one.
func (d *Doc) Raw(path ...any) ([]byte, bool) {
	n := d.lookup(path)
	if n == nil {
		return nil, false
	}
	return d.src[n.start:n.end], true
}

// Has says a value sits at path.
func (d *Doc) Has(path ...any) bool { return d.lookup(path) != nil }

// Decode unmarshals the value at path into v; it reports false, and
// leaves v alone, when there is none.
func (d *Doc) Decode(v any, path ...any) (bool, error) {
	raw, ok := d.Raw(path...)
	if !ok {
		return false, nil
	}
	return true, json.Unmarshal(raw, v)
}

// Keys are the member keys of the object at path, in document order;
// nil when there is no object there.
func (d *Doc) Keys(path ...any) []string {
	n := d.lookup(path)
	if n == nil || n.kind != kindObject {
		return nil
	}
	out := make([]string, len(n.members))
	for i, m := range n.members {
		out[i] = m.key
	}
	return out
}

// Len is the number of elements of the array at path, or of members of
// the object there; 0 when there is neither.
func (d *Doc) Len(path ...any) int {
	n := d.lookup(path)
	if n == nil {
		return 0
	}
	if n.kind == kindArray {
		return len(n.elems)
	}
	return len(n.members)
}

// Set writes v at path. An existing value is replaced; a missing member
// is added at the end of its object, with any missing objects on the way
// to it. A null on the way counts as missing: the objects that lead to
// path replace it. An array element must exist already: Append adds one.
func (d *Doc) Set(v any, path ...any) error {
	if len(path) == 0 {
		return fmt.Errorf("jsonedit: set: empty path")
	}
	if d.root == nil {
		if err := d.reset([]byte("{}\n")); err != nil {
			return err
		}
	}
	if d.null(d.root) {
		if err := d.splice(d.root.start, d.root.end, "{}"); err != nil {
			return err
		}
	}
	parent, i := d.root, 0
	for ; i < len(path)-1; i++ {
		child, _ := d.item(parent, path[i])
		if child == nil || d.null(child) {
			break
		}
		parent = child
	}
	if i < len(path)-1 {
		// A value on the way is missing, or null: write it, holding the
		// rest of the path as nested objects.
		nested := v
		for j := len(path) - 1; j > i; j-- {
			k, ok := path[j].(string)
			if !ok {
				return fmt.Errorf("jsonedit: set: element %v of %s does not exist", path[j], pathString(path[:j]))
			}
			nested = Object{{Key: k, Value: nested}}
		}
		if null, at := d.item(parent, path[i]); null != nil {
			return d.replace(null, at, nested)
		}
		key, ok := path[i].(string)
		if !ok {
			return fmt.Errorf("jsonedit: set: element %v of %s does not exist", path[i], pathString(path[:i]))
		}
		return d.insertMember(parent, key, nested, path[:i])
	}
	switch k := path[len(path)-1].(type) {
	case string:
		if parent.kind != kindObject {
			return fmt.Errorf("jsonedit: set: %s is not an object", pathString(path[:len(path)-1]))
		}
		if m := findMember(parent, k); m != nil {
			return d.replace(m.val, m.keyStart, v)
		}
		return d.insertMember(parent, k, v, path[:len(path)-1])
	case int:
		if parent.kind != kindArray || k < 0 || k >= len(parent.elems) {
			return fmt.Errorf("jsonedit: set: %s has no element %d", pathString(path[:len(path)-1]), k)
		}
		e := parent.elems[k]
		return d.replace(e, e.start, v)
	default:
		return fmt.Errorf("jsonedit: set: path step %v is neither a key nor an index", k)
	}
}

// Append adds v as the last element of the array at path. A missing
// array, or a null in its place, is written as a member holding v alone
// (Set).
func (d *Doc) Append(v any, path ...any) error {
	n := d.lookup(path)
	if n == nil || d.null(n) {
		return d.Set([]any{v}, path...)
	}
	if n.kind != kindArray {
		return fmt.Errorf("jsonedit: append: %s is not an array", pathString(path))
	}
	if len(n.elems) == 0 {
		ind := d.lineIndent(n.start)
		child := ind + d.unit
		val, err := d.encode(v, child, false)
		if err != nil {
			return err
		}
		return d.splice(n.start, n.end, "[\n"+child+val+"\n"+ind+"]")
	}
	last := n.elems[len(n.elems)-1]
	if d.ownLine(last.start) {
		ind := d.indentBefore(last.start)
		val, err := d.encode(v, ind, false)
		if err != nil {
			return err
		}
		return d.splice(last.end, last.end, ",\n"+ind+val)
	}
	val, err := d.encode(v, "", true)
	if err != nil {
		return err
	}
	return d.splice(last.end, last.end, ", "+val)
}

// Remove deletes the member or element at path, with the separator
// before it (after it, for the first of several). Removing a value that
// is not there is no change. Of a key written twice, the last member
// goes, and the earlier one is the value at path from then on.
func (d *Doc) Remove(path ...any) error {
	if len(path) == 0 {
		return fmt.Errorf("jsonedit: remove: empty path")
	}
	parent := d.lookup(path[:len(path)-1])
	if parent == nil {
		return nil
	}
	var spans [][2]int // per item: where its text starts, where its value ends
	idx := -1
	switch k := path[len(path)-1].(type) {
	case string:
		if parent.kind != kindObject {
			return nil
		}
		for i, m := range parent.members {
			spans = append(spans, [2]int{m.keyStart, m.val.end})
			if m.key == k {
				idx = i
			}
		}
	case int:
		if parent.kind != kindArray {
			return nil
		}
		for _, e := range parent.elems {
			spans = append(spans, [2]int{e.start, e.end})
		}
		if k >= 0 && k < len(parent.elems) {
			idx = k
		}
	}
	switch {
	case idx < 0:
		return nil
	case len(spans) == 1:
		return d.splice(parent.start+1, parent.end-1, "")
	case idx > 0:
		return d.splice(spans[idx-1][1], spans[idx][1], "")
	default:
		return d.splice(spans[0][0], spans[1][0], "")
	}
}

// insertMember adds key: v at the end of obj, which sits at path.
func (d *Doc) insertMember(obj *node, key string, v any, path []any) error {
	if obj.kind != kindObject {
		return fmt.Errorf("jsonedit: set: %s is not an object", pathString(path))
	}
	k, err := d.encode(key, "", true)
	if err != nil {
		return err
	}
	if len(obj.members) == 0 {
		ind := d.lineIndent(obj.start)
		child := ind + d.unit
		val, err := d.encode(v, child, false)
		if err != nil {
			return err
		}
		return d.splice(obj.start, obj.end, "{\n"+child+k+": "+val+"\n"+ind+"}")
	}
	last := obj.members[len(obj.members)-1]
	if d.ownLine(last.keyStart) {
		ind := d.indentBefore(last.keyStart)
		val, err := d.encode(v, ind, false)
		if err != nil {
			return err
		}
		return d.splice(last.val.end, last.val.end, ",\n"+ind+k+": "+val)
	}
	val, err := d.encode(v, "", true)
	if err != nil {
		return err
	}
	return d.splice(last.val.end, last.val.end, ", "+k+": "+val)
}

// replace writes v over the value n, whose member or element starts at
// at: indented below that line when it stands on a line of its own,
// on one line when it shares its line with siblings.
func (d *Doc) replace(n *node, at int, v any) error {
	inline := !d.ownLine(at)
	val, err := d.encode(v, d.lineIndent(at), inline)
	if err != nil {
		return err
	}
	return d.splice(n.start, n.end, val)
}

// splice replaces src[start:end] with text and parses the result. On an
// error the document is left as it was.
func (d *Doc) splice(start, end int, text string) error {
	out := make([]byte, 0, len(d.src)-(end-start)+len(text))
	out = append(out, d.src[:start]...)
	out = append(out, text...)
	out = append(out, d.src[end:]...)
	unit := d.unit
	if err := d.reset(out); err != nil {
		return fmt.Errorf("jsonedit: an edit made invalid JSON: %w", err)
	}
	// A document whose nesting showed nothing keeps the unit it wrote
	// with.
	if d.unit == defaultUnit {
		d.unit = unit
	}
	return nil
}

// encode is v as JSON text: on one line when inline, else indented by
// the document's unit below a first line that starts at prefix. HTML
// characters are written as they are.
func (d *Doc) encode(v any, prefix string, inline bool) (string, error) {
	raw, err := marshal(v)
	if err != nil {
		return "", fmt.Errorf("jsonedit: encode: %w", err)
	}
	var b bytes.Buffer
	if inline {
		err = json.Compact(&b, raw)
	} else {
		err = json.Indent(&b, raw, prefix, d.unit)
	}
	if err != nil {
		return "", fmt.Errorf("jsonedit: encode: %w", err)
	}
	return b.String(), nil
}

// marshal is json.Marshal without the escaping of HTML characters.
func marshal(v any) ([]byte, error) {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(b.Bytes(), "\n"), nil
}

// lookup is the value at path, or nil.
func (d *Doc) lookup(path []any) *node {
	n := d.root
	for _, s := range path {
		if n == nil {
			return nil
		}
		n = d.step(n, s)
	}
	return n
}

// step is the member or element of n that s names, or nil.
func (d *Doc) step(n *node, s any) *node {
	v, _ := d.item(n, s)
	return v
}

// item is the member or element of n that s names, and the offset its
// text starts at: a member's key, an element's value. nil when n holds
// none.
func (d *Doc) item(n *node, s any) (*node, int) {
	switch k := s.(type) {
	case string:
		if n.kind == kindObject {
			if m := findMember(n, k); m != nil {
				return m.val, m.keyStart
			}
		}
	case int:
		if n.kind == kindArray && k >= 0 && k < len(n.elems) {
			return n.elems[k], n.elems[k].start
		}
	}
	return nil, 0
}

// null says n is the value null.
func (d *Doc) null(n *node) bool {
	return n.kind == kindScalar && string(d.src[n.start:n.end]) == "null"
}

// findMember is obj's member key. With a key written twice it is the
// last, the one encoding/json keeps.
func findMember(obj *node, key string) *member {
	for i := len(obj.members) - 1; i >= 0; i-- {
		if obj.members[i].key == key {
			return &obj.members[i]
		}
	}
	return nil
}

// lineStart is the offset of the first byte of the line holding pos.
func (d *Doc) lineStart(pos int) int {
	i := bytes.LastIndexByte(d.src[:pos], '\n')
	return i + 1
}

// lineIndent is the leading whitespace of the line holding pos.
func (d *Doc) lineIndent(pos int) string {
	s := d.lineStart(pos)
	e := s
	for e < len(d.src) && (d.src[e] == ' ' || d.src[e] == '\t') {
		e++
	}
	return string(d.src[s:e])
}

// ownLine says only whitespace precedes pos on its line.
func (d *Doc) ownLine(pos int) bool {
	return len(bytes.Trim(d.src[d.lineStart(pos):pos], " \t")) == 0
}

// indentBefore is the whitespace before pos on its line, which ownLine
// says is all that precedes it.
func (d *Doc) indentBefore(pos int) string {
	return string(d.src[d.lineStart(pos):pos])
}

// detectUnit is the indent unit the document's nesting shows: the
// first container, breadth first, whose first item stands on a line of
// its own deeper than the container's line.
func (d *Doc) detectUnit() string {
	queue := []*node{d.root}
	for len(queue) > 0 {
		n := queue[0]
		queue = queue[1:]
		var first int
		switch {
		case n.kind == kindObject && len(n.members) > 0:
			first = n.members[0].keyStart
			for _, m := range n.members {
				queue = append(queue, m.val)
			}
		case n.kind == kindArray && len(n.elems) > 0:
			first = n.elems[0].start
			queue = append(queue, n.elems...)
		default:
			continue
		}
		if !d.ownLine(first) {
			continue
		}
		child, parent := d.indentBefore(first), d.lineIndent(n.start)
		if len(child) > len(parent) && strings.HasPrefix(child, parent) {
			return child[len(parent):]
		}
	}
	return defaultUnit
}

// pathString renders a path for an error.
func pathString(path []any) string {
	if len(path) == 0 {
		return "the document"
	}
	var b strings.Builder
	for _, s := range path {
		switch k := s.(type) {
		case string:
			if b.Len() > 0 {
				b.WriteByte('.')
			}
			b.WriteString(k)
		default:
			fmt.Fprintf(&b, "[%v]", k)
		}
	}
	return b.String()
}

// Object is a JSON object whose members are written in their order.
type Object []Member

// Member is one member of an Object.
type Member struct {
	Key   string
	Value any
}

// MarshalJSON writes the members in order.
func (o Object) MarshalJSON() ([]byte, error) {
	var b bytes.Buffer
	b.WriteByte('{')
	for i, m := range o {
		if i > 0 {
			b.WriteByte(',')
		}
		k, err := marshal(m.Key)
		if err != nil {
			return nil, err
		}
		v, err := marshal(m.Value)
		if err != nil {
			return nil, err
		}
		b.Write(k)
		b.WriteByte(':')
		b.Write(v)
	}
	b.WriteByte('}')
	return b.Bytes(), nil
}

// parser walks text json.Valid has accepted, so it checks nothing.
type parser struct {
	src []byte
	pos int
}

func (p *parser) ws() {
	for p.pos < len(p.src) {
		switch p.src[p.pos] {
		case ' ', '\t', '\n', '\r':
			p.pos++
		default:
			return
		}
	}
}

func (p *parser) value() *node {
	n := &node{start: p.pos}
	switch p.src[p.pos] {
	case '{':
		n.kind = kindObject
		p.pos++
		p.ws()
		for p.src[p.pos] != '}' {
			keyStart := p.pos
			p.str()
			var key string
			_ = json.Unmarshal(p.src[keyStart:p.pos], &key)
			p.ws()
			p.pos++ // ':'
			p.ws()
			n.members = append(n.members, member{key: key, keyStart: keyStart, val: p.value()})
			p.ws()
			if p.src[p.pos] == ',' {
				p.pos++
				p.ws()
			}
		}
		p.pos++
	case '[':
		n.kind = kindArray
		p.pos++
		p.ws()
		for p.src[p.pos] != ']' {
			n.elems = append(n.elems, p.value())
			p.ws()
			if p.src[p.pos] == ',' {
				p.pos++
				p.ws()
			}
		}
		p.pos++
	case '"':
		p.str()
	default:
		for p.pos < len(p.src) && !strings.ContainsRune(" \t\r\n,]}", rune(p.src[p.pos])) {
			p.pos++
		}
	}
	n.end = p.pos
	return n
}

// str moves past the string that starts at pos.
func (p *parser) str() {
	p.pos++
	for p.src[p.pos] != '"' {
		if p.src[p.pos] == '\\' {
			p.pos++
		}
		p.pos++
	}
	p.pos++
}
