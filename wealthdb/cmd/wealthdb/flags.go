package main

import (
	"errors"
	"flag"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/errs"
)

// globalFlags collects the parsed top-level (pre-subcommand)
// flags. Subcommand handlers consult these to know which config
// to load and whether to force read-only mode.
type globalFlags struct {
	ConfigPath    string
	ForceReadOnly bool
	// now is the clock the reports read today from (clock); nil is
	// the wall clock.
	now func() time.Time
}

// clock is today as the reports read it: a window's default end, an
// as-of day's default, the forward views' start.
func (g globalFlags) clock() time.Time {
	if g.now != nil {
		return g.now()
	}
	return time.Now()
}

// defaultConfigPath returns wealthdb.cfg under the XDG config dir
// ($XDG_CONFIG_HOME, falling back to ~/.config), resolved at parse
// time so help text shows the concrete value.
func defaultConfigPath() string {
	if xdg := os.Getenv("XDG_CONFIG_HOME"); xdg != "" {
		return filepath.Join(xdg, "wealthdb.cfg")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		// No $HOME → fall back to the unexpanded literal. The
		// user will get a clear error on Load.
		return "$HOME/.config/wealthdb.cfg"
	}
	return filepath.Join(home, ".config", "wealthdb.cfg")
}

// parseGlobal extracts top-level flags from the leading slice of
// args. Returns the consumed flags, the remaining args (starting
// with the subcommand name), and a usage-writer error if parsing
// failed.
func parseGlobal(args []string, errOut io.Writer) (globalFlags, []string, error) {
	fs := flag.NewFlagSet("wealthdb", flag.ContinueOnError)
	fs.SetOutput(errOut)
	fs.Usage = func() {
		printGlobalUsage(errOut)
	}

	g := globalFlags{ConfigPath: defaultConfigPath()}
	fs.StringVar(&g.ConfigPath, "c", g.ConfigPath, "path to wealthdb config file")
	fs.StringVar(&g.ConfigPath, "config", g.ConfigPath, "path to wealthdb config file")
	fs.BoolVar(&g.ForceReadOnly, "r", false, "force read-only access to the gold DB")
	fs.BoolVar(&g.ForceReadOnly, "read-only", false, "force read-only access to the gold DB")

	if err := fs.Parse(args); err != nil {
		return g, nil, err
	}
	return g, fs.Args(), nil
}

// parseFlags parses a subcommand's args into fs, the one way every
// subcommand reads its flags. Flags may follow positionals (see
// reorderFlagsFirst), and a fused -C delta is split first. On -h it
// returns flag.ErrHelp, which the dispatcher reads as success: flag has
// already printed the usage. Any other failure is a usage error naming
// cmd, after flag has printed the detail.
func parseFlags(fs *flag.FlagSet, args []string, cmd string) error {
	err := fs.Parse(reorderFlagsFirst(fs, splitFusedColumnsFlag(args)))
	if err == nil || errors.Is(err, flag.ErrHelp) {
		return err
	}
	return errs.Newf(errs.ExitUsage, "%s: bad flags", cmd)
}

// noPositionals refuses positional arguments on a command that takes
// none.
func noPositionals(fs *flag.FlagSet, cmd string) error {
	if fs.NArg() == 0 {
		return nil
	}
	fs.Usage()
	return errs.Newf(errs.ExitUsage, "%s: unexpected positional argument %q", cmd, fs.Arg(0))
}

// reorderFlagsFirst moves the flag tokens of args, each with the value
// it consumes, ahead of the positionals, keeping the relative order
// within each group. Go's flag package stops at the first positional,
// so without this `transactions 2025 -f csv` would read `-f csv` as two
// more dates. A flag fs declares consumes the next token unless it is
// boolean; the --flag=value form is one token. `-` alone (the open end
// of a range) and `--` are positionals.
func reorderFlagsFirst(fs *flag.FlagSet, args []string) []string {
	flags := make([]string, 0, len(args))
	positional := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			// Everything after the terminator is positional, even
			// a token that looks like a flag.
			positional = append(positional, args[i:]...)
			break
		}
		if a == "-" || !strings.HasPrefix(a, "-") {
			positional = append(positional, a)
			continue
		}
		flags = append(flags, a)
		if strings.Contains(a, "=") {
			continue
		}
		if takesValue(fs, strings.TrimLeft(a, "-")) && i+1 < len(args) {
			i++
			flags = append(flags, args[i])
		}
	}
	return append(flags, positional...)
}

// takesValue reports whether fs declares name as a flag that consumes
// the next token: any flag but a boolean one.
func takesValue(fs *flag.FlagSet, name string) bool {
	f := fs.Lookup(name)
	if f == nil {
		return false
	}
	b, ok := f.Value.(interface{ IsBoolFlag() bool })
	return !ok || !b.IsBoolFlag()
}

// registerAsOf declares -d / --as-of, the day an as-of report is taken
// at.
func registerAsOf(fs *flag.FlagSet) *string {
	asOf := fs.String("d", "", "as-of date (YYYY-MM-DD; default today UTC)")
	fs.StringVar(asOf, "as-of", "", "as-of date (YYYY-MM-DD; default today UTC)")
	return asOf
}

// checkChoice refuses a flag value outside its vocabulary, naming the
// values it takes.
func checkChoice(cmd, flagName, value string, allowed ...string) error {
	if slices.Contains(allowed, value) {
		return nil
	}
	return errs.Newf(errs.ExitUsage, "%s: invalid --%s %q (want %s)", cmd, flagName, value, strings.Join(allowed, " | "))
}

// isSet reports whether any of the given spellings of a flag was typed
// on the command line, as opposed to sitting at its default. The
// refusals need it twice over: `--period monthly` is the default value
// AND a thing a caller can type at a sankey, and a flag registered
// under both a short and a long name is a different *flag.Flag under
// each, so a refusal that asks for one spelling misses the other.
func isSet(fs *flag.FlagSet, names ...string) bool {
	found := false
	fs.Visit(func(f *flag.Flag) {
		for _, name := range names {
			if f.Name == name {
				found = true
			}
		}
	})
	return found
}
