package main

import (
	"fmt"
	"strings"
)

// wrapWords breaks s at spaces into lines of at most width bytes; a
// longer word stands alone on its line.
func wrapWords(s string, width int) []string {
	var lines []string
	line := ""
	for _, w := range strings.Fields(s) {
		switch {
		case line == "":
			line = w
		case len(line)+1+len(w) > width:
			lines = append(lines, line)
			line = w
		default:
			line += " " + w
		}
	}
	return append(lines, line)
}

// andList joins words as prose: "a, b and c".
func andList(words []string) string { return joinProse(words, "and") }

// orList joins words as prose: "a, b or c".
func orList(words []string) string { return joinProse(words, "or") }

// joinProse joins words as prose, the last two by conj.
func joinProse(words []string, conj string) string {
	if len(words) < 2 {
		return strings.Join(words, "")
	}
	return strings.Join(words[:len(words)-1], ", ") + " " + conj + " " + words[len(words)-1]
}

// flagDescription wraps a flag's description to the usage's description
// column.
func flagDescription(s string) string {
	return strings.Join(wrapWords(s, 52), "\n"+strings.Repeat(" ", 21))
}

func orDefault(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
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

// normalizeCell is the form filters compare in: lower case, with an
// underscore read as a space, so an id spelling matches a label.
func normalizeCell(s string) string {
	return strings.TrimSpace(strings.ReplaceAll(strings.ToLower(s), "_", " "))
}

// tagNote is a tag a column prints and what it means: a quality tag, a
// basis.
type tagNote struct{ tag, meaning string }

// tagNotes are tags as the usage texts list them: aligned, each meaning
// wrapped beside its tag.
func tagNotes(tags []tagNote) string {
	width := 0
	for _, q := range tags {
		width = max(width, len(q.tag))
	}
	var b strings.Builder
	for _, q := range tags {
		tag := q.tag
		for _, line := range wrapWords(q.meaning, 72-4-width-2) {
			fmt.Fprintf(&b, "\n    %-*s  %s", width, tag, line)
			tag = ""
		}
	}
	return b.String()
}

// tagList are tags as the MCP help lists them: one "- tag: meaning."
// line each.
func tagList(tags []tagNote) string {
	var b strings.Builder
	for _, q := range tags {
		b.WriteString("\n- " + q.tag + ": " + q.meaning + ".")
	}
	return b.String()
}
