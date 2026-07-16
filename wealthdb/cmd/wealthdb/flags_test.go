package main

import (
	"os"
	"path/filepath"
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
