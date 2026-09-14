package main

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/leejianrong/kopicode/internal/engine"
)

// This file is `kopicode serve` (ADR-0013), driven end to end over its own stdio
// protocol against a scripted stdio client — the same seam session_test.go and
// print_test.go use for the other two surfaces, with the client here standing in
// for the orchestrator that would spawn serve as a child. The comprehensive
// scripted-client suite, the cross-session concurrency proof, the internal/lock
// collision case, and the wire-protocol doc are KAN-1030/1031; what this file
// proves is KAN-1029's own scope: the three methods, the event tee, and the
// framing, each an observable outcome on the wire, never internal loop state.

// --- a scripted stdio client ------------------------------------------------

// serveHarness runs serve in a goroutine over in-memory pipes and collects every
// message it writes, so a test can send requests and read responses and
// notifications the way an orchestrator would.
type serveHarness struct {
	t       *testing.T
	inW     *io.PipeWriter
	exit    chan int
	decoded chan struct{} // closed when the output decoder has consumed every line

	mu  sync.Mutex
	all []map[string]any
}

func startServe(t *testing.T, base engine.Options) *serveHarness {
	t.Helper()
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	h := &serveHarness{t: t, inW: inW, exit: make(chan int, 1), decoded: make(chan struct{})}

	go func() {
		defer close(h.decoded)
		sc := bufio.NewScanner(outR)
		sc.Buffer(make([]byte, 0, 1<<16), 1<<20)
		for sc.Scan() {
			var m map[string]any
			if err := json.Unmarshal(sc.Bytes(), &m); err == nil {
				h.mu.Lock()
				h.all = append(h.all, m)
				h.mu.Unlock()
			}
		}
	}()
	go func() {
		code := serve(context.Background(), inR, outW, io.Discard, base)
		_ = outW.Close()
		h.exit <- code
	}()

	t.Cleanup(func() { _ = inW.Close() })
	return h
}

// send writes one JSON-RPC message as a line.
func (h *serveHarness) send(v any) {
	h.t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		h.t.Fatalf("marshalling a request: %v", err)
	}
	if _, err := h.inW.Write(append(b, '\n')); err != nil {
		h.t.Fatalf("writing to serve: %v", err)
	}
}

// awaitResponse polls the collected messages until the response to id arrives (a
// message carrying that id and no method), returning it.
func (h *serveHarness) awaitResponse(id int) map[string]any {
	h.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		h.mu.Lock()
		for _, m := range h.all {
			if m["method"] == nil && numEq(m["id"], id) {
				h.mu.Unlock()
				return m
			}
		}
		h.mu.Unlock()
		time.Sleep(5 * time.Millisecond)
	}
	h.t.Fatalf("timed out waiting for the response to id %d", id)
	return nil
}

// close ends the client's input and returns serve's exit code, after which every
// message it will ever send has been collected.
func (h *serveHarness) close() int {
	h.t.Helper()
	_ = h.inW.Close()
	select {
	case code := <-h.exit:
		<-h.decoded // serve has closed its output; wait for the decoder to drain it
		return code
	case <-time.After(10 * time.Second):
		h.t.Fatal("serve did not exit after its input closed")
		return -1
	}
}

// notificationsFor returns every session.event notification tagged with session.
func (h *serveHarness) notificationsFor(session string) []map[string]any {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []map[string]any
	for _, m := range h.all {
		if m["method"] != methodSessionEvent {
			continue
		}
		params, _ := m["params"].(map[string]any)
		if params != nil && params["session"] == session {
			out = append(out, m)
		}
	}
	return out
}

func numEq(v any, want int) bool {
	f, ok := v.(float64)
	return ok && int(f) == want
}

func eventKinds(notes []map[string]any) []string {
	var kinds []string
	for _, n := range notes {
		params, _ := n["params"].(map[string]any)
		ev, _ := params["event"].(map[string]any)
		if k, ok := ev["kind"].(string); ok {
			kinds = append(kinds, k)
		}
	}
	return kinds
}

func hasKind(kinds []string, want string) bool {
	return slices.Contains(kinds, want)
}

// --- a provider that blocks until the turn's context is cancelled -----------

// gatedProvider signals reached when a request arrives, then blocks that request
// until the client cancels it. It is how the cancel test makes a turn sit in the
// provider stream long enough for session.cancel to interrupt it deterministically.
func gatedProvider(t *testing.T) (*httptest.Server, <-chan struct{}) {
	t.Helper()
	reached := make(chan struct{}, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case reached <- struct{}{}:
		default:
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		<-r.Context().Done() // the client cancels the turn; unblock and end the response
	}))
	t.Cleanup(srv.Close)
	return srv, reached
}

// --- the tests --------------------------------------------------------------

// TestServeStartRunsATurnAndTeesEvents is session.start's headline: a session
// opens on a working tree, its first turn runs to a clean stop, and the record's
// events reach the client as session.event notifications tagged with the session
// id — the tee ADR-0013 decision 3 names.
func TestServeStartRunsATurnAndTeesEvents(t *testing.T) {
	t.Setenv(engine.APIKeyEnv, "kopicode-test-credential")
	srv := scriptedProvider(t, sseBody("the fix is in"))
	h := startServe(t, engine.Options{ProviderBaseURL: srv.URL})

	h.send(map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": methodSessionStart,
		"params": map[string]any{"session": "s1", "dir": t.TempDir(), "prompt": "why does it fail?"},
	})
	resp := h.awaitResponse(1)

	if resp["error"] != nil {
		t.Fatalf("session.start errored: %v", resp["error"])
	}
	result, _ := resp["result"].(map[string]any)
	if result == nil {
		t.Fatalf("session.start has no result: %v", resp)
	}
	if result["session"] != "s1" {
		t.Errorf("result session = %v, want s1", result["session"])
	}
	if result["stop"] != "completed" {
		t.Errorf("result stop = %v, want completed", result["stop"])
	}
	if !numEq(result["exit_code"], 0) {
		t.Errorf("result exit_code = %v, want 0", result["exit_code"])
	}
	if result["record"] == nil || result["record"] == "" {
		t.Error("result record is empty; start opened the journal and should say where")
	}

	kinds := eventKinds(h.notificationsFor("s1"))
	if !hasKind(kinds, "session_started") {
		t.Errorf("notifications %v carry no session_started; the event tee is not wired", kinds)
	}
	if !hasKind(kinds, "assistant_message") {
		t.Errorf("notifications %v carry no assistant_message; the turn's own events are not teed", kinds)
	}

	if code := h.close(); code != exitSuccess {
		t.Errorf("serve exit = %d, want %d", code, exitSuccess)
	}
	// SessionEnded is written at shutdown, so it lands only after close.
	if !hasKind(eventKinds(h.notificationsFor("s1")), "session_ended") {
		t.Error("no session_ended notification after shutdown; a closed session owes its record that bookend")
	}
}

// TestServeSubmitContinuesAnOpenSession proves the resident property: one process
// runs a second turn on the same open session via session.submit, with no second
// engine.Open and no restart. The record path belongs to start alone.
func TestServeSubmitContinuesAnOpenSession(t *testing.T) {
	t.Setenv(engine.APIKeyEnv, "kopicode-test-credential")
	srv := scriptedProvider(t, sseBody("first answer"), sseBody("second answer"))
	h := startServe(t, engine.Options{ProviderBaseURL: srv.URL})

	dir := t.TempDir()
	h.send(map[string]any{"jsonrpc": "2.0", "id": 1, "method": methodSessionStart,
		"params": map[string]any{"session": "s1", "dir": dir, "prompt": "first"}})
	start := h.awaitResponse(1)
	if start["error"] != nil {
		t.Fatalf("session.start errored: %v", start["error"])
	}

	h.send(map[string]any{"jsonrpc": "2.0", "id": 2, "method": methodSessionSubmit,
		"params": map[string]any{"session": "s1", "prompt": "second"}})
	submit := h.awaitResponse(2)
	if submit["error"] != nil {
		t.Fatalf("session.submit errored: %v", submit["error"])
	}
	result, _ := submit["result"].(map[string]any)
	if result["session"] != "s1" {
		t.Errorf("submit session = %v, want s1", result["session"])
	}
	if result["stop"] != "completed" {
		t.Errorf("submit stop = %v, want completed", result["stop"])
	}
	if result["record"] != nil {
		t.Errorf("submit result carries a record %v; only start opens the journal", result["record"])
	}

	if code := h.close(); code != exitSuccess {
		t.Errorf("serve exit = %d, want %d", code, exitSuccess)
	}
}

// TestServeCancelInterruptsAnInFlightTurn drives ADR-0013 decision 3's cancel:
// with a turn parked in the provider stream, session.cancel cancels its context —
// the REPL's Ctrl-C mechanism — and the turn settles on StopCancelled while the
// cancel itself is acknowledged.
func TestServeCancelInterruptsAnInFlightTurn(t *testing.T) {
	t.Setenv(engine.APIKeyEnv, "kopicode-test-credential")
	srv, reached := gatedProvider(t)
	h := startServe(t, engine.Options{ProviderBaseURL: srv.URL})

	h.send(map[string]any{"jsonrpc": "2.0", "id": 1, "method": methodSessionStart,
		"params": map[string]any{"session": "s1", "dir": t.TempDir(), "prompt": "loop forever"}})

	select {
	case <-reached:
	case <-time.After(10 * time.Second):
		t.Fatal("the turn never reached the provider; nothing to cancel")
	}

	h.send(map[string]any{"jsonrpc": "2.0", "id": 2, "method": methodSessionCancel,
		"params": map[string]any{"session": "s1"}})
	cancelResp := h.awaitResponse(2)
	if cancelResp["error"] != nil {
		t.Fatalf("session.cancel errored: %v", cancelResp["error"])
	}
	cancelRes, _ := cancelResp["result"].(map[string]any)
	if cancelRes["cancelled"] != true {
		t.Errorf("cancel result = %v, want cancelled true", cancelRes)
	}

	start := h.awaitResponse(1)
	result, _ := start["result"].(map[string]any)
	if result == nil {
		t.Fatalf("session.start did not return a result after cancel: %v", start)
	}
	if result["stop"] != "cancelled" {
		t.Errorf("start stop = %v, want cancelled", result["stop"])
	}

	h.close()
}

// TestServeRejectsAnUnknownMethod: a method this surface does not have is a
// method-not-found error, not a silent drop.
func TestServeRejectsAnUnknownMethod(t *testing.T) {
	h := startServe(t, engine.Options{})
	h.send(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "session.explode"})
	resp := h.awaitResponse(1)
	rpcErr, _ := resp["error"].(map[string]any)
	if rpcErr == nil {
		t.Fatalf("unknown method got no error: %v", resp)
	}
	if !numEq(rpcErr["code"], codeMethodNotFound) {
		t.Errorf("error code = %v, want %d", rpcErr["code"], codeMethodNotFound)
	}
	h.close()
}

// TestServeRejectsSubmitToAnUnknownSession: submit before any matching start is
// an unknown-session error naming the id.
func TestServeRejectsSubmitToAnUnknownSession(t *testing.T) {
	h := startServe(t, engine.Options{})
	h.send(map[string]any{"jsonrpc": "2.0", "id": 1, "method": methodSessionSubmit,
		"params": map[string]any{"session": "ghost", "prompt": "hello"}})
	resp := h.awaitResponse(1)
	rpcErr, _ := resp["error"].(map[string]any)
	if rpcErr == nil {
		t.Fatalf("submit to an unknown session got no error: %v", resp)
	}
	if !numEq(rpcErr["code"], codeUnknownSession) {
		t.Errorf("error code = %v, want %d", rpcErr["code"], codeUnknownSession)
	}
	h.close()
}

// TestServeRejectsAReusedSessionId: a second start with a live id is refused,
// not silently attached to the running session.
func TestServeRejectsAReusedSessionId(t *testing.T) {
	t.Setenv(engine.APIKeyEnv, "kopicode-test-credential")
	srv, reached := gatedProvider(t)
	h := startServe(t, engine.Options{ProviderBaseURL: srv.URL})

	h.send(map[string]any{"jsonrpc": "2.0", "id": 1, "method": methodSessionStart,
		"params": map[string]any{"session": "s1", "dir": t.TempDir(), "prompt": "run"}})
	<-reached // s1 is registered and its turn is in flight

	h.send(map[string]any{"jsonrpc": "2.0", "id": 2, "method": methodSessionStart,
		"params": map[string]any{"session": "s1", "dir": t.TempDir(), "prompt": "again"}})
	resp := h.awaitResponse(2)
	rpcErr, _ := resp["error"].(map[string]any)
	if rpcErr == nil {
		t.Fatalf("a reused session id got no error: %v", resp)
	}
	if !numEq(rpcErr["code"], codeSessionExists) {
		t.Errorf("error code = %v, want %d", rpcErr["code"], codeSessionExists)
	}

	// Free the first turn so shutdown is clean.
	h.send(map[string]any{"jsonrpc": "2.0", "id": 3, "method": methodSessionCancel,
		"params": map[string]any{"session": "s1"}})
	h.awaitResponse(3)
	h.close()
}

// TestServeReportsAParseError: a line that is not JSON gets a parse-error
// response with a null id, and the loop keeps going.
func TestServeReportsAParseError(t *testing.T) {
	h := startServe(t, engine.Options{})
	if _, err := h.inW.Write([]byte("{not json}\n")); err != nil {
		t.Fatalf("writing the bad line: %v", err)
	}
	// A well-formed request after the bad line still gets answered — the parse
	// error did not end the loop.
	h.send(map[string]any{"jsonrpc": "2.0", "id": 7, "method": "session.explode"})
	resp := h.awaitResponse(7)
	if resp["error"] == nil {
		t.Fatalf("the loop stopped after a parse error: id 7 went unanswered")
	}

	h.mu.Lock()
	defer h.mu.Unlock()
	var sawParse bool
	for _, m := range h.all {
		if e, ok := m["error"].(map[string]any); ok && numEq(e["code"], codeParseError) {
			sawParse = true
		}
	}
	if !sawParse {
		t.Error("no parse-error response was emitted for the malformed line")
	}
	h.inW.Close()
}
