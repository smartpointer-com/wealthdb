package main

import (
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
)

// paramKind is a tool parameter's JSON type.
type paramKind int

const (
	paramString paramKind = iota
	paramInteger
	paramBoolean
)

// param is one tool parameter. The input schema, describe's listing
// and the validation messages are all generated from it, so a
// parameter is described in exactly one place.
type param struct {
	name string
	kind paramKind
	doc  string
	// enum is the vocabulary of a string parameter, if it has one.
	// Values are matched case-insensitively.
	enum []string
}

// enumAliases are spellings a model reaches for that name a value of
// the vocabulary. They are accepted silently: the result header shows
// the value used.
var enumAliases = map[string]string{
	"year": "annual", "yearly": "annual", "annually": "annual",
	"quarter": "quarterly", "month": "monthly", "week": "weekly", "day": "daily",
	"all": "total", "whole": "total",
}

// maxIntegerParam bounds an integer parameter: a billion rows is more
// than any report holds, and the bound keeps offset+limit from
// overflowing.
const maxIntegerParam = 1_000_000_000

// toolArgs are one call's arguments, checked against the tool's
// parameters: every name known, every value of its kind, every enum
// value in its vocabulary. Absent, null and empty-string values are
// all "not given".
type toolArgs struct {
	values map[string]any // string, int or bool, by the param's kind
}

// parseArgs decodes and checks raw call arguments. It is lenient about
// form, the way small models need it to be — a number where a string
// was asked for, "10" for an integer, "true" for a boolean, any case
// for an enum value — and strict about meaning: an unknown parameter
// or a value outside a vocabulary is an error that names the fix.
func parseArgs(tool string, params []param, raw json.RawMessage) (*toolArgs, error) {
	a := &toolArgs{values: map[string]any{}}
	if len(raw) == 0 || string(raw) == "null" {
		return a, nil
	}
	var in map[string]json.RawMessage
	if err := json.Unmarshal(raw, &in); err != nil {
		return nil, fmt.Errorf("the arguments are not a JSON object: %v", err)
	}
	byName := make(map[string]param, len(params))
	for _, p := range params {
		byName[p.name] = p
	}
	names := make([]string, 0, len(in))
	for name := range in {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		p, ok := byName[name]
		if !ok {
			return nil, unknownParamError(tool, name, params)
		}
		v, given, err := decodeParam(p, in[name])
		if err != nil {
			return nil, fmt.Errorf("%s: %s %v", tool, name, err)
		}
		if given {
			a.values[name] = v
		}
	}
	return a, nil
}

// decodeParam reads one value as p's kind. given is false for null and
// for an empty or blank string.
func decodeParam(p param, raw json.RawMessage) (v any, given bool, err error) {
	var x any
	if err := json.Unmarshal(raw, &x); err != nil {
		return nil, false, fmt.Errorf("is not valid JSON")
	}
	if x == nil {
		return nil, false, nil
	}
	if s, ok := x.(string); ok && strings.TrimSpace(s) == "" {
		return nil, false, nil
	}
	switch p.kind {
	case paramInteger:
		var f float64
		switch t := x.(type) {
		case float64:
			f = t
		case string:
			n, err := strconv.ParseFloat(strings.TrimSpace(t), 64)
			if err != nil {
				return nil, false, fmt.Errorf("must be a whole number, not %q", t)
			}
			f = n
		default:
			return nil, false, fmt.Errorf("must be a whole number")
		}
		if f != math.Trunc(f) {
			return nil, false, fmt.Errorf("must be a whole number, not %v", f)
		}
		// Far beyond any row count, and safe to add to another.
		if math.Abs(f) > maxIntegerParam {
			return nil, false, fmt.Errorf("must be at most %d", maxIntegerParam)
		}
		return int(f), true, nil
	case paramBoolean:
		switch t := x.(type) {
		case bool:
			return t, true, nil
		case float64:
			return t != 0, true, nil
		case string:
			switch strings.ToLower(strings.TrimSpace(t)) {
			case "true", "yes", "on", "1":
				return true, true, nil
			case "false", "no", "off", "0":
				return false, true, nil
			}
		}
		return nil, false, fmt.Errorf("must be true or false")
	}
	var s string
	switch t := x.(type) {
	case string:
		s = strings.TrimSpace(t)
	case float64:
		// A year sent as a number is the commonest case.
		s = strconv.FormatFloat(t, 'f', -1, 64)
	case bool:
		s = strconv.FormatBool(t)
	default:
		return nil, false, fmt.Errorf("must be a string")
	}
	if len(p.enum) == 0 {
		return s, true, nil
	}
	low := strings.ToLower(s)
	if alias, ok := enumAliases[low]; ok && contains(p.enum, alias) {
		low = alias
	}
	if !contains(p.enum, low) {
		return nil, false, fmt.Errorf("%q is not one of its values; use %s", s, orList(p.enum))
	}
	return low, true, nil
}

// unknownParamError names the parameters the tool does take, and the
// one that was probably meant.
func unknownParamError(tool, name string, params []param) error {
	known := make([]string, len(params))
	for i, p := range params {
		known[i] = p.name
	}
	msg := fmt.Sprintf("%s has no parameter %q", tool, name)
	if guess, ok := nearMiss(strings.ToLower(name), known); ok {
		msg += fmt.Sprintf(" (did you mean %q?)", guess)
	}
	return fmt.Errorf("%s; its parameters are %s", msg, strings.Join(known, ", "))
}

// str is a string parameter, "" when not given.
func (a *toolArgs) str(name string) string {
	s, _ := a.values[name].(string)
	return s
}

// has reports whether a parameter was given.
func (a *toolArgs) has(name string) bool {
	_, ok := a.values[name]
	return ok
}

// integer is an integer parameter and whether it was given.
func (a *toolArgs) integer(name string) (int, bool) {
	n, ok := a.values[name].(int)
	return n, ok
}

// flag is a boolean parameter, def when not given.
func (a *toolArgs) flag(name string, def bool) bool {
	if b, ok := a.values[name].(bool); ok {
		return b
	}
	return def
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// orList renders a vocabulary for a message: "a, b or c".
func orList(values []string) string {
	switch len(values) {
	case 0:
		return ""
	case 1:
		return values[0]
	}
	return strings.Join(values[:len(values)-1], ", ") + " or " + values[len(values)-1]
}

// nearMiss returns the one candidate within a typo of s: a similarity
// ratio of at least 0.8 over the longest common subsequence, the
// measure difflib uses. Two candidates that close are ambiguous, and
// none is returned.
func nearMiss(s string, candidates []string) (string, bool) {
	found, n := "", 0
	seen := map[string]bool{}
	for _, c := range candidates {
		if seen[c] {
			continue
		}
		seen[c] = true
		if similarity(s, strings.ToLower(c)) >= 0.8 {
			found = c
			n++
		}
	}
	return found, n == 1
}

// similarity is 2·LCS / (len(a)+len(b)), 1.0 for identical strings.
func similarity(a, b string) float64 {
	if len(a)+len(b) == 0 {
		return 1
	}
	prev := make([]int, len(b)+1)
	cur := make([]int, len(b)+1)
	for i := 1; i <= len(a); i++ {
		for j := 1; j <= len(b); j++ {
			switch {
			case a[i-1] == b[j-1]:
				cur[j] = prev[j-1] + 1
			case prev[j] >= cur[j-1]:
				cur[j] = prev[j]
			default:
				cur[j] = cur[j-1]
			}
		}
		prev, cur = cur, prev
	}
	return 2 * float64(prev[len(b)]) / float64(len(a)+len(b))
}
