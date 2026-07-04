package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/ptu/wealthdb/internal/errs"
	"github.com/ptu/wealthdb/internal/wizard"
	"golang.org/x/term"
)

func init() {
	register("config", cmdConfig)
}

func cmdConfig(_ context.Context, g globalFlags, subargs []string, stdin io.Reader, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("wealthdb config", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		fmt.Fprintln(stderr, `usage: wealthdb config [-c <config-file-path>]

Interactive first-time setup. Walks you through:
  - gold DB path
  - default output currency
  - one or more silver sources (id / kind / path)

Refuses to clobber an existing config file (use a different -c
path, or delete the old file). Requires an interactive TTY;
pipe-driven invocations are rejected to avoid silently producing
empty configs.`)
	}
	if err := fs.Parse(subargs); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return errs.Newf(2, "config: bad flags")
	}
	if fs.NArg() != 0 {
		fs.Usage()
		return errs.Newf(2, "config: unexpected positional argument %q", fs.Arg(0))
	}

	// Refuse to clobber. Same exit code as `init`-on-existing
	// (DESIGN.md §4.10 row 4).
	if _, err := os.Stat(g.ConfigPath); err == nil {
		return errs.Newf(errs.ExitInitExisting,
			"config file %q already exists. Delete or move it first, or pass -c <other-path>.", g.ConfigPath)
	}

	// TTY check — only meaningful when stdin is a real *os.File.
	// In tests we pass a *bytes.Buffer / strings.Reader; those
	// aren't *os.File, so we skip the check and proceed.
	if f, ok := stdin.(*os.File); ok {
		if !term.IsTerminal(int(f.Fd())) {
			return errs.Newf(2,
				"wealthdb config requires an interactive terminal; "+
					"re-run with `docker run -it ...` or hand-edit the JSON per docs/DESIGN.md §5.")
		}
	}

	// Ensure parent dir exists before resolving
	// gold-DB defaults that may rely on the XDG data dir already.
	configDir := filepath.Dir(g.ConfigPath)
	if err := os.MkdirAll(configDir, 0o755); err != nil {
		return errs.Wrap(errs.ExitOpenFailed, fmt.Errorf("create config dir %q: %w", configDir, err))
	}

	def := wizard.Defaults{
		GoldDB:          defaultGoldDBPath(),
		DefaultCurrency: "USD",
	}
	res, err := wizard.Run(stdin, stdout, g.ConfigPath, def)
	if err != nil {
		return err
	}

	data, err := json.MarshalIndent(res.Config, "", "    ")
	if err != nil {
		return fmt.Errorf("marshal config: %w", err)
	}
	data = append(data, '\n')
	if err := os.WriteFile(res.ConfigPath, data, 0o644); err != nil {
		return errs.Wrap(errs.ExitOpenFailed, fmt.Errorf("write config: %w", err))
	}

	fmt.Fprintf(stdout, "\nWrote config to %s\n", res.ConfigPath)
	return nil
}

// defaultGoldDBPath returns the gold DB under the XDG data dir
// ($XDG_DATA_HOME/wealthdb/wealthdb.db, falling back to
// ~/.local/share/wealthdb/wealthdb.db). Same shape as the README /
// docs prescribe. Returns a resolved path so it needs no further
// env expansion when written into the config.
func defaultGoldDBPath() string {
	if xdg := os.Getenv("XDG_DATA_HOME"); xdg != "" {
		return filepath.Join(xdg, "wealthdb", "wealthdb.db")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "$HOME/.local/share/wealthdb/wealthdb.db"
	}
	return filepath.Join(home, ".local", "share", "wealthdb", "wealthdb.db")
}
