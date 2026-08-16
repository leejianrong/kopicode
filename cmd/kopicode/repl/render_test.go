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
					s.Stream(chunk)
				}
				s.Render(repl.Event{Kind: repl.KindAssistant, Text: tc.recorded})
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
		s.Stream("a guess that was wrong")
		s.Render(repl.Event{Kind: repl.KindAssistant, Text: "what the record holds"})
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
		s.Stream("hello")
		s.Render(repl.Event{Kind: repl.KindAssistant, Text: "hello there"})
		s.Render(repl.Event{Kind: repl.KindAssistant, Text: "hello again"})
	})

	if !strings.Contains(got, "hello again") {
		t.Errorf("the second message was truncated by the first message's stream:\n%q", got)
	}
}

func TestEveryKindRendersSomething(t *testing.T) {
	kinds := []repl.Kind{
		repl.KindSessionStarted, repl.KindUser, repl.KindAssistant, repl.KindThinking,
		repl.KindToolCall, repl.KindToolRepair, repl.KindToolFailed, repl.KindToolResult,
		repl.KindEditApplied, repl.KindEditRejected, repl.KindSyntaxGate,
		repl.KindPermissionDecided, repl.KindSnapshot, repl.KindVerification,
		repl.KindSessionEnded, repl.KindNotice, repl.KindError, repl.KindTurnCancelled,
		repl.KindTurnStopped,
	}

	for _, k := range kinds {
		got := render(t, func(s repl.Surface) {
			s.Render(repl.Event{Kind: k, Text: "body", Tool: "run_shell", Path: "x.go", Reason: "why"})
		})
		if strings.TrimSpace(got) == "" {
			t.Errorf("kind %d rendered nothing", uint8(k))
		}
		if strings.Contains(got, "no rendering for event kind") {
			t.Errorf("kind %d fell through to the unhandled case", uint8(k))
		}
	}
}

// TestAnUnrenderedKindIsLoud. An event the journal holds and the user was
// never told about is the one failure this surface exists to prevent, so the
// default case reports rather than drops.
func TestAnUnrenderedKindIsLoud(t *testing.T) {
	got := render(t, func(s repl.Surface) {
		s.Render(repl.Event{Kind: repl.Kind(200)})
	})
	if !strings.Contains(got, "no rendering for event kind 200") {
		t.Errorf("an unhandled kind was dropped silently:\n%q", got)
	}

	zero := render(t, func(s repl.Surface) { s.Render(repl.Event{}) })
	if !strings.Contains(zero, "no rendering for event kind 0") {
		t.Errorf("the zero Event was dropped silently:\n%q", zero)
	}
}

// TestASyntaxGateThatDidNotRunDoesNotReadAsAPass — ADR-0006 §4. "no checker
// available" and "exit 0" must not render the same way.
func TestASyntaxGateThatDidNotRunDoesNotReadAsAPass(t *testing.T) {
	notRun := render(t, func(s repl.Surface) {
		s.Render(repl.Event{Kind: repl.KindSyntaxGate, Path: "notes.txt"})
	})
	passed := render(t, func(s repl.Surface) {
		s.Render(repl.Event{
			Kind: repl.KindSyntaxGate, Path: "x.go", Checker: "gofmt -e", Ran: true,
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
		s.Render(repl.Event{Kind: repl.KindEditApplied, Path: "x.go", Mode: "fuzzy"})
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
		s.Render(repl.Event{
			Kind: repl.KindToolResult, Tool: "run_shell", Size: 70000,
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
		s.Render(repl.Event{Kind: repl.KindUser, Text: "fix the parser"})
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
