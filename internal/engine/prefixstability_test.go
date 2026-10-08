package engine_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/leejianrong/kopicode/internal/engine"
)

// KAN-1969. Provider prompt caching hits only on an exact prefix, so the whole
// scheme rests on one property: every request is the previous request's messages
// followed by new ones, with the system prompt and the tool catalogue unchanged.
// This holds it in place against whatever the loop grows next, and checks that
// one stable session id is sent for OpenRouter's sticky routing.
func TestEveryRequestExtendsThePreviousOneByteForByte(t *testing.T) {
	t.Setenv(engine.APIKeyEnv, "sk-or-not-a-real-key-0123456789")

	type captured struct {
		messages []json.RawMessage
		tools    json.RawMessage
		session  string
	}
	var (
		mu   sync.Mutex
		reqs []captured
	)
	replies := []string{
		`data: {"id":"a","model":"m","choices":[{"index":0,"delta":{"role":"assistant","tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"list_dir","arguments":"{\"path\":\".\"}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":50,"completion_tokens":5,"total_tokens":55}}`,
		`data: {"id":"b","model":"m","choices":[{"index":0,"delta":{"role":"assistant","content":"all done"},"finish_reason":"stop"}],"usage":{"prompt_tokens":90,"completion_tokens":2,"total_tokens":92}}`,
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body struct {
			Messages []json.RawMessage `json:"messages"`
			Tools    json.RawMessage   `json:"tools"`
		}
		if err := json.Unmarshal(raw, &body); err != nil {
			t.Errorf("body is not JSON: %v", err)
		}
		mu.Lock()
		n := len(reqs)
		reqs = append(reqs, captured{body.Messages, body.Tools, r.Header.Get("X-Session-Id")})
		mu.Unlock()
		if n >= len(replies) {
			n = len(replies) - 1
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, replies[n]+"\n\ndata: [DONE]\n\n")
	}))
	defer srv.Close()

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	sel, err := engine.ResolveSelection(dir, engine.SelectionOverrides{})
	if err != nil {
		t.Fatal(err)
	}
	s, err := engine.Open(t.Context(), engine.Options{Dir: dir, Selection: sel, SessionID: "stable-session", ProviderBaseURL: srv.URL})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close(context.Background()) })
	if _, err := s.Run(t.Context(), "list the files"); err != nil {
		t.Fatalf("Run: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(reqs) < 2 {
		t.Fatalf("saw %d requests, want at least 2: the test needs a second request to compare", len(reqs))
	}
	for i := 1; i < len(reqs); i++ {
		prev, cur := reqs[i-1], reqs[i]
		if len(cur.messages) <= len(prev.messages) {
			t.Fatalf("request %d has %d messages after %d: history must only grow", i+1, len(cur.messages), len(prev.messages))
		}
		for j := range prev.messages {
			if string(cur.messages[j]) != string(prev.messages[j]) {
				t.Errorf("request %d changed message %d:\n  was %s\n  now %s\nan edit here is a cache miss for everything after it",
					i+1, j, prev.messages[j], cur.messages[j])
			}
		}
		if string(cur.tools) != string(prev.tools) {
			t.Errorf("request %d changed the tool catalogue; it sits before the history, so every cached token is lost", i+1)
		}
		if cur.session != "stable-session" || prev.session != "stable-session" {
			t.Errorf("X-Session-Id = %q then %q, want %q on every request", prev.session, cur.session, "stable-session")
		}
	}
}
