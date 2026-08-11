// Package lineedit is the minimal line editor behind kopicode's REPL prompt:
// raw-mode input with history, arrow keys, and Ctrl-A/E/K/U.
//
// It implements the input half of docs/adr/0004-line-oriented-repl.md, which
// buys a "small line-editor dependency" and refuses a TUI framework. Nothing
// here owns the screen. The editor repaints the current line and never touches
// scrollback, so the terminal keeps its selection and its history, and a
// crash leaves a terminal that still works.
//
// # Why this lives under cmd/
//
// ADR-0003 says a front end may import internal/engine and internal/bench and
// nothing else, and internal/arch enforces it. A line editor is presentation —
// it is how a surface asks for input, and the engine has no opinion about it —
// so it belongs to the surface that uses it rather than to the engine's
// internals. Putting it in internal/ would have meant widening the boundary
// guard to let it back out again, which is the wrong direction to fix a
// misplaced package.
//
// # Scope, and what is left to the REPL (KAN-793)
//
// This package produces a line. It does not print model output, does not
// stream, and does not know what a turn is.
//
//   - **Ctrl-C at the prompt** is [ErrInterrupted]: the line in progress is
//     discarded and the caller is expected to print a fresh prompt with the
//     session alive.
//   - **Ctrl-C during a turn** is not this package's problem, and by design it
//     does not have to be. Raw mode is entered and left per ReadLine call, so
//     while the engine is working the terminal is in its normal line
//     discipline and Ctrl-C is an ordinary SIGINT that the REPL catches with
//     signal.Notify. ADR-0004's "interrupt is honoured mid-stream" is
//     therefore a plain signal handler rather than a second keyboard reader
//     racing the first.
//   - **Ctrl-D on an empty line** is io.EOF, which the REPL should read as
//     "end the session". On a line with text it deletes forward, as readline
//     does.
//
// # Contracts worth knowing before you call it
//
// The prompt must be plain text with no escape sequences. It is measured in
// runes to place the cursor, and it is written verbatim on the non-interactive
// path where SLICE-1 requires output to be escape-free.
//
// A single ReadLine runs at a time; the editor is not safe for concurrent
// reads. [Editor.Restore] is the exception and is safe from anywhere,
// including a signal handler.
//
// Lines longer than the terminal is wide will wrap, and the repaint does not
// account for the wrap. Multi-line editing is not in this card.
package lineedit

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"sync"
	"unicode/utf8"
)

// ErrInterrupted is returned when the user presses Ctrl-C at the prompt. The
// partial line is discarded and no history entry is made.
//
// It is a sentinel rather than a bare error so a caller can tell "the user
// changed their mind" from "the terminal broke", which are the same thing to a
// string comparison and very different to a REPL.
var ErrInterrupted = errors.New("lineedit: interrupted")

// Config assembles an Editor. Every field has a usable zero value except the
// streams, which default to the process's own.
type Config struct {
	// In is where keystrokes come from. Defaults to os.Stdin.
	In io.Reader
	// Out is where the prompt and the repainted line go. Defaults to os.Stdout.
	Out io.Writer
	// Terminal is the raw-mode seam. Defaults to NonInteractive, which is the
	// safe direction: no raw mode, no escapes, plain line reading.
	Terminal Terminal
	// Prompt is written before each line. Plain text only — see the package
	// comment.
	Prompt string
	// History seeds the recall list, oldest first.
	History []string
}

// Editor reads one line at a time.
type Editor struct {
	in       *bufio.Reader
	out      io.Writer
	terminal Terminal

	prompt      string
	promptWidth int

	history []string

	// mu guards restore only. It exists because Restore is documented as
	// callable from a signal handler, which means from another goroutine,
	// while ReadLine is blocked in a read.
	mu      sync.Mutex
	restore func() error
}

// New builds an Editor from cfg.
func New(cfg Config) *Editor {
	in := cfg.In
	if in == nil {
		in = os.Stdin
	}
	out := cfg.Out
	if out == nil {
		out = os.Stdout
	}
	terminal := cfg.Terminal
	if terminal == nil {
		terminal = NonInteractive()
	}

	e := &Editor{
		in:       bufio.NewReader(in),
		out:      out,
		terminal: terminal,
		history:  append([]string(nil), cfg.History...),
	}
	e.SetPrompt(cfg.Prompt)
	return e
}

// Stdio is the constructor a REPL wants: the process's own streams, with real
// terminal detection.
func Stdio(prompt string) *Editor {
	return New(Config{
		In:       os.Stdin,
		Out:      os.Stdout,
		Terminal: OSTerminal(os.Stdin, os.Stdout),
		Prompt:   prompt,
	})
}

// SetPrompt changes the prompt for subsequent reads.
func (e *Editor) SetPrompt(prompt string) {
	e.prompt = prompt
	e.promptWidth = utf8.RuneCountInString(prompt)
}

// History returns the recall list, oldest first.
func (e *Editor) History() []string {
	return append([]string(nil), e.history...)
}

// Restore puts the terminal back if this editor left it in raw mode, and does
// nothing otherwise.
//
// ReadLine already defers this, including on a panic, so the normal path needs
// no help. It is exported for the paths a defer cannot reach: a SIGTERM or a
// SIGHUP handler, which runs on another goroutine while ReadLine is blocked in
// a read that will never return. Calling it there is what stops a killed
// session leaving the user typing blind into a terminal with no echo.
//
// It is idempotent and safe to call concurrently. Nothing can help with
// SIGKILL, and a REPL that wants to survive that should say so in its own
// documentation rather than pretend here.
func (e *Editor) Restore() error {
	e.mu.Lock()
	restore := e.restore
	e.restore = nil
	e.mu.Unlock()

	if restore == nil {
		return nil
	}
	return restore()
}

// ReadLine reads and returns one line, without its terminator.
//
// It returns [ErrInterrupted] on Ctrl-C and io.EOF when input ends. A line
// that arrives before an unterminated EOF is returned with a nil error, and the
// io.EOF comes on the next call — the same contract bufio has, so a script
// whose last line lacks a newline is not silently dropped.
func (e *Editor) ReadLine() (string, error) {
	if !e.terminal.IsInteractive() {
		return e.readPlain()
	}

	restore, err := e.terminal.MakeRaw()
	if err != nil {
		return "", fmt.Errorf("lineedit: %w", err)
	}
	e.mu.Lock()
	e.restore = restore
	e.mu.Unlock()

	// Deferred rather than called at each return, so a panic anywhere below —
	// in a write to a closed pipe, in the engine's own code if it ever calls
	// in here — still hands the terminal back. A raw terminal surviving a
	// crash is the exact robustness failure ADR-0004 refuses a TUI to avoid.
	defer func() {
		_ = e.Restore()
	}()

	return e.readInteractive()
}

// readPlain is the no-terminal path: no raw mode, no editing, and **not one
// escape byte written**. SLICE-1 requires piped output to be valid
// line-oriented text, and the way to guarantee that is for this path to have no
// code that could emit an escape rather than for it to strip them afterwards.
func (e *Editor) readPlain() (string, error) {
	if _, err := io.WriteString(e.out, e.prompt); err != nil {
		return "", fmt.Errorf("lineedit: writing prompt: %w", err)
	}

	text, err := e.in.ReadString('\n')
	if err != nil {
		if errors.Is(err, io.EOF) {
			if text == "" {
				return "", io.EOF
			}
			// Final line with no trailing newline. Return it now; the next
			// call reports the EOF.
			e.remember(text)
			return text, nil
		}
		return "", fmt.Errorf("lineedit: reading input: %w", err)
	}

	text = strings.TrimSuffix(text, "\n")
	text = strings.TrimSuffix(text, "\r")
	e.remember(text)
	return text, nil
}

// readInteractive runs the key loop with the terminal already in raw mode.
func (e *Editor) readInteractive() (string, error) {
	keys := &keyReader{r: e.in}
	s := newSession(e.history)

	if err := e.redraw(&s.line); err != nil {
		return "", err
	}

	for {
		k, r, err := keys.next()
		if err != nil {
			if errors.Is(err, io.EOF) {
				return e.finishAtEOF(s)
			}
			return "", fmt.Errorf("lineedit: reading input: %w", err)
		}

		switch s.handle(k, r) {
		case outcomeAccept:
			text := s.line.String()
			if err := e.writeString("\r\n"); err != nil {
				return "", err
			}
			e.remember(text)
			return text, nil

		case outcomeInterrupt:
			// ^C is echoed because raw mode turned off the echo the tty
			// would normally do, and a Ctrl-C that leaves no mark looks
			// like the editor hung.
			if err := e.writeString("^C\r\n"); err != nil {
				return "", err
			}
			return "", ErrInterrupted

		case outcomeEOF:
			if err := e.writeString("\r\n"); err != nil {
				return "", err
			}
			return "", io.EOF

		case outcomeContinue, outcomeUnhandled:
			// outcomeUnhandled is unreachable — key_internal_test.go fails
			// the suite for any key constant that reaches it. Repainting is
			// the right thing to do anyway: a user's session is not the
			// place to panic over a missing case.
		}

		if err := e.redraw(&s.line); err != nil {
			return "", err
		}
	}
}

// finishAtEOF handles input ending mid-read: a line already typed is returned,
// an empty one becomes io.EOF.
func (e *Editor) finishAtEOF(s *session) (string, error) {
	if err := e.writeString("\r\n"); err != nil {
		return "", err
	}
	if s.line.empty() {
		return "", io.EOF
	}
	text := s.line.String()
	e.remember(text)
	return text, nil
}

// remember appends to history, skipping blanks and immediate repeats — the two
// entries nobody has ever wanted to arrow back to.
func (e *Editor) remember(text string) {
	if strings.TrimSpace(text) == "" {
		return
	}
	if n := len(e.history); n > 0 && e.history[n-1] == text {
		return
	}
	e.history = append(e.history, text)
}

// csiClearToEnd erases from the cursor to the end of the line.
const csiClearToEnd = "\x1b[K"

// redraw repaints the current line in place.
//
// One line, always the current one, never scrollback — ADR-0004 decision 3.
// The sequence is: return to column zero, erase the line, write prompt and
// text, return to column zero again, then move right to where the cursor
// belongs. Repainting the whole line on every keystroke is more bytes than a
// clever incremental update and cannot get out of step with the buffer, which
// at one prompt's worth of text is the right trade.
func (e *Editor) redraw(l *line) error {
	var b strings.Builder
	b.WriteString("\r")
	b.WriteString(csiClearToEnd)
	b.WriteString(e.prompt)
	b.WriteString(l.String())
	b.WriteString("\r")
	if n := e.promptWidth + l.pos; n > 0 {
		b.WriteString("\x1b[")
		b.WriteString(strconv.Itoa(n))
		b.WriteString("C")
	}
	return e.writeString(b.String())
}

func (e *Editor) writeString(s string) error {
	if _, err := io.WriteString(e.out, s); err != nil {
		return fmt.Errorf("lineedit: writing to the terminal: %w", err)
	}
	return nil
}
