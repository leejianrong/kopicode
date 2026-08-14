package engine_test

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/leejianrong/kopicode/internal/anchor"
	"github.com/leejianrong/kopicode/internal/engine"
	"github.com/leejianrong/kopicode/internal/journal"
	"github.com/leejianrong/kopicode/internal/parse"
	"github.com/leejianrong/kopicode/internal/permission"
	"github.com/leejianrong/kopicode/internal/provider"
	"github.com/leejianrong/kopicode/internal/provider/mock"
	"github.com/leejianrong/kopicode/internal/syntax"
	"github.com/leejianrong/kopicode/internal/tools"
)

// TestSessionRunsToACleanStop is the first of the card's three done-when
// clauses, driven on all three extraction routes because which route a model
// reaches for is a finding and the loop must not care.
//
// The shipped fixtures are two-turn sessions: read a file, then answer in
// prose. The clean stop is the second reply — the model asked for no tool, so
// there is nothing left to do. What is asserted is the whole event sequence,
// the mock provider being drained, and the tree being unchanged by a session
// that only read.
func TestSessionRunsToACleanStop(t *testing.T) {
	routes := map[string]parse.Route{
		"two_turn_native_tool_call":      parse.RouteNative,
		"two_turn_fenced_json_tool_call": parse.RouteFencedJSON,
		"two_turn_xml_tag_tool_call":     parse.RouteXMLTag,
	}

	for name, route := range routes {
		t.Run(name, func(t *testing.T) {
			prov, err := mock.Load(name)
			if err != nil {
				t.Fatalf("loading fixture: %v", err)
			}
			h := newHarness(t, prov, map[string]string{"internal/greet/greet.go": greetGo})

			res, err := h.eng.Run(t.Context(), "Why does Greet(\"\") render as \"Hello, !\"?")
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			if res.Stop != engine.StopCompleted {
				t.Errorf("stop = %s, want %s", res.Stop, engine.StopCompleted)
			}
			if res.Turns != 2 {
				t.Errorf("turns = %d, want 2 — the fixture records two", res.Turns)
			}
			if err := h.eng.Close(t.Context()); err != nil {
				t.Fatalf("Close: %v", err)
			}

			// A session that stopped early passes every assertion about the
			// turns it did reach, which is what this catches.
			if err := prov.Drained(); err != nil {
				t.Errorf("provider not drained: %v", err)
			}

			want := []journal.Type{
				journal.TypeSessionStarted,
				journal.TypeUserMessage,
				journal.TypeProviderRequest,
				journal.TypeProviderResponse,
				journal.TypeToolCallRequested,
				journal.TypeToolCallParsed,
				journal.TypeToolResult,
				journal.TypeProviderRequest,
				journal.TypeProviderResponse,
				journal.TypeAssistantMessage,
				journal.TypeSessionEnded,
			}
			if route != parse.RouteNative {
				// A text-route reply carries prose as well as the call, so the
				// assistant message is journaled on turn one too.
				want = append(want[:4:4],
					append([]journal.Type{journal.TypeAssistantMessage}, want[4:]...)...)
			}
			if diff := cmp.Diff(want, h.types()); diff != "" {
				t.Errorf("event sequence (-want +got):\n%s", diff)
			}

			evs := h.events()
			parsed := sole[journal.ToolCallParsed](t, evs)
			if parsed.Tool != tools.ToolReadFile {
				t.Errorf("tool = %q, want %q", parsed.Tool, tools.ToolReadFile)
			}
			if parsed.Route != route.String() {
				t.Errorf("route = %q, want %q", parsed.Route, route)
			}

			ended := sole[journal.SessionEnded](t, evs)
			if ended.Reason != "completed" || ended.ExitCode != 0 {
				t.Errorf("session ended %q exit %d, want \"completed\" exit 0", ended.Reason, ended.ExitCode)
			}

			// The read has to have actually read: an empty result would mean
			// the loop dispatched into nothing and the model saw a failure.
			result := sole[journal.ToolResult](t, evs)
			if result.ErrorKind != "" {
				t.Errorf("tool result error kind = %q, want success", result.ErrorKind)
			}
			if missing, ok := containsAll(result.Output.Inline, "greet.go", "func Greet"); !ok {
				t.Errorf("read_file output does not contain %q:\n%s", missing, result.Output.Inline)
			}

			// A session that only read must leave the tree alone, and must not
			// have claimed a snapshot of a turn that changed nothing.
			onDisk, err := os.ReadFile(filepath.Join(h.root, "internal", "greet", "greet.go"))
			if err != nil {
				t.Fatalf("reading the file back: %v", err)
			}
			if string(onDisk) != greetGo {
				t.Errorf("the file changed during a read-only session")
			}
			if len(h.snap.turns) != 0 {
				t.Errorf("snapshotted turns %v on a session that touched nothing", h.snap.turns)
			}
		})
	}
}

// TestArgEncodingIsJournaled holds KAN-838's field to something that populates
// it. The native fixture sends OpenAI's double-encoded JSON string; the text
// routes write a bare object. Both normalise to the same arguments, and the
// difference is the finding.
func TestArgEncodingIsJournaled(t *testing.T) {
	cases := map[string]string{
		"two_turn_native_tool_call":      parse.ArgsJSONString.String(),
		"two_turn_fenced_json_tool_call": parse.ArgsObject.String(),
		"two_turn_xml_tag_tool_call":     parse.ArgsObject.String(),
	}

	for name, want := range cases {
		t.Run(name, func(t *testing.T) {
			prov, err := mock.Load(name)
			if err != nil {
				t.Fatalf("loading fixture: %v", err)
			}
			h := newHarness(t, prov, map[string]string{"internal/greet/greet.go": greetGo})
			if _, err := h.eng.Run(t.Context(), "read it"); err != nil {
				t.Fatalf("Run: %v", err)
			}

			got := sole[journal.ToolCallParsed](t, h.events())
			if got.ArgEncoding != want {
				t.Errorf("arg_encoding = %q, want %q", got.ArgEncoding, want)
			}
			// Empty means "written before the field existed" and must never be
			// what a live session writes.
			if got.ArgEncoding == "" {
				t.Error("arg_encoding is empty, which is reserved for records written before the field existed")
			}
		})
	}
}

// TestMaxTurnsCapIsJournaledAndClassifiesAsHarness is the second done-when
// clause.
//
// The script never stops: every reply calls read_file again. The cap is what
// ends it, and the record has to say so in a way SLICE-1 §9's classifier can
// read without judgement — which classify() below implements from the ADR's own
// wording rather than from anything this package exports.
func TestMaxTurnsCapIsJournaledAndClassifiesAsHarness(t *testing.T) {
	const cap = 3
	replies := repeat(scriptedReply{
		calls: []wireCall{nativeCall("call-1", tools.ToolReadFile, `{"path":"greet.go"}`)},
		usage: wireUsage{Prompt: 10, Completion: 5, Total: 15},
	}, cap)

	h := scriptHarness(t, replies, oneAttemptPerTurn(cap),
		map[string]string{"greet.go": greetGo}, withMaxTurns(cap))

	res, err := h.eng.Run(t.Context(), "keep going")
	if err != nil {
		t.Fatalf("Run: %v — reaching the cap is a stop, not an error", err)
	}
	if res.Stop != engine.StopMaxTurns {
		t.Fatalf("stop = %s, want %s", res.Stop, engine.StopMaxTurns)
	}
	if res.Turns != cap {
		t.Errorf("turns = %d, want the cap %d", res.Turns, cap)
	}
	if err := h.eng.Close(t.Context()); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := h.prov.Drained(); err != nil {
		t.Errorf("provider not drained: %v — the loop should have spent exactly the cap", err)
	}

	ended := sole[journal.SessionEnded](t, h.events())
	if ended.Reason != "max_turns" {
		t.Errorf("session ended %q, want \"max_turns\"", ended.Reason)
	}
	if ended.ExitCode != 4 {
		t.Errorf("exit code = %d, want 4 (harness error, docs/SLICE-1.md §Build Plan step 14)", ended.ExitCode)
	}

	if got := classify(h.events()); got != bucketHarness {
		t.Errorf("classified %q, want %q — docs/SLICE-1.md §9 names the max-turns cap "+
			"as a harness failure outright", got, bucketHarness)
	}
}

// TestTokenBudgetStopsBeforeTheNextRequest pins what "enforced" means for a
// bound whose only authoritative input arrives after the request that spends
// it.
//
// The budget is spend-to-date, checked before every provider call. Two replies
// of 15 tokens with a budget of 20 must therefore send exactly one request: the
// first is sent under a spend of zero, the second is refused under a spend of
// 15... no — 15 is below 20, so the second is sent, and the third is refused
// under 30. The script carries three replies and the last must go unserved,
// which is what Drained reports.
func TestTokenBudgetStopsBeforeTheNextRequest(t *testing.T) {
	replies := repeat(scriptedReply{
		calls: []wireCall{nativeCall("call-1", tools.ToolReadFile, `{"path":"greet.go"}`)},
		usage: wireUsage{Prompt: 10, Completion: 5, Total: 15},
	}, 3)

	h := scriptHarness(t, replies, oneAttemptPerTurn(3),
		map[string]string{"greet.go": greetGo}, withTokenBudget(20), withMaxTurns(10))

	res, err := h.eng.Run(t.Context(), "spend it")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Stop != engine.StopBudgetExhausted {
		t.Fatalf("stop = %s, want %s", res.Stop, engine.StopBudgetExhausted)
	}
	if res.Turns != 2 {
		t.Errorf("turns = %d, want 2: the budget is crossed by the reply that reports the spend, "+
			"so the request in flight completes and the next one is refused", res.Turns)
	}
	if res.Tokens.Total != 30 {
		t.Errorf("reported total = %d, want 30", res.Tokens.Total)
	}
	if err := h.prov.Drained(); err == nil {
		t.Error("the provider was drained: the loop spent every recorded reply, so nothing was refused")
	}

	if err := h.eng.Close(t.Context()); err != nil {
		t.Fatalf("Close: %v", err)
	}
	ended := sole[journal.SessionEnded](t, h.events())
	if ended.Reason != "budget_exhausted" {
		t.Errorf("session ended %q, want \"budget_exhausted\"", ended.Reason)
	}

	// The decision must come from what the provider reported, and the record
	// must show that: every ProviderRequest carries a zero token count, because
	// there is no tokenizer here and an estimate in a field named `tokens` is
	// fabricated precision.
	for i, req := range payloadsOf[journal.ProviderRequest](t, h.events()) {
		if req.Tokens != (journal.TokenCounts{}) {
			t.Errorf("ProviderRequest %d carries tokens %+v; the prompt count is not knowable "+
				"before the reply and must not be guessed", i, req.Tokens)
		}
	}
	responses := payloadsOf[journal.ProviderResponse](t, h.events())
	if len(responses) != 2 {
		t.Fatalf("got %d ProviderResponse events, want 2", len(responses))
	}
	for i, resp := range responses {
		if resp.Tokens.Total != 15 {
			t.Errorf("ProviderResponse %d total = %d, want 15 as the fixture reports", i, resp.Tokens.Total)
		}
	}
}

// TestRepairRoundTripStaysInsideOneTurn holds the accounting the cap depends
// on. A repair is another attempt at the same turn, not a new turn: the unit of
// repair is the reply, and a budget of two repairs must not be able to spend
// three turns of a cap.
//
// The mock provider checks the loop's stated (turn, attempt) against the
// recording, so a loop that counted either differently fails here rather than
// passing quietly.
func TestRepairRoundTripStaysInsideOneTurn(t *testing.T) {
	replies := []scriptedReply{
		// Turn 1, attempt 1: a fenced block that does not parse.
		{text: "```tool\n{\"tool\": \"read_file\", \"arguments\": {\"path\": }\n```",
			usage: wireUsage{Prompt: 10, Completion: 5, Total: 15}},
		// Turn 1, attempt 2: the corrected call.
		{calls: []wireCall{nativeCall("call-1", tools.ToolReadFile, `{"path":"greet.go"}`)},
			usage: wireUsage{Prompt: 12, Completion: 5, Total: 17}},
		// Turn 2: done.
		{text: "The switch has no empty-name case.", usage: wireUsage{Prompt: 20, Completion: 8, Total: 28}},
	}
	positions := [][2]int{{1, 1}, {1, 2}, {2, 1}}

	h := scriptHarness(t, replies, positions, map[string]string{"greet.go": greetGo}, withMaxTurns(2))

	res, err := h.eng.Run(t.Context(), "read it")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Stop != engine.StopCompleted {
		t.Fatalf("stop = %s, want %s", res.Stop, engine.StopCompleted)
	}
	if res.Turns != 2 {
		t.Errorf("turns = %d, want 2: three replies, but the repair was a second attempt at turn 1", res.Turns)
	}
	if err := h.prov.Drained(); err != nil {
		t.Errorf("provider not drained: %v", err)
	}

	evs := h.events()
	repaired := sole[journal.ToolCallRepaired](t, evs)
	if repaired.Attempt != 1 {
		t.Errorf("repair attempt = %d, want 1", repaired.Attempt)
	}
	if repaired.Classification == "" {
		t.Error("the repair recorded no classification, so parse-failure kinds cannot be scored")
	}
	if repaired.Error.Inline == "" {
		t.Error("the repair recorded no feedback, so repair-recovery cannot be attributed to the wording")
	}

	// The malformed reply is on the record verbatim, which is what makes the
	// corpus of real malformed calls (KAN-777) harvestable from journals.
	requested := payloadsOf[journal.ToolCallRequested](t, evs)
	if len(requested) != 2 {
		t.Fatalf("got %d ToolCallRequested events, want 2 (the malformed reply and the repaired call)", len(requested))
	}
	if !strings.Contains(requested[0].Raw.Inline, "```tool") {
		t.Errorf("the malformed reply was not journaled as it was emitted:\n%s", requested[0].Raw.Inline)
	}

	// The repair happened inside turn 1, so both attempts carry that turn on
	// the envelope.
	for _, ev := range evs {
		if _, ok := ev.Payload.(journal.ToolCallRepaired); ok && ev.Turn != 1 {
			t.Errorf("the repair is journaled on turn %d, want 1", ev.Turn)
		}
	}
}

// TestExhaustedRepairsContinueTheTurn holds docs/SLICE-1.md §3: after the
// budget is spent the call fails and the turn continues with the failure as an
// observation, rather than the session aborting.
func TestExhaustedRepairsContinueTheTurn(t *testing.T) {
	malformed := scriptedReply{
		text:  "```tool\n{\"tool\": \"read_file\", \"arguments\": {\"path\": }\n```",
		usage: wireUsage{Prompt: 10, Completion: 5, Total: 15},
	}
	replies := []scriptedReply{
		malformed, malformed, malformed,
		{text: "I give up and will describe it instead.", usage: wireUsage{Prompt: 20, Completion: 8, Total: 28}},
	}
	positions := [][2]int{{1, 1}, {1, 2}, {1, 3}, {2, 1}}

	h := scriptHarness(t, replies, positions, map[string]string{"greet.go": greetGo}, withMaxTurns(3))

	res, err := h.eng.Run(t.Context(), "read it")
	if err != nil {
		t.Fatalf("Run: %v — an exhausted repair budget must not abort the session", err)
	}
	if res.Stop != engine.StopCompleted {
		t.Errorf("stop = %s, want %s", res.Stop, engine.StopCompleted)
	}
	if err := h.prov.Drained(); err != nil {
		t.Errorf("provider not drained: %v", err)
	}

	evs := h.events()
	if got := len(payloadsOf[journal.ToolCallRepaired](t, evs)); got != parse.DefaultBudget {
		t.Errorf("got %d repairs, want the budget %d", got, parse.DefaultBudget)
	}
	failed := sole[journal.ToolCallFailed](t, evs)
	if failed.Reason == "" {
		t.Error("the give-up recorded no reason")
	}

	// A ToolCallFailed after repairs were exhausted is a harness failure by
	// SLICE-1 §9's first clause, even though the session ran to a clean stop.
	if got := classify(evs); got != bucketHarness {
		t.Errorf("classified %q, want %q", got, bucketHarness)
	}
}

// TestNoRepairArm holds that the budget is a parameter and not a constant:
// running with none is how the experiment measures what repair buys.
func TestNoRepairArm(t *testing.T) {
	replies := []scriptedReply{
		{text: "```tool\n{\"tool\": \"read_file\", \"arguments\": {\"path\": }\n```",
			usage: wireUsage{Prompt: 10, Completion: 5, Total: 15}},
		{text: "Never mind.", usage: wireUsage{Prompt: 20, Completion: 8, Total: 28}},
	}

	h := scriptHarness(t, replies, oneAttemptPerTurn(2),
		map[string]string{"greet.go": greetGo}, withRepairBudget(0), withMaxTurns(2))

	if _, err := h.eng.Run(t.Context(), "read it"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	evs := h.events()
	if got := len(payloadsOf[journal.ToolCallRepaired](t, evs)); got != 0 {
		t.Errorf("got %d repairs under a disabled budget, want 0", got)
	}
	if got := len(payloadsOf[journal.ToolCallFailed](t, evs)); got != 1 {
		t.Errorf("got %d failures, want 1 — the first malformed call fails immediately", got)
	}
}

// TestEditIsJournaledAndTheSyntaxGateFollowsIt holds the adjacency SLICE-1 §9
// classifies on: a syntax-gate failure *immediately following* an EditApplied
// is a harness signal, so nothing may come between them.
func TestEditIsJournaledAndTheSyntaxGateFollowsIt(t *testing.T) {
	// The anchor is derived the way read_file derives it, so the call the
	// script makes is one the model could only have made after a read — which
	// is the structural guarantee ADR-0006 rests on.
	anchors := anchor.Derive(anchor.Split([]byte(greetGo)))
	const defaultLine = 5 // 0-based: "\tdefault:"

	args, err := json.Marshal(map[string]string{
		"path":         "greet.go",
		"anchor_start": anchors[defaultLine],
		"anchor_end":   anchors[defaultLine],
		"new_text":     "\tcase \"\":\n\t\treturn \"Hello, stranger!\"\n\tdefault:\n",
	})
	if err != nil {
		t.Fatalf("encoding edit arguments: %v", err)
	}

	replies := []scriptedReply{
		{calls: []wireCall{nativeCall("call-edit", tools.ToolEditFile, string(args))},
			usage: wireUsage{Prompt: 20, Completion: 9, Total: 29}},
		{text: "Done.", usage: wireUsage{Prompt: 30, Completion: 4, Total: 34}},
	}

	h := scriptHarness(t, replies, oneAttemptPerTurn(2), map[string]string{"greet.go": greetGo}, withMaxTurns(2))

	if _, err := h.eng.Run(t.Context(), "add an empty-name case"); err != nil {
		t.Fatalf("Run: %v", err)
	}

	evs := h.events()
	applied := sole[journal.EditApplied](t, evs)
	if applied.Mode != tools.ModeAnchored {
		t.Errorf("edit mode = %q, want %q", applied.Mode, tools.ModeAnchored)
	}
	if applied.AnchorVersion == "" {
		t.Error("EditApplied carries no anchor version; a bump is an experiment-series boundary")
	}
	if !strings.Contains(applied.Diff.Inline, "+++") {
		t.Errorf("EditApplied carries no diff:\n%s", applied.Diff.Inline)
	}

	types := h.types()
	at := -1
	for i, ty := range types {
		if ty == journal.TypeEditApplied {
			at = i
		}
	}
	if at < 0 || at+1 >= len(types) || types[at+1] != journal.TypeSyntaxGateRun {
		t.Fatalf("SyntaxGateRun does not immediately follow EditApplied:\n%v", types)
	}

	gate := sole[journal.SyntaxGateRun](t, evs)
	if gate.Ran {
		t.Error("the gate reports it ran, but the harness gave it a LookPath that finds nothing")
	}
	if gate.ExitCode == 0 {
		t.Error("a gate that did not run reports exit 0, which reads as a pass")
	}

	// The file really changed, and the turn that changed it was snapshotted.
	onDisk, err := os.ReadFile(filepath.Join(h.root, "greet.go"))
	if err != nil {
		t.Fatalf("reading back: %v", err)
	}
	if string(onDisk) == greetGo {
		t.Error("the file is unchanged, so the edit did not apply")
	}
	if diff := cmp.Diff([]int{1}, h.snap.turns); diff != "" {
		t.Errorf("snapshotted turns (-want +got):\n%s", diff)
	}
	snap := sole[journal.TurnSnapshot](t, evs)
	if !strings.HasSuffix(snap.Ref, "/1") {
		t.Errorf("snapshot ref = %q, want the one for turn 1", snap.Ref)
	}
}

// TestFuzzyEditMarksTheSession holds the `unattributed` trigger: SLICE-1 §9
// classifies a session that used the fuzzy fallback at any point, and the mode
// on EditApplied is the only thing that says so.
func TestFuzzyEditMarksTheSession(t *testing.T) {
	replies := []scriptedReply{
		{calls: []wireCall{nativeCall("call-fuzzy", tools.ToolEditFileFuzzy,
			`{"path":"greet.go","before":"\tdefault:\n","after":"\tcase \"\":\n\t\treturn \"Hello, stranger!\"\n\tdefault:\n"}`)},
			usage: wireUsage{Prompt: 10, Completion: 5, Total: 15}},
		{text: "Done.", usage: wireUsage{Prompt: 20, Completion: 4, Total: 24}},
	}

	h := scriptHarness(t, replies, oneAttemptPerTurn(2), map[string]string{"greet.go": greetGo}, withMaxTurns(2))
	if _, err := h.eng.Run(t.Context(), "fix it"); err != nil {
		t.Fatalf("Run: %v", err)
	}

	evs := h.events()
	applied := sole[journal.EditApplied](t, evs)
	if applied.Mode != tools.ModeFuzzy {
		t.Fatalf("edit mode = %q, want %q", applied.Mode, tools.ModeFuzzy)
	}
	if got := classify(evs); got != bucketUnattributed {
		t.Errorf("classified %q, want %q", got, bucketUnattributed)
	}
}

// TestPermissionIsAskedAndJournaled holds the M1 affordance from the engine's
// side: the engine decides *that* consent is required, and the record carries
// the request and the answer as a pair a reader can tie to the call they gated.
func TestPermissionIsAskedAndJournaled(t *testing.T) {
	replies := []scriptedReply{
		{calls: []wireCall{nativeCall("call-shell", tools.ToolRunShell, `{"command":"echo hello"}`)},
			usage: wireUsage{Prompt: 10, Completion: 5, Total: 15}},
		{text: "It printed hello.", usage: wireUsage{Prompt: 20, Completion: 4, Total: 24}},
	}
	h := scriptHarness(t, replies, oneAttemptPerTurn(2), nil, withMaxTurns(2))

	if _, err := h.eng.Run(t.Context(), "run it"); err != nil {
		t.Fatalf("Run: %v", err)
	}

	evs := h.events()
	req := sole[journal.PermissionRequested](t, evs)
	dec := sole[journal.PermissionDecided](t, evs)
	if req.Kind != permission.KindRunShell.String() {
		t.Errorf("kind = %q, want %q", req.Kind, permission.KindRunShell)
	}
	if req.RequestID != dec.RequestID || req.RequestID != "call-shell" {
		t.Errorf("request/decision ids = %q/%q, want both to be the call id", req.RequestID, dec.RequestID)
	}
	if dec.Source != permission.SourceUser.String() {
		t.Errorf("source = %q, want %q — a policy's answer must not be indistinguishable from a human's",
			dec.Source, permission.SourceUser)
	}

	// The argv consented to is the argv that ran. A permission record naming a
	// different command than the shell executed is consent for something else.
	if len(h.policy.requests) != 1 {
		t.Fatalf("the policy was asked %d times, want 1", len(h.policy.requests))
	}
	if diff := cmp.Diff(tools.ShellArgv("echo hello"), h.policy.requests[0].Action.Command); diff != "" {
		t.Errorf("consented argv (-want +got):\n%s", diff)
	}
	if h.policy.requests[0].Action.Dir != h.rootReal(t) {
		t.Errorf("consented working directory = %q, want the repository root %q — an empty Dir means "+
			"the process's own, which is how a bench task escapes its worktree",
			h.policy.requests[0].Action.Dir, h.rootReal(t))
	}

	result := sole[journal.ToolResult](t, evs)
	if result.ExitCode == nil || *result.ExitCode != 0 {
		t.Errorf("shell exit code = %v, want 0", result.ExitCode)
	}
	if !strings.Contains(result.Output.Inline, "hello") {
		t.Errorf("shell output does not carry the command's stdout:\n%s", result.Output.Inline)
	}
}

// TestDeniedPermissionIsAnObservationNotAHarnessFailure holds the
// classification. A refusal is the harness working exactly as designed, and
// booking it as `internal` would be noise against an acceptance criterion of
// zero harness failures.
func TestDeniedPermissionIsAnObservationNotAHarnessFailure(t *testing.T) {
	replies := []scriptedReply{
		{calls: []wireCall{nativeCall("call-shell", tools.ToolRunShell, `{"command":"rm -rf /"}`)},
			usage: wireUsage{Prompt: 10, Completion: 5, Total: 15}},
		{text: "Understood, I will not.", usage: wireUsage{Prompt: 20, Completion: 4, Total: 24}},
	}
	h := scriptHarness(t, replies, oneAttemptPerTurn(2), nil, withMaxTurns(2))
	h.policy.verdict = permission.VerdictDeny
	h.policy.reason = "not on my machine"

	res, err := h.eng.Run(t.Context(), "clean up")
	if err != nil {
		t.Fatalf("Run: %v — a denial is an observation, not a failure of the loop", err)
	}
	if res.Stop != engine.StopCompleted {
		t.Errorf("stop = %s, want %s", res.Stop, engine.StopCompleted)
	}

	evs := h.events()
	dec := sole[journal.PermissionDecided](t, evs)
	if dec.Decision != permission.VerdictDeny.String() {
		t.Errorf("decision = %q, want %q", dec.Decision, permission.VerdictDeny)
	}
	result := sole[journal.ToolResult](t, evs)
	if result.ErrorKind != tools.FaultTask.String() {
		t.Errorf("error kind = %q, want %q", result.ErrorKind, tools.FaultTask)
	}
	if got := classify(evs); got != bucketModel {
		t.Errorf("classified %q, want %q", got, bucketModel)
	}
}

// TestOversizedToolOutputSpillsRatherThanClipping holds CLAUDE.md's rule at the
// loop's own boundary: the model is shown everything, and the record keeps
// everything.
func TestOversizedToolOutputSpillsRatherThanClipping(t *testing.T) {
	if testing.Short() {
		// Writing and reading back a megabyte is cheap but not free, and the
		// property is about size rather than about the loop.
		t.Skip("skipping the blob-spill size check in -short")
	}
	big := strings.Repeat("diagnostic line that justifies the fix\n", 3000)

	replies := []scriptedReply{
		{calls: []wireCall{nativeCall("call-read", tools.ToolReadFile, `{"path":"big.txt","limit":100000}`)},
			usage: wireUsage{Prompt: 10, Completion: 5, Total: 15}},
		{text: "Read it.", usage: wireUsage{Prompt: 20, Completion: 4, Total: 24}},
	}
	h := scriptHarness(t, replies, oneAttemptPerTurn(2), map[string]string{"big.txt": big}, withMaxTurns(2))

	if _, err := h.eng.Run(t.Context(), "read it"); err != nil {
		t.Fatalf("Run: %v", err)
	}

	result := sole[journal.ToolResult](t, h.events())
	if !result.Output.Spilled() {
		t.Fatalf("output of %d bytes was not spilled to a blob", result.Output.Size)
	}
	// Read puts the content back, so a caller reads the value that was
	// appended. Nothing may be missing from it.
	if int64(len(result.Output.Inline)) != result.Output.Size {
		t.Errorf("rehydrated %d bytes of a %d-byte value: something was clipped",
			len(result.Output.Inline), result.Output.Size)
	}
	if !strings.Contains(result.Output.Inline, "diagnostic line that justifies the fix") {
		t.Error("the spilled output does not contain what was read")
	}
}

// TestReplayedSessionProducesAByteIdenticalJournal is docs/SLICE-1.md's
// determinism criterion at the seam it is about. Two runs of one recording,
// with an injected clock and an injected session id, must produce the same
// bytes — which is what rules out a goroutine or a map range in any output
// path.
func TestReplayedSessionProducesAByteIdenticalJournal(t *testing.T) {
	run := func() []byte {
		t.Helper()
		p, err := mock.Load("two_turn_native_tool_call")
		if err != nil {
			t.Fatalf("loading fixture: %v", err)
		}
		h := newHarness(t, p, map[string]string{"internal/greet/greet.go": greetGo},
			withCWD("/fixed/working/directory"))
		if _, err := h.eng.Run(t.Context(), "why is it wrong?"); err != nil {
			t.Fatalf("Run: %v", err)
		}
		if err := h.eng.Close(t.Context()); err != nil {
			t.Fatalf("Close: %v", err)
		}
		if err := h.jrn.Close(); err != nil {
			t.Fatalf("closing the journal: %v", err)
		}
		b, err := os.ReadFile(h.jrn.Path())
		if err != nil {
			t.Fatalf("reading the journal file: %v", err)
		}
		return b
	}

	first, second := run(), run()
	if string(first) != string(second) {
		t.Errorf("two replays of one recording produced different journals\n--- first ---\n%s\n--- second ---\n%s",
			first, second)
	}
	if len(first) == 0 {
		t.Fatal("positive control failed: the journal is empty, so the comparison above proves nothing")
	}
}

// TestStreamSeamSeesEveryDelta holds the seam KAN-793 prints through. It is
// called synchronously on the goroutine pulling the stream, which is what keeps
// the journal above byte-identical.
func TestStreamSeamSeesEveryDelta(t *testing.T) {
	p, err := mock.Load("two_turn_fenced_json_tool_call")
	if err != nil {
		t.Fatalf("loading fixture: %v", err)
	}
	var text strings.Builder
	h := newHarness(t, p, map[string]string{"internal/greet/greet.go": greetGo},
		withStream(func(d provider.Delta) {
			if d.Kind == provider.DeltaContent {
				text.WriteString(d.Text)
			}
		}))

	if _, err := h.eng.Run(t.Context(), "read it"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if missing, ok := containsAll(text.String(), "```tool", "read_file"); !ok {
		t.Errorf("the stream seam never saw %q; it saw:\n%s", missing, text.String())
	}
}

// TestUnknownToolIsAHarnessFailure holds the direction ADR-0006 asks us to be
// wrong in. A model naming a tool the catalogue offers and the dispatcher does
// not is the two disagreeing, which is a harness defect and must not be scored
// as the model's.
func TestUnknownToolIsAHarnessFailure(t *testing.T) {
	replies := []scriptedReply{
		{calls: []wireCall{nativeCall("call-x", "teleport", `{}`)},
			usage: wireUsage{Prompt: 10, Completion: 5, Total: 15}},
		{text: "Sorry.", usage: wireUsage{Prompt: 20, Completion: 4, Total: 24}},
	}
	// The repair loop would ordinarily catch this and ask the model to fix it;
	// disabling repair is what gets an unknown name as far as the dispatcher.
	h := scriptHarness(t, replies, oneAttemptPerTurn(2), nil, withMaxTurns(2), withRepairBudget(0))

	if _, err := h.eng.Run(t.Context(), "go"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	evs := h.events()
	failed := sole[journal.ToolCallFailed](t, evs)
	if failed.Tool != "teleport" {
		t.Errorf("failed tool = %q, want %q", failed.Tool, "teleport")
	}
	if got := classify(evs); got != bucketHarness {
		t.Errorf("classified %q, want %q", got, bucketHarness)
	}
}

// TestNewRefusesAConfigurationItCannotRunSafely holds the fail-closed
// direction: a missing collaborator must not be the quiet way to disable a
// gate, and a bound left negative must not be the quiet way to remove it.
func TestNewRefusesAConfigurationItCannotRunSafely(t *testing.T) {
	root := t.TempDir()
	set, err := tools.NewSet(root)
	if err != nil {
		t.Fatalf("tool set: %v", err)
	}
	t.Cleanup(func() { _ = set.Close() })
	gate, err := permission.New(set.Root.Path(), set.Root.Resolver(), &recordingPolicy{verdict: permission.VerdictAllow})
	if err != nil {
		t.Fatalf("gate: %v", err)
	}
	jrn, err := journal.Open(root, "cfg", journal.WithClock(fixedClock()))
	if err != nil {
		t.Fatalf("journal: %v", err)
	}
	t.Cleanup(func() { _ = jrn.Close() })
	p, err := mock.Load("two_turn_native_tool_call")
	if err != nil {
		t.Fatalf("fixture: %v", err)
	}
	good := engine.Config{
		SessionID:   "cfg",
		Selection:   testSelection(p),
		Provider:    p,
		Journal:     jrn,
		Tools:       set,
		Permissions: gate,
		Syntax:      &syntax.Gate{Root: set.Root.Path()},
	}

	// Positive control: the configuration everything below breaks one field of
	// must itself be accepted, or every case passes for the wrong reason.
	if _, err := engine.New(good); err != nil {
		t.Fatalf("positive control failed: the complete configuration was refused: %v", err)
	}

	cases := map[string]func(*engine.Config){
		"no session id":    func(c *engine.Config) { c.SessionID = "" },
		"no model id":      func(c *engine.Config) { c.Selection.ModelID = "" },
		"no harness hash":  func(c *engine.Config) { c.Selection.HarnessConfigHash = "" },
		"no provider":      func(c *engine.Config) { c.Provider = nil },
		"no journal":       func(c *engine.Config) { c.Journal = nil },
		"no tools":         func(c *engine.Config) { c.Tools = nil },
		"no permissions":   func(c *engine.Config) { c.Permissions = nil },
		"no syntax gate":   func(c *engine.Config) { c.Syntax = nil },
		"no turn cap":      func(c *engine.Config) { c.Selection.Config.MaxTurns = 0 },
		"negative turns":   func(c *engine.Config) { c.Selection.Config.MaxTurns = -1 },
		"negative budget":  func(c *engine.Config) { c.Selection.Config.TokenBudget = -1 },
		"negative repairs": func(c *engine.Config) { c.Selection.Config.RepairBudget = -1 },
		"empty tool set":   func(c *engine.Config) { c.Selection.Config.ToolSet = nil },
		"unknown tool":     func(c *engine.Config) { c.Selection.Config.ToolSet = []string{"teleport"} },
	}
	for name, breakIt := range cases {
		t.Run(name, func(t *testing.T) {
			cfg := good
			breakIt(&cfg)
			if _, err := engine.New(cfg); !errors.Is(err, engine.ErrConfig) {
				t.Errorf("New = %v, want an error wrapping ErrConfig", err)
			}
		})
	}
}

// TestRunBeforeStartIsRefused holds the session lifecycle, so a surface cannot
// produce turns that no SessionStarted accounts for.
func TestRunBeforeStartIsRefused(t *testing.T) {
	p, err := mock.Load("two_turn_native_tool_call")
	if err != nil {
		t.Fatalf("fixture: %v", err)
	}
	root := t.TempDir()
	set, err := tools.NewSet(root)
	if err != nil {
		t.Fatalf("tool set: %v", err)
	}
	t.Cleanup(func() { _ = set.Close() })
	gate, err := permission.New(set.Root.Path(), set.Root.Resolver(), &recordingPolicy{verdict: permission.VerdictAllow})
	if err != nil {
		t.Fatalf("gate: %v", err)
	}
	jrn, err := journal.Open(root, "lifecycle", journal.WithClock(fixedClock()))
	if err != nil {
		t.Fatalf("journal: %v", err)
	}
	t.Cleanup(func() { _ = jrn.Close() })

	eng, err := engine.New(engine.Config{
		SessionID: "lifecycle", Selection: testSelection(p),
		Provider: p, Journal: jrn, Tools: set,
		Permissions: gate, Syntax: &syntax.Gate{Root: set.Root.Path()},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := eng.Run(t.Context(), "go"); !errors.Is(err, engine.ErrNotStarted) {
		t.Errorf("Run before Start = %v, want ErrNotStarted", err)
	}
	if err := eng.Close(t.Context()); !errors.Is(err, engine.ErrNotStarted) {
		t.Errorf("Close before Start = %v, want ErrNotStarted", err)
	}
	if err := eng.Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := eng.Start(t.Context()); !errors.Is(err, engine.ErrAlreadyStarted) {
		t.Errorf("second Start = %v, want ErrAlreadyStarted", err)
	}
	if err := eng.Close(t.Context()); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := eng.Run(t.Context(), "go"); !errors.Is(err, engine.ErrEnded) {
		t.Errorf("Run after Close = %v, want ErrEnded", err)
	}
}

// rootReal is the symlink-resolved repository root, which is what a resolved
// permission target is compared against. On macOS a t.TempDir lives under a
// symlinked /var, so comparing against h.root would fail for the wrong reason.
func (h *harness) rootReal(t *testing.T) string {
	t.Helper()
	real, err := filepath.EvalSymlinks(h.root)
	if err != nil {
		t.Fatalf("resolving %s: %v", h.root, err)
	}
	return real
}

// classify is SLICE-1 §9's three-bucket rule, implemented from the document's
// own wording over the journal alone.
//
// It is here rather than imported because the classifier is KAN-797 and does
// not exist yet. That makes it a claim about what this card's events *permit*
// rather than a test of the shipped classifier — which is the honest reading,
// and is exactly what this card owes: the events a mechanical rule can be
// written over.
type bucket string

const (
	bucketHarness      bucket = "harness"
	bucketUnattributed bucket = "unattributed"
	bucketModel        bucket = "model"
)

func classify(evs []journal.Event) bucket {
	harness := false
	unattributed := false
	prevWasEdit := false

	for _, ev := range evs {
		switch p := ev.Payload.(type) {
		case journal.ToolCallFailed:
			harness = true
		case journal.ToolResult:
			if p.ErrorKind == tools.FaultInternal.String() {
				harness = true
			}
		case journal.SyntaxGateRun:
			if prevWasEdit && p.Ran && p.ExitCode != 0 {
				harness = true
			}
		case journal.EditApplied:
			if p.Mode == tools.ModeFuzzy {
				unattributed = true
			}
		case journal.SessionEnded:
			switch p.Reason {
			case "max_turns":
				harness = true
			case "error":
				// A provider error surviving retries, or the harness breaking.
				harness = true
			}
		}
		_, prevWasEdit = ev.Payload.(journal.EditApplied)
	}

	switch {
	case harness:
		return bucketHarness
	case unattributed:
		return bucketUnattributed
	default:
		return bucketModel
	}
}
