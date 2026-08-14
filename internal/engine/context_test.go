package engine_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/leejianrong/kopicode/internal/engine"
	"github.com/leejianrong/kopicode/internal/provider"
)

// call is a native tool call as a provider issues one.
func call(id, name, args string) provider.ToolCall {
	return provider.ToolCall{ID: id, Name: name, Arguments: json.RawMessage(args)}
}

// reply is an assistant reply carrying text and native calls.
func reply(content string, calls ...provider.ToolCall) provider.Reply {
	return provider.Reply{Content: content, ToolCalls: calls}
}

// A whole native-route turn, in order, with nothing dropped along the way.
func TestAssembleNativeRoute(t *testing.T) {
	a := engine.NewAssembler("you are kopicode")
	a.AppendUser("fix the failing test")
	a.AppendAssistant(reply("looking", call("call_1", "read_file", `{"path":"a.go"}`)))
	if err := a.AppendToolResult("call_1", "1| package a"); err != nil {
		t.Fatalf("AppendToolResult: %v", err)
	}
	a.AppendAssistant(reply("that is the bug"))

	want := []provider.Message{
		{Role: provider.RoleSystem, Content: "you are kopicode"},
		{Role: provider.RoleUser, Content: "fix the failing test"},
		{
			Role:      provider.RoleAssistant,
			Content:   "looking",
			ToolCalls: []provider.ToolCall{call("call_1", "read_file", `{"path":"a.go"}`)},
		},
		{Role: provider.RoleTool, Content: "1| package a", ToolCallID: "call_1"},
		{Role: provider.RoleAssistant, Content: "that is the bug"},
	}
	if diff := cmp.Diff(want, a.Messages()); diff != "" {
		t.Errorf("assembled messages (-want +got):\n%s", diff)
	}
}

// The reply's reasoning is not fed back: the journal keeps it as a separate
// ThinkingBlock because it is not fed back the same way, and doing so silently
// would be a different harness than the one being measured.
func TestAssembleDropsReasoning(t *testing.T) {
	a := engine.NewAssembler("")
	a.AppendAssistant(provider.Reply{Content: "the answer", Reasoning: "first, consider"})

	got := a.Messages()
	if len(got) != 1 {
		t.Fatalf("got %d messages, want 1: %+v", len(got), got)
	}
	if strings.Contains(got[0].Content, "consider") {
		t.Errorf("reasoning text reached the request: %q", got[0].Content)
	}
}

// An empty system prompt emits no system message rather than an empty one.
func TestAssembleEmptySystemPrompt(t *testing.T) {
	a := engine.NewAssembler("")
	a.AppendUser("hello")

	got := a.Messages()
	want := []provider.Message{{Role: provider.RoleUser, Content: "hello"}}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("assembled messages (-want +got):\n%s", diff)
	}
	if a.SystemPrompt() != "" {
		t.Errorf("SystemPrompt() = %q, want empty", a.SystemPrompt())
	}
}

// A text-route call has no provider-issued id to echo, so its result travels as
// a user turn — and the call itself stays in the assistant text where the model
// wrote it, rather than being re-encoded as wire history that never happened.
func TestAssembleTextRouteResultIsAUserTurn(t *testing.T) {
	const written = "```json\n{\"tool\":\"grep\",\"args\":{\"pattern\":\"x\"}}\n```"
	a := engine.NewAssembler("")
	a.AppendAssistant(reply(written))
	if err := a.AppendToolResult("", "a.go:3: x"); err != nil {
		t.Fatalf("AppendToolResult: %v", err)
	}

	want := []provider.Message{
		{Role: provider.RoleAssistant, Content: written},
		{Role: provider.RoleUser, Content: "a.go:3: x"},
	}
	if diff := cmp.Diff(want, a.Messages()); diff != "" {
		t.Errorf("assembled messages (-want +got):\n%s", diff)
	}
}

// A tool result that cannot be placed is refused. The alternative to these
// errors is a provider 400 several layers away from the mistake.
func TestAppendToolResultFailsClosed(t *testing.T) {
	newAnswered := func(t *testing.T) *engine.Assembler {
		t.Helper()
		a := engine.NewAssembler("")
		a.AppendAssistant(reply("", call("call_1", "read_file", `{}`)))
		if err := a.AppendToolResult("call_1", "ok"); err != nil {
			t.Fatalf("AppendToolResult: %v", err)
		}
		return a
	}

	cases := []struct {
		name   string
		build  func(*testing.T) *engine.Assembler
		callID string
		want   error
	}{
		{
			name:   "no assistant message at all",
			build:  func(*testing.T) *engine.Assembler { return engine.NewAssembler("") },
			callID: "call_1",
			want:   engine.ErrUnknownToolCall,
		},
		{
			name: "id the assistant never issued",
			build: func(*testing.T) *engine.Assembler {
				a := engine.NewAssembler("")
				a.AppendAssistant(reply("", call("call_1", "read_file", `{}`)))
				return a
			},
			callID: "call_2",
			want:   engine.ErrUnknownToolCall,
		},
		{
			name:   "answered twice",
			build:  newAnswered,
			callID: "call_1",
			want:   engine.ErrToolCallAnswered,
		},
		{
			name: "id from a superseded assistant message",
			build: func(t *testing.T) *engine.Assembler {
				t.Helper()
				a := newAnswered(t)
				a.AppendAssistant(reply("", call("call_2", "grep", `{}`)))
				return a
			},
			callID: "call_1",
			want:   engine.ErrUnknownToolCall,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a := tc.build(t)
			before := len(a.Messages())
			err := a.AppendToolResult(tc.callID, "output")
			if !errors.Is(err, tc.want) {
				t.Fatalf("AppendToolResult(%q) error = %v, want %v", tc.callID, err, tc.want)
			}
			if got := len(a.Messages()); got != before {
				t.Errorf("a refused result still reached the request: %d messages, want %d", got, before)
			}
			if !strings.Contains(err.Error(), tc.callID) {
				t.Errorf("error %q does not name the call id %q", err, tc.callID)
			}
		})
	}
}

// Unanswered is what lets the loop hold the invariant this type cannot: a
// provider refuses a request whose assistant message made calls nothing answers.
func TestUnanswered(t *testing.T) {
	a := engine.NewAssembler("")
	if got := a.Unanswered(); len(got) != 0 {
		t.Errorf("Unanswered() on a fresh assembler = %v, want none", got)
	}

	a.AppendAssistant(reply("", call("call_1", "read_file", `{}`), call("call_2", "grep", `{}`)))
	if diff := cmp.Diff([]string{"call_1", "call_2"}, a.Unanswered()); diff != "" {
		t.Errorf("Unanswered() (-want +got):\n%s", diff)
	}

	if err := a.AppendToolResult("call_1", "ok"); err != nil {
		t.Fatalf("AppendToolResult: %v", err)
	}
	if diff := cmp.Diff([]string{"call_2"}, a.Unanswered()); diff != "" {
		t.Errorf("Unanswered() after one result (-want +got):\n%s", diff)
	}

	if err := a.AppendToolResult("call_2", "ok"); err != nil {
		t.Fatalf("AppendToolResult: %v", err)
	}
	if got := a.Unanswered(); len(got) != 0 {
		t.Errorf("Unanswered() after both results = %v, want none", got)
	}
}

// Naive full history is the decision, so a long session carries every message it
// ever produced. This is the property a compaction strategy would break, and it
// must break a test when someone adds one without an ADR.
func TestAssembleKeepsFullHistory(t *testing.T) {
	const turns = 20
	a := engine.NewAssembler("system")
	for i := range turns {
		a.AppendUser(fmt.Sprintf("user %d", i))
		a.AppendAssistant(reply("", call(fmt.Sprintf("call_%d", i), "read_file", `{}`)))
		if err := a.AppendToolResult(fmt.Sprintf("call_%d", i), fmt.Sprintf("result %d", i)); err != nil {
			t.Fatalf("AppendToolResult: %v", err)
		}
	}

	got := a.Messages()
	if want := 1 + turns*3; len(got) != want {
		t.Fatalf("got %d messages after %d turns, want %d — history is being dropped", len(got), turns, want)
	}
	if got[1].Content != "user 0" {
		t.Errorf("oldest turn is %q, want %q — the window is sliding", got[1].Content, "user 0")
	}
}

// Never truncate. CLAUDE.md's rule is usually read as being about the journal;
// an assembler that clipped a large tool result to save tokens would be the same
// boundary crossed from the inside, and the model would lose the diagnostic
// output that justifies the fix.
func TestAssemblerNeverTruncates(t *testing.T) {
	huge := strings.Repeat("diagnostic output that justifies the fix\n", 30_000) // ~1.2 MiB
	if len(huge) < 1<<20 {
		t.Fatalf("fixture is only %d bytes; it must exceed any plausible clip point", len(huge))
	}

	a := engine.NewAssembler("")
	a.AppendAssistant(reply("", call("call_1", "run_shell", `{}`)))
	if err := a.AppendToolResult("call_1", huge); err != nil {
		t.Fatalf("AppendToolResult: %v", err)
	}
	a.AppendUser(huge)

	for i, m := range a.Messages() {
		if m.Role == provider.RoleAssistant {
			continue
		}
		if m.Content != huge {
			t.Fatalf("message %d (role %s) was altered: %d bytes reached the request, want %d",
				i, m.Role, len(m.Content), len(huge))
		}
	}
}

// A replayed session must produce a byte-identical journal, so the assembled
// request is a pure function of what was appended: same appends, same bytes,
// every time and in any order of calls.
func TestAssembleIsDeterministic(t *testing.T) {
	build := func() *engine.Assembler {
		a := engine.NewAssembler("system")
		a.AppendUser("go")
		a.AppendAssistant(reply("working", call("call_1", "read_file", `{"path":"a.go"}`), call("call_2", "grep", `{"pattern":"x"}`)))
		if err := a.AppendToolResult("call_2", "second result first"); err != nil {
			t.Fatalf("AppendToolResult: %v", err)
		}
		if err := a.AppendToolResult("call_1", "first result second"); err != nil {
			t.Fatalf("AppendToolResult: %v", err)
		}
		return a
	}

	encode := func(a *engine.Assembler) string {
		t.Helper()
		b, err := json.Marshal(a.Messages())
		if err != nil {
			t.Fatalf("marshalling assembled messages: %v", err)
		}
		return string(b)
	}

	first := encode(build())
	for i := range 8 {
		if got := encode(build()); got != first {
			t.Fatalf("assembly %d differs from the first:\n%s\n%s", i, first, got)
		}
	}

	a := build()
	if got := encode(a); got != encode(a) {
		t.Error("two Messages() calls on one assembler differ")
	}
}

// The returned slice is the caller's to keep, not the assembler's to lose: a
// loop holding on to the previous request must not be able to rewrite this one.
func TestMessagesIsACopy(t *testing.T) {
	a := engine.NewAssembler("system")
	a.AppendAssistant(reply("text", call("call_1", "read_file", `{}`)))

	got := a.Messages()
	got[0].Content = "clobbered"
	got[1].ToolCalls[0].Name = "clobbered"

	fresh := a.Messages()
	if fresh[0].Content != "system" {
		t.Errorf("system message = %q, want %q", fresh[0].Content, "system")
	}
	if fresh[1].ToolCalls[0].Name != "read_file" {
		t.Errorf("tool call name = %q, want %q", fresh[1].ToolCalls[0].Name, "read_file")
	}
}

// Size counts exactly what the request carries, so growth can be watched before
// a request is sent rather than reconstructed after.
func TestSize(t *testing.T) {
	a := engine.NewAssembler("sys") // 3
	a.AppendUser("user")            // 4
	a.AppendAssistant(reply("assistant", call("call_1", "read_file", `{"path":"a.go"}`)))
	// 9 + len("call_1")=6 + len("read_file")=9 + len(`{"path":"a.go"}`)=15
	if err := a.AppendToolResult("call_1", "out"); err != nil { // 3 + 6
		t.Fatalf("AppendToolResult: %v", err)
	}

	want := engine.Size{Messages: 4, Bytes: 3 + 4 + 9 + 6 + 9 + 15 + 3 + 6}
	if diff := cmp.Diff(want, a.Size()); diff != "" {
		t.Errorf("Size() (-want +got):\n%s", diff)
	}
	wantTokens := want.Bytes / engine.BytesPerTokenEstimate
	if got := a.Size().EstimatedTokens(); got != wantTokens {
		t.Errorf("EstimatedTokens() = %d, want %d", got, wantTokens)
	}

	// Size describes the request Messages() returns, not some other one.
	if got := len(a.Messages()); got != a.Size().Messages {
		t.Errorf("Size().Messages = %d, but Messages() returned %d", a.Size().Messages, got)
	}
}

// Every field of provider.Message has to be filled by something the assembler
// produces, or it is a field the assembler silently never sets — a new one
// added to the wire format is exactly the kind of omission a hand-written
// expectation table would sail past. Derived by reflection rather than listed,
// with a positive control below so a broken traversal cannot pass by asserting
// nothing.
func TestAssemblerCoversEveryMessageField(t *testing.T) {
	a := engine.NewAssembler("system prompt")
	a.AppendUser("user text")
	a.AppendAssistant(reply("assistant text", call("call_1", "read_file", `{"path":"a.go"}`)))
	if err := a.AppendToolResult("call_1", "tool output"); err != nil {
		t.Fatalf("AppendToolResult: %v", err)
	}

	if missing := unsetFields(a.Messages()); len(missing) != 0 {
		t.Errorf("no assembled message ever sets provider.Message field(s) %v; "+
			"either the assembler must fill them or this test must say why not", missing)
	}
}

// The positive control for the check above: a message set that genuinely leaves
// fields unset must be reported, so a traversal that silently visits nothing
// fails here instead of passing there.
func TestUnsetFieldsControl(t *testing.T) {
	missing := unsetFields([]provider.Message{{Role: provider.RoleUser, Content: "only text"}})
	want := []string{"ToolCalls", "ToolCallID"}
	if diff := cmp.Diff(want, missing); diff != "" {
		t.Errorf("unsetFields on a text-only message (-want +got):\n%s", diff)
	}
}

// unsetFields names the fields of provider.Message that no message in msgs sets
// to a non-zero value, in declaration order.
func unsetFields(msgs []provider.Message) []string {
	typ := reflect.TypeFor[provider.Message]()
	var missing []string
	for i := range typ.NumField() {
		name := typ.Field(i).Name
		set := false
		for _, m := range msgs {
			if !reflect.ValueOf(m).Field(i).IsZero() {
				set = true
				break
			}
		}
		if !set {
			missing = append(missing, name)
		}
	}
	return missing
}
