package main

import "strings"

// splitFusedColumnsFlag rewrites argv tokens of the form
// `-C+foo,bar-baz` or `--columns-baz` into two tokens (`-C`,
// `+foo,bar-baz`) so Go's stdlib flag package can parse them.
// Without this, `-C+foo` is read as a flag literally named
// `C+foo`. The `=` form (`-C=+foo`, `--columns=+foo`) and the
// space form (`-C +foo`) are already handled by the stdlib, so
// we leave them alone.
func splitFusedColumnsFlag(args []string) []string {
	out := make([]string, 0, len(args))
	for _, a := range args {
		switch {
		case len(a) > 2 && a[0:2] == "-C" && (a[2] == '+' || a[2] == '-'):
			out = append(out, "-C", a[2:])
		case len(a) > len("--columns") && strings.HasPrefix(a, "--columns") &&
			(a[len("--columns")] == '+' || a[len("--columns")] == '-'):
			out = append(out, "--columns", a[len("--columns"):])
		default:
			out = append(out, a)
		}
	}
	return out
}

// parseColumnsDelta parses a +/--prefixed column expression such as
// "+foo,bar-baz+qux" into ordered (adds, removes) slices. Returns
// isDelta=false (with no error and empty slices) when s doesn't
// start with a +/- sign — the caller falls back to its absolute-
// list handling ('default', 'all', explicit comma list).
//
// The `+` and `-` characters act as section markers; whatever
// follows up to the next marker is a comma-separated list of
// column names assigned to that section. Column names in this
// project use underscores, so the markers can't collide with a
// literal hyphen inside a column name.
func parseColumnsDelta(s string) (adds, removes []string, isDelta bool) {
	s = strings.TrimSpace(s)
	if s == "" || (s[0] != '+' && s[0] != '-') {
		return nil, nil, false
	}
	var sign byte
	var current strings.Builder
	flush := func() {
		if current.Len() == 0 {
			return
		}
		for _, n := range strings.Split(current.String(), ",") {
			n = strings.TrimSpace(n)
			if n == "" {
				continue
			}
			switch sign {
			case '+':
				adds = append(adds, n)
			case '-':
				removes = append(removes, n)
			}
		}
		current.Reset()
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == '+' || c == '-' {
			flush()
			sign = c
			continue
		}
		current.WriteByte(c)
	}
	flush()
	return adds, removes, true
}

// applyColumnsDelta returns base + adds - removes, preserving the
// order of `base` and appending unique adds at the end. Adds
// already present in base are skipped (no duplicates, no reorder).
// Removes that aren't in the result are a silent no-op so users
// can write "-account_id" without first checking it's in the
// default for that command.
func applyColumnsDelta(base, adds, removes []string) []string {
	out := make([]string, 0, len(base)+len(adds))
	seen := make(map[string]bool, len(base)+len(adds))
	for _, n := range base {
		if !seen[n] {
			out = append(out, n)
			seen[n] = true
		}
	}
	for _, n := range adds {
		if !seen[n] {
			out = append(out, n)
			seen[n] = true
		}
	}
	if len(removes) == 0 {
		return out
	}
	drop := make(map[string]bool, len(removes))
	for _, n := range removes {
		drop[n] = true
	}
	filtered := out[:0]
	for _, n := range out {
		if drop[n] {
			continue
		}
		filtered = append(filtered, n)
	}
	return filtered
}
