package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// dataRootVar is the variable that names the data folder: of the
// default config in the environment, and of every config in its own
// path values.
const dataRootVar = "WEALTHDB_DATA_ROOT"

// DefaultPath is the default config: wealthdb.cfg in the XDG config dir
// ($XDG_CONFIG_HOME, falling back to ~/.config), absolute and clean.
func DefaultPath() string {
	if xdg := os.Getenv("XDG_CONFIG_HOME"); xdg != "" {
		return cleanAbs(filepath.Join(xdg, "wealthdb.cfg"))
	}
	home, err := os.UserHomeDir()
	if err != nil {
		// No home directory: the literal path, which no file has, so
		// the run answers that nothing is set up.
		return "$HOME/.config/wealthdb.cfg"
	}
	return filepath.Join(home, ".config", "wealthdb.cfg")
}

// IsDefault says path names the default config. It compares the
// absolute, clean form of the two paths, and follows no link. A config
// that is not the default one is a setup of its own, kept in the
// config's folder: that folder is its data folder (DataRoot), and a
// command a message names for it names the config.
func IsDefault(path string) bool {
	return cleanAbs(path) == DefaultPath()
}

// DataRoot is the data folder of the setup whose config is at path,
// absolute: the database and every collector's data. fromEnv says
// WEALTHDB_DATA_ROOT names it.
//
// The default config's is WEALTHDB_DATA_ROOT, else
// $XDG_DATA_HOME/wealthdb, else ~/.local/share/wealthdb. Any other
// config's is the folder that holds the config, whatever the variable
// says.
func DataRoot(path string) (root string, fromEnv bool) {
	if !IsDefault(path) {
		return filepath.Dir(cleanAbs(path)), false
	}
	if v := os.Getenv(dataRootVar); v != "" {
		return cleanAbs(v), true
	}
	if xdg := os.Getenv("XDG_DATA_HOME"); xdg != "" {
		return cleanAbs(filepath.Join(xdg, "wealthdb")), false
	}
	home, _ := os.UserHomeDir()
	return cleanAbs(filepath.Join(home, ".local", "share", "wealthdb")), false
}

// cleanAbs is p made absolute against the working directory and clean,
// or p as it is when the working directory cannot be read.
func cleanAbs(p string) string {
	if a, err := filepath.Abs(p); err == nil {
		return a
	}
	return p
}

// expandPath resolves `~`, `$HOME`, environment vars, and relative
// paths in a path value of the config at configPath. The config's
// folder is the root for relative paths, so a config can name
// `./data/silver.db` wherever the wealthdb binary's cwd is.
//
// Resolution order:
//  1. Replace leading `~/` or `$HOME/` with the home directory.
//  2. Expand environment variables ($FOO, ${FOO}) via os.Expand.
//     WEALTHDB_DATA_ROOT stands for the data folder of the config's
//     setup (DataRoot): the config's folder when the config is not the
//     default one, whatever the environment holds.
//  3. If the result is still relative, resolve against the config's
//     folder.
//
// Empty input returns an error.
func expandPath(p, configPath string) (string, error) {
	if p == "" {
		return "", fmt.Errorf("expandPath: empty path")
	}

	// Step 1: explicit ~ / $HOME prefix expansion.
	p, err := ExpandHome(p)
	if err != nil {
		return "", fmt.Errorf("expandPath: %w", err)
	}

	// Step 2: generic env-var expansion for the rest. Unlike
	// os.ExpandEnv, a referenced-but-unset variable is a hard error
	// rather than a silent "" — a config that says
	// ${WEALTHDB_DATA_ROOT}/wealthdb.db must not quietly collapse to
	// /wealthdb.db when the variable is missing (e.g. a launchd job
	// that doesn't source the shell env). The default config's data
	// folder is no exception: with the variable unset or empty, the
	// folder the engine falls back to is not the one the config names.
	var missing []string
	p = os.Expand(p, func(name string) string {
		if name == dataRootVar {
			if root, fromEnv := DataRoot(configPath); fromEnv || !IsDefault(configPath) {
				return root
			}
		} else if v, ok := os.LookupEnv(name); ok {
			return v
		}
		missing = append(missing, name)
		return ""
	})
	if len(missing) > 0 {
		return "", fmt.Errorf("expandPath: undefined environment variable(s) %s in %q",
			strings.Join(missing, ", "), p)
	}

	// Step 3: resolve relative against the config's folder.
	if !filepath.IsAbs(p) {
		p = filepath.Join(filepath.Dir(configPath), p)
	}

	return filepath.Clean(p), nil
}

// ExpandHome replaces a bare `~`, or a leading `~/` or `$HOME/`, with the home
// directory and returns any other path unchanged.
func ExpandHome(p string) (string, error) {
	var rest string
	switch {
	case p == "~":
	case strings.HasPrefix(p, "~/"):
		rest = p[2:]
	case strings.HasPrefix(p, "$HOME/"):
		rest = p[len("$HOME/"):]
	default:
		return p, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("read $HOME: %w", err)
	}
	return filepath.Join(home, rest), nil
}
