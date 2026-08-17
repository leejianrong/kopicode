package bench

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/leejianrong/kopicode/internal/corpus"
	"github.com/leejianrong/kopicode/internal/engine"
	"github.com/leejianrong/kopicode/internal/journal"
	"github.com/leejianrong/kopicode/internal/provider/fixture"
	"github.com/leejianrong/kopicode/internal/provider/mock"
)

// TestEngineAgentRunShellSeesTheTaskTempHOME is KAN-874's acceptance
// criterion, exercised end to end through the same wiring the runner uses.
//
// Before this card, tools.NewSet(spec.Dir) had no seam for the per-task temp
// HOME the runner already gives the oracle (see oracleEnv in oracle.go), so
// model-authored run_shell fell through to childEnv's ambient
// os.Environ() — the operator's real HOME — while the oracle that scores the
// same task ran against a clean one. A task could pass only because its shell
// reached the operator's real ~/.gitconfig or toolchain caches, which will not
// reproduce on another machine.
//
// This drives EngineAgent.runSession — Run's body once a provider is chosen —
// with a hand-built mock.Provider that scripts one run_shell call reading
// $HOME, followed by a clean stop, and reads the recorded tool output back out
// of the journal. It uses runSession rather than Run because Run's provider
// selection only knows ProviderLive and ProviderMock, and ProviderMock always
// loads one of the two shipped fixtures — neither of which calls run_shell.
func TestEngineAgentRunShellSeesTheTaskTempHOME(t *testing.T) {
	// No go.mod, no Makefile: forced verification (internal/verify) discovers
	// nothing to run, which keeps this test about run_shell's environment
	// rather than about a toolchain command this fixture never claims to have
	// satisfied.
	dir := t.TempDir()

	// The ambient HOME a leaked fix would have let the child see, and the
	// per-task one the runner is supposed to give it instead. Both are real
	// temp directories, and distinct, so a test that passed by accident —
	// t.TempDir() colliding with itself — cannot happen quietly.
	realHome := t.TempDir()
	t.Setenv("HOME", realHome)
	taskHome := t.TempDir()
	if taskHome == realHome {
		t.Fatal("t.TempDir() returned the same path twice; the test proves nothing")
	}

	sel, err := engine.ResolveSelection(t.TempDir(), engine.SelectionOverrides{})
	if err != nil {
		t.Fatalf("resolving the default selection: %v", err)
	}

	prov := scriptOneShellCallThenStop(t, sel, `echo "home=[$HOME]"`)

	outDir := t.TempDir()
	const sessionID = "kan874-sess"
	agent := EngineAgent{}
	out, err := agent.runSession(t.Context(), prov, SessionSpec{
		Task:      corpus.Task{ID: "kan874", Statement: "read $HOME and report it"},
		Dir:       dir,
		Home:      taskHome,
		OutDir:    outDir,
		SessionID: sessionID,
		Selection: sel,
	})
	if err != nil {
		t.Fatalf("runSession: %v", err)
	}
	if out.Stop != engine.StopCompleted.Reason() {
		t.Fatalf("stop = %q, want %q — the script did not run the way this test assumes", out.Stop, engine.StopCompleted.Reason())
	}

	text := readJournalText(t, outDir, sessionID)
	if !strings.Contains(text, "home=["+taskHome+"]") {
		t.Errorf("the journal does not show run_shell seeing the task's temp HOME %s:\n%s", taskHome, text)
	}
	if strings.Contains(text, "home=["+realHome+"]") {
		t.Errorf("run_shell saw the operator's real HOME %s instead of the task's temp one — "+
			"tools.Set.Home was not wired through", realHome)
	}
}

// readJournalText reads the whole of one session's journal back, the way
// smoke_integration_test.go does for the same reason: the record outlives
// whatever produced it, and this is the only place that matters for a test
// that needs to see what run_shell actually reported.
func readJournalText(t *testing.T, outDir, sessionID string) string {
	t.Helper()
	path := filepath.Join(journal.SessionDir(outDir, sessionID), journal.EventsFile)
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading the journal at %s: %v", path, err)
	}
	return string(b)
}

// The wire shapes below are a deliberately small, local copy of the ones
// internal/engine/harness_test.go builds scripted replies from. They cannot be
// shared: that file's types are unexported in package engine_test, and this
// package must not import internal/engine's tests to get them. Only what one
// native run_shell tool call and one clean stop need is reproduced.

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
}

// scriptOneShellCallThenStop builds a two-turn mock.Provider: turn 1 is a
// native run_shell call for command, turn 2 is a clean prose stop that asks
// for no tool. mock.New rather than mock.Load, because this fixture exists
// only to drive one assertion and does not belong in the shipped, validated
// set under internal/provider/fixture/data.
func scriptOneShellCallThenStop(t *testing.T, sel engine.Selection, command string) *mock.Provider {
	t.Helper()

	args, err := json.Marshal(map[string]string{"command": command})
	if err != nil {
		t.Fatalf("encoding run_shell arguments: %v", err)
	}
	shellBody, err := json.Marshal(wireBody{
		ID: "gen-0001", Object: "chat.completion", Created: 1767312000,
		Model: sel.ModelID, Provider: "TestProvider",
		Choices: []wireChoice{{
			Message: wireMessage{
				Role: "assistant",
				ToolCalls: []wireCall{{
					ID: "call_1", Type: "function",
					Function: wireFunction{Name: "run_shell", Arguments: args},
				}},
			},
			FinishReason: "tool_calls",
		}},
	})
	if err != nil {
		t.Fatalf("encoding the run_shell reply: %v", err)
	}

	done := "the child saw its HOME; nothing left to do"
	stopBody, err := json.Marshal(wireBody{
		ID: "gen-0002", Object: "chat.completion", Created: 1767312001,
		Model: sel.ModelID, Provider: "TestProvider",
		Choices: []wireChoice{{
			Message:      wireMessage{Role: "assistant", Content: &done},
			FinishReason: "stop",
		}},
	})
	if err != nil {
		t.Fatalf("encoding the stop reply: %v", err)
	}

	fx := fixture.Fixture{
		FormatVersion: fixture.Version,
		Origin:        fixture.OriginHandAuthored,
		OriginNote: "written for TestEngineAgentRunShellSeesTheTaskTempHOME (KAN-874); " +
			"models nothing about real provider traffic beyond the two-turn native tool-call shape " +
			"internal/provider/fixture/data/two_turn_native_tool_call.json already carries",
		Name:    "kan874_bench_run_shell_home",
		ModelID: sel.ModelID,
		Pin: fixture.Pin{
			Order:          sel.Pin.Order,
			AllowFallbacks: sel.Pin.AllowFallbacks,
			Quantizations:  sel.Pin.Quantizations,
		},
		Exchanges: []fixture.Exchange{
			{Turn: 1, Attempt: 1, Note: "run_shell reads $HOME", Response: fixture.Response{Status: 200, Body: shellBody}},
			{Turn: 2, Attempt: 1, Note: "clean stop", Response: fixture.Response{Status: 200, Body: stopBody}},
		},
	}
	return mock.New(fx)
}
