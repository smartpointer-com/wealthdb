package main

import (
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/gold"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/output"
)

// writeFormatted dispatches to the right output.Write* function
// for the chosen format.
func writeFormatted(w io.Writer, f output.Format, t output.Table) error {
	switch f {
	case output.FormatTable:
		return output.WriteTable(w, t)
	case output.FormatCSV:
		return output.WriteCSV(w, t)
	case output.FormatCSVPlain:
		return output.WriteCSVPlain(w, t)
	case output.FormatJSON:
		return output.WriteJSON(w, t)
	}
	return fmt.Errorf("unsupported output format %q", f)
}

// formatCents renders a decimal-string-as-pointer with exactly two
// fractional digits — the canonical convention for money columns.
// Returns "" for nil so empty cells stay empty (not "0.00"). On parse
// failure falls back to the raw string rather than erroring.
func formatCents(p *string) string {
	if p == nil {
		return ""
	}
	d, err := canonical.NewDecimalFromString(*p)
	if err != nil {
		return *p
	}
	return d.StringFixed(2)
}

// formatPct renders a return ratio as a percentage with two decimals, or "n/a"
// when the figure is undefined (the reason is in the quality column).
func formatPct(p *float64) string {
	if p == nil {
		return "n/a"
	}
	return formatPctOrBlank(p)
}

// formatPctOrBlank is formatPct for a ratio whose absence needs no
// reason, such as an unrealized gain on a holding with no cost basis:
// the cell stays empty.
func formatPctOrBlank(p *float64) string {
	if p == nil {
		return ""
	}
	pct := *p * 100
	return formatPercent(&pct)
}

// formatDateTime is the human-readable form for second-grain
// audit/registration timestamps.
func formatDateTime(epoch int64) string {
	return time.Unix(epoch, 0).UTC().Format("2006-01-02 15:04:05Z")
}

// thousands renders n with comma separators, for the paging line.
func thousands(n int) string {
	if n < 0 {
		return "-" + thousands(-n)
	}
	s := strconv.Itoa(n)
	for i := len(s) - 3; i > 0; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	return s
}

// fixedOrBlank is a figure with prec decimals, blank for nil.
func fixedOrBlank(p *float64, prec int) string {
	if p == nil {
		return ""
	}
	return strconv.FormatFloat(*p, 'f', prec, 64)
}

// strOrEmpty is a nullable text, blank for nil.
func strOrEmpty(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

// formatDate is an epoch second's UTC date.
func formatDate(epoch int64) string {
	return time.Unix(epoch, 0).UTC().Format("2006-01-02")
}

// formatEpochDay renders a day number (days since the epoch) as a date.
func formatEpochDay(day int64) string {
	return formatDate(day * gold.SecondsPerDay)
}

// formatPercent is a figure already in percent with two decimals, blank
// for nil.
func formatPercent(p *float64) string { return fixedOrBlank(p, 2) }

// intOrEmpty is a nullable count, blank for nil.
func intOrEmpty(p *int64) string {
	if p == nil {
		return ""
	}
	return fmt.Sprintf("%d", *p)
}

// yesNo renders a stated flag, empty where the source states nothing.
func yesNo(b *bool) string {
	switch {
	case b == nil:
		return ""
	case *b:
		return "yes"
	default:
		return "no"
	}
}

// unsignedZero is a figure's cell without the sign of a zero: a tiny
// negative that rounds to zero prints as 0.00, never as -0.00.
func unsignedZero(cell string) string {
	digits, negative := strings.CutPrefix(cell, "-")
	if !negative || digits == "" || digits[0] < '0' || digits[0] > '9' || strings.Trim(digits, "0.") != "" {
		return cell
	}
	return digits
}

// periodStart renders the bucket's opening date, empty for the
// `total` bucket, which has no start beyond the window's own.
func periodStart(periodStart *int64) string {
	if periodStart == nil {
		return ""
	}
	return formatDate(*periodStart)
}
