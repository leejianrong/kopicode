package lineedit

import (
	"fmt"
	"os"

	"golang.org/x/term"
)

// Terminal is the raw-mode seam, and the reason the rest of this package can be
// tested at all.
//
// Everything platform-specific in a line editor lives behind these two methods:
// whether editing is possible, and how to turn the terminal's line discipline
// off and back on. termios and the Windows console API disagree about all of
// it, golang.org/x/term is the one dependency that papers over the difference,
// and a test cannot be a TTY. Keeping the raw-mode transition at the edge means
// the editing logic is driven by bytes in a test and by a real terminal in
// production, over identical code.
type Terminal interface {
	// IsInteractive reports whether raw-mode editing is possible. False sends
	// the editor down the plain-reader path.
	IsInteractive() bool

	// MakeRaw puts the terminal into raw mode and returns the function that
	// puts it back. It is called once per ReadLine, so the terminal spends
	// the time between lines in its normal state.
	MakeRaw() (restore func() error, err error)
}

// OSTerminal drives a real terminal through golang.org/x/term.
//
// Interactive means **both** ends are terminals. Raw mode is a property of the
// input side, but every escape the editor writes to redraw a line goes to the
// output side, and `kopicode > transcript.txt` with a keyboard attached would
// otherwise fill the file with cursor-movement escapes — which SLICE-1's
// acceptance criterion for piped output forbids outright.
func OSTerminal(in, out *os.File) Terminal {
	return &osTerminal{in: in, out: out}
}

type osTerminal struct {
	in, out *os.File
}

func (t *osTerminal) IsInteractive() bool {
	if t.in == nil || t.out == nil {
		return false
	}
	return term.IsTerminal(int(t.in.Fd())) && term.IsTerminal(int(t.out.Fd()))
}

func (t *osTerminal) MakeRaw() (func() error, error) {
	fd := int(t.in.Fd())
	state, err := term.MakeRaw(fd)
	if err != nil {
		return nil, fmt.Errorf("entering raw mode on %s: %w", t.in.Name(), err)
	}
	return func() error {
		if err := term.Restore(fd, state); err != nil {
			return fmt.Errorf("restoring %s: %w", t.in.Name(), err)
		}
		return nil
	}, nil
}

// NonInteractive is a Terminal that never enters raw mode: reading degrades to
// plain lines with no editing and no escape sequences.
//
// It is the default when a Config names no Terminal, because that is the safe
// direction to be wrong in. An editor that wrongly believes it has a terminal
// writes cursor escapes into a pipe and waits for keys that will never be
// distinguished from text; one that wrongly believes it has a pipe merely
// offers no arrow keys.
func NonInteractive() Terminal { return nonInteractive{} }

type nonInteractive struct{}

func (nonInteractive) IsInteractive() bool { return false }

func (nonInteractive) MakeRaw() (func() error, error) {
	// Unreachable through ReadLine, which checks IsInteractive first. An
	// error rather than a no-op, so a future caller that gets the order
	// wrong finds out.
	return nil, fmt.Errorf("lineedit: MakeRaw on a non-interactive terminal")
}
