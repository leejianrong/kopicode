package provider_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/leejianrong/kopicode/internal/provider"
	"github.com/leejianrong/kopicode/internal/provider/fixture"
)

// This file drives the SSE path with hand-written frames, so the edge cases
// that a fixture does not (and should not) contain — a truncated stream, a
// frame after the sentinel, a body that is not SSE at all — are exercised
// somewhere. The fixture-driven replay lives in internal/provider/mock, which
// is where the recorded traffic belongs.

// frames joins SSE lines into the bytes they arrive as, blank separators
// included.
func frames(lines ...string) io.Reader {
	return strings.NewReader(strings.Join(lines, "\n") + "\n")
}

// chunk renders one `data: ` line carrying a chunk with the given choice body.
func chunk(choice string) string {
	return `data: {"id":"gen-1","object":"chat.completion.chunk","created":1767225600,` +
		`"model":"m","provider":"P","choices":[` + choice + `]}`
}

const doneFrame = "data: [DONE]"

// usageFrame is the trailing chunk carrying the token accounting and nothing
// else, which is one of the two spellings OpenRouter is known to use.
const usageFrame = `data: {"id":"gen-1","object":"chat.completion.chunk","created":1767225600,` +
	`"model":"m","provider":"P","choices":[],"usage":{"prompt_tokens":10,"completion_tokens":4,"total_tokens":14}}`

// drain pulls a stream to the end and returns the deltas it produced.
func drain(t *testing.T, s *provider.Stream) []provider.Delta {
	t.Helper()
	var out []provider.Delta
	for s.Next() {
		out = append(out, s.Delta())
	}
	return out
}

func TestStreamDeliversContentInArrivalOrder(t *testing.T) {
	s := provider.NewSSEStream(t.Context(), frames(
		fixture.KeepAliveComment,
		"",
		chunk(`{"index":0,"delta":{"role":"assistant","content":""},"finish_reason":null}`),
		"",
		chunk(`{"index":0,"delta":{"content":"Hello, "},"finish_reason":null}`),
		"",
		chunk(`{"index":0,"delta":{"content":"world"},"finish_reason":null}`),
		"",
		chunk(`{"index":0,"delta":{"content":null},"finish_reason":"stop"}`),
		"",
		usageFrame,
		"",
		doneFrame,
	), json.RawMessage(`{"raw":true}`))

	got := drain(t, s)
	want := []provider.Delta{
		{Kind: provider.DeltaContent, Text: "Hello, "},
		{Kind: provider.DeltaContent, Text: "world"},
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("deltas (-want +got):\n%s", diff)
	}
	if err := s.Err(); err != nil {
		t.Fatalf("Err() = %v, want nil", err)
	}

	reply, err := s.Reply()
	if err != nil {
		t.Fatalf("Reply() error: %v", err)
	}
	if reply.Content != "Hello, world" {
		t.Errorf("content = %q, want %q", reply.Content, "Hello, world")
	}
	if reply.FinishReason != "stop" {
		t.Errorf("finish reason = %q, want %q", reply.FinishReason, "stop")
	}
	if reply.ServedBy != "P" {
		t.Errorf("served by = %q, want %q", reply.ServedBy, "P")
	}
	if want := (provider.Usage{Prompt: 10, Completion: 4, Total: 14}); reply.Usage != want {
		t.Errorf("usage = %+v, want %+v", reply.Usage, want)
	}
	if string(reply.Raw) != `{"raw":true}` {
		t.Errorf("raw = %s, want the body passed in verbatim", reply.Raw)
	}
}

// TestStreamEmitsReasoningBeforeContent pins the order within one chunk. The
// journal keeps a ThinkingBlock separate from the answer, and a REPL that
// printed them the other way round would be describing a different session.
func TestStreamEmitsReasoningBeforeContent(t *testing.T) {
	s := provider.NewSSEStream(t.Context(), frames(
		chunk(`{"index":0,"delta":{"reasoning":"thinking","content":"answer"},"finish_reason":null}`),
		"",
		chunk(`{"index":0,"delta":{},"finish_reason":"stop"}`),
		"",
		usageFrame,
		"",
		doneFrame,
	), nil)

	got := drain(t, s)
	want := []provider.Delta{
		{Kind: provider.DeltaReasoning, Text: "thinking"},
		{Kind: provider.DeltaContent, Text: "answer"},
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("deltas (-want +got):\n%s", diff)
	}
	reply, err := s.Reply()
	if err != nil {
		t.Fatalf("Reply() error: %v", err)
	}
	if reply.Reasoning != "thinking" || reply.Content != "answer" {
		t.Errorf("reply reasoning/content = %q/%q, want %q/%q",
			reply.Reasoning, reply.Content, "thinking", "answer")
	}
}

// TestStreamMergesToolCallFragmentsByIndex is the bug OpenRouter's own snippet
// contains: appending fragments in arrival order rather than merging them by
// index. Two interleaved calls is what tells the two implementations apart.
func TestStreamMergesToolCallFragmentsByIndex(t *testing.T) {
	s := provider.NewSSEStream(t.Context(), frames(
		chunk(`{"index":0,"delta":{"tool_calls":[{"index":1,"id":"call_b","type":"function","function":{"name":"grep","arguments":""}}]},"finish_reason":null}`),
		"",
		chunk(`{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_a","type":"function","function":{"name":"read_file","arguments":"{\"path\": "}}]},"finish_reason":null}`),
		"",
		chunk(`{"index":0,"delta":{"tool_calls":[{"index":1,"function":{"arguments":"{\"pattern\": \"x\"}"}}]},"finish_reason":null}`),
		"",
		chunk(`{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"a.go\"}"}}]},"finish_reason":"tool_calls"}`),
		"",
		usageFrame,
		"",
		doneFrame,
	), nil)

	if got := drain(t, s); len(got) != 0 {
		t.Errorf("tool-call fragments produced %d delta(s); a partial call is not something the loop "+
			"can act on, so it is delivered assembled instead: %+v", len(got), got)
	}
	reply, err := s.Reply()
	if err != nil {
		t.Fatalf("Reply() error: %v", err)
	}

	want := []provider.ToolCall{
		// First-seen index order, which is call_b: it opened the stream even
		// though its index is higher.
		{ID: "call_b", Name: "grep", Arguments: json.RawMessage(`"{\"pattern\": \"x\"}"`)},
		{ID: "call_a", Name: "read_file", Arguments: json.RawMessage(`"{\"path\": \"a.go\"}"`)},
	}
	if diff := cmp.Diff(want, reply.ToolCalls); diff != "" {
		t.Errorf("tool calls (-want +got):\n%s", diff)
	}
}

// TestStreamKeepsArgumentBytesUnescaped. The arguments value reaches the
// journal through parse, so a mapping that ran it back through the escaping
// encoder would change bytes the record is supposed to preserve.
func TestStreamKeepsArgumentBytesUnescaped(t *testing.T) {
	s := provider.NewSSEStream(t.Context(), frames(
		chunk(`{"index":0,"delta":{"tool_calls":[{"index":0,"id":"c","type":"function","function":{"name":"write_file","arguments":"{\"content\": \"a < b && c > d\"}"}}]},"finish_reason":"tool_calls"}`),
		"",
		usageFrame,
		"",
		doneFrame,
	), nil)
	drain(t, s)

	reply, err := s.Reply()
	if err != nil {
		t.Fatalf("Reply() error: %v", err)
	}
	got := string(reply.ToolCalls[0].Arguments)
	// The three runes encoding/json escapes by default, spelled the way it
	// spells them rather than written out here.
	for _, r := range []rune{'<', '>', '&'} {
		if escape := fmt.Sprintf(`\u%04x`, r); strings.Contains(got, escape) {
			t.Errorf("arguments came back HTML-escaped (%s): %s", escape, got)
		}
	}
	var decoded string
	if err := json.Unmarshal(reply.ToolCalls[0].Arguments, &decoded); err != nil {
		t.Fatalf("arguments are not a JSON string: %v", err)
	}
	if want := `{"content": "a < b && c > d"}`; decoded != want {
		t.Errorf("arguments decode to %q, want %q", decoded, want)
	}
}

// TestStreamAcceptsABareArgumentsObject. The documented delta shape carries
// arguments as a JSON string, but providers differ and fixture's wire notes say
// so. A whole call in one delta with an object argument must not be dropped.
func TestStreamAcceptsABareArgumentsObject(t *testing.T) {
	s := provider.NewSSEStream(t.Context(), frames(
		chunk(`{"index":0,"delta":{"tool_calls":[{"index":0,"id":"c","type":"function","function":{"name":"read_file","arguments":{"path":"a.go"}}}]},"finish_reason":"tool_calls"}`),
		"",
		usageFrame,
		"",
		doneFrame,
	), nil)
	drain(t, s)

	reply, err := s.Reply()
	if err != nil {
		t.Fatalf("Reply() error: %v", err)
	}
	if got, want := string(reply.ToolCalls[0].Arguments), `{"path":"a.go"}`; got != want {
		t.Errorf("arguments = %s, want %s", got, want)
	}
}

func TestStreamRefusesMixedArgumentEncodings(t *testing.T) {
	s := provider.NewSSEStream(t.Context(), frames(
		chunk(`{"index":0,"delta":{"tool_calls":[{"index":0,"id":"c","type":"function","function":{"name":"read_file","arguments":"{\"path\":"}}]},"finish_reason":null}`),
		"",
		chunk(`{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":{"path":"a.go"}}}]},"finish_reason":null}`),
		"",
		doneFrame,
	), nil)
	drain(t, s)

	if !errors.Is(s.Err(), provider.ErrMalformedStream) {
		t.Fatalf("Err() = %v, want ErrMalformedStream", s.Err())
	}
}

func TestStreamFailuresAreTypedAndSpecific(t *testing.T) {
	cases := []struct {
		name  string
		lines []string
		want  error
	}{
		{
			name:  "a line that is neither data, comment nor blank",
			lines: []string{"event: message", doneFrame},
			want:  provider.ErrMalformedStream,
		},
		{
			name: "a frame after the sentinel",
			lines: []string{
				chunk(`{"index":0,"delta":{"content":"hi"},"finish_reason":"stop"}`),
				doneFrame,
				chunk(`{"index":0,"delta":{"content":"more"},"finish_reason":null}`),
			},
			want: provider.ErrMalformedStream,
		},
		{
			name:  "a chunk that is not JSON",
			lines: []string{"data: {not json", doneFrame},
			want:  provider.ErrMalformedStream,
		},
		{
			name:  "a chunk carrying two choices",
			lines: []string{`data: {"object":"chat.completion.chunk","choices":[{"index":0},{"index":1}]}`, doneFrame},
			want:  provider.ErrMalformedStream,
		},
		{
			name: "no sentinel — the connection ended, the model did not",
			lines: []string{
				chunk(`{"index":0,"delta":{"content":"hi"},"finish_reason":"stop"}`),
			},
			want: provider.ErrStreamIncomplete,
		},
		{
			name:  "no chunks at all",
			lines: []string{fixture.KeepAliveComment, "", doneFrame},
			want:  provider.ErrStreamIncomplete,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := provider.NewSSEStream(t.Context(), frames(tc.lines...), nil)
			drain(t, s)

			if !errors.Is(s.Err(), tc.want) {
				t.Fatalf("Err() = %v, want %v", s.Err(), tc.want)
			}
			if _, err := s.Reply(); !errors.Is(err, tc.want) {
				t.Errorf("Reply() error = %v, want %v — a failed stream must not hand back a reply", err, tc.want)
			}
		})
	}
}

// errReader fails part way through, which is what a dropped connection looks
// like to the decoder.
type errReader struct {
	head string
	read bool
}

func (r *errReader) Read(p []byte) (int, error) {
	if !r.read {
		r.read = true
		return copy(p, r.head), nil
	}
	return 0, fmt.Errorf("connection reset")
}

func TestStreamReportsATransportFailure(t *testing.T) {
	s := provider.NewSSEStream(t.Context(), &errReader{head: chunk(`{"index":0,"delta":{"content":"hi"},"finish_reason":null}`) + "\n"}, nil)
	drain(t, s)

	if s.Err() == nil {
		t.Fatal("Err() = nil, want the read failure")
	}
	if !strings.Contains(s.Err().Error(), "connection reset") {
		t.Errorf("Err() = %v, want it to name the underlying failure", s.Err())
	}
}

func TestReplyIsRefusedBeforeTheStreamIsDrained(t *testing.T) {
	s := provider.NewSSEStream(t.Context(), frames(
		chunk(`{"index":0,"delta":{"content":"hi"},"finish_reason":"stop"}`),
		"",
		usageFrame,
		"",
		doneFrame,
	), nil)

	if _, err := s.Reply(); !errors.Is(err, provider.ErrStreamNotFinished) {
		t.Fatalf("Reply() before Next returned %v, want ErrStreamNotFinished", err)
	}
	if !s.Next() {
		t.Fatalf("Next() = false, want the first delta: %v", s.Err())
	}
	if _, err := s.Reply(); !errors.Is(err, provider.ErrStreamNotFinished) {
		t.Fatalf("Reply() mid-stream returned %v, want ErrStreamNotFinished — a half-read reply that "+
			"looks whole is how a cancelled turn gets recorded as a model answer", err)
	}
	drain(t, s)
	if _, err := s.Reply(); err != nil {
		t.Fatalf("Reply() after draining: %v", err)
	}
}

func TestClosingAnUnfinishedStreamRefusesAReply(t *testing.T) {
	s := provider.NewSSEStream(t.Context(), frames(
		chunk(`{"index":0,"delta":{"content":"hi"},"finish_reason":null}`),
		"",
		chunk(`{"index":0,"delta":{"content":" there"},"finish_reason":"stop"}`),
		"",
		usageFrame,
		"",
		doneFrame,
	), nil)

	if !s.Next() {
		t.Fatalf("Next() = false, want the first delta: %v", s.Err())
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close(): %v", err)
	}
	if _, err := s.Reply(); !errors.Is(err, provider.ErrStreamIncomplete) {
		t.Fatalf("Reply() after an early Close returned %v, want ErrStreamIncomplete", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("second Close(): %v — Close must be safe to repeat so it can sit in a defer", err)
	}
}

// closeCounter records that the body was released, which is what stops a
// cancelled stream from leaking a response.
type closeCounter struct {
	io.Reader
	closes int
}

func (c *closeCounter) Close() error { c.closes++; return nil }

func TestCloseReleasesTheBody(t *testing.T) {
	body := &closeCounter{Reader: frames(doneFrame)}
	s := provider.NewSSEStream(t.Context(), body, nil)
	drain(t, s)

	if err := s.Close(); err != nil {
		t.Fatalf("Close(): %v", err)
	}
	if body.closes != 1 {
		t.Fatalf("body closed %d times, want exactly 1", body.closes)
	}
	if err := s.Close(); err != nil || body.closes != 1 {
		t.Fatalf("second Close() = %v with %d closes, want nil and no second close", err, body.closes)
	}
}

func TestCancellationBeforeTheFirstDelta(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	s := provider.NewSSEStream(ctx, frames(
		chunk(`{"index":0,"delta":{"content":"hi"},"finish_reason":"stop"}`),
		"",
		doneFrame,
	), nil)

	if s.Next() {
		t.Fatal("Next() = true on a cancelled context")
	}
	if !errors.Is(s.Err(), context.Canceled) {
		t.Fatalf("Err() = %v, want it to wrap context.Canceled", s.Err())
	}
}

// TestCancellationMidStream is the Ctrl-C case: the reply is arriving, the user
// gives up, and the stream must stop at the next increment rather than after
// the whole reply has been read.
func TestCancellationMidStream(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	s := provider.NewSSEStream(ctx, frames(
		chunk(`{"index":0,"delta":{"content":"one"},"finish_reason":null}`),
		"",
		chunk(`{"index":0,"delta":{"content":"two"},"finish_reason":null}`),
		"",
		chunk(`{"index":0,"delta":{"content":"three"},"finish_reason":"stop"}`),
		"",
		usageFrame,
		"",
		doneFrame,
	), nil)

	var got []provider.Delta
	for s.Next() {
		got = append(got, s.Delta())
		cancel()
	}

	if len(got) != 1 {
		t.Fatalf("read %d delta(s) after cancelling on the first, want 1: %+v", len(got), got)
	}
	if !errors.Is(s.Err(), context.Canceled) {
		t.Fatalf("Err() = %v, want it to wrap context.Canceled", s.Err())
	}
	if _, err := s.Reply(); !errors.Is(err, context.Canceled) {
		t.Errorf("Reply() = %v, want the cancellation — a cancelled turn is not a model answer", err)
	}
}

func TestBodyStreamServesANonStreamedReply(t *testing.T) {
	raw := json.RawMessage(`{"id":"gen-9","object":"chat.completion","created":1,"model":"m","provider":"P",` +
		`"choices":[{"index":0,"message":{"role":"assistant","content":"done","tool_calls":[` +
		`{"id":"c1","type":"function","function":{"name":"read_file","arguments":"{\"path\": \"a.go\"}"}}]},` +
		`"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":3,"completion_tokens":2,"total_tokens":5}}`)

	s := provider.NewBodyStream(t.Context(), raw)
	got := drain(t, s)
	want := []provider.Delta{{Kind: provider.DeltaContent, Text: "done"}}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("deltas (-want +got):\n%s", diff)
	}

	reply, err := s.Reply()
	if err != nil {
		t.Fatalf("Reply() error: %v", err)
	}
	if reply.FinishReason != "tool_calls" || len(reply.ToolCalls) != 1 {
		t.Fatalf("reply = %+v, want one tool call finishing with tool_calls", reply)
	}
	if got, want := string(reply.ToolCalls[0].Arguments), `"{\"path\": \"a.go\"}"`; got != want {
		t.Errorf("arguments = %s, want the wire bytes %s", got, want)
	}
	if string(reply.Raw) != string(raw) {
		t.Error("Raw is not the body that was passed in")
	}
}

func TestBodyStreamRefusesRubbish(t *testing.T) {
	s := provider.NewBodyStream(t.Context(), json.RawMessage(`{"choices":[]}`))
	drain(t, s)
	if !errors.Is(s.Err(), provider.ErrMalformedStream) {
		t.Fatalf("Err() = %v, want ErrMalformedStream", s.Err())
	}
}

// TestStreamIsDeterministic reads the same bytes twice and requires the same
// deltas and the same assembled reply. It is the provider's half of the
// byte-identical-journal criterion: whatever else varies in a replayed session,
// it does not vary here.
func TestStreamIsDeterministic(t *testing.T) {
	lines := []string{
		fixture.KeepAliveComment,
		"",
		chunk(`{"index":0,"delta":{"role":"assistant","content":""},"finish_reason":null}`),
		"",
		chunk(`{"index":0,"delta":{"reasoning":"hmm"},"finish_reason":null}`),
		"",
		chunk(`{"index":0,"delta":{"tool_calls":[{"index":2,"id":"c2","type":"function","function":{"name":"grep","arguments":"{}"}}]},"finish_reason":null}`),
		"",
		chunk(`{"index":0,"delta":{"tool_calls":[{"index":0,"id":"c0","type":"function","function":{"name":"read_file","arguments":"{}"}}]},"finish_reason":null}`),
		"",
		chunk(`{"index":0,"delta":{"tool_calls":[{"index":1,"id":"c1","type":"function","function":{"name":"list_dir","arguments":"{}"}}]},"finish_reason":"tool_calls"}`),
		"",
		usageFrame,
		"",
		doneFrame,
	}

	trace := func() string {
		s := provider.NewSSEStream(t.Context(), frames(lines...), json.RawMessage(`{"raw":1}`))
		var b strings.Builder
		for s.Next() {
			fmt.Fprintf(&b, "%s\t%q\n", s.Delta().Kind, s.Delta().Text)
		}
		if s.Err() != nil {
			t.Fatalf("stream failed: %v", s.Err())
		}
		reply, err := s.Reply()
		if err != nil {
			t.Fatalf("Reply(): %v", err)
		}
		out, err := json.Marshal(reply)
		if err != nil {
			t.Fatalf("marshalling reply: %v", err)
		}
		b.Write(out)
		return b.String()
	}

	// Ten runs rather than two: map iteration order is randomised per range
	// statement, so a single repeat can agree by luck. Three tool calls whose
	// indices arrive out of order are what would expose it.
	first := trace()
	for i := range 10 {
		if got := trace(); got != first {
			t.Fatalf("run %d differs from the first:\n%s\nvs\n%s", i+2, got, first)
		}
	}
	if !strings.Contains(first, `"c2"`) {
		t.Fatalf("the trace does not mention the tool calls; it would pass over anything:\n%s", first)
	}
}
