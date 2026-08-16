package repl_test

import (
	"context"
	"strings"
	"testing"

	"github.com/leejianrong/kopicode/cmd/kopicode/repl"
	"github.com/leejianrong/kopicode/internal/engine"
)

// render runs one turn that does whatever fn asks and returns what the user
// saw, with the prompt noise stripped so the assertions are about the
// rendering.
func render(t *testing.T, fn func(s repl.Surface)) string {
	t.Helper()

	var out strings.Builder
	loop, err := repl.New(repl.Config{
		In:  strings.NewReader("go\n"),
		Out: &out,
		Turn: func(_ context.Context, _ string, s repl.Surface) (engine.Result, error) {
			fn(s)
			return engine.Result{Stop: engine.StopCompleted}, nil
		},
	})
	if err != nil {
		t.Fatalf("building the loop: %v", err)
	}
	if _, err := loop.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	return strings.ReplaceAll(out.String(), "kopicode> ", "")
}

// TestTheRecordDecidesWhatTheUserSaw is the property the package comment
// promises: streamed text is an optimistic prefix of the journal's
// AssistantMessage, so what reaches the terminal is exactly the recorded
// message — never the streamed bytes twice, never a message the record does
// not hold.
func TestTheRecordDecidesWhatTheUserSaw(t *testing.T) {
	tests := []struct {
		name     string
		streamed []string
		recorded string
		want     string
	}{
		{
			name:     "nothing streamed prints the whole recorded message",
			recorded: "the fix is in Open()",
			want:     "the fix is in Open()",
		},
		{
			name:     "a complete stream prints nothing more",
			streamed: []string{"the fix ", "is in Open()"},
			recorded: "the fix is in Open()",
			want:     "the fix is in Open()",
		},
		{
			name:     "a partial stream prints only the remainder",
			streamed: []string{"the fix "},
			recorded: "the fix is in Open()",
			want:     "the fix is in Open()",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := render(t, func(s repl.Surface) {
				for _, chunk := range tc.streamed {
					s.Render(delta(chunk))
				}
				s.Render(engine.Event{Kind: engine.EventAssistantMessage, Seq: 1, Text: tc.recorded})
			})

			if strings.Count(got, tc.want) != 1 {
				t.Errorf("the recorded message appears %d times, want exactly 1:\n%q",
					strings.Count(got, tc.want), got)
			}
		})
	}
}

// TestADivergentStreamShowsTheRecordAndSaysSo.
//
// If the live echo was not a prefix of what the journal recorded, the record
// wins and the difference is printed. Silently preferring either one would
// make the terminal and the record disagree with nobody able to tell.
func TestADivergentStreamShowsTheRecordAndSaysSo(t *testing.T) {
	got := render(t, func(s repl.Surface) {
		s.Render(delta("a guess that was wrong"))
		s.Render(engine.Event{Kind: engine.EventAssistantMessage, Seq: 1, Text: "what the record holds"})
	})

	if !strings.Contains(got, "what the record holds") {
		t.Errorf("the recorded message was not printed:\n%q", got)
	}
	if !strings.Contains(got, "differed") {
		t.Errorf("the divergence was not reported:\n%q", got)
	}
}

// TestASecondMessageDoesNotInheritTheFirstsStream: the streamed buffer is per
// message. Without the reset, turn two's message would be printed minus turn
// one's prefix, which is a silent truncation of the record.
func TestASecondMessageDoesNotInheritTheFirstsStream(t *testing.T) {
	got := render(t, func(s repl.Surface) {
		s.Render(delta("hello"))
		s.Render(engine.Event{Kind: engine.EventAssistantMessage, Seq: 1, Text: "hello there"})
		s.Render(engine.Event{Kind: engine.EventAssistantMessage, Seq: 2, Text: "hello again"})
	})

	if !strings.Contains(got, "hello again") {
		t.Errorf("the second message was truncated by the first message's stream:\n%q", got)
	}
}

// TestEveryEventTheEngineRecordsReachesTheUser.
//
// Three kinds are deliberately silent — a provider request paints progress, a
// provider response clears it, and a tool-call-requested is the raw text of a
// call whose repair says what was wrong — and a permission request is asked
// rather than printed. Everything else must produce a line, because an event
// the record holds and the user was never told about is the failure this
// surface exists to prevent.
func TestEveryEventTheEngineRecordsReachesTheUser(t *testing.T) {
	silent := map[engine.EventKind]string{
		engine.EventProviderRequest:     "paints progress on the current line",
		engine.EventProviderResponse:    "clears it",
		engine.EventToolCallRequested:   "the repair or failure that follows says what was wrong",
		engine.EventPermissionRequested: "is asked by Loop.Ask rather than printed",
	}

	for _, e := range everyKind() {
		if e.Kind == engine.EventUnspecified || e.Kind == engine.EventDelta {
			continue
		}
		got := render(t, func(s repl.Surface) { s.Render(e) })

		if why, ok := silent[e.Kind]; ok {
			if strings.Contains(got, "[") {
				t.Errorf("engine.%s printed a line, but it %s:\n%q", e.Kind, why, got)
			}
			continue
		}
		if strings.TrimSpace(got) == "" {
			t.Errorf("engine.%s rendered nothing", e.Kind)
		}
		if strings.Contains(got, "no rendering for") {
			t.Errorf("engine.%s fell through to the unhandled case", e.Kind)
		}
	}
}

// TestAnUnrenderedKindIsLoud. An event the journal holds and the user was
// never told about is the one failure this surface exists to prevent, so the
// default case reports rather than drops.
func TestAnUnrenderedKindIsLoud(t *testing.T) {
	got := render(t, func(s repl.Surface) {
		s.Render(engine.Event{Kind: engine.EventKind(200)})
	})
	if !strings.Contains(got, "no rendering for") {
		t.Errorf("an unhandled kind was dropped silently:\n%q", got)
	}

	zero := render(t, func(s repl.Surface) { s.Render(engine.Event{}) })
	if !strings.Contains(zero, "no rendering for") {
		t.Errorf("the zero Event was dropped silently:\n%q", zero)
	}
}

// TestASyntaxGateThatDidNotRunDoesNotReadAsAPass — ADR-0006 §4. "no checker
// available" and "exit 0" must not render the same way.
func TestASyntaxGateThatDidNotRunDoesNotReadAsAPass(t *testing.T) {
	notRun := render(t, func(s repl.Surface) {
		s.Render(engine.Event{Kind: engine.EventSyntaxGate, Seq: 1, Path: "notes.txt"})
	})
	passed := render(t, func(s repl.Surface) {
		s.Render(engine.Event{
			Kind: engine.EventSyntaxGate, Seq: 1, Path: "x.go", Checker: "gofmt -e", Ran: true,
		})
	})

	if !strings.Contains(notRun, "not run") {
		t.Errorf("a gate with no checker did not say so:\n%q", notRun)
	}
	if strings.Contains(passed, "not run") {
		t.Errorf("a gate that ran was rendered as one that did not:\n%q", passed)
	}
}

// TestAFuzzyEditIsVisible. A session containing one fuzzy edit is classified
// unattributed (SLICE-1 §4), so the mode is shown rather than folded into
// "applied".
func TestAFuzzyEditIsVisible(t *testing.T) {
	got := render(t, func(s repl.Surface) {
		s.Render(engine.Event{Kind: engine.EventEditApplied, Seq: 1, Path: "x.go", Mode: "fuzzy"})
	})
	if !strings.Contains(got, "fuzzy") {
		t.Errorf("a fuzzy edit was not marked as one:\n%q", got)
	}
}

// TestToolOutputIsSummarisedBySizeAndNeverPasted. Nothing is truncated here
// because nothing is printed here: the whole output is in the journal, and the
// line points at how much of it there is.
func TestToolOutputIsSummarisedBySizeAndNeverPasted(t *testing.T) {
	got := render(t, func(s repl.Surface) {
		s.Render(engine.Event{
			Kind: engine.EventToolResult, Seq: 1, Tool: "run_shell", Size: 70000,
			ExitCode: 1, HasExitCode: true, Reason: "task",
		})
	})

	for _, want := range []string{"run_shell", "exit 1", "68.4 KiB"} {
		if !strings.Contains(got, want) {
			t.Errorf("tool result summary does not contain %q:\n%q", want, got)
		}
	}
}

// TestThePipedTranscriptCarriesTheUsersOwnWords. Piped, nothing echoed what
// the user typed, so a transcript without it is a monologue. On a terminal the
// user has already read it and the echo would be a duplicate.
func TestThePipedTranscriptCarriesTheUsersOwnWords(t *testing.T) {
	user := func(s repl.Surface) {
		s.Render(engine.Event{Kind: engine.EventUserMessage, Seq: 1, Text: "fix the parser"})
	}

	piped := render(t, user)
	if !strings.Contains(piped, "fix the parser") {
		t.Errorf("the piped transcript does not carry the user's message:\n%q", piped)
	}

	var out strings.Builder
	loop, err := repl.New(repl.Config{
		In: strings.NewReader("go\n"), Out: &out, Interactive: true,
		Turn: func(_ context.Context, _ string, s repl.Surface) (engine.Result, error) {
			user(s)
			return engine.Result{Stop: engine.StopCompleted}, nil
		},
	})
	if err != nil {
		t.Fatalf("building the loop: %v", err)
	}
	if _, err := loop.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if strings.Contains(out.String(), "fix the parser") {
		t.Errorf("the terminal echoed the user's message back at them:\n%q", out.String())
	}
}
