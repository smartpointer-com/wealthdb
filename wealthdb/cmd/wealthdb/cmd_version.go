package main

import (
	"context"
	"fmt"
	"io"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/errs"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/version"
)

func init() {
	register("version", cmdVersion)
}

func cmdVersion(_ context.Context, _ globalFlags, subargs []string, _ io.Reader, stdout, _ io.Writer) error {
	if len(subargs) != 0 {
		return errs.Newf(2, "version: unexpected argument %q", subargs[0])
	}
	fmt.Fprintf(stdout, "wealthdb %s\n", version.String())
	return nil
}
