package engine_test

import (
	"context"
	"errors"
	"testing"

	"github.com/leejianrong/kopicode/internal/engine"
	"github.com/leejianrong/kopicode/internal/journal"
	"github.com/leejianrong/kopicode/internal/provider"
	"github.com/leejianrong/kopicode/internal/provider/mock"
)

// TestCancellationMidStreamStopsTheTurn drives the half of the card's third
// clause that needs no subprocess: a context cancelled while the reply is still
// arriving.
//
// The cancellation is fired from inside the stream callback, which is the
// closest a test gets to Ctrl-C landing between two tokens. What must follow is
// that the turn stops rather than the partial reply being recorded as an
// answer: a half-read reply journaled as a whole one is how a cancelled turn
// becomes a model failure.
func TestCancellationMidStreamStopsTheTurn(t *testing.T) {
	p, err := mock.Load("two_turn_fenced_json_tool_call")
	if err != nil {
		t.Fatalf("loading fixture: %v", err)
	}

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	seen := 0
	h := newHarness(t, p, map[string]string{"internal/greet/greet.go": greetGo},
		withStream(func(provider.Delta) {
			seen++
			if seen == 2 {
				cancel()
			}
		}))

	res, err := h.eng.Run(ctx, "read it")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Run = %v, want an error wrapping context.Canceled", err)
	}
	if res.Stop != engine.StopCancelled {
		t.Errorf("stop = %s, want %s", res.Stop, engine.StopCancelled)
	}
	if seen < 2 {
		t.Fatalf("positive control failed: the stream produced %d deltas, so the cancellation "+
			"never landed mid-reply", seen)
	}

	if err := h.eng.Close(ctx); err != nil {
		t.Fatalf("Close: %v — the journal deliberately ignores a cancelled context so that "+
			"the event explaining why a turn stopped still gets written", err)
	}

	evs := h.events()
	if got := len(payloadsOf[journal.ProviderRequest](t, evs)); got != 1 {
		t.Errorf("got %d ProviderRequest events, want 1", got)
	}
	if got := len(payloadsOf[journal.ProviderResponse](t, evs)); got != 0 {
		t.Errorf("got %d ProviderResponse events, want 0: no reply ever completed", got)
	}
	if got := len(payloadsOf[journal.AssistantMessage](t, evs)); got != 0 {
		t.Errorf("got %d AssistantMessage events, want 0: the partial reply is not an answer", got)
	}

	ended := sole[journal.SessionEnded](t, evs)
	if ended.Reason != "cancelled" {
		t.Errorf("session ended %q, want \"cancelled\"", ended.Reason)
	}
	if ended.ExitCode == 0 {
		t.Error("a cancelled session exited 0, which reports a task that did not finish as a success")
	}

	// A cancellation is neither bucket. What must not happen is it landing in
	// `harness` against an acceptance criterion of zero — which is what a
	// SessionEnded reason of "error" would do.
	if got := classify(evs); got == bucketHarness {
		t.Error("a cancelled session classified as a harness failure")
	}
}

// TestCancelledContextRefusesToStartATurn holds the cheap case: a context
// already cancelled when Run is called must not reach the provider at all.
func TestCancelledContextRefusesToStartATurn(t *testing.T) {
	p, err := mock.Load("two_turn_native_tool_call")
	if err != nil {
		t.Fatalf("loading fixture: %v", err)
	}
	h := newHarness(t, p, map[string]string{"internal/greet/greet.go": greetGo})

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	res, err := h.eng.Run(ctx, "read it")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Run = %v, want an error wrapping context.Canceled", err)
	}
	if res.Stop != engine.StopCancelled {
		t.Errorf("stop = %s, want %s", res.Stop, engine.StopCancelled)
	}
	if h.prov.Consumed() != 0 {
		t.Errorf("the provider served %d replies to an already-cancelled turn", h.prov.Consumed())
	}
	// The user's message is still on the record: it was said, and a record that
	// dropped it would describe a session the user did not have.
	if got := len(payloadsOf[journal.UserMessage](t, h.events())); got != 1 {
		t.Errorf("got %d UserMessage events, want 1", got)
	}
}
