package main

import (
	"fmt"
	"slices"
	"strings"
	"sync"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/config"
)

// A report family is a command of the CLI and a tool of the MCP server:
// its views, how it reads its dates, and how a front-end's options
// become a request. A view is one row: how its report is built and
// everything the front-ends need to know of it. Each family is declared
// beside its registries, in cmd_<family>.go.

// view is one view of a family.
type view struct {
	name string
	// build is the view's report, the runner the CLI and the MCP server
	// share. a holds an MCP call's arguments, which only the diagnostics
	// read; the CLI passes nil.
	build func(cfg *config.Config, req request, a *toolArgs) *report
	// total says how the view's money adds up over its rows, for the
	// total line of a filtered result.
	total totalRule
	// principal is the view's main column: what "value", "amount" and
	// the family's own word mean in a sort. nil where it has none.
	principal func(req request) string
	// side marks a view that is no grain of the others but a table of
	// its own (returns' stated): the coarse-to-fine walks of escalate,
	// finerViews and the help skip it.
	side bool
	// series says --period (period on MCP) makes the view a series.
	series bool
	// forward says the view looks forward: its window defaults to the
	// horizon after the current month and may reach into the future
	// (parseForwardWindow).
	forward bool
	// takes are the family's options that only some of its views take,
	// by their MCP names, where this view takes them.
	takes []string
	// excludes are the named filters the view prints the key column of
	// but does not carry, because the column means something else here;
	// keyAlias names the view's key column where it prints the filter's
	// under another name. carries derives the rest.
	excludes []string
	keyAlias map[string]string
	// ownCurrency is the result header's word for a view with no output
	// currency, its amounts in their own; "" for the others.
	ownCurrency string
	// header names the view's own options in force, after the family's.
	header func(req request) []string
	// usage is the view's own -h text, where it has one (holdings).
	usage func() string
}

// takesOption reports whether the view takes option, one of the
// family's options only some views take.
func (v *view) takesOption(option string) bool { return slices.Contains(v.takes, option) }

// fixedColumn is the principal of a view whose main column does not
// depend on the request.
func fixedColumn(name string) func(request) string {
	return func(request) string { return name }
}

// family is a report command and tool: its views, coarse to fine, and
// what holds for all of them.
type family struct {
	// name is the command's and the tool's name.
	name string
	// views are the family's views, coarse to fine. A tool of one shape
	// has a single view with no name.
	views  []view
	window windowRule
	// currency says the tool takes a currency parameter.
	currency bool
	// narrow is the paging line's advice for cutting a long result.
	narrow string
	// prepare fills the family's options into an MCP call's req once the
	// view is settled, refuses what the CLI refuses, and notes what it
	// chose. fam is the family itself, whose views its refusals name.
	prepare func(fam *family, a *toolArgs, req *request, notes *[]string) error
	// header names the family's options in force, for the result header;
	// a view's own follow.
	header func(req request) []string

	// carried are the named filters each view carries (carries),
	// derived once.
	derive  sync.Once
	carried map[string][]string
}

// windowRule is how a family reads its dates: a window over from/to
// with its CLI default, an as-of date, or none.
type windowRule int

const (
	noWindow windowRule = iota
	asOfDate
	trailingYear   // spending, income, cashflow: the last twelve months
	sinceInception // returns: since the first snapshot
	pastMonth      // transactions: the past 30 days
)

// names are the family's view names, coarse to fine; nil for a tool of
// one shape.
func (fam *family) names() []string {
	if fam.oneShape() {
		return nil
	}
	return fam.viewsOrOne()
}

// oneShape reports whether the family is a tool of one shape: a single
// view with no name.
func (fam *family) oneShape() bool { return len(fam.views) == 1 && fam.views[0].name == "" }

// viewsOrOne is the family's view names, or the one shape ("") of a
// tool that has none.
func (fam *family) viewsOrOne() []string {
	out := make([]string, len(fam.views))
	for i := range fam.views {
		out[i] = fam.views[i].name
	}
	return out
}

// namesWhere are the names of the views keep holds for, in order.
func (fam *family) namesWhere(keep func(*view) bool) []string {
	var out []string
	for i := range fam.views {
		if keep(&fam.views[i]) {
			out = append(out, fam.views[i].name)
		}
	}
	return out
}

// seriesViews are the views a series takes, in order.
func (fam *family) seriesViews() []string {
	return fam.namesWhere(func(v *view) bool { return v.series })
}

// forwardViews are the views that look forward, in order.
func (fam *family) forwardViews() []string {
	return fam.namesWhere(func(v *view) bool { return v.forward })
}

// taking are the views that take option, in order.
func (fam *family) taking(option string) []string {
	return fam.namesWhere(func(v *view) bool { return v.takesOption(option) })
}

// view is the family's view of that name, nil for none. A tool of one
// shape has its one view under any name: the CLI's status --marks names
// its request's view marks.
func (fam *family) view(name string) *view {
	if fam.oneShape() {
		return &fam.views[0]
	}
	for i := range fam.views {
		if fam.views[i].name == name {
			return &fam.views[i]
		}
	}
	return nil
}

// build is req's report: its view's.
func (fam *family) build(cfg *config.Config, req request, a *toolArgs) *report {
	v := fam.view(req.view)
	if v == nil {
		panic(fmt.Sprintf("%s: no view %q", fam.name, req.view))
	}
	return v.build(cfg, req, a)
}

// headerFor names the options in force for req: the family's, then its
// view's.
func (fam *family) headerFor(req request) []string {
	var h []string
	if fam.header != nil {
		h = fam.header(req)
	}
	if v := fam.view(req.view); v != nil && v.header != nil {
		h = append(h, v.header(req)...)
	}
	return h
}

// ladder is the family's views without its side views: the coarse to
// fine order the walks follow.
func (fam *family) ladder() []string {
	return fam.namesWhere(func(v *view) bool { return !v.side })
}

// walk is the views escalate may settle view on: the ladder, or a side
// view alone, which no filter moves off.
func (fam *family) walk(name string) []string {
	if v := fam.view(name); v != nil && v.side {
		return []string{name}
	}
	return fam.ladder()
}

// reportFamilies are the report tools and commands by name, in
// describe's order: where a column a view lacks is printed instead, and
// the family a CLI condition binds with. They are the tools whose family
// reads a window or a date; the diagnostics read neither.
var reportFamilies = func() []reportFamily {
	var out []reportFamily
	for _, t := range toolSpecs {
		if t.family != nil && t.family.window != noWindow {
			out = append(out, reportFamily{t.family.name, t.family})
		}
	}
	return out
}()

// reportFamily is a report family by its name.
type reportFamily struct {
	name string
	fam  *family
}

// columnSets is the tail of a family's usage text for views: each
// view's columns, then each view's default set, one line each, the
// view's name padded to pad. req is the request the columns are listed
// at (currency CCY); columns names a view's columns where its line says
// more than the list (gains' sources: "silver_source, then summary's").
func columnSets(fam *family, pad int, views []string, req request, columns map[string]string) (available, defaults string) {
	var a, d strings.Builder
	for _, name := range views {
		req.view = name
		rep := fam.build(nil, req, nil)
		cols, ok := columns[name]
		if !ok {
			names := make([]string, len(rep.columns))
			for i, c := range rep.columns {
				names[i] = c.name
			}
			cols = strings.Join(names, ", ")
		}
		fmt.Fprintf(&a, "\n  %-*s %s", pad, name, cols)
		fmt.Fprintf(&d, "\n  %-*s %s", pad, name, strings.Join(rep.defaults, ", "))
	}
	return a.String(), d.String()
}
