package engine_test

import (
	"context"
	"errors"
	"testing"

	"github.com/leejianrong/kopicode/internal/engine"
	"github.com/leejianrong/kopicode/internal/journal"
)

// This file is docs/adr/0009-ask-tool-contract.md's own test seam: the ask
// tool's dispatch, driven through the engine's public interface with the
// mock/replay provider and a scripted Answerer, exactly the primary seam
// CLAUDE.md's "Test seam" section names. Nothing here reaches into runAsk's
// internals — every assertion is a journal event, a tool result string, or a
// Stop the session settled on.

// scriptedAnswerer returns a fixed reply, or a fixed error, and records the
// request it was asked so a test can check what the tool actually sent.
type scriptedAnswerer struct {
	answer engine.AskAnswer
	err    error
	seen   []engine.AskRequest
}

func (s *scriptedAnswerer) Ask(_ context.Context, req engine.AskRequest) (engine.AskAnswer, error) {
	s.seen = append(s.seen, req)
	if s.err != nil {
		return engine.AskAnswer{}, s.err
	}
	return s.answer, nil
}

// askTurn is the two-reply script every test below drives: the model calls
// ask with a question and some context, then — once it has the tool
// result — replies with prose and no further call, the clean stop.
func askTurn(question, askContext string) []scriptedReply {
	return []scriptedReply{
		{
			calls: []wireCall{nativeCall("call-ask-1", "ask",
				`{"question":"`+question+`","context":"`+askContext+`"}`)},
			usage: wireUsage{Prompt: 10, Completion: 5, Total: 15},
		},
		{text: "thanks, using that.", usage: wireUsage{Prompt: 20, Completion: 5, Total: 25}},
	}
}

// TestAskRoundTripsAHumanAnswer is the card's headline: a full
// ask-question-get-answer-continue-the-turn exchange, journaled correctly,
// through Engine.Run.
func TestAskRoundTripsAHumanAnswer(t *testing.T) {
	ans := &scriptedAnswerer{answer: engine.AskAnswer{Text: "config/retry.toml"}}
	h := scriptHarness(t, askTurn("which config file?", "checked config.go, no default"),
		oneAttemptPerTurn(2), nil, withMaxTurns(2), withAsk(ans.Ask, "user"))

	res, err := h.eng.Run(t.Context(), "fix the retry backoff")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Stop != engine.StopCompleted {
		t.Fatalf("stop = %s, want %s", res.Stop, engine.StopCompleted)
	}

	// The Answerer actually received the model's question and context.
	if len(ans.seen) != 1 {
		t.Fatalf("Answerer was asked %d times, want 1", len(ans.seen))
	}
	if got := ans.seen[0]; got.Question != "which config file?" || got.Context != "checked config.go, no default" {
		t.Errorf("Answerer saw %+v, want the model's own question and context", got)
	}

	events := h.events()

	req := sole[journal.AskRequested](t, events)
	if req.Question.Inline != "which config file?" {
		t.Errorf("AskRequested.Question = %q", req.Question.Inline)
	}
	if req.Context.Inline != "checked config.go, no default" {
		t.Errorf("AskRequested.Context = %q", req.Context.Inline)
	}

	ansEv := sole[journal.AskAnswered](t, events)
	if ansEv.CallID != req.CallID {
		t.Errorf("AskAnswered.CallID = %q, want %q (the same call)", ansEv.CallID, req.CallID)
	}
	if ansEv.Answer.Inline != "config/retry.toml" {
		t.Errorf("AskAnswered.Answer = %q", ansEv.Answer.Inline)
	}
	if ansEv.Source != "user" {
		t.Errorf("AskAnswered.Source = %q, want %q", ansEv.Source, "user")
	}
	if ansEv.Refused {
		t.Error("AskAnswered.Refused = true for an answer that was actually given")
	}

	// The model sees the human's answer as this call's ordinary tool result —
	// ADR-0009 decision 1: nothing new on the wire.
	var found bool
	for _, r := range payloadsOf[journal.ToolResult](t, events) {
		if r.Tool != "ask" {
			continue
		}
		found = true
		if r.Output.Inline != "config/retry.toml" {
			t.Errorf("ToolResult.Output = %q, want the human's verbatim answer", r.Output.Inline)
		}
		if r.ErrorKind != "" {
			t.Errorf("ToolResult.ErrorKind = %q, want empty (a successful call)", r.ErrorKind)
		}
	}
	if !found {
		t.Fatal("no ToolResult for the ask call")
	}

	// ask is tagged permission.OperationRead (ADR-0009 decision 2): Gate.classify
	// lets it through unconditionally, so it must raise zero permission events.
	if perms := payloadsOf[journal.PermissionRequested](t, events); len(perms) != 0 {
		t.Errorf("ask call raised %d PermissionRequested events, want 0", len(perms))
	}

	// ask changes nothing on the tree, so it earns no snapshot.
	if len(h.snap.turns) != 0 {
		t.Errorf("ask call produced a TurnSnapshot at turn(s) %v, want none", h.snap.turns)
	}
}

// TestAskEmptyReplyIsARealAnswer holds ADR-0009's Consequences section to the
// letter: an empty reply the human actually gave is not the same event as the
// question going unanswered, and free text has no safe default to fall back
// on the way a refused permission does.
func TestAskEmptyReplyIsARealAnswer(t *testing.T) {
	ans := &scriptedAnswerer{answer: engine.AskAnswer{Text: ""}}
	h := scriptHarness(t, askTurn("any preference?", ""),
		oneAttemptPerTurn(2), nil, withMaxTurns(2), withAsk(ans.Ask, "user"))

	if _, err := h.eng.Run(t.Context(), "go"); err != nil {
		t.Fatalf("Run: %v", err)
	}

	events := h.events()
	ansEv := sole[journal.AskAnswered](t, events)
	if ansEv.Answer.Inline != "" {
		t.Errorf("AskAnswered.Answer = %q, want the empty string the human actually sent", ansEv.Answer.Inline)
	}
	if ansEv.Refused {
		t.Error("an empty answer was recorded as a refusal")
	}

	for _, r := range payloadsOf[journal.ToolResult](t, events) {
		if r.Tool != "ask" {
			continue
		}
		if r.Output.Inline == "" {
			t.Error("the model was shown an empty tool result for an empty human answer; " +
				"it cannot tell that apart from a tool producing nothing")
		}
	}
}

// TestAskRefusalIsNonFatal is ADR-0009 decision 2's Alternatives-rejected
// case made concrete: an Answerer that cannot get an answer ends the call,
// not the exchange, and the model reads the refusal as an ordinary tool
// result and carries on to a clean stop.
func TestAskRefusalIsNonFatal(t *testing.T) {
	refusal := errors.New("no human is present to answer this question")
	ans := &scriptedAnswerer{err: refusal}
	h := scriptHarness(t, askTurn("which config file?", ""),
		oneAttemptPerTurn(2), nil, withMaxTurns(2), withAsk(ans.Ask, "policy"))

	res, err := h.eng.Run(t.Context(), "go")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Stop != engine.StopCompleted {
		t.Fatalf("stop = %s, want %s — a refused ask must not end the exchange", res.Stop, engine.StopCompleted)
	}

	events := h.events()
	ansEv := sole[journal.AskAnswered](t, events)
	if !ansEv.Refused {
		t.Error("AskAnswered.Refused = false for a call the Answerer errored on")
	}
	if ansEv.Source != "policy" {
		t.Errorf("AskAnswered.Source = %q, want %q", ansEv.Source, "policy")
	}
	if ansEv.Answer.Inline != refusal.Error() {
		t.Errorf("AskAnswered.Answer = %q, want the refusal's own text %q", ansEv.Answer.Inline, refusal.Error())
	}

	for _, r := range payloadsOf[journal.ToolResult](t, events) {
		if r.Tool != "ask" {
			continue
		}
		if r.Output.Inline != refusal.Error() {
			t.Errorf("ToolResult.Output = %q, want the refusal text so the model can read it", r.Output.Inline)
		}
		// tools.FaultTask's wire value. A refusal is "the harness worked
		// exactly as designed and the answer was no" (faultOf's own doc
		// comment), never tools.FaultInternal — see dispatch.go's errAskRefused.
		if r.ErrorKind != "task" {
			t.Errorf("ToolResult.ErrorKind = %q, want %q", r.ErrorKind, "task")
		}
	}
}

// TestAskWithNoAnswererConfiguredFailsClosed is [Config.Ask]'s own default:
// a caller that builds a Config directly and forgets the field gets a
// fail-closed refusal, not a panic and not a silent "yes" — mirroring
// [asker.Ask]'s nil-Consenter direction.
func TestAskWithNoAnswererConfiguredFailsClosed(t *testing.T) {
	// No withAsk: cfg.Ask and cfg.AskSource are left at their zero values,
	// which is exactly what internal/bench's session builder does today (see
	// internal/bench/session.go) and what New defaults for.
	h := scriptHarness(t, askTurn("which config file?", ""), oneAttemptPerTurn(2), nil, withMaxTurns(2))

	res, err := h.eng.Run(t.Context(), "go")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Stop != engine.StopCompleted {
		t.Fatalf("stop = %s, want %s", res.Stop, engine.StopCompleted)
	}

	events := h.events()
	ansEv := sole[journal.AskAnswered](t, events)
	if !ansEv.Refused {
		t.Error("an unconfigured Ask answered instead of refusing")
	}
	if ansEv.Source != "policy" {
		t.Errorf("AskAnswered.Source = %q, want %q — nobody wired an Answerer, "+
			"so the honest attribution is policy, never a human", ansEv.Source, "policy")
	}
}

// TestOpenWiresAskThroughOptions is the same round trip as
// TestAskRoundTripsAHumanAnswer, but through [engine.Open] rather than
// [engine.New] directly — the seam a real front end actually drives, and the
// one that resolves [engine.Options.Ask]/[engine.Options.AskMode] into
// [engine.Config.Ask]/[engine.Config.AskSource] via mustAnswerer. Nothing
// about ask's dispatch is retested here; what is under test is that Open's
// wiring reaches the same place harness_test.go's withAsk reaches by hand.
func TestOpenWiresAskThroughOptions(t *testing.T) {
	ans := &scriptedAnswerer{answer: engine.AskAnswer{Text: "config/retry.toml"}}
	prov := script(t, askTurn("which config file?", "checked config.go"), oneAttemptPerTurn(2))

	s, _ := openWithProvider(t, prov, engine.Options{
		Ask: ans.Ask,
		// AskUnattended so the test also proves mustAnswerer derives Source
		// from AskMode alone, independent of Ask being non-nil — the same
		// property mustAskPolicy already holds for Consent/ConsentMode.
		AskMode: engine.AskUnattended,
	})

	res, err := s.Run(t.Context(), "fix the retry backoff")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Stop != engine.StopCompleted {
		t.Fatalf("stop = %s, want %s", res.Stop, engine.StopCompleted)
	}
	if err := s.Close(t.Context()); err != nil {
		t.Fatalf("Close: %v", err)
	}

	events := readJournal(t, s.Path())
	ansEv := sole[journal.AskAnswered](t, events)
	if ansEv.Answer.Inline != "config/retry.toml" {
		t.Errorf("AskAnswered.Answer = %q", ansEv.Answer.Inline)
	}
	if ansEv.Source != "policy" {
		t.Errorf("AskAnswered.Source = %q, want %q — AskMode was AskUnattended", ansEv.Source, "policy")
	}
	if ansEv.Refused {
		t.Error("AskAnswered.Refused = true for an answer that was actually given")
	}
}
