package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/leejianrong/kopicode/internal/engine"
)

// capturingProvider answers every request with body and keeps what it was sent.
func capturingProvider(t *testing.T, body string) (*httptest.Server, func() []string) {
	t.Helper()
	var mu sync.Mutex
	var reqs []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		mu.Lock()
		reqs = append(reqs, string(raw))
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), reqs...)
	}
}

func startRPC(h *serveHarness, id int, params map[string]any) map[string]any {
	h.send(map[string]any{"jsonrpc": "2.0", "id": id, "method": methodSessionStart, "params": params})
	return h.awaitResponse(id)
}

func closeRPC(h *serveHarness, id int, session string) {
	h.send(map[string]any{"jsonrpc": "2.0", "id": id, "method": methodSessionClose, "params": map[string]any{"session": session}})
	h.awaitResponse(id)
}

func rpcErrCode(resp map[string]any) (int, bool) {
	e, _ := resp["error"].(map[string]any)
	f, ok := e["code"].(float64)
	return int(f), ok
}

// A resumed session continues the earlier one's history on the same journal,
// under a client key of its own choosing.
func TestServeResumeContinuesAnEarlierSession(t *testing.T) {
	t.Setenv(engine.APIKeyEnv, "kopicode-test-credential")
	srv, requests := capturingProvider(t, sseBody("ok"))
	h := startServe(t, engine.Options{ProviderBaseURL: srv.URL})
	dir := t.TempDir()

	first := startRPC(h, 1, startPayload("s1", dir, "remember the word kumquat"))
	if first["error"] != nil {
		t.Fatalf("first start: %v", first["error"])
	}
	record1, _ := first["result"].(map[string]any)["record"].(string)
	closeRPC(h, 2, "s1")

	second := startRPC(h, 3, startPayload("round-2", dir, "what was the word?", map[string]any{"resume": "s1"}))
	if second["error"] != nil {
		t.Fatalf("resume: %v", second["error"])
	}
	res, _ := second["result"].(map[string]any)
	if res["session"] != "round-2" || res["record"] != record1 {
		t.Errorf("result = %v, want the client's key round-2 on the journal %s", res, record1)
	}
	reqs := requests()
	if len(reqs) != 2 || !strings.Contains(reqs[1], "kumquat") || !strings.Contains(reqs[1], "what was the word?") {
		t.Errorf("the resumed turn's request did not carry the earlier history: %d requests", len(reqs))
	}
}

// An id with no record is a usage error and opens nothing, so the key is free.
func TestServeResumeOfAnUnknownIDIsAUsageError(t *testing.T) {
	t.Setenv(engine.APIKeyEnv, "kopicode-test-credential")
	srv, requests := capturingProvider(t, sseBody("ok"))
	h := startServe(t, engine.Options{ProviderBaseURL: srv.URL})
	dir := t.TempDir()

	resp := startRPC(h, 1, startPayload("s1", dir, "go", map[string]any{"resume": "no-such-session"}))
	if code, ok := rpcErrCode(resp); !ok || code != codeUsageError {
		t.Fatalf("response = %v, want error %d", resp, codeUsageError)
	}
	if n := len(requests()); n != 0 {
		t.Errorf("%d provider requests for a session that should not have opened", n)
	}
	if ok := startRPC(h, 2, startPayload("s1", dir, "go")); ok["error"] != nil {
		t.Errorf("the key was not freed: %v", ok["error"])
	}
}

func TestServeResumeCannotBeCombinedWithHandoff(t *testing.T) {
	t.Setenv(engine.APIKeyEnv, "kopicode-test-credential")
	srv, _ := capturingProvider(t, sseBody("ok"))
	h := startServe(t, engine.Options{ProviderBaseURL: srv.URL})

	resp := startRPC(h, 1, startPayload("s1", t.TempDir(), "go", map[string]any{"resume": "x", "handoff": "doc"}))
	if code, ok := rpcErrCode(resp); !ok || code != codeUsageError {
		t.Fatalf("response = %v, want error %d", resp, codeUsageError)
	}
}

// The per-tree lock still holds: the earlier session is still open, so
// resuming it under another key is refused.
func TestServeResumeOfASessionStillOpenIsLocked(t *testing.T) {
	t.Setenv(engine.APIKeyEnv, "kopicode-test-credential")
	srv, _ := capturingProvider(t, sseBody("ok"))
	h := startServe(t, engine.Options{ProviderBaseURL: srv.URL})
	dir := t.TempDir()

	if r := startRPC(h, 1, startPayload("s1", dir, "go")); r["error"] != nil {
		t.Fatalf("start: %v", r["error"])
	}
	resp := startRPC(h, 2, startPayload("s2", dir, "again", map[string]any{"resume": "s1"}))
	if code, ok := rpcErrCode(resp); !ok || code != codeSessionLocked {
		t.Fatalf("response = %v, want error %d", resp, codeSessionLocked)
	}
}
