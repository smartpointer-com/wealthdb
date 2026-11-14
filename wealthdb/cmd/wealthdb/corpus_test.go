package main

// The report corpus: what the CLI and the MCP server answer to every
// report, view and option, one file per case, for a diff before and
// after a change. It asserts nothing; the proof is an empty diff -r.
//
// TestReportCorpus is skipped unless WEALTHDB_CORPUS_DIR names an empty
// or missing output directory. Without WEALTHDB_CORPUS_CONFIG it runs
// over the seeded fixtures, one subdirectory each; with it, over that
// config's gold alone, opened read-only. The clock reads fixtureToday,
// or WEALTHDB_CORPUS_TODAY (YYYY-MM-DD) where it is set.
//
// The tests run in the image (wealthdb-test), which sees the module
// directory and the Go cache and no other path, and gets no host
// variable. The output goes under the Go cache, and go test's -exec
// hands the variables in. A run makes some twenty thousand calls;
// -parallel bounds how many fixtures run at once. Before a change,
// over the fixtures:
//
//	out=${XDG_CACHE_HOME:-$HOME/.cache}/wealthdb/go/corpus
//	wealthdb/wealthdb-test -count=1 -timeout 120m -parallel 4 \
//	    -run '^TestReportCorpus$' \
//	    -exec "env WEALTHDB_CORPUS_DIR=$out/before" ./cmd/wealthdb
//
// After it, the same into $out/after, then diff -r $out/before
// $out/after. Over a demo household (make demo), WEALTHDB_GOLD_VERIFY
// names its gold, and wealthdb-go mounts that directory read-only:
//
//	demo=$HOME/wealthdb-demo
//	WEALTHDB_GOLD_VERIFY=$demo/wealthdb.db wealthdb/wealthdb-test -count=1 \
//	    -timeout 120m -run '^TestReportCorpus$' \
//	    -exec "env WEALTHDB_CORPUS_DIR=$out/demo-before WEALTHDB_CORPUS_CONFIG=$demo/wealthdb.cfg" \
//	    ./cmd/wealthdb
//
// Over real gold the files hold real data: they are written outside the
// tree and never committed.

import (
	"bytes"
	"database/sql"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/config"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/gold"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/version"
)

// corpusFixtures are the seeded worlds the corpus runs over without a
// config: those of TestMCPMatchesCLI, the benchmark, and the typed
// names privacy pseudonymizes.
var corpusFixtures = []struct {
	name  string
	setup func(*testing.T) string
}{
	{"spending", func(t *testing.T) string { return withFeeLine(t, setupSpendingGold(t)) }},
	{"income", setupIncomeGold},
	{"cashflow", func(t *testing.T) string { return withResolutions(t, setupCashflowGold(t)) }},
	{"returns", setupReturnsGold},
	{"gains", setupGainsGold},
	{"exposure", func(t *testing.T) string { return setupExposureGold(t) }},
	{"yield", setupYieldGold},
	{"benchmark", setupBenchmarkGold},
	{"typedlabel", setupTypedLabelGold},
}

func TestReportCorpus(t *testing.T) {
	out := os.Getenv("WEALTHDB_CORPUS_DIR")
	if out == "" {
		t.Skip("set WEALTHDB_CORPUS_DIR to write the report corpus")
	}
	if cfg := os.Getenv("WEALTHDB_CORPUS_CONFIG"); cfg != "" {
		reportCorpus(t, cfg, out)
		return
	}
	for _, fx := range corpusFixtures {
		t.Run(fx.name, func(t *testing.T) {
			t.Parallel()
			reportCorpus(t, fx.setup(t), filepath.Join(out, fx.name))
		})
	}
}

// corpus is one run of reportCorpus: the config it reads, the directory
// it writes, and the clock both front ends read.
type corpus struct {
	t       *testing.T
	cfgPath string
	cfg     *config.Config
	dir     string
	now     func() time.Time
	mask    *strings.Replacer
	written map[string]bool
	// benchmark is the first benchmark gold holds, the --benchmark value.
	benchmark string
	// usages are the -h texts the help cases hold, longest first: a
	// refusal that prints one again names its case instead.
	usages []usageText
}

type usageText struct{ text, name string }

// reportCorpus writes one file per case into dir, which must be empty or
// missing, and nothing else:
//   - help, help filter, help benchmarks, every listed command's -h and
//     every view's;
//   - every view of every tool's command in each format, with and without
//     -p, with -C default and -C all, as far as its usage offers them;
//   - every view with each value of every option its family's usage
//     names, so an option a view does not take shows its refusal;
//   - per report family, two calls with two errors each;
//   - per view, -p with and without a -F that keeps every row;
//   - on both MCP endpoints, every view of every tool as csv, one
//     filtered and one paged call per tool, and every describe topic.
//
// The cases come from the tool list and the usage texts, so a view or
// an option added later is covered by itself. Every CLI call runs with
// -r: the corpus never writes the gold it reads.
func reportCorpus(t *testing.T, cfgPath, dir string) {
	t.Helper()
	if entries, err := os.ReadDir(dir); err == nil && len(entries) > 0 {
		t.Fatalf("corpus: %s is not empty", dir)
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("corpus: %v", err)
	}
	today := fixtureToday
	if day := os.Getenv("WEALTHDB_CORPUS_TODAY"); day != "" {
		if today, err = time.Parse(time.DateOnly, day); err != nil {
			t.Fatalf("corpus: WEALTHDB_CORPUS_TODAY: %v", err)
		}
		today = today.Add(12 * time.Hour)
	}
	c := &corpus{t: t, cfgPath: cfgPath, cfg: cfg, dir: dir, written: map[string]bool{},
		now: func() time.Time { return today },
		// Longest first: the replacer tries its pairs in order.
		mask: strings.NewReplacer(cfgPath, "<config>", cfg.GoldDB, "<gold>",
			filepath.Dir(cfgPath), "<root>", filepath.Dir(cfg.GoldDB), "<root>", version.String(), "<version>")}
	c.benchmark = firstBenchmark(t, cfg.GoldDB)
	tools := newTestMCP(cfgPath, false).tools()
	c.help(tools)
	for _, tool := range tools {
		if tool.family != nil {
			c.command(tool)
		}
	}
	c.twoErrors(tools)
	for _, privacy := range []bool{false, true} {
		c.mcp(privacy)
	}
}

// firstBenchmark is the first benchmark by name that the gold at path
// holds, or "" where it holds none: the config names built-in ones a
// fixture's gold never synced.
func firstBenchmark(t *testing.T, path string) string {
	t.Helper()
	db, err := gold.Open(path, gold.ModeReadOnly)
	if err != nil {
		t.Fatalf("corpus: %v", err)
	}
	defer db.Close()
	var name sql.NullString
	if err := db.QueryRow(`SELECT min(benchmark) FROM benchmark_funds`).Scan(&name); err != nil {
		t.Fatalf("corpus: %v", err)
	}
	return name.String
}

// write stores one case under name, a slash-separated path. A name met
// twice is a fault of the enumeration, never an overwrite.
func (c *corpus) write(name, text string) {
	c.t.Helper()
	if c.written[name] {
		c.t.Fatalf("corpus: case %s written twice", name)
	}
	c.written[name] = true
	path := filepath.Join(c.dir, filepath.FromSlash(name)+".txt")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		c.t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(c.mask.Replace(text)), 0o644); err != nil {
		c.t.Fatal(err)
	}
}

// cli runs one command line on the corpus's config, read-only, writes
// its stdout, stderr and exit code as case name, and returns stdout,
// stderr and the code.
func (c *corpus) cli(name string, args ...string) (string, string, int) {
	c.t.Helper()
	var so, se bytes.Buffer
	code := runAt(c.now, append([]string{"-c", c.cfgPath, "-r"}, args...), nil, &so, &se)
	stderr := se.String()
	for _, u := range c.usages {
		stderr = strings.ReplaceAll(stderr, u.text, "<the text of "+u.name+">\n")
	}
	c.write(name, fmt.Sprintf("$ wealthdb %s\nexit %d\n--- stdout\n%s--- stderr\n%s",
		strings.Join(quoteArgs(args), " "), code, so.String(), stderr))
	return so.String(), se.String(), code
}

// helpCase writes a help case and keeps its text for the cases after it.
func (c *corpus) helpCase(name string, args ...string) {
	c.t.Helper()
	_, se, _ := c.cli(name, args...)
	if len(se) < 200 || slices.ContainsFunc(c.usages, func(u usageText) bool { return u.text == se }) {
		return
	}
	c.usages = append(c.usages, usageText{se, name})
	slices.SortStableFunc(c.usages, func(a, b usageText) int { return len(b.text) - len(a.text) })
}

func quoteArgs(args []string) []string {
	out := make([]string, len(args))
	for i, a := range args {
		out[i] = a
		if a == "" || strings.ContainsAny(a, " \t'\"*$<>|") {
			out[i] = strconv.Quote(a)
		}
	}
	return out
}

// segment is one part of a case name: a view, a flag or a value, kept
// legible and safe as a file name.
func segment(s string) string {
	if s == "" {
		return "_"
	}
	return strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', strings.ContainsRune("._+-", r):
			return r
		}
		return '_'
	}, s)
}

// help writes the listing, the two help topics, every listed command's
// -h and every view's.
func (c *corpus) help(tools []toolSpec) {
	c.helpCase("cli/help/help", "help")
	c.helpCase("cli/help/help_filter", "help", "filter")
	c.helpCase("cli/help/help_benchmarks", "help", "benchmarks")
	for _, h := range commandHelps {
		if _, ok := subcommands[h.name]; ok && !h.hostSide {
			c.helpCase("cli/help/command/"+segment(h.name), h.name, "-h")
		}
	}
	for _, tool := range tools {
		if tool.family == nil || tool.family.names() == nil {
			continue
		}
		for _, v := range tool.family.names() {
			c.helpCase("cli/help/view/"+segment(tool.name)+"_"+segment(v), tool.name, v, "-h")
		}
	}
}

// usageFlag is a flag a usage text names: its spelling, the metavar of
// its value ("" for a switch) and the values the text lists, if any.
type usageFlag struct {
	name    string // with its dashes: -d, --period
	metavar string
	vocab   []string
}

// The synopsis names a flag as [-x VALUE], [--flag a|b] or [--switch];
// a flag listing names it at the start of an indented line, with its
// other spellings before the value: -s, --source ID.
var (
	synopsisFlag = regexp.MustCompile(`(?:^|[\s\[|])(--?[A-Za-z][A-Za-z0-9-]*)(?:[ =]([A-Z][A-Z0-9_-]*|[a-z0-9]+(?:\|[a-z0-9]+)+))?`)
	listingFlag  = regexp.MustCompile(`^\s+((?:--?[A-Za-z][A-Za-z0-9-]*,\s*)*--?[A-Za-z][A-Za-z0-9-]*)(?:[ =]([A-Z][A-Z0-9_-]*))?(?:\s|$)`)
)

// outputFlags are the flags the format matrix and the -F cases cover,
// and -h; the options are every other flag.
var outputFlags = []string{"-f", "--format", "-C", "--columns", "-F", "--filter", "-p", "--privacy", "-h", "--help"}

// usageFlags reads the flags a usage text names: the synopsis, from its
// "usage:" line to the first blank line, then the indented flag
// listing. A flag named in both keeps the first reading with a value.
func usageFlags(text string) []usageFlag {
	var flags []usageFlag
	add := func(f usageFlag) {
		for i := range flags {
			if flags[i].name == f.name {
				if flags[i].metavar == "" && len(flags[i].vocab) == 0 {
					flags[i] = f
				}
				return
			}
		}
		flags = append(flags, f)
	}
	lines := strings.Split(text, "\n")
	start := slices.IndexFunc(lines, func(l string) bool { return strings.HasPrefix(strings.TrimSpace(l), "usage:") })
	if start < 0 {
		return nil
	}
	i := start
	for ; i < len(lines) && strings.TrimSpace(lines[i]) != ""; i++ {
		for _, m := range synopsisFlag.FindAllStringSubmatch(lines[i], -1) {
			f := usageFlag{name: m[1]}
			if strings.Contains(m[2], "|") {
				f.vocab = strings.Split(m[2], "|")
			} else {
				f.metavar = m[2]
			}
			add(f)
		}
	}
	for ; i < len(lines); i++ {
		m := listingFlag.FindStringSubmatch(lines[i])
		if m == nil {
			continue
		}
		for _, name := range strings.Split(m[1], ",") {
			add(usageFlag{name: strings.TrimSpace(name), metavar: m[2]})
		}
	}
	return flags
}

type usageFlagList []usageFlag

func (fs usageFlagList) has(name string) bool {
	return slices.ContainsFunc(fs, func(f usageFlag) bool { return f.name == name })
}

// options are the flags of a usage text that are not output flags.
func options(flags []usageFlag) usageFlagList {
	var out usageFlagList
	for _, f := range flags {
		if !slices.Contains(outputFlags, f.name) {
			out = append(out, f)
		}
	}
	return out
}

// values are the values the corpus gives a flag: the usage's own list,
// else the vocabulary of the tool parameter of the same name, else one
// sample for its metavar. A switch has none.
func (c *corpus) values(tool toolSpec, f usageFlag) []string {
	if len(f.vocab) > 0 {
		return f.vocab
	}
	if f.metavar == "" {
		return nil
	}
	name := strings.ReplaceAll(strings.TrimLeft(f.name, "-"), "-", "_")
	for _, p := range tool.params {
		if p.name == name && len(p.enum) > 0 {
			return p.enum
		}
	}
	switch f.metavar {
	case "DATE", "YYYY-MM-DD":
		// The last month end before today: a day the data reaches.
		now := c.now()
		return []string{time.Date(now.Year(), now.Month(), 0, 0, 0, 0, 0, time.UTC).Format(time.DateOnly)}
	case "CCY":
		return []string{"CHF"}
	case "BP":
		return []string{"-50"}
	case "NAME":
		if c.benchmark != "" {
			return []string{c.benchmark}
		}
	case "ID", "SOURCE":
		if len(c.cfg.SilverSources) > 0 {
			return []string{c.cfg.SilverSources[0].ID}
		}
	}
	return []string{"corpus-" + strings.ToLower(f.metavar)}
}

// cliView is one shape of a tool's command: its arguments before any
// flag the corpus adds, and the usage text its -h prints.
type cliView struct {
	label string
	args  []string
	usage usageFlagList
}

// cliViews are a tool's views as command lines, then the shapes a
// boolean parameter asks for (status --marks).
func (c *corpus) cliViews(tool toolSpec) []cliView {
	var out []cliView
	for _, v := range tool.family.viewsOrOne() {
		args := []string{tool.name}
		if v != "" {
			args = append(args, v)
		}
		var so, se bytes.Buffer
		runAt(c.now, append([]string{"-c", c.cfgPath, "-r"}, append(slices.Clone(args), "-h")...), nil, &so, &se)
		out = append(out, cliView{label: v, args: args, usage: usageFlags(se.String() + so.String())})
	}
	for _, name := range slices.Sorted(maps.Keys(tool.alt)) {
		flag := "--" + strings.ReplaceAll(name, "_", "-")
		out = append(out, cliView{label: name, args: []string{tool.name, flag}, usage: out[0].usage})
	}
	return out
}

// command writes a tool's command: the format matrix and the -F pair of
// every view, then every view with every value of every option.
func (c *corpus) command(tool toolSpec) {
	views := c.cliViews(tool)
	var famOpts usageFlagList
	for _, v := range views {
		for _, f := range options(v.usage) {
			if !famOpts.has(f.name) {
				famOpts = append(famOpts, f)
			}
		}
	}
	for _, v := range views {
		base := "cli/" + segment(tool.name) + "/" + segment(v.label) + "/"
		formats, privacy, cols := []string{""}, []bool{false}, []string{""}
		if v.usage.has("-f") {
			formats = output4
		}
		if v.usage.has("-p") {
			privacy = []bool{false, true}
		}
		if v.usage.has("-C") {
			cols = []string{"default", "all"}
		}
		for _, f := range formats {
			for _, col := range cols {
				for _, p := range privacy {
					args, label := slices.Clone(v.args), "matrix"
					if f != "" {
						args, label = append(args, "-f", f), label+"_f-"+f
					}
					if col != "" {
						args, label = append(args, "-C", col), label+"_C-"+col
					}
					if p {
						args, label = append(args, "-p"), label+"_p"
					}
					c.cli(base+label, args...)
				}
			}
		}
		// Under -p a pseudonym numbers over the rows; -F must not
		// renumber them.
		if v.usage.has("-p") && v.usage.has("-F") {
			args := append(slices.Clone(v.args), "-f", "csv", "-p")
			if so, _, code := c.cli(base+"p_without_F", args...); code == 0 {
				if col := fullColumn(so); col != "" {
					c.cli(base+"p_with_F", append(args, "-F", col+" is not empty")...)
				}
			}
		}
		if _, alt := tool.alt[v.label]; alt {
			continue
		}
		for _, f := range famOpts {
			vals := c.values(tool, f)
			if vals == nil {
				c.cli(base+"opt_"+segment(strings.TrimLeft(f.name, "-")), append(slices.Clone(v.args), f.name)...)
				continue
			}
			for _, val := range vals {
				c.cli(base+"opt_"+segment(strings.TrimLeft(f.name, "-"))+"_"+segment(val),
					append(slices.Clone(v.args), f.name, val)...)
			}
		}
	}
}

var output4 = []string{"table", "csv", "csv_plain", "json"}

// twoErrors writes, per report family, two calls on its first view that
// each hold two errors: a bad date and a bad format, then a bad option
// value and a bad column. Which one a family reports first is surface.
func (c *corpus) twoErrors(tools []toolSpec) {
	for _, fam := range reportFamilies {
		i := slices.IndexFunc(tools, func(t toolSpec) bool { return t.name == fam.name })
		if i < 0 {
			c.t.Fatalf("corpus: no tool for the report family %s", fam.name)
		}
		v := c.cliViews(tools[i])[0]
		base := "cli/" + segment(fam.name) + "/" + segment(v.label) + "/"
		date := []string{"not-a-date"}
		if fam.window == asOfDate {
			date = []string{"-d", "not-a-date"}
		}
		c.cli(base+"errors_date_format", append(append(slices.Clone(v.args), date...), "-f", "xml")...)
		bad := usageFlag{name: "-x"}
		for _, f := range options(v.usage) {
			if len(f.vocab) > 0 || len(c.values(tools[i], f)) > 1 {
				bad = f
				break
			}
		}
		c.cli(base+"errors_option_column", append(slices.Clone(v.args), bad.name, "bogus", "-C", "no_such_column")...)
	}
}

// mcp writes the server's answers on one endpoint: every view of every
// tool as csv with default arguments, a filtered and a paged call per
// tool, and every describe topic.
func (c *corpus) mcp(privacy bool) {
	s := newTestMCP(c.cfgPath, privacy)
	s.chars = defaultMCPChars
	s.now = c.now
	base := "mcp/plain/"
	if privacy {
		base = "mcp/privacy/"
	}
	call := func(name, tool string, args map[string]any) string {
		raw, err := json.Marshal(args)
		if err != nil {
			c.t.Fatal(err)
		}
		text, err := callTool(c.t, s, tool, args)
		result := "--- result\n" + text
		if err != nil {
			result = "--- error\n" + s.scrubPaths(err.Error())
		}
		c.write(name, fmt.Sprintf("call %s %s\n%s\n", tool, raw, result))
		return text
	}
	for _, tool := range s.tools() {
		if tool.family == nil {
			continue
		}
		dir := base + segment(tool.name) + "/"
		var first string
		for _, v := range tool.family.viewsOrOne() {
			args := map[string]any{"format": "csv"}
			if v != "" {
				args["view"] = v
			}
			text := call(dir+"view_"+segment(v), tool.name, args)
			if first == "" {
				first = text
			}
		}
		for _, name := range slices.Sorted(maps.Keys(tool.alt)) {
			call(dir+"view_"+segment(name), tool.name, map[string]any{"format": "csv", name: true})
		}
		// The first condition the tool takes: a column's cell may be no
		// value its filter reads, as "total" is no period.
		conds := rowConditions(first)
		cond := "value is not empty"
		for _, cnd := range conds {
			cond = cnd
			if _, err := callTool(c.t, s, tool.name, map[string]any{"format": "csv", "filter": cnd}); err == nil {
				break
			}
		}
		call(dir+"filtered", tool.name, map[string]any{"format": "csv", "filter": cond})
		call(dir+"paged", tool.name, map[string]any{"format": "csv", "limit": 2, "offset": 1})
	}
	topics := append(append([]string{""}, s.reportToolNames()...), describeTopics...)
	for _, topic := range topics {
		label := topic
		if label == "" {
			label = "overview"
		}
		call(base+"describe/"+segment(label), "describe", map[string]any{"topic": topic})
	}
}

// fullColumn is the first column of a csv output with a value in every
// row, so that a condition it is not empty keeps them all; "" where no
// column has.
func fullColumn(out string) string {
	r := csv.NewReader(strings.NewReader(out))
	r.FieldsPerRecord = -1
	records, err := r.ReadAll()
	if err != nil || len(records) < 2 {
		return ""
	}
	for i, col := range records[0] {
		if !slices.ContainsFunc(records[1:], func(row []string) bool { return i >= len(row) || row[i] == "" }) {
			return col
		}
	}
	return ""
}

// rowConditions are conditions on a csv result's columns, one per
// column in order, that its first row meets; on a result with no row,
// conditions that keep any row with a value.
func rowConditions(result string) []string {
	_, body, _ := strings.Cut(result, "\n")
	r := csv.NewReader(strings.NewReader(body))
	r.FieldsPerRecord = -1
	header, err := r.Read()
	if err != nil {
		return nil
	}
	row, err := r.Read()
	if err != nil || len(row) != len(header) || strings.HasPrefix(row[0], "(no rows") {
		row = nil
	}
	conds := make([]string, len(header))
	for i, col := range header {
		conds[i] = col + " is not empty"
		if row != nil && row[i] != "" {
			conds[i] = col + " = " + row[i]
		}
	}
	return conds
}
