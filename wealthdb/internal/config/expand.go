package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// expandPath resolves `~`, `$HOME`, environment vars, and relative
// paths in a config path value. The `baseDir` argument is the
// directory of the config file, used as the root for relative
// paths so users can write paths like `./data/silver.db` without
// caring where the wealthdb binary's cwd is.
//
// Resolution order:
//  1. Replace leading `~/` or `$HOME/` with the home directory.
//  2. Expand environment variables ($FOO, ${FOO}) via os.Expand.
//  3. If the result is still relative, resolve against baseDir.
//
// Empty input returns an error.
func expandPath(p, baseDir string) (string, error) {
	if p == "" {
		return "", fmt.Errorf("expandPath: empty path")
	}

	// Step 1: explicit ~ / $HOME prefix expansion.
	switch {
	case p == "~":
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("expandPath: read $HOME: %w", err)
		}
		p = home
	case strings.HasPrefix(p, "~/"):
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("expandPath: read $HOME: %w", err)
		}
		p = filepath.Join(home, p[2:])
	case strings.HasPrefix(p, "$HOME/"):
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("expandPath: read $HOME: %w", err)
		}
		p = filepath.Join(home, p[len("$HOME/"):])
	}

	// Step 2: generic env-var expansion for the rest. Unlike
	// os.ExpandEnv, a referenced-but-unset variable is a hard error
	// rather than a silent "" — a config that says
	// ${WEALTHDB_DATA_ROOT}/wealthdb.db must not quietly collapse to
	// /wealthdb.db when the variable is missing (e.g. a launchd job
	// that doesn't source the shell env).
	var missing []string
	p = os.Expand(p, func(name string) string {
		if v, ok := os.LookupEnv(name); ok {
			return v
		}
		missing = append(missing, name)
		return ""
	})
	if len(missing) > 0 {
		return "", fmt.Errorf("expandPath: undefined environment variable(s) %s in %q",
			strings.Join(missing, ", "), p)
	}

	// Step 3: resolve relative against baseDir.
	if !filepath.IsAbs(p) {
		p = filepath.Join(baseDir, p)
	}

	return filepath.Clean(p), nil
}
