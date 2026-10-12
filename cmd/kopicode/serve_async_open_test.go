package main

import (
	"testing"

	"github.com/leejianrong/kopicode/internal/engine"
)

// TestServeSubmitSentRightAfterStartQueuesBehindTheOpen is the ordering rule
// KAN-2015 needs: engine.Open runs off the read loop, so a submit that follows a
// start on the next line reaches the manager before the session is open. It must
// find the session and run after the start's own turn, not be refused as unknown.
func TestServeSubmitSentRightAfterStartQueuesBehindTheOpen(t *testing.T) {
	t.Setenv(engine.APIKeyEnv, "kopicode-test-credential")
	srv := scriptedProvider(t, sseBody("first answer"), sseBody("second answer"))
	h := startServe(t, engine.Options{ProviderBaseURL: srv.URL})

	h.send(map[string]any{"jsonrpc": "2.0", "id": 1, "method": methodSessionStart,
		"params": startPayload("s1", t.TempDir(), "first")})
	h.send(map[string]any{"jsonrpc": "2.0", "id": 2, "method": methodSessionSubmit,
		"params": map[string]any{"session": "s1", "prompt": "second"}})

	start, submit := h.awaitResponse(1), h.awaitResponse(2)
	if start["error"] != nil || submit["error"] != nil {
		t.Fatalf("start error %v, submit error %v; a submit right after a start must queue behind it", start["error"], submit["error"])
	}
	startRes, _ := start["result"].(map[string]any)
	submitRes, _ := submit["result"].(map[string]any)
	if startRes["record"] == nil || submitRes["record"] != nil {
		t.Errorf("record on start %v and on submit %v: the start must be the opening turn", startRes["record"], submitRes["record"])
	}
	h.close()
}

// TestServeFailedOpenAnswersEveryQueuedRequestAndFreesTheId: when the open that
// runs off the read loop fails, the start and a submit already queued behind it
// each get an error response (neither is dropped, so no client waits forever),
// and the id can be started again.
func TestServeFailedOpenAnswersEveryQueuedRequestAndFreesTheId(t *testing.T) {
	t.Setenv(engine.APIKeyEnv, "kopicode-test-credential")
	srv := scriptedProvider(t, sseBody("done"), sseBody("again"))
	h := startServe(t, engine.Options{ProviderBaseURL: srv.URL})

	dir := t.TempDir()
	h.send(map[string]any{"jsonrpc": "2.0", "id": 1, "method": methodSessionStart,
		"params": startPayload("s1", dir, "first")})
	if r := h.awaitResponse(1); r["error"] != nil {
		t.Fatalf("the first session.start errored: %v", r["error"])
	}

	// s1 holds dir's lock, so s2's open fails; the submit follows immediately.
	h.send(map[string]any{"jsonrpc": "2.0", "id": 2, "method": methodSessionStart,
		"params": startPayload("s2", dir, "second")})
	h.send(map[string]any{"jsonrpc": "2.0", "id": 3, "method": methodSessionSubmit,
		"params": map[string]any{"session": "s2", "prompt": "third"}})
	for _, id := range []int{2, 3} {
		resp := h.awaitResponse(id)
		rpcErr, _ := resp["error"].(map[string]any)
		if rpcErr == nil {
			t.Fatalf("request %d on a session that never opened got no error: %v", id, resp)
		}
	}

	if rows := sessionsOf(t, h, 4); rows["s2"] != nil {
		t.Errorf("s2 is still listed after its open failed: %v", rows["s2"])
	}

	// Free the tree, and the id works again.
	h.send(map[string]any{"jsonrpc": "2.0", "id": 5, "method": methodSessionClose, "params": map[string]any{"session": "s1"}})
	if r := h.awaitResponse(5); r["error"] != nil {
		t.Fatalf("session.close: %v", r["error"])
	}
	h.send(map[string]any{"jsonrpc": "2.0", "id": 6, "method": methodSessionStart,
		"params": startPayload("s2", dir, "retry")})
	if r := h.awaitResponse(6); r["error"] != nil {
		t.Errorf("retrying s2 once the tree is free: %v", r["error"])
	}
	h.close()
}

// TestServeSessionsAnswersForSessionsStartedBackToBack: server.sessions never
// waits on an open, and a session mid-open is listed (as running) rather than
// missing, so a client that just started it can see it.
func TestServeSessionsAnswersForSessionsStartedBackToBack(t *testing.T) {
	t.Setenv(engine.APIKeyEnv, "kopicode-test-credential")
	srv := scriptedProvider(t, sseBody("a"), sseBody("b"), sseBody("c"))
	h := startServe(t, engine.Options{ProviderBaseURL: srv.URL})

	ids := []string{"s1", "s2", "s3"}
	for i, id := range ids {
		h.send(map[string]any{"jsonrpc": "2.0", "id": i + 1, "method": methodSessionStart,
			"params": startPayload(id, t.TempDir(), "go")})
	}
	rows := sessionsOf(t, h, 10)
	for _, id := range ids {
		if rows[id] == nil {
			t.Errorf("%s is not listed right after its start was sent", id)
		}
	}
	for i := range ids {
		if r := h.awaitResponse(i + 1); r["error"] != nil {
			t.Errorf("start %d: %v", i+1, r["error"])
		}
	}
	h.close()
}
