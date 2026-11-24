package wizard

import (
	"bufio"
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/config"
)

// The screen keeps the spacing: never two blank lines in a row, and one
// blank line before a question, whatever the print sites write.
func TestScreenSpacing(t *testing.T) {
	var b bytes.Buffer
	s := newScreen(&b, Palette{})
	fmt.Fprint(s, "\n\nfirst\n\n\n\nsecond\n")
	s.gap()
	s.gap()
	fmt.Fprint(s, "third")
	s.gap()
	fmt.Fprint(s, "\nfourth\n")
	if got, want := b.String(), "first\n\nsecond\n\nthird\n\nfourth\n"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}

	// An answer's Enter moved the cursor to a new line: one line break
	// more is the blank line.
	b.Reset()
	s = newScreen(&b, Palette{})
	p := &prompter{in: bufio.NewReader(strings.NewReader("a\nb\n")), out: s}
	fmt.Fprint(s, "Help.\n")
	if _, err := p.line("\n\nFirst: "); err != nil {
		t.Fatal(err)
	}
	if _, err := p.line("Second: "); err != nil {
		t.Fatal(err)
	}
	s.ok("Saved: b.")
	if got, want := b.String(), "Help.\n\nFirst: \nSecond: Saved: b.\n"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// A colour moves no cursor: it neither hides a blank line nor makes one,
// and a line break in raw mode (\r\n) counts as one.
func TestScreenSpacingWithColours(t *testing.T) {
	var b bytes.Buffer
	s := newScreen(&b, Colors(true))
	s.title("Title")
	s.help("help")
	s.gap()
	fmt.Fprint(s, s.Key("a")+"  option\r\n\r\n\r\n")
	s.title("Next")
	want := "\x1b[1mTitle\x1b[0m\n\x1b[2mhelp\x1b[0m\n\n\x1b[1;33ma\x1b[0m  option\r\n\r\n\r\x1b[1mNext\x1b[0m\n"
	if got := b.String(); got != want {
		t.Errorf("got  %q\nwant %q", got, want)
	}
}

// A palette that is off changes nothing, and one that is on never lets a
// colour run across a line break.
func TestPalette(t *testing.T) {
	off, on := Palette{}, Colors(true)
	text := "one\n\ntwo"
	for name, f := range map[string]func(Palette, string) string{
		"Title": Palette.Title, "Help": Palette.Help, "Key": Palette.Key, "Cmd": Palette.Cmd,
		"Ask": Palette.Ask, "OK": Palette.OK, "Err": Palette.Err, "Mark": Palette.Mark,
	} {
		if got := f(off, text); got != text {
			t.Errorf("%s, off: %q", name, got)
		}
		got := f(on, text)
		for _, line := range strings.Split(got, "\n") {
			if line != "" && (!strings.HasPrefix(line, "\x1b[") || !strings.HasSuffix(line, "\x1b[0m")) {
				t.Errorf("%s: line %q is not closed", name, line)
			}
		}
		if strings.Count(got, "\n") != 2 || f(on, "") != "" {
			t.Errorf("%s: %q", name, got)
		}
	}
}

// What is unread after the last answer decides the menu's pause: the
// confirmation of a change is not, anything else is.
func TestScreenUnread(t *testing.T) {
	var b bytes.Buffer
	s := newScreen(&b, Colors(true))
	fmt.Fprintln(s, "before the answer")
	s.answered()
	s.ok("Saved: x.")
	s.gap()
	if s.unread() {
		t.Error("a confirmation counts as unread")
	}
	s.help("more to read")
	if !s.unread() {
		t.Error("a line to read does not count")
	}
	s.clear()
	if s.unread() {
		t.Error("a cleared window holds something unread")
	}
}

// echo is an input whose answers show in the output as a terminal shows
// them: the typed line, and the line break of its Enter.
type echo struct {
	in  *bufio.Reader
	out *bytes.Buffer
}

func (e *echo) Read(p []byte) (int, error) {
	line, err := e.in.ReadString('\n')
	e.out.WriteString(line)
	return copy(p, line), err
}

// Every screen of the setup keeps the rules, as a terminal shows it: no
// two blank lines in a row, and a blank line before every question.
func TestEveryQuestionStandsAfterABlankLine(t *testing.T) {
	flows := map[string][]string{
		"wizard":  {"", num(t, "plaid"), "", num(t, "plaid"), "chase", num(t, "viac"), "a", "", "", "on", "off", ""},
		"banks":   {"y", "a", "", "?", ""},
		"model":   {"http://localhost:1/v1", "", "m", "", "y"},
		"options": {"a", "3", "c", "40", "7", "e", "on", "h", "-", ""},
		"ledgers": {"a", "/elsewhere/pins.csv", "", "c", "", ""},
		"file":    nil,
		"data":    nil,
	}
	for name, answers := range flows {
		h := newHarness(t)
		h.inContainer = true
		if err := os.MkdirAll(filepath.Dir(h.cfg), 0o700); err != nil {
			t.Fatal(err)
		}
		if name != "wizard" {
			h.write(basics+"}\n", 0o600)
			h.section = name
		}
		h.collect("plaid", "schwab", "schwab.db")
		var out bytes.Buffer
		in := &echo{in: bufio.NewReader(strings.NewReader(strings.Join(answers, "\n") + "\n")), out: &out}
		err := Run(Options{ConfigPath: h.cfg, DataRoot: h.root, In: in, Out: &out, Section: h.section, Colors: Colors(true),
			InContainer: true, Actions: Actions{TestModel: func(m config.ModelConfig) (string, error) { return "OK", nil }}})
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		text := out.String()
		if strings.Contains(text, "\n\n\n") {
			t.Errorf("%s: two blank lines in a row:\n%q", name, text)
		}
		// No line is wider than an 80-column window. A path names a scratch
		// folder here, longer than a real one, so lines that show it are
		// left out.
		plain := regexp.MustCompile("\x1b\\[[0-9;]*m").ReplaceAllString(text, "")
		for _, line := range strings.Split(plain, "\n") {
			if n := utf8.RuneCountInString(line); n > 80 && !strings.Contains(line, h.root) && !strings.Contains(line, h.cfg) {
				t.Errorf("%s: a line of %d columns: %q", name, n, line)
			}
		}
		// A question is the text in the colour of Ask; its first line
		// follows a blank line.
		const ask = "\x1b[1;32m"
		asked := 0
		for i := strings.Index(text, ask); i >= 0; {
			before := text[:i]
			if !strings.HasSuffix(before, "\n\n") && !strings.HasSuffix(before, "\x1b[0m\n") {
				t.Errorf("%s: a question with no blank line before it: %q", name, text[max(0, i-60):min(len(text), i+60)])
			}
			asked++
			j := strings.Index(text[i+1:], ask)
			if j < 0 {
				break
			}
			i += 1 + j
		}
		if asked == 0 && len(answers) > 0 {
			t.Errorf("%s: no question found", name)
		}
	}
}
