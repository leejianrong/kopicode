package lineedit_test

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"testing/iotest"

	"github.com/google/go-cmp/cmp"

	"github.com/leejianrong/kopicode/cmd/kopicode/lineedit"
)

// A test process is not a terminal and never will be, so the whole suite runs
// against this: the raw-mode seam from terminal.go, with the platform half
// replaced by a counter.
//
// It is what makes the editing logic testable headless, which is the design
// problem this card actually had. Everything below drives the editor with the
// same byte sequences a terminal sends and asserts on what comes back out —
// the resulting line and the bytes written — never on cursor state or on which
// method got called.
type fakeTerminal struct {
	interactive bool

	mu       sync.Mutex
	rawCalls int
	restores int
	makeErr  error
}

func (t *fakeTerminal) IsInteractive() bool { return t.interactive }

func (t *fakeTerminal) MakeRaw() (func() error, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.makeErr != nil {
		return nil, t.makeErr
	}
	t.rawCalls++
	return func() error {
		t.mu.Lock()
		defer t.mu.Unlock()
		t.restores++
		return nil
	}, nil
}

func (t *fakeTerminal) counts() (raw, restored int) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.rawCalls, t.restores
}

// interactiveEditor wires an editor to a byte script, as though the script had
// been typed at a terminal.
func interactiveEditor(input string) (*lineedit.Editor, *fakeTerminal, *bytes.Buffer) {
	term := &fakeTerminal{interactive: true}
	out := &bytes.Buffer{}
	e := lineedit.New(lineedit.Config{
		In:       strings.NewReader(input),
		Out:      out,
		Terminal: term,
		Prompt:   "> ",
	})
	return e, term, out
}

// readOne drives one line and fails on anything but a clean read.
func readOne(t *testing.T, e *lineedit.Editor) string {
	t.Helper()
	got, err := e.ReadLine()
	if err != nil {
		t.Fatalf("ReadLine: %v", err)
	}
	return got
}

// rep spells "press this key n times", which the scripts below do constantly.
func rep(keystroke string, n int) string { return strings.Repeat(keystroke, n) }

// Escape sequences, spelled once, so the scripts below read as keystrokes.
const (
	up        = "\x1b[A"
	down      = "\x1b[B"
	right     = "\x1b[C"
	left      = "\x1b[D"
	delKey    = "\x1b[3~"
	homeKey   = "\x1b[H"
	endKey    = "\x1b[F"
	enter     = "\r"
	backspace = "\x7f"
	ctrlA     = "\x01"
	ctrlC     = "\x03"
	ctrlD     = "\x04"
	ctrlE     = "\x05"
	ctrlK     = "\x0b"
	ctrlU     = "\x15"
)

// TestBindings is the card's acceptance criterion: history, arrows, and
// Ctrl-A/E/K/U, each doing what a user of any other shell expects.
func TestBindings(t *testing.T) {
	cases := []struct {
		name  string
		input string
		want  string
	}{
		{"plain typing", "hello" + enter, "hello"},
		{"empty line", enter, ""},

		{"left then insert", "helo" + left + "l" + enter, "hello"},
		{"left right cancel out", "abc" + left + left + right + "X" + enter, "abXc"},
		{"left stops at the start", "ab" + rep(left, 4) + "X" + enter, "Xab"},
		{"right stops at the end", "ab" + rep(right, 2) + "X" + enter, "abX"},

		{"ctrl-a goes to the start", "world" + ctrlA + "hello " + enter, "hello world"},
		{"ctrl-e goes to the end", "abc" + ctrlA + ctrlE + "!" + enter, "abc!"},

		{"ctrl-k kills to the end", "hello world" + ctrlA + ctrlK + "bye" + enter, "bye"},
		{"ctrl-k from mid-line", "hello world" + rep(left, 5) + ctrlK + enter, "hello "},
		{"ctrl-k at the end does nothing", "hello" + ctrlK + enter, "hello"},

		{"ctrl-u kills to the start", "hello world" + ctrlU + "bye" + enter, "bye"},
		{"ctrl-u from mid-line", "hello world" + rep(left, 5) + ctrlU + enter, "world"},
		{"ctrl-u at the start does nothing", "hello" + ctrlA + ctrlU + enter, "hello"},

		{"backspace", "helllo" + rep(left, 2) + backspace + enter, "hello"},
		{"backspace at the start does nothing", "abc" + ctrlA + backspace + enter, "abc"},

		{"delete key", "hxello" + ctrlA + right + delKey + enter, "hello"},
		{"ctrl-d mid-line deletes forward", "hxello" + ctrlA + right + ctrlD + enter, "hello"},
		{"ctrl-d at the end does nothing", "abc" + ctrlD + enter, "abc"},

		{"home key", "world" + homeKey + "hello " + enter, "hello world"},
		{"end key", "abc" + ctrlA + endKey + "!" + enter, "abc!"},

		{"newline is also enter", "hello\n", "hello"},
		{"unbound control bytes are dropped", "he\x00l\x1clo" + enter, "hello"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e, _, _ := interactiveEditor(tc.input)
			if got := readOne(t, e); got != tc.want {
				t.Errorf("ReadLine() = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestApplicationCursorKeys covers the arrows as a terminal sends them in
// application cursor mode — ESC O A rather than ESC [ A. Half the terminals in
// use send these, and an editor that only knows CSI has arrow keys that work
// on the author's machine.
func TestApplicationCursorKeys(t *testing.T) {
	e, _, _ := interactiveEditor("helo\x1bODl" + enter)
	if got := readOne(t, e); got != "hello" {
		t.Errorf("ReadLine() = %q, want %q", got, "hello")
	}
}

// TestHistory walks the recall list in both directions, including the part
// everyone forgets: coming back down past the newest entry restores the line
// that was being typed when navigation started.
func TestHistory(t *testing.T) {
	script := strings.Join([]string{
		"first" + enter,
		"second" + enter,
		rep(up, 2) + enter,              // back two: "first"
		rep(up, 4) + enter,              // walking off the top stops at the oldest
		"draft" + up + down + enter,     // up then back down restores the draft
		"x" + up + rep(down, 2) + enter, // walking off the bottom stays on the draft
	}, "")

	e, _, _ := interactiveEditor(script)

	want := []string{"first", "second", "first", "first", "draft", "x"}
	var got []string
	for range want {
		got = append(got, readOne(t, e))
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("lines (-want +got):\n%s", diff)
	}
}

// TestHistorySkipsBlanksAndRepeats keeps the recall list worth arrowing
// through. Neither entry has ever been wanted.
func TestHistorySkipsBlanksAndRepeats(t *testing.T) {
	script := "a" + enter + enter + "   " + enter + "a" + enter + "b" + enter
	e, _, _ := interactiveEditor(script)
	for range 5 {
		readOne(t, e)
	}
	want := []string{"a", "b"}
	if diff := cmp.Diff(want, e.History()); diff != "" {
		t.Errorf("history (-want +got):\n%s", diff)
	}
}

// TestSeededHistoryIsRecallable is what lets a REPL persist history across
// sessions without this package knowing where it is stored.
func TestSeededHistoryIsRecallable(t *testing.T) {
	term := &fakeTerminal{interactive: true}
	e := lineedit.New(lineedit.Config{
		In:       strings.NewReader(up + up + enter),
		Out:      &bytes.Buffer{},
		Terminal: term,
		History:  []string{"older", "newer"},
	})
	if got := readOne(t, e); got != "older" {
		t.Errorf("ReadLine() = %q, want %q", got, "older")
	}
}

// splitReader hands out a fixed sequence of chunks, one per Read call. It is
// how a terminal that delivers half an escape sequence is reproduced on
// demand.
type splitReader struct {
	chunks []string
}

func (r *splitReader) Read(p []byte) (int, error) {
	for len(r.chunks) > 0 && r.chunks[0] == "" {
		r.chunks = r.chunks[1:]
	}
	if len(r.chunks) == 0 {
		return 0, io.EOF
	}
	n := copy(p, r.chunks[0])
	r.chunks[0] = r.chunks[0][n:]
	return n, nil
}

// TestSplitEscapeSequences is the proof for the failure mode that only shows up
// on someone else's machine.
//
// An arrow key is ESC [ A and nothing guarantees the three bytes arrive in one
// Read. A decoder that inspects whatever a single Read returned works
// perfectly against a fast local terminal and mangles input over a pipe, an ssh
// session or a loaded machine — the shape of bug that passes every test anyone
// runs by hand.
//
// So this does not test one plausible split. It tests **every** split of a
// script full of escape sequences, and separately the pathological case of one
// byte per Read. If the decoder ever starts caring about chunk boundaries, one
// of these fails.
func TestSplitEscapeSequences(t *testing.T) {
	const script = "helo" + left + "l" + endKey + "!" + up + delKey + enter
	const want = "hello!"

	t.Run("every split point", func(t *testing.T) {
		for i := range len(script) + 1 {
			term := &fakeTerminal{interactive: true}
			e := lineedit.New(lineedit.Config{
				In:       &splitReader{chunks: []string{script[:i], script[i:]}},
				Out:      &bytes.Buffer{},
				Terminal: term,
				Prompt:   "> ",
			})
			got, err := e.ReadLine()
			if err != nil {
				t.Fatalf("split after byte %d: ReadLine: %v", i, err)
			}
			if got != want {
				t.Fatalf("split after byte %d: ReadLine() = %q, want %q\n"+
					"the decoder is treating a Read boundary as a sequence boundary",
					i, got, want)
			}
		}
	})

	t.Run("one byte per read", func(t *testing.T) {
		term := &fakeTerminal{interactive: true}
		e := lineedit.New(lineedit.Config{
			In:       iotest.OneByteReader(strings.NewReader(script)),
			Out:      &bytes.Buffer{},
			Terminal: term,
			Prompt:   "> ",
		})
		if got := readOne(t, e); got != want {
			t.Errorf("ReadLine() = %q, want %q", got, want)
		}
	})
}

// TestUTF8 covers the reason the cursor is a rune index and not a byte index.
//
// A byte cursor lets Left land between the two bytes of "é", and the line
// handed to the engine then contains a broken sequence — which no amount of
// ASCII testing finds. Display *width* is a separate problem and is out of
// scope: a CJK ideograph edits correctly here and draws its cursor one column
// off, which line.go says out loud.
func TestUTF8(t *testing.T) {
	cases := []struct {
		name  string
		input string
		want  string
	}{
		{"insert multi-byte runes", "héllo" + enter, "héllo"},
		{"left steps over a whole rune", "héllo" + rep(left, 4) + "X" + enter, "hXéllo"},
		{"backspace removes a whole rune", "héllo" + rep(left, 3) + backspace + enter, "hllo"},
		{"delete removes a whole rune", "héllo" + rep(left, 4) + delKey + enter, "hllo"},
		{"kill to end after a multi-byte rune", "héllo" + rep(left, 3) + ctrlK + enter, "hé"},
		{"kill to start before a multi-byte rune", "héllo" + rep(left, 3) + ctrlU + enter, "llo"},
		{"wide runes edit correctly", "日本語" + left + "X" + enter, "日本X語"},
		{"emoji", "ok 👍" + backspace + "!" + enter, "ok !"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e, _, _ := interactiveEditor(tc.input)
			if got := readOne(t, e); got != tc.want {
				t.Errorf("ReadLine() = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestMultiByteRuneSplitAcrossReads is the UTF-8 half of the split-read
// problem: a two-byte rune arriving one byte at a time must not decode as two
// replacement characters.
func TestMultiByteRuneSplitAcrossReads(t *testing.T) {
	const script = "caf\xc3\xa9" + enter
	for i := range len(script) + 1 {
		term := &fakeTerminal{interactive: true}
		e := lineedit.New(lineedit.Config{
			In:       &splitReader{chunks: []string{script[:i], script[i:]}},
			Out:      &bytes.Buffer{},
			Terminal: term,
		})
		got, err := e.ReadLine()
		if err != nil {
			t.Fatalf("split after byte %d: %v", i, err)
		}
		if got != "café" {
			t.Fatalf("split after byte %d: ReadLine() = %q, want %q", i, got, "café")
		}
	}
}

// TestInterrupt pins the editor half of Ctrl-C, which KAN-793 inherits: the
// line in progress is discarded, the error is a sentinel the REPL can match on,
// and the *session* is untouched — the very next ReadLine works.
//
// Ctrl-C during a turn is deliberately not modelled here. Raw mode is entered
// and left per ReadLine, so while the engine is working the terminal is in its
// normal line discipline and Ctrl-C is an ordinary SIGINT for the REPL to
// catch.
func TestInterrupt(t *testing.T) {
	e, term, out := interactiveEditor("abandon me" + ctrlC + "kept" + enter)

	got, err := e.ReadLine()
	if !errors.Is(err, lineedit.ErrInterrupted) {
		t.Fatalf("ReadLine after Ctrl-C: err = %v, want ErrInterrupted", err)
	}
	if got != "" {
		t.Errorf("ReadLine after Ctrl-C returned %q, want the partial line discarded", got)
	}
	if !strings.Contains(out.String(), "^C") {
		t.Error("no ^C was echoed; raw mode has already turned off the tty's echo, " +
			"so a silent Ctrl-C looks like a hung editor")
	}

	if got := readOne(t, e); got != "kept" {
		t.Errorf("the read after Ctrl-C returned %q, want %q — the session must survive", got, "kept")
	}
	if len(e.History()) != 1 || e.History()[0] != "kept" {
		t.Errorf("history = %q, want only the accepted line; an abandoned line is not history", e.History())
	}

	if _, restored := term.counts(); restored != 2 {
		t.Errorf("restores = %d, want 2 — every ReadLine hands the terminal back", restored)
	}
}

// TestCtrlDOnAnEmptyLineIsEOF is the other half of the ambiguity, and the
// contract KAN-793 turns into "end the session".
func TestCtrlDOnAnEmptyLineIsEOF(t *testing.T) {
	e, _, _ := interactiveEditor(ctrlD)
	got, err := e.ReadLine()
	if !errors.Is(err, io.EOF) {
		t.Fatalf("ReadLine on Ctrl-D: err = %v, want io.EOF", err)
	}
	if got != "" {
		t.Errorf("ReadLine on Ctrl-D returned %q, want %q", got, "")
	}
}

// TestInputEndingMidLine covers a stream that stops without a terminator. The
// typed line is returned and the io.EOF comes on the next call, which is
// bufio's contract and the one a caller already knows.
func TestInputEndingMidLine(t *testing.T) {
	e, _, _ := interactiveEditor("unterminated")

	if got := readOne(t, e); got != "unterminated" {
		t.Errorf("ReadLine() = %q, want the typed line", got)
	}
	if _, err := e.ReadLine(); !errors.Is(err, io.EOF) {
		t.Errorf("second ReadLine: err = %v, want io.EOF", err)
	}
}

// TestPlainModeIsEscapeFree is SLICE-1's acceptance criterion for a non-TTY:
// "piped output contains no ANSI escapes and is valid line-oriented text".
//
// It is asserted over the editor's whole output rather than over a formatter,
// because the criterion is about bytes on the pipe. And raw mode must never be
// attempted — a MakeRaw against a pipe is an error the REPL would have to
// explain.
func TestPlainModeIsEscapeFree(t *testing.T) {
	term := &fakeTerminal{interactive: false}
	out := &bytes.Buffer{}
	e := lineedit.New(lineedit.Config{
		In:       strings.NewReader("one\ntwo\r\nthree"),
		Out:      out,
		Terminal: term,
		Prompt:   "> ",
	})

	var got []string
	for {
		line, err := e.ReadLine()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("ReadLine: %v", err)
		}
		got = append(got, line)
	}

	want := []string{"one", "two", "three"}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("lines (-want +got):\n%s", diff)
	}
	if bytes.ContainsRune(out.Bytes(), 0x1b) {
		t.Errorf("piped output contains an ANSI escape: %q", out.String())
	}
	if raw, _ := term.counts(); raw != 0 {
		t.Errorf("MakeRaw was called %d times against a pipe; there is no raw mode to enter", raw)
	}
}

// TestEscapesInPipedInputAreNotInterpreted is the other direction of the same
// criterion. Bytes from a pipe are text, not keystrokes: a here-doc containing
// an ESC must arrive at the engine intact rather than being silently eaten as
// an arrow key.
func TestEscapesInPipedInputAreNotInterpreted(t *testing.T) {
	term := &fakeTerminal{interactive: false}
	e := lineedit.New(lineedit.Config{
		In:       strings.NewReader("fix the \x1b[31mred\x1b[0m output\n"),
		Out:      &bytes.Buffer{},
		Terminal: term,
	})
	want := "fix the \x1b[31mred\x1b[0m output"
	if got := readOne(t, e); got != want {
		t.Errorf("ReadLine() = %q, want %q", got, want)
	}
}

// TestInteractiveOutputRedrawsOnlyTheCurrentLine holds ADR-0004 decision 3:
// progress and editing repaint at most the current line and never scrollback.
// A stray "\n" mid-edit would scroll the terminal and put the prompt somewhere
// the repaint no longer owns.
func TestInteractiveOutputRedrawsOnlyTheCurrentLine(t *testing.T) {
	e, _, out := interactiveEditor("abc" + left + "X" + enter)
	readOne(t, e)

	// The only line feed is the one that ends the accepted line.
	if n := strings.Count(out.String(), "\n"); n != 1 {
		t.Errorf("output has %d line feeds, want 1 (the accepted line); "+
			"anything else has scrolled the terminal mid-edit: %q", n, out.String())
	}
	if !strings.HasSuffix(out.String(), "\r\n") {
		t.Errorf("output does not end with CRLF; raw mode has no ONLCR, so a bare "+
			"\\n leaves the next line indented under the prompt: %q", out.String())
	}
	if !strings.Contains(out.String(), "abXc") {
		t.Errorf("the edited line was never drawn: %q", out.String())
	}
}

// panicWriter fails the way a real terminal does when the far end goes away,
// except louder, so the deferred restore is exercised on the one path a
// `defer` exists for.
type panicWriter struct{}

func (panicWriter) Write([]byte) (int, error) { panic("the terminal went away") }

// TestTerminalIsRestoredOnEveryExitPath is the robustness promise ADR-0004
// rests on. A line editor that leaves a terminal in raw mode after a crash
// gives the user a shell with no echo and no Ctrl-C, and that is precisely the
// class of failure the ADR refuses a TUI framework to avoid.
//
// Every way out of ReadLine is enumerated here, panic included.
func TestTerminalIsRestoredOnEveryExitPath(t *testing.T) {
	exits := map[string]func(*testing.T, *lineedit.Editor){
		"accepted line": func(t *testing.T, e *lineedit.Editor) {
			if _, err := e.ReadLine(); err != nil {
				t.Fatalf("ReadLine: %v", err)
			}
		},
		"interrupt": func(t *testing.T, e *lineedit.Editor) {
			if _, err := e.ReadLine(); !errors.Is(err, lineedit.ErrInterrupted) {
				t.Fatalf("err = %v, want ErrInterrupted", err)
			}
		},
		"end of input": func(t *testing.T, e *lineedit.Editor) {
			if _, err := e.ReadLine(); !errors.Is(err, io.EOF) {
				t.Fatalf("err = %v, want io.EOF", err)
			}
		},
		"read error": func(t *testing.T, e *lineedit.Editor) {
			if _, err := e.ReadLine(); err == nil {
				t.Fatal("ReadLine returned no error on a failing reader")
			}
		},
	}
	inputs := map[string]io.Reader{
		"accepted line": strings.NewReader("hello" + enter),
		"interrupt":     strings.NewReader(ctrlC),
		"end of input":  strings.NewReader(""),
		"read error":    iotest.ErrReader(fmt.Errorf("the tty hung up")),
	}

	for name, drive := range exits {
		t.Run(name, func(t *testing.T) {
			term := &fakeTerminal{interactive: true}
			e := lineedit.New(lineedit.Config{
				In: inputs[name], Out: &bytes.Buffer{}, Terminal: term,
			})
			drive(t, e)
			raw, restored := term.counts()
			if raw != 1 || restored != 1 {
				t.Errorf("MakeRaw %d, restore %d; want 1 and 1", raw, restored)
			}
		})
	}

	t.Run("panic", func(t *testing.T) {
		term := &fakeTerminal{interactive: true}
		e := lineedit.New(lineedit.Config{
			In: strings.NewReader("hello" + enter), Out: panicWriter{}, Terminal: term,
		})

		func() {
			defer func() {
				if recover() == nil {
					t.Error("the panic did not propagate; ReadLine must not swallow it")
				}
			}()
			_, _ = e.ReadLine()
		}()

		if raw, restored := term.counts(); raw != 1 || restored != 1 {
			t.Errorf("MakeRaw %d, restore %d after a panic; want 1 and 1 — "+
				"a crash must not leave the user's terminal raw", raw, restored)
		}
	})
}

// TestRestoreIsIdempotent lets a signal handler and the deferred restore both
// fire without the terminal being handed back twice.
func TestRestoreIsIdempotent(t *testing.T) {
	e, term, _ := interactiveEditor("hi" + enter)
	readOne(t, e)

	for range 3 {
		if err := e.Restore(); err != nil {
			t.Fatalf("Restore: %v", err)
		}
	}
	if _, restored := term.counts(); restored != 1 {
		t.Errorf("restores = %d, want 1", restored)
	}
}

// TestRestoreBeforeAnyReadIsANoOp covers the signal handler that fires before
// the first prompt is ever drawn.
func TestRestoreBeforeAnyReadIsANoOp(t *testing.T) {
	e, term, _ := interactiveEditor(enter)
	if err := e.Restore(); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if _, restored := term.counts(); restored != 0 {
		t.Errorf("restores = %d before any read, want 0", restored)
	}
}

// gatedReader blocks in Read until it is released, which is what a terminal
// waiting for a keystroke does.
type gatedReader struct {
	started chan struct{}
	release chan struct{}
	once    sync.Once
	rest    io.Reader
}

func (r *gatedReader) Read(p []byte) (int, error) {
	r.once.Do(func() {
		close(r.started)
		<-r.release
	})
	return r.rest.Read(p)
}

// TestRestoreDuringABlockedRead is the case Restore is exported for: a SIGTERM
// or SIGHUP handler runs on another goroutine while ReadLine is parked in a
// read that may never return. The deferred restore cannot help there, so the
// call has to be safe from outside — which under -race means the field really
// is guarded and not merely documented as such.
func TestRestoreDuringABlockedRead(t *testing.T) {
	gate := &gatedReader{
		started: make(chan struct{}),
		release: make(chan struct{}),
		rest:    strings.NewReader("hi" + enter),
	}
	term := &fakeTerminal{interactive: true}
	e := lineedit.New(lineedit.Config{In: gate, Out: &bytes.Buffer{}, Terminal: term})

	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = e.ReadLine()
	}()

	<-gate.started
	if err := e.Restore(); err != nil {
		t.Fatalf("Restore from another goroutine: %v", err)
	}
	close(gate.release)
	<-done

	if _, restored := term.counts(); restored != 1 {
		t.Errorf("restores = %d, want exactly 1 — the handler restored it, and the "+
			"deferred call must not do it again", restored)
	}
}

// TestRawModeFailureIsReported keeps a broken terminal from being mistaken for
// end of input, which would end a REPL session with no explanation.
func TestRawModeFailureIsReported(t *testing.T) {
	term := &fakeTerminal{interactive: true, makeErr: fmt.Errorf("ioctl: inappropriate device")}
	e := lineedit.New(lineedit.Config{
		In: strings.NewReader("hi" + enter), Out: &bytes.Buffer{}, Terminal: term,
	})

	_, err := e.ReadLine()
	if err == nil {
		t.Fatal("ReadLine returned no error when raw mode could not be entered")
	}
	if errors.Is(err, io.EOF) {
		t.Errorf("a raw-mode failure surfaced as io.EOF: %v", err)
	}
	if !strings.Contains(err.Error(), "inappropriate device") {
		t.Errorf("err = %v; the underlying cause was lost", err)
	}
}

// TestNonInteractiveIsTheDefault pins the safe default. An editor that wrongly
// believes it has a terminal writes cursor escapes into a pipe; one that
// wrongly believes it has a pipe merely offers no arrow keys.
func TestNonInteractiveIsTheDefault(t *testing.T) {
	out := &bytes.Buffer{}
	e := lineedit.New(lineedit.Config{In: strings.NewReader("hello\n"), Out: out})

	if got := readOne(t, e); got != "hello" {
		t.Errorf("ReadLine() = %q, want %q", got, "hello")
	}
	if bytes.ContainsRune(out.Bytes(), 0x1b) {
		t.Errorf("the default editor emitted an escape: %q", out.String())
	}
}
