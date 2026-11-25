package main

import (
	"context"
	"database/sql"
	"flag"
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/config"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/errs"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/output"
)

// A report family is a command of the CLI and a tool of the MCP server:
// its views, how it reads its dates, and how a front-end's options
// become a request. A view is one row: how its report is built and
// everything the front-ends need to know of it. Each report family is
// declared beside its registries in cmd_<family>.go; the diagnostics'
// in mcp_tools.go.

// view is one view of a family.
type view struct {
	name string
	// build is the view's report, the runner the CLI and the MCP server
	// share.
	build func(cfg *config.Config, req request) *report
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
	// window is how a view that looks forward reads its FROM and TO (from
	// and to on MCP): its window defaults to the horizon after the current
	// month and may reach into the future (parseForwardWindow). nil for a
	// view that does not look forward.
	window windowParser
	// checkWindow refuses a window the view cannot read (the bridge's
	// whole months); today is the end of the current day.
	checkWindow func(from, to, today int64) error
	// benchmark is the dimension of benchmark_exposure the view's rows
	// read; "" where no benchmark applies.
	benchmark string
	// takes are the family's options that only some of its views take,
	// by their MCP names, where this view takes them.
	takes []string
	// options replace the family's options of the same name on this view
	// (spending fees' period); refuses are the family's options the view
	// does not take, which the CLI registers to refuse by name. refuseWhy
	// follows an MCP refusal of one of them, or of the currency of a view
	// in its own currencies (ownCurrency).
	options   []option
	refuses   []string
	refuseWhy string
	// fixed marks a view of one fixed shape (holdings global): the CLI
	// takes no -C and no -F there.
	fixed bool
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

// forward reports whether the view looks forward: it reads its own
// window.
func (v *view) forward() bool { return v.window != nil }

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
	// options are the family's options, in the order the CLI registers
	// them.
	options []option
	// settle reads the options in into req, with every rule of which
	// view takes which option, and notes what it chose. On the CLI it
	// runs before the window and the config are read, on MCP after the
	// dates. fam is the family itself, whose views its refusals name.
	settle func(fam *family, in optionSource, req *request, notes *[]string) error
	// after reads the options that need the config, on the CLI once it
	// is loaded (cfg), on MCP right after settle (cfg nil).
	after func(in optionSource, cfg *config.Config, req *request) error
	// configFirst says the CLI loads the config before it reads the
	// window, whose forward views need the config's horizon. So a call
	// with a bad date and a bad config or format reports the config's
	// error here and the date's in the other families.
	configFirst bool
	// viewPrefix says the CLI's errors name the view alone ("positions:
	// ..."), as each view's usage line does (holdings).
	viewPrefix bool
	// usage is the CLI's -h text; a view's own replaces it.
	usage func() string
	// skeletons writes the declaration records -f ndjson asks the
	// coverage view for, nil where the family has none.
	skeletons func(ctx context.Context, open func() (*sql.DB, error), asOf int64, ccy string, w io.Writer) error
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
	return fam.namesWhere((*view).forward)
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
func (fam *family) build(cfg *config.Config, req request) *report {
	v := fam.view(req.view)
	if v == nil {
		panic(fmt.Sprintf("%s: no view %q", fam.name, req.view))
	}
	return v.build(cfg, req)
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

// reportFamilies are the report families, in describe's order: where
// columnElsewhere looks for a column a view lacks. They are the tools
// whose family reads a window or a date; the diagnostics read neither.
var reportFamilies = func() []*family {
	var out []*family
	for _, t := range toolSpecs {
		if t.family != nil && t.family.window != noWindow {
			out = append(out, t.family)
		}
	}
	return out
}()

// columnSets is the tail of a family's usage text for views: each
// view's columns, then each view's default set, one line each, the
// view's name padded to pad. req is the request the columns are listed
// at (currency CCY); columns names a view's columns where its line says
// more than the list (gains' sources: "silver_source, then summary's").
func columnSets(fam *family, pad int, views []string, req request, columns map[string]string) (available, defaults string) {
	var a, d strings.Builder
	for _, name := range views {
		req.view = name
		rep := fam.build(nil, req)
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

// ---- options -------------------------------------------------------------

// optKind is how an option's value reads.
type optKind int

const (
	optText  optKind = iota // a string, checked against choices where it has them
	optFlag                 // a boolean
	optInt                  // an integer
	optOnOff                // on | off on the CLI, a boolean on MCP
)

// option is one option of a family: its name on MCP, its spellings on
// the CLI, how it reads and what it is when not given.
type option struct {
	// name is the MCP parameter and the name the rules use.
	name string
	// cli are the CLI flag's names, the short one first; nil for an
	// option MCP alone has.
	cli  []string
	kind optKind
	// choices are a text option's values, checked by choose.
	choices []string
	// def is the option's value when not given: the CLI's, then MCP's.
	def [2]string
	// only says the CLI registers the flag on the views that take it
	// alone; on the others it is unknown.
	only bool
}

// optionsFor are v's options: the family's, as v replaces them.
func (fam *family) optionsFor(v *view) []option {
	out := slices.Clone(fam.options)
	for _, o := range v.options {
		for i := range out {
			if out[i].name == o.name {
				out[i] = o
			}
		}
	}
	return out
}

// optionSource is a front end's options for one call: the CLI's flags
// or an MCP call's arguments.
type optionSource interface {
	// has reports whether the option was given.
	has(name string) bool
	// str is the option's value, else this front end's default.
	str(name string) string
	// on is a boolean option's value, else this front end's default.
	on(name string) bool
	// num is an integer option's value, 0 when not given.
	num(name string) int
	// spell is the option's name as this front end writes it: --period
	// on the CLI, period on MCP.
	spell(name string) string
	// cli reports whether the source is the CLI.
	cli() bool
	// opt is the option of that name.
	opt(name string) option
	// series reads period and the window into req.series, a series of a
	// view among views, with the front end's refusals (parseSeries,
	// seriesArg).
	series(cmd string, views []string, req *request, notes *[]string) error
}

// choose is a text option's value, refused where its choices do not
// hold it. An MCP enum has mostly admitted only a choice already; MCP's
// boolean for an on | off option has no other value.
func choose(in optionSource, cmd, name string) (string, error) {
	v, o := in.str(name), in.opt(name)
	if o.kind == optOnOff && !in.cli() {
		return v, nil
	}
	return v, checkChoice(cmd, o.cli[0], v, o.choices...)
}

// refuseOptions refuses a call that sets one of names: the options of
// the family's other views that view does not take. why follows the
// refusal.
func refuseOptions(in optionSource, cmd, view string, names []string, why string) error {
	for _, name := range names {
		if in.has(name) {
			return errs.Newf(errs.ExitUsage, "%s: %s takes no %s%s", cmd, view, in.spell(name), why)
		}
	}
	return nil
}

// optionOf is the option of that name; the zero option for none.
func optionOf(opts []option, name string) option {
	for _, o := range opts {
		if o.name == name {
			return o
		}
	}
	return option{name: name}
}

// cliOptions are the CLI's flags of one view, parsed into fs at now.
type cliOptions struct {
	fs   *flag.FlagSet
	opts []option
	now  time.Time
}

// names are the CLI spellings of the option: the family's, or the
// output currency's, which every report registers (currencyFlags).
func (c cliOptions) names(name string) []string {
	if name == "currency" {
		return currencyFlags
	}
	return optionOf(c.opts, name).cli
}

func (c cliOptions) has(name string) bool {
	n := c.names(name)
	return len(n) > 0 && isSet(c.fs, n...)
}

func (c cliOptions) str(name string) string {
	n := c.names(name)
	if len(n) == 0 {
		return ""
	}
	if f := c.fs.Lookup(n[0]); f != nil {
		return f.Value.String()
	}
	return ""
}

func (c cliOptions) on(name string) bool {
	if optionOf(c.opts, name).kind == optOnOff {
		return c.str(name) == "on"
	}
	return c.str(name) == "true"
}

func (c cliOptions) num(name string) int {
	n, _ := strconv.Atoi(c.str(name))
	return n
}

func (c cliOptions) spell(name string) string {
	n := c.names(name)
	if len(n) == 0 {
		return "--" + name
	}
	if len(n[0]) == 1 {
		return "-" + n[0]
	}
	return "--" + n[0]
}

func (cliOptions) cli() bool { return true }

func (c cliOptions) opt(name string) option { return optionOf(c.opts, name) }

func (c cliOptions) series(cmd string, views []string, req *request, _ *[]string) error {
	var err error
	req.series, err = parseSeries(cmd, c.fs, req.view, c.str("period"), views, c.now)
	return err
}

// register declares opts on fs, the views' own and those the view
// takes. The CLI prints its usage texts, never the flags', so a flag
// carries no description.
func (c cliOptions) register(v *view) {
	for _, o := range c.opts {
		if len(o.cli) == 0 || slices.Contains(v.refuses, o.name) || o.only && !v.takesOption(o.name) {
			continue
		}
		switch o.kind {
		case optFlag:
			p := c.fs.Bool(o.cli[0], o.def[0] == "true", "")
			for _, alias := range o.cli[1:] {
				c.fs.BoolVar(p, alias, o.def[0] == "true", "")
			}
		case optInt:
			n, _ := strconv.Atoi(o.def[0])
			p := c.fs.Int(o.cli[0], n, "")
			for _, alias := range o.cli[1:] {
				c.fs.IntVar(p, alias, n, "")
			}
		default:
			p := c.fs.String(o.cli[0], o.def[0], "")
			for _, alias := range o.cli[1:] {
				c.fs.StringVar(p, alias, o.def[0], "")
			}
		}
	}
}

// mcpOptions are an MCP call's arguments.
type mcpOptions struct {
	a    *toolArgs
	opts []option
}

func (m mcpOptions) has(name string) bool { return m.a.has(name) }

func (m mcpOptions) str(name string) string {
	if v := m.a.str(name); v != "" {
		return v
	}
	return optionOf(m.opts, name).def[1]
}

func (m mcpOptions) on(name string) bool {
	return m.a.flag(name, optionOf(m.opts, name).def[1] == "true")
}

func (m mcpOptions) num(name string) int {
	n, _ := m.a.integer(name)
	return n
}

func (mcpOptions) spell(name string) string { return name }

func (mcpOptions) cli() bool { return false }

func (m mcpOptions) opt(name string) option { return optionOf(m.opts, name) }

func (m mcpOptions) series(cmd string, views []string, req *request, notes *[]string) error {
	return seriesArg(cmd, views, m.a, req, notes)
}

// settleMCP settles an MCP call's options for req's view: the options
// the view does not take refused first, as the CLI refuses them while it
// parses, then the family's settle, then its after.
func (fam *family) settleMCP(a *toolArgs, req *request, notes *[]string) error {
	if fam.settle == nil {
		return nil
	}
	v := fam.view(req.view)
	in := mcpOptions{a: a, opts: fam.optionsFor(v)}
	if err := fam.refuseMCP(in, v); err != nil {
		return err
	}
	if err := fam.settle(fam, in, req, notes); err != nil {
		return err
	}
	if fam.after == nil {
		return nil
	}
	return fam.after(in, nil, req)
}

// refuseMCP refuses the options of an MCP call that v does not take:
// one only its sibling views take (option.only), then the currency of a
// view in its own currencies and those v refuses, with v.refuseWhy.
func (fam *family) refuseMCP(in mcpOptions, v *view) error {
	for _, o := range in.opts {
		if o.only && !v.takesOption(o.name) && in.has(o.name) {
			return fmt.Errorf("%s: %s belongs to view=%s", fam.name, o.name, andList(fam.taking(o.name)))
		}
	}
	refused := v.refuses
	if v.ownCurrency != "" {
		refused = append([]string{"currency"}, refused...)
	}
	return refuseOptions(in, fam.name, v.name, refused, v.refuseWhy)
}

// defaultRequest is a request for view with every option at its MCP
// default, its dates an empty call's on the day now: what describe lists
// a view's columns from, and what the view walk and the derived carries
// read a view's columns from. An empty call settles.
func (fam *family) defaultRequest(view, currency string, now time.Time, horizon int) request {
	req := request{view: view, currency: currency}
	empty := &toolArgs{values: map[string]any{}}
	var notes []string
	if resolveDates(fam, empty, now, horizon, &req) == nil {
		_ = fam.settleMCP(empty, &req, &notes)
	}
	return req
}

// ---- the CLI's run -------------------------------------------------------

// runReport is a report view on the CLI: its flags, the options its
// family settles, the window and the config in the family's order, then
// the report, written with -C, -F, -f and -p. A view in an output
// currency first says on stderr which currencies gold holds amounts in
// that do not convert to it (openGoldForReport): the report prints as
// it does without the line, and leaves those amounts out.
func runReport(ctx context.Context, g globalFlags, fam *family, view string, args []string, stdout, stderr io.Writer) error {
	v := fam.view(view)
	name, prefix := "wealthdb "+fam.name, fam.name
	if view != "" {
		name += " " + view
	}
	if fam.viewPrefix {
		prefix = view
	}
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	now := g.clock()
	in := cliOptions{fs: fs, opts: fam.optionsFor(v), now: now}
	in.register(v)
	refused := refuseFlags(fs, fam.name, view, v.refuses)
	rf := registerReportFlags(fs, !v.fixed)
	usage := fam.usage
	if v.usage != nil {
		usage = v.usage
	}
	fs.Usage = func() { fmt.Fprintln(stderr, usage()) }
	if err := parseFlags(fs, args, prefix); err != nil {
		return err
	}
	if err := refused(); err != nil {
		return err
	}
	skeletons := fam.skeletons != nil && *rf.format == skeletonFormat
	if skeletons {
		if err := checkSkeletonFlags(fam.name, fs, view, in.str("period"), *rf.privacy); err != nil {
			return err
		}
		// The shared parser knows the table formats only; the skeletons
		// are written below.
		*rf.format = string(output.FormatTable)
	}
	req := request{view: view}
	var notes []string
	if err := fam.settle(fam, in, &req, &notes); err != nil {
		return err
	}
	var fmtChoice output.Format
	var cfg *config.Config
	resolve := func() (err error) {
		fmtChoice, cfg, req.currency, err = rf.resolve(g, prefix)
		return err
	}
	window := func() error { return fam.readWindow(in, v, prefix, now, cfg, &req) }
	steps := []func() error{window, resolve}
	if fam.configFirst {
		steps = []func() error{resolve, window}
	}
	for _, step := range steps {
		if err := step(); err != nil {
			return err
		}
	}
	if fam.after != nil {
		if err := fam.after(in, cfg, &req); err != nil {
			return err
		}
	}
	req.categories = overrideCategories(cfg)
	open := func() (*sql.DB, error) { return openGoldForRead(g, cfg) }
	if skeletons {
		return fam.skeletons(ctx, open, req.asOf, req.currency, stdout)
	}
	out := rf.output(fmtChoice, fam, req, cfg, now)
	out.prefix = prefix
	if fam.currency && v.ownCurrency == "" {
		open = func() (*sql.DB, error) { return openGoldForReport(ctx, g, cfg, req.currency, stderr) }
	}
	return writeReport(ctx, fam.build(cfg, req), out, open, stdout)
}

// readWindow reads the CLI's window off in into req under the family's
// rule: the as-of date (-d), or FROM [TO] with the family's default, the
// view's own parser for a forward view. A trailing-year family reads
// today too: its forward views and the in_progress tag read it.
func (fam *family) readWindow(in cliOptions, v *view, prefix string, now time.Time, cfg *config.Config, req *request) error {
	fs := in.fs
	var err error
	switch fam.window {
	case asOfDate:
		// A family with series reads the positionals with --period.
		if fam.seriesViews() == nil {
			if err := noPositionals(fs, prefix); err != nil {
				return err
			}
		}
		if req.asOf, err = parseAsOf(in.str("as_of"), now); err != nil {
			return errs.Newf(errs.ExitUsage, "%s: %s", prefix, err.Error())
		}
		return nil
	case sinceInception:
		if req.from, req.to, err = parseReturnsWindow(fs.Args(), now); err != nil {
			fs.Usage()
			return errs.Newf(errs.ExitUsage, "%s: %s", prefix, err.Error())
		}
		if v.checkWindow != nil {
			if err := v.checkWindow(req.from, req.to, anchorToDay(now.UTC(), true).Unix()); err != nil {
				return errs.Newf(errs.ExitUsage, "%s: %s", prefix, err.Error())
			}
		}
		if v.takesOption("as_of") {
			if req.asOf, err = viewAsOf(in.str("as_of"), now); err != nil {
				return errs.Newf(errs.ExitUsage, "%s: %s", prefix, err.Error())
			}
		}
		return nil
	case trailingYear:
		horizon := 0
		if cfg != nil {
			horizon = cfg.YieldHorizon()
		}
		req.asOf = anchorToDay(now.UTC(), true).Unix()
		req.from, req.to, err = reportWindow(fs, prefix, now, horizon, v.window)
		return err
	case pastMonth:
		if req.from, req.to, err = parseDateRange(fs.Args(), now); err != nil {
			fs.Usage()
			return errs.Newf(errs.ExitUsage, "%s: %s", prefix, err.Error())
		}
	}
	return nil
}

// routeReport is a report command: `wealthdb <family> <view> [args]`,
// or the family's one shape.
func routeReport(fam *family) func(context.Context, globalFlags, []string, io.Reader, io.Writer, io.Writer) error {
	return func(ctx context.Context, g globalFlags, subargs []string, _ io.Reader, stdout, stderr io.Writer) error {
		if fam.oneShape() {
			return runReport(ctx, g, fam, "", subargs, stdout, stderr)
		}
		view, rest, err := routeView(fam.name, subargs, fam.names(), fam.usage, stderr)
		if err != nil {
			return err
		}
		return runReport(ctx, g, fam, view, rest, stdout, stderr)
	}
}
