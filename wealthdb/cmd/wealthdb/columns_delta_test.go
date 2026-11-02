package main

import (
	"reflect"
	"strings"
	"testing"
)

func TestParseColumnsDelta(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in      string
		adds    []string
		removes []string
		isDelta bool
	}{
		{"", nil, nil, false},
		{"default", nil, nil, false},
		{"all", nil, nil, false},
		{"foo,bar", nil, nil, false},
		{"+foo", []string{"foo"}, nil, true},
		{"-foo", nil, []string{"foo"}, true},
		{"+foo,bar", []string{"foo", "bar"}, nil, true},
		{"-foo,bar", nil, []string{"foo", "bar"}, true},
		{"+foo-bar", []string{"foo"}, []string{"bar"}, true},
		{"+foo,bar-baz,qux", []string{"foo", "bar"}, []string{"baz", "qux"}, true},
		{"+foo-bar+baz", []string{"foo", "baz"}, []string{"bar"}, true},
		{"  +foo , bar  -  baz  ", []string{"foo", "bar"}, []string{"baz"}, true},
	}
	for _, c := range cases {
		adds, removes, isDelta := parseColumnsDelta(c.in)
		if isDelta != c.isDelta {
			t.Errorf("parseColumnsDelta(%q): isDelta = %v, want %v", c.in, isDelta, c.isDelta)
			continue
		}
		if !reflect.DeepEqual(adds, c.adds) {
			t.Errorf("parseColumnsDelta(%q): adds = %v, want %v", c.in, adds, c.adds)
		}
		if !reflect.DeepEqual(removes, c.removes) {
			t.Errorf("parseColumnsDelta(%q): removes = %v, want %v", c.in, removes, c.removes)
		}
	}
}

func TestApplyColumnsDelta(t *testing.T) {
	t.Parallel()
	base := []string{"a", "b", "c", "d"}
	cases := []struct {
		name    string
		adds    []string
		removes []string
		want    []string
	}{
		{"add new", []string{"e"}, nil, []string{"a", "b", "c", "d", "e"}},
		{"add dup is no-op", []string{"b"}, nil, []string{"a", "b", "c", "d"}},
		{"remove one", nil, []string{"b"}, []string{"a", "c", "d"}},
		{"remove missing is no-op", nil, []string{"z"}, []string{"a", "b", "c", "d"}},
		{"add+remove", []string{"e"}, []string{"a"}, []string{"b", "c", "d", "e"}},
		{"add then remove same", []string{"e"}, []string{"e"}, []string{"a", "b", "c", "d"}},
		{"empty adds/removes", nil, nil, []string{"a", "b", "c", "d"}},
	}
	for _, c := range cases {
		got := applyColumnsDelta(base, c.adds, c.removes)
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
	}
}

// TestColumnsTakeHeaders: -C reads a column by its registry name or by
// its header as printed, any case, in a list and in a delta, as the
// server's columns parameter does.
func TestColumnsTakeHeaders(t *testing.T) {
	t.Parallel()
	type row struct{}
	reg := []columnSpec[row]{
		{Name: "name"}, {Name: "value", Header: "value_USD"}, {Name: "quality"},
	}
	names := func(cs []columnSpec[row]) []string {
		var out []string
		for _, c := range cs {
			out = append(out, c.Name)
		}
		return out
	}
	for _, c := range []struct{ expr, want string }{
		{"name,value_USD", "name value"},
		{"NAME,Value_usd", "name value"},
		{"name,value", "name value"},
		{"-value_USD", "name"},
		{"+quality,-value_USD", "name quality"},
	} {
		got, err := resolveColumns(c.expr, []string{"name", "value"}, reg)
		if err != nil || strings.Join(names(got), " ") != c.want {
			t.Errorf("-C %s = %v %v, want %s", c.expr, names(got), err, c.want)
		}
	}
	if _, err := resolveColumns("value_CHF", nil, reg); err == nil || !strings.Contains(err.Error(), `unknown column "value_CHF"`) {
		t.Errorf("another currency's header: %v", err)
	}
}
