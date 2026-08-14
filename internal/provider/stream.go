package provider

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
)

// SSE framing. The transport is plain text/event-stream: `data: ` lines each
// carrying one JSON object, a blank line between events, and a final
// `data: [DONE]`.
//
// OpenRouter also sends SSE *comment* lines — a line beginning with a colon —
// as keep-alives while it waits on an upstream, and its documentation says in
// as many words to skip lines starting with ":" before calling a JSON parser. A
// client that does not is one JSON error away from dropping a stream, so the
// skip is here and a fixture carries a keep-alive to drive it.
const (
	sseDataPrefix    = "data: "
	sseCommentPrefix = ":"
	sseDone          = "[DONE]"
)

// maxFrameBytes bounds one SSE line.
//
// bufio.Scanner's default is 64 KiB, which a single chunk carrying a large
// argument blob could exceed, and the failure mode is a truncated stream
// reported as a scan error rather than as a provider fault. 1 MiB per line is
// generous for a chunk and still a bound; a line over it fails loudly rather
// than being clipped, because a clipped chunk is a silently wrong reply.
const maxFrameBytes = 1 << 20

// Sentinel causes, for errors.Is.
var (
	// ErrStreamIncomplete reports a stream that stopped before the provider
	// said it was finished — no [DONE] sentinel, or no chunks at all. It is the
	// difference between "the model stopped" and "the connection did", which
	// the bench classifier must not confuse.
	ErrStreamIncomplete = errors.New("provider: stream ended before the provider finished")

	// ErrStreamNotFinished reports Reply being asked for before the stream was
	// drained. Returning a zero Reply there would hand the loop a reply the
	// model never sent.
	ErrStreamNotFinished = errors.New("provider: reply is not assembled yet")

	// ErrMalformedStream reports a frame this build could not read.
	ErrMalformedStream = errors.New("provider: malformed stream")
)

// DeltaKind says what a [Delta] carries.
type DeltaKind uint8

const (
	// DeltaUnknown is the zero value and is never emitted.
	DeltaUnknown DeltaKind = iota
	// DeltaContent is assistant text.
	DeltaContent
	// DeltaReasoning is reasoning-token text, which the journal keeps separate
	// from the answer and the REPL renders differently.
	DeltaReasoning
)

var deltaKindText = map[DeltaKind]string{
	DeltaUnknown:   "unknown",
	DeltaContent:   "content",
	DeltaReasoning: "reasoning",
}

// String returns the wire form of the kind.
func (k DeltaKind) String() string {
	if s, ok := deltaKindText[k]; ok {
		return s
	}
	return fmt.Sprintf("delta_kind(%d)", uint8(k))
}

// Delta is one increment of a reply as it arrives.
type Delta struct {
	Kind DeltaKind
	Text string
}

// Stream is one reply, delivered as it arrives.
//
// The shape is bufio.Scanner's, for the reason Scanner has it: a loop that
// reads until there is no more, then asks once what went wrong.
//
//	for s.Next() {
//	    print(s.Delta().Text)
//	}
//	if s.Err() != nil { … }
//	reply, err := s.Reply()
//
// A Stream is pulled by one goroutine and is not safe for concurrent use. It
// starts none of its own: every byte is decoded inside the caller's call to
// Next, so the order of deltas is a function of the bytes and not of the
// scheduler. That is what a byte-identical replayed journal rests on.
//
// Cancellation is checked before each delta is produced, so a context cancelled
// mid-reply stops the stream at the next increment rather than after it has
// finished — which is what Ctrl-C during a long reply has to do.
type Stream struct {
	ctx     context.Context
	scan    *bufio.Scanner
	closer  io.Closer
	raw     json.RawMessage
	wire    bytes.Buffer
	acc     accumulator
	pending []Delta
	current Delta
	sawDone bool
	sawData bool
	done    bool
	err     error
}

// NewSSEStream reads a streamed reply from an SSE body.
//
// raw is the *assembled* response body the reply should be journaled under —
// one JSON completion object. The replay provider has one recorded beside the
// frames and passes it through, so a replayed journal records the bytes the
// provider sent rather than a re-encoding of them.
//
// A live streaming client has none, and passes nil. That is not an omission and
// it is not fixed by assembling one: OpenRouter never sent an assembled body, so
// building one out of the chunks would put a re-encoding in the record under a
// field whose whole point is that it is not one. What the provider did send is
// the frames, which is what [Stream.Transcript] returns.
//
// body is closed by [Stream.Close] when it is an io.Closer, which is what stops
// a cancelled stream from leaking the response.
func NewSSEStream(ctx context.Context, body io.Reader, raw json.RawMessage) *Stream {
	if ctx == nil {
		ctx = context.Background()
	}

	s := &Stream{ctx: ctx, raw: raw}
	// Tee before the scanner, not after it: bufio.Scanner strips the line
	// terminators, so frames rebuilt from its tokens would be a reconstruction
	// of the wire and not the wire. Reading through the tee costs one copy of
	// the reply, which the journal was going to hold anyway.
	scan := bufio.NewScanner(io.TeeReader(body, &s.wire))
	scan.Buffer(make([]byte, 0, 64*1024), maxFrameBytes)
	s.scan = scan

	if c, ok := body.(io.Closer); ok {
		s.closer = c
	}
	return s
}

// Transcript returns the bytes read from the provider so far, verbatim.
//
// For a streamed reply this is what [Reply].Raw is for an assembled one: the
// record of what actually arrived, suitable for journal.ProviderResponse.Body,
// with nothing re-encoded and nothing dropped — keep-alive comments, blank
// separators and the [DONE] sentinel included.
//
// "So far" is literal. The scanner reads ahead, so mid-stream this can hold more
// than has been delivered as deltas; after the stream is drained it is the whole
// body. It is empty for a stream built by [NewBodyStream], which read no wire.
func (s *Stream) Transcript() []byte {
	return append([]byte(nil), s.wire.Bytes()...)
}

// NewBodyStream serves a reply that was not streamed: one assembled body,
// delivered as a single content delta.
//
// It exists because a fixture recorded without streaming, or a provider call
// made without it, still has to reach the loop through the same type. The
// deltas are synthetic and say so: one for the whole content, which is the most
// a non-streamed reply can honestly claim about arrival order.
func NewBodyStream(ctx context.Context, raw json.RawMessage) *Stream {
	if ctx == nil {
		ctx = context.Background()
	}
	s := &Stream{ctx: ctx, raw: raw}

	var body completion
	if err := json.Unmarshal(raw, &body); err != nil {
		s.err = fmt.Errorf("%w: decoding response body: %w", ErrMalformedStream, err)
		s.done = true
		return s
	}
	if err := s.acc.addBody(body); err != nil {
		s.err = fmt.Errorf("%w: %w", ErrMalformedStream, err)
		s.done = true
		return s
	}

	if text := s.acc.reasoning.String(); text != "" {
		s.pending = append(s.pending, Delta{Kind: DeltaReasoning, Text: text})
	}
	if text := s.acc.content.String(); text != "" {
		s.pending = append(s.pending, Delta{Kind: DeltaContent, Text: text})
	}
	s.sawData, s.sawDone = true, true
	return s
}

// Next advances to the next delta, reporting false when the reply is finished
// or has failed. Ask [Stream.Err] which.
func (s *Stream) Next() bool {
	if s.done {
		return false
	}
	if err := s.ctx.Err(); err != nil {
		s.fail(fmt.Errorf("provider: stream cancelled: %w", err))
		return false
	}
	if len(s.pending) > 0 {
		s.current, s.pending = s.pending[0], s.pending[1:]
		return true
	}
	if s.scan == nil {
		s.finish()
		return false
	}

	for s.scan.Scan() {
		if err := s.ctx.Err(); err != nil {
			s.fail(fmt.Errorf("provider: stream cancelled: %w", err))
			return false
		}
		deltas, err := s.frame(s.scan.Text())
		if err != nil {
			s.fail(err)
			return false
		}
		if len(deltas) == 0 {
			continue
		}
		s.current, s.pending = deltas[0], deltas[1:]
		return true
	}
	if err := s.scan.Err(); err != nil {
		s.fail(fmt.Errorf("provider: reading stream: %w", err))
		return false
	}

	s.finish()
	return false
}

// frame folds one SSE line, returning any deltas it carried.
func (s *Stream) frame(line string) ([]Delta, error) {
	switch {
	case line == "":
		// The blank separator between events.
		return nil, nil
	case strings.HasPrefix(line, sseCommentPrefix):
		// A keep-alive comment. Carries no payload by definition, and handing
		// it to a JSON decoder is how a client loses a stream.
		return nil, nil
	case !strings.HasPrefix(line, sseDataPrefix):
		return nil, fmt.Errorf("%w: line is neither a %q line, an SSE comment nor blank: %q",
			ErrMalformedStream, sseDataPrefix, line)
	}

	payload := strings.TrimPrefix(line, sseDataPrefix)
	if payload == sseDone {
		s.sawDone = true
		return nil, nil
	}
	if s.sawDone {
		return nil, fmt.Errorf("%w: a frame followed the %s sentinel: %q", ErrMalformedStream, sseDone, line)
	}

	var chunk completion
	if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
		return nil, fmt.Errorf("%w: decoding chunk: %w", ErrMalformedStream, err)
	}
	s.sawData = true

	deltas, err := s.acc.addChunk(chunk)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrMalformedStream, err)
	}
	return deltas, nil
}

// finish ends a stream that ran out of frames, deciding whether it ended or was
// cut off.
func (s *Stream) finish() {
	s.done = true
	switch {
	case !s.sawData:
		s.err = fmt.Errorf("%w: it carried no chunks at all", ErrStreamIncomplete)
	case !s.sawDone:
		s.err = fmt.Errorf("%w: no %s%s sentinel arrived", ErrStreamIncomplete, sseDataPrefix, sseDone)
	}
}

// fail records the first error and stops the stream.
func (s *Stream) fail(err error) {
	s.done = true
	if s.err == nil {
		s.err = err
	}
}

// Delta returns the increment [Stream.Next] just advanced to.
func (s *Stream) Delta() Delta { return s.current }

// Err reports why the stream stopped, or nil when it finished cleanly.
func (s *Stream) Err() error { return s.err }

// Reply returns the assembled reply.
//
// It is an error rather than a zero value before the stream is drained, and an
// error when the stream failed: a half-read reply that looks like a whole one
// is how a cancelled turn gets recorded as a model answer.
func (s *Stream) Reply() (Reply, error) {
	if s.err != nil {
		return Reply{}, s.err
	}
	if !s.done {
		return Reply{}, ErrStreamNotFinished
	}
	return s.acc.reply(s.raw), nil
}

// Close releases the underlying body. It is safe to call more than once and on
// a stream that was never read, which is what makes it usable in a defer.
//
// Closing a stream that has not finished fails it: there is no reply to be had
// from a body nobody read to the end, and returning the fragment that did
// arrive would record a partial answer as a whole one.
func (s *Stream) Close() error {
	if !s.done {
		s.fail(fmt.Errorf("%w: it was closed before it finished", ErrStreamIncomplete))
	}
	if s.closer == nil {
		return nil
	}
	c := s.closer
	s.closer = nil
	return c.Close()
}
