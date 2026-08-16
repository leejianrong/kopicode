package repl_test

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"testing"

	"github.com/leejianrong/kopicode/cmd/kopicode/repl"
	"github.com/leejianrong/kopicode/internal/engine"
)

// busyTurn exercises every printing path this package has, in one turn: the
// live stream, the recorded message, a thinking block, tool calls and results,
// both edit outcomes, the syntax gate, verification, a snapshot, a permission
// decision, a progress indicator, and the three surface-only kinds.
//
// It is deliberately one function used by several tests. "Piped output has no
// escapes" is only worth asserting over output that covers everything the
// surface can print, and a per-test hand-rolled subset is how the one path
// that emits an escape ends up being the untested one.
func busyTurn(ctx context.Context, _ string, s repl.Surface) (engine.Result, error) {
	s.Progress("waiting for the model")
	s.Stream("I will read ")
	s.Stream("the file first.\n")
	s.Render(repl.Event{Kind: repl.KindAssistant, Text: "I will read the file first.\n"})

	s.Render(repl.Event{Kind: repl.KindThinking, Text: "the anchors look stale\nre-read first"})
	s.Render(repl.Event{Kind: repl.KindToolCall, Tool: "read_file", Detail: "internal/x.go"})
	s.Render(repl.Event{
		Kind: repl.KindToolResult, Tool: "read_file", Size: 4096,
	})
	s.Render(repl.Event{Kind: repl.KindToolRepair, Tool: "edit_file", Reason: "invalid_json"})
	s.Render(repl.Event{Kind: repl.KindToolFailed, Reason: "unlabelled_fence"})
	s.Render(repl.Event{Kind: repl.KindEditApplied, Path: "internal/x.go", Mode: "anchored"})
	s.Render(repl.Event{
		Kind: repl.KindEditRejected, Path: "internal/x.go", Reason: "anchor_drift",
		Text: "a1b2c3d4 12| func Open(",
	})
	s.Render(repl.Event{
		Kind: repl.KindSyntaxGate, Path: "internal/x.go", Checker: "gofmt -e", Ran: true, ExitCode: 1,
	})
	s.Render(repl.Event{Kind: repl.KindSyntaxGate, Path: "notes.txt"})
	s.Render(repl.Event{Kind: repl.KindSnapshot, Ref: "refs/kopicode/s-1/3"})
	s.Render(repl.Event{
		Kind: repl.KindVerification, Command: []string{"go", "test", "./..."}, ExitCode: 0,
	})
	s.Render(repl.Event{Kind: repl.KindPermissionDecided, Decision: "allow", Source: "user"})
	s.Render(repl.Event{Kind: repl.KindNotice, Text: "resumed"})
	s.Render(repl.Event{Kind: repl.KindError, Text: "provider returned no usable reply"})
	s.Render(repl.Event{Kind: repl.KindSessionStarted, Detail: "s-1"})
	s.Render(repl.Event{Kind: repl.KindSessionEnded, Reason: "completed"})

	if _, err := s.Ask(ctx, repl.Consent{
		Kind: "run_shell", Tool: "run_shell",
		Detail: "go test ./...",
		Reason: "model-authored shell commands always require consent",
	}); err != nil {
		return engine.Result{Stop: engine.StopHarnessError}, err
	}

	s.Progress("")
	return engine.Result{Stop: engine.StopCompleted, Turns: 1}, nil
}

// escape is any ESC byte. The criterion in docs/SLICE-1.md §Test Plan is
// "piped output contains no ANSI escapes", and every ANSI sequence starts with
// one, so this is the whole check rather than a pattern that has to keep up
// with the sequences someone might add.
const escape = '\x1b'

// TestNoEscapesWhenPiped is half of KAN-793's "done when", asserted on the
// bytes rather than on a claim about them.
//
// It drives a whole session — two turns, every event kind, a permission
// prompt, an interrupted turn — through the non-interactive path and requires
// the captured output to hold no ESC byte and no control character other than
// newline. That second half matters as much as the first: a carriage return
// left in a piped transcript makes `less` and `git diff` show a line that is
// not what the file holds, which is the same failure as an escape wearing
// different clothes.
func TestNoEscapesWhenPiped(t *testing.T) {
	h := newHarness(t, "do the thing\ny\nand again\ny\n", false, busyTurn, busyTurn)

	if _, err := h.run(); err != nil {
		t.Fatalf("Run: %v", err)
	}

	out := h.text()
	if out == "" {
		t.Fatal("the session printed nothing, so this asserted nothing")
	}
	if i := strings.IndexByte(out, escape); i >= 0 {
		t.Errorf("piped output contains an escape at byte %d: %q\n"+
			"docs/adr/0004-line-oriented-repl.md decision 3 and docs/SLICE-1.md require piped "+
			"output to be valid line-oriented text with no escapes\nfull output:\n%s",
			i, out[i:min(i+16, len(out))], out)
	}
	for i := range len(out) {
		if c := out[i]; c < 0x20 && c != '\n' {
			t.Errorf("piped output contains control byte %#x at %d; only newline is line-oriented text", c, i)
		}
	}
	if !strings.HasSuffix(out, "\n") {
		t.Errorf("piped output does not end with a newline, so the last line is not a line:\n%q",
			out[max(0, len(out)-40):])
	}
}

// TestProgressWritesNothingWhenPiped.
//
// A progress indicator whose no-TTY form is "print it as a line" would put a
// dozen transient statuses in the middle of a transcript that is supposed to
// be the session. The no-op is the behaviour, not a fallback, so it is
// asserted directly: the same session with and without progress calls
// produces identical bytes.
func TestProgressWritesNothingWhenPiped(t *testing.T) {
	quiet := func(_ context.Context, _ string, s repl.Surface) (engine.Result, error) {
		s.Render(repl.Event{Kind: repl.KindNotice, Text: "one"})
		s.Render(repl.Event{Kind: repl.KindNotice, Text: "two"})
		return engine.Result{Stop: engine.StopCompleted}, nil
	}
	noisy := func(_ context.Context, _ string, s repl.Surface) (engine.Result, error) {
		s.Progress("thinking")
		s.Render(repl.Event{Kind: repl.KindNotice, Text: "one"})
		s.Progress("still thinking")
		s.Render(repl.Event{Kind: repl.KindNotice, Text: "two"})
		s.Progress("")
		return engine.Result{Stop: engine.StopCompleted}, nil
	}

	a := newHarness(t, "go\n", false, quiet)
	b := newHarness(t, "go\n", false, noisy)
	if _, err := a.run(); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if _, err := b.run(); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if a.text() != b.text() {
		t.Errorf("progress changed the piped transcript.\nwithout:\n%q\nwith:\n%q", a.text(), b.text())
	}
}

// escapeVocabulary is every escape sequence this package is allowed to write
// on a terminal, and it is short on purpose.
//
// ADR-0004 decision 3: a progress indicator repaints at most the current line
// and never scrollback. What breaks that is cursor movement — CSI A/B/E/F, a
// scroll region, a clear-screen, the alt screen — so the guard is a vocabulary
// check rather than a "does it look right" one. Nothing here moves the cursor
// off the current line.
var escapeVocabulary = map[string]string{
	"\x1b[K":  "erase to end of line",
	"\x1b[0m": "reset",
	"\x1b[1m": "bold",
	"\x1b[2m": "dim",
}

// anyEscape matches a CSI sequence plus a generous tail, so a sequence outside
// the vocabulary is *found* rather than skipped by a pattern that only knows
// the approved ones.
var anyEscape = regexp.MustCompile(`\x1b(\[[0-9;?]*[A-Za-z]|[()#][A-Za-z0-9]|[A-Za-z=><])`)

// TestProgressNeverLeavesTheCurrentLine holds the interactive path to the
// vocabulary above.
//
// The Terminal is left non-interactive while the *writer* is interactive, so
// the escapes in the captured bytes are this package's and not the line
// editor's. They are different questions — whether input can be read a key at
// a time, and whether output may carry escapes — and `kopicode > transcript`
// with a keyboard attached is the case that needs them apart.
func TestProgressNeverLeavesTheCurrentLine(t *testing.T) {
	h := newHarness(t, "do the thing\ny\n", true, busyTurn)

	if _, err := h.run(); err != nil {
		t.Fatalf("Run: %v", err)
	}

	out := h.text()
	found := anyEscape.FindAllString(out, -1)
	if len(found) == 0 {
		t.Fatal("the interactive path wrote no escapes at all, so this asserted nothing " +
			"about which ones it writes")
	}
	for _, seq := range found {
		if _, ok := escapeVocabulary[seq]; !ok {
			t.Errorf("interactive output contains %q, which is not in this package's escape "+
				"vocabulary %v\nprogress repaints the current line only and never scrollback "+
				"(docs/adr/0004-line-oriented-repl.md decision 3)", seq, vocabulary())
		}
	}

	// A progress status must never be committed to scrollback: every one that
	// is painted is erased before anything else is written, so the status text
	// cannot survive into a line the user scrolls back to.
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, "waiting for the model") && !strings.Contains(line, "\x1b[K") {
			t.Errorf("a progress status reached scrollback un-erased: %q", line)
		}
	}
}

func vocabulary() []string {
	out := make([]string, 0, len(escapeVocabulary))
	for seq, what := range escapeVocabulary {
		out = append(out, fmt.Sprintf("%q (%s)", seq, what))
	}
	return out
}
