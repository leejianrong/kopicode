package engine_test

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/leejianrong/kopicode/internal/engine"
	"github.com/leejianrong/kopicode/internal/journal"
)

// This file is KAN-851's done-when at the point that matters most: not that
// provider.Client notifies an observer (internal/provider's own tests cover
// that), but that engine.Open — the real construction site a front end
// reaches through — actually wires that observer to the session's journal, so
// a retry storm the live client survives in silence to Complete's caller still
// lands on disk. engine.Provider stays one method throughout: Session.Run
// below never sees a retry, only a StopCompleted, and the retry shows up
// exclusively because the journal was read back afterward.

// oneRetrySSE is a minimal, single-turn reply: prose, no tool calls, finish
// reason "stop". It is written by hand rather than borrowed from a shipped
// fixture because every shipped fixture is a two-turn tool-call exchange, and
// this test needs the loop to reach a clean stop after exactly one retried
// request.
var oneRetrySSE = []string{
	`data: {"id":"gen-1","object":"chat.completion.chunk","created":1,"model":"qwen/qwen3-coder-next",` +
		`"provider":"parasail","choices":[{"index":0,"delta":{"content":"ok"},"finish_reason":null}]}`,
	``,
	`data: {"id":"gen-1","object":"chat.completion.chunk","created":1,"model":"qwen/qwen3-coder-next",` +
		`"provider":"parasail","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],` +
		`"usage":{"prompt_tokens":3,"completion_tokens":1,"total_tokens":4}}`,
	``,
	`data: [DONE]`,
}

// TestALiveRetryStormReachesTheJournalThroughOpen drives the live client
// through engine.Open exactly as cmd/kopicode does: a real HTTP round trip
// against an httptest server, OPENROUTER_API_KEY set, ProviderBaseURL pointed
// at the server. The provider answers 429 twice before succeeding, and
// Session.Run sees none of that — only a clean stop — which is the whole
// point: the retries are invisible to engine.Provider and visible only in the
// record.
func TestALiveRetryStormReachesTheJournalThroughOpen(t *testing.T) {
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		if calls <= 2 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusTooManyRequests)
			fmt.Fprint(w, `{"error":{"code":429,"message":"rate limited"}}`)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		for _, line := range oneRetrySSE {
			fmt.Fprintln(w, line)
		}
	}))
	defer srv.Close()

	t.Setenv(engine.APIKeyEnv, "kopicode-fake-key-not-a-credential-0851")

	sel, err := engine.ResolveSelection(t.TempDir(), engine.SelectionOverrides{})
	if err != nil {
		t.Fatalf("resolving the default selection: %v", err)
	}

	dir := t.TempDir()
	s, err := engine.Open(t.Context(), engine.Options{
		Dir:             dir,
		Selection:       sel,
		SessionID:       "retry-through-open",
		Now:             fixedClock(),
		ProviderBaseURL: srv.URL,
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close(t.Context()) })

	res, err := s.Run(t.Context(), "say ok")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Stop != engine.StopCompleted {
		t.Fatalf("stop = %s, want %s — engine.Provider must see only the eventual success", res.Stop, engine.StopCompleted)
	}
	if calls != 3 {
		t.Fatalf("the provider was called %d time(s), want 3 (two 429s and a success)", calls)
	}
	if err := s.Close(t.Context()); err != nil {
		t.Fatalf("Close: %v", err)
	}

	events := readJournal(t, s.Path())
	var retries []journal.ProviderRetried
	for _, ev := range events {
		if p, ok := ev.Payload.(journal.ProviderRetried); ok {
			retries = append(retries, p)
		}
	}

	if len(retries) != 2 {
		t.Fatalf("journal holds %d ProviderRetried event(s), want 2 — the client's own retries "+
			"must be visible in the record even though Session.Run never saw them\nevents: %v",
			len(retries), types(events))
	}
	for i, want := range []journal.ProviderRetried{
		{Attempt: 1, Try: 1, Cause: "http 429"},
		{Attempt: 1, Try: 2, Cause: "http 429"},
	} {
		got := retries[i]
		if got.Attempt != want.Attempt || got.Try != want.Try || got.Cause != want.Cause {
			t.Errorf("retry %d = %+v, want Attempt=%d Try=%d Cause=%q", i, got, want.Attempt, want.Try, want.Cause)
		}
		if got.OfTries <= 0 {
			t.Errorf("retry %d OfTries = %d, want a positive attempt budget", i, got.OfTries)
		}
		if got.DelayMS < 0 {
			t.Errorf("retry %d DelayMS = %d, want >= 0", i, got.DelayMS)
		}
	}
}
