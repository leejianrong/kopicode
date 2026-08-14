package engine_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/leejianrong/kopicode/internal/engine"
	"github.com/leejianrong/kopicode/internal/journal"
	"github.com/leejianrong/kopicode/internal/parse"
	"github.com/leejianrong/kopicode/internal/permission"
	"github.com/leejianrong/kopicode/internal/provider"
	"github.com/leejianrong/kopicode/internal/provider/fixture"
	"github.com/leejianrong/kopicode/internal/provider/mock"
	"github.com/leejianrong/kopicode/internal/repo"
	"github.com/leejianrong/kopicode/internal/syntax"
	"github.com/leejianrong/kopicode/internal/tools"
)

// This file is the engine's test seam, built the way CLAUDE.md says the primary
// one is: the engine driven through its public interface by the mock/replay
// provider, against a temp working tree, with an injected clock and an injected
// session id. Nothing here reaches into the loop's state; every assertion is
// about the journal, the resulting tree, or an exit code.
//
// **No git.** The fixtures here are plain temp directories, not repositories.
// internal/repo owns the git side and has the fixtures that assert their own
// isolation; the engine's contact with it is one interface with one method,
// which stubSnapshotter satisfies. A git subprocess in this package would be a
// second place to get the containment rules wrong for no test it can only get
// here — see .claude/skills/agent-ground-rules/SKILL.md.

// fixedClock is the injected time source. A journal stamped from the wall clock
// cannot be compared byte for byte across two replays, which is the acceptance
// criterion in docs/SLICE-1.md §Test Plan.
func fixedClock() func() time.Time {
	t := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	return func() time.Time {
		t = t.Add(time.Second)
		return t
	}
}

// greetGo is the file the shipped fixtures read. Its content is irrelevant to
// the loop; what matters is that the path the recorded call names exists, so a
// read that fails would be a real failure rather than a missing fixture file.
const greetGo = `package greet

// Greet returns a greeting for name.
func Greet(name string) string {
	switch name {
	default:
		return "Hello, " + name + "!"
	}
}
`

// harness is one engine under test, with everything it was built from.
type harness struct {
	t    *testing.T
	root string
	jrn  *journal.FileJournal
	prov *mock.Provider
	eng  *engine.Engine
	snap *stubSnapshotter
	// asked records every consent request the policy was handed, so a test can
	// assert the engine asked without the policy having to be a mock.
	policy *recordingPolicy
}

// harnessOption tunes one harness before the engine is built.
type harnessOption func(*engine.Config)

// The bounds live on the harness configuration, not on the engine, because
// ADR-0007 decision 6 puts every one of them in the hash preimage: a bound the
// loop held itself would be a bound outside the arm.
func withMaxTurns(n int) harnessOption {
	return func(c *engine.Config) { c.Selection.Config.MaxTurns = n }
}
func withTokenBudget(n int) harnessOption {
	return func(c *engine.Config) { c.Selection.Config.TokenBudget = n }
}
func withRepairBudget(n int) harnessOption {
	return func(c *engine.Config) { c.Selection.Config.RepairBudget = n }
}
func withToolSet(names ...string) harnessOption {
	return func(c *engine.Config) { c.Selection.Config.ToolSet = names }
}

// withProvider replaces the provider the loop drives, keeping the arm identity
// the harness was built with. It is how a test reaches the *live* client's shape
// — where a streamed reply carries no assembled body — which the replay provider
// cannot produce because a recording always has one.
func withProvider(p engine.Provider) harnessOption {
	return func(c *engine.Config) { c.Provider = p }
}
func withStream(f func(provider.Delta)) harnessOption {
	return func(c *engine.Config) { c.Stream = f }
}

// withCWD pins the directory SessionStarted records.
//
// The byte-identical-replay criterion is about the loop being a pure function
// of the recording, and a t.TempDir is not: it carries a per-run suffix, so a
// journal that honestly records it differs between two runs for a reason that
// has nothing to do with the loop. Pinning it is how that test compares the
// thing it is about.
func withCWD(dir string) harnessOption { return func(c *engine.Config) { c.CWD = dir } }

// withCatalogue replaces the tool schemas the repair loop consults.
func withCatalogue(cat parse.Tools) harnessOption {
	return func(c *engine.Config) { c.Catalogue = cat }
}

// newHarness builds a working tree, a journal, an engine over prov, and starts
// the session.
func newHarness(t *testing.T, prov *mock.Provider, files map[string]string, opts ...harnessOption) *harness {
	t.Helper()

	root := t.TempDir()
	for name, content := range files {
		path := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("creating %s: %v", filepath.Dir(path), err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatalf("writing %s: %v", path, err)
		}
	}

	set, err := tools.NewSet(root)
	if err != nil {
		t.Fatalf("opening tool set: %v", err)
	}
	t.Cleanup(func() { _ = set.Close() })

	pol := &recordingPolicy{verdict: permission.VerdictAllow}
	gate, err := permission.New(set.Root.Path(), set.Root.Resolver(), pol)
	if err != nil {
		t.Fatalf("building permission gate: %v", err)
	}

	jrn, err := journal.Open(root, "session-under-test", journal.WithClock(fixedClock()))
	if err != nil {
		t.Fatalf("opening journal: %v", err)
	}
	t.Cleanup(func() { _ = jrn.Close() })

	snap := &stubSnapshotter{}

	cfg := engine.Config{
		SessionID:   "session-under-test",
		Selection:   testSelection(prov),
		Build:       journal.BuildInfo{Version: "test", Commit: "unknown", TreeState: "unknown", Source: "unknown"},
		CWD:         root,
		Provider:    prov,
		Journal:     jrn,
		Tools:       set,
		Permissions: gate,
		// LookPath refuses every checker, so the gate records an honest
		// "tool_missing" instead of shelling out to the machine's real Go
		// toolchain. What is under test here is that the gate ran and was
		// journaled, not what gofmt thinks; internal/syntax owns the latter.
		Syntax:    &syntax.Gate{Root: set.Root.Path(), LookPath: func(string) (string, error) { return "", os.ErrNotExist }},
		Snapshots: snap,
	}
	for _, opt := range opts {
		opt(&cfg)
	}

	eng, err := engine.New(cfg)
	if err != nil {
		t.Fatalf("building engine: %v", err)
	}
	if err := eng.Start(t.Context()); err != nil {
		t.Fatalf("starting session: %v", err)
	}

	return &harness{t: t, root: root, jrn: jrn, prov: prov, eng: eng, snap: snap, policy: pol}
}

// testSelection is the arm the harness runs, built from the registered default
// so the bounds and the sampling under test are the shipped ones, with the model
// and the pin taken from whichever recording is being replayed.
//
// It resolves through the real registry rather than filling a struct by hand:
// the fields the loop reads have to be the fields KAN-805 populates, and a
// hand-built Selection would go on compiling after that stopped being true.
func testSelection(prov *mock.Provider) engine.Selection {
	sel, err := engine.ResolveSelection(os.TempDir(), engine.SelectionOverrides{})
	if err != nil {
		panic("engine_test: resolving the default selection: " + err.Error())
	}
	sel.ModelID = prov.ModelID()
	sel.Pin = prov.Pin()
	return sel
}

// events reads the journal back, failing on any read error rather than skipping
// a damaged line.
func (h *harness) events() []journal.Event {
	h.t.Helper()
	var out []journal.Event
	for ev, err := range h.jrn.Read(context.Background()) {
		if err != nil {
			h.t.Fatalf("reading the journal: %v", err)
		}
		out = append(out, ev)
	}
	return out
}

// types is the event sequence, as discriminators, for comparing a whole
// session's shape in one assertion.
func (h *harness) types() []journal.Type {
	h.t.Helper()
	evs := h.events()
	out := make([]journal.Type, len(evs))
	for i, ev := range evs {
		out[i] = ev.Type()
	}
	return out
}

// only returns the payloads of one type, in order.
func payloadsOf[T journal.Payload](t *testing.T, evs []journal.Event) []T {
	t.Helper()
	var out []T
	for _, ev := range evs {
		if p, ok := ev.Payload.(T); ok {
			out = append(out, p)
		}
	}
	return out
}

// sole returns the single payload of a type, failing when there is not exactly
// one. A test that meant "the" event and got two is a test asserting less than
// it reads.
func sole[T journal.Payload](t *testing.T, evs []journal.Event) T {
	t.Helper()
	got := payloadsOf[T](t, evs)
	if len(got) != 1 {
		var zero T
		t.Fatalf("want exactly one %T, got %d", zero, len(got))
	}
	return got[0]
}

// recordingPolicy answers every consent request the same way and remembers what
// it was asked.
type recordingPolicy struct {
	verdict  permission.Verdict
	source   permission.Source
	reason   string
	requests []permission.Request
}

func (p *recordingPolicy) Decide(_ context.Context, req permission.Request) (permission.Decision, error) {
	p.requests = append(p.requests, req)
	src := p.source
	if src == permission.SourceUnspecified {
		src = permission.SourceUser
	}
	return permission.Decision{Verdict: p.verdict, Source: src, Reason: p.reason}, nil
}

// stubSnapshotter stands in for repo.Snapshotter so the engine's "a turn
// touched files, so record the tree" path is driven without a git fixture.
type stubSnapshotter struct {
	turns []int
	err   error
}

func (s *stubSnapshotter) Snapshot(_ context.Context, turn int) (repo.Snapshot, error) {
	if s.err != nil {
		return repo.Snapshot{}, s.err
	}
	s.turns = append(s.turns, turn)
	return repo.Snapshot{
		Ref:    fmt.Sprintf("refs/kopicode/session-under-test/%d", turn),
		Commit: fmt.Sprintf("%040d", turn),
		Tree:   fmt.Sprintf("%040d", turn+100),
	}, nil
}

// --- hand-built provider traffic -------------------------------------------
//
// The shipped fixtures under internal/provider/fixture cover the three
// extraction routes on a two-turn session, and the tests that only need "a
// session that runs" use them. What they cannot cover is a reply that calls
// run_shell, or a reply that never stops, because a fixture is a recording of
// one session and these are the sessions the *loop's* bounds are about. Those
// bodies are built here, assembled rather than streamed: what a hand-built
// stream would add is coverage of the SSE decoder, which is
// internal/provider's subject and already driven by the shipped fixtures.

type wireUsage struct {
	Prompt     int `json:"prompt_tokens"`
	Completion int `json:"completion_tokens"`
	Total      int `json:"total_tokens"`
}

type wireFunction struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

type wireCall struct {
	ID       string       `json:"id"`
	Type     string       `json:"type"`
	Function wireFunction `json:"function"`
}

type wireMessage struct {
	Role      string     `json:"role"`
	Content   *string    `json:"content"`
	ToolCalls []wireCall `json:"tool_calls,omitempty"`
}

type wireChoice struct {
	Index        int         `json:"index"`
	Message      wireMessage `json:"message"`
	FinishReason string      `json:"finish_reason"`
}

type wireBody struct {
	ID       string       `json:"id"`
	Object   string       `json:"object"`
	Created  int64        `json:"created"`
	Model    string       `json:"model"`
	Provider string       `json:"provider"`
	Choices  []wireChoice `json:"choices"`
	Usage    wireUsage    `json:"usage"`
}

const (
	testModelID  = "test/model"
	testServedBy = "TestProvider"
)

var testPin = provider.Pin{Order: []string{"testprovider/bf16"}, AllowFallbacks: false, Quantizations: []string{"bf16"}}

// scriptedReply is one reply to hand back.
type scriptedReply struct {
	// text is the assistant's prose. A text-route call goes here.
	text string
	// calls are native tool calls. Args is written verbatim, so a test can
	// choose between the wire's JSON-string encoding and a bare object.
	calls []wireCall
	// usage is the token accounting the provider reports, which is the only
	// number the budget is allowed to be decided from.
	usage wireUsage
}

// nativeCall builds a native tool call whose arguments arrive as a bare object.
func nativeCall(id, name, args string) wireCall {
	return wireCall{ID: id, Type: "function", Function: wireFunction{Name: name, Arguments: json.RawMessage(args)}}
}

// script turns a sequence of replies into a fixture the mock provider serves.
//
// Turn and attempt are assigned by position with one attempt per turn unless
// repairs is set, which is what makes a fixture that expects a repair round trip
// expressible: the mock checks the loop's stated position against the recording
// and errors on a mismatch, so a loop that counted turns differently fails here
// rather than passing quietly.
func script(t *testing.T, replies []scriptedReply, positions [][2]int) *mock.Provider {
	t.Helper()
	if len(positions) != len(replies) {
		t.Fatalf("script: %d replies but %d positions", len(replies), len(positions))
	}

	fx := fixture.Fixture{
		FormatVersion: fixture.Version,
		Origin:        fixture.OriginHandAuthored,
		Name:          "engine_test_script",
		ModelID:       testModelID,
		Pin: fixture.Pin{
			Order:          testPin.Order,
			AllowFallbacks: testPin.AllowFallbacks,
			Quantizations:  testPin.Quantizations,
		},
	}

	for i, r := range replies {
		finish := "stop"
		if len(r.calls) > 0 {
			finish = "tool_calls"
		}
		var content *string
		if r.text != "" {
			text := r.text
			content = &text
		}
		body, err := json.Marshal(wireBody{
			ID:       fmt.Sprintf("gen-%04d", i),
			Object:   "chat.completion",
			Created:  1767312000,
			Model:    testModelID,
			Provider: testServedBy,
			Choices: []wireChoice{{
				Index:        0,
				Message:      wireMessage{Role: "assistant", Content: content, ToolCalls: r.calls},
				FinishReason: finish,
			}},
			Usage: r.usage,
		})
		if err != nil {
			t.Fatalf("script: encoding reply %d: %v", i, err)
		}
		fx.Exchanges = append(fx.Exchanges, fixture.Exchange{
			Turn:     positions[i][0],
			Attempt:  positions[i][1],
			Note:     fmt.Sprintf("engine_test reply %d", i),
			Response: fixture.Response{Status: 200, Body: body},
		})
	}
	return mock.New(fx)
}

// oneAttemptPerTurn is the ordinary position list: reply i answers turn i+1,
// attempt 1.
func oneAttemptPerTurn(n int) [][2]int {
	out := make([][2]int, n)
	for i := range out {
		out[i] = [2]int{i + 1, 1}
	}
	return out
}

// repeat is a script whose every reply is the same, for driving the turn cap.
func repeat(r scriptedReply, n int) []scriptedReply {
	out := make([]scriptedReply, n)
	for i := range out {
		out[i] = r
	}
	return out
}

// scriptHarness is the common setup for a hand-built script.
func scriptHarness(t *testing.T, replies []scriptedReply, positions [][2]int, files map[string]string, opts ...harnessOption) *harness {
	t.Helper()
	return newHarness(t, script(t, replies, positions), files, opts...)
}

// containsAll reports the first want not found in got, for a readable failure.
func containsAll(got string, want ...string) (string, bool) {
	for _, w := range want {
		if !strings.Contains(got, w) {
			return w, false
		}
	}
	return "", true
}
