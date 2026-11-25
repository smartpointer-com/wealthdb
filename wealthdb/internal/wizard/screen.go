package wizard

import (
	"fmt"
	"io"
	"strings"
	"unicode/utf8"
)

// Palette colours the kinds of text the setup prints, so that a title,
// a help line, an option, a command to run and a question each read as
// what they are. The zero value leaves text as it is: a pipe, a test, a
// terminal that sets NO_COLOR or cannot do colours.
type Palette struct{ on bool }

// Colors is the palette that colours when on.
func Colors(on bool) Palette { return Palette{on: on} }

// paint wraps each line of s in the SGR code, so that no colour runs
// across a line break.
func (p Palette) paint(code, s string) string {
	if !p.on || s == "" {
		return s
	}
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		if l != "" {
			lines[i] = "\x1b[" + code + "m" + l + "\x1b[0m"
		}
	}
	return strings.Join(lines, "\n")
}

// Title is a heading: bold.
func (p Palette) Title(s string) string { return p.paint("1", s) }

// Help is text that explains: dimmed, so it stands behind what it
// explains.
func (p Palette) Help(s string) string { return p.paint("2", s) }

// Key is the letter or number that picks an option: bold yellow.
func (p Palette) Key(s string) string { return p.paint("1;33", s) }

// Cmd is a command to run in the terminal: cyan.
func (p Palette) Cmd(s string) string { return p.paint("36", s) }

// Ask is a question that waits for an answer: bold green.
func (p Palette) Ask(s string) string { return p.paint("1;32", s) }

// OK is the confirmation of a change: green.
func (p Palette) OK(s string) string { return p.paint("32", s) }

// Err is an answer not taken, or a change not made: red.
func (p Palette) Err(s string) string { return p.paint("31", s) }

// Mark is the menu's marked line: reverse video.
func (p Palette) Mark(s string) string { return p.paint("7", s) }

// screen is where the setup writes. It keeps the vertical spacing in
// one place, so no print site decides it:
//
//   - never two blank lines in a row: a line break past one blank line
//     is dropped;
//   - one blank line before every question, heading and list (gap).
//
// It also keeps what was written since the last answer, less the
// confirmations of a change, which tells the menu whether a section
// left something to read.
type screen struct {
	Palette
	w io.Writer
	// nl is the number of line breaks the output ends with, 2 at most:
	// 0 in a line, 1 at the start of one, 2 after a blank line.
	nl int
	// esc is where a write stands in an escape sequence: 0 outside one,
	// 1 after ESC, 2 inside a CSI sequence. A sequence moves no cursor
	// here, so it leaves nl alone.
	esc  int
	tail strings.Builder
	// quiet keeps a write out of tail: the confirmation of a change.
	quiet bool
}

// newScreen writes to w. The output starts as after a blank line, so a
// first heading is the first line.
func newScreen(w io.Writer, p Palette) *screen { return &screen{Palette: p, w: w, nl: 2} }

func (s *screen) Write(b []byte) (int, error) {
	out := make([]byte, 0, len(b))
	for _, c := range b {
		switch {
		case s.esc == 1:
			s.esc = 0
			if c == '[' {
				s.esc = 2
			}
		case s.esc == 2:
			if c >= 0x40 && c <= 0x7e {
				s.esc = 0
			}
		case c == 0x1b:
			s.esc = 1
		case c == '\r':
		case c == '\n':
			if s.nl >= 2 {
				continue
			}
			s.nl++
		default:
			s.nl = 0
		}
		out = append(out, c)
	}
	if !s.quiet {
		s.tail.Write(out)
	}
	if _, err := s.w.Write(out); err != nil {
		return 0, err
	}
	return len(b), nil
}

// gap ends the line and leaves one blank line, unless there is one.
func (s *screen) gap() {
	for s.nl < 2 {
		fmt.Fprintln(s)
	}
}

// answered marks an answer read: the Enter that ended it moved the
// cursor to a new line, and what was written before it was read.
func (s *screen) answered() {
	s.nl = max(s.nl, 1)
	s.tail.Reset()
}

// outside marks output that did not pass through the screen, as a
// command the setup ran prints: it ended at the start of a line, and it
// stays to be read (unread).
func (s *screen) outside() {
	s.nl = 1
	s.tail.WriteString("-")
}

// unread says something was written since the last answer beyond blank
// lines and the confirmations of a change.
func (s *screen) unread() bool { return strings.TrimSpace(s.tail.String()) != "" }

// clear empties the terminal's window, for the menu.
func (s *screen) clear() {
	fmt.Fprint(s.w, "\x1b[H\x1b[2J")
	s.nl, s.esc = 2, 0
	s.tail.Reset()
}

// title prints a heading after a blank line.
func (s *screen) title(text string) {
	s.gap()
	fmt.Fprintln(s, s.Title(text))
}

// width is the column text wraps at: an 80-column window less a margin.
const width = 78

// clipLeft is s when it has at most n characters, else its last n-1
// after an ellipsis.
func clipLeft(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return "…" + string(r[len(r)-(n-1):])
}

// fit wraps each line of text longer than width at its spaces, the
// lines it adds indented as the line they come from.
func fit(text string) string {
	lines := strings.Split(text, "\n")
	var out []string
	for _, l := range lines {
		if utf8.RuneCountInString(l) <= width {
			out = append(out, l)
			continue
		}
		indent := l[:len(l)-len(strings.TrimLeft(l, " "))]
		for _, w := range wrap(strings.TrimSpace(l), width-len(indent)) {
			out = append(out, indent+w)
		}
	}
	return strings.Join(out, "\n")
}

// help prints lines that explain, wrapped to the window.
func (s *screen) help(format string, a ...any) {
	fmt.Fprintln(s, s.Help(fit(fmt.Sprintf(format, a...))))
}

// say prints plain lines, wrapped to the window.
func (s *screen) say(format string, a ...any) {
	fmt.Fprintln(s, fit(fmt.Sprintf(format, a...)))
}

// ok prints the confirmation of a change, which the menu's summary
// shows too: it leaves nothing unread.
func (s *screen) ok(format string, a ...any) {
	s.quiet = true
	fmt.Fprintln(s, s.OK(fit(fmt.Sprintf(format, a...))))
	s.quiet = false
}

// fail prints an answer not taken, or a change not made.
func (s *screen) fail(format string, a ...any) {
	fmt.Fprintln(s, s.Err(fit(fmt.Sprintf(format, a...))))
}

// options prints the choices of a question after a blank line, one to a
// line: the key that picks it, then what it does.
func (s *screen) options(opts ...[2]string) {
	s.gap()
	for _, o := range opts {
		fmt.Fprintf(s, "  %s  %s\n", s.Key(o[0]), o[1])
	}
}

// command prints a command to run, on a line of its own between blank
// lines, six columns in.
func (s *screen) command(cmd string) {
	s.gap()
	fmt.Fprintf(s, "      %s\n", s.Cmd(cmd))
	s.gap()
}
