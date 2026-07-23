package main

import (
	"context"
	"fmt"
	"io"

	"github.com/ptu-gh/wealthdb/wealthdb/internal/errs"
	"github.com/ptu-gh/wealthdb/wealthdb/internal/version"
)

func init() {
	register("version", cmdVersion)
}

func cmdVersion(_ context.Context, _ globalFlags, subargs []string, _ io.Reader, stdout, _ io.Writer) error {
	if len(subargs) != 0 {
		return errs.Newf(2, "version: unexpected argument %q", subargs[0])
	}
	fmt.Fprintf(stdout, "wealthdb %s\n", version.Version)
	return nil
}
