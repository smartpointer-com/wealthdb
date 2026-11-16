package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The config default follows the XDG Base Directory spec:
// $XDG_CONFIG_HOME when set, ~/.config otherwise (an empty value
// counts as unset, per the spec).
func TestDefaultConfigPathHonorsXDGConfigHome(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", "/xdg/conf")
	if got, want := defaultConfigPath(), filepath.Join("/xdg/conf", "wealthdb.cfg"); got != want {
		t.Errorf("defaultConfigPath() = %q, want %q", got, want)
	}
}

func TestDefaultConfigPathFallsBackToDotConfig(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", "")
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skipf("no home dir: %v", err)
	}
	if got, want := defaultConfigPath(), filepath.Join(home, ".config", "wealthdb.cfg"); got != want {
		t.Errorf("defaultConfigPath() = %q, want %q", got, want)
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
