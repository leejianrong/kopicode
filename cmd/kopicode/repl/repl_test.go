package repl_test

import (
	"context"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/leejianrong/kopicode/cmd/kopicode/repl"
	"github.com/leejianrong/kopicode/internal/engine"
)

// harness drives a Loop with scripted input and captures every byte it wrote.
//
// The seam under test is the Loop's public surface — input in, bytes out, an
// engine.Stop back — and nothing here asserts on the Loop's internals. That is
// the repo's rule about observable outcomes, and it is also what makes these
// tests survive the wiring the loop is still waiting for.
type harness struct {
	t *testing.T

	out  strings.Builder
	sig  chan os.Signal
	loop *repl.Loop

	// prompts is every prompt the turn function was handed, in order.
	prompts []string
	// closed counts Close calls. Exactly one is the contract: the session
	// record has one ending.
	closed int
}

// turnScript is one scripted exchange.
type turnScript func(ctx context.Context, prompt string, s repl.Surface) (engine.Result, error)

// completed is the ordinary turn: the model said something and stopped.
func completed(text string) turnScript {
	return func(_ context.Context, _ string, s repl.Surface) (engine.Result, error) {
		s.Render(delta(text))
		s.Render(engine.Event{Kind: engine.EventAssistantMessage, Seq: 1, Text: text})
		return engine.Result{Stop: engine.StopCompleted, Turns: 1}, nil
	}
}

// delta is a fragment of the reply as it arrives: the one event with no record
// behind it, which is why its Seq is zero.
func delta(text string) engine.Event {
	return engine.Event{Kind: engine.EventDelta, Text: text, Reason: "content"}
}

func newHarness(t *testing.T, input string, interactive bool, scripts ...turnScript) *harness {
	t.Helper()

	h := &harness{t: t, sig: make(chan os.Signal, 16)}

	var n int
	loop, err := repl.New(repl.Config{
		In:          strings.NewReader(input),
		Out:         &h.out,
		Interactive: interactive,
		Interrupts:  h.sig,
		Turn: func(ctx context.Context, prompt string, s repl.Surface) (engine.Result, error) {
			h.prompts = append(h.prompts, prompt)
			if n >= len(scripts) {
				h.t.Errorf("turn %d (%q) has no script", n+1, prompt)
				return engine.Result{Stop: engine.StopCompleted}, nil
			}
			script := scripts[n]
			n++
			return script(ctx, prompt, s)
		},
		Close: func(context.Context) error {
			h.closed++
			return nil
		},
	})
	if err != nil {
		t.Fatalf("building the loop: %v", err)
	}
	h.loop = loop
	return h
}

func (h *harness) run() (engine.Stop, error) {
	h.t.Helper()
	return h.loop.Run(context.Background())
}

func (h *harness) text() string { return h.out.String() }

func TestARefusedConfigDoesNotBuildALoop(t *testing.T) {
	if _, err := repl.New(repl.Config{}); !errors.Is(err, repl.ErrNoTurn) {
		t.Fatalf("New with no TurnFunc: err = %v, want %v", err, repl.ErrNoTurn)
	}
}

func TestPromptsUntilInputEnds(t *testing.T) {
	h := newHarness(t, "first\nsecond\n", false,
		completed("one\n"),
		completed("two\n"),
	)

	stop, err := h.run()
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if stop != engine.StopCompleted {
		t.Errorf("stop = %v, want %v", stop, engine.StopCompleted)
	}
	if got, want := h.prompts, []string{"first", "second"}; !equalStrings(got, want) {
		t.Errorf("prompts = %v, want %v", got, want)
	}
	if h.closed != 1 {
		t.Errorf("Close called %d times, want exactly 1 — the session record has one ending", h.closed)
	}
	for _, want := range []string{"one", "two"} {
		if !strings.Contains(h.text(), want) {
			t.Errorf("output does not contain %q:\n%s", want, h.text())
		}
	}
}

func TestBlankLinesAndExitAreNotTurns(t *testing.T) {
	h := newHarness(t, "\n   \n/exit\nnever reached\n", false)

	if _, err := h.run(); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(h.prompts) != 0 {
		t.Errorf("blank lines and /exit ran %d turns: %v", len(h.prompts), h.prompts)
	}
}

func TestAProviderErrorReturnsToThePromptWithTheSessionAlive(t *testing.T) {
	failing := func(context.Context, string, repl.Surface) (engine.Result, error) {
		return engine.Result{Stop: engine.StopProviderError}, errors.New("upstream 503 after 4 attempts")
	}

	h := newHarness(t, "one\ntwo\n", false, failing, completed("recovered\n"))

	stop, err := h.run()
	if err != nil {
		t.Fatalf("a provider error ended the session: %v", err)
	}
	if stop != engine.StopCompleted {
		t.Errorf("stop = %v, want %v", stop, engine.StopCompleted)
	}
	if len(h.prompts) != 2 {
		t.Fatalf("the second prompt never ran: %v", h.prompts)
	}
	if !strings.Contains(h.text(), "upstream 503") {
		t.Errorf("the provider error was not reported:\n%s", h.text())
	}
	if !strings.Contains(h.text(), "recovered") {
		t.Errorf("the session did not survive the provider error:\n%s", h.text())
	}
}

func TestAHarnessErrorEndsTheSession(t *testing.T) {
	broken := func(context.Context, string, repl.Surface) (engine.Result, error) {
		return engine.Result{Stop: engine.StopHarnessError}, errors.New("journal refused a write")
	}

	h := newHarness(t, "one\ntwo\n", false, broken)

	stop, err := h.run()
	if err == nil {
		t.Fatal("a harness error did not end the session")
	}
	if stop != engine.StopHarnessError {
		t.Errorf("stop = %v, want %v", stop, engine.StopHarnessError)
	}
	if len(h.prompts) != 1 {
		t.Errorf("the loop kept prompting after a harness error: %v", h.prompts)
	}
	if h.closed != 1 {
		t.Errorf("Close called %d times, want 1 — a session that broke is the one whose ending "+
			"most needs to be on the record", h.closed)
	}
}

// TestTheTurnCapIsReportedAndTheSessionContinues covers the stops that are
// neither clean nor fatal. They are the ones a user has to be told about,
// because the loop stopped for a reason the next prompt might change.
func TestTheTurnCapIsReportedAndTheSessionContinues(t *testing.T) {
	capped := func(context.Context, string, repl.Surface) (engine.Result, error) {
		return engine.Result{Stop: engine.StopMaxTurns, Turns: 12}, nil
	}

	h := newHarness(t, "one\ntwo\n", false, capped, completed("ok\n"))

	if _, err := h.run(); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !strings.Contains(h.text(), "max_turns") {
		t.Errorf("the turn cap was not reported:\n%s", h.text())
	}
	if len(h.prompts) != 2 {
		t.Errorf("the session did not survive the turn cap: %v", h.prompts)
	}
}

// TestCloseFailureIsReportedRatherThanSwallowed: the session record's ending
// failing to be written is a harness error, and a REPL that exited 0 over it
// would be reporting a complete session that is not on disk.
func TestCloseFailureIsReportedRatherThanSwallowed(t *testing.T) {
	loop, err := repl.New(repl.Config{
		In:  strings.NewReader(""),
		Out: &strings.Builder{},
		Turn: func(context.Context, string, repl.Surface) (engine.Result, error) {
			return engine.Result{Stop: engine.StopCompleted}, nil
		},
		Close: func(context.Context) error { return errors.New("fsync failed") },
	})
	if err != nil {
		t.Fatalf("building the loop: %v", err)
	}

	stop, err := loop.Run(context.Background())
	if err == nil {
		t.Fatal("a failed Close was swallowed")
	}
	if stop != engine.StopHarnessError {
		t.Errorf("stop = %v, want %v", stop, engine.StopHarnessError)
	}
}

// TestConcurrentSignalDeliveryIsRaceFree exists for -race: the interrupt
// watcher is the only goroutine this package starts, and it shares a cancel
// func and a flag with the goroutine running the turn.
func TestConcurrentSignalDeliveryIsRaceFree(t *testing.T) {
	sig := make(chan os.Signal, 16)

	loop, err := repl.New(repl.Config{
		In:         strings.NewReader("one\n"),
		Out:        &strings.Builder{},
		Interrupts: sig,
		Turn: func(ctx context.Context, _ string, _ repl.Surface) (engine.Result, error) {
			var wg sync.WaitGroup
			for range 8 {
				wg.Add(1)
				go func() {
					defer wg.Done()
					sig <- os.Interrupt
				}()
			}
			<-ctx.Done()
			wg.Wait()
			return engine.Result{Stop: engine.StopCancelled}, ctx.Err()
		},
	})
	if err != nil {
		t.Fatalf("building the loop: %v", err)
	}

	if _, err := loop.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
