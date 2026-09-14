//go:build integration

package main

// End-to-end coverage for `kopicode serve` (ADR-0013, KAN-1031): a scripted
// stdio client drives the resident surface through the shapes an orchestrator
// actually puts it through — a session that runs turn after turn in one process,
// a session that is cancelled and then keeps working, two sessions running at
// once with their own records, and the working-tree lock collision that must
// come back as a wire error rather than wedge the process.
//
// It reuses serve_test.go's serveHarness (the same in-process stdio seam the
// KAN-1029/1030 tests use) rather than reinventing one, and it asserts only
// observable outcomes: the responses and notifications on the wire, and the
// record each session leaves on disk. No internal loop state is read — the wire
// event stream is the journal's own projection (engine.Options.Events tees each
// journal event after it is appended), so asserting the events is asserting the
// record.
//
// It is behind the integration tag because it is the heavier end-to-end suite,
// the same place fork/resume/print-retry keep theirs; the focused concurrency,
// serialization and lock-code proofs live in the fast serve_test.go.

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/leejianrong/kopicode/internal/engine"
)

// countKind reports how many of the given session's notifications carry kind.
func countKind(notes []map[string]any, kind string) int {
	n := 0
	for _, k := range eventKinds(notes) {
		if k == kind {
			n++
		}
	}
	return n
}

// gateFirstThenComplete blocks its first request until the turn is cancelled —
// signalling reached when it arrives — and completes every later request
// normally. It is how the survive-a-cancel test parks turn one long enough to
// cancel it, then lets turn two run to a clean stop.
func gateFirstThenComplete(t *testing.T) (*httptest.Server, <-chan struct{}) {
	t.Helper()
	reached := make(chan struct{}, 1)
	var n int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&n, 1) == 1 {
			select {
			case reached <- struct{}{}:
			default:
			}
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
			<-r.Context().Done() // parked until the turn is cancelled
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(sseBody("done")))
	}))
	t.Cleanup(srv.Close)
	return srv, reached
}

// recordDirExists fails unless path is a session record directory holding a
// non-empty events.jsonl — the on-disk journal the wire events are teed from.
func recordDirExists(t *testing.T, path string) {
	t.Helper()
	if path == "" {
		t.Fatal("the session reported no record path")
	}
	info, err := os.Stat(path)
	if err != nil || !info.IsDir() {
		t.Fatalf("record %q is not a directory: %v", path, err)
	}
	events := filepath.Join(path, "events.jsonl")
	ei, err := os.Stat(events)
	if err != nil {
		t.Fatalf("record %q has no events.jsonl: %v", path, err)
	}
	if ei.Size() == 0 {
		t.Fatalf("record %q has an empty events.jsonl", path)
	}
}

// completedTurn awaits the response to id and asserts it is a clean completion,
// returning the result object so a caller can read the record path.
func completedTurn(t *testing.T, h *serveHarness, id int) map[string]any {
	t.Helper()
	resp := h.awaitResponse(id)
	if resp["error"] != nil {
		t.Fatalf("turn %d errored: %v", id, resp["error"])
	}
	result, _ := resp["result"].(map[string]any)
	if result == nil {
		t.Fatalf("turn %d has no result: %v", id, resp)
	}
	if result["stop"] != "completed" {
		t.Fatalf("turn %d stop = %v, want completed", id, result["stop"])
	}
	return result
}

// TestServeRunsAResidentSessionOverManyTurns is the resident property end to
// end: one process opens a session and runs three turns on it — a start and two
// submits — with no restart and no second Open, each turn recorded. The wire
// carries one session_started and one assistant_message per turn, and the record
// start named is a real journal on disk.
func TestServeRunsAResidentSessionOverManyTurns(t *testing.T) {
	t.Setenv(engine.APIKeyEnv, "kopicode-test-credential")
	srv := scriptedProvider(t, sseBody("one"), sseBody("two"), sseBody("three"))
	h := startServe(t, engine.Options{ProviderBaseURL: srv.URL})

	dir := t.TempDir()
	h.send(map[string]any{"jsonrpc": "2.0", "id": 1, "method": methodSessionStart,
		"params": map[string]any{"session": "s1", "dir": dir, "prompt": "first"}})
	start := completedTurn(t, h, 1)
	record, _ := start["record"].(string)
	recordDirExists(t, record)

	h.send(map[string]any{"jsonrpc": "2.0", "id": 2, "method": methodSessionSubmit,
		"params": map[string]any{"session": "s1", "prompt": "second"}})
	if r := completedTurn(t, h, 2); r["record"] != nil {
		t.Errorf("submit result carries a record %v; only start opens the journal", r["record"])
	}

	h.send(map[string]any{"jsonrpc": "2.0", "id": 3, "method": methodSessionSubmit,
		"params": map[string]any{"session": "s1", "prompt": "third"}})
	completedTurn(t, h, 3)

	notes := h.notificationsFor("s1")
	if got := countKind(notes, "session_started"); got != 1 {
		t.Errorf("session_started notifications = %d, want 1 for one resident session", got)
	}
	if got := countKind(notes, "assistant_message"); got < 3 {
		t.Errorf("assistant_message notifications = %d, want at least 3 (one per turn); the three turns "+
			"did not all run in the one process", got)
	}

	if code := h.close(); code != exitSuccess {
		t.Errorf("serve exit = %d, want %d", code, exitSuccess)
	}
	if got := countKind(h.notificationsFor("s1"), "session_ended"); got != 1 {
		t.Errorf("session_ended notifications = %d, want 1 after shutdown", got)
	}
}

// TestServeSessionSurvivesACancelAndContinues proves session.cancel does not end
// the session, the promise decision 3 makes: a turn parked in the provider is
// cancelled and settles StopCancelled, and then a fresh submit on the same
// session runs to a clean completion. A cancel that tore the session down would
// leave the second turn with nowhere to go.
func TestServeSessionSurvivesACancelAndContinues(t *testing.T) {
	t.Setenv(engine.APIKeyEnv, "kopicode-test-credential")
	srv, reached := gateFirstThenComplete(t)
	h := startServe(t, engine.Options{ProviderBaseURL: srv.URL})

	dir := t.TempDir()
	h.send(map[string]any{"jsonrpc": "2.0", "id": 1, "method": methodSessionStart,
		"params": map[string]any{"session": "s1", "dir": dir, "prompt": "loop forever"}})
	<-reached

	h.send(map[string]any{"jsonrpc": "2.0", "id": 2, "method": methodSessionCancel,
		"params": map[string]any{"session": "s1"}})
	cancelResp := h.awaitResponse(2)
	if cancelResp["error"] != nil {
		t.Fatalf("session.cancel errored: %v", cancelResp["error"])
	}

	start := h.awaitResponse(1)
	result, _ := start["result"].(map[string]any)
	if result == nil || result["stop"] != "cancelled" {
		t.Fatalf("the cancelled turn did not settle cancelled: %v", start)
	}

	// The session is still open. A new turn on it runs normally.
	h.send(map[string]any{"jsonrpc": "2.0", "id": 3, "method": methodSessionSubmit,
		"params": map[string]any{"session": "s1", "prompt": "now really"}})
	completedTurn(t, h, 3)

	h.close()
}

// TestServeRunsConcurrentSessionsInOneProcess drives two sessions on two working
// trees through one resident process — a start and a submit each — and checks
// they stay independent: every turn completes, and each session leaves its own
// distinct record. This is the resident multi-session property from the outside,
// where serve_test.go's concurrency test proves the turns actually overlap.
func TestServeRunsConcurrentSessionsInOneProcess(t *testing.T) {
	t.Setenv(engine.APIKeyEnv, "kopicode-test-credential")
	srv := scriptedProvider(t, sseBody("done"))
	h := startServe(t, engine.Options{ProviderBaseURL: srv.URL})

	h.send(map[string]any{"jsonrpc": "2.0", "id": 1, "method": methodSessionStart,
		"params": map[string]any{"session": "s1", "dir": t.TempDir(), "prompt": "one"}})
	h.send(map[string]any{"jsonrpc": "2.0", "id": 2, "method": methodSessionStart,
		"params": map[string]any{"session": "s2", "dir": t.TempDir(), "prompt": "two"}})
	r1 := completedTurn(t, h, 1)
	r2 := completedTurn(t, h, 2)

	rec1, _ := r1["record"].(string)
	rec2, _ := r2["record"].(string)
	recordDirExists(t, rec1)
	recordDirExists(t, rec2)
	if rec1 == rec2 {
		t.Fatalf("both sessions share record %q; their journals are not isolated", rec1)
	}

	h.send(map[string]any{"jsonrpc": "2.0", "id": 3, "method": methodSessionSubmit,
		"params": map[string]any{"session": "s1", "prompt": "one more"}})
	h.send(map[string]any{"jsonrpc": "2.0", "id": 4, "method": methodSessionSubmit,
		"params": map[string]any{"session": "s2", "prompt": "two more"}})
	completedTurn(t, h, 3)
	completedTurn(t, h, 4)

	for _, sess := range []string{"s1", "s2"} {
		notes := h.notificationsFor(sess)
		if !hasKind(eventKinds(notes), "session_started") {
			t.Errorf("session %s carries no session_started", sess)
		}
		if !hasKind(eventKinds(notes), "assistant_message") {
			t.Errorf("session %s carries no assistant_message; its turns were not teed", sess)
		}
	}

	h.close()
}

// TestServeLockCollisionLeavesTheProcessUsable drives the internal/lock collision
// end to end: a second session.start on a tree the first session already holds
// comes back as a codeSessionLocked wire error — promptly, or awaitResponse's own
// deadline would fail the test — and the resident process is unharmed, so the
// first session keeps running turns. The collision is a refusal, not a wedge.
func TestServeLockCollisionLeavesTheProcessUsable(t *testing.T) {
	t.Setenv(engine.APIKeyEnv, "kopicode-test-credential")
	srv := scriptedProvider(t, sseBody("done"))
	h := startServe(t, engine.Options{ProviderBaseURL: srv.URL})

	dir := t.TempDir()
	h.send(map[string]any{"jsonrpc": "2.0", "id": 1, "method": methodSessionStart,
		"params": map[string]any{"session": "s1", "dir": dir, "prompt": "first"}})
	completedTurn(t, h, 1)

	h.send(map[string]any{"jsonrpc": "2.0", "id": 2, "method": methodSessionStart,
		"params": map[string]any{"session": "s2", "dir": dir, "prompt": "second"}})
	resp := h.awaitResponse(2)
	rpcErr, _ := resp["error"].(map[string]any)
	if rpcErr == nil {
		t.Fatalf("a second session on a locked tree got no error: %v", resp)
	}
	if !numEq(rpcErr["code"], codeSessionLocked) {
		t.Errorf("error code = %v, want %d (codeSessionLocked)", rpcErr["code"], codeSessionLocked)
	}

	// The refused start left the process fully usable: the first session runs on.
	h.send(map[string]any{"jsonrpc": "2.0", "id": 3, "method": methodSessionSubmit,
		"params": map[string]any{"session": "s1", "prompt": "still here"}})
	completedTurn(t, h, 3)

	h.close()
}
