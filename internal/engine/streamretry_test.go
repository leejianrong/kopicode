package engine_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/leejianrong/kopicode/internal/engine"
	"github.com/leejianrong/kopicode/internal/journal"
)

// KOP-220 and KOP-221 through a whole session: the model's second reply goes
// silent after the provider opened the stream, the session asks again, and it
// finishes. The journal says so, and the tool call from the first reply, which had
// already run, is not run again.
func TestASessionContinuesWhenAReplyStreamGoesSilent(t *testing.T) {
	t.Setenv(engine.APIKeyEnv, "sk-or-not-a-real-key-0123456789")
	t.Setenv(engine.StreamIdleEnv, "1")

	toolCall := `data: {"id":"a","model":"m","choices":[{"index":0,"delta":{"role":"assistant","tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"list_dir","arguments":"{\"path\":\".\"}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":50,"completion_tokens":5,"total_tokens":55}}`
	done := `data: {"id":"b","model":"m","choices":[{"index":0,"delta":{"role":"assistant","content":"all done"},"finish_reason":"stop"}],"usage":{"prompt_tokens":90,"completion_tokens":2,"total_tokens":92}}`

	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		switch requests.Add(1) {
		case 1:
			_, _ = io.WriteString(w, toolCall+"\n\ndata: [DONE]\n\n")
		case 2:
			// The second reply: headers, a keep-alive, and then nothing.
			_, _ = io.WriteString(w, ": keep-alive\n\n")
			w.(http.Flusher).Flush()
			<-r.Context().Done()
		default:
			_, _ = io.WriteString(w, done+"\n\ndata: [DONE]\n\n")
		}
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
	s, err := engine.Open(t.Context(), engine.Options{Dir: dir, Selection: sel, SessionID: "silent-stream", ProviderBaseURL: srv.URL})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close(context.Background()) })

	res, err := s.Run(t.Context(), "list the files")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Stop != engine.StopCompleted {
		t.Fatalf("stop = %v (%s), want the session to finish on the retry", res.Stop, res.Detail)
	}
	if got := requests.Load(); got != 3 {
		t.Errorf("requests = %d, want 3: the tool-call reply, the silent one, the retry", got)
	}

	events := readJournal(t, s.Path())
	var retried []journal.ProviderRetried
	var requestsJournaled, listDirResults int
	for _, ev := range events {
		switch p := ev.Payload.(type) {
		case journal.ProviderRetried:
			retried = append(retried, p)
		case journal.ProviderRequest:
			requestsJournaled++
		case journal.ToolResult:
			if p.Tool == "list_dir" {
				listDirResults++
			}
		}
	}
	if len(retried) != 1 {
		t.Fatalf("ProviderRetried events = %+v, want exactly one for the silent stream", retried)
	}
	if r := retried[0]; r.Cause != "stream silent" || r.Try != 1 || !strings.Contains(r.Detail, "went silent") {
		t.Errorf("ProviderRetried = %+v, want a silent stream on try 1 with the cause quoted", r)
	}
	if requestsJournaled != 2 {
		t.Errorf("ProviderRequest events = %d, want 2: a retry inside one request is not a new request", requestsJournaled)
	}
	if listDirResults != 1 {
		t.Errorf("list_dir ran %d times, want once: a reply whose calls ran is never replayed", listDirResults)
	}
}

func TestAnUnreadableStreamIdleLimitIsRefused(t *testing.T) {
	t.Setenv(engine.APIKeyEnv, "sk-or-not-a-real-key-0123456789")
	for _, v := range []string{"abc", "-5", "1.5", "3m"} {
		t.Run(v, func(t *testing.T) {
			t.Setenv(engine.StreamIdleEnv, v)
			dir := t.TempDir()
			sel, err := engine.ResolveSelection(dir, engine.SelectionOverrides{})
			if err != nil {
				t.Fatal(err)
			}
			_, err = engine.Open(t.Context(), engine.Options{Dir: dir, Selection: sel, SessionID: "bad-idle", ProviderBaseURL: "http://127.0.0.1:1"})
			if !errors.Is(err, engine.ErrConfig) || !strings.Contains(err.Error(), engine.StreamIdleEnv) {
				t.Errorf("Open with %s=%q = %v, want a config error naming the variable", engine.StreamIdleEnv, v, err)
			}
		})
	}
}

// KOP-224: the Codex route gets the same bound. The first reply goes silent after
// the backend opened the stream; the session asks again, finishes, and the journal
// holds the retry. Nothing about the login changes.
func TestACodexSessionContinuesWhenAReplyStreamGoesSilent(t *testing.T) {
	codexHome(t, 0o600, fakeLogin())
	t.Setenv(engine.StreamIdleEnv, "1")
	stream, err := os.ReadFile(filepath.Join("..", "provider", "testdata", "codex", "text_turn.sse"))
	if err != nil {
		t.Fatal(err)
	}

	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		if requests.Add(1) == 1 {
			_, _ = io.WriteString(w, ": keep-alive\n\n")
			w.(http.Flusher).Flush()
			<-r.Context().Done()
			return
		}
		_, _ = w.Write(stream)
	}))
	defer srv.Close()

	dir := t.TempDir()
	sel, err := engine.ResolveSelection(dir, engine.SelectionOverrides{Provider: engine.ProviderCodex, Model: "gpt-5.6-luna"})
	if err != nil {
		t.Fatal(err)
	}
	s, err := engine.Open(t.Context(), engine.Options{Dir: dir, Selection: sel, SessionID: "codex-silent", ProviderBaseURL: srv.URL})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close(context.Background()) })

	res, err := s.Run(t.Context(), "say ok")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Stop != engine.StopCompleted {
		t.Fatalf("stop = %v (%s), want the session to finish on the retry", res.Stop, res.Detail)
	}
	var retried []journal.ProviderRetried
	for _, ev := range readJournal(t, s.Path()) {
		if p, ok := ev.Payload.(journal.ProviderRetried); ok {
			retried = append(retried, p)
		}
	}
	if len(retried) != 1 || retried[0].Cause != "stream silent" || !strings.Contains(retried[0].Detail, "went silent") {
		t.Errorf("ProviderRetried events = %+v, want one silent stream with the cause quoted", retried)
	}
	if got := requests.Load(); got != 2 {
		t.Errorf("requests = %d, want 2", got)
	}
}
