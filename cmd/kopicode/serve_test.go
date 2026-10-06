package main

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
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
	return startServeWith(t, base, remoteConsentTimeout)
}

// startServeWith is startServe with the live-consent timeout stated, the seam
// --consent-timeout reaches serve through.
func startServeWith(t *testing.T, base engine.Options, consentTimeout time.Duration) *serveHarness {
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
		code := serveWith(context.Background(), inR, outW, io.Discard, base, consentTimeout)
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

// awaitRequest polls the collected messages until a server-initiated request
// for method arrives — one carrying that method and an id, which is what tells
// it apart from a notification (session.event carries a method but no id).
// Returns the whole message so a caller can read its params and echo its id
// back in a reply (ADR-0016's consent.request is this surface's first and, for
// now, only such request).
func (h *serveHarness) awaitRequest(method string) map[string]any {
	h.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		h.mu.Lock()
		for _, m := range h.all {
			if m["method"] == method && m["id"] != nil {
				h.mu.Unlock()
				return m
			}
		}
		h.mu.Unlock()
		time.Sleep(5 * time.Millisecond)
	}
	h.t.Fatalf("timed out waiting for a %s request", method)
	return nil
}

// sawRequest reports whether a request carrying method and an id has arrived so
// far, without waiting for one. awaitRequest is the waiting form.
func (h *serveHarness) sawRequest(method string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, m := range h.all {
		if m["method"] == method && m["id"] != nil {
			return true
		}
	}
	return false
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

// startPayload builds session.start's params with ADR-0016's now-required
// consent_mode. "unattended_policy" plus containment_provided is every
// existing test's actual behaviour from before that field existed — Consent
// stays nil, ConsentMode is engine.ConsentUnattended, and base's Policy/
// AskPolicy (or the fixed unattended-deny default) governs exactly as before —
// so this is the default every test but the remote_interactive-specific ones
// below should use. Extra params (model, harness, ...) merge in on top.
func startPayload(session, dir, prompt string, extra ...map[string]any) map[string]any {
	p := map[string]any{
		"session": session, "dir": dir, "prompt": prompt,
		"consent_mode": consentModeUnattendedPolicy, "containment_provided": true,
	}
	for _, e := range extra {
		for k, v := range e {
			p[k] = v
		}
	}
	return p
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

// concurrentProbeProvider signals entered as every request arrives — never
// dropping a signal, unlike gatedProvider's buffered-1 reached — and blocks each
// request until the client cancels its turn. It is how the concurrency test
// proves how many turns sit in the provider at once: two sessions parked here
// together is two turns running concurrently.
func concurrentProbeProvider(t *testing.T) (*httptest.Server, <-chan struct{}) {
	t.Helper()
	entered := make(chan struct{}, 16)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		entered <- struct{}{}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		<-r.Context().Done() // the turn is cancelled; unblock and end the response
	}))
	t.Cleanup(srv.Close)
	return srv, entered
}

// serializingProvider signals entered per request, then blocks it until the test
// sends on proceed, after which it streams a clean completion so the turn ends
// StopCompleted. It tracks the peak number of requests in flight at once, which
// a single-worker-per-session queue holds at 1 for one session's turns however
// many are submitted.
func serializingProvider(t *testing.T) (srv *httptest.Server, entered <-chan struct{}, proceed chan<- struct{}, peak *int32) {
	t.Helper()
	enteredCh := make(chan struct{}, 16)
	proceedCh := make(chan struct{})
	var inflight, maxInflight int32
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&inflight, 1)
		for {
			old := atomic.LoadInt32(&maxInflight)
			if n <= old || atomic.CompareAndSwapInt32(&maxInflight, old, n) {
				break
			}
		}
		enteredCh <- struct{}{}
		select {
		case <-proceedCh:
		case <-r.Context().Done():
		}
		atomic.AddInt32(&inflight, -1)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(sseBody("done")))
	}))
	t.Cleanup(srv.Close)
	return srv, enteredCh, proceedCh, &maxInflight
}

// awaitEntered receives one entered signal or fails the test — a turn that never
// reached the provider is a hang, not a pass.
func awaitEntered(t *testing.T, entered <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		t.Fatalf("%s never reached the provider", what)
	}
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
		"params": startPayload("s1", t.TempDir(), "why does it fail?"),
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
		"params": startPayload("s1", dir, "first")})
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
		"params": startPayload("s1", t.TempDir(), "loop forever")})

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
		"params": startPayload("s1", t.TempDir(), "run")})
	<-reached // s1 is registered and its turn is in flight

	h.send(map[string]any{"jsonrpc": "2.0", "id": 2, "method": methodSessionStart,
		"params": startPayload("s1", t.TempDir(), "again")})
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

// TestServeRunsDifferentSessionsConcurrently is KAN-1030's headline: two sessions
// on two working trees run their turns at the same time. Both turns park in the
// provider together — a state a single global turn lock could never reach, since
// it would hold the second turn until the first returned — so receiving both
// entered signals is the proof the per-session workers run concurrently.
func TestServeRunsDifferentSessionsConcurrently(t *testing.T) {
	t.Setenv(engine.APIKeyEnv, "kopicode-test-credential")
	srv, entered := concurrentProbeProvider(t)
	h := startServe(t, engine.Options{ProviderBaseURL: srv.URL})

	h.send(map[string]any{"jsonrpc": "2.0", "id": 1, "method": methodSessionStart,
		"params": startPayload("s1", t.TempDir(), "one")})
	h.send(map[string]any{"jsonrpc": "2.0", "id": 2, "method": methodSessionStart,
		"params": startPayload("s2", t.TempDir(), "two")})

	awaitEntered(t, entered, "the first session's turn")
	awaitEntered(t, entered, "the second session's turn (a global turn lock would hold it behind the first)")

	// Both are in flight; cancel them so their turns settle and shutdown is clean.
	h.send(map[string]any{"jsonrpc": "2.0", "id": 3, "method": methodSessionCancel,
		"params": map[string]any{"session": "s1"}})
	h.send(map[string]any{"jsonrpc": "2.0", "id": 4, "method": methodSessionCancel,
		"params": map[string]any{"session": "s2"}})
	h.awaitResponse(3)
	h.awaitResponse(4)

	for _, id := range []int{1, 2} {
		resp := h.awaitResponse(id)
		result, _ := resp["result"].(map[string]any)
		if result == nil {
			t.Fatalf("start id %d returned no result after cancel: %v", id, resp)
		}
		if result["stop"] != "cancelled" {
			t.Errorf("start id %d stop = %v, want cancelled", id, result["stop"])
		}
	}
	h.close()
}

// TestServeSerializesSameSessionTurns proves the other half: turns of one session
// never overlap. Two submits queue behind the in-flight start, and each reaches
// the provider only once the one before it has completed, so the peak in-flight
// count for the session stays 1 across all three turns. The queue is why the
// submits are accepted at all rather than refused as busy.
func TestServeSerializesSameSessionTurns(t *testing.T) {
	t.Setenv(engine.APIKeyEnv, "kopicode-test-credential")
	srv, entered, proceed, peak := serializingProvider(t)
	h := startServe(t, engine.Options{ProviderBaseURL: srv.URL})

	dir := t.TempDir()
	h.send(map[string]any{"jsonrpc": "2.0", "id": 1, "method": methodSessionStart,
		"params": startPayload("s1", dir, "A")})
	awaitEntered(t, entered, "turn A")

	// B and C queue behind A rather than being rejected. Neither can reach the
	// provider while A holds the session's single worker.
	h.send(map[string]any{"jsonrpc": "2.0", "id": 2, "method": methodSessionSubmit,
		"params": map[string]any{"session": "s1", "prompt": "B"}})
	h.send(map[string]any{"jsonrpc": "2.0", "id": 3, "method": methodSessionSubmit,
		"params": map[string]any{"session": "s1", "prompt": "C"}})

	// Release the turns one at a time; each release lets exactly the next one in.
	proceed <- struct{}{}
	if r := h.awaitResponse(1); r["error"] != nil {
		t.Fatalf("turn A errored: %v", r["error"])
	}
	awaitEntered(t, entered, "turn B (only after A completed)")
	proceed <- struct{}{}
	if r := h.awaitResponse(2); r["error"] != nil {
		t.Fatalf("turn B errored: %v", r["error"])
	}
	awaitEntered(t, entered, "turn C (only after B completed)")
	proceed <- struct{}{}
	if r := h.awaitResponse(3); r["error"] != nil {
		t.Fatalf("turn C errored: %v", r["error"])
	}

	if got := atomic.LoadInt32(peak); got != 1 {
		t.Errorf("peak concurrent turns for one session = %d, want 1; same-session turns must serialize", got)
	}
	h.close()
}

// TestServeStartRefusesALockedWorkingTree turns internal/lock's
// one-session-per-working-tree rule into a wire error. The first session holds
// its tree's lock for its whole life, so a second session.start on the same dir
// is refused with codeSessionLocked — the same refusal run --print exits 4 on,
// carried over the wire rather than swallowed as a generic open failure.
func TestServeStartRefusesALockedWorkingTree(t *testing.T) {
	t.Setenv(engine.APIKeyEnv, "kopicode-test-credential")
	srv := scriptedProvider(t, sseBody("done"))
	h := startServe(t, engine.Options{ProviderBaseURL: srv.URL})

	dir := t.TempDir()
	h.send(map[string]any{"jsonrpc": "2.0", "id": 1, "method": methodSessionStart,
		"params": startPayload("s1", dir, "first")})
	if r := h.awaitResponse(1); r["error"] != nil {
		t.Fatalf("the first session.start errored: %v", r["error"])
	}

	// s1 is still open and still holds dir's lock. A second session on the same
	// tree is the collision internal/lock refuses.
	h.send(map[string]any{"jsonrpc": "2.0", "id": 2, "method": methodSessionStart,
		"params": startPayload("s2", dir, "second")})
	resp := h.awaitResponse(2)
	rpcErr, _ := resp["error"].(map[string]any)
	if rpcErr == nil {
		t.Fatalf("a second session on a locked tree got no error: %v", resp)
	}
	if !numEq(rpcErr["code"], codeSessionLocked) {
		t.Errorf("error code = %v, want %d (codeSessionLocked)", rpcErr["code"], codeSessionLocked)
	}
	h.close()
}

// --- ADR-0016: consent_mode -------------------------------------------------

// TestServeSessionStartRequiresAConsentMode: decision 3's "no default that
// grants capability" — an omitted consent_mode is a usage error, refused
// before engine.Open ever runs, not a silent fallback to either mode.
func TestServeSessionStartRequiresAConsentMode(t *testing.T) {
	h := startServe(t, engine.Options{})
	h.send(map[string]any{"jsonrpc": "2.0", "id": 1, "method": methodSessionStart,
		"params": map[string]any{"session": "s1", "dir": t.TempDir(), "prompt": "hi"}})
	resp := h.awaitResponse(1)
	rpcErr, _ := resp["error"].(map[string]any)
	if rpcErr == nil {
		t.Fatalf("session.start with no consent_mode got no error: %v", resp)
	}
	if !numEq(rpcErr["code"], codeUsageError) {
		t.Errorf("error code = %v, want %d (codeUsageError)", rpcErr["code"], codeUsageError)
	}
	h.close()
}

// TestServeSessionStartRejectsAnUnrecognisedConsentMode: neither of the two
// declared values is not the same failure as omitting the field entirely (both
// are refused, but a caller who mistyped the value gets told what it typed),
// so this is proven as its own case rather than assumed to fall out of the
// missing-field test.
func TestServeSessionStartRejectsAnUnrecognisedConsentMode(t *testing.T) {
	h := startServe(t, engine.Options{})
	h.send(map[string]any{"jsonrpc": "2.0", "id": 1, "method": methodSessionStart,
		"params": map[string]any{
			"session": "s1", "dir": t.TempDir(), "prompt": "hi", "consent_mode": "yolo",
		}})
	resp := h.awaitResponse(1)
	rpcErr, _ := resp["error"].(map[string]any)
	if rpcErr == nil {
		t.Fatalf("session.start with consent_mode \"yolo\" got no error: %v", resp)
	}
	if !numEq(rpcErr["code"], codeUsageError) {
		t.Errorf("error code = %v, want %d (codeUsageError)", rpcErr["code"], codeUsageError)
	}
	h.close()
}

// TestServeUnattendedPolicyRequiresContainmentAcknowledgment: ADR-0011
// decision 4's containment requirement, made a wire-level usage error rather
// than a trust-the-caller assumption — requesting unattended_policy without
// containment_provided: true is refused exactly like a missing consent_mode.
func TestServeUnattendedPolicyRequiresContainmentAcknowledgment(t *testing.T) {
	h := startServe(t, engine.Options{})
	h.send(map[string]any{"jsonrpc": "2.0", "id": 1, "method": methodSessionStart,
		"params": map[string]any{
			"session": "s1", "dir": t.TempDir(), "prompt": "hi",
			"consent_mode": consentModeUnattendedPolicy,
		}})
	resp := h.awaitResponse(1)
	rpcErr, _ := resp["error"].(map[string]any)
	if rpcErr == nil {
		t.Fatalf("unattended_policy with no containment_provided got no error: %v", resp)
	}
	if !numEq(rpcErr["code"], codeUsageError) {
		t.Errorf("error code = %v, want %d (codeUsageError)", rpcErr["code"], codeUsageError)
	}
	h.close()
}

// TestServeRemoteInteractiveAsksTheClientForConsent is the end-to-end proof of
// ADR-0016's actual wire: a session declaring consent_mode: remote_interactive
// runs a turn whose model reply calls run_shell, and instead of the usual
// declared-allowlist/deny path, the server sends a consent.request naming the
// session, the kind and the command, blocks the turn on it, and — once the
// client replies allow — the shell actually runs and the turn completes,
// proving the answer reached the permission gate rather than being ignored.
func TestServeRemoteInteractiveAsksTheClientForConsent(t *testing.T) {
	t.Setenv(engine.APIKeyEnv, "kopicode-test-credential")
	srv := scriptedProvider(t,
		sseToolCall("call-1", "run_shell", `{"command":"echo hi"}`),
		sseBody("done"),
	)
	h := startServe(t, engine.Options{ProviderBaseURL: srv.URL})

	h.send(map[string]any{"jsonrpc": "2.0", "id": 1, "method": methodSessionStart,
		"params": map[string]any{
			"session": "s1", "dir": t.TempDir(), "prompt": "run a command",
			"consent_mode": consentModeRemoteInteractive,
		}})

	req := h.awaitRequest(methodConsentRequest)
	params, _ := req["params"].(map[string]any)
	if params["session"] != "s1" {
		t.Errorf("consent.request params.session = %v, want s1", params["session"])
	}
	if params["kind"] != "run_shell" {
		t.Errorf("consent.request params.kind = %v, want run_shell", params["kind"])
	}
	// The structured form (#177): the exact line and argv, so a client does not
	// strip a prefix off the joined detail.
	if params["command"] != "echo hi" {
		t.Errorf("consent.request params.command = %v, want the bare line %q", params["command"], "echo hi")
	}
	if argv, _ := params["argv"].([]any); len(argv) != 3 || argv[0] != "/bin/sh" || argv[1] != "-c" || argv[2] != "echo hi" {
		t.Errorf("consent.request params.argv = %v, want [/bin/sh -c echo hi]", params["argv"])
	}

	h.send(map[string]any{"jsonrpc": "2.0", "id": req["id"], "result": map[string]any{"answer": "allow"}})

	resp := h.awaitResponse(1)
	if resp["error"] != nil {
		t.Fatalf("session.start errored: %v", resp["error"])
	}
	result, _ := resp["result"].(map[string]any)
	if result["stop"] != "completed" {
		t.Errorf("stop = %v, want completed: %v", result["stop"], resp)
	}

	kinds := eventKinds(h.notificationsFor("s1"))
	if !hasKind(kinds, "permission_decided") {
		t.Errorf("no permission_decided event; the allowed shell command's decision is not on the record: %v", kinds)
	}
	if !hasKind(kinds, "tool_result") {
		t.Errorf("no tool_result event; the allowed shell command never ran: %v", kinds)
	}

	h.close()
}

// TestServeCloseEndsOneSessionAndFreesItsTree drives KAN-1795: session.close
// writes and announces the session's SessionEnded while the process stays up,
// releases the working-tree lock, and frees the id — so the same id can start on
// the same directory again, which is what a resident client reusing one child
// across delegations needs.
func TestServeCloseEndsOneSessionAndFreesItsTree(t *testing.T) {
	t.Setenv(engine.APIKeyEnv, "kopicode-test-credential")
	srv := scriptedProvider(t, sseBody("first"), sseBody("second"))
	h := startServe(t, engine.Options{ProviderBaseURL: srv.URL})

	dir := t.TempDir()
	h.send(map[string]any{"jsonrpc": "2.0", "id": 1, "method": methodSessionStart,
		"params": startPayload("s1", dir, "one")})
	h.awaitResponse(1)
	if hasKind(eventKinds(h.notificationsFor("s1")), "session_ended") {
		t.Fatal("session_ended announced before any close; it should wait for the close")
	}

	h.send(map[string]any{"jsonrpc": "2.0", "id": 2, "method": methodSessionClose,
		"params": map[string]any{"session": "s1"}})
	closed := h.awaitResponse(2)
	if closed["error"] != nil {
		t.Fatalf("session.close errored: %v", closed["error"])
	}
	result, _ := closed["result"].(map[string]any)
	if result["session"] != "s1" || result["closed"] != true {
		t.Errorf("close result = %v, want session s1 closed true", result)
	}
	// The response follows the announcement, so it is already on the wire.
	if !hasKind(eventKinds(h.notificationsFor("s1")), "session_ended") {
		t.Error("no session_ended after session.close; the record's bookend was not written")
	}

	// The process is still up, the tree's lock is free and the id is reusable.
	h.send(map[string]any{"jsonrpc": "2.0", "id": 3, "method": methodSessionStart,
		"params": startPayload("s1", dir, "two")})
	again := h.awaitResponse(3)
	if again["error"] != nil {
		t.Fatalf("restarting s1 on the same tree after a close errored: %v", again["error"])
	}

	if code := h.close(); code != exitSuccess {
		t.Errorf("serve exit = %d, want %d", code, exitSuccess)
	}
}

// TestServeCloseRefusesUnknownAndClosedSessions: a close for an id with no open
// session, and a submit to a session whose close is already accepted, both get
// the unknown-session code rather than queuing work that will never run.
func TestServeCloseRefusesUnknownAndClosedSessions(t *testing.T) {
	t.Setenv(engine.APIKeyEnv, "kopicode-test-credential")
	srv, reached := gatedProvider(t)
	h := startServe(t, engine.Options{ProviderBaseURL: srv.URL})

	h.send(map[string]any{"jsonrpc": "2.0", "id": 1, "method": methodSessionClose,
		"params": map[string]any{"session": "nope"}})
	resp := h.awaitResponse(1)
	rpcErr, _ := resp["error"].(map[string]any)
	if rpcErr == nil || !numEq(rpcErr["code"], codeUnknownSession) {
		t.Fatalf("closing an unknown session = %v, want error %d", resp, codeUnknownSession)
	}

	h.send(map[string]any{"jsonrpc": "2.0", "id": 2, "method": methodSessionStart,
		"params": startPayload("s1", t.TempDir(), "run")})
	<-reached
	h.send(map[string]any{"jsonrpc": "2.0", "id": 3, "method": methodSessionClose,
		"params": map[string]any{"session": "s1"}})
	h.send(map[string]any{"jsonrpc": "2.0", "id": 4, "method": methodSessionSubmit,
		"params": map[string]any{"session": "s1", "prompt": "too late"}})
	late := h.awaitResponse(4)
	rpcErr, _ = late["error"].(map[string]any)
	if rpcErr == nil || !numEq(rpcErr["code"], codeUnknownSession) {
		t.Errorf("submit after an accepted close = %v, want error %d", late, codeUnknownSession)
	}
	h.send(map[string]any{"jsonrpc": "2.0", "id": 5, "method": methodSessionClose,
		"params": map[string]any{"session": "s1"}})
	twice := h.awaitResponse(5)
	rpcErr, _ = twice["error"].(map[string]any)
	if rpcErr == nil || !numEq(rpcErr["code"], codeUnknownSession) {
		t.Errorf("a second close = %v, want error %d", twice, codeUnknownSession)
	}

	h.send(map[string]any{"jsonrpc": "2.0", "id": 6, "method": methodSessionCancel,
		"params": map[string]any{"session": "s1"}})
	h.awaitResponse(6)
	h.close()
}

// TestServeCloseWaitsForTheTurnsAlreadyAccepted: a close queued behind a running
// turn does not cut it off — the turn settles and answers first, then the close
// answers — so a client can close straight after its last submit.
func TestServeCloseWaitsForTheTurnsAlreadyAccepted(t *testing.T) {
	t.Setenv(engine.APIKeyEnv, "kopicode-test-credential")
	srv, reached := gatedProvider(t)
	h := startServe(t, engine.Options{ProviderBaseURL: srv.URL})

	h.send(map[string]any{"jsonrpc": "2.0", "id": 1, "method": methodSessionStart,
		"params": startPayload("s1", t.TempDir(), "run")})
	<-reached
	h.send(map[string]any{"jsonrpc": "2.0", "id": 2, "method": methodSessionClose,
		"params": map[string]any{"session": "s1"}})

	// Nothing has answered the close while the turn is still parked.
	time.Sleep(100 * time.Millisecond)
	h.mu.Lock()
	for _, m := range h.all {
		if m["method"] == nil && numEq(m["id"], 2) {
			h.mu.Unlock()
			t.Fatal("session.close answered while the turn ahead of it was still running")
		}
	}
	h.mu.Unlock()

	h.send(map[string]any{"jsonrpc": "2.0", "id": 3, "method": methodSessionCancel,
		"params": map[string]any{"session": "s1"}})
	h.awaitResponse(3)
	start := h.awaitResponse(1)
	closed := h.awaitResponse(2)

	if r, _ := start["result"].(map[string]any); r == nil || r["stop"] != "cancelled" {
		t.Errorf("the turn ahead of the close = %v, want it to settle cancelled", start)
	}
	if r, _ := closed["result"].(map[string]any); r == nil || r["closed"] != true {
		t.Errorf("close = %v, want closed true", closed)
	}
	h.close()
}

// --- ADR-0017: consent_mode "auto" ------------------------------------------

// permissionDecisions returns every permission_decided record announced for a
// session, as the wire carries it: decision, source and reason.
func permissionDecisions(notes []map[string]any) []map[string]any {
	var out []map[string]any
	for _, n := range notes {
		params, _ := n["params"].(map[string]any)
		ev, _ := params["event"].(map[string]any)
		if ev["kind"] == "permission_decided" {
			out = append(out, ev)
		}
	}
	return out
}

// TestServeAutoAllowsAnOrdinaryShellWithoutAsking is the point of the mode: a
// session declaring consent_mode "auto" runs a shell command with no
// consent.request on the wire, and the decision that let it run is journalled
// as an auto decision rather than as a user's, a policy file's or a remote's.
func TestServeAutoAllowsAnOrdinaryShellWithoutAsking(t *testing.T) {
	t.Setenv(engine.APIKeyEnv, "kopicode-test-credential")
	srv := scriptedProvider(t,
		sseToolCall("call-1", "run_shell", `{"command":"echo hi"}`),
		sseBody("done"),
	)
	h := startServe(t, engine.Options{ProviderBaseURL: srv.URL})

	h.send(map[string]any{"jsonrpc": "2.0", "id": 1, "method": methodSessionStart,
		"params": map[string]any{
			"session": "s1", "dir": t.TempDir(), "prompt": "run a command",
			"consent_mode": consentModeAuto,
		}})
	resp := h.awaitResponse(1) // would hang here if the server were waiting on a consent reply
	if resp["error"] != nil {
		t.Fatalf("session.start errored: %v", resp["error"])
	}
	if result, _ := resp["result"].(map[string]any); result["stop"] != "completed" {
		t.Errorf("stop = %v, want completed: %v", result["stop"], resp)
	}

	notes := h.notificationsFor("s1")
	if !hasKind(eventKinds(notes), "tool_result") {
		t.Errorf("no tool_result; the allowed command never ran: %v", eventKinds(notes))
	}
	decisions := permissionDecisions(notes)
	if len(decisions) != 1 {
		t.Fatalf("permission_decided records = %d, want 1: %v", len(decisions), decisions)
	}
	if decisions[0]["decision"] != "allow" || decisions[0]["source"] != "auto" {
		t.Errorf("decision = %v from %v, want allow from auto", decisions[0]["decision"], decisions[0]["source"])
	}
	h.close()
}

// TestServeAutoRefusesANeverAllowCommandWithAReason: a sudo command is not run
// and not asked about; it is refused, on the record, with the rule that fired.
func TestServeAutoRefusesANeverAllowCommandWithAReason(t *testing.T) {
	t.Setenv(engine.APIKeyEnv, "kopicode-test-credential")
	srv := scriptedProvider(t,
		sseToolCall("call-1", "run_shell", `{"command":"sudo rm -rf /var/lib"}`),
		sseBody("understood"),
	)
	h := startServe(t, engine.Options{ProviderBaseURL: srv.URL})

	h.send(map[string]any{"jsonrpc": "2.0", "id": 1, "method": methodSessionStart,
		"params": map[string]any{
			"session": "s1", "dir": t.TempDir(), "prompt": "do it",
			"consent_mode": consentModeAuto,
		}})
	resp := h.awaitResponse(1)
	if resp["error"] != nil {
		t.Fatalf("session.start errored: %v", resp["error"])
	}

	decisions := permissionDecisions(h.notificationsFor("s1"))
	if len(decisions) != 1 {
		t.Fatalf("permission_decided records = %d, want 1: %v", len(decisions), decisions)
	}
	d := decisions[0]
	if d["decision"] != "deny" || d["source"] != "auto" {
		t.Errorf("decision = %v from %v, want deny from auto", d["decision"], d["source"])
	}
	if reason, _ := d["reason"].(string); !strings.Contains(reason, "sudo") {
		t.Errorf("reason = %q, want it to name the rule that fired (sudo)", reason)
	}
	h.close()
}

// TestServeAutoHonoursACallerNeverAllowEntry: never_allow adds to the built-in
// list for this session.
func TestServeAutoHonoursACallerNeverAllowEntry(t *testing.T) {
	t.Setenv(engine.APIKeyEnv, "kopicode-test-credential")
	srv := scriptedProvider(t,
		sseToolCall("call-1", "run_shell", `{"command":"terraform apply -auto-approve"}`),
		sseBody("understood"),
	)
	h := startServe(t, engine.Options{ProviderBaseURL: srv.URL})

	h.send(map[string]any{"jsonrpc": "2.0", "id": 1, "method": methodSessionStart,
		"params": map[string]any{
			"session": "s1", "dir": t.TempDir(), "prompt": "deploy",
			"consent_mode": consentModeAuto, "never_allow": []string{"terraform apply"},
		}})
	h.awaitResponse(1)

	decisions := permissionDecisions(h.notificationsFor("s1"))
	if len(decisions) != 1 || decisions[0]["decision"] != "deny" {
		t.Fatalf("decisions = %v, want one deny", decisions)
	}
	if reason, _ := decisions[0]["reason"].(string); !strings.Contains(reason, "terraform apply") {
		t.Errorf("reason = %q, want it to name the caller's entry", reason)
	}
	h.close()
}

// TestServeAutoIgnoresTheProcessPolicyFile: --policy-file belongs to
// unattended_policy sessions. A process started with one must still be able to
// open an auto session, rather than have engine.Open refuse the two answerers.
func TestServeAutoIgnoresTheProcessPolicyFile(t *testing.T) {
	t.Setenv(engine.APIKeyEnv, "kopicode-test-credential")
	srv := scriptedProvider(t, sseBody("done"))
	h := startServe(t, engine.Options{
		ProviderBaseURL: srv.URL,
		Policy:          &engine.PolicyFile{Root: t.TempDir(), Allow: [][]string{{"true"}}},
	})
	h.send(map[string]any{"jsonrpc": "2.0", "id": 1, "method": methodSessionStart,
		"params": map[string]any{
			"session": "s1", "dir": t.TempDir(), "prompt": "hi", "consent_mode": consentModeAuto,
		}})
	if resp := h.awaitResponse(1); resp["error"] != nil {
		t.Fatalf("auto session beside a process-level policy file errored: %v", resp["error"])
	}
	h.close()
}

// TestServeNeverAllowIsAUsageErrorUnlessAuto: a caller that sent a never-allow
// list is relying on it, so a mode that would not consult it is refused rather
// than quietly less safe than the caller believes. A malformed entry is refused
// the same way, before anything is opened.
func TestServeNeverAllowIsAUsageErrorUnlessAuto(t *testing.T) {
	for name, extra := range map[string]map[string]any{
		"remote_interactive": {"consent_mode": consentModeRemoteInteractive, "never_allow": []string{"terraform apply"}},
		"unattended_policy":  {"consent_mode": consentModeUnattendedPolicy, "containment_provided": true, "never_allow": []string{"x"}},
		"auto, empty entry":  {"consent_mode": consentModeAuto, "never_allow": []string{" "}},
		"auto, shell syntax": {"consent_mode": consentModeAuto, "never_allow": []string{"rm -rf; ls"}},
	} {
		t.Run(name, func(t *testing.T) {
			h := startServe(t, engine.Options{})
			params := map[string]any{"session": "s1", "dir": t.TempDir(), "prompt": "hi"}
			for k, v := range extra {
				params[k] = v
			}
			h.send(map[string]any{"jsonrpc": "2.0", "id": 1, "method": methodSessionStart, "params": params})
			resp := h.awaitResponse(1)
			rpcErr, _ := resp["error"].(map[string]any)
			if rpcErr == nil || !numEq(rpcErr["code"], codeUsageError) {
				t.Fatalf("got %v, want a usage error", resp)
			}
			h.close()
		})
	}
}

// TestServeRemoteInteractiveIgnoresTheProcessPolicyFile: --policy-file belongs
// to unattended_policy sessions, so a process started with one must still open
// a remote_interactive session rather than have engine.Open refuse the pair of
// answerers (the live consenter and the declared policy).
func TestServeRemoteInteractiveIgnoresTheProcessPolicyFile(t *testing.T) {
	t.Setenv(engine.APIKeyEnv, "kopicode-test-credential")
	srv := scriptedProvider(t, sseBody("done"))
	h := startServe(t, engine.Options{
		ProviderBaseURL: srv.URL,
		Policy:          &engine.PolicyFile{Root: t.TempDir(), Allow: [][]string{{"true"}}},
	})
	h.send(map[string]any{"jsonrpc": "2.0", "id": 1, "method": methodSessionStart,
		"params": map[string]any{
			"session": "s1", "dir": t.TempDir(), "prompt": "hi", "consent_mode": consentModeRemoteInteractive,
		}})
	if resp := h.awaitResponse(1); resp["error"] != nil {
		t.Fatalf("remote_interactive session beside a process-level policy file errored: %v", resp["error"])
	}
	h.close()
}

// TestServeConsentTimeoutIsConfigurable: --consent-timeout reaches the live
// consenter. With a short timeout and a client that never answers, the request
// is denied on the clock the process was given and the denial is on the record
// attributed remote, exactly as an explicit deny would be.
func TestServeConsentTimeoutIsConfigurable(t *testing.T) {
	t.Setenv(engine.APIKeyEnv, "kopicode-test-credential")
	srv := scriptedProvider(t,
		sseToolCall("call-1", "run_shell", `{"command":"echo hi"}`),
		sseBody("done"),
	)
	h := startServeWith(t, engine.Options{ProviderBaseURL: srv.URL}, 150*time.Millisecond)
	start := time.Now()
	h.send(map[string]any{"jsonrpc": "2.0", "id": 1, "method": methodSessionStart,
		"params": map[string]any{
			"session": "s1", "dir": t.TempDir(), "prompt": "go", "consent_mode": consentModeRemoteInteractive,
		}})
	h.awaitRequest(methodConsentRequest) // never answered
	resp := h.awaitResponse(1)
	if resp["error"] != nil {
		t.Fatalf("session.start errored: %v", resp["error"])
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("took %s; the 150ms timeout was not applied (the default is %s)", elapsed, remoteConsentTimeout)
	}
	decisions := permissionDecisions(h.notificationsFor("s1"))
	if len(decisions) != 1 || decisions[0]["decision"] != "deny" || decisions[0]["source"] != "remote" {
		t.Errorf("decisions = %v, want one deny attributed remote", decisions)
	}
	h.close()
}

func TestConsentTimeoutFlagIsBounded(t *testing.T) {
	for _, tc := range []struct {
		arg  string
		want int // exit code when refused, or -1 for accepted
	}{
		{"--consent-timeout=5m", -1},
		{"--consent-timeout=1s", -1},
		{"--consent-timeout=24h", -1},
		{"--consent-timeout=0", exitUsage},
		{"--consent-timeout=500ms", exitUsage},
		{"--consent-timeout=25h", exitUsage},
		{"--consent-timeout=-5s", exitUsage},
		{"--consent-timeout=soon", exitUsage},
	} {
		var stderr strings.Builder
		_, timeout, code, ok := residentOptions("serve", "hint", []string{tc.arg}, &stderr)
		if tc.want == -1 {
			if !ok {
				t.Errorf("%s refused: %s", tc.arg, stderr.String())
			}
			continue
		}
		if ok || code != tc.want {
			t.Errorf("%s: ok=%v code=%d timeout=%s, want a usage refusal", tc.arg, ok, code, timeout)
		}
	}
	_, timeout, _, ok := residentOptions("serve", "hint", nil, io.Discard)
	if !ok || timeout != remoteConsentTimeout {
		t.Errorf("default = %s (ok %v), want %s", timeout, ok, remoteConsentTimeout)
	}
	_, timeout, _, _ = residentOptions("mcp", "hint", []string{"--consent-timeout=2m"}, io.Discard)
	if timeout != 2*time.Minute {
		t.Errorf("parsed timeout = %s, want 2m", timeout)
	}
}

// TestServeSessionConsentTimeoutOverridesTheProcessFlag (#178): one resident
// serve is shared by sessions that want different waits. The process is given a
// ten-minute timeout and the session asks for one second, so a client that never
// answers is denied on the session's clock.
func TestServeSessionConsentTimeoutOverridesTheProcessFlag(t *testing.T) {
	t.Setenv(engine.APIKeyEnv, "kopicode-test-credential")
	srv := scriptedProvider(t,
		sseToolCall("call-1", "run_shell", `{"command":"echo hi"}`),
		sseBody("done"),
	)
	h := startServeWith(t, engine.Options{ProviderBaseURL: srv.URL}, 10*time.Minute)
	start := time.Now()
	h.send(map[string]any{"jsonrpc": "2.0", "id": 1, "method": methodSessionStart,
		"params": map[string]any{
			"session": "s1", "dir": t.TempDir(), "prompt": "go",
			"consent_mode": consentModeRemoteInteractive, "consent_timeout": "1s",
		}})
	h.awaitRequest(methodConsentRequest) // never answered
	if resp := h.awaitResponse(1); resp["error"] != nil {
		t.Fatalf("session.start errored: %v", resp["error"])
	}
	if elapsed := time.Since(start); elapsed > 30*time.Second {
		t.Errorf("took %s; the session's 1s consent_timeout did not override the process's 10m", elapsed)
	}
	decisions := permissionDecisions(h.notificationsFor("s1"))
	if len(decisions) != 1 || decisions[0]["decision"] != "deny" || decisions[0]["source"] != "remote" {
		t.Errorf("decisions = %v, want one deny attributed remote", decisions)
	}
	h.close()
}

// TestServeSessionConsentTimeoutIsBoundedAndOnlyForRemote: the same 1s to 24h
// bounds as the flag, and a mode that never waits on a live answer refuses it
// rather than ignoring it, as never_allow does.
func TestServeSessionConsentTimeoutIsBoundedAndOnlyForRemote(t *testing.T) {
	for name, extra := range map[string]map[string]any{
		"zero":              {"consent_mode": consentModeRemoteInteractive, "consent_timeout": "0s"},
		"below the floor":   {"consent_mode": consentModeRemoteInteractive, "consent_timeout": "500ms"},
		"above the ceiling": {"consent_mode": consentModeRemoteInteractive, "consent_timeout": "25h"},
		"not a duration":    {"consent_mode": consentModeRemoteInteractive, "consent_timeout": "soon"},
		"auto":              {"consent_mode": consentModeAuto, "consent_timeout": "5m"},
		"unattended_policy": {"consent_mode": consentModeUnattendedPolicy, "containment_provided": true, "consent_timeout": "5m"},
	} {
		t.Run(name, func(t *testing.T) {
			h := startServe(t, engine.Options{})
			params := map[string]any{"session": "s1", "dir": t.TempDir(), "prompt": "hi"}
			for k, v := range extra {
				params[k] = v
			}
			h.send(map[string]any{"jsonrpc": "2.0", "id": 1, "method": methodSessionStart, "params": params})
			resp := h.awaitResponse(1)
			rpcErr, _ := resp["error"].(map[string]any)
			if rpcErr == nil || !numEq(rpcErr["code"], codeUsageError) {
				t.Fatalf("got %v, want a usage error", resp)
			}
			h.close()
		})
	}
}

// TestServeReadOnlySessionRefusesAFileWrite (#176, ADR-0019): a read_only
// session whose model calls write_file gets a denial it can read, the file is
// not created, and no consent.request is sent — the refusal is the gate's, not a
// question for the client.
func TestServeReadOnlySessionRefusesAFileWrite(t *testing.T) {
	t.Setenv(engine.APIKeyEnv, "kopicode-test-credential")
	srv := scriptedProvider(t,
		sseToolCall("call-1", "write_file", `{"path":"new.txt","content":"hi"}`),
		sseBody("done"),
	)
	h := startServe(t, engine.Options{ProviderBaseURL: srv.URL})
	dir := t.TempDir()
	h.send(map[string]any{"jsonrpc": "2.0", "id": 1, "method": methodSessionStart,
		"params": map[string]any{
			"session": "s1", "dir": dir, "prompt": "write a file",
			"consent_mode": consentModeRemoteInteractive, "read_only": true,
		}})
	if resp := h.awaitResponse(1); resp["error"] != nil {
		t.Fatalf("session.start errored: %v", resp["error"])
	}
	if _, err := os.Stat(filepath.Join(dir, "new.txt")); err == nil {
		t.Error("new.txt exists: a read-only session wrote a file")
	}
	if h.sawRequest(methodConsentRequest) {
		t.Error("a consent.request was sent; the refusal must not be put to the client")
	}
	h.close()
}

// TestServeReadOnlyIsAUsageErrorUnderAuto: auto runs shell unasked and a shell
// line can write, so the pair is refused rather than quietly less read-only
// than the caller believes.
func TestServeReadOnlyIsAUsageErrorUnderAuto(t *testing.T) {
	h := startServe(t, engine.Options{})
	h.send(map[string]any{"jsonrpc": "2.0", "id": 1, "method": methodSessionStart,
		"params": map[string]any{
			"session": "s1", "dir": t.TempDir(), "prompt": "hi",
			"consent_mode": consentModeAuto, "read_only": true,
		}})
	resp := h.awaitResponse(1)
	rpcErr, _ := resp["error"].(map[string]any)
	if rpcErr == nil || !numEq(rpcErr["code"], codeUsageError) {
		t.Fatalf("got %v, want a usage error", resp)
	}
	h.close()
}

// askEvents returns the ask_answered events of a session.
func askEvents(notes []map[string]any) []map[string]any {
	var out []map[string]any
	for _, n := range notes {
		params, _ := n["params"].(map[string]any)
		ev, _ := params["event"].(map[string]any)
		if ev["kind"] == "ask_answered" {
			out = append(out, ev)
		}
	}
	return out
}

// TestServeRemoteAskPutsTheQuestionToTheClient (#173, ADR-0020): with
// ask_mode "remote" the model's ask arrives as an ask.request carrying the
// question and context, the client's text goes back to the model as the tool's
// result, and the journal attributes the answer to "remote".
func TestServeRemoteAskPutsTheQuestionToTheClient(t *testing.T) {
	t.Setenv(engine.APIKeyEnv, "kopicode-test-credential")
	srv := scriptedProvider(t,
		sseToolCall("call-1", "ask", `{"question":"tabs or spaces?","context":"gofmt is not set up"}`),
		sseBody("done"),
	)
	h := startServe(t, engine.Options{ProviderBaseURL: srv.URL})
	h.send(map[string]any{"jsonrpc": "2.0", "id": 1, "method": methodSessionStart,
		"params": map[string]any{
			"session": "s1", "dir": t.TempDir(), "prompt": "style the file",
			"consent_mode": consentModeRemoteInteractive, "ask_mode": "remote",
		}})

	req := h.awaitRequest(methodAskRequest)
	params, _ := req["params"].(map[string]any)
	if params["session"] != "s1" || params["question"] != "tabs or spaces?" || params["context"] != "gofmt is not set up" {
		t.Errorf("ask.request params = %v", params)
	}
	h.send(map[string]any{"jsonrpc": "2.0", "id": req["id"], "result": map[string]any{"text": "tabs"}})

	if resp := h.awaitResponse(1); resp["error"] != nil {
		t.Fatalf("session.start errored: %v", resp["error"])
	}
	evs := askEvents(h.notificationsFor("s1"))
	if len(evs) != 1 || evs[0]["source"] != "remote" || evs[0]["text"] != "tabs" || evs[0]["reason"] == "refused" {
		t.Errorf("ask_answered = %v, want one answer \"tabs\" with source remote", evs)
	}
	h.close()
}

// TestServeRemoteAskExpiryIsNotADenial: an unanswered question has no safe
// default, so the wait running out gives the model the same "nobody could
// answer" refusal the headless case does — journalled as refused, and the turn
// goes on.
func TestServeRemoteAskExpiryIsNotADenial(t *testing.T) {
	t.Setenv(engine.APIKeyEnv, "kopicode-test-credential")
	srv := scriptedProvider(t,
		sseToolCall("call-1", "ask", `{"question":"which one?"}`),
		sseBody("done"),
	)
	h := startServe(t, engine.Options{ProviderBaseURL: srv.URL})
	h.send(map[string]any{"jsonrpc": "2.0", "id": 1, "method": methodSessionStart,
		"params": map[string]any{
			"session": "s1", "dir": t.TempDir(), "prompt": "go", "consent_mode": consentModeRemoteInteractive,
			"ask_mode": "remote", "consent_timeout": "1s",
		}})
	h.awaitRequest(methodAskRequest) // never answered
	resp := h.awaitResponse(1)
	if resp["error"] != nil {
		t.Fatalf("session.start errored: %v", resp["error"])
	}
	if result, _ := resp["result"].(map[string]any); result["stop"] != "completed" {
		t.Errorf("stop = %v, want completed: the turn should go on after an unanswered ask", result["stop"])
	}
	evs := askEvents(h.notificationsFor("s1"))
	if len(evs) != 1 || evs[0]["reason"] != "refused" || evs[0]["text"] != "no human is present to answer this question" {
		t.Errorf("ask_answered = %v, want one refusal with the nobody-could-answer text", evs)
	}
	h.close()
}

// TestServeAskModeIsValidated: remote ask needs the live consent client it rides
// on, and only one non-default value exists.
func TestServeAskModeIsValidated(t *testing.T) {
	for name, extra := range map[string]map[string]any{
		"auto":              {"consent_mode": consentModeAuto, "ask_mode": "remote"},
		"unattended_policy": {"consent_mode": consentModeUnattendedPolicy, "containment_provided": true, "ask_mode": "remote"},
		"unknown value":     {"consent_mode": consentModeRemoteInteractive, "ask_mode": "sometimes"},
	} {
		t.Run(name, func(t *testing.T) {
			h := startServe(t, engine.Options{})
			params := map[string]any{"session": "s1", "dir": t.TempDir(), "prompt": "hi"}
			for k, v := range extra {
				params[k] = v
			}
			h.send(map[string]any{"jsonrpc": "2.0", "id": 1, "method": methodSessionStart, "params": params})
			resp := h.awaitResponse(1)
			rpcErr, _ := resp["error"].(map[string]any)
			if rpcErr == nil || !numEq(rpcErr["code"], codeUsageError) {
				t.Fatalf("got %v, want a usage error", resp)
			}
			h.close()
		})
	}
}
