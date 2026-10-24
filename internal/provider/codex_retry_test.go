package provider

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// KOP-224: the Codex client bounds a silent stream and retries one that broke,
// as the OpenRouter client does (KOP-220, KOP-221). The recorded text turn is the
// reply a retry must end with.

const codexIdle = 150 * time.Millisecond

// instantClock answers every backoff at once, and remembers what was asked.
type instantClock struct {
	mu    sync.Mutex
	asked []time.Duration
}

func (c *instantClock) NewTimer(d time.Duration) (<-chan time.Time, func()) {
	c.mu.Lock()
	c.asked = append(c.asked, d)
	c.mu.Unlock()
	ch := make(chan time.Time, 1)
	ch <- time.Time{}
	return ch, func() {}
}

type topRand struct{}

func (topRand) Int64N(n int64) int64 { return n - 1 }

func codexRetryClient(t *testing.T, srv *httptest.Server, retry Retry, events *[]RetryEvent, mu *sync.Mutex, extra ...CodexOption) *CodexClient {
	t.Helper()
	opts := []CodexOption{
		WithCodexBaseURL(srv.URL),
		WithCodexClock(&instantClock{}),
		WithCodexRand(topRand{}),
		WithCodexRetry(retry),
		WithCodexStreamIdleTimeout(codexIdle),
		WithCodexRetryObserver(func(_ context.Context, ev RetryEvent) {
			mu.Lock()
			defer mu.Unlock()
			*events = append(*events, ev)
		}),
	}
	c, err := NewCodexClient(fakeCreds(t), append(opts, extra...)...)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func codexRequest() Request {
	return Request{ModelID: "gpt-5.6-luna", Turn: 2, Attempt: 1, Messages: []Message{{Role: RoleUser, Content: "hi"}}}
}

// holdCodexOpen answers 200 with only keep-alive comments until the client leaves.
func holdCodexOpen(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/event-stream")
	for {
		if _, err := io.WriteString(w, ": keep-alive\n\n"); err != nil {
			return
		}
		w.(http.Flusher).Flush()
		select {
		case <-r.Context().Done():
			return
		case <-time.After(codexIdle / 5):
		}
	}
}

func TestACodexStreamThatGoesSilentIsRetriedAndFinishes(t *testing.T) {
	raw := string(readFixture(t, "text_turn.sse"))
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			holdCodexOpen(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, raw)
	}))
	defer srv.Close()

	var events []RetryEvent
	var mu sync.Mutex
	c := codexRetryClient(t, srv, Retry{MaxAttempts: 3, Base: time.Second, Cap: 8 * time.Second}, &events, &mu)
	s, err := c.Complete(t.Context(), codexRequest())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close() //nolint:errcheck // a test's defer.
	content, _ := drainStream(t, s)
	if err := s.Err(); err != nil {
		t.Fatalf("stream error after the retry: %v", err)
	}
	if content != "ok" {
		t.Errorf("content = %q, want the recorded reply", content)
	}
	if calls.Load() != 2 {
		t.Errorf("requests = %d, want the silent one and the retry", calls.Load())
	}
	mu.Lock()
	defer mu.Unlock()
	if len(events) != 1 || events[0].Cause != "stream silent" || events[0].Turn != 2 || events[0].Try != 1 || events[0].OfTries != 3 ||
		!strings.Contains(events[0].Detail, "went silent") {
		t.Errorf("retry events = %+v, want one silent stream on try 1 of 3 with the cause quoted", events)
	}
}

func TestACodexStreamThatNeverRecoversFailsWithTheCause(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		holdCodexOpen(w, r)
	}))
	defer srv.Close()

	var events []RetryEvent
	var mu sync.Mutex
	c := codexRetryClient(t, srv, DefaultRetry, &events, &mu)
	s, err := c.Complete(t.Context(), codexRequest())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close() //nolint:errcheck // a test's defer.

	done := make(chan struct{})
	go func() { defer close(done); drainStream(t, s) }()
	select {
	case <-done:
	case <-time.After(20 * time.Second):
		t.Fatal("a silent Codex stream hung the reader")
	}
	if _, err := s.Reply(); !errors.Is(err, ErrRetriesExhausted) || !errors.Is(err, ErrStreamSilent) {
		t.Fatalf("Reply error = %v, want retries exhausted wrapping the silence", err)
	}
	if got := calls.Load(); got != int32(DefaultStreamAttempts) {
		t.Errorf("requests = %d, want %d", got, DefaultStreamAttempts)
	}
}

func TestACodexStreamThatResetsMidwayIsRetried(t *testing.T) {
	raw := string(readFixture(t, "text_turn.sse"))
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		if calls.Add(1) == 1 {
			_, _ = io.WriteString(w, raw[:len(raw)/2])
			w.(http.Flusher).Flush()
			panic(http.ErrAbortHandler)
		}
		_, _ = io.WriteString(w, raw)
	}))
	defer srv.Close()

	var events []RetryEvent
	var mu sync.Mutex
	c := codexRetryClient(t, srv, DefaultRetry, &events, &mu)
	s, err := c.Complete(t.Context(), codexRequest())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close() //nolint:errcheck // a test's defer.
	drainStream(t, s)
	if err := s.Err(); err != nil {
		t.Fatalf("stream error after the reset: %v", err)
	}
	if string(s.Transcript()) != raw {
		t.Error("the transcript is not the finished attempt's bytes alone")
	}
	mu.Lock()
	defer mu.Unlock()
	if len(events) != 1 || events[0].Detail == "" {
		t.Errorf("events = %+v, want one retry with its cause", events)
	}
}

func TestACodexRetryIsOffAtOneAttempt(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		holdCodexOpen(w, r)
	}))
	defer srv.Close()
	var events []RetryEvent
	var mu sync.Mutex
	c := codexRetryClient(t, srv, Retry{MaxAttempts: 1}, &events, &mu)
	s, err := c.Complete(t.Context(), codexRequest())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close() //nolint:errcheck // a test's defer.
	drainStream(t, s)
	if err := s.Err(); !errors.Is(err, ErrStreamSilent) || errors.Is(err, ErrRetriesExhausted) {
		t.Errorf("Err = %v, want the bare silence", err)
	}
	if calls.Load() != 1 || len(events) != 0 {
		t.Errorf("MaxAttempts 1 still retried: %d requests, %+v", calls.Load(), events)
	}
}

func TestACodexRefusalIsStillNotRetried(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"error":{"message":"spent"}}`)
	}))
	defer srv.Close()
	var events []RetryEvent
	var mu sync.Mutex
	c := codexRetryClient(t, srv, DefaultRetry, &events, &mu)
	if _, err := c.Complete(t.Context(), codexRequest()); err == nil {
		t.Fatal("a 429 was accepted")
	}
	if calls.Load() != 1 || len(events) != 0 {
		t.Errorf("a refused request was retried: %d requests, %+v", calls.Load(), events)
	}
}
