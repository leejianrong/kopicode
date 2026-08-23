package engine_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/leejianrong/kopicode/internal/journal"
	"github.com/leejianrong/kopicode/internal/permission"
	"github.com/leejianrong/kopicode/internal/provider"
	"github.com/leejianrong/kopicode/internal/tools"
)

// This file is ADR-0012 decision 1 (KAN-989): collapsing a run of
// byte-identical, already-denied tool calls into a short note on the wire,
// without touching what the journal records. It is driven exactly the way
// verification_test.go drives the "what does the model see next" question —
// recordingProvider wrapping the script provider, so a test can inspect
// provider.Request.Messages for the request that carries one call's answer,
// rather than trusting the loop's internal state.

// theDenialReason is the exact reason permission.UnattendedDenyPolicy gives
// for a run_shell request — the actual text KAN-988's dogfood session saw
// repeated four times over before this card. Using the real production text,
// rather than a shorter stand-in, is what makes "the collapsed note is
// shorter than the reason it replaces" a meaningful assertion instead of an
// artifact of a test picking a conveniently short fixture string.
const theDenialReason = "shell commands are never approved in unattended mode (no sandbox exists yet to " +
	"contain them) — this will not be approved on retry; use the file tools instead"

// repeatedShellCall builds n turns each issuing the identical run_shell call
// against command, one call per turn with a fresh call id — a model retrying
// the exact call it was just told no to keeps issuing a new call id each
// time, which is what the real dogfood session did — followed by one final
// turn that stops with prose.
func repeatedShellCall(command string, n int) []scriptedReply {
	replies := make([]scriptedReply, 0, n+1)
	for i := 0; i < n; i++ {
		replies = append(replies, scriptedReply{
			calls: []wireCall{nativeCall(fmt.Sprintf("call-%d", i+1), tools.ToolRunShell,
				fmt.Sprintf(`{"command":%q}`, command))},
		})
	}
	replies = append(replies, scriptedReply{text: "Understood, giving up on that."})
	return replies
}

// toolMessage returns the content of req's tool-result message answering
// callID, failing the test if there is none.
func toolMessage(t *testing.T, req provider.Request, callID string) string {
	t.Helper()
	for _, m := range req.Messages {
		if m.Role == provider.RoleTool && m.ToolCallID == callID {
			return m.Content
		}
	}
	t.Fatalf("request (turn %d attempt %d) carries no tool-result message for call %q",
		req.Turn, req.Attempt, callID)
	return ""
}

// TestRepeatedIdenticalDenialsCollapseOnTheWireButNotInTheJournal is the
// card's central proof. Four turns each retry the exact run_shell call
// KAN-988's dogfood run saw denied identically; the fifth turn stops. The
// journal must hold the full reason for all four denials — collapsing is a
// wire-only concern — but only the first call's tool-result message reaching
// the model carries that full text; the following three get a short note.
func TestRepeatedIdenticalDenialsCollapseOnTheWireButNotInTheJournal(t *testing.T) {
	const n = 4
	replies := repeatedShellCall("rm test_bug.js", n)
	p := script(t, replies, oneAttemptPerTurn(n+1))
	spy := &recordingProvider{inner: p}
	h := newHarness(t, p, nil, withMaxTurns(n+1), withProvider(spy))
	h.policy.verdict = permission.VerdictDeny
	h.policy.reason = theDenialReason

	if _, err := h.eng.Run(t.Context(), "clean up the scratch file"); err != nil {
		t.Fatalf("Run: %v", err)
	}

	evs := h.events()

	// (d) The journal records every single call in full, collapsed or not:
	// one PermissionRequested, one PermissionDecided and one ToolResult per
	// call, and every ToolResult still carries the complete reason.
	results := payloadsOf[journal.ToolResult](t, evs)
	if len(results) != n {
		t.Fatalf("got %d ToolResult events, want %d — the journal must record every call",
			len(results), n)
	}
	if got := len(payloadsOf[journal.PermissionRequested](t, evs)); got != n {
		t.Errorf("got %d PermissionRequested events, want %d", got, n)
	}
	if got := len(payloadsOf[journal.PermissionDecided](t, evs)); got != n {
		t.Errorf("got %d PermissionDecided events, want %d", got, n)
	}
	for _, r := range results {
		if !strings.Contains(r.Output.Inline, theDenialReason) {
			t.Errorf("ToolResult for call %q lost the full denial reason from the journal: %q",
				r.CallID, r.Output.Inline)
		}
	}

	// (a) and (b): what reached the model. Call i's answer is carried by the
	// request for turn i+1, i.e. spy.requests[i] (spy.requests[0] is turn 1's
	// own request, with no tool results yet).
	if len(spy.requests) < n+1 {
		t.Fatalf("the loop sent %d requests, want at least %d", len(spy.requests), n+1)
	}
	var fullLen int
	for i := 1; i <= n; i++ {
		callID := fmt.Sprintf("call-%d", i)
		content := toolMessage(t, spy.requests[i], callID)
		if i == 1 {
			if !strings.Contains(content, theDenialReason) {
				t.Errorf("the first denial's tool-result content lost the full reason: %q", content)
			}
			fullLen = len(content)
			continue
		}
		if strings.Contains(content, theDenialReason) {
			t.Errorf("call %q's collapsed tool-result content repeats the full reason verbatim: %q",
				callID, content)
		}
		if content == "" {
			t.Errorf("call %q's collapsed tool-result content is empty", callID)
		}
		if len(content) >= fullLen {
			t.Errorf("call %q's collapsed note (%d bytes) is not shorter than the first call's full denial "+
				"text (%d bytes) it replaces: %q", callID, len(content), fullLen, content)
		}
	}
}

// TestADifferentCallInBetweenResetsTheDenialStreak is (c): a denied call is
// only ever collapsed against the call *immediately* preceding it. A
// different, unrelated call in between — even one that is itself denied —
// must reset the streak, so the repeat of the first call afterwards gets the
// full reason again, not a collapsed note.
func TestADifferentCallInBetweenResetsTheDenialStreak(t *testing.T) {
	replies := []scriptedReply{
		{calls: []wireCall{nativeCall("call-1", tools.ToolRunShell, `{"command":"rm test_bug.js"}`)}},
		{calls: []wireCall{nativeCall("call-2", tools.ToolRunShell, `{"command":"echo unrelated"}`)}},
		{calls: []wireCall{nativeCall("call-3", tools.ToolRunShell, `{"command":"rm test_bug.js"}`)}},
		{text: "Understood."},
	}
	p := script(t, replies, oneAttemptPerTurn(4))
	spy := &recordingProvider{inner: p}
	h := newHarness(t, p, nil, withMaxTurns(4), withProvider(spy))
	h.policy.verdict = permission.VerdictDeny
	h.policy.reason = theDenialReason

	if _, err := h.eng.Run(t.Context(), "clean up"); err != nil {
		t.Fatalf("Run: %v", err)
	}

	// call-3's command is byte-identical to call-1's, but call-2 — a
	// different command — was dispatched in between, so call-3 must not be
	// read as a repeat of call-1.
	content := toolMessage(t, spy.requests[3], "call-3")
	if !strings.Contains(content, theDenialReason) {
		t.Errorf("a denied call after a different, unrelated call in between was wrongly collapsed: %q", content)
	}

	// Every one of the three denials is still journaled in full.
	evs := h.events()
	results := payloadsOf[journal.ToolResult](t, evs)
	if len(results) != 3 {
		t.Fatalf("got %d ToolResult events, want 3", len(results))
	}
	for _, r := range results {
		if !strings.Contains(r.Output.Inline, theDenialReason) {
			t.Errorf("ToolResult for call %q lost the full denial reason from the journal: %q",
				r.CallID, r.Output.Inline)
		}
	}
}

// TestAllowedRepeatedIdenticalCallsAreNeverCollapsed is (e): collapsing is
// scoped to denials only. A model that deliberately reruns the same allowed
// command twice — a normal thing to do, and each run may have a genuinely
// different real result — must see its own real output both times, never a
// denial-shaped note.
func TestAllowedRepeatedIdenticalCallsAreNeverCollapsed(t *testing.T) {
	replies := []scriptedReply{
		{calls: []wireCall{nativeCall("call-1", tools.ToolRunShell, `{"command":"echo same"}`)}},
		{calls: []wireCall{nativeCall("call-2", tools.ToolRunShell, `{"command":"echo same"}`)}},
		{text: "Both runs succeeded."},
	}
	p := script(t, replies, oneAttemptPerTurn(3))
	spy := &recordingProvider{inner: p}
	// The default policy verdict from newHarness is permission.VerdictAllow.
	h := newHarness(t, p, nil, withMaxTurns(3), withProvider(spy))

	if _, err := h.eng.Run(t.Context(), "run the same command twice"); err != nil {
		t.Fatalf("Run: %v", err)
	}

	content := toolMessage(t, spy.requests[2], "call-2")
	if strings.Contains(content, "denied") || strings.Contains(content, "identically") {
		t.Errorf("an allowed repeated call was collapsed as though it were a denial: %q", content)
	}
	if !strings.Contains(content, "same") {
		t.Errorf("call-2's own real output is missing, replaced by something else: %q", content)
	}

	// run_shell always asks (it is never a "read"), and both calls here are
	// allowed. Two calls, two decisions — collapsing never applies to either.
	evs := h.events()
	decisions := payloadsOf[journal.PermissionDecided](t, evs)
	if len(decisions) != 2 {
		t.Fatalf("got %d PermissionDecided events, want 2", len(decisions))
	}
	for _, d := range decisions {
		if d.Decision != permission.VerdictAllow.String() {
			t.Errorf("decision = %q, want %q", d.Decision, permission.VerdictAllow)
		}
	}
}
