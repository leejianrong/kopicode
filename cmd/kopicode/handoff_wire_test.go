package main

import (
	"os"
	"strings"
	"testing"

	"github.com/leejianrong/kopicode/internal/engine"
)

// narrative is what the model "writes" when asked for a handoff.
const wireNarrative = "## Goal\n\nfinish the parser\n\n## Next step\n\nrun go test\n"

func TestServeHandoffDraftsADocumentAndAnotherSessionStartsFromIt(t *testing.T) {
	t.Setenv(engine.APIKeyEnv, "kopicode-test-credential")
	srv := scriptedProvider(t, sseBody("did some work"), sseBody(wireNarrative), sseBody("carrying on"))
	h := startServe(t, engine.Options{ProviderBaseURL: srv.URL})

	dir := t.TempDir()
	h.send(map[string]any{"jsonrpc": "2.0", "id": 1, "method": methodSessionStart,
		"params": startPayload("s1", dir, "work on the parser")})
	if r := h.awaitResponse(1); r["error"] != nil {
		t.Fatalf("session.start: %v", r["error"])
	}

	h.send(map[string]any{"jsonrpc": "2.0", "id": 2, "method": methodSessionHandoff,
		"params": map[string]any{"session": "s1", "goal": "ship it"}})
	resp := h.awaitResponse(2)
	if resp["error"] != nil {
		t.Fatalf("session.handoff: %v", resp["error"])
	}
	res, _ := resp["result"].(map[string]any)
	doc, _ := res["document"].(string)
	if res["session"] != "s1" || res["goal"] != "ship it" || !strings.Contains(doc, "finish the parser") ||
		!strings.Contains(doc, "## Recorded by kopicode") {
		t.Fatalf("result = %v", res)
	}
	path, _ := res["path"].(string)
	if got, err := os.ReadFile(path); err != nil || string(got) != doc {
		t.Fatalf("the projection file does not hold the document (%v)", err)
	}

	// The session is as it was: it still takes a turn, and it was not ended.
	h.send(map[string]any{"jsonrpc": "2.0", "id": 3, "method": methodSessionSubmit,
		"params": map[string]any{"session": "s1", "prompt": "carry on"}})
	if r := h.awaitResponse(3); r["error"] != nil {
		t.Fatalf("session.submit after a handoff: %v", r["error"])
	}

	// A second session, in another tree, starts from the document.
	h.send(map[string]any{"jsonrpc": "2.0", "id": 4, "method": methodSessionStart,
		"params": startPayload("s2", t.TempDir(), "go on", map[string]any{"handoff": doc, "handoff_from": "s1"})})
	start := h.awaitResponse(4)
	if start["error"] != nil {
		t.Fatalf("session.start with a handoff: %v", start["error"])
	}
	record, _ := start["result"].(map[string]any)["record"].(string)
	raw, err := os.ReadFile(record + "/events.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"parent_session":"s1"`, `"scope":"handoff"`, "finish the parser"} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("the new session's journal lacks %s", want)
		}
	}
	h.close()
}

func TestServeHandoffRefusals(t *testing.T) {
	t.Setenv(engine.APIKeyEnv, "kopicode-test-credential")
	srv := scriptedProvider(t, sseBody("unused"))
	h := startServe(t, engine.Options{ProviderBaseURL: srv.URL})

	h.send(map[string]any{"jsonrpc": "2.0", "id": 1, "method": methodSessionHandoff, "params": map[string]any{}})
	if e, _ := h.awaitResponse(1)["error"].(map[string]any); e == nil || !numEq(e["code"], codeInvalidParams) {
		t.Errorf("no session id: error = %v", e)
	}
	h.send(map[string]any{"jsonrpc": "2.0", "id": 2, "method": methodSessionHandoff, "params": map[string]any{"session": "nope"}})
	if e, _ := h.awaitResponse(2)["error"].(map[string]any); e == nil || !numEq(e["code"], codeUnknownSession) {
		t.Errorf("unknown session: error = %v", e)
	}
	h.close()
}

func TestMCPHandoffToolDraftsAndStartTakesItBack(t *testing.T) {
	t.Setenv(engine.APIKeyEnv, "kopicode-test-credential")
	srv := scriptedProvider(t, sseBody("did some work"), sseBody(wireNarrative), sseBody("carrying on"))
	h := startMCP(t, engine.Options{ProviderBaseURL: srv.URL})
	initialize(h, 1, false)

	callTool(h, 2, toolStart, map[string]any{"dir": t.TempDir(), "prompt": "work", "consent_mode": "auto", "session": "a"}, nil)
	if _, isErr := toolOutcome(t, h.awaitResponse(2)); isErr {
		t.Fatal("start failed")
	}
	callTool(h, 3, toolHandoff, map[string]any{"session": "a", "goal": "ship it"}, nil)
	out, isErr := toolOutcome(t, h.awaitResponse(3))
	doc, _ := out["document"].(string)
	if isErr || !strings.Contains(doc, "finish the parser") || out["path"] == "" {
		t.Fatalf("handoff outcome = %v (error %v)", out, isErr)
	}

	callTool(h, 4, toolStart, map[string]any{"dir": t.TempDir(), "prompt": "go on", "consent_mode": "auto",
		"session": "b", "handoff": doc, "handoff_from": "a"}, nil)
	start, isErr := toolOutcome(t, h.awaitResponse(4))
	if isErr {
		t.Fatalf("start from a handoff failed: %v", start)
	}
	raw, err := os.ReadFile(start["record"].(string) + "/events.jsonl")
	if err != nil || !strings.Contains(string(raw), `"parent_session":"a"`) || !strings.Contains(string(raw), `"scope":"handoff"`) {
		t.Errorf("the new session's journal does not record the handoff (%v)", err)
	}

	callTool(h, 5, toolHandoff, map[string]any{"session": "nope"}, nil)
	if _, isErr := toolOutcome(t, h.awaitResponse(5)); !isErr {
		t.Error("a handoff for an unknown session succeeded")
	}
	h.close()
}
