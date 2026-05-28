// Package wizard implements `wealthdb config`'s interactive
// first-time setup. The Run entry point reads prompts from an
// io.Reader and emits text to an io.Writer, so the cmd-layer
// dispatcher can drive it in tests with scripted stdin without
// needing a real TTY.
package wizard

import (
	"bufio"
	"fmt"
	"io"
	"strings"
)

// prompter wraps the user-facing I/O so individual prompt helpers
// stay short.
type prompter struct {
	in  *bufio.Reader
	out io.Writer
}

func newPrompter(in io.Reader, out io.Writer) *prompter {
	return &prompter{in: bufio.NewReader(in), out: out}
}

// ask prints a question and a [default] in brackets (omitted if
// dflt is empty), returns the typed line trimmed. Empty input
// returns the default.
func (p *prompter) ask(question, dflt string) (string, error) {
	if dflt != "" {
		fmt.Fprintf(p.out, "%s [%s]: ", question, dflt)
	} else {
		fmt.Fprintf(p.out, "%s: ", question)
	}
	line, err := p.in.ReadString('\n')
	if err != nil && err != io.EOF {
		return "", fmt.Errorf("read input: %w", err)
	}
	s := strings.TrimSpace(line)
	if s == "" {
		return dflt, nil
	}
	return s, nil
}

// askValidated re-prompts until the supplied validator accepts
// the input. The validator returns a non-nil error to reject;
// the error's text is shown to the user and the prompt repeats.
// Limited to 5 attempts so a misbehaving piped input can't loop
// forever.
func (p *prompter) askValidated(question, dflt string, validate func(string) error) (string, error) {
	const maxAttempts = 5
	for i := 0; i < maxAttempts; i++ {
		v, err := p.ask(question, dflt)
		if err != nil {
			return "", err
		}
		if vErr := validate(v); vErr != nil {
			fmt.Fprintf(p.out, "  ✗ %s\n", vErr.Error())
			continue
		}
		return v, nil
	}
	return "", fmt.Errorf("too many invalid answers for %q", question)
}

// askYesNo accepts y/yes/n/no (case-insensitive). dflt must be
// true or false; the [Y/n] / [y/N] hint reflects it. Empty input
// returns the default.
func (p *prompter) askYesNo(question string, dflt bool) (bool, error) {
	hint := "y/N"
	if dflt {
		hint = "Y/n"
	}
	const maxAttempts = 5
	for i := 0; i < maxAttempts; i++ {
		fmt.Fprintf(p.out, "%s [%s]: ", question, hint)
		line, err := p.in.ReadString('\n')
		if err != nil && err != io.EOF {
			return false, fmt.Errorf("read input: %w", err)
		}
		s := strings.ToLower(strings.TrimSpace(line))
		switch s {
		case "":
			return dflt, nil
		case "y", "yes":
			return true, nil
		case "n", "no":
			return false, nil
		}
		fmt.Fprintf(p.out, "  ✗ please answer y or n\n")
	}
	return false, fmt.Errorf("too many invalid answers for yes/no")
}
