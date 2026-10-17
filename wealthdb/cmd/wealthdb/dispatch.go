package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/errs"
)

// Run is the testable entry point. main is a one-liner around it.
// args is everything after the program name (i.e., os.Args[1:]).
// Returns the process exit code; the caller is responsible for
// os.Exit.
//
// stdout is for normal output (e.g. `wealthdb holdings positions`'s
// table).
// stderr is for log lines, error messages, and usage text.
func Run(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	g, rest, err := parseGlobal(args, stderr)
	if err != nil {
		// `--help` is a request that was answered, not a misuse:
		// flag has already written the usage through fs.Usage, and
		// asking for help succeeds. Everything else that fails to
		// parse is a usage error, which flag has also already
		// reported.
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return errs.ExitUsage
	}
	if len(rest) == 0 {
		printGlobalUsage(stderr)
		return errs.ExitUsage
	}

	subcommand, subargs := rest[0], rest[1:]
	handler, ok := subcommands[subcommand]
	if !ok {
		// The listing names a command or two this binary does not
		// serve; say which side runs them rather than calling a
		// command the help just advertised unknown.
		if c, host := hostSideCommand(subcommand); host {
			fmt.Fprintf(stderr, "wealthdb: %q is served by the host-side wealthdb wrapper, not this binary.\n%s\n",
				subcommand, c.detail())
			return errs.ExitUsage
		}
		fmt.Fprintf(stderr, "wealthdb: unknown subcommand %q\n\n%s", subcommand, usageString())
		return errs.ExitUsage
	}

	ctx := context.Background()
	err = handler(ctx, g, subargs, stdin, stdout, stderr)
	// A subcommand answers -h by printing its usage and returning
	// flag.ErrHelp: a request that was answered, as at the top level.
	if err == nil || errors.Is(err, flag.ErrHelp) {
		return 0
	}

	var ex *errs.ExitError
	if errors.As(err, &ex) {
		fmt.Fprintf(stderr, "wealthdb: %s\n", ex.Error())
		return ex.Code
	}
	fmt.Fprintf(stderr, "wealthdb: %s\n", err.Error())
	return 1
}

// subcommandHandler is the signature every cmd_*.go handler
// implements. ctx is canceled when the dispatcher unwinds;
// subargs are everything after the subcommand name.
type subcommandHandler func(
	ctx context.Context,
	g globalFlags,
	subargs []string,
	stdin io.Reader,
	stdout, stderr io.Writer,
) error

// subcommands is the dispatch table. Each cmd_*.go file
// contributes one entry from its init().
var subcommands = map[string]subcommandHandler{}

func register(name string, h subcommandHandler) {
	if _, dup := subcommands[name]; dup {
		panic(fmt.Sprintf("subcommand %q registered twice", name))
	}
	subcommands[name] = h
}
