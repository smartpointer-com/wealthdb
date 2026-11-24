package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
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
// data folder: the config it writes loads, init creates the database,
// and status names the collect command of the source with no data.
func TestConfigThenInitThenStatus(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join(dir, "data")
	t.Setenv("WEALTHDB_DATA_ROOT", root)
	t.Setenv("WEALTHDB_LOCALE", "en_US")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	cfg := filepath.Join(dir, "cfg", "wealthdb.cfg")
	pick := ""
	for i, c := range silver.Catalogue() {
		if c.Name == "viac" {
			pick = fmt.Sprint(i + 1)
		}
	}
	answers := strings.Join([]string{"", pick, "", "", "", ""}, "\n") + "\n"
	var so, se bytes.Buffer
	if code := Run([]string{"-c", cfg, "config"}, strings.NewReader(answers), &so, &se); code != 0 {
		t.Fatalf("config exit %d\nstdout %s\nstderr %s", code, so.String(), se.String())
	}
	text, _ := os.ReadFile(cfg)
	for _, want := range []string{`"gold_db": "${WEALTHDB_DATA_ROOT}/wealthdb.db"`, `"default_currency": "USD"`,
		`"path": "${WEALTHDB_DATA_ROOT}/viac/viac.db"`} {
		if !strings.Contains(string(text), want) {
			t.Errorf("config lacks %s:\n%s", want, text)
		}
	}
	for _, want := range []string{"created the database at " + filepath.Join(root, "wealthdb.db"),
		"What to do now", "\n  1.  wealthdb-collect viac login\n\n", "\n  2.  wealthdb load -a\n\n"} {
		if !strings.Contains(so.String(), want) {
			t.Errorf("output lacks %q:\n%s", want, so.String())
		}
	}
	out, errOut, code := run(t, "-c", cfg, "status")
	if code != 0 || !strings.Contains(out, "no data yet. Run: wealthdb-collect viac login") {
		t.Errorf("status: exit %d\n%s%s", code, out, errOut)
	}
	// The menu runs now, and leaves the file as it is.
	so.Reset()
	if code := Run([]string{"-c", cfg, "config"}, strings.NewReader("q\n"), &so, &se); code != 0 {
		t.Fatalf("menu exit %d: %s", code, se.String())
	}
	if after, _ := os.ReadFile(cfg); !bytes.Equal(after, text) || !strings.Contains(so.String(), "Walk through") {
		t.Errorf("menu:\n%s\nfile:\n%s", so.String(), after)
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
		steps := strings.Contains(se, "The first steps:") && strings.Contains(se, "2. wealthdb init") &&
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
	body := fmt.Sprintf(`{"gold_db": %q, "default_currency": "USD", "silver_sources": [
  {"id": "schwab-test", "kind": "schwab", "subsources": [
     {"kind": "schwab-api", "path": %q},
     {"kind": "schwab-web", "path": %q}]},
  {"id": "viac", "kind": "viac", "path": %q}]}`,
		goldPathFromCfg(cfg), filepath.Join(dir, "schwab.db"), web, filepath.Join(dir, "viac", "viac.db"))
	if err := os.WriteFile(cfg, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, se, code := run(t, "-c", cfg, "init"); code != 0 {
		t.Fatalf("init: %s", se)
	}
	so, se, code := run(t, "-c", cfg, "load", "-a")
	if code != 0 {
		t.Fatalf("load -a exit %d\n%s%s", code, so, se)
	}
	for _, want := range []string{
		"load: viac: no data yet. Run: wealthdb-collect viac login",
		"load: schwab-test: schwab-web: no data yet. Run: wealthdb-collect schwab-web login",
		"load: schwab-test: 1 snapshot row(s)",
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
	if code == 0 || !strings.Contains(se, "viac: no data yet. Run: wealthdb-collect viac login") {
		t.Errorf("load viac: exit %d, %s", code, se)
	}
	so, _, _ = run(t, "-c", cfg, "status")
	if !strings.Contains(so, "[viac] no data yet. Run: wealthdb-collect viac login") {
		t.Errorf("status:\n%s", so)
	}
	so, _, _ = run(t, "-c", cfg, "status", "viac")
	if !strings.Contains(so, "no data yet. Run: wealthdb-collect viac login") {
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
