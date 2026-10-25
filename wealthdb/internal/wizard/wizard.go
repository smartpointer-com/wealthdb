package wizard

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/config"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/silver"
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
// holds each answer as typed, with paths unexpanded; the caller
// serialises it to JSON and writes it to ConfigPath.
type Result struct {
	Config     config.Config
	ConfigPath string
}

// Run drives the first-time setup conversation. configPath is
// where the resulting wealthdb.cfg will be written; the caller
// has already confirmed that file is absent or empty.
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
			if !config.IsLikelyISO4217(s) {
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

// promptSilverSource walks through one silver-source
// entry: id (slug, not-already-used), kind (a registered adapter),
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
			if !config.IDPattern.MatchString(s) {
				return fmt.Errorf("must match %s", config.IDPattern.String())
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
		kindHint = strings.Join(knownKinds, " | ")
	}
	kind, err := p.askValidated(
		fmt.Sprintf("  Kind (%s)", kindHint),
		"",
		func(s string) error {
			if len(knownKinds) == 0 {
				return nil
			}
			if slices.Contains(knownKinds, s) {
				return nil
			}
			return fmt.Errorf("unknown kind; want one of %s", strings.Join(knownKinds, ", "))
		},
	)
	if err != nil {
		return nil, err
	}

	path, err := p.askValidated(
		"  Path to silver SQLite",
		"",
		func(s string) error {
			return probeSilverDB(expandLeadingHome(s))
		},
	)
	if err != nil {
		return nil, err
	}

	return &config.SilverSource{ID: id, Kind: kind, Path: path}, nil
}

// probeSilverDB opens the path as a read-only SQLite and checks
// that a `dump_runs` or `load_runs` table is present. That's our
// minimum-viable "looks like a silver SQLite" test — collector
// silvers carry dump_runs, load-only silvers (e.g. manual) carry
// load_runs, so the absence of both is a clear signal the
// configured path is wrong.
func probeSilverDB(path string) error {
	if _, err := os.Stat(path); err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("file does not exist: %s", path)
		}
		return err
	}
	db, err := silver.OpenReadOnlySQLite(path, "silver")
	if err != nil {
		return fmt.Errorf("could not read as SQLite: %w", err)
	}
	defer db.Close()
	ctx := context.Background()
	for _, table := range []string{"dump_runs", "load_runs"} {
		ok, err := silver.HasTables(ctx, db, table)
		if err != nil {
			return fmt.Errorf("could not inspect schema: %w", err)
		}
		if ok {
			return nil
		}
	}
	return fmt.Errorf("file has neither a `dump_runs` nor a `load_runs` table — is it really a silver SQLite from a wealthdb collector?")
}

// expandLeadingHome expands a leading `~` or `$HOME` the way the
// config loader does. Relative paths stay as typed: a prompt answer is
// expected to be absolute or home-anchored. A home directory that
// cannot be read leaves the input unchanged for the probe to reject.
func expandLeadingHome(s string) string {
	if p, err := config.ExpandHome(s); err == nil {
		return p
	}
	return s
}
