package wizard

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"strings"
)

// errQuit is a q, or the end of the input (Ctrl+D), at any prompt: the
// command stops, and every answer confirmed before it is saved. No
// other answer ends a run: a prompt asks again until it has one it
// takes.
var errQuit = errors.New("quit")

// prompter asks questions a line at a time, each after a blank line.
type prompter struct {
	in  *bufio.Reader
	out *screen
}

// line prints the question and reads one answer, trimmed. q, or the end
// of the input, is errQuit.
func (p *prompter) line(question string) (string, error) {
	p.out.gap()
	// A question that would wrap breaks at a space, so it reads in one
	// piece; the answer follows its last line.
	trail := question[len(strings.TrimRight(question, " ")):]
	fmt.Fprint(p.out, p.out.Ask(fit(strings.TrimRight(question, " "))+trail))
	s, err := p.in.ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return "", fmt.Errorf("read input: %w", err)
	}
	if err != nil && s == "" {
		fmt.Fprintln(p.out)
		return "", errQuit
	}
	p.out.answered()
	s = strings.TrimSpace(s)
	if strings.EqualFold(s, "q") {
		return "", errQuit
	}
	return s, nil
}

// ask is line with the saved value in brackets; Enter keeps it.
func (p *prompter) ask(question, saved string) (string, error) {
	q := question + ": "
	if saved != "" {
		q = fmt.Sprintf("%s [%s]: ", question, saved)
	}
	s, err := p.line(q)
	if err != nil || s != "" {
		return s, err
	}
	return saved, nil
}

// askValid asks until check takes the answer, and returns what check
// makes of it. check's error is shown, and the question asked again.
func (p *prompter) askValid(question, saved string, check func(string) (string, error)) (string, error) {
	for {
		s, err := p.ask(question, saved)
		if err != nil {
			return "", err
		}
		v, err := check(s)
		if err == nil {
			return v, nil
		}
		p.out.fail("  %s", err.Error())
	}
}

// onOff asks for on or off, yes or no; Enter keeps saved.
func (p *prompter) onOff(question string, saved bool) (bool, error) {
	v, err := p.askValid(question+" (on or off)", onOffWord(saved), func(s string) (string, error) {
		switch strings.ToLower(s) {
		case "on", "y", "yes":
			return "on", nil
		case "off", "n", "no":
			return "off", nil
		}
		return "", errors.New("Type on or off.")
	})
	return v == "on", err
}

// yesNo asks a yes or no question; Enter is dflt.
func (p *prompter) yesNo(question string, dflt bool) (bool, error) {
	hint := "y/N"
	if dflt {
		hint = "Y/n"
	}
	for {
		s, err := p.line(fmt.Sprintf("%s [%s]: ", question, hint))
		if err != nil {
			return false, err
		}
		switch strings.ToLower(s) {
		case "":
			return dflt, nil
		case "y", "yes":
			return true, nil
		case "n", "no":
			return false, nil
		}
		p.out.fail("  Type y or n.")
	}
}

func onOffWord(on bool) string {
	if on {
		return "on"
	}
	return "off"
}
