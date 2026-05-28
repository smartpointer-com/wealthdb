package wizard

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	_ "modernc.org/sqlite"

	"github.com/ptu/wealthdb/internal/config"
	"github.com/ptu/wealthdb/internal/silver"
)

// Defaults captures the recommended values the wizard offers as
// "press enter to accept". Lifted into a struct so the caller
// (cmd/wealthdb) can populate $HOME-aware defaults without the
// wizard needing to know about the host filesystem.
type Defaults struct {
	GoldDB          string
	DefaultCurrency string
}

// Result is what the wizard hands back to the caller. The config
// is fully validated and path-expanded; the caller serialises it
// to JSON and writes it to ConfigPath.
type Result struct {
	Config     config.Config
	ConfigPath string
}

// idPattern mirrors config.idPattern — kept here to avoid a
// cross-package import cycle and so the wizard can reject bad
// IDs at prompt time rather than at validate time.
var idPattern = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

// Run drives the first-time setup conversation. configPath is
// where the resulting wealthdb.cfg will be written; the caller
// has already confirmed that file doesn't exist.
//
// stdin is line-buffered internally; pass an unbuffered reader.
// stdout receives prompts and progress lines.
func Run(stdin io.Reader, stdout io.Writer, configPath string, def Defaults) (*Result, error) {
	p := newPrompter(stdin, stdout)
	fmt.Fprintln(stdout, "wealthdb first-time setup")
	fmt.Fprintln(stdout, "—————————————————————————")
	fmt.Fprintf(stdout, "Writing config to: %s\n\n", configPath)

	goldDB, err := p.askValidated(
		"Path for the gold DuckDB file",
		def.GoldDB,
		func(s string) error {
			if s == "" {
				return fmt.Errorf("required")
			}
			expanded := expandLeadingHome(s)
			dir := filepath.Dir(expanded)
			if _, err := os.Stat(dir); err != nil {
				if os.IsNotExist(err) {
					return fmt.Errorf("parent directory %s does not exist (create it first, then re-run)", dir)
				}
				return err
			}
			return nil
		},
	)
	if err != nil {
		return nil, err
	}

	defaultCcy, err := p.askValidated(
		"Default output currency (ISO 4217, 3 uppercase letters)",
		def.DefaultCurrency,
		func(s string) error {
			if !isISO4217Shape(s) {
				return fmt.Errorf("not a 3-letter uppercase code")
			}
			return nil
		},
	)
	if err != nil {
		return nil, err
	}

	cfg := config.Config{
		GoldDB:          goldDB,
		DefaultCurrency: defaultCcy,
	}

	knownKinds := silver.Kinds()
	if len(knownKinds) == 0 {
		// Library / test builds that don't blank-import the
		// adapter packages won't see any registered kinds.
		// Allow any string in that case; production wealthdb
		// always has at least one.
		fmt.Fprintln(stdout, "  (no silver adapters registered; kind validation skipped)")
	}

	fmt.Fprintln(stdout)
	fmt.Fprintln(stdout, "Now add at least one silver source.")
	for {
		src, err := promptSilverSource(p, cfg.SilverSources, knownKinds)
		if err != nil {
			return nil, err
		}
		cfg.SilverSources = append(cfg.SilverSources, *src)

		more, err := p.askYesNo("Add another silver source?", false)
		if err != nil {
			return nil, err
		}
		if !more {
			break
		}
	}

	fmt.Fprintln(stdout)
	fmt.Fprintf(stdout, "Setup complete — %d silver source(s) configured.\n", len(cfg.SilverSources))
	fmt.Fprintln(stdout, "Next steps:")
	fmt.Fprintf(stdout, "  ./wealthdb -c %s init\n", configPath)
	fmt.Fprintf(stdout, "  ./wealthdb -c %s load -a\n", configPath)

	return &Result{Config: cfg, ConfigPath: configPath}, nil
}

// promptSilverSource walks the user through one silver-source
// entry: id (slug, not-already-used), kind (registered or auto),
// path (file exists and looks like a silver SQLite).
func promptSilverSource(p *prompter, already []config.SilverSource, knownKinds []string) (*config.SilverSource, error) {
	usedIDs := make(map[string]struct{}, len(already))
	for _, s := range already {
		usedIDs[s.ID] = struct{}{}
	}

	id, err := p.askValidated(
		"  Silver source id (e.g. 'schwab', 'ubs-main')",
		"",
		func(s string) error {
			if !idPattern.MatchString(s) {
				return fmt.Errorf("must match %s", idPattern.String())
			}
			if _, dup := usedIDs[s]; dup {
				return fmt.Errorf("already in use in this config")
			}
			return nil
		},
	)
	if err != nil {
		return nil, err
	}

	kindHint := ""
	if len(knownKinds) > 0 {
		kindHint = strings.Join(append(append([]string{}, knownKinds...), "auto"), " | ")
	}
	kind, err := p.askValidated(
		fmt.Sprintf("  Kind (%s)", kindHint),
		"",
		func(s string) error {
			if len(knownKinds) == 0 {
				return nil
			}
			if s == "auto" {
				return nil
			}
			for _, k := range knownKinds {
				if k == s {
					return nil
				}
			}
			return fmt.Errorf("unknown kind; want one of %s or 'auto'", strings.Join(knownKinds, ", "))
		},
	)
	if err != nil {
		return nil, err
	}

	path, err := p.askValidated(
		"  Path to silver SQLite",
		"",
		func(s string) error {
			expanded := expandLeadingHome(s)
			if err := probeSilverDB(expanded); err != nil {
				return err
			}
			return nil
		},
	)
	if err != nil {
		return nil, err
	}

	return &config.SilverSource{ID: id, Kind: kind, Path: path}, nil
}

// probeSilverDB opens the path as a read-only SQLite and checks
// that a `dump_runs` table is present. That's our minimum-viable
// "looks like a silver SQLite" test — every adapter relies on
// dump_runs, so its absence is a clear signal the user pointed
// at the wrong file.
func probeSilverDB(path string) error {
	if _, err := os.Stat(path); err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("file does not exist: %s", path)
		}
		return err
	}
	dsn := fmt.Sprintf("file:%s?mode=ro&_pragma=query_only(true)", path)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return fmt.Errorf("could not open as SQLite: %w", err)
	}
	defer db.Close()
	if err := db.Ping(); err != nil {
		return fmt.Errorf("could not read as SQLite: %w", err)
	}
	var n int
	err = db.QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='dump_runs'`,
	).Scan(&n)
	if err != nil {
		return fmt.Errorf("could not inspect schema: %w", err)
	}
	if n == 0 {
		return fmt.Errorf("file is missing the `dump_runs` table — is it really a silver SQLite from a *-dump tool?")
	}
	return nil
}

// expandLeadingHome handles `~/...` and `$HOME/...` exactly like
// config.expandPath (kept local to avoid exposing the package-
// internal helper). For wizard input we don't try to resolve
// relative paths — users typing into a prompt are expected to
// give absolute or home-anchored paths.
func expandLeadingHome(s string) string {
	switch {
	case s == "~":
		if home, err := os.UserHomeDir(); err == nil {
			return home
		}
	case strings.HasPrefix(s, "~/"):
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, s[2:])
		}
	case strings.HasPrefix(s, "$HOME/"):
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, s[len("$HOME/"):])
		}
	}
	return s
}

func isISO4217Shape(s string) bool {
	if len(s) != 3 {
		return false
	}
	for _, r := range s {
		if r < 'A' || r > 'Z' {
			return false
		}
	}
	return true
}
