package main

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The config default follows the XDG Base Directory spec:
// $XDG_CONFIG_HOME when set, ~/.config otherwise (an empty value
// counts as unset, per the spec).
func TestDefaultConfigPathHonorsXDGConfigHome(t *testing.T) {
	t.Setenv("WEALTHDB_CONFIG", "")
	t.Setenv("XDG_CONFIG_HOME", "/xdg/conf")
	if got, want := defaultConfigPath(), filepath.Join("/xdg/conf", "wealthdb.cfg"); got != want {
		t.Errorf("defaultConfigPath() = %q, want %q", got, want)
	}
}

func TestDefaultConfigPathFallsBackToDotConfig(t *testing.T) {
	t.Setenv("WEALTHDB_CONFIG", "")
	t.Setenv("XDG_CONFIG_HOME", "")
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skipf("no home dir: %v", err)
	}
	if got, want := defaultConfigPath(), filepath.Join(home, ".config", "wealthdb.cfg"); got != want {
		t.Errorf("defaultConfigPath() = %q, want %q", got, want)
	}
}

// WEALTHDB_CONFIG names the config file in place of the XDG default,
// as the engine wrapper and the web and mcp scripts read it.
func TestDefaultConfigPathHonorsWealthdbConfig(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", "/xdg/conf")
	t.Setenv("WEALTHDB_CONFIG", "/elsewhere/my.cfg")
	if got, want := defaultConfigPath(), "/elsewhere/my.cfg"; got != want {
		t.Errorf("defaultConfigPath() = %q, want %q", got, want)
	}
}

// A relative path in either variable is made absolute against the
// working directory, as the engine wrapper makes it before it mounts.
func TestDefaultConfigPathIsAbsolute(t *testing.T) {
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("WEALTHDB_CONFIG", "rel/my.cfg")
	if got, want := defaultConfigPath(), filepath.Join(wd, "rel", "my.cfg"); got != want {
		t.Errorf("WEALTHDB_CONFIG relative: defaultConfigPath() = %q, want %q", got, want)
	}
	t.Setenv("WEALTHDB_CONFIG", "")
	t.Setenv("XDG_CONFIG_HOME", "rel/conf")
	if got, want := defaultConfigPath(), filepath.Join(wd, "rel", "conf", "wealthdb.cfg"); got != want {
		t.Errorf("XDG_CONFIG_HOME relative: defaultConfigPath() = %q, want %q", got, want)
	}
	// A path -c names is made absolute and clean the same way.
	for _, flag := range []string{"-c", "--config"} {
		g, rest, err := parseGlobal([]string{flag, "rel/./my.cfg", "status"}, io.Discard)
		if err != nil || len(rest) != 1 {
			t.Fatalf("parseGlobal: %v, rest %v", err, rest)
		}
		if want := filepath.Join(wd, "rel", "my.cfg"); g.ConfigPath != want {
			t.Errorf("%s relative: ConfigPath = %q, want %q", flag, g.ConfigPath, want)
		}
	}
}

// A command a message names carries -c for a config that is not the
// default one, and none for the default.
func TestCommandForNamesTheConfig(t *testing.T) {
	t.Setenv("WEALTHDB_CONFIG", "/x/default.cfg")
	for _, tc := range []struct{ path, rest, want string }{
		{"/x/default.cfg", "init", "wealthdb init"},
		{"/x/default.cfg", "", "wealthdb"},
		{"/x/try.cfg", "load -a", "wealthdb -c /x/try.cfg load -a"},
		{"/x/try.cfg", "", "wealthdb -c /x/try.cfg"},
		// A path the shell would act on is quoted, so the command works
		// when pasted; a plain one stays as it is.
		{"/x/my setup/try.cfg", "init", "wealthdb -c '/x/my setup/try.cfg' init"},
		{"/x/it's/try.cfg", "load -a", `wealthdb -c '/x/it'\''s/try.cfg' load -a`},
		{"/x/a_b-c.d:e@f%g+h=i,j/try.cfg", "init", "wealthdb -c /x/a_b-c.d:e@f%g+h=i,j/try.cfg init"},
	} {
		if got := commandFor(tc.path, tc.rest); got != tc.want {
			t.Errorf("commandFor(%q, %q) = %q, want %q", tc.path, tc.rest, got, tc.want)
		}
	}
	if got := setupCommand("/x/try.cfg"); got != "wealthdb -c /x/try.cfg config" {
		t.Errorf("setupCommand = %q", got)
	}
	for in, want := range map[string]string{
		"plain/path.cfg": "plain/path.cfg",
		"":               "''",
		"a b":            "'a b'",
		"$HOME/x":        "'$HOME/x'",
		"~/x":            "'~/x'",
		"a*b":            "'a*b'",
		`a"b`:            `'a"b'`,
		"a'b'c":          `'a'\''b'\''c'`,
		"é.cfg":          "'é.cfg'",
	} {
		if got := shellWord(in); got != want {
			t.Errorf("shellWord(%q) = %s, want %s", in, got, want)
		}
	}
	// The message of a database that is not there names the init of the
	// same config, and stays a format for the path alone.
	if got, want := fmt.Sprintf(missingGoldMsg("/x/100%.cfg"), "/x/g.db"),
		`gold database "/x/g.db" does not exist. Run 'wealthdb -c /x/100%.cfg init' first (requires write access).`; got != want {
		t.Errorf("missingGoldMsg: %q, want %q", got, want)
	}
}

// The data folder is absolute too, whichever variable names it.
func TestDataRootIsAbsolute(t *testing.T) {
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("WEALTHDB_DATA_ROOT", "rel/data")
	if got, named := dataRoot(); got != filepath.Join(wd, "rel", "data") || !named {
		t.Errorf("WEALTHDB_DATA_ROOT relative: dataRoot() = %q, %v", got, named)
	}
	t.Setenv("WEALTHDB_DATA_ROOT", "")
	t.Setenv("XDG_DATA_HOME", "rel/xdg")
	if got, named := dataRoot(); got != filepath.Join(wd, "rel", "xdg", "wealthdb") || named {
		t.Errorf("XDG_DATA_HOME relative: dataRoot() = %q, %v", got, named)
	}
	// With no home directory either, the folder is still absolute.
	t.Setenv("XDG_DATA_HOME", "")
	t.Setenv("HOME", "")
	if got, named := dataRoot(); got != filepath.Join(wd, ".local", "share", "wealthdb") || named {
		t.Errorf("no home directory: dataRoot() = %q, %v", got, named)
	}
}

// A config's notices go to the command's stderr, once however often
// the command loads the config, and a config without one prints
// nothing.
func TestConfigNoticesPrintOncePerCommand(t *testing.T) {
	t.Parallel()
	const base = `{"gold_db": "/tmp/g.db", "default_currency": "USD",
		"silver_sources": [{"id": "ct", "kind": "cointracking", "path": "/data/ct.db"}]`
	const notice = "config: returns_transfer_matching: window_days and tolerance_pct are ignored"
	for _, tc := range []struct {
		name, block string
		want        int
	}{
		{"knob set", `, "returns_transfer_matching": {"enabled": true, "window_days": 3}}`, 1},
		{"enabled only", `, "returns_transfer_matching": {"enabled": true}}`, 0},
		{"absent", `}`, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cfg := webTestCfg(t, base+tc.block)
			var out, errb bytes.Buffer
			if code := Run([]string{"-c", cfg, "config-env", "sources"}, nil, &out, &errb); code != 0 {
				t.Fatalf("exit %d, stderr=%s", code, errb.String())
			}
			if got := strings.Count(errb.String(), notice); got != tc.want {
				t.Errorf("stderr holds the notice %d time(s), want %d:\n%s", got, tc.want, errb.String())
			}
			if strings.Contains(out.String(), "config:") {
				t.Errorf("the notice reached stdout:\n%s", out.String())
			}

			// A second load of the same command prints nothing more.
			var again bytes.Buffer
			g := globalFlags{ConfigPath: cfg, notices: noticesOnce(&again)}
			for range 2 {
				if _, err := g.loadConfig(); err != nil {
					t.Fatal(err)
				}
			}
			if got := strings.Count(again.String(), notice); got != tc.want {
				t.Errorf("two loads print the notice %d time(s), want %d", got, tc.want)
			}
		})
	}
}
