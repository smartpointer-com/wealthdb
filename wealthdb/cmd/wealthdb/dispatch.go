package main

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/ptu-gh/wealthdb/wealthdb/internal/errs"
)

// Run is the testable entry point. main is a one-liner around it.
// args is everything after the program name (i.e., os.Args[1:]).
// Returns the process exit code; the caller is responsible for
// os.Exit.
//
// stdout is for normal output (e.g. `wealthdb positions`'s table).
// stderr is for log lines, error messages, and usage text.
func Run(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	g, rest, err := parseGlobal(args, stderr)
	if err != nil {
		// flag.ContinueOnError prints to stderr already; return 2
		// (same code the stdlib uses for usage errors).
		return 2
	}
	if len(rest) == 0 {
		fmt.Fprint(stderr, globalUsage)
		return 2
	}

	subcommand, subargs := rest[0], rest[1:]
	handler, ok := subcommands[subcommand]
	if !ok {
		fmt.Fprintf(stderr, "wealthdb: unknown subcommand %q\n\n%s", subcommand, globalUsage)
		return 2
	}

	ctx := context.Background()
	err = handler(ctx, g, subargs, stdin, stdout, stderr)
	if err == nil {
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
