package repl_test

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/leejianrong/kopicode/cmd/kopicode/repl"
	"github.com/leejianrong/kopicode/internal/engine"
)

// TestCtrlCMidStreamLeavesAUsableSession is half of KAN-793's "done when", and
// it is written as the whole sequence rather than as three assertions about
// pieces, because "usable" is a property of what happens *next*.
//
// The turn streams, blocks, and is interrupted. What has to be true afterwards:
//
//   - the turn's context is cancelled, which is the mechanism that reaches the
//     provider stream and the shell tool's process group, since the engine
//     threads that one context all the way down;
//   - the cancellation is reported to the user rather than looking like a
//     model that stopped talking;
//   - the prompt comes back, the next turn runs, and it completes;
//   - the session is closed exactly once, at the end, by the loop and not by
//     the interrupt.
func TestCtrlCMidStreamLeavesAUsableSession(t *testing.T) {
	sig := make(chan os.Signal, 4)
	streamed := make(chan struct{})
	cancelled := make(chan struct{})

	var out strings.Builder
	var prompts []string
	closed := 0
	turn := 0

	loop, err := repl.New(repl.Config{
		In:         strings.NewReader("read the file\nwhat did you find\n"),
		Out:        &out,
		Interrupts: sig,
		Close:      func(context.Context) error { closed++; return nil },
		Turn: func(ctx context.Context, prompt string, s repl.Surface) (engine.Result, error) {
			prompts = append(prompts, prompt)
			turn++
			if turn > 1 {
				s.Render(delta("nothing, I was interrupted\n"))
				s.Render(engine.Event{Kind: engine.EventAssistantMessage, Seq: 9,
					Text: "nothing, I was interrupted\n"})
				return engine.Result{Stop: engine.StopCompleted, Turns: 1}, nil
			}

			s.Progress("waiting for the model")
			s.Render(delta("I will start by reading"))
			close(streamed)

			// The user presses Ctrl-C here, with the reply half printed. The
			// channel is buffered so this cannot block, and the watcher the
			// loop started for this turn is what picks it up — no sleep, no
			// timing assumption.
			sig <- os.Interrupt

			<-ctx.Done()
			close(cancelled)
			// The engine's own behaviour, stood in for: it journals a
			// TurnCancelled the moment it observes the cancelled context and
			// announces it through this very surface (KAN-857). The
			// `[cancelled]` line asserted below is therefore a rendering of the
			// record, which is the property this package holds for every other
			// line it prints.
			s.Render(engine.Event{Kind: engine.EventTurnCancelled, Seq: 8, Turn: 1,
				Reason: "provider_stream", Text: ctx.Err().Error()})
			return engine.Result{Stop: engine.StopCancelled, Turns: 1}, ctx.Err()
		},
	})
	if err != nil {
		t.Fatalf("building the loop: %v", err)
	}

	stop, err := loop.Run(context.Background())
	if err != nil {
		t.Fatalf("Ctrl-C ended the session: %v", err)
	}
	if stop != engine.StopCompleted {
		t.Errorf("session stop = %v, want %v — cancelling a turn must not end the session",
			stop, engine.StopCompleted)
	}

	select {
	case <-cancelled:
	default:
		t.Fatal("the turn's context was never cancelled; nothing would have reached the shell process group")
	}
	select {
	case <-streamed:
	default:
		t.Fatal("the turn never streamed, so this did not test an interruption mid-stream")
	}

	if len(prompts) != 2 {
		t.Fatalf("the prompt did not come back after Ctrl-C: %v", prompts)
	}
	if closed != 1 {
		t.Errorf("Close called %d times, want 1", closed)
	}

	text := out.String()
	if !strings.Contains(text, "[cancelled]") {
		t.Errorf("the interruption was not reported to the user:\n%s", text)
	}
	if !strings.Contains(text, "nothing, I was interrupted") {
		t.Errorf("the turn after the interruption did not run:\n%s", text)
	}
	if i, j := strings.Index(text, "[cancelled]"), strings.Index(text, "nothing, I was interrupted"); i > j {
		t.Errorf("the cancellation was reported after the next turn's output:\n%s", text)
	}
}

// TestAnInterruptBetweenTurnsDoesNotCancelTheNextOne.
//
// Piped, Ctrl-C at the prompt is a signal rather than a byte lineedit can see,
// so it lands in the channel with no turn to cancel. Left there it would abort
// the next turn — a turn the user never interrupted, and the kind of bug that
// shows up as "it randomly cancels itself".
func TestAnInterruptBetweenTurnsDoesNotCancelTheNextOne(t *testing.T) {
	ran := false
	watchful := func(ctx context.Context, _ string, _ repl.Surface) (engine.Result, error) {
		ran = true
		if err := ctx.Err(); err != nil {
			t.Errorf("the turn started already cancelled: %v", err)
		}
		return engine.Result{Stop: engine.StopCompleted}, nil
	}

	h := newHarness(t, "go\n", false, watchful)
	h.sig <- os.Interrupt
	h.sig <- os.Interrupt

	if _, err := h.run(); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !ran {
		t.Fatal("the turn never ran")
	}
	if strings.Contains(h.text(), "[cancelled]") {
		t.Errorf("a stale interrupt cancelled a turn the user did not interrupt:\n%s", h.text())
	}
}

// TestCtrlCAtThePromptDiscardsTheLineAndKeepsTheSession covers lineedit's half
// of the interrupt story. The editor returns ErrInterrupted, the partial line
// is gone, and the loop prompts again — no turn, no session ending.
func TestCtrlCAtThePromptDiscardsTheLineAndKeepsTheSession(t *testing.T) {
	// \x03 on the non-terminal path is ordinary text rather than a key, so
	// the interrupt is driven through the editor's own contract instead: a
	// Terminal that reports itself interactive, fed the raw bytes a terminal
	// would send. \r is what Enter sends in raw mode.
	var out strings.Builder
	var prompts []string
	closed := 0

	loop, err := repl.New(repl.Config{
		In:          strings.NewReader("half a thou\x03second thought\r"),
		Out:         &out,
		Interactive: true,
		Terminal:    fakeTerminal{},
		Turn: func(_ context.Context, prompt string, _ repl.Surface) (engine.Result, error) {
			prompts = append(prompts, prompt)
			return engine.Result{Stop: engine.StopCompleted}, nil
		},
		Close: func(context.Context) error { closed++; return nil },
	})
	if err != nil {
		t.Fatalf("building the loop: %v", err)
	}

	if _, err := loop.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if want := []string{"second thought"}; !equalStrings(prompts, want) {
		t.Errorf("prompts = %v, want %v — the interrupted line must be discarded, "+
			"not merged into the next one", prompts, want)
	}
	if closed != 1 {
		t.Errorf("Close called %d times, want 1", closed)
	}
}

// fakeTerminal reports itself interactive without touching a real one, which
// is the seam lineedit exists to offer: the key decoding runs over bytes a
// test supplies.
type fakeTerminal struct{}

func (fakeTerminal) IsInteractive() bool { return true }

func (fakeTerminal) MakeRaw() (func() error, error) { return func() error { return nil }, nil }
