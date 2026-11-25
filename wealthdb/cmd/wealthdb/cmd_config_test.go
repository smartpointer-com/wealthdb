package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/config"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/errs"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/silver"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/wizard"
)

// TestConfigNeedsATerminal: a real file on stdin that is no terminal is
// a pipe or a redirect, and the setup refuses it without a write.
func TestConfigNeedsATerminal(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	cfg := filepath.Join(dir, "wealthdb.cfg")
	in, err := os.Create(filepath.Join(dir, "answers"))
	if err != nil {
		t.Fatal(err)
	}
	defer in.Close()
	var so, se bytes.Buffer
	if code := Run([]string{"-c", cfg, "config"}, in, &so, &se); code != errs.ExitUsage {
		t.Errorf("exit %d, want %d; stderr %s", code, errs.ExitUsage, se.String())
	}
	if !strings.Contains(se.String(), "needs an interactive terminal") {
		t.Errorf("stderr = %s", se.String())
	}
	if _, err := os.Stat(cfg); !os.IsNotExist(err) {
		t.Errorf("the config was written: %v", err)
	}
}

// TestConfigUnknownSection: a name that is no section is a bad argument:
// exit 2 with the usage, whether the input is a terminal, a pipe or a
// reader, and before anything is written.
func TestConfigUnknownSection(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	cfg := filepath.Join(dir, "sub", "wealthdb.cfg")
	pipe, err := os.Create(filepath.Join(dir, "answers"))
	if err != nil {
		t.Fatal(err)
	}
	defer pipe.Close()
	for name, in := range map[string]io.Reader{"a pipe": pipe, "a reader": strings.NewReader("q\n")} {
		var so, se bytes.Buffer
		code := Run([]string{"-c", cfg, "config", "bankz"}, in, &so, &se)
		if code != errs.ExitUsage {
			t.Errorf("%s: exit %d, want %d", name, code, errs.ExitUsage)
		}
		for _, want := range []string{`wealthdb: config: no section "bankz" (want one of primary-currency, banks-and-brokers, `,
			", or validate)", "usage: wealthdb [-c <file>] config"} {
			if !strings.Contains(se.String(), want) {
				t.Errorf("%s: stderr lacks %q:\n%s", name, want, se.String())
			}
		}
		if strings.Contains(se.String(), "wealthdb config needs an interactive terminal") || so.Len() != 0 {
			t.Errorf("%s: the section is not checked first:\n%s%s", name, so.String(), se.String())
		}
	}
	if _, err := os.Stat(filepath.Dir(cfg)); !os.IsNotExist(err) {
		t.Errorf("the config folder was made: %v", err)
	}
	// Every name the usage lists is a section, and so is a short name.
	for _, name := range append(wizard.Sections(), "banks", "currency", "model") {
		if !wizard.HasSection(name) {
			t.Errorf("%q is no section", name)
		}
	}
}

// TestConfigValidate: validate needs no terminal, says what is wrong
// with the exit code a script reads, and creates nothing.
func TestConfigValidate(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	cfg := filepath.Join(dir, "wealthdb.cfg")
	in, err := os.Create(filepath.Join(dir, "stdin"))
	if err != nil {
		t.Fatal(err)
	}
	defer in.Close()
	validate := func() (int, string, string) {
		var so, se bytes.Buffer
		code := Run([]string{"-c", cfg, "config", "validate"}, in, &so, &se)
		return code, so.String(), se.String()
	}
	code, _, se := validate()
	if code != errs.ExitUsage || !strings.Contains(se, "holds no setup yet. Run: wealthdb -c "+cfg+" config") {
		t.Errorf("no file: exit %d, %s", code, se)
	}
	if _, err := os.Stat(cfg); !os.IsNotExist(err) {
		t.Errorf("validate made the file: %v", err)
	}
	for body, want := range map[string]int{
		`{"default_currency": "USD"}`:                                       errs.ExitUsage,
		"{\n  \"gold_db\": \"/x/g.db\"\n  \"default_currency\": \"USD\"\n}": 1,
	} {
		if err := os.WriteFile(cfg, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		if code, _, se := validate(); code != want {
			t.Errorf("%s: exit %d, want %d; %s", body, code, want, se)
		} else if want == 1 && (!strings.Contains(se, "the config is not valid: ") || !strings.Contains(se, "(line 3, column 4)")) {
			t.Errorf("%s: %s", body, se)
		}
	}
	gold := filepath.Join(dir, "g.db")
	if err := os.WriteFile(cfg, []byte(`{"gold_db": "`+gold+`", "default_currency": "CHF"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if code, so, se := validate(); code != 0 || so != cfg+" is valid: 0 sources, reports in CHF.\n" {
		t.Errorf("valid: exit %d, %q, %s", code, so, se)
	}
}

// TestConfigThenInitThenStatus walks the wizard end to end on a scratch
// data folder, for the default config and for a setup of its own: the
// config it writes loads, init creates the database in the setup's data
// folder, and the to-do block, status and load name the collect command
// of the source with no data, for the config the run read.
func TestConfigThenInitThenStatus(t *testing.T) {
	pick := ""
	for i, c := range silver.Catalogue() {
		if c.Name == "viac" {
			pick = fmt.Sprint(i + 1)
		}
	}
	answers := strings.Join([]string{"", pick, "", "", "", ""}, "\n") + "\n"
	for _, tc := range []struct {
		name string
		// cfg and root are the config and the data folder its setup
		// reads, under the test's folder; envRoot is WEALTHDB_DATA_ROOT,
		// under it too, or unset when empty.
		cfg, root, envRoot string
		// byEnv: WEALTHDB_CONFIG names the config, and no -c.
		byEnv bool
		// own: the config is not the default one, so every command names
		// it, quoted where the path holds a blank.
		own bool
	}{
		{name: "the default config", cfg: "conf/wealthdb.cfg", root: "data", envRoot: "data"},
		{name: "the default config, no WEALTHDB_DATA_ROOT", cfg: "conf/wealthdb.cfg", root: "xdg/wealthdb"},
		{name: "a setup of its own", cfg: "try/wealthdb.cfg", root: "try", envRoot: "data", own: true},
		{name: "a setup of its own, no WEALTHDB_DATA_ROOT", cfg: "try/wealthdb.cfg", root: "try", own: true},
		{name: "a setup of its own, a blank in the path", cfg: "my setup/wealthdb.cfg", root: "my setup", envRoot: "data", own: true},
		{name: "a setup of its own, named by WEALTHDB_CONFIG", cfg: "try/wealthdb.cfg", root: "try", envRoot: "data", own: true, byEnv: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			cfg, root := filepath.Join(dir, tc.cfg), filepath.Join(dir, tc.root)
			elsewhere := filepath.Join(dir, "data")
			t.Setenv("XDG_CONFIG_HOME", filepath.Join(dir, "conf"))
			t.Setenv("XDG_DATA_HOME", filepath.Join(dir, "xdg"))
			t.Setenv("WEALTHDB_DATA_ROOT", "")
			if tc.envRoot != "" {
				t.Setenv("WEALTHDB_DATA_ROOT", filepath.Join(dir, tc.envRoot))
			}
			t.Setenv("WEALTHDB_LOCALE", "en_US")
			for _, d := range []string{root, elsewhere} {
				if err := os.MkdirAll(d, 0o700); err != nil {
					t.Fatal(err)
				}
			}
			named := []string{"-c", cfg}
			if tc.byEnv {
				t.Setenv("WEALTHDB_CONFIG", cfg)
				named = nil
			}
			args := func(rest ...string) []string { return append(slices.Clone(named), rest...) }
			prog, collect := "wealthdb", "wealthdb-collect"
			if tc.own {
				prog = "wealthdb -c " + shellWord(cfg)
				collect = prog + " collect"
			}

			var so, se bytes.Buffer
			if code := Run(args("config"), strings.NewReader(answers), &so, &se); code != 0 {
				t.Fatalf("config exit %d\nstdout %s\nstderr %s", code, so.String(), se.String())
			}
			text, _ := os.ReadFile(cfg)
			// The paths go through the variable when it stands for the
			// data folder in this config: the environment names the
			// folder, or the config is a setup of its own.
			gold, viac := `"gold_db": "${WEALTHDB_DATA_ROOT}/wealthdb.db"`, `"path": "${WEALTHDB_DATA_ROOT}/viac/viac.db"`
			if !tc.own && tc.envRoot == "" {
				gold = fmt.Sprintf(`"gold_db": %q`, filepath.Join(root, "wealthdb.db"))
				viac = fmt.Sprintf(`"path": %q`, filepath.Join(root, "viac", "viac.db"))
			}
			for _, want := range []string{gold, `"default_currency": "USD"`, viac} {
				if !strings.Contains(string(text), want) {
					t.Errorf("config lacks %s:\n%s", want, text)
				}
			}
			for _, want := range []string{"Data folder:  " + root + "\n",
				"created the database at " + filepath.Join(root, "wealthdb.db"),
				"What to do now", "\n  1.  " + collect + " viac login\n\n", "\n  2.  " + prog + " load -a\n\n"} {
				if !strings.Contains(so.String(), want) {
					t.Errorf("output lacks %q:\n%s", want, so.String())
				}
			}
			if _, err := os.Stat(filepath.Join(root, "wealthdb.db")); err != nil {
				t.Errorf("the database is not in the data folder %s: %v", root, err)
			}
			// A setup of its own leaves the folder the variable names
			// alone.
			if left, _ := os.ReadDir(elsewhere); root != elsewhere && len(left) != 0 {
				t.Errorf("the run wrote into %s, the folder of another setup: %v", elsewhere, left)
			}
			noData := "no data yet. Run: " + collect + " viac login"
			out, errOut, code := run(t, args("status")...)
			if code != 0 || !strings.Contains(out, "[viac] "+noData+"\n") {
				t.Errorf("status: exit %d, want %q\n%s%s", code, noData, out, errOut)
			}
			if out, errOut, code = run(t, args("status", "viac")...); code != 0 || !strings.Contains(out, "\n  "+noData+"\n") {
				t.Errorf("status viac: exit %d, want %q\n%s%s", code, noData, out, errOut)
			}
			if out, errOut, code = run(t, args("load", "-a")...); code != 0 || !strings.Contains(out, "load: viac: "+noData+"\n") {
				t.Errorf("load -a: exit %d, want %q\n%s%s", code, noData, out, errOut)
			}
			if _, errOut, code = run(t, args("load", "viac")...); code == 0 || !strings.Contains(errOut, "viac: "+noData) {
				t.Errorf("load viac: exit %d, want %q\n%s", code, noData, errOut)
			}
			if tc.own == strings.Contains(so.String()+out+errOut, "wealthdb-collect") {
				t.Errorf("own setup %v, but the collect commands read:\n%s%s%s", tc.own, so.String(), out, errOut)
			}
			// The menu runs now, and leaves the file as it is.
			so.Reset()
			if code := Run(args("config"), strings.NewReader("q\n"), &so, &se); code != 0 {
				t.Fatalf("menu exit %d: %s", code, se.String())
			}
			if after, _ := os.ReadFile(cfg); !bytes.Equal(after, text) || !strings.Contains(so.String(), "Walk through") {
				t.Errorf("menu:\n%s\nfile:\n%s", so.String(), after)
			}
		})
	}
}

// TestPointersBeforeSetup: with no config, an empty one or an unfinished
// one, a command names the setup and exits with the usage code; a
// config refused for another reason keeps the engine's message.
func TestPointersBeforeSetup(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	for _, tc := range []struct {
		name, body string // body "-" is no file at all
		code       int
		want       string
	}{
		{"absent", "-", errs.ExitUsage, "Not set up yet. Run: wealthdb -c "},
		{"empty", "", errs.ExitUsage, "Not set up yet. Run: wealthdb -c "},
		{"unfinished", `{"default_currency": "USD"}`, errs.ExitUsage, "Setup is not finished. Run: wealthdb -c "},
		{"refused", `{"gold_db": "/x/g.db", "default_currency": "usd"}`, 1, "not a 3-letter ISO 4217 code"},
	} {
		cfg := filepath.Join(dir, tc.name+".cfg")
		if tc.body != "-" {
			if err := os.WriteFile(cfg, []byte(tc.body), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		for _, args := range [][]string{{"status"}, {"load", "-a"}, {"holdings", "global"}, {"init"}} {
			_, se, code := run(t, append([]string{"-c", cfg}, args...)...)
			if code != tc.code || !strings.Contains(se, tc.want) {
				t.Errorf("%s: %v: exit %d, stderr %q; want %d and %q", tc.name, args, code, se, tc.code, tc.want)
			}
		}
		// help and version answer whatever the config holds.
		for _, args := range [][]string{{"help"}, {"version"}, {"config-env", "collectors"}} {
			if _, se, code := run(t, append([]string{"-c", cfg}, args...)...); code != 0 {
				t.Errorf("%s: %v: exit %d, stderr %s", tc.name, args, code, se)
			}
		}
		// wealthdb alone shows the first steps before a setup, the
		// listing after.
		_, se, _ := run(t, "-c", cfg)
		// Each step names the config the run read.
		steps := strings.Contains(se, "The first steps:") && strings.Contains(se, "  1. wealthdb -c "+cfg+" config ") &&
			strings.Contains(se, "  2. wealthdb -c "+cfg+" init ") && strings.Contains(se, "  3. wealthdb -c "+cfg+" load -a ") &&
			strings.Contains(se, "Run 'wealthdb help' for everything else.")
		if steps != (tc.name != "refused") {
			t.Errorf("%s: wealthdb alone:\n%s", tc.name, se)
		}
	}
}

func TestConfigEnvCollectors(t *testing.T) {
	t.Parallel()
	so, se, code := run(t, "-c", filepath.Join(t.TempDir(), "none.cfg"), "config-env", "collectors")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, se)
	}
	got := strings.Fields(so)
	if !slices.Contains(got, "viac") || !slices.Contains(got, "svb") || slices.Contains(got, "synthetic") {
		t.Errorf("collectors = %v", got)
	}
	if len(got) != len(silver.Catalogue()) {
		t.Errorf("%d lines for %d collectors", len(got), len(silver.Catalogue()))
	}
}

// TestLoadSkipsWhatHasNoData: for sources gold has never loaded, load
// -a loads what has data and names the collect command of what has
// none, a subsource without data is left out of its source's first
// load, and load of the source alone fails naming it; status says the
// same.
func TestLoadSkipsWhatHasNoData(t *testing.T) {
	t.Parallel()
	cfg := setupCLITest(t)
	dir := filepath.Dir(cfg)
	web := filepath.Join(dir, "schwab-web.db")
	// A kind several collectors write, at a file name none of them uses.
	own := filepath.Join(dir, "statements.db")
	body := fmt.Sprintf(`{"gold_db": %q, "default_currency": "USD", "silver_sources": [
  {"id": "schwab-test", "kind": "schwab", "subsources": [
     {"kind": "schwab-api", "path": %q},
     {"kind": "schwab-web", "path": %q}]},
  {"id": "viac", "kind": "viac", "path": %q},
  {"id": "own", "kind": "fidelity", "path": %q}]}`,
		goldPathFromCfg(cfg), filepath.Join(dir, "schwab.db"), web, filepath.Join(dir, "viac", "viac.db"), own)
	if err := os.WriteFile(cfg, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, se, code := run(t, "-c", cfg, "init"); code != 0 {
		t.Fatalf("init: %s", se)
	}
	// The config is not the default one, so a collect command names it.
	collect := "wealthdb -c " + cfg + " collect"
	so, se, code := run(t, "-c", cfg, "load", "-a")
	if code != 0 {
		t.Fatalf("load -a exit %d\n%s%s", code, so, se)
	}
	for _, want := range []string{
		"load: viac: no data yet. Run: " + collect + " viac login\n",
		"load: schwab-test: schwab-web: no data yet. Run: " + collect + " schwab-web login\n",
		"load: schwab-test: 1 snapshot row(s)",
		// No collector is known for the file: the line names the file.
		"load: own: no data yet; the file " + own + " does not exist\n",
	} {
		if !strings.Contains(so, want) {
			t.Errorf("load -a lacks %q:\n%s", want, so)
		}
	}
	// A beginner's first load into an empty database is no news about
	// earlier cash flow reports: there were none.
	if strings.Contains(so, "FIRST pass") {
		t.Errorf("the first load into an empty gold announced the first pass:\n%s", so)
	}
	_, se, code = run(t, "-c", cfg, "load", "viac")
	if code == 0 || !strings.Contains(se, "viac: no data yet. Run: "+collect+" viac login") {
		t.Errorf("load viac: exit %d, %s", code, se)
	}
	so, _, _ = run(t, "-c", cfg, "status")
	if !strings.Contains(so, "[viac] no data yet. Run: "+collect+" viac login\n") {
		t.Errorf("status:\n%s", so)
	}
	so, _, _ = run(t, "-c", cfg, "status", "viac")
	if !strings.Contains(so, "\n  no data yet. Run: "+collect+" viac login\n") {
		t.Errorf("status viac:\n%s", so)
	}

	// schwab-test is loaded now: its missing feed may have contributed,
	// so it is an error, not data that is not there yet. load -a still
	// loads the others and exits non-zero.
	so, se, code = run(t, "-c", cfg, "load", "-a")
	if code == 0 {
		t.Errorf("load -a with a loaded source's feed missing exit 0\n%s%s", so, se)
	}
	if want := "load: schwab-test: its data file is missing: " + web; !strings.Contains(se, want) {
		t.Errorf("load -a stderr lacks %q:\n%s", want, se)
	}
	if !strings.Contains(so, "load: viac: no data yet.") || !strings.Contains(so, "timing: enrichment") {
		t.Errorf("load -a did not go on with the rest:\n%s", so)
	}
	_, se, code = run(t, "-c", cfg, "load", "schwab-test")
	if code == 0 || !strings.Contains(se, "schwab-test: its data file is missing: "+web) {
		t.Errorf("load schwab-test: exit %d, %s", code, se)
	}
	so, _, _ = run(t, "-c", cfg, "status")
	if !strings.Contains(so, "[schwab] ERROR: its data file is missing: "+web) {
		t.Errorf("status:\n%s", so)
	}
	so, _, _ = run(t, "-c", cfg, "status", "schwab-test")
	if !strings.Contains(so, "  ERROR: its data file is missing: "+web) {
		t.Errorf("status schwab-test:\n%s", so)
	}
}

// TestLoadedSourceWhoseFileIsGone: a source gold has loaded, whose one
// file is gone, fails its load whatever the reason the file is missing,
// and load -a goes on with the others and exits non-zero.
func TestLoadedSourceWhoseFileIsGone(t *testing.T) {
	t.Parallel()
	cfg := setupCLITest(t)
	dir := filepath.Dir(cfg)
	api := filepath.Join(dir, "schwab.db")
	second := filepath.Join(dir, "second.db")
	data, err := os.ReadFile(api)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(second, data, 0o600); err != nil {
		t.Fatal(err)
	}
	body := fmt.Sprintf(`{"gold_db": %q, "default_currency": "USD", "silver_sources": [
  {"id": "one", "kind": "schwab", "path": %q},
  {"id": "two", "kind": "schwab", "path": %q}]}`, goldPathFromCfg(cfg), api, second)
	if err := os.WriteFile(cfg, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, se, code := run(t, "-c", cfg, "init"); code != 0 {
		t.Fatalf("init: %s", se)
	}
	if so, se, code := run(t, "-c", cfg, "load", "-a"); code != 0 {
		t.Fatalf("load -a: %s%s", so, se)
	}
	if err := os.Remove(second); err != nil {
		t.Fatal(err)
	}
	so, se, code := run(t, "-c", cfg, "load", "-a")
	if code == 0 || !strings.Contains(se, "load: two: its data file is missing: "+second) {
		t.Errorf("load -a: exit %d\n%s%s", code, so, se)
	}
	if strings.Contains(so, "no data yet") || !strings.Contains(so, "load: one: up-to-date") {
		t.Errorf("load -a stdout:\n%s", so)
	}
	if so, _, _ := run(t, "-c", cfg, "status"); !strings.Contains(so, "[schwab] ERROR: its data file is missing: "+second) {
		t.Errorf("status:\n%s", so)
	}
}

// TestFeedsNameTheirCollector covers config.SilverSource.Feeds, which
// the pointers read.
func TestFeedsNameTheirCollector(t *testing.T) {
	t.Parallel()
	s := config.SilverSource{ID: "u", Kind: "ubs", Subsources: []config.SilverSubsource{
		{Kind: "ubs-web", Path: "/d/w.db"}, {Kind: "ubs-psn", Path: "/d/p.db"}}}
	got := s.Feeds()
	if len(got) != 2 || got[0].Collector != "ubs-web" || got[1].Collector != "ubs-psn" || got[1].Subsource != "ubs-psn" {
		t.Errorf("feeds = %+v", got)
	}
}

// TestTestModel: a server that answers with an error status refused the
// question; an address where nothing answers did not answer at all.
func TestTestModel(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/missing/") {
			http.Error(w, `{"error": "model not found"}`, http.StatusNotFound)
			return
		}
		fmt.Fprint(w, `{"choices": [{"message": {"content": "OK"}}]}`)
	}))
	defer srv.Close()
	ctx := context.Background()
	if reply, err := testModel(ctx, config.ModelConfig{BaseURL: srv.URL + "/v1", Name: "m"}); err != nil || reply != "OK" {
		t.Errorf("reply %q, %v", reply, err)
	}
	var refused *wizard.ModelRefused
	if _, err := testModel(ctx, config.ModelConfig{BaseURL: srv.URL + "/missing/v1", Name: "m"}); !errors.As(err, &refused) {
		t.Errorf("an error status: %v", err)
	}
	gone := httptest.NewServer(http.NotFoundHandler())
	gone.Close()
	if _, err := testModel(ctx, config.ModelConfig{BaseURL: gone.URL + "/v1", Name: "m"}); err == nil || errors.As(err, &refused) {
		t.Errorf("no server: %v", err)
	}
}

// TestReloadSkipsWhatHasNoData: reload applies the rule of load. A
// source gold has never loaded, with no file, is left out with the same
// line, on both ways of a reload -a, and the reload succeeds. Named
// alone it is an error. A source gold has loaded whose file is gone
// fails the reload, and its rows stay in gold: a rebuild swaps nothing,
// and an in-place reload does not reset it.
func TestReloadSkipsWhatHasNoData(t *testing.T) {
	t.Parallel()
	cfg := setupCLITest(t)
	dir := filepath.Dir(cfg)
	api := filepath.Join(dir, "schwab.db")
	body := fmt.Sprintf(`{"gold_db": %q, "default_currency": "USD", "silver_sources": [
  {"id": "schwab-test", "kind": "schwab", "path": %q},
  {"id": "viac", "kind": "viac", "path": %q}]}`, goldPathFromCfg(cfg), api, filepath.Join(dir, "viac", "viac.db"))
	if err := os.WriteFile(cfg, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, se, code := run(t, "-c", cfg, "init"); code != 0 {
		t.Fatalf("init: %s", se)
	}
	if so, se, code := run(t, "-c", cfg, "load", "-a"); code != 0 {
		t.Fatalf("load -a: %s%s", so, se)
	}
	held := func() string {
		t.Helper()
		so, se, code := run(t, "-c", cfg, "snapshots", "schwab-test")
		if code != 0 || so == "" {
			t.Fatalf("snapshots: exit %d\n%s%s", code, so, se)
		}
		return so
	}
	before := held()
	// The config is not the default one, so the collect command names it.
	collect := "wealthdb -c " + cfg + " collect viac login"
	noData := "reload: viac: no data yet. Run: " + collect + "\n"
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"reload", "-a"}, "reload: rebuilt gold from 1 source(s)"},
		{[]string{"reload", "-a", "--in-place"}, "reload: schwab-test: reset + 1 snapshot row(s)"},
		{[]string{"reload", "schwab-test", "viac"}, "reload: schwab-test: reset + 1 snapshot row(s)"},
	} {
		so, se, code := run(t, append([]string{"-c", cfg}, tc.args...)...)
		if code != 0 || !strings.Contains(so, noData) || !strings.Contains(so, tc.want) {
			t.Errorf("%v: exit %d, want 0 with %q and %q\n%s%s", tc.args, code, noData, tc.want, so, se)
		}
		if strings.Contains(se, "unable to open") {
			t.Errorf("%v: the missing file reached the adapter:\n%s", tc.args, se)
		}
		if got := held(); got != before {
			t.Errorf("%v changed what gold holds of the source with data:\n%s\nwas\n%s", tc.args, got, before)
		}
	}
	_, se, code := run(t, "-c", cfg, "reload", "viac")
	if code == 0 || !strings.Contains(se, "viac: no data yet. Run: "+collect) {
		t.Errorf("reload viac: exit %d, %s", code, se)
	}

	// The loaded source loses its file: every reload of it fails naming
	// the file, and gold keeps its rows.
	if err := os.Remove(api); err != nil {
		t.Fatal(err)
	}
	missing := "schwab-test: its data file is missing: " + api
	for _, args := range [][]string{{"reload", "-a"}, {"reload", "-a", "--in-place"}, {"reload", "schwab-test"},
		{"reload", "schwab-test", "viac"}} {
		so, se, code := run(t, append([]string{"-c", cfg}, args...)...)
		if code == 0 || !strings.Contains(se, missing) {
			t.Errorf("%v with the file gone: exit %d, want an error with %q\n%s%s", args, code, missing, so, se)
		}
		if strings.Contains(so, "rebuilt gold") || strings.Contains(so, "schwab-test: reset") {
			t.Errorf("%v went on without the file:\n%s", args, so)
		}
		if got := held(); got != before {
			t.Errorf("%v with the file gone changed gold:\n%s\nwas\n%s", args, got, before)
		}
	}
}

// TestLoadNamesWhatTheConfigLeavesOut: load -a says which collected data
// the config does not name, for a collector it does name, and the
// command that adds it, for the config the load read. The data is
// looked for in the data folder of that config's setup: the folder
// WEALTHDB_DATA_ROOT names for the default config, and the config's own
// folder for any other, whatever the variable says.
func TestLoadNamesWhatTheConfigLeavesOut(t *testing.T) {
	for _, tc := range []struct {
		name, cfg, root string
		own             bool
	}{
		{"the default config", "conf/wealthdb.cfg", "data", false},
		{"a setup of its own", "my setup/wealthdb.cfg", "my setup", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			cfg, root := filepath.Join(dir, tc.cfg), filepath.Join(dir, tc.root)
			t.Setenv("XDG_CONFIG_HOME", filepath.Join(dir, "conf"))
			t.Setenv("WEALTHDB_DATA_ROOT", filepath.Join(dir, "data"))
			// One linked login in the config, with no data yet; another
			// collected beside it that the config does not name, and a
			// third in the folder the variable names.
			for _, f := range []string{filepath.Join(root, "plaid", "second", "second.db"),
				filepath.Join(dir, "data", "plaid", "third", "third.db")} {
				if err := os.MkdirAll(filepath.Dir(f), 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(f, nil, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.MkdirAll(filepath.Dir(cfg), 0o700); err != nil {
				t.Fatal(err)
			}
			body := `{"gold_db": "${WEALTHDB_DATA_ROOT}/wealthdb.db", "default_currency": "USD", "silver_sources": [
  {"id": "first", "kind": "plaid", "path": "${WEALTHDB_DATA_ROOT}/plaid/first/first.db"}]}`
			if err := os.WriteFile(cfg, []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, se, code := run(t, "-c", cfg, "init"); code != 0 {
				t.Fatalf("init: %s", se)
			}
			if _, err := os.Stat(filepath.Join(root, "wealthdb.db")); err != nil {
				t.Errorf("the database is not in the data folder %s: %v", root, err)
			}
			so, se, code := run(t, "-c", cfg, "load", "-a")
			if code != 0 {
				t.Fatalf("load -a: exit %d\n%s%s", code, so, se)
			}
			wants := []string{
				"load: first: no data yet. Run: wealthdb-collect plaid link --item first\n",
				"load: collected, but not in the config, so not loaded: Plaid (linked banks): second, Plaid (linked banks): third\n",
				"load: to add it, run: wealthdb config banks\n",
			}
			if tc.own {
				prog := "wealthdb -c '" + cfg + "'"
				wants = []string{
					"load: first: no data yet. Run: " + prog + " collect plaid link --item first\n",
					"load: collected, but not in the config, so not loaded: Plaid (linked banks): second\n",
					"load: to add it, run: " + prog + " config banks\n",
				}
			}
			for _, want := range wants {
				if !strings.Contains(so, want) {
					t.Errorf("load -a lacks %q:\n%s", want, so)
				}
			}
			// A load of one source says nothing of the others.
			if so, _, _ := run(t, "-c", cfg, "load", "first"); strings.Contains(so, "not in the config") {
				t.Errorf("load first:\n%s", so)
			}
		})
	}
}

// TestPointersQuoteTheConfigPath: a config path that holds a blank or a
// single quote is quoted in every command a message names, so the
// command works when pasted into a shell.
func TestPointersQuoteTheConfigPath(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	for name, quoted := range map[string]string{
		"my setup": "'" + filepath.Join(dir, "my setup", "wealthdb.cfg") + "'",
		"it's":     "'" + filepath.Join(dir, `it'\''s`, "wealthdb.cfg") + "'",
	} {
		cfg := filepath.Join(dir, name, "wealthdb.cfg")
		// Before a setup: the pointer of a report, and the first steps.
		_, se, code := run(t, "-c", cfg, "status")
		if want := "Not set up yet. Run: wealthdb -c " + quoted + " config"; code != errs.ExitUsage || !strings.Contains(se, want) {
			t.Errorf("%s: status: exit %d, want %q: %s", name, code, want, se)
		}
		_, se, _ = run(t, "-c", cfg)
		for _, rest := range []string{"config", "init", "load -a"} {
			if want := ". wealthdb -c " + quoted + " " + rest + " "; !strings.Contains(se, want) {
				t.Errorf("%s: the first steps lack %q:\n%s", name, want, se)
			}
		}
		// The setup itself: a stop names the command that goes on.
		var so, errb bytes.Buffer
		if code := Run([]string{"-c", cfg, "config"}, strings.NewReader("q\n"), &so, &errb); code != 0 {
			t.Fatalf("%s: config: exit %d: %s", name, code, errb.String())
		}
		if want := "To go on with the setup, run this command:\n\n    wealthdb -c " + quoted + " config\n"; !strings.Contains(so.String(), want) {
			t.Errorf("%s: the setup's stop lacks %q:\n%s", name, want, so.String())
		}
		// After a setup with no database: the init a report names.
		body := fmt.Sprintf(`{"gold_db": %q, "default_currency": "USD"}`, filepath.Join(dir, name, "wealthdb.db"))
		if err := os.WriteFile(cfg, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		_, se, code = run(t, "-c", cfg, "holdings", "global")
		if want := "Run 'wealthdb -c " + quoted + " init' first"; code != errs.ExitMissingDB || !strings.Contains(se, want) {
			t.Errorf("%s: holdings: exit %d, want %q: %s", name, code, want, se)
		}
	}
}
