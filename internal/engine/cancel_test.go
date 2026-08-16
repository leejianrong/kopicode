package engine_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
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

	// The cancellation itself is on the record and says what it caught. Without
	// it the whole story of this turn is a ProviderRequest with no
	// ProviderResponse, which is an absence and reads identically to a crash
	// (KAN-857).
	cancelled := sole[journal.TurnCancelled](t, evs)
	if cancelled.Phase != "provider_stream" {
		t.Errorf("phase = %q, want %q — the reply was still arriving", cancelled.Phase, "provider_stream")
	}
	if !strings.HasSuffix(cancelled.Detail.Inline, context.Canceled.Error()) {
		t.Errorf("detail = %q, want the wrapped error the loop saw, ending in %q",
			cancelled.Detail.Inline, context.Canceled)
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

// TestAMidStreamCancellationIsOnTheRecordOnDisk is KAN-857's test.
//
// It causes a real cancellation — the context is cancelled from inside the
// stream callback, between two tokens of a reply that is still arriving — and
// then asserts on the **file**, read as bytes, rather than on the payload the
// emitter was handed. Everything upstream of the file is plumbing this card
// could have got right while leaving the record exactly as empty as before.
//
// The three things a reader needs, and what each one rules out:
//
//   - a TurnCancelled line exists → the record says the turn was interrupted,
//     rather than the reader inferring it from a ProviderRequest with no
//     ProviderResponse after it;
//   - it carries the turn and the phase → which turn, and what was in flight;
//   - the file ends with a newline → the record was closed rather than cut off.
//     internal/journal refuses an unterminated final line precisely because a
//     partial write can be valid JSON, so this is the check that separates "the
//     turn was cancelled" from "the process died mid-write".
func TestAMidStreamCancellationIsOnTheRecordOnDisk(t *testing.T) {
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

	if _, err := h.eng.Run(ctx, "read it"); !errors.Is(err, context.Canceled) {
		t.Fatalf("Run = %v, want an error wrapping context.Canceled", err)
	}
	if seen < 2 {
		t.Fatalf("positive control failed: the stream produced %d deltas, so the cancellation "+
			"never landed mid-reply", seen)
	}
	if err := h.eng.Close(ctx); err != nil {
		t.Fatalf("Close: %v", err)
	}

	path := filepath.Join(journal.SessionDir(h.root, "session-under-test"), journal.EventsFile)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading the record: %v", err)
	}
	if len(raw) == 0 || raw[len(raw)-1] != '\n' {
		t.Fatalf("the record's last line has no newline terminator, so a reader cannot tell it "+
			"from a write a crash caught in flight:\n%s", raw)
	}

	var line []byte
	for _, l := range bytes.Split(bytes.TrimSuffix(raw, []byte("\n")), []byte("\n")) {
		if bytes.Contains(l, []byte(`"type":"TurnCancelled"`)) {
			line = l
			break
		}
	}
	if line == nil {
		t.Fatalf("no TurnCancelled line in the record, so a cancelled turn is still only an "+
			"absence:\n%s", raw)
	}

	var ev journal.Event
	if err := json.Unmarshal(line, &ev); err != nil {
		t.Fatalf("decoding %s: %v", line, err)
	}
	if ev.Turn != 1 {
		t.Errorf("turn = %d, want 1 — the turn that was interrupted, not the session", ev.Turn)
	}
	if ev.Seq == 0 {
		t.Error("seq = 0, which is engine.Event's marker for a delta: this is in the record")
	}
	got, ok := ev.Payload.(journal.TurnCancelled)
	if !ok {
		t.Fatalf("payload is %T, want journal.TurnCancelled", ev.Payload)
	}
	if got.Phase != "provider_stream" {
		t.Errorf("phase = %q, want %q", got.Phase, "provider_stream")
	}
	if !strings.HasSuffix(got.Detail.Inline, context.Canceled.Error()) {
		t.Errorf("detail = %q, want the loop's error ending in %q", got.Detail.Inline, context.Canceled)
	}
	// The same string SessionEnded carries. They are one fact, and a shortened
	// copy on either would be a second account of it.
	if ended := sole[journal.SessionEnded](t, h.events()); ended.Detail.Inline != got.Detail.Inline {
		t.Errorf("TurnCancelled.Detail = %q but SessionEnded.Detail = %q; the record gives two "+
			"accounts of one cancellation", got.Detail.Inline, ended.Detail.Inline)
	}
}

// TestACancelledTurnSurvivesASessionThatCompletes is the absence this card is
// named for, and the reason the cancellation is its own event rather than a
// field on SessionEnded.
//
// The REPL's whole point is that Ctrl-C ends a turn and not the session: the
// prompt comes back, the user types again, and the session closes cleanly. So
// the session's own ending says "completed" — truthfully — and if that were the
// only place a cancellation could be recorded, the interruption would leave no
// trace at all.
func TestACancelledTurnSurvivesASessionThatCompletes(t *testing.T) {
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

	if _, err := h.eng.Run(ctx, "read it"); !errors.Is(err, context.Canceled) {
		t.Fatalf("first Run = %v, want an error wrapping context.Canceled", err)
	}

	// The user types again. A fresh context, because the REPL gives every turn a
	// child of the session's and the cancelled one is gone.
	res, err := h.eng.Run(t.Context(), "what did you find")
	if err != nil {
		t.Fatalf("second Run: %v", err)
	}
	if res.Stop != engine.StopCompleted {
		t.Fatalf("second Run stopped %s, want %s", res.Stop, engine.StopCompleted)
	}
	if err := h.eng.Close(t.Context()); err != nil {
		t.Fatalf("Close: %v", err)
	}

	evs := h.events()
	ended := sole[journal.SessionEnded](t, evs)
	if ended.Reason != "completed" {
		t.Fatalf("session ended %q, want \"completed\" — this test is only about a cancellation "+
			"that a clean ending would otherwise hide", ended.Reason)
	}

	cancelled := payloadsOf[journal.TurnCancelled](t, evs)
	if len(cancelled) != 1 {
		t.Fatalf("got %d TurnCancelled events, want 1: the session ended \"completed\", so this "+
			"event is the only thing on the record that says a turn was interrupted", len(cancelled))
	}
	if cancelled[0].Phase != "provider_stream" {
		t.Errorf("phase = %q, want %q", cancelled[0].Phase, "provider_stream")
	}

	// Which turn, checked on the envelope: a TurnCancelled names a turn and a
	// SessionEnded names none, which is how a reader tells a cancelled turn from
	// a cancelled session.
	for _, ev := range evs {
		switch ev.Payload.(type) {
		case journal.TurnCancelled:
			if ev.Turn != 1 {
				t.Errorf("TurnCancelled is on turn %d, want the interrupted turn 1", ev.Turn)
			}
		case journal.SessionEnded:
			if ev.Turn != 0 {
				t.Errorf("SessionEnded is on turn %d, want 0 — it is outside any turn", ev.Turn)
			}
		}
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
