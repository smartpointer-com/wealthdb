package main

import (
	"bytes"
	"strings"
	"testing"
)

// TestUsageListsEverySubcommand is the reason the listing is
// generated rather than written out: a subcommand registers itself
// from its own file's init(), so a new one used to reach the
// dispatcher without reaching the help, and nothing said so. The
// check runs both ways — a listed command that nothing serves is the
// same defect seen from the other side.
func TestUsageListsEverySubcommand(t *testing.T) {
	t.Parallel()
	listed := map[string]commandHelp{}
	for _, c := range commandHelps {
		if _, dup := listed[c.name]; dup {
			t.Errorf("%q is listed twice", c.name)
		}
		listed[c.name] = c
	}

	for name := range subcommands {
		if hiddenSubcommands[name] {
			if _, ok := listed[name]; ok {
				t.Errorf("%q is both hidden and listed", name)
			}
			continue
		}
		if _, ok := listed[name]; !ok {
			t.Errorf("subcommand %q is registered but missing from the usage listing", name)
		}
	}

	for name, c := range listed {
		if c.hostSide {
			if _, ok := subcommands[name]; ok {
				t.Errorf("%q is listed as host-side but this binary registers it", name)
			}
			continue
		}
		if _, ok := subcommands[name]; !ok {
			t.Errorf("%q is listed but no subcommand is registered for it", name)
		}
	}
}

// TestEveryListedCommandSaysWhatItDoes pins that no entry reaches the
// listing blank — an empty column reads as a rendering fault, and the
// listing is the only place many of these commands are documented.
func TestEveryListedCommandSaysWhatItDoes(t *testing.T) {
	t.Parallel()
	for _, c := range commandHelps {
		if c.name == "" {
			t.Error("an entry carries no name")
		}
		if strings.TrimSpace(c.short) == "" {
			t.Errorf("%q carries no short description", c.name)
		}
		if strings.TrimSpace(c.detail()) == "" {
			t.Errorf("%q resolves to no detail for 'wealthdb help %s'", c.name, c.name)
		}
	}
}

// TestHelpViewsCoverEveryHoldingsView pins that `wealthdb help
// <view>` answers for each grain `holdings` dispatches, since a view
// is not a subcommand and would otherwise report as unknown.
func TestHelpViewsCoverEveryHoldingsView(t *testing.T) {
	t.Parallel()
	for view := range holdingsViews {
		if _, ok := viewHelp[view]; !ok {
			t.Errorf("holdings view %q has no blurb for 'wealthdb help %s'", view, view)
		}
	}
	for view := range viewHelp {
		if _, ok := holdingsViews[view]; !ok {
			t.Errorf("%q is blurbed as a holdings view but is not one", view)
		}
	}
}

// runUsage captures what the CLI writes for one invocation. Usage
// goes to stderr, so stdout is expected to stay empty.
func runUsage(t *testing.T, args ...string) string {
	t.Helper()
	var stdout, stderr bytes.Buffer
	Run(args, strings.NewReader(""), &stdout, &stderr)
	if stdout.Len() != 0 {
		t.Errorf("Run(%v) wrote to stdout: %q", args, stdout.String())
	}
	return stderr.String()
}

// TestTheThreeHelpPathsPrintTheSameThing pins that the three ways in
// agree to the byte. They used to disagree twice over: two hand-kept
// lists that had drifted apart on membership, and `help` printing
// both of them one after the other.
func TestTheThreeHelpPathsPrintTheSameThing(t *testing.T) {
	t.Parallel()
	bare := runUsage(t)
	for _, args := range [][]string{{"--help"}, {"-help"}, {"help"}} {
		if got := runUsage(t, args...); got != bare {
			t.Errorf("`wealthdb %s` output differs from the bare invocation:\n--- bare ---\n%s\n--- %s ---\n%s",
				strings.Join(args, " "), bare, strings.Join(args, " "), got)
		}
	}
}

// TestUsageNamesEveryCommandOnce pins that each command appears in
// the rendered listing exactly once, which is what the second copy
// of the list used to break.
func TestUsageNamesEveryCommandOnce(t *testing.T) {
	t.Parallel()
	out := runUsage(t)
	for _, c := range commandHelps {
		n := 0
		for _, line := range strings.Split(out, "\n") {
			if strings.HasPrefix(strings.TrimSpace(line), c.name+" ") ||
				strings.TrimSpace(line) == c.name+"  "+c.short ||
				strings.HasPrefix(strings.TrimSpace(line), c.name+"  ") {
				n++
			}
		}
		if n != 1 {
			t.Errorf("%q appears %d times in the listing, want exactly 1", c.name, n)
		}
	}
}

// TestAHostSideCommandSaysWhichSideRunsIt pins that a command the
// listing advertises but this binary does not serve is answered with
// the wrapper that does, rather than reported as unknown.
func TestAHostSideCommandSaysWhichSideRunsIt(t *testing.T) {
	t.Parallel()
	var host string
	for _, c := range commandHelps {
		if c.hostSide {
			host = c.name
			break
		}
	}
	if host == "" {
		t.Skip("no host-side command is listed")
	}
	out := runUsage(t, host)
	if strings.Contains(out, "unknown subcommand") {
		t.Errorf("`wealthdb %s` reports the command the help lists as unknown:\n%s", host, out)
	}
	if !strings.Contains(out, "host-side") {
		t.Errorf("`wealthdb %s` does not say which side serves it:\n%s", host, out)
	}
}

// TestHelpForOneCommandNamesIt pins the per-command path, including
// the holdings views, which are reached through `holdings` and would
// otherwise read as unknown.
func TestHelpForOneCommandNamesIt(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"load", "categorize", "resolutions", "web", "positions", "global"} {
		out := runUsage(t, "help", name)
		if strings.Contains(out, "unknown subcommand") {
			t.Errorf("`wealthdb help %s` reports it as unknown:\n%s", name, out)
		}
		if !strings.Contains(out, name) {
			t.Errorf("`wealthdb help %s` does not name it:\n%s", name, out)
		}
	}
	if out := runUsage(t, "help", "no-such-command"); !strings.Contains(out, "unknown subcommand") {
		t.Errorf("`wealthdb help no-such-command` did not report it as unknown:\n%s", out)
	}
}

// TestAskingForHelpSucceeds pins the exit codes apart: an explicit
// request for help was answered, so it succeeds, while a bare
// invocation is a command missing rather than a question asked and
// stays a usage error.
func TestAskingForHelpSucceeds(t *testing.T) {
	t.Parallel()
	var stdout, stderr bytes.Buffer
	for _, args := range [][]string{{"--help"}, {"-help"}, {"help"}} {
		if code := Run(args, strings.NewReader(""), &stdout, &stderr); code != 0 {
			t.Errorf("`wealthdb %s` exited %d, want 0", strings.Join(args, " "), code)
		}
	}
	if code := Run(nil, strings.NewReader(""), &stdout, &stderr); code != 2 {
		t.Errorf("the bare invocation exited %d, want 2", code)
	}
	if code := Run([]string{"no-such-command"}, strings.NewReader(""), &stdout, &stderr); code != 2 {
		t.Errorf("an unknown subcommand exited %d, want 2", code)
	}
}
