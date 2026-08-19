package engine_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/leejianrong/kopicode/internal/engine"
	"github.com/leejianrong/kopicode/internal/journal"
	"github.com/leejianrong/kopicode/internal/parse"
	"github.com/leejianrong/kopicode/internal/provider"
)

// This file is KAN-939's test half: replayHistory's mapping from journal
// events onto Assembler calls, held to account event kind by event kind
// (TestReplayHistory*), and the whole resume path driven through engine.Open
// twice in sequence (TestResumeCarriesFullHistoryIntoTheNextRequest and
// neighbours) — the end-to-end evidence that a second process actually sees
// what the first one wrote.
//
// No git here, for the same reason open_test.go gives: internal/repo owns
// the snapshotter's own attach behaviour and has the fixtures to test it in
// isolation (internal/repo/snapshot_test.go). What this file is about is the
// journal-to-assembler replay and the engine.Open wiring around it.

// msgs is a shorthand for building an expected []provider.Message.
func msgs(m ...provider.Message) []provider.Message { return m }

func user(text string) provider.Message {
	return provider.Message{Role: provider.RoleUser, Content: text}
}

func assistant(text string, calls ...provider.ToolCall) provider.Message {
	return provider.Message{Role: provider.RoleAssistant, Content: text, ToolCalls: calls}
}

func toolMsg(callID, output string) provider.Message {
	return provider.Message{Role: provider.RoleTool, Content: output, ToolCallID: callID}
}

// replay is the test's own tiny driver: a fresh, no-system-prompt Assembler,
// replayHistory over evs, and the resulting Messages — or a fatal failure,
// since every case in this file expects the replay to succeed.
func replay(t *testing.T, evs []journal.Event) []provider.Message {
	t.Helper()
	asm := engine.NewAssembler("")
	if err := engine.ReplayHistoryForTest(asm, evs); err != nil {
		t.Fatalf("ReplayHistoryForTest: %v", err)
	}
	return asm.Messages()
}

// ev builds one journal.Event carrying payload, with a seq the test does not
// otherwise care about — replayHistory reads events in the order given, not
// by consulting Seq itself, so a fixed placeholder is enough to satisfy the
// type without claiming an ordering the slice literal already states.
func ev(payload journal.Payload) journal.Event {
	return journal.Event{SchemaVersion: journal.SchemaVersion, Payload: payload}
}

func TestReplayHistoryUserAndAssistantProse(t *testing.T) {
	got := replay(t, []journal.Event{
		ev(journal.UserMessage{Text: journal.InlineText("hello")}),
		ev(journal.ProviderResponse{}),
		ev(journal.AssistantMessage{Text: journal.InlineText("hi there")}),
	})
	want := msgs(user("hello"), assistant("hi there"))
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("Messages() (-want +got):\n%s", diff)
	}
}

// TestReplayHistoryDropsThinkingBlocks holds Assembler.AppendAssistant's own
// contract — reasoning is never fed back — across the resume boundary too.
func TestReplayHistoryDropsThinkingBlocks(t *testing.T) {
	got := replay(t, []journal.Event{
		ev(journal.UserMessage{Text: journal.InlineText("why?")}),
		ev(journal.ProviderResponse{}),
		ev(journal.ThinkingBlock{Text: journal.InlineText("let me think...")}),
		ev(journal.AssistantMessage{Text: journal.InlineText("because")}),
	})
	want := msgs(user("why?"), assistant("because"))
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("Messages() (-want +got):\n%s", diff)
	}
	for _, m := range got {
		if strings.Contains(m.Content, "let me think") {
			t.Errorf("a ThinkingBlock reached the replayed history: %+v", m)
		}
	}
}

// TestReplayHistoryNativeToolCallAndResult is the dominant real case: a
// dispatched native call answers with a tool-role message echoing the
// provider's own call id, exactly as Engine.dispatch produced it the first
// time.
func TestReplayHistoryNativeToolCallAndResult(t *testing.T) {
	got := replay(t, []journal.Event{
		ev(journal.UserMessage{Text: journal.InlineText("read it")}),
		ev(journal.ProviderResponse{}),
		ev(journal.ToolCallRequested{CallID: "call-1", Raw: journal.InlineText(`{"path":"a.txt"}`)}),
		ev(journal.ToolCallParsed{
			CallID: "call-1", Tool: "read_file", Args: json.RawMessage(`{"path":"a.txt"}`),
			Route: parse.RouteNative.String(), ArgEncoding: "object",
		}),
		ev(journal.ToolResult{CallID: "call-1", Tool: "read_file", Output: journal.InlineText("file contents")}),
		ev(journal.ProviderResponse{}),
		ev(journal.AssistantMessage{Text: journal.InlineText("done")}),
	})

	want := msgs(
		user("read it"),
		assistant("", provider.ToolCall{ID: "call-1", Name: "read_file", Arguments: json.RawMessage(`{"path":"a.txt"}`)}),
		toolMsg("call-1", "file contents"),
		assistant("done"),
	)
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("Messages() (-want +got):\n%s", diff)
	}
}

// TestReplayHistoryMultipleNativeCallsInOneReply is the case that motivates
// buffering a whole attempt before committing it: dispatch journals each
// call's Parsed/Result pair before moving to the next, so the two
// ToolCallParsed events for this one reply are not adjacent on the wire, and
// the replayed AppendAssistant call still has to carry both ids in its one
// ToolCalls array.
func TestReplayHistoryMultipleNativeCallsInOneReply(t *testing.T) {
	got := replay(t, []journal.Event{
		ev(journal.UserMessage{Text: journal.InlineText("read both")}),
		ev(journal.ProviderResponse{}),
		ev(journal.ToolCallRequested{CallID: "call-1", Raw: journal.InlineText("")}),
		ev(journal.ToolCallParsed{CallID: "call-1", Tool: "read_file",
			Args: json.RawMessage(`{"path":"a.txt"}`), Route: parse.RouteNative.String()}),
		ev(journal.ToolResult{CallID: "call-1", Tool: "read_file", Output: journal.InlineText("a")}),
		ev(journal.ToolCallRequested{CallID: "call-2", Raw: journal.InlineText("")}),
		ev(journal.ToolCallParsed{CallID: "call-2", Tool: "read_file",
			Args: json.RawMessage(`{"path":"b.txt"}`), Route: parse.RouteNative.String()}),
		ev(journal.ToolResult{CallID: "call-2", Tool: "read_file", Output: journal.InlineText("b")}),
	})

	want := msgs(
		user("read both"),
		assistant("",
			provider.ToolCall{ID: "call-1", Name: "read_file", Arguments: json.RawMessage(`{"path":"a.txt"}`)},
			provider.ToolCall{ID: "call-2", Name: "read_file", Arguments: json.RawMessage(`{"path":"b.txt"}`)},
		),
		toolMsg("call-1", "a"),
		toolMsg("call-2", "b"),
	)
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("Messages() (-want +got):\n%s", diff)
	}
}

// TestReplayHistoryTextRouteToolCallAndResult is the fenced/XML case: the
// wire never issued an id for this call, so its result travels back as a
// plain user turn, matching what Assembler.AppendToolResult("", output) —
// which is what the original dispatch call made — actually did.
func TestReplayHistoryTextRouteToolCallAndResult(t *testing.T) {
	raw := "```tool\n{\"tool\":\"read_file\",\"arguments\":{\"path\":\"a.txt\"}}\n```"
	got := replay(t, []journal.Event{
		ev(journal.UserMessage{Text: journal.InlineText("read it")}),
		ev(journal.ProviderResponse{}),
		ev(journal.AssistantMessage{Text: journal.InlineText(raw)}),
		ev(journal.ToolCallRequested{CallID: "kc-1-1", Raw: journal.InlineText(raw)}),
		ev(journal.ToolCallParsed{
			CallID: "kc-1-1", Tool: "read_file", Args: json.RawMessage(`{"path":"a.txt"}`),
			Route: parse.RouteFencedJSON.String(),
		}),
		ev(journal.ToolResult{CallID: "kc-1-1", Tool: "read_file", Output: journal.InlineText("file contents")}),
	})

	want := msgs(
		user("read it"),
		assistant(raw), // no ToolCalls: the wire issued none for a text-route call
		user("file contents"),
	)
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("Messages() (-want +got):\n%s", diff)
	}
}

// TestReplayHistoryRepairedFeedbackBecomesUserTurn covers the repair path: no
// ToolCallParsed exists for this attempt (the whole point of a repair), so
// the replayed reply carries no pending native calls and the feedback lands
// as a user turn — matching Engine.observe when Unanswered() is empty.
func TestReplayHistoryRepairedFeedbackBecomesUserTurn(t *testing.T) {
	got := replay(t, []journal.Event{
		ev(journal.UserMessage{Text: journal.InlineText("fix it")}),
		ev(journal.ProviderResponse{}),
		ev(journal.AssistantMessage{Text: journal.InlineText("```tool\nnot quite json\n```")}),
		ev(journal.ToolCallRequested{CallID: "kc-1", Raw: journal.InlineText("```tool\nnot quite json\n```")}),
		ev(journal.ToolCallRepaired{CallID: "kc-1", Attempt: 1, Classification: "unparseable",
			Error: journal.InlineText("that did not parse as a tool call; try again")}),
		ev(journal.ProviderResponse{}),
		ev(journal.AssistantMessage{Text: journal.InlineText("sorted")}),
	})

	want := msgs(
		user("fix it"),
		assistant("```tool\nnot quite json\n```"),
		user("that did not parse as a tool call; try again"),
		assistant("sorted"),
	)
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("Messages() (-want +got):\n%s", diff)
	}
}

// TestReplayHistoryFailedFeedbackBecomesUserTurn is TestReplayHistoryRepairedFeedbackBecomesUserTurn's
// sibling once the repair budget is spent.
func TestReplayHistoryFailedFeedbackBecomesUserTurn(t *testing.T) {
	got := replay(t, []journal.Event{
		ev(journal.UserMessage{Text: journal.InlineText("fix it")}),
		ev(journal.ProviderResponse{}),
		ev(journal.AssistantMessage{Text: journal.InlineText("nonsense")}),
		ev(journal.ToolCallRequested{CallID: "kc-1", Raw: journal.InlineText("nonsense")}),
		ev(journal.ToolCallFailed{CallID: "kc-1", Reason: "unparseable",
			Detail: journal.InlineText("giving up; the call never parsed")}),
	})

	want := msgs(
		user("fix it"),
		assistant("nonsense"),
		user("giving up; the call never parsed"),
	)
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("Messages() (-want +got):\n%s", diff)
	}
}

// TestReplayHistoryBlockingVerificationBecomesUserTurn is the verification
// half: a failing forced-verification run's output is fed back as a user
// turn after a turn that dispatched and answered every call, matching
// Engine.verify's own call into Engine.observe.
func TestReplayHistoryBlockingVerificationBecomesUserTurn(t *testing.T) {
	got := replay(t, []journal.Event{
		ev(journal.UserMessage{Text: journal.InlineText("fix the build")}),
		ev(journal.ProviderResponse{}),
		ev(journal.ToolCallParsed{CallID: "call-1", Tool: "write_file",
			Args: json.RawMessage(`{"path":"a.go","content":"broken"}`), Route: parse.RouteNative.String()}),
		ev(journal.ToolResult{CallID: "call-1", Tool: "write_file", Output: journal.InlineText("wrote a.go")}),
		ev(journal.TurnSnapshot{Ref: "refs/kopicode/s/1", Commit: "deadbeef"}),
		ev(journal.VerificationRun{Command: []string{"go", "build", "./..."}, Source: "discovered",
			ExitCode: 2, Output: journal.InlineText("a.go:1: syntax error")}),
	})

	want := msgs(
		user("fix the build"),
		assistant("", provider.ToolCall{ID: "call-1", Name: "write_file",
			Arguments: json.RawMessage(`{"path":"a.go","content":"broken"}`)}),
		toolMsg("call-1", "wrote a.go"),
		user("a.go:1: syntax error"),
	)
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("Messages() (-want +got):\n%s", diff)
	}
}

// TestReplayHistoryPassingVerificationAddsNothing is the other half: a
// verification that ran and passed answers an earlier rejection but is never
// itself fed back — Engine.verify only calls observe on a blocking result.
func TestReplayHistoryPassingVerificationAddsNothing(t *testing.T) {
	got := replay(t, []journal.Event{
		ev(journal.UserMessage{Text: journal.InlineText("fix the build")}),
		ev(journal.ProviderResponse{}),
		ev(journal.ToolCallParsed{CallID: "call-1", Tool: "write_file",
			Args: json.RawMessage(`{"path":"a.go","content":"fine"}`), Route: parse.RouteNative.String()}),
		ev(journal.ToolResult{CallID: "call-1", Tool: "write_file", Output: journal.InlineText("wrote a.go")}),
		ev(journal.VerificationRun{Command: []string{"go", "build", "./..."}, Source: "discovered",
			ExitCode: 0, Output: journal.InlineText("ok")}),
	})

	want := msgs(
		user("fix the build"),
		assistant("", provider.ToolCall{ID: "call-1", Name: "write_file",
			Arguments: json.RawMessage(`{"path":"a.go","content":"fine"}`)}),
		toolMsg("call-1", "wrote a.go"),
	)
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("Messages() (-want +got):\n%s", diff)
	}
}

// TestReplayHistoryNotRunVerificationAddsNothing holds the same line for a
// verification that never concluded (Skip set, ExitCode -1): it is not
// evidence in either direction, so nothing is fed back — matching
// verify.Result.Blocks()'s "Outcome == Failed" exactly.
func TestReplayHistoryNotRunVerificationAddsNothing(t *testing.T) {
	got := replay(t, []journal.Event{
		ev(journal.UserMessage{Text: journal.InlineText("fix it")}),
		ev(journal.ProviderResponse{}),
		ev(journal.ToolCallParsed{CallID: "call-1", Tool: "write_file",
			Args: json.RawMessage(`{}`), Route: parse.RouteNative.String()}),
		ev(journal.ToolResult{CallID: "call-1", Tool: "write_file", Output: journal.InlineText("wrote")}),
		ev(journal.VerificationRun{Source: "none", ExitCode: -1, Skip: "no_command"}),
	})

	want := msgs(
		user("fix it"),
		assistant("", provider.ToolCall{ID: "call-1", Name: "write_file", Arguments: json.RawMessage(`{}`)}),
		toolMsg("call-1", "wrote"),
	)
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("Messages() (-want +got):\n%s", diff)
	}
}

// TestReplayHistoryAcrossTwoExchanges is the multi-Run() case: a session
// resumed after its first Session.Run call finished cleanly and a second one
// started, which is the ordinary REPL shape (one Run per prompt, one journal
// across the whole session).
func TestReplayHistoryAcrossTwoExchanges(t *testing.T) {
	got := replay(t, []journal.Event{
		ev(journal.UserMessage{Text: journal.InlineText("first")}),
		ev(journal.ProviderResponse{}),
		ev(journal.AssistantMessage{Text: journal.InlineText("first reply")}),
		ev(journal.SessionEnded{Reason: "completed"}),
		// A resume's own SessionStarted, then the second exchange.
		ev(journal.SessionStarted{}),
		ev(journal.UserMessage{Text: journal.InlineText("second")}),
		ev(journal.ProviderResponse{}),
		ev(journal.AssistantMessage{Text: journal.InlineText("second reply")}),
	})

	want := msgs(
		user("first"), assistant("first reply"),
		user("second"), assistant("second reply"),
	)
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("Messages() (-want +got):\n%s", diff)
	}
}

// --- engine.Open, twice, in sequence -----------------------------------

// fakeReplyProvider serves one canned assistant reply per call, and remembers
// every request it was handed — including the Messages a resumed session's
// history reconstruction put on the wire, which is the fact this file's
// headline test is about.
type fakeReplyProvider struct {
	mu       sync.Mutex
	bodies   [][]byte
	i        int
	requests []provider.Request
}

func (p *fakeReplyProvider) Complete(ctx context.Context, req provider.Request) (*provider.Stream, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.requests = append(p.requests, req)
	if p.i >= len(p.bodies) {
		return nil, fmt.Errorf("fakeReplyProvider: no reply left for request %d", p.i)
	}
	body := p.bodies[p.i]
	p.i++
	return provider.NewBodyStream(ctx, body), nil
}

// proseBody is a one-shot, non-streamed reply body carrying only assistant
// prose — a clean stop, so the turn ends without touching any tool.
func proseBody(t *testing.T, text string) []byte {
	t.Helper()
	body := wireBody{
		ID: "gen-1", Object: "chat.completion", Model: testModelID, Provider: testServedBy,
		Choices: []wireChoice{{Message: wireMessage{Role: "assistant", Content: &text}, FinishReason: "stop"}},
		Usage:   wireUsage{Prompt: 5, Completion: 3, Total: 8},
	}
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshalling a fake reply body: %v", err)
	}
	return raw
}

// resumeSelection is the arm both halves of a resume test run under: the
// registered default, which is enough here since fakeReplyProvider never
// checks the model id or the pin the way the mock/fixture provider does.
func resumeSelection(t *testing.T) engine.Selection {
	t.Helper()
	sel, err := engine.ResolveSelection(t.TempDir(), engine.SelectionOverrides{})
	if err != nil {
		t.Fatalf("resolving the default selection: %v", err)
	}
	return sel
}

// TestResumeCarriesFullHistoryIntoTheNextRequest is the card's end-to-end
// proof: a session opened, run once and closed; the same session id opened
// again with Options.Resume, run a second time; and the *second* process's
// first provider request carries the *first* process's whole exchange,
// because nothing but the journal on disk told it to.
func TestResumeCarriesFullHistoryIntoTheNextRequest(t *testing.T) {
	dir := t.TempDir()
	sel := resumeSelection(t)
	ctx := context.Background()

	first := &fakeReplyProvider{bodies: [][]byte{proseBody(t, "Hi, ready to help.")}}
	s1, err := engine.Open(ctx, engine.Options{
		Dir: dir, Selection: sel, SessionID: "resume-e2e", Provider: first, Now: fixedClock(),
	})
	if err != nil {
		t.Fatalf("first Open: %v", err)
	}
	if _, err := s1.Run(ctx, "hello"); err != nil {
		t.Fatalf("first Run: %v", err)
	}
	if err := s1.Close(ctx); err != nil {
		t.Fatalf("first Close: %v", err)
	}

	second := &fakeReplyProvider{bodies: [][]byte{proseBody(t, "You said hello.")}}
	s2, err := engine.Open(ctx, engine.Options{
		Dir: dir, Selection: sel, SessionID: "resume-e2e", Resume: true, Provider: second, Now: fixedClock(),
	})
	if err != nil {
		t.Fatalf("resuming Open: %v", err)
	}
	defer func() { _ = s2.Close(ctx) }()

	res, err := s2.Run(ctx, "what did I just say?")
	if err != nil {
		t.Fatalf("resumed Run: %v", err)
	}
	if res.Stop != engine.StopCompleted {
		t.Errorf("stop = %s, want %s", res.Stop, engine.StopCompleted)
	}

	if len(second.requests) == 0 {
		t.Fatal("the resumed session never called its provider")
	}
	sent := second.requests[0].Messages

	var texts []string
	for _, m := range sent {
		texts = append(texts, string(m.Role)+":"+m.Content)
	}
	joined := strings.Join(texts, "\n")

	for _, want := range []string{"hello", "Hi, ready to help.", "what did I just say?"} {
		if !strings.Contains(joined, want) {
			t.Errorf("the resumed session's first request does not carry %q; messages sent:\n%s", want, joined)
		}
	}

	// And in order: the interrupted exchange's user turn, its reply, then the
	// new prompt — never the other way around, and never duplicated.
	hi := indexOfContent(sent, "hello")
	reply := indexOfContent(sent, "Hi, ready to help.")
	next := indexOfContent(sent, "what did I just say?")
	if hi < 0 || reply < 0 || next < 0 {
		t.Fatalf("could not locate all three messages in %v", sent)
	}
	if hi >= reply || reply >= next {
		t.Errorf("messages are out of order: hello@%d, reply@%d, new prompt@%d", hi, reply, next)
	}
}

// listSessions is what TestResumeRequiresAnExistingSession checks a refused
// resume left behind: nothing, because ADR-0007 decision 4's ordering — no
// half-session record for a session that never started — applies to this
// refusal exactly as it does to an unknown model.
func listSessions(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(dir, journal.StateDir, journal.SessionsSubdir))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatalf("listing session records under %s: %v", dir, err)
	}
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		out = append(out, e.Name())
	}
	return out
}

func indexOfContent(msgs []provider.Message, substr string) int {
	for i, m := range msgs {
		if strings.Contains(m.Content, substr) {
			return i
		}
	}
	return -1
}

// TestOpenWithoutResumeStartsWithNoHistory holds the other half of the
// card's requirement: a caller that did not set Options.Resume gets an
// assembler with no prior history, even when the journal on disk (because
// SessionID happens to collide, or because a caller reused one on purpose for
// some other reason) already holds some. Resume has to be asked for.
func TestOpenWithoutResumeStartsWithNoHistory(t *testing.T) {
	dir := t.TempDir()
	sel := resumeSelection(t)
	ctx := context.Background()

	first := &fakeReplyProvider{bodies: [][]byte{proseBody(t, "first reply")}}
	s1, err := engine.Open(ctx, engine.Options{
		Dir: dir, Selection: sel, SessionID: "no-resume", Provider: first, Now: fixedClock(),
	})
	if err != nil {
		t.Fatalf("first Open: %v", err)
	}
	if _, err := s1.Run(ctx, "first prompt"); err != nil {
		t.Fatalf("first Run: %v", err)
	}
	if err := s1.Close(ctx); err != nil {
		t.Fatalf("first Close: %v", err)
	}

	second := &fakeReplyProvider{bodies: [][]byte{proseBody(t, "second reply")}}
	s2, err := engine.Open(ctx, engine.Options{
		// Same directory, same id, Resume left false: the journal-level machinery
		// still resumes the file (that is unconditional and predates this card),
		// but nothing here asked for the history to be replayed into the loop.
		Dir: dir, Selection: sel, SessionID: "no-resume", Provider: second, Now: fixedClock(),
	})
	if err != nil {
		t.Fatalf("second Open (no Resume): %v", err)
	}
	defer func() { _ = s2.Close(ctx) }()

	if _, err := s2.Run(ctx, "second prompt"); err != nil {
		t.Fatalf("second Run: %v", err)
	}
	if len(second.requests) == 0 {
		t.Fatal("the second session never called its provider")
	}
	sent := second.requests[0].Messages
	if indexOfContent(sent, "first prompt") >= 0 || indexOfContent(sent, "first reply") >= 0 {
		t.Errorf("a session opened without Resume carried the earlier session's history: %v", sent)
	}
}

// TestResumeRequiresAnExistingSession holds Options.Resume to its documented
// contract: asking to resume a session that never ran is a configuration
// error, not a quiet fresh start under the id the caller expected to
// continue.
func TestResumeRequiresAnExistingSession(t *testing.T) {
	dir := t.TempDir()
	_, err := engine.Open(context.Background(), engine.Options{
		Dir: dir, Selection: resumeSelection(t), SessionID: "never-existed", Resume: true,
		Provider: &fakeReplyProvider{}, Now: fixedClock(),
	})
	if !errors.Is(err, engine.ErrConfig) {
		t.Fatalf("Open with Resume on a nonexistent session = %v, want ErrConfig", err)
	}

	// And nothing was created for the refusal to leave behind.
	entries := listSessions(t, dir)
	if len(entries) != 0 {
		t.Errorf("a refused resume created session record(s): %v", entries)
	}
}

// TestResumeRequiresANonEmptySessionID: resuming "mint one for me" is not a
// request Open can satisfy.
func TestResumeRequiresANonEmptySessionID(t *testing.T) {
	_, err := engine.Open(context.Background(), engine.Options{
		Dir: t.TempDir(), Selection: resumeSelection(t), Resume: true,
		Provider: &fakeReplyProvider{}, Now: fixedClock(),
	})
	if !errors.Is(err, engine.ErrConfig) {
		t.Fatalf("Open with Resume and no SessionID = %v, want ErrConfig", err)
	}
}
