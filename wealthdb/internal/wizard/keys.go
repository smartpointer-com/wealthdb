package wizard

import (
	"bufio"
	"errors"
	"io"
)

// Key is one key press, as the menu reads it in raw mode.
type Key int

// The keys the menu tells apart.
const (
	KeyOther Key = iota
	KeyUp
	KeyDown
	KeyEnter
	KeyDigit
	KeyQuit      // q
	KeyInterrupt // Ctrl+C, a byte in raw mode
	KeyEOF       // Ctrl+D, or the end of the input
)

// ReadKey reads one key press from a terminal in raw mode. It returns
// the digit pressed for KeyDigit. An escape sequence is read whole:
// ESC [ or ESC O, any parameters, and a final byte, of which A is up and
// B is down. An ESC followed by anything else is that next key.
func ReadKey(r *bufio.Reader) (Key, byte, error) {
	b, err := r.ReadByte()
	if errors.Is(err, io.EOF) {
		return KeyEOF, 0, nil
	}
	if err != nil {
		return KeyOther, 0, err
	}
	switch {
	case b == 0x1b:
		next, err := r.ReadByte()
		if err != nil {
			return KeyOther, 0, nil
		}
		if next != '[' && next != 'O' {
			_ = r.UnreadByte()
			return ReadKey(r)
		}
		for {
			c, err := r.ReadByte()
			if err != nil {
				return KeyOther, 0, nil
			}
			if c >= 0x40 && c <= 0x7e {
				switch c {
				case 'A':
					return KeyUp, 0, nil
				case 'B':
					return KeyDown, 0, nil
				}
				return KeyOther, 0, nil
			}
		}
	case b == '\r' || b == '\n':
		return KeyEnter, 0, nil
	case b >= '0' && b <= '9':
		return KeyDigit, b - '0', nil
	case b == 'q' || b == 'Q':
		return KeyQuit, 0, nil
	case b == 0x03:
		return KeyInterrupt, 0, nil
	case b == 0x04:
		return KeyEOF, 0, nil
	}
	return KeyOther, 0, nil
}
