package journal_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/leejianrong/kopicode/internal/journal"
)

// stamp is a fixed time so an encoded event is a fixed string.
var stamp = time.Date(2026, 8, 11, 9, 30, 0, 0, time.UTC)

func seed(seq uint64, p journal.Payload) journal.Event {
	return journal.Event{
		SchemaVersion: journal.SchemaVersion,
		SessionID:     "01JQ0000000000000000000000",
		Seq:           seq,
		Turn:          3,
		Time:          stamp,
		Payload:       p,
	}
}

func intPtr(i int) *int          { return &i }
func i64Ptr(i int64) *int64      { return &i }
func text(s string) journal.Text { return journal.InlineText(s) }

// fixtures is one populated payload per known type. Every field is set to a
// non-zero value, so a field that fails to round-trip shows up as a diff rather
// than as two matching zeroes.
//
// TestFixturesCoverEveryKnownType makes this table a gate: a new event type
// without a fixture fails the suite.
func fixtures() map[journal.Type]journal.Payload {
	pin := journal.ProviderPin{
		Order:          []string{"deepinfra/fp8"},
		AllowFallbacks: false,
		Quantizations:  []string{"fp8"},
	}
	tokens := journal.TokenCounts{Prompt: 1200, Completion: 340, Total: 1540}

	return map[journal.Type]journal.Payload{
		journal.TypeSessionStarted: journal.SessionStarted{
			CWD:               "/home/jian/projects/kopicode",
			RepoHead:          "6c9eab8f0e2b1c",
			ModelID:           "qwen/qwen3-coder-next",
			Provider:          pin,
			HarnessConfigHash: "sha256:abc123",
			Build: journal.BuildInfo{
				Version:   "v0.1.0-3-g6c9eab8",
				Commit:    "6c9eab8f0e2b1c4d5e6f708192a3b4c5d6e7f809",
				TreeState: "dirty",
				Source:    "ldflags",
			},
		},
		journal.TypeUserMessage:      journal.UserMessage{Text: text("fix the failing test in internal/parse")},
		journal.TypeAssistantMessage: journal.AssistantMessage{Text: text("Reading the test first.")},
		journal.TypeThinkingBlock:    journal.ThinkingBlock{Text: text("the anchor probably drifted")},
		journal.TypeProviderRequest: journal.ProviderRequest{
			ModelID:  "qwen/qwen3-coder-next",
			Provider: pin,
			Sampling: journal.Sampling{Temperature: 0.2, TopP: 0.95, MaxTokens: 8192, Seed: i64Ptr(7)},
			Tokens:   tokens,
			Attempt:  1,
		},
		journal.TypeProviderRetried: journal.ProviderRetried{
			Attempt: 1,
			Try:     2,
			OfTries: 6,
			DelayMS: 2000,
			Cause:   "http 429",
		},
		journal.TypeProviderResponse: journal.ProviderResponse{
			Body:         journal.BlobText("e3b0c44298fc1c149afbf4c8996fb924", 91234),
			Tokens:       tokens,
			FinishReason: "tool_calls",
			ServedBy:     "deepinfra/fp8",
		},
		journal.TypeToolCallRequested: journal.ToolCallRequested{
			CallID: "call_1",
			Raw:    text("```tool\n{\"tool\":\"read_file\"}\n```"),
		},
		journal.TypeToolCallParsed: journal.ToolCallParsed{
			CallID: "call_1",
			Tool:   "read_file",
			Args:   json.RawMessage(`{"path":"internal/parse/parse.go"}`),
			Route:  "fenced_json",
			// Deliberately the non-zero encoding. "object" is what
			// parse.ArgsObject stringifies to and is also what a fixture would
			// carry by accident, so a round trip that lost the field would
			// still look right.
			ArgEncoding: "json_string",
		},
		journal.TypeToolCallRepaired: journal.ToolCallRepaired{
			CallID:         "call_1",
			Attempt:        2,
			Classification: "missing_arg",
			Error:          text(`read_file needs "path"; you sent {"file":"x.go"}`),
		},
		journal.TypeToolCallFailed: journal.ToolCallFailed{
			CallID: "call_1",
			Tool:   "read_file",
			Reason: "repairs_exhausted",
			Detail: text("2 repair attempts, still unparseable"),
		},
		journal.TypeToolResult: journal.ToolResult{
			CallID:    "call_1",
			Tool:      "run_shell",
			Output:    text("ok  github.com/leejianrong/kopicode/internal/parse  0.4s"),
			ExitCode:  intPtr(0),
			ErrorKind: "task",
		},
		journal.TypeEditApplied: journal.EditApplied{
			CallID:        "call_2",
			Path:          "internal/parse/parse.go",
			Mode:          "anchored",
			AnchorStart:   "a1b2",
			AnchorEnd:     "c3d4",
			AnchorVersion: "v1",
			Diff:          text("@@ -1,3 +1,3 @@\n-old\n+new\n"),
		},
		journal.TypeEditRejected: journal.EditRejected{
			CallID:      "call_3",
			Path:        "internal/parse/parse.go",
			Mode:        "anchored",
			Reason:      "anchor_drift",
			AnchorStart: "a1b2",
			AnchorEnd:   "c3d4",
			Detail:      text("current anchors: 9f8e, 7d6c"),
		},
		journal.TypeSyntaxGateRun: journal.SyntaxGateRun{
			Path:     "internal/parse/parse.go",
			Checker:  "gofmt -e",
			Ran:      true,
			ExitCode: 1,
			Output:   text("parse.go:12:1: expected declaration"),
		},
		journal.TypePermissionRequested: journal.PermissionRequested{
			RequestID: "perm_1",
			Kind:      "run_shell",
			Tool:      "run_shell",
			Detail:    text("go test ./... && gofmt -l ."),
			Reason:    "run_shell always asks",
		},
		journal.TypePermissionDecided: journal.PermissionDecided{
			RequestID: "perm_1",
			Decision:  "allow_session",
			Source:    "user",
			Reason:    "approved at the prompt",
		},
		journal.TypeTurnSnapshot: journal.TurnSnapshot{
			Ref:    "refs/kopicode/01JQ0/3",
			Commit: "1111111111111111111111111111111111111111",
			Tree:   "2222222222222222222222222222222222222222",
			Parent: "3333333333333333333333333333333333333333",
		},
		journal.TypeVerificationRun: journal.VerificationRun{
			Command:  []string{"uv", "run", "pytest"},
			Source:   "configured",
			ExitCode: -1,
			Skip:     "tool_missing",
			Output:   text("verification: nothing ran, so nothing here is a pass — `uv` is not installed"),
		},
		journal.TypeTurnCancelled: journal.TurnCancelled{
			Phase:  "provider_stream",
			Detail: text("context canceled"),
		},
		journal.TypeSessionEnded: journal.SessionEnded{
			Reason:   "completed",
			ExitCode: 0,
			Detail:   text("task complete"),
		},
	}
}

// TestFixturesCoverEveryKnownType keeps the round-trip table honest. Adding an
// event type without a fixture would otherwise leave it untested and look green.
func TestFixturesCoverEveryKnownType(t *testing.T) {
	f := fixtures()
	for _, typ := range journal.KnownTypes() {
		if _, ok := f[typ]; !ok {
			t.Errorf("event type %q has no round-trip fixture — add one to fixtures()", typ)
		}
	}
	for typ := range f {
		if _, err := journal.ParsePayload(typ, []byte(`{}`)); err != nil {
			t.Errorf("fixture type %q is not registered: %v", typ, err)
		}
	}
}

// TestRoundTripEveryKnownType is property 1: every type survives encode/decode
// with its discriminator intact.
func TestRoundTripEveryKnownType(t *testing.T) {
	var seq uint64
	for _, typ := range journal.KnownTypes() {
		seq++
		payload := fixtures()[typ]

		t.Run(string(typ), func(t *testing.T) {
			if payload.Type() != typ {
				t.Fatalf("payload reports type %q but is registered under %q", payload.Type(), typ)
			}

			ev := seed(seq, payload)
			line, err := journal.Marshal(ev)
			if err != nil {
				t.Fatalf("Marshal: %v", err)
			}

			// The discriminator must be on the wire, not merely in Go.
			var wire struct {
				SchemaVersion int             `json:"schema_version"`
				SessionID     string          `json:"session_id"`
				Seq           uint64          `json:"seq"`
				Turn          int             `json:"turn"`
				Type          string          `json:"type"`
				TS            string          `json:"ts"`
				Payload       json.RawMessage `json:"payload"`
			}
			if err := json.Unmarshal(line, &wire); err != nil {
				t.Fatalf("encoded event is not valid JSON: %v\n%s", err, line)
			}
			if wire.Type != string(typ) {
				t.Errorf("wire type = %q, want %q", wire.Type, typ)
			}
			if wire.SchemaVersion != journal.SchemaVersion {
				t.Errorf("wire schema_version = %d, want %d", wire.SchemaVersion, journal.SchemaVersion)
			}
			if wire.Seq != seq || wire.SessionID != ev.SessionID || wire.Turn != ev.Turn {
				t.Errorf("envelope not preserved: %+v", wire)
			}
			if wire.TS != "2026-08-11T09:30:00Z" {
				t.Errorf("ts = %q, want RFC 3339 UTC", wire.TS)
			}
			if len(wire.Payload) == 0 || string(wire.Payload) == "null" {
				t.Errorf("payload missing from the line: %s", line)
			}

			var got journal.Event
			if err := json.Unmarshal(line, &got); err != nil {
				t.Fatalf("Unmarshal: %v", err)
			}
			if diff := cmp.Diff(ev, got); diff != "" {
				t.Errorf("round trip changed the event (-want +got):\n%s", diff)
			}
			if got.Type() != typ {
				t.Errorf("decoded Type() = %q, want %q", got.Type(), typ)
			}

			// Encoding the decoded event reproduces the same bytes.
			again, err := journal.Marshal(got)
			if err != nil {
				t.Fatalf("re-Marshal: %v", err)
			}
			if string(again) != string(line) {
				t.Errorf("re-encode differs:\n first: %s\nsecond: %s", line, again)
			}
		})
	}
}

// TestUnknownTypeSurvivesRoundTrip is property 2, and the one that is easy to
// pass while still losing data: keeping the type string but dropping the
// payload would satisfy a weaker assertion than this one.
func TestUnknownTypeSurvivesRoundTrip(t *testing.T) {
	const line = `{"schema_version":2,"session_id":"s1","seq":9,"turn":4,` +
		`"type":"SubagentSpawned","ts":"2026-08-11T09:30:00Z",` +
		`"payload":{"agent":"reviewer","budget_tokens":50000,"nested":{"a":[1,2,3]}}}`

	var ev journal.Event
	if err := json.Unmarshal([]byte(line), &ev); err != nil {
		t.Fatalf("an unknown type must decode, not error: %v", err)
	}

	if ev.Type() != journal.Type("SubagentSpawned") {
		t.Errorf("Type() = %q, want SubagentSpawned", ev.Type())
	}
	unknown, ok := ev.Payload.(journal.UnknownPayload)
	if !ok {
		t.Fatalf("payload is %T, want journal.UnknownPayload", ev.Payload)
	}
	const wantPayload = `{"agent":"reviewer","budget_tokens":50000,"nested":{"a":[1,2,3]}}`
	if string(unknown.Raw) != wantPayload {
		t.Errorf("payload bytes not preserved:\n got: %s\nwant: %s", unknown.Raw, wantPayload)
	}
	if ev.SchemaVersion != 2 {
		t.Errorf("schema_version = %d, want 2 — a future version must not be rewritten", ev.SchemaVersion)
	}

	// The bytes, not just the absence of an error.
	out, err := journal.Marshal(ev)
	if err != nil {
		t.Fatalf("re-encoding an unknown type: %v", err)
	}
	if string(out) != line {
		t.Errorf("unknown event did not survive:\n got: %s\nwant: %s", out, line)
	}
}

// TestUnknownEnvelopeFieldSurvives covers the other half of reading a journal
// from a newer build: a field added to the envelope must not be dropped either.
func TestUnknownEnvelopeFieldSurvives(t *testing.T) {
	const line = `{"schema_version":3,"session_id":"s1","seq":1,"turn":0,` +
		`"type":"UserMessage","ts":"2026-08-11T09:30:00Z",` +
		`"payload":{"text":{"inline":"hi","size":2}},"branch_id":"fork-7"}`

	var ev journal.Event
	if err := json.Unmarshal([]byte(line), &ev); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if got := string(ev.Extra["branch_id"]); got != `"fork-7"` {
		t.Fatalf("Extra[branch_id] = %q, want \"fork-7\"", got)
	}

	out, err := journal.Marshal(ev)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var back map[string]json.RawMessage
	if err := json.Unmarshal(out, &back); err != nil {
		t.Fatalf("re-encoded event is not valid JSON: %v", err)
	}
	if string(back["branch_id"]) != `"fork-7"` {
		t.Errorf("unknown envelope field lost on re-encode: %s", out)
	}
	if string(back["type"]) != `"UserMessage"` {
		t.Errorf("discriminator lost on re-encode: %s", out)
	}
	// The known payload still decodes as itself.
	if _, ok := ev.Payload.(journal.UserMessage); !ok {
		t.Errorf("payload is %T, want journal.UserMessage", ev.Payload)
	}
}

// TestExtraCannotShadowAnEnvelopeField: a preserved field must never be able to
// rewrite the discriminator on the way out.
func TestExtraCannotShadowAnEnvelopeField(t *testing.T) {
	ev := seed(1, journal.UserMessage{Text: journal.InlineText("hi")})
	ev.Extra = map[string]json.RawMessage{"type": json.RawMessage(`"Forged"`)}

	out, err := journal.Marshal(ev)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var back map[string]json.RawMessage
	if err := json.Unmarshal(out, &back); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if string(back["type"]) != `"UserMessage"` {
		t.Errorf("Extra overwrote the discriminator: %s", out)
	}
}

// TestMissingOrUnknownDiscriminator is property 3. The message has to name what
// was missing and where, or whoever hits it at 2am deletes the guard.
func TestMissingOrUnknownDiscriminator(t *testing.T) {
	t.Run("missing on the wire", func(t *testing.T) {
		const line = `{"schema_version":1,"session_id":"s1","seq":7,"turn":2,` +
			`"ts":"2026-08-11T09:30:00Z","payload":{}}`

		var ev journal.Event
		err := json.Unmarshal([]byte(line), &ev)
		if err == nil {
			t.Fatal("an event with no type decoded without error")
		}
		if !errors.Is(err, journal.ErrMissingField) {
			t.Errorf("errors.Is(err, ErrMissingField) = false: %v", err)
		}
		var fe *journal.FieldError
		if !errors.As(err, &fe) {
			t.Fatalf("errors.As(*FieldError) = false: %v", err)
		}
		if fe.Field != "type" {
			t.Errorf("FieldError.Field = %q, want \"type\"", fe.Field)
		}
		if fe.Seq != 7 {
			t.Errorf("FieldError.Seq = %d, want 7 — the message must say which event", fe.Seq)
		}
		for _, want := range []string{"seq 7", `"type"`, "missing"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error message %q does not mention %q", err, want)
			}
		}
	})

	t.Run("empty string on the wire", func(t *testing.T) {
		const line = `{"schema_version":1,"session_id":"s1","seq":7,"turn":2,` +
			`"type":"","ts":"2026-08-11T09:30:00Z","payload":{}}`

		var ev journal.Event
		if err := json.Unmarshal([]byte(line), &ev); !errors.Is(err, journal.ErrMissingField) {
			t.Errorf("empty type: err = %v, want ErrMissingField", err)
		}
	})

	t.Run("unknown, strictly", func(t *testing.T) {
		_, err := journal.ParsePayload("Frobnicate", []byte(`{}`))
		if !errors.Is(err, journal.ErrUnknownType) {
			t.Fatalf("ParsePayload: err = %v, want ErrUnknownType", err)
		}
		for _, want := range []string{"Frobnicate", "unknown event type", "Event.UnmarshalJSON"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error message %q does not mention %q", err, want)
			}
		}
	})

	t.Run("missing, strictly", func(t *testing.T) {
		_, err := journal.ParsePayload("", []byte(`{}`))
		if !errors.Is(err, journal.ErrMissingField) {
			t.Fatalf("ParsePayload: err = %v, want ErrMissingField", err)
		}
	})
}

// TestRejectsIncoherentEnvelopes: the envelope invariants hold in both
// directions, so anything that decodes can be encoded again.
func TestRejectsIncoherentEnvelopes(t *testing.T) {
	const payload = `"payload":{"text":{"inline":"hi","size":2}}`
	cases := []struct {
		name  string
		line  string
		field string
		is    error
	}{
		{
			name:  "no schema version",
			line:  `{"session_id":"s1","seq":1,"turn":0,"type":"UserMessage","ts":"2026-08-11T09:30:00Z",` + payload + `}`,
			field: "schema_version",
			is:    journal.ErrMissingField,
		},
		{
			name:  "no session id",
			line:  `{"schema_version":1,"seq":1,"turn":0,"type":"UserMessage","ts":"2026-08-11T09:30:00Z",` + payload + `}`,
			field: "session_id",
			is:    journal.ErrMissingField,
		},
		{
			name:  "seq is zero",
			line:  `{"schema_version":1,"session_id":"s1","seq":0,"turn":0,"type":"UserMessage","ts":"2026-08-11T09:30:00Z",` + payload + `}`,
			field: "seq",
			is:    journal.ErrInvalidField,
		},
		{
			name:  "ts is not rfc 3339",
			line:  `{"schema_version":1,"session_id":"s1","seq":1,"turn":0,"type":"UserMessage","ts":"11 Aug 2026",` + payload + `}`,
			field: "ts",
			is:    journal.ErrInvalidField,
		},
		{
			name:  "payload is not an object",
			line:  `{"schema_version":1,"session_id":"s1","seq":1,"turn":0,"type":"UserMessage","ts":"2026-08-11T09:30:00Z","payload":42}`,
			field: "payload",
			is:    journal.ErrInvalidField,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var ev journal.Event
			err := json.Unmarshal([]byte(tc.line), &ev)
			if err == nil {
				t.Fatalf("decoded without error: %s", tc.line)
			}
			if !errors.Is(err, tc.is) {
				t.Errorf("errors.Is(err, %v) = false: %v", tc.is, err)
			}
			var fe *journal.FieldError
			if !errors.As(err, &fe) {
				t.Fatalf("errors.As(*FieldError) = false: %v", err)
			}
			if fe.Field != tc.field {
				t.Errorf("FieldError.Field = %q, want %q", fe.Field, tc.field)
			}
		})
	}
}

// TestMarshalRejectsIncoherentEvents: an event the engine failed to stamp is a
// bug, and a journal line missing its seq is worse than a loud failure.
func TestMarshalRejectsIncoherentEvents(t *testing.T) {
	cases := []struct {
		name  string
		event journal.Event
		field string
	}{
		{"no payload", journal.Event{SchemaVersion: 1, SessionID: "s1", Seq: 1, Time: stamp}, "payload"},
		{"no session", journal.Event{SchemaVersion: 1, Seq: 1, Time: stamp, Payload: journal.UserMessage{}}, "session_id"},
		{"no seq", journal.Event{SchemaVersion: 1, SessionID: "s1", Time: stamp, Payload: journal.UserMessage{}}, "seq"},
		{"no clock", journal.Event{SchemaVersion: 1, SessionID: "s1", Seq: 1, Payload: journal.UserMessage{}}, "ts"},
		{"no schema version", journal.Event{SessionID: "s1", Seq: 1, Time: stamp, Payload: journal.UserMessage{}}, "schema_version"},
		{"unnamed payload", journal.Event{SchemaVersion: 1, SessionID: "s1", Seq: 1, Time: stamp,
			Payload: journal.UnknownPayload{Raw: json.RawMessage(`{}`)}}, "type"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := journal.Marshal(tc.event)
			if err == nil {
				t.Fatal("Marshal accepted an incoherent event")
			}
			var fe *journal.FieldError
			if !errors.As(err, &fe) {
				t.Fatalf("errors.As(*FieldError) = false: %v", err)
			}
			if fe.Field != tc.field {
				t.Errorf("FieldError.Field = %q, want %q", fe.Field, tc.field)
			}
		})
	}
}

// TestMarshalDoesNotEscapeHTML: a journal is read by a human, and a shell
// command full of unicode escapes is not.
func TestMarshalDoesNotEscapeHTML(t *testing.T) {
	ev := seed(1, journal.PermissionRequested{
		RequestID: "perm_1",
		Kind:      "run_shell",
		Tool:      "run_shell",
		Detail:    journal.InlineText(`go build ./... && echo "<done>"`),
	})
	out, err := journal.Marshal(ev)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	// Spelled with fmt rather than as literals, so this file does not have to
	// contain the escape sequences it is asserting the absence of.
	for _, r := range []rune{'&', '<', '>'} {
		escape := fmt.Sprintf("\\u%04x", r)
		if strings.Contains(string(out), escape) {
			t.Errorf("Marshal emitted the escape %s for %q: %s", escape, r, out)
		}
	}
	if !strings.Contains(string(out), `go build ./... && echo`) {
		t.Errorf("the command is not readable in the record: %s", out)
	}
	var got journal.Event
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if diff := cmp.Diff(ev, got); diff != "" {
		t.Errorf("round trip changed the event (-want +got):\n%s", diff)
	}
}

// TestNeverTruncates: no field type in this package bounds what it can hold.
// The blob spill is a separate concern; clipping is not a fallback.
func TestNeverTruncates(t *testing.T) {
	huge := strings.Repeat("diagnostic output that justifies the fix\n", 4000) // ~160 KiB
	ev := seed(1, journal.ToolResult{
		CallID: "call_1",
		Tool:   "run_shell",
		Output: journal.InlineText(huge),
	})
	out, err := journal.Marshal(ev)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var got journal.Event
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	result, ok := got.Payload.(journal.ToolResult)
	if !ok {
		t.Fatalf("payload is %T", got.Payload)
	}
	if result.Output.Inline != huge {
		t.Errorf("output changed length: got %d bytes, want %d", len(result.Output.Inline), len(huge))
	}
}

// TestMarshalJSONMatchesMarshal keeps the two entry points from drifting apart
// in anything but HTML escaping.
func TestMarshalJSONMatchesMarshal(t *testing.T) {
	ev := seed(1, journal.UserMessage{Text: journal.InlineText("plain text")})
	viaMarshal, err := journal.Marshal(ev)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	viaJSON, err := json.Marshal(ev)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	if string(viaMarshal) != string(viaJSON) {
		t.Errorf("Marshal and json.Marshal disagree:\n%s\n%s", viaMarshal, viaJSON)
	}
}

// FuzzEventUnmarshal: decoding a journal line from disk is decoding untrusted
// bytes — a crash-truncated line, a corrupted file, an event from a build that
// does not exist yet. It may error. It may not panic, and whatever it accepts
// it must be able to write back out.
func FuzzEventUnmarshal(f *testing.F) {
	ev := seed(1, journal.UserMessage{Text: journal.InlineText("hello")})
	line, err := journal.Marshal(ev)
	if err != nil {
		f.Fatalf("seeding: %v", err)
	}
	f.Add(string(line))
	f.Add(`{}`)
	f.Add(`{"type":"UserMessage"}`)
	f.Add(`{"schema_version":1,"session_id":"s","seq":1,"turn":0,"type":"Future","ts":"2026-08-11T09:30:00Z","payload":{"a":[1,{"b":null}]}}`)
	f.Add(`{"schema_version":1,"session_id":"s","seq":1,"turn":0,"type":"ToolResult","ts":"2026-08-11T09:30:00Z","payload":{"output":{"inline":"x","size":1}},"future":true}`)
	f.Add(`{"schema_version":1,"session_id":"s","seq":18446744073709551616,"turn":0,"type":"UserMessage","ts":"2026-08-11T09:30:00Z","payload":{}}`)
	f.Add("\x00\xff not json")

	f.Fuzz(func(t *testing.T, in string) {
		var first journal.Event
		if err := json.Unmarshal([]byte(in), &first); err != nil {
			return // rejecting garbage is the correct outcome
		}

		out, err := journal.Marshal(first)
		if err != nil {
			t.Fatalf("accepted on decode but rejected on encode: %v\ninput: %q", err, in)
		}

		var second journal.Event
		if err := json.Unmarshal(out, &second); err != nil {
			t.Fatalf("its own output does not decode: %v\noutput: %s", err, out)
		}
		if diff := cmp.Diff(first, second); diff != "" {
			t.Fatalf("round trip is not stable (-first +second):\n%s\ninput: %q", diff, in)
		}
	})
}
