package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/config"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/errs"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/gold"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/loader"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/wizard"
	"golang.org/x/term"
)

func init() {
	register("config", cmdConfig)
}

func cmdConfig(ctx context.Context, g globalFlags, subargs []string, stdin io.Reader, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("wealthdb config", flag.ContinueOnError)
	fs.SetOutput(stderr)
	walk := fs.Bool("wizard", false, "walk the step-by-step setup, whatever the config holds")
	fs.Usage = func() {
		fmt.Fprintf(stderr, `usage: wealthdb [-c <file>] config [--wizard] [<section>]
       wealthdb [-c <file>] config validate

Set up wealthdb: the currency of the reports, the banks and brokers to
read, the dashboards and the MCP server. Every answer is saved in
wealthdb.cfg at once. q or Ctrl+C stops, and the next run starts from
what is saved.

A first run walks the setup step by step, and a run again goes on with
it until every step holds an answer: a currency, a source, and the
dashboards and the MCP server on or off. Then the command is a menu of
every section, each with its saved value. --wizard walks the steps
again. A section's name opens that section alone:
%s

The AI model is set in the menu too. The other settings are edited in
the file; the menu lists them with their docs. validate checks the file
as every command reads it, and exits 0 when it is valid: run it after
an edit by hand. It needs no terminal.

-c names another config file, to try a setup or keep several. The data
folder is the one the wealthdb wrapper mounts (WEALTHDB_DATA_ROOT, else
~/.local/share/wealthdb). The setup needs an interactive terminal.
`, sectionList())
	}
	if err := parseFlags(fs, subargs, "config"); err != nil {
		return err
	}
	if fs.NArg() > 1 {
		fs.Usage()
		return errs.Newf(errs.ExitUsage, "config: name one section at most")
	}
	if fs.Arg(0) == "validate" {
		if *walk {
			return errs.Newf(errs.ExitUsage, "config: validate takes no --wizard")
		}
		return validateConfig(g.ConfigPath, stdout)
	}

	// A test drives the setup with a reader that is no *os.File; a real
	// file that is not a terminal is a pipe or a redirect, refused so a
	// script cannot answer for a person.
	var tty wizard.Terminal
	if f, ok := stdin.(*os.File); ok {
		fd := int(f.Fd())
		if !term.IsTerminal(fd) {
			return errs.Newf(errs.ExitUsage,
				"wealthdb config needs an interactive terminal; run it in one, or edit the file (docs/DESIGN.md §5)")
		}
		if os.Getenv("TERM") != "dumb" {
			tty = rawTerminal{fd: fd}
		}
		stop := stopOnSignal(fd, stdout, colors(stdout), setupCommand(g.ConfigPath))
		defer stop()
	}

	if err := os.MkdirAll(filepath.Dir(g.ConfigPath), 0o755); err != nil {
		return errs.Wrap(errs.ExitOpenFailed, fmt.Errorf("create config dir: %w", err))
	}
	root, fromEnv := dataRoot()
	return wizard.Run(wizard.Options{
		ConfigPath:      g.ConfigPath,
		DataRoot:        root,
		DataRootFromEnv: fromEnv,
		Locale:          hostLocale(),
		In:              stdin,
		Out:             stdout,
		Terminal:        tty,
		Wizard:          *walk,
		Section:         fs.Arg(0),
		Command:         setupCommand(g.ConfigPath),
		InContainer:     os.Getenv("WEALTHDB_HOST_OS") != "",
		Colors:          colors(stdout),
		Actions: wizard.Actions{
			Init: func() error {
				cfg, err := g.loadConfig()
				if err != nil {
					return err
				}
				return createGold(ctx, g, cfg, stdout)
			},
			Load:      func(id string) error { return cmdLoad(ctx, g, []string{id}, nil, stdout, stdout) },
			Reset:     func(id string) error { return cmdReset(ctx, g, []string{id}, nil, stdout, stdout) },
			Loaded:    loadedSources,
			TestModel: func(m config.ModelConfig) (string, error) { return testModel(ctx, m) },
		},
	})
}

// validateConfig is `wealthdb config validate`: the verdict on the
// config at path. A config with no setup, or one not finished, is a
// usage error that names the setup; a config the engine refuses exits 1.
func validateConfig(path string, stdout io.Writer) error {
	st, ok, err := wizard.Check(path)
	switch {
	case err == nil:
		fmt.Fprintln(stdout, ok)
		return nil
	case st == config.StateAbsent || st == config.StateUnfinished:
		return errs.Newf(errs.ExitUsage, "%s. Run: %s", err.Error(), setupCommand(path))
	}
	return fmt.Errorf("the config is not valid: %w", err)
}

// testModel asks the model a one-word question, with a ceiling that lets
// a local server load the model first.
func testModel(ctx context.Context, m config.ModelConfig) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()
	reply, err := callLLM(ctx, &m, "Answer with the single word OK.", "Are you there?")
	// callLLM names an error status "HTTP <code> from <url>", and a body
	// it cannot read "decode response": the server answered both.
	if err != nil && (strings.HasPrefix(err.Error(), "HTTP ") || strings.HasPrefix(err.Error(), "decode response")) {
		return "", &wizard.ModelRefused{Err: err}
	}
	return reply, err
}

// sectionList is the names of the sections for the usage text, two
// spaces in, in lines of at most 72 columns.
func sectionList() string {
	var lines []string
	line := ""
	for _, name := range wizard.Sections() {
		if line != "" && len(line)+len(name)+2 > 70 {
			lines, line = append(lines, line+","), ""
		}
		if line != "" {
			line += ", "
		}
		line += name
	}
	return "  " + strings.Join(append(lines, line), "\n  ")
}

// colors is the palette for what a command prints to w: colours when w
// is a terminal that takes them. NO_COLOR set to anything, or a TERM of
// dumb, turns them off (https://no-color.org).
func colors(w io.Writer) wizard.Palette {
	f, ok := w.(*os.File)
	return wizard.Colors(ok && term.IsTerminal(int(f.Fd())) &&
		os.Getenv("NO_COLOR") == "" && os.Getenv("TERM") != "dumb")
}

// rawTerminal is the terminal on fd.
type rawTerminal struct{ fd int }

// Raw puts the terminal in raw mode; restore puts it back.
func (t rawTerminal) Raw() (func(), error) {
	st, err := term.MakeRaw(t.fd)
	if err != nil {
		return nil, err
	}
	return func() { _ = term.Restore(t.fd, st) }, nil
}

// Rows is the height of the terminal's window, 0 when it cannot be read.
func (t rawTerminal) Rows() int {
	_, rows, err := term.GetSize(t.fd)
	if err != nil {
		return 0
	}
	return rows
}

// stopOnSignal makes an interrupt, a hangup or a termination stop the
// setup at once: a write under way finishes, the terminal is put back
// as it was, and the process exits 130. Every answer confirmed before
// is on disk. The returned function puts the terminal back and ends the
// watch, for every other way out, a panic included.
func stopOnSignal(fd int, out io.Writer, p wizard.Palette, command string) func() {
	orig, err := term.GetState(fd)
	restore := func() {
		if err == nil {
			_ = term.Restore(fd, orig)
		}
	}
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	go func() {
		if _, ok := <-sig; !ok {
			return
		}
		wizard.WaitForSave()
		restore()
		fmt.Fprint(out, "\n"+wizard.Stopped(p, command))
		os.Exit(130)
	}()
	return func() {
		signal.Stop(sig)
		close(sig)
		restore()
	}
}

// dataRoot is the data folder the wrapper mounts, and whether
// WEALTHDB_DATA_ROOT names it: the variable, else $XDG_DATA_HOME/wealthdb,
// else ~/.local/share/wealthdb.
func dataRoot() (string, bool) {
	if v := os.Getenv("WEALTHDB_DATA_ROOT"); v != "" {
		return filepath.Clean(v), true
	}
	if xdg := os.Getenv("XDG_DATA_HOME"); xdg != "" {
		return filepath.Join(xdg, "wealthdb"), false
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".local", "share", "wealthdb"), false
}

// hostLocale is the locale the wrapper forwards from the host, else the
// container's own.
func hostLocale() string {
	for _, k := range []string{"WEALTHDB_LOCALE", "LC_ALL", "LC_MONETARY", "LANG"} {
		if v := os.Getenv(k); v != "" {
			return v
		}
	}
	return ""
}

// loadedSources are the sources the database at path holds.
func loadedSources(path string) ([]string, error) {
	db, err := gold.Open(path, gold.ModeReadOnly)
	if err != nil {
		return nil, err
	}
	defer db.Close()
	return loader.New(db).ListSourceIDs(context.Background())
}
