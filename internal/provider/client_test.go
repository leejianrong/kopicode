package provider_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/leejianrong/kopicode/internal/provider"
	"github.com/leejianrong/kopicode/internal/provider/fixture"
)

// This file drives the live client at the transport level, against an
// httptest.Server, which is the seam internal/provider/mock's package doc says
// this card should add: "KAN-776 should still add a transport-level replay for
// the client's own tests — the fixtures already carry what it needs, including
// status and the allowlisted response headers. That is a different seam for a
// different subject, not a replacement for this one."
//
// Nothing here reaches openrouter.ai and nothing here reads
// OPENROUTER_API_KEY. The one live test is in client_live_test.go behind the
// `live` build tag, because a suite that needs a credential and a network is a
// suite that is skipped, and a skipped test asserts nothing.

// fakeKey is the credential the tests send.
//
// Deliberately low-entropy and self-describing: it has to be long enough for the
// journal's redactor to act on (12 bytes) and distinctive enough that a grep
// finding it proves something, without looking enough like a real OpenRouter key
// to trip a secret scanner on this repository's own history.
const fakeKey = "kopicode-fake-key-not-a-credential-0001"

// replayFixture is the shipped recording the transport-level tests replay. Its
// pin and model are the ones the request tests assert reach the wire, so the pin
// is taken from the data rather than written down a fourth time.
const replayFixture = "two_turn_native_tool_call"

func loadFixture(t *testing.T) fixture.Fixture {
	t.Helper()
	f, err := fixture.Load(fixture.FS(), replayFixture)
	if err != nil {
		t.Fatalf("loading fixture %q: %v", replayFixture, err)
	}
	return f
}

// pin converts a fixture's pin into the provider's.
func pinOf(f fixture.Fixture) provider.Pin {
	return provider.Pin{
		Order:          f.Pin.Order,
		AllowFallbacks: f.Pin.AllowFallbacks,
		Quantizations:  f.Pin.Quantizations,
	}
}

// request builds a pinned request against the fixture's arm.
func request(f fixture.Fixture) provider.Request {
	return provider.Request{
		ModelID:  f.ModelID,
		Pin:      pinOf(f),
		Sampling: provider.Sampling{Temperature: 0.2, TopP: 0.95, MaxTokens: 1024},
		Messages: []provider.Message{
			{Role: provider.RoleSystem, Content: "you are kopicode"},
			{Role: provider.RoleUser, Content: "add a test"},
		},
		Turn:    1,
		Attempt: 1,
	}
}

// newClient builds a client pointed at srv with the retry path fully injected.
func newClient(t *testing.T, srv *httptest.Server, opts ...provider.ClientOption) *provider.Client {
	t.Helper()
	base := []provider.ClientOption{
		provider.WithBaseURL(srv.URL),
		provider.WithHTTPClient(srv.Client()),
	}
	c, err := provider.NewClient(provider.NewAPIKey(fakeKey), append(base, opts...)...)
	if err != nil {
		t.Fatalf("building client: %v", err)
	}
	return c
}

// writeSSE serves lines as an event stream, flushing each one so a test can
// observe arrival order.
func writeSSE(t *testing.T, w http.ResponseWriter, lines []string) {
	t.Helper()
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	for _, line := range lines {
		if _, err := io.WriteString(w, line+"\n"); err != nil {
			return
		}
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	}
}

// TestCompleteReplaysARecordedStream is the transport-level replay: the
// fixture's own frames, served over HTTP, decoded by the shipping client.
//
// It asserts the reply against the facts the fixture states rather than against
// values written here, so a fixture replaced by a real recording (KAN-774)
// re-checks the client rather than needing this test rewritten.
func TestCompleteReplaysARecordedStream(t *testing.T) {
	f := loadFixture(t)
	if len(f.Exchanges) == 0 {
		t.Fatalf("fixture %q records no exchanges", f.Name)
	}

	// Every exchange, not the first one: the fixture's turns differ in kind —
	// one is a tool call and carries no text at all, the next is the terminal
	// answer — and a test that replayed only turn one would leave the other
	// shape undriven.
	for _, ex := range f.Exchanges {
		t.Run(fmt.Sprintf("turn %d attempt %d", ex.Turn, ex.Attempt), func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				writeSSE(t, w, ex.Response.Stream)
			}))
			defer srv.Close()

			stream, err := newClient(t, srv).Complete(t.Context(), request(f))
			if err != nil {
				t.Fatalf("Complete: %v", err)
			}
			defer stream.Close() //nolint:errcheck // the assertions below cover the stream's own error.

			deltas := drain(t, stream)
			if err := stream.Err(); err != nil {
				t.Fatalf("draining the stream: %v", err)
			}
			// A reply that is nothing but a native tool call carries no
			// user-visible text, which is why this is conditional rather than
			// absolute — but a terminal answer that produced no deltas means
			// nothing about arrival was driven.
			if len(ex.Expect.Tools) == 0 && len(deltas) == 0 {
				t.Error("a text reply produced no deltas")
			}

			reply, err := stream.Reply()
			if err != nil {
				t.Fatalf("Reply: %v", err)
			}
			if reply.FinishReason != ex.Expect.FinishReason {
				t.Errorf("finish reason %q, fixture says %q", reply.FinishReason, ex.Expect.FinishReason)
			}
			if reply.ServedBy != ex.Expect.ServedBy {
				t.Errorf("served by %q, fixture says %q", reply.ServedBy, ex.Expect.ServedBy)
			}
			want := provider.Usage{
				Prompt:     ex.Expect.Usage.Prompt,
				Completion: ex.Expect.Usage.Completion,
				Total:      ex.Expect.Usage.Total,
			}
			if diff := cmp.Diff(want, reply.Usage); diff != "" {
				t.Errorf("usage (-fixture +client):\n%s", diff)
			}
			var names []string
			for _, tc := range reply.ToolCalls {
				names = append(names, tc.Name)
			}
			// Only the native route puts calls on the reply; the fenced and
			// XML routes carry them in the text, and extraction is
			// internal/parse's job rather than this client's.
			if ex.Expect.Route == "native" {
				if diff := cmp.Diff(ex.Expect.Tools, names); diff != "" {
					t.Errorf("tool calls (-fixture +client):\n%s", diff)
				}
			}
		})
	}
}

// TestTranscriptIsTheBytesTheProviderSent covers the one thing a live streamed
// reply has that a replayed one does not: no assembled body.
//
// Reply.Raw is nil, deliberately — see NewSSEStream — and the record of what
// arrived is the transcript, verbatim, framing included.
func TestTranscriptIsTheBytesTheProviderSent(t *testing.T) {
	f := loadFixture(t)
	lines := f.Exchanges[0].Response.Stream

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeSSE(t, w, lines)
	}))
	defer srv.Close()

	stream, err := newClient(t, srv).Complete(t.Context(), request(f))
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	defer stream.Close() //nolint:errcheck // the assertions below cover the stream's own error.
	drain(t, stream)

	reply, err := stream.Reply()
	if err != nil {
		t.Fatalf("Reply: %v", err)
	}
	if reply.Raw != nil {
		t.Errorf("Reply.Raw is %q; a streamed reply has no assembled body and inventing one "+
			"would put a re-encoding in the record", reply.Raw)
	}

	want := strings.Join(lines, "\n") + "\n"
	if got := string(stream.Transcript()); got != want {
		t.Errorf("transcript does not match what the server wrote\n got: %q\nwant: %q", got, want)
	}
}

// TestRequestCarriesThePinAndTheCredential is the positive control for every
// no-leak assertion in key_test.go: the key really is sent, so a test that finds
// it nowhere is testing something.
func TestRequestCarriesThePinAndTheCredential(t *testing.T) {
	f := loadFixture(t)

	var (
		gotAuth   string
		gotAccept string
		gotBody   []byte
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotAccept = r.Header.Get("Accept")
		gotBody, _ = io.ReadAll(r.Body)
		writeSSE(t, w, f.Exchanges[0].Response.Stream)
	}))
	defer srv.Close()

	stream, err := newClient(t, srv).Complete(t.Context(), request(f))
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	drain(t, stream)
	_ = stream.Close()

	if want := "Bearer " + fakeKey; gotAuth != want {
		t.Errorf("Authorization header is %q, want %q — if the key is not sent, "+
			"the no-leak tests pass for the wrong reason", gotAuth, want)
	}
	if gotAccept != "text/event-stream" {
		t.Errorf("Accept header is %q, want text/event-stream", gotAccept)
	}

	var sent struct {
		Model    string       `json:"model"`
		Stream   bool         `json:"stream"`
		Provider provider.Pin `json:"provider"`
		Temp     float64      `json:"temperature"`
		Messages []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(gotBody, &sent); err != nil {
		t.Fatalf("decoding the request this client sent: %v\n%s", err, gotBody)
	}
	if sent.Model != f.ModelID {
		t.Errorf("model %q, want %q", sent.Model, f.ModelID)
	}
	if !sent.Stream {
		t.Error("stream is false; SLICE-1's provider streams and Ctrl-C has to cancel mid-reply")
	}
	if diff := cmp.Diff(pinOf(f), sent.Provider); diff != "" {
		t.Errorf("the pin on the wire (-want +got):\n%s\n"+
			"an unpinned A/B number is not evidence (ADR-0005 §2)", diff)
	}
	if sent.Temp != 0.2 {
		t.Errorf("temperature %v, want 0.2 sent explicitly — omitting it takes the provider's default", sent.Temp)
	}
	if len(sent.Messages) != 2 || sent.Messages[0].Role != "system" || sent.Messages[1].Content != "add a test" {
		t.Errorf("messages did not survive the encoding: %+v", sent.Messages)
	}
}

// TestUnpinnedRequestIsRefusedBeforeItIsSent holds the ADR-0005 §2 rule where it
// can still be acted on. A result with no pin cannot match the declared pin, so
// it is discarded — after it has been paid for.
func TestUnpinnedRequestIsRefusedBeforeItIsSent(t *testing.T) {
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { calls++ }))
	defer srv.Close()

	req := request(loadFixture(t))
	req.Pin = provider.Pin{}

	_, err := newClient(t, srv).Complete(t.Context(), req)
	if !errors.Is(err, provider.ErrUnpinned) {
		t.Fatalf("Complete with no pin: %v, want ErrUnpinned", err)
	}
	if calls != 0 {
		t.Errorf("the unpinned request reached the provider %d time(s); it must be refused before it is billable", calls)
	}
}

// TestRetriesA429WithTheInjectedDelays is the retry half of the card's
// done-when: the delays are the policy's, drawn from the injected RNG, taken
// from the injected clock, and asserted exactly.
func TestRetriesA429WithTheInjectedDelays(t *testing.T) {
	f := loadFixture(t)

	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		if calls <= 2 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusTooManyRequests)
			fmt.Fprint(w, `{"error":{"code":429,"message":"rate limited"}}`)
			return
		}
		writeSSE(t, w, f.Exchanges[0].Response.Stream)
	}))
	defer srv.Close()

	clock := &manualClock{}
	policy := provider.Retry{MaxAttempts: 4, Base: time.Second, Cap: 8 * time.Second}
	c := newClient(t, srv,
		provider.WithClock(clock),
		provider.WithRand(topOfInterval()),
		provider.WithRetry(policy),
	)

	stream, err := c.Complete(t.Context(), request(f))
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	drain(t, stream)
	_ = stream.Close()

	if calls != 3 {
		t.Errorf("the provider was called %d time(s), want 3 (two 429s and a success)", calls)
	}
	// Drawing the top of the interval every time makes the delays the ceilings,
	// which is the schedule to assert against.
	want := []time.Duration{policy.Ceiling(0), policy.Ceiling(1)}
	if diff := cmp.Diff(want, clock.delays()); diff != "" {
		t.Errorf("backoff delays (-want +got):\n%s", diff)
	}
}

// TestGivesUpAtTheCap is the other half: the attempt budget is a cap and not a
// suggestion, and the failure names the provider's last word rather than
// replacing it.
func TestGivesUpAtTheCap(t *testing.T) {
	const body = `{"error":{"code":503,"message":"upstream is having a day"}}`

	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		fmt.Fprint(w, body)
	}))
	defer srv.Close()

	clock := &manualClock{}
	policy := provider.Retry{MaxAttempts: 4, Base: time.Second, Cap: 2 * time.Second}
	c := newClient(t, srv,
		provider.WithClock(clock),
		provider.WithRand(topOfInterval()),
		provider.WithRetry(policy),
	)

	_, err := c.Complete(t.Context(), request(loadFixture(t)))
	if !errors.Is(err, provider.ErrRetriesExhausted) {
		t.Fatalf("Complete against a permanent 503: %v, want ErrRetriesExhausted", err)
	}

	var apiErr *provider.APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("the give-up error does not carry the provider's own failure: %v", err)
	}
	if apiErr.Status != http.StatusServiceUnavailable || apiErr.Body != body {
		t.Errorf("last failure is %d %q, want %d %q", apiErr.Status, apiErr.Body, http.StatusServiceUnavailable, body)
	}

	if calls != policy.MaxAttempts {
		t.Errorf("the provider was called %d time(s), want %d — the attempt budget is the cap", calls, policy.MaxAttempts)
	}
	// One delay fewer than there are attempts: the client does not wait after
	// the send it has already decided is the last one.
	want := []time.Duration{policy.Ceiling(0), policy.Ceiling(1), policy.Ceiling(2)}
	if diff := cmp.Diff(want, clock.delays()); diff != "" {
		t.Errorf("backoff delays (-want +got):\n%s", diff)
	}
}

// TestRetryObserverSeesEachRetryThenSuccess is KAN-851's done-when at the
// client level: a retry storm inside Complete is not just logged, it reaches
// an observer wired at construction — the seam a caller uses to journal it —
// with the same facts a journal.ProviderRetried event needs.
func TestRetryObserverSeesEachRetryThenSuccess(t *testing.T) {
	f := loadFixture(t)

	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		if calls <= 2 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusTooManyRequests)
			fmt.Fprint(w, `{"error":{"code":429,"message":"rate limited"}}`)
			return
		}
		writeSSE(t, w, f.Exchanges[0].Response.Stream)
	}))
	defer srv.Close()

	clock := &manualClock{}
	policy := provider.Retry{MaxAttempts: 4, Base: time.Second, Cap: 8 * time.Second}
	var seen []provider.RetryEvent
	c := newClient(t, srv,
		provider.WithClock(clock),
		provider.WithRand(topOfInterval()),
		provider.WithRetry(policy),
		provider.WithRetryObserver(func(_ context.Context, ev provider.RetryEvent) {
			seen = append(seen, ev)
		}),
	)

	req := request(f)
	req.Turn, req.Attempt = 3, 1
	stream, err := c.Complete(t.Context(), req)
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	drain(t, stream)
	_ = stream.Close()

	want := []provider.RetryEvent{
		{Turn: 3, Attempt: 1, Try: 1, OfTries: 4, Delay: policy.Ceiling(0), Cause: "http 429"},
		{Turn: 3, Attempt: 1, Try: 2, OfTries: 4, Delay: policy.Ceiling(1), Cause: "http 429"},
	}
	if diff := cmp.Diff(want, seen); diff != "" {
		t.Errorf("retry events (-want +got):\n%s", diff)
	}
}

// TestRetryObserverSeesExhaustion is the other half: an observer sees every
// retry right up to the give-up, not just the ones that led somewhere.
func TestRetryObserverSeesExhaustion(t *testing.T) {
	const body = `{"error":{"code":503,"message":"upstream is having a day"}}`

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		fmt.Fprint(w, body)
	}))
	defer srv.Close()

	clock := &manualClock{}
	policy := provider.Retry{MaxAttempts: 4, Base: time.Second, Cap: 2 * time.Second}
	var seen []provider.RetryEvent
	c := newClient(t, srv,
		provider.WithClock(clock),
		provider.WithRand(topOfInterval()),
		provider.WithRetry(policy),
		provider.WithRetryObserver(func(_ context.Context, ev provider.RetryEvent) {
			seen = append(seen, ev)
		}),
	)

	req := request(loadFixture(t))
	req.Turn, req.Attempt = 1, 1
	_, err := c.Complete(t.Context(), req)
	if !errors.Is(err, provider.ErrRetriesExhausted) {
		t.Fatalf("Complete against a permanent 503: %v, want ErrRetriesExhausted", err)
	}

	// One retry notification fewer than there are attempts: the client does
	// not wait — and so does not notify — after the send it has already
	// decided is the last one, exactly as it does not wait on TestGivesUpAtTheCap.
	want := []provider.RetryEvent{
		{Turn: 1, Attempt: 1, Try: 1, OfTries: 4, Delay: policy.Ceiling(0), Cause: "http 503"},
		{Turn: 1, Attempt: 1, Try: 2, OfTries: 4, Delay: policy.Ceiling(1), Cause: "http 503"},
		{Turn: 1, Attempt: 1, Try: 3, OfTries: 4, Delay: policy.Ceiling(2), Cause: "http 503"},
	}
	if diff := cmp.Diff(want, seen); diff != "" {
		t.Errorf("retry events (-want +got):\n%s", diff)
	}
}

// TestNoRetryObserverMeansNothingIsNotified. Nil is the default and the
// mock/replay provider's whole story: WithRetryObserver is never called there,
// so this is what "not wired" looks like from Complete's side, and it must not
// panic.
func TestNoRetryObserverMeansNothingIsNotified(t *testing.T) {
	f := loadFixture(t)
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		if calls == 1 {
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		writeSSE(t, w, f.Exchanges[0].Response.Stream)
	}))
	defer srv.Close()

	c := newClient(t, srv, provider.WithClock(&manualClock{}), provider.WithRand(topOfInterval()))

	stream, err := c.Complete(t.Context(), request(f))
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	drain(t, stream)
	_ = stream.Close()
}

// TestDoesNotRetryWhatCannotSucceed. A 4xx that is not 429 describes the
// request: sending it again spends the budget and the wall clock to be told the
// same thing later.
func TestDoesNotRetryWhatCannotSucceed(t *testing.T) {
	for _, status := range []int{
		http.StatusBadRequest,
		http.StatusUnauthorized,
		http.StatusPaymentRequired,
		http.StatusNotFound,
		http.StatusUnprocessableEntity,
	} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			var calls int
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				calls++
				w.WriteHeader(status)
				fmt.Fprintf(w, `{"error":{"code":%d}}`, status)
			}))
			defer srv.Close()

			clock := &manualClock{}
			c := newClient(t, srv, provider.WithClock(clock), provider.WithRand(topOfInterval()))

			_, err := c.Complete(t.Context(), request(loadFixture(t)))
			var apiErr *provider.APIError
			if !errors.As(err, &apiErr) || apiErr.Status != status {
				t.Fatalf("Complete against %d: %v, want an APIError carrying that status", status, err)
			}
			if errors.Is(err, provider.ErrRetriesExhausted) {
				t.Error("a non-retryable status was reported as exhausted retries")
			}
			if calls != 1 {
				t.Errorf("the provider was called %d time(s), want 1", calls)
			}
			if d := clock.delays(); len(d) != 0 {
				t.Errorf("the client backed off %v before giving up on a request that cannot succeed", d)
			}
		})
	}
}

// TestRetriesATransportFailure. Nothing was answered, so nothing was billed and
// nothing was half-printed — which is exactly the case where sending again is
// safe.
func TestRetriesATransportFailure(t *testing.T) {
	f := loadFixture(t)

	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		if calls == 1 {
			// Aborts the connection without a response and without a stack
			// trace in the test log.
			panic(http.ErrAbortHandler)
		}
		writeSSE(t, w, f.Exchanges[0].Response.Stream)
	}))
	defer srv.Close()

	clock := &manualClock{}
	c := newClient(t, srv, provider.WithClock(clock), provider.WithRand(topOfInterval()))

	stream, err := c.Complete(t.Context(), request(f))
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	drain(t, stream)
	_ = stream.Close()

	if calls != 2 {
		t.Errorf("the provider was called %d time(s), want 2", calls)
	}
	if d := clock.delays(); len(d) != 1 {
		t.Errorf("the client waited %v, want exactly one backoff", d)
	}
}

// TestCancellationDuringBackoffReturnsAtOnce. Ctrl-C while the client is waiting
// out a 429 must return, not finish the sleep first.
func TestCancellationDuringBackoffReturnsAtOnce(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()

	// A clock whose timer never fires: the only way out of the wait is the
	// context.
	clock := &manualClock{freeze: true}
	c := newClient(t, srv, provider.WithClock(clock), provider.WithRand(topOfInterval()))

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		_, err := c.Complete(ctx, request(loadFixture(t)))
		done <- err
	}()

	// Wait until the client is inside the backoff, then cancel.
	for len(clock.delays()) == 0 {
		time.Sleep(time.Millisecond)
	}
	cancel()

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Complete after cancellation: %v, want an error wrapping context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Complete did not return after its context was cancelled; the backoff is not cancellable")
	}
}

// TestCancellationMidStreamStopsTheReply drives the Ctrl-C case end to end,
// over HTTP rather than over a strings.Reader.
func TestCancellationMidStreamStopsTheReply(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, chunk(`{"index":0,"delta":{"content":"first"},"finish_reason":null}`)+"\n\n") //nolint:errcheck // the client is about to hang up.
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		// Hold the response open until the test is done with it.
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	defer srv.Close()
	defer close(release)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	stream, err := newClient(t, srv).Complete(ctx, request(loadFixture(t)))
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	defer stream.Close() //nolint:errcheck // the stream is being cancelled on purpose.

	if !stream.Next() {
		t.Fatalf("no delta arrived before cancellation: %v", stream.Err())
	}
	if got := stream.Delta().Text; got != "first" {
		t.Errorf("first delta is %q, want %q", got, "first")
	}

	cancel()
	for stream.Next() {
	}
	if !errors.Is(stream.Err(), context.Canceled) {
		t.Fatalf("stream error after cancellation: %v, want an error wrapping context.Canceled", stream.Err())
	}
	if _, err := stream.Reply(); err == nil {
		t.Error("a cancelled stream handed back a reply; a half-read answer must not look like a whole one")
	}
}

// TestNonStreamedSuccessGoesThroughTheBodyPath. OpenRouter answers some requests
// with a plain JSON object and a 200 — an error object, or a proxy that declined
// to stream. Feeding that to the SSE reader would report "malformed stream",
// which is true about the bytes and useless about what happened.
func TestNonStreamedSuccessGoesThroughTheBodyPath(t *testing.T) {
	f := loadFixture(t)
	body := f.Exchanges[0].Response.Body

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		w.Write(body) //nolint:errcheck // a short write fails the assertions below.
	}))
	defer srv.Close()

	stream, err := newClient(t, srv).Complete(t.Context(), request(f))
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	defer stream.Close() //nolint:errcheck // the assertions below cover the stream's own error.
	drain(t, stream)

	reply, err := stream.Reply()
	if err != nil {
		t.Fatalf("Reply: %v", err)
	}
	if string(reply.Raw) != string(body) {
		t.Errorf("Reply.Raw is not the body the provider sent\n got: %s\nwant: %s", reply.Raw, body)
	}
	if reply.FinishReason != f.Exchanges[0].Expect.FinishReason {
		t.Errorf("finish reason %q, fixture says %q", reply.FinishReason, f.Exchanges[0].Expect.FinishReason)
	}
}

// TestFailedResponseBodyIsNeverTruncated. The body of a failed provider call is
// the diagnostic, and clipping it exactly where a reader looks is the specific
// failure this project designs out.
func TestFailedResponseBodyIsNeverTruncated(t *testing.T) {
	big := strings.Repeat("x", 300*1024)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		io.WriteString(w, big) //nolint:errcheck // a short write fails the assertion below.
	}))
	defer srv.Close()

	_, err := newClient(t, srv).Complete(t.Context(), request(loadFixture(t)))
	var apiErr *provider.APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("Complete: %v, want an APIError", err)
	}
	if len(apiErr.Body) != len(big) {
		t.Errorf("the failure body is %d bytes, the provider sent %d", len(apiErr.Body), len(big))
	}
}

// TestNewClientRefusesAnEmptyKey. A client without a credential fails every
// request with a 401, which reads like a provider outage and is a configuration
// error.
func TestNewClientRefusesAnEmptyKey(t *testing.T) {
	if _, err := provider.NewClient(provider.APIKey{}); !errors.Is(err, provider.ErrNoAPIKey) {
		t.Fatalf("NewClient with no key: %v, want ErrNoAPIKey", err)
	}
}

// TestConcurrentCompletesDoNotRaceOnTheJitterSource. The engine's loop is
// sequential, but a client is a shared value and *rand.Rand is not safe for
// concurrent use — so the one call into it is serialised rather than documented
// as somebody else's problem. -race is what makes this assert anything.
func TestConcurrentCompletesDoNotRaceOnTheJitterSource(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()

	c := newClient(t, srv,
		provider.WithClock(&manualClock{}),
		provider.WithRetry(provider.Retry{MaxAttempts: 3, Base: time.Nanosecond, Cap: time.Nanosecond}),
	)

	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = c.Complete(t.Context(), request(loadFixture(t)))
		}()
	}
	wg.Wait()
}
