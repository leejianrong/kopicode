package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/leejianrong/kopicode/internal/engine"
)

// TestHandoffInTheREPLEndsOneSessionAndStartsAnotherFromTheDocument drives the
// whole front end: a turn, /handoff, a yes, and a second turn. The second turn
// must run in a new session, in the same working tree, whose model was given the
// document and whose journal names the session it came from.
func TestHandoffInTheREPLEndsOneSessionAndStartsAnotherFromTheDocument(t *testing.T) {
	t.Setenv(engine.APIKeyEnv, "kopicode-test-credential")
	t.Setenv("VISUAL", "")
	t.Setenv("EDITOR", "")

	var mu sync.Mutex
	var bodies []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies = append(bodies, string(raw))
		mu.Unlock()
		reply := "all good"
		if strings.Contains(string(raw), "Write a handoff for the next session") {
			reply = "## Goal\n\nfinish the parser\n\n## Next step\n\nrun go test\n"
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(sseBody(reply)))
	}))
	t.Cleanup(srv.Close)

	stdout, stderr, code, dir := runSession(t, "start the parser\n/handoff finish it\ny\nkeep going\n", engine.Options{
		ProviderBaseURL: srv.URL,
	})
	if code != exitSuccess {
		t.Fatalf("exit code = %d. stderr:\n%s\nstdout:\n%s", code, stderr, stdout)
	}
	if !strings.Contains(stdout, "started from the handoff") {
		t.Errorf("stdout never said a new session started:\n%s", stdout)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(bodies) != 3 {
		t.Fatalf("provider saw %d requests, want 3 (turn, draft, turn)", len(bodies))
	}
	if strings.Contains(bodies[0], "<handoff>") || !strings.Contains(bodies[2], "<handoff>") || !strings.Contains(bodies[2], "finish the parser") {
		t.Errorf("the handoff reached the wrong request: first=%q last=%q", bodies[0], bodies[2])
	}
	if strings.Contains(bodies[2], "start the parser") {
		t.Errorf("the new session was shown the old conversation, not just the handoff")
	}

	sessions, err := os.ReadDir(filepath.Join(dir, ".kopicode", "sessions"))
	if err != nil || len(sessions) != 2 {
		t.Fatalf("sessions = %v (%v), want two", sessions, err)
	}
	var parent, child string
	for _, s := range sessions {
		raw, _ := os.ReadFile(filepath.Join(dir, ".kopicode", "sessions", s.Name(), "events.jsonl"))
		if strings.Contains(string(raw), `"parent_session"`) {
			child = s.Name()
			if !strings.Contains(string(raw), `"scope":"handoff"`) {
				t.Errorf("the child's journal does not hold the handoff")
			}
		} else {
			parent = s.Name()
			if !strings.Contains(string(raw), `"type":"HandoffWritten"`) || !strings.Contains(string(raw), `"type":"SessionEnded"`) {
				t.Errorf("the parent's journal lacks HandoffWritten or SessionEnded")
			}
		}
	}
	if parent == "" || child == "" {
		t.Fatalf("could not tell parent from child in %v", sessions)
	}
	raw, _ := os.ReadFile(filepath.Join(dir, ".kopicode", "sessions", child, "events.jsonl"))
	if !strings.Contains(string(raw), `"parent_session":"`+parent+`"`) {
		t.Errorf("the child does not name %s as its parent", parent)
	}
	if _, err := os.Stat(engine.HandoffPath(dir, parent)); err != nil {
		t.Errorf("no projection file: %v", err)
	}
}
