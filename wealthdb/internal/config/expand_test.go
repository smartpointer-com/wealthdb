package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The default config is wealthdb.cfg in the XDG config dir, whatever
// names the config a run reads, and a path is compared to it in its
// absolute, clean form.
func TestIsDefault(t *testing.T) {
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_CONFIG_HOME", "/xdg/conf")
	t.Setenv("WEALTHDB_CONFIG", "/elsewhere/my.cfg")
	if got, want := DefaultPath(), "/xdg/conf/wealthdb.cfg"; got != want {
		t.Errorf("DefaultPath() = %q, want %q", got, want)
	}
	for path, want := range map[string]bool{
		"/xdg/conf/wealthdb.cfg":          true,
		"/xdg/conf/./x/../wealthdb.cfg":   true,
		"/xdg/conf/try.cfg":               false,
		"/xdg/conf/try/wealthdb.cfg":      false,
		"/elsewhere/my.cfg":               false,
		"wealthdb.cfg":                    false,
		filepath.Join(wd, "wealthdb.cfg"): false,
	} {
		if got := IsDefault(path); got != want {
			t.Errorf("IsDefault(%q) = %v, want %v", path, got, want)
		}
	}
	// A relative XDG_CONFIG_HOME is read against the working directory,
	// and so is a relative path.
	t.Setenv("XDG_CONFIG_HOME", "rel/conf")
	if !IsDefault("rel/conf/wealthdb.cfg") || !IsDefault(filepath.Join(wd, "rel", "conf", "wealthdb.cfg")) {
		t.Errorf("a relative XDG_CONFIG_HOME: DefaultPath() = %q", DefaultPath())
	}
	t.Setenv("XDG_CONFIG_HOME", "")
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skipf("no home dir: %v", err)
	}
	if !IsDefault(filepath.Join(home, ".config", "wealthdb.cfg")) {
		t.Errorf("no XDG_CONFIG_HOME: DefaultPath() = %q", DefaultPath())
	}
}

// The default config's data folder is WEALTHDB_DATA_ROOT, else the XDG
// data dir, absolute whichever names it.
func TestDataRootOfTheDefaultConfig(t *testing.T) {
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_CONFIG_HOME", "/xdg/conf")
	cfg := DefaultPath()
	t.Setenv("WEALTHDB_DATA_ROOT", "/data/root")
	if got, named := DataRoot(cfg); got != "/data/root" || !named {
		t.Errorf("WEALTHDB_DATA_ROOT: DataRoot = %q, %v", got, named)
	}
	t.Setenv("WEALTHDB_DATA_ROOT", "rel/data")
	if got, named := DataRoot(cfg); got != filepath.Join(wd, "rel", "data") || !named {
		t.Errorf("WEALTHDB_DATA_ROOT relative: DataRoot = %q, %v", got, named)
	}
	t.Setenv("WEALTHDB_DATA_ROOT", "")
	t.Setenv("XDG_DATA_HOME", "/xdg/data")
	if got, named := DataRoot(cfg); got != "/xdg/data/wealthdb" || named {
		t.Errorf("XDG_DATA_HOME: DataRoot = %q, %v", got, named)
	}
	t.Setenv("XDG_DATA_HOME", "rel/xdg")
	if got, named := DataRoot(cfg); got != filepath.Join(wd, "rel", "xdg", "wealthdb") || named {
		t.Errorf("XDG_DATA_HOME relative: DataRoot = %q, %v", got, named)
	}
	t.Setenv("XDG_DATA_HOME", "")
	home := t.TempDir()
	t.Setenv("HOME", home)
	if got, named := DataRoot(cfg); got != filepath.Join(home, ".local", "share", "wealthdb") || named {
		t.Errorf("neither variable: DataRoot = %q, %v", got, named)
	}
	// With no home directory either, the folder is still absolute.
	t.Setenv("HOME", "")
	if got, named := DataRoot(cfg); got != filepath.Join(wd, ".local", "share", "wealthdb") || named {
		t.Errorf("no home directory: DataRoot = %q, %v", got, named)
	}
}

// A config that is not the default one is a setup of its own: its data
// folder is the folder that holds it, whatever WEALTHDB_DATA_ROOT and
// XDG_DATA_HOME say.
func TestDataRootOfAnotherConfigIsItsFolder(t *testing.T) {
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_CONFIG_HOME", "/xdg/conf")
	for _, env := range []string{"/data/elsewhere", ""} {
		t.Setenv("WEALTHDB_DATA_ROOT", env)
		t.Setenv("XDG_DATA_HOME", env)
		for path, want := range map[string]string{
			"/try/wealthdb.cfg":            "/try",
			"/try/a/../my setup/./x.cfg":   "/try/my setup",
			"rel/try.cfg":                  filepath.Join(wd, "rel"),
			"/xdg/conf/other.cfg":          "/xdg/conf",
			"/xdg/conf/sub/wealthdb.cfg":   "/xdg/conf/sub",
			"/data/elsewhere/wealthdb.cfg": "/data/elsewhere",
		} {
			if got, named := DataRoot(path); got != want || named {
				t.Errorf("WEALTHDB_DATA_ROOT=%q: DataRoot(%q) = %q, %v, want %q, false", env, path, got, named, want)
			}
		}
	}
}

// ${WEALTHDB_DATA_ROOT} in a config stands for the data folder of that
// config's setup: the variable for the default config, and the config's
// own folder for any other, whatever the variable says.
func TestLoadExpandsDataRootByConfig(t *testing.T) {
	dir := t.TempDir()
	const body = `{"gold_db": "${WEALTHDB_DATA_ROOT}/wealthdb.db", "default_currency": "USD",
 "silver_sources": [{"id": "viac", "kind": "viac", "path": "$WEALTHDB_DATA_ROOT/viac/viac.db"}]}`
	write := func(parts ...string) string {
		t.Helper()
		path := filepath.Join(append([]string{dir}, parts...)...)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(dir, "conf"))
	def, own := write("conf", "wealthdb.cfg"), write("my setup", "wealthdb.cfg")
	elsewhere := filepath.Join(dir, "elsewhere")
	check := func(what, path, root string) {
		t.Helper()
		c, err := Load(path)
		if err != nil {
			t.Fatalf("%s: Load: %v", what, err)
		}
		if c.GoldDB != filepath.Join(root, "wealthdb.db") || c.SilverSources[0].Path != filepath.Join(root, "viac", "viac.db") {
			t.Errorf("%s: gold_db = %q, path = %q, want both under %q", what, c.GoldDB, c.SilverSources[0].Path, root)
		}
	}

	t.Setenv("WEALTHDB_DATA_ROOT", elsewhere)
	check("the default config", def, elsewhere)
	check("another config, the variable set", own, filepath.Dir(own))
	if got, err := ExpandPath("${WEALTHDB_DATA_ROOT}/ledgers/pins.csv", own); err != nil || got != filepath.Join(filepath.Dir(own), "ledgers", "pins.csv") {
		t.Errorf("ExpandPath for another config = %q, %v", got, err)
	}
	if got, err := ExpandPath("${WEALTHDB_DATA_ROOT}/ledgers/pins.csv", def); err != nil || got != filepath.Join(elsewhere, "ledgers", "pins.csv") {
		t.Errorf("ExpandPath for the default config = %q, %v", got, err)
	}
	// A relative path still resolves against the config's folder.
	if got, err := ExpandPath("ledgers/pins.csv", def); err != nil || got != filepath.Join(dir, "conf", "ledgers", "pins.csv") {
		t.Errorf("ExpandPath of a relative path = %q, %v", got, err)
	}

	// With the variable unset or empty, the default config that names it
	// is refused, and the other config still reads its own folder.
	for _, unset := range []bool{false, true} {
		t.Setenv("WEALTHDB_DATA_ROOT", "")
		if unset {
			os.Unsetenv("WEALTHDB_DATA_ROOT")
		}
		if _, err := Load(def); err == nil || !strings.Contains(err.Error(), "undefined environment variable(s) WEALTHDB_DATA_ROOT") {
			t.Errorf("the default config, unset=%v: err = %v, want the variable named as undefined", unset, err)
		}
		check("another config, no variable", own, filepath.Dir(own))
	}
}
