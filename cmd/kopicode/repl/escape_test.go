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
	for _, e := range everyKind() {
		s.Render(e)
	}

	if _, err := s.Ask(ctx, engine.ConsentRequest{
		Kind: "run_shell", Tool: "run_shell",
		Detail: "go test ./...",
		Reason: "model-authored shell commands always require consent",
	}); err != nil {
		return engine.Result{Stop: engine.StopHarnessError}, err
	}

	s.Progress("")
	return engine.Result{Stop: engine.StopCompleted, Turns: 1}, nil
}

// everyKind is one populated event of every kind the engine can emit, plus the
// zero value.
//
// It is derived from engine.EventKind rather than hand-listed, by way of
// TestEveryEventKindIsCovered below: a kind added to the engine and not added
// here fails that test, so "piped output has no escapes" keeps being asserted
// over *everything* the surface can print rather than over the subset somebody
// remembered.
func everyKind() []engine.Event {
	return []engine.Event{
		{Kind: engine.EventSessionStarted, Seq: 1, Detail: "s-1", Text: "qwen/qwen3-coder-next"},
		{Kind: engine.EventUserMessage, Seq: 2, Text: "fix the parser"},
		{Kind: engine.EventDelta, Text: "I will read ", Reason: "content"},
		{Kind: engine.EventDelta, Text: "the file first.\n", Reason: "content"},
		{Kind: engine.EventDelta, Text: "hmm\n", Reason: "reasoning"},
		{Kind: engine.EventAssistantMessage, Seq: 3, Text: "I will read the file first.\n"},
		{Kind: engine.EventThinking, Seq: 4, Text: "the anchors look stale\nre-read first"},
		{Kind: engine.EventProviderRequest, Seq: 5, Detail: "qwen/qwen3-coder-next", ExitCode: 1, HasExitCode: true},
		{Kind: engine.EventProviderRetried, Seq: 6, Detail: "try 2 of 6", Reason: "http 429", ExitCode: 1, HasExitCode: true},
		{Kind: engine.EventProviderResponse, Seq: 7, Reason: "stop", Source: "parasail/bf16", Size: 900},
		{Kind: engine.EventToolCallRequested, Seq: 8, Detail: "kc-1", Text: "```tool\n{"},
		{Kind: engine.EventToolCallParsed, Seq: 9, Tool: "read_file", Detail: `{"path":"internal/x.go"}`, Reason: "native"},
		{Kind: engine.EventToolCallRepaired, Seq: 10, Reason: "invalid_json", ExitCode: 1, HasExitCode: true},
		{Kind: engine.EventToolCallFailed, Seq: 11, Reason: "unlabelled_fence"},
		{Kind: engine.EventToolResult, Seq: 12, Tool: "read_file", Size: 4096},
		{Kind: engine.EventEditApplied, Seq: 13, Path: "internal/x.go", Mode: "anchored"},
		{Kind: engine.EventEditRejected, Seq: 14, Path: "internal/x.go", Reason: "anchor_drift",
			Text: "a1b2c3d4 12| func Open("},
		{Kind: engine.EventSyntaxGate, Seq: 15, Path: "internal/x.go", Checker: "gofmt -e", Ran: true,
			ExitCode: 1, HasExitCode: true},
		{Kind: engine.EventSyntaxGate, Seq: 16, Path: "notes.txt"},
		{Kind: engine.EventPermissionRequested, Seq: 17, Tool: "run_shell", Detail: "go test ./...", Mode: "run_shell"},
		{Kind: engine.EventPermissionDecided, Seq: 18, Decision: "allow", Source: "user"},
		{Kind: engine.EventTurnSnapshot, Seq: 19, Ref: "refs/kopicode/s-1/3"},
		{Kind: engine.EventVerification, Seq: 20, Command: []string{"go", "test", "./..."}, ExitCode: 0, HasExitCode: true},
		// Every phase, plus one this build does not know: the renderer names an
		// unrecognised phase rather than dropping it, and that branch is a place
		// an escape could enter through the record.
		{Kind: engine.EventTurnCancelled, Seq: 21, Turn: 3, Reason: "provider_stream",
			Text: "context canceled"},
		{Kind: engine.EventTurnCancelled, Seq: 22, Turn: 3, Reason: "tool_call", Text: "context canceled"},
		{Kind: engine.EventTurnCancelled, Seq: 23, Turn: 3, Reason: "verification", Text: "context canceled"},
		{Kind: engine.EventTurnCancelled, Seq: 24, Turn: 3, Reason: "between_steps", Text: "context canceled"},
		{Kind: engine.EventTurnCancelled, Seq: 25, Turn: 3, Reason: "some_future_phase",
			Text: "context deadline exceeded"},
		{Kind: engine.EventUnknown, Seq: 26, Reason: "SomethingNewer", Text: `{"a":1}`},
		{Kind: engine.EventSessionEnded, Seq: 27, Reason: "completed", ExitCode: 0, HasExitCode: true},
		{Kind: engine.EventSessionForked, Seq: 28, Detail: "s-source", ExitCode: 1, HasExitCode: true, Size: 5},
		// The zero value, which must be reported rather than dropped.
		{},
	}
}

// TestEveryEventKindIsCovered holds everyKind to the engine's vocabulary.
//
// Without it, a kind added to internal/engine would render through the default
// case — "no rendering for ..." — and no test would notice, because the two
// tests that matter here both drive everyKind.
func TestEveryEventKindIsCovered(t *testing.T) {
	seen := map[engine.EventKind]bool{}
	for _, e := range everyKind() {
		seen[e.Kind] = true
	}

	for k := engine.EventUnspecified; k <= engine.EventDelta; k++ {
		if !seen[k] {
			t.Errorf("engine.%s is not in everyKind(), so nothing asserts how the REPL prints it "+
				"or that printing it emits no escape when piped", k)
		}
	}
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
	one := engine.Event{Kind: engine.EventTurnSnapshot, Seq: 1, Ref: "refs/kopicode/s/1"}
	two := engine.Event{Kind: engine.EventTurnSnapshot, Seq: 2, Ref: "refs/kopicode/s/2"}

	quiet := func(_ context.Context, _ string, s repl.Surface) (engine.Result, error) {
		s.Render(one)
		s.Render(two)
		return engine.Result{Stop: engine.StopCompleted}, nil
	}
	noisy := func(_ context.Context, _ string, s repl.Surface) (engine.Result, error) {
		s.Progress("thinking")
		s.Render(one)
		s.Progress("still thinking")
		s.Render(two)
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
