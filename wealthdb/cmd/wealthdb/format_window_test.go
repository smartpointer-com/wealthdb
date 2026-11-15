package main

import (
	"testing"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
)

// A load's window prints as its days, or as "all" for a load that
// re-emitted the whole history.
func TestFormatWindow(t *testing.T) {
	if got := formatWindow(86400, 3*86400); got != "1970-01-02..1970-01-04" {
		t.Errorf("formatWindow(span) = %q", got)
	}
	w := canonical.FullWindow(1)
	if got := formatWindow(w.Start, w.End); got != "all" {
		t.Errorf("formatWindow(full) = %q, want all", got)
	}
}
