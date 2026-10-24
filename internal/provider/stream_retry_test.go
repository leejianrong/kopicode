package provider_test

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

	"github.com/leejianrong/kopicode/internal/provider"
	"github.com/leejianrong/kopicode/internal/provider/fixture"
)

// KOP-220 and KOP-221: a stream that goes silent or breaks after the provider
// opened it. Every server here answers the first request badly and the next one
// with the fixture's recorded stream, so "the session continued" is checked against
// the reply the fixture states.

// idle is short enough to keep the suite fast and long enough that a loaded CI
// machine does not trip it on a stream that is sending.
const idle = 150 * time.Millisecond

// retryObserver collects the events the client reports.
type retryObserver struct {
	mu  sync.Mutex
	got []provider.RetryEvent
}

func (o *retryObserver) observe(_ context.Context, ev provider.RetryEvent) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.got = append(o.got, ev)
}

func (o *retryObserver) events() []provider.RetryEvent {
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([]provider.RetryEvent(nil), o.got...)
}

// holdOpen serves a stream's headers and then only keep-alive comments, as a
// provider waiting on a dead upstream does, until the client gives up on the
// request. Comments are not data, so the stream is silent however many arrive.
func holdOpen(t *testing.T, w http.ResponseWriter, r *http.Request) {
	t.Helper()
	writeSSE(t, w, []string{fixture.KeepAliveComment, ""})
	for {
		select {
		case <-r.Context().Done():
			return
		case <-time.After(idle / 5):
			if _, err := io.WriteString(w, fixture.KeepAliveComment+"\n\n"); err != nil {
				return
			}
			w.(http.Flusher).Flush()
		}
	}
}

// abortMidStream serves the first few frames of the recording and then drops the
// connection without ending the body, which the client reads as a reset.
func abortMidStream(t *testing.T, w http.ResponseWriter, lines []string) {
	t.Helper()
	writeSSE(t, w, lines[:len(lines)/2])
	panic(http.ErrAbortHandler)
}

func newRetryClient(t *testing.T, srv *httptest.Server, obs *retryObserver, policy provider.Retry, extra ...provider.ClientOption) *provider.Client {
	t.Helper()
	opts := []provider.ClientOption{
		provider.WithClock(&manualClock{}),
		provider.WithRand(topOfInterval()),
		provider.WithRetry(policy),
		provider.WithStreamIdleTimeout(idle),
		provider.WithRetryObserver(obs.observe),
	}
	return newClient(t, srv, append(opts, extra...)...)
}

func TestASilentStreamIsRetriedAndTheReplyIsTheRecordedOne(t *testing.T) {
	f := loadFixture(t)
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			holdOpen(t, w, r)
			return
		}
		writeSSE(t, w, f.Exchanges[0].Response.Stream)
	}))
	defer srv.Close()

	obs := &retryObserver{}
	c := newRetryClient(t, srv, obs, provider.Retry{MaxAttempts: 3, Base: time.Second, Cap: 8 * time.Second})

	req := request(f)
	req.Turn, req.Attempt = 4, 1
	stream, err := c.Complete(t.Context(), req)
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	defer stream.Close() //nolint:errcheck // a test's defer.
	drain(t, stream)
	reply, err := stream.Reply()
	if err != nil {
		t.Fatalf("Reply after the retry: %v", err)
	}
	if reply.Content == "" && len(reply.ToolCalls) == 0 {
		t.Errorf("the retried reply is empty: %+v", reply)
	}
	if got := calls.Load(); got != 2 {
		t.Errorf("requests sent = %d, want 2 (the silent one and the retry)", got)
	}

	events := obs.events()
	if len(events) != 1 {
		t.Fatalf("retry events = %+v, want exactly one", events)
	}
	ev := events[0]
	if ev.Cause != "stream silent" || ev.Turn != 4 || ev.Attempt != 1 || ev.Try != 1 || ev.OfTries != 3 {
		t.Errorf("retry event = %+v, want a silent stream on try 1 of 3 for turn 4 attempt 1", ev)
	}
	if !strings.Contains(ev.Detail, "went silent") {
		t.Errorf("event detail %q does not say the stream went silent", ev.Detail)
	}
}

func TestAStreamThatNeverRecoversFailsWithTheCauseInsteadOfHanging(t *testing.T) {
	f := loadFixture(t)
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		holdOpen(t, w, r)
	}))
	defer srv.Close()

	obs := &retryObserver{}
	c := newRetryClient(t, srv, obs, provider.Retry{MaxAttempts: 6, Base: time.Second, Cap: 8 * time.Second})

	stream, err := c.Complete(t.Context(), request(f))
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	defer stream.Close() //nolint:errcheck // a test's defer.

	done := make(chan struct{})
	go func() { defer close(done); drain(t, stream) }()
	select {
	case <-done:
	case <-time.After(20 * time.Second):
		t.Fatal("the stream did not end: a silent provider hung the reader")
	}

	_, err = stream.Reply()
	if !errors.Is(err, provider.ErrRetriesExhausted) || !errors.Is(err, provider.ErrStreamSilent) {
		t.Fatalf("Reply error = %v, want retries exhausted wrapping the silence", err)
	}
	if !strings.Contains(err.Error(), "no data for") {
		t.Errorf("error %q does not quote the cause whole", err)
	}
	if got, want := calls.Load(), int32(provider.DefaultStreamAttempts); got != want {
		t.Errorf("requests sent = %d, want the stream budget of %d, not the request policy's 6", got, want)
	}
	if got := len(obs.events()); got != provider.DefaultStreamAttempts-1 {
		t.Errorf("journaled retries = %d, want %d: every attempt is recorded", got, provider.DefaultStreamAttempts-1)
	}
}

func TestAReplyThatKeepsSendingIsNeverCutOff(t *testing.T) {
	f := loadFixture(t)
	lines := f.Exchanges[0].Response.Stream
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		// Slower overall than the idle limit, steady within it.
		for _, line := range lines {
			if _, err := io.WriteString(w, line+"\n"); err != nil {
				return
			}
			w.(http.Flusher).Flush()
			if line != "" && strings.HasPrefix(line, "data: ") {
				select {
				case <-time.After(idle / 3):
				case <-r.Context().Done():
					return
				}
			}
		}
	}))
	defer srv.Close()

	obs := &retryObserver{}
	c := newRetryClient(t, srv, obs, provider.DefaultRetry)
	stream, err := c.Complete(t.Context(), request(f))
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	defer stream.Close() //nolint:errcheck // a test's defer.
	start := time.Now()
	drain(t, stream)
	if _, err := stream.Reply(); err != nil {
		t.Fatalf("Reply: %v", err)
	}
	if took := time.Since(start); took <= idle {
		t.Skipf("the fixture streamed in %s, under the idle limit, so this proved nothing", took)
	}
	if calls.Load() != 1 || len(obs.events()) != 0 {
		t.Errorf("a steady stream was retried: %d requests, events %+v", calls.Load(), obs.events())
	}
}

func TestAConnectionResetMidStreamIsRetried(t *testing.T) {
	f := loadFixture(t)
	lines := f.Exchanges[0].Response.Stream
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) == 1 {
			abortMidStream(t, w, lines)
			return
		}
		writeSSE(t, w, lines)
	}))
	defer srv.Close()

	obs := &retryObserver{}
	c := newRetryClient(t, srv, obs, provider.DefaultRetry)
	stream, err := c.Complete(t.Context(), request(f))
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	defer stream.Close() //nolint:errcheck // a test's defer.
	drain(t, stream)
	if _, err := stream.Reply(); err != nil {
		t.Fatalf("Reply after the reset: %v", err)
	}
	events := obs.events()
	if len(events) != 1 || (events[0].Cause != "stream broken" && events[0].Cause != "stream incomplete") {
		t.Fatalf("retry events = %+v, want one broken or incomplete stream", events)
	}
	if events[0].Detail == "" {
		t.Error("the event carries no detail: the cause must be quoted whole")
	}
	// The record holds the attempt that finished, not the failed one's fragment.
	if got := string(stream.Transcript()); !strings.HasSuffix(strings.TrimSpace(got), "data: [DONE]") {
		t.Errorf("transcript does not end with the finished attempt's [DONE]: %q", got)
	}
}

func TestRetryingStreamsIsOffWhenThePolicyAllowsOneSend(t *testing.T) {
	f := loadFixture(t)
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		holdOpen(t, w, r)
	}))
	defer srv.Close()

	obs := &retryObserver{}
	c := newRetryClient(t, srv, obs, provider.Retry{MaxAttempts: 1})
	stream, err := c.Complete(t.Context(), request(f))
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	defer stream.Close() //nolint:errcheck // a test's defer.
	drain(t, stream)
	if err := stream.Err(); !errors.Is(err, provider.ErrStreamSilent) || errors.Is(err, provider.ErrRetriesExhausted) {
		t.Errorf("Err = %v, want the bare silence and no retry wrapper", err)
	}
	if calls.Load() != 1 || len(obs.events()) != 0 {
		t.Errorf("MaxAttempts 1 still retried: %d requests, events %+v", calls.Load(), obs.events())
	}
}

func TestACancelledStreamIsNotRetried(t *testing.T) {
	f := loadFixture(t)
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		holdOpen(t, w, r)
	}))
	defer srv.Close()

	obs := &retryObserver{}
	c := newRetryClient(t, srv, obs, provider.DefaultRetry, provider.WithStreamIdleTimeout(time.Minute))
	ctx, cancel := context.WithCancel(t.Context())
	stream, err := c.Complete(ctx, request(f))
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	defer stream.Close() //nolint:errcheck // a test's defer.
	time.AfterFunc(50*time.Millisecond, cancel)
	drain(t, stream)
	if err := stream.Err(); !errors.Is(err, context.Canceled) {
		t.Errorf("Err = %v, want the caller's cancellation", err)
	}
	if calls.Load() != 1 || len(obs.events()) != 0 {
		t.Errorf("a cancelled stream was retried: %d requests, events %+v", calls.Load(), obs.events())
	}
}

func TestAnIdleLimitBelowZeroRemovesTheWatch(t *testing.T) {
	f := loadFixture(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeSSE(t, w, []string{fixture.KeepAliveComment, ""})
		select {
		case <-time.After(3 * idle):
		case <-r.Context().Done():
			return
		}
		for _, line := range f.Exchanges[0].Response.Stream {
			_, _ = io.WriteString(w, line+"\n")
		}
	}))
	defer srv.Close()

	obs := &retryObserver{}
	c := newRetryClient(t, srv, obs, provider.DefaultRetry, provider.WithStreamIdleTimeout(-1))
	stream, err := c.Complete(t.Context(), request(f))
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	defer stream.Close() //nolint:errcheck // a test's defer.
	drain(t, stream)
	if err := stream.Err(); err != nil {
		t.Errorf("Err = %v, want a clean stream with the limit removed", err)
	}
	if len(obs.events()) != 0 {
		t.Errorf("retries with no limit set: %+v", obs.events())
	}
}

func TestAFrameThatCannotBeDecodedIsNotRetried(t *testing.T) {
	f := loadFixture(t)
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		writeSSE(t, w, []string{"data: {not json", ""})
	}))
	defer srv.Close()

	obs := &retryObserver{}
	c := newRetryClient(t, srv, obs, provider.DefaultRetry)
	stream, err := c.Complete(t.Context(), request(f))
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	defer stream.Close() //nolint:errcheck // a test's defer.
	drain(t, stream)
	if err := stream.Err(); !errors.Is(err, provider.ErrMalformedStream) {
		t.Errorf("Err = %v, want a malformed stream", err)
	}
	if calls.Load() != 1 || len(obs.events()) != 0 {
		t.Errorf("a deterministic failure was retried: %d requests, events %+v", calls.Load(), obs.events())
	}
}
