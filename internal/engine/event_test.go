package engine_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/leejianrong/kopicode/internal/engine"
	"github.com/leejianrong/kopicode/internal/journal"
)

// samplePayloads is one populated value of every payload type the journal can
// decode, plus an unknown one.
//
// It is a hand-written table held to the journal's own registry by
// TestEveryJournalPayloadCrosses below, which is what keeps it honest: a
// payload type added to internal/journal and not added here fails that test
// rather than quietly reaching a surface as [engine.EventUnspecified].
func samplePayloads() []journal.Payload {
	exit := 3
	return []journal.Payload{
		journal.SessionStarted{
			CWD: "/repo", RepoHead: "abc123", ModelID: "qwen/qwen3-coder-next",
			HarnessConfigHash: "h1", Build: journal.BuildInfo{TreeState: "clean"},
		},
		journal.UserMessage{Text: journal.InlineText("fix it")},
		journal.AssistantMessage{Text: journal.InlineText("on it")},
		journal.ThinkingBlock{Text: journal.InlineText("hmm")},
		journal.ProviderRequest{ModelID: "qwen/qwen3-coder-next", Attempt: 2},
		journal.ProviderRetried{Attempt: 1, Try: 2, OfTries: 6, DelayMS: 2000, Cause: "http 429"},
		journal.ProviderResponse{
			Body: journal.InlineText("{}"), FinishReason: "stop", ServedBy: "parasail/bf16",
			Tokens: journal.TokenCounts{Total: 42},
		},
		journal.ToolCallRequested{CallID: "kc-1", Raw: journal.InlineText("```tool")},
		journal.ToolCallParsed{
			CallID: "kc-1", Tool: "read_file", Args: json.RawMessage(`{"path":"x.go"}`),
			Route: "native", ArgEncoding: "object",
		},
		journal.ToolCallRepaired{
			CallID: "kc-1", Attempt: 1, Classification: "invalid_json",
			Error: journal.InlineText("that was not JSON"),
		},
		journal.ToolCallFailed{
			CallID: "kc-1", Tool: "edit_file", Reason: "unknown_tool",
			Detail: journal.InlineText("no such tool"),
		},
		journal.ToolResult{
			CallID: "kc-1", Tool: "run_shell", Output: journal.InlineText("ok"),
			ExitCode: &exit, ErrorKind: "task",
		},
		journal.EditApplied{
			CallID: "kc-1", Path: "x.go", Mode: "anchored", AnchorStart: "a1b2c3d4",
			AnchorEnd: "e5f6a7b8", AnchorVersion: "v1", Diff: journal.InlineText("@@"),
		},
		journal.EditRejected{
			CallID: "kc-1", Path: "x.go", Mode: "anchored", Reason: "anchor_drift",
			Detail: journal.InlineText("current anchors follow"),
		},
		journal.SyntaxGateRun{
			Path: "x.go", Checker: "gofmt -e", Ran: true, ExitCode: 1,
			Output: journal.InlineText("x.go:3: expected ';'"),
		},
		journal.PermissionRequested{
			RequestID: "kc-1", Kind: "run_shell", Tool: "run_shell",
			Detail: journal.InlineText("go test ./..."), Reason: "always asks",
		},
		journal.PermissionDecided{
			RequestID: "kc-1", Decision: "allow", Source: "user", Reason: "user said yes",
		},
		journal.TurnSnapshot{Ref: "refs/kopicode/s/1", Commit: "c1", Tree: "t1"},
		journal.VerificationRun{
			Command: []string{"go", "test", "./..."}, Source: "discovered", ExitCode: 1,
			Output: journal.InlineText("FAIL"),
		},
		journal.TurnCancelled{Phase: "provider_stream", Detail: journal.InlineText("context canceled")},
		journal.SessionEnded{Reason: "completed", ExitCode: 0, Detail: journal.InlineText("")},
		journal.UnknownPayload{EventType: "SomethingNewer", Raw: json.RawMessage(`{"a":1}`)},
	}
}

// TestEveryJournalPayloadCrosses is the guard the event stream rests on.
//
// A payload type with no case in the mapper reaches a surface as
// EventUnspecified, which is a fact the record holds and the user was never
// told about — the specific failure the stream exists to prevent, and one that
// arrives silently, on the day someone adds an event type for an unrelated
// reason.
//
// The table is checked against journal.KnownTypes rather than against itself,
// so forgetting to extend the table fails here too.
func TestEveryJournalPayloadCrosses(t *testing.T) {
	sampled := map[journal.Type]bool{}
	for _, p := range samplePayloads() {
		sampled[p.Type()] = true
	}
	for _, known := range journal.KnownTypes() {
		if !sampled[known] {
			t.Errorf("journal.%s has no sample in samplePayloads(), so nothing checks that it "+
				"reaches a surface at all", known)
		}
	}

	for _, p := range samplePayloads() {
		ev := journal.Event{
			SchemaVersion: journal.SchemaVersion,
			SessionID:     "s-1",
			Seq:           7,
			Turn:          3,
			Time:          time.Unix(0, 0).UTC(),
			Payload:       p,
		}

		got := engine.EventOfForTest(ev)
		if got.Kind == engine.EventUnspecified {
			t.Errorf("journal.%s maps to EventUnspecified: a surface would be told an event "+
				"happened and nothing about what it was", p.Type())
		}
		if got.Kind == engine.EventDelta {
			t.Errorf("journal.%s maps to EventDelta, which is the one kind with no record "+
				"behind it", p.Type())
		}
		if got.Seq != 7 || got.Turn != 3 {
			t.Errorf("journal.%s lost its envelope: seq %d turn %d, want 7 and 3",
				p.Type(), got.Seq, got.Turn)
		}
	}
}

// TestEveryEventKindIsReachable is the other direction.
//
// A kind declared and never produced is a case a surface has to handle for no
// reason, and — worse — reads as evidence that something is rendered when
// nothing produces it.
func TestEveryEventKindIsReachable(t *testing.T) {
	produced := map[engine.EventKind]bool{
		// The only kind produced outside the mapper.
		engine.EventDelta: true,
	}
	for _, p := range samplePayloads() {
		ev := journal.Event{
			SchemaVersion: journal.SchemaVersion, SessionID: "s", Seq: 1, Turn: 1,
			Time: time.Unix(0, 0).UTC(), Payload: p,
		}
		produced[engine.EventOfForTest(ev).Kind] = true
	}

	for k := engine.EventSessionStarted; k <= engine.EventDelta; k++ {
		if !produced[k] {
			t.Errorf("engine.%s is declared but nothing produces it", k)
		}
	}
}

// TestTheEventKindNamesAreStable. A surface renders by kind and a report reads
// the name; renaming one silently is how a log filter stops matching.
func TestTheEventKindNamesAreStable(t *testing.T) {
	want := map[engine.EventKind]string{
		engine.EventUnspecified:         "unspecified",
		engine.EventSessionStarted:      "session_started",
		engine.EventUserMessage:         "user_message",
		engine.EventAssistantMessage:    "assistant_message",
		engine.EventThinking:            "thinking",
		engine.EventProviderRequest:     "provider_request",
		engine.EventProviderRetried:     "provider_retried",
		engine.EventProviderResponse:    "provider_response",
		engine.EventToolCallRequested:   "tool_call_requested",
		engine.EventToolCallParsed:      "tool_call_parsed",
		engine.EventToolCallRepaired:    "tool_call_repaired",
		engine.EventToolCallFailed:      "tool_call_failed",
		engine.EventToolResult:          "tool_result",
		engine.EventEditApplied:         "edit_applied",
		engine.EventEditRejected:        "edit_rejected",
		engine.EventSyntaxGate:          "syntax_gate",
		engine.EventPermissionRequested: "permission_requested",
		engine.EventPermissionDecided:   "permission_decided",
		engine.EventTurnSnapshot:        "turn_snapshot",
		engine.EventVerification:        "verification",
		engine.EventTurnCancelled:       "turn_cancelled",
		engine.EventSessionEnded:        "session_ended",
		engine.EventUnknown:             "unknown",
		engine.EventDelta:               "delta",
	}
	for k, name := range want {
		if got := k.String(); got != name {
			t.Errorf("EventKind(%d).String() = %q, want %q", uint8(k), got, name)
		}
	}
}

// TestAnUnknownPayloadKeepsItsNameAndItsBytes. The journal's compatibility
// promise is that an event type this build does not know survives a round trip;
// a surface being told only that "something happened" would drop the half of
// that promise a person can act on.
func TestAnUnknownPayloadKeepsItsNameAndItsBytes(t *testing.T) {
	ev := journal.Event{
		SchemaVersion: journal.SchemaVersion, SessionID: "s", Seq: 4, Turn: 1,
		Time: time.Unix(0, 0).UTC(),
		// A type name this build genuinely does not know. It used to be
		// "TurnCancelled", which stopped being a future type the day KAN-857
		// registered it — an unknown-payload fixture named after a known type
		// would still pass while testing something other than what it says.
		Payload: journal.UnknownPayload{EventType: "SomethingNewer", Raw: json.RawMessage(`{"why":"ctrl-c"}`)},
	}

	got := engine.EventOfForTest(ev)
	if got.Kind != engine.EventUnknown {
		t.Errorf("kind = %s, want %s", got.Kind, engine.EventUnknown)
	}
	if got.Reason != "SomethingNewer" {
		t.Errorf("Reason = %q, want the type name", got.Reason)
	}
	if got.Text != `{"why":"ctrl-c"}` {
		t.Errorf("Text = %q, want the payload bytes verbatim", got.Text)
	}
}

// TestASpilledValueReportsItsSizeRatherThanNothing.
//
// A payload field over the journal's threshold is stored out of line, so on the
// append path Inline is empty and only Size is known. A surface must be able to
// say "68 KiB of output" rather than showing nothing and implying there was
// none — nothing is truncated here because nothing is carried here, and the
// whole value is in the record.
func TestASpilledValueReportsItsSizeRatherThanNothing(t *testing.T) {
	ev := journal.Event{
		SchemaVersion: journal.SchemaVersion, SessionID: "s", Seq: 5, Turn: 1,
		Time: time.Unix(0, 0).UTC(),
		Payload: journal.ToolResult{
			CallID: "kc-1", Tool: "run_shell",
			Output: journal.BlobText("deadbeef", 70000),
		},
	}

	got := engine.EventOfForTest(ev)
	if got.Size != 70000 {
		t.Errorf("Size = %d, want 70000 — the size the record holds", got.Size)
	}
	if got.Text != "" {
		t.Errorf("Text = %q, want empty: a spilled value is in a blob, not in the event", got.Text)
	}
}
