package fixture

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// Recorder is an [http.RoundTripper] that turns real provider traffic into
// [Fixture] data (KAN-774), so a fixture stops being something a human
// invents and starts being something a live call produced.
//
// # Why a RoundTripper, and not a copy of the request-building logic
//
// The live client (internal/provider/client.go) already builds the request,
// sets the pin, and drives the retry policy; duplicating any of that here
// would be a second place for the wire format to drift. A RoundTripper sits
// underneath all of it — [Recorder] wraps whatever [http.RoundTripper] the
// client was going to use (typically [http.DefaultTransport]) and is handed
// to [provider.NewClient] through provider.WithHTTPClient, exactly the seam
// client_test.go already drives against an httptest.Server. The client never
// knows it is being recorded.
//
// # Why the credential never reaches the file
//
// The request carries OPENROUTER_API_KEY in its Authorization header
// (internal/provider/client.go, newHTTPRequest — "the only place the
// credential is read"). Recorder never reads that header for any reason: it
// keeps only the request headers named in [allowedRequestHeaders], and every
// other header — Authorization included — is dropped while the request is
// still in memory, before a [Fixture] value exists to put it in. This is an
// allowlist and not a denylist on purpose (CLAUDE.md, "Boundaries that must
// not be crossed" and docs/SLICE-1.md §Build Plan step 3): a denylist is
// correct only until the provider adds a second auth-bearing header, and the
// header nobody thought to add to the denylist is the one that leaks. The
// response side reuses [allowedResponseHeaders], which already governs every
// fixture in the data directory.
//
// # Turn and Attempt
//
// Neither travels on the wire — they are the engine's own bookkeeping
// (journal.ProviderRequest.Attempt is the analogue) — so a caller recording a
// real session attaches them to the request's context with [WithMeta] before
// calling Complete. A request recorded with no metadata is filed as turn 1,
// attempt 1, which is enough for a single-exchange recording and wrong for
// anything longer, which is exactly why [WithMeta] exists.
//
// # What Recorder does not do
//
// It does not validate what it builds — call [Validate] on the result, the
// same as any other fixture, before it is trusted. It does not decide the
// pin or the model: both are read back out of the request body every
// provider.Client already sends, the same way [Validate] reads them out of a
// hand-authored fixture rather than being told them twice. And it does not
// record a request that failed before a response arrived: a transport error
// has no reply to scrub and nothing this format has room for.
type Recorder struct {
	next http.RoundTripper

	mu        sync.Mutex
	exchanges []Exchange
	modelID   string
	pin       Pin
	errs      []error
}

// NewRecorder wraps next, which handles the actual send. A nil next uses
// [http.DefaultTransport], which is never wrong for a caller pointed at a
// real endpoint and only ever wrong in a test, which supplies its own.
func NewRecorder(next http.RoundTripper) *Recorder {
	if next == nil {
		next = http.DefaultTransport
	}
	return &Recorder{next: next}
}

// recordMeta is the per-request bookkeeping [WithMeta] attaches to a
// context, because Turn and Attempt are not on the wire for RoundTrip to
// read back out.
type recordMeta struct {
	Turn    int
	Attempt int
	Note    string
}

type recordMetaKey struct{}

// WithMeta attaches the turn, attempt and note a recorded request belongs to,
// the way a caller building a real session would before handing ctx to
// [provider.Client.Complete]. [Recorder] cannot discover any of these three
// from the HTTP exchange itself — they are the engine's bookkeeping, not the
// wire's.
func WithMeta(ctx context.Context, turn, attempt int, note string) context.Context {
	return context.WithValue(ctx, recordMetaKey{}, recordMeta{Turn: turn, Attempt: attempt, Note: note})
}

// metaFrom reads back what [WithMeta] attached, defaulting to a single
// exchange's worth of bookkeeping when the caller set none — enough to record
// one request without ceremony, and visibly wrong (every exchange claims
// turn 1 attempt 1) the moment there is more than one.
func metaFrom(ctx context.Context) recordMeta {
	if m, ok := ctx.Value(recordMetaKey{}).(recordMeta); ok {
		return m
	}
	return recordMeta{Turn: 1, Attempt: 1, Note: "recorded"}
}

// recordedRequest is the sliver of the chat-completions request body this
// package needs back out: the model and the pin, both of which land in
// [Fixture] fields a response never carries. It is declared locally, the way
// wire.go declares the response shapes locally, rather than importing
// internal/provider's own wireRequest — this package imports nothing of the
// engine's or the client's, by design (see the package doc).
type recordedRequest struct {
	Model    string `json:"model"`
	Provider Pin    `json:"provider"`
}

// RoundTrip sends req unchanged through next and captures the exchange it
// draws, scrubbing headers on both sides before anything is kept in memory
// as a candidate [Exchange].
//
// A transport failure (next.RoundTrip returning an error) is passed straight
// through and recorded nowhere: nothing came back to scrub, and a request
// that never got a reply is not evidence about the provider.
func (r *Recorder) RoundTrip(req *http.Request) (*http.Response, error) {
	meta := metaFrom(req.Context())

	reqBody, err := drainAndRestore(req)
	if err != nil {
		return nil, fmt.Errorf("fixture: recorder: reading request body: %w", err)
	}
	var rr recordedRequest
	if len(reqBody) > 0 {
		if err := json.Unmarshal(reqBody, &rr); err != nil {
			return nil, fmt.Errorf("fixture: recorder: decoding request body: %w", err)
		}
	}
	reqHeaders := scrubHeaders(req.Header, allowedRequestHeaders)

	resp, err := r.next.RoundTrip(req)
	if err != nil {
		return resp, err
	}

	respHeaders := scrubHeaders(resp.Header, allowedResponseHeaders)
	streaming := isEventStreamContentType(resp.Header.Get("Content-Type"))
	status := resp.StatusCode

	resp.Body = &capturingBody{
		underlying: resp.Body,
		finish: func(raw []byte) {
			r.record(meta, rr, status, reqHeaders, respHeaders, streaming, raw)
		},
	}
	return resp, nil
}

// record turns one captured exchange into a candidate [Exchange] and keeps
// it, or keeps the reason it could not, so a caller learns about a
// malformed recording from [Recorder.Fixture] rather than from a panic
// half way through a real session.
func (r *Recorder) record(meta recordMeta, rr recordedRequest, status int, reqHeaders, respHeaders map[string]string, streaming bool, raw []byte) {
	ex, err := buildExchange(meta, status, reqHeaders, respHeaders, streaming, raw)

	r.mu.Lock()
	defer r.mu.Unlock()
	if err != nil {
		r.errs = append(r.errs, fmt.Errorf("turn %d attempt %d: %w", meta.Turn, meta.Attempt, err))
		return
	}
	r.exchanges = append(r.exchanges, ex)
	if r.modelID == "" {
		r.modelID = rr.Model
	}
	if len(r.pin.Order) == 0 {
		r.pin = rr.Provider
	}
}

// buildExchange assembles one [Exchange] from a captured response body,
// folding a streamed reply back into the same assembled shape a
// non-streamed one already has — see [assembleStream], which this reuses
// rather than re-deriving.
func buildExchange(meta recordMeta, status int, reqHeaders, respHeaders map[string]string, streaming bool, raw []byte) (Exchange, error) {
	var (
		body      completion
		lines     []string
		bodyBytes []byte
		err       error
	)
	if streaming {
		lines = splitSSELines(raw)
		body, err = assembleStream(lines)
		if err != nil {
			return Exchange{}, fmt.Errorf("assembling stream: %w", err)
		}
		bodyBytes, err = json.Marshal(body)
		if err != nil {
			return Exchange{}, fmt.Errorf("marshaling assembled body: %w", err)
		}
	} else {
		body, err = decodeBody(raw)
		if err != nil {
			return Exchange{}, fmt.Errorf("decoding body: %w", err)
		}
		bodyBytes = raw
	}

	return Exchange{
		Turn:           meta.Turn,
		Attempt:        meta.Attempt,
		Note:           meta.Note,
		RequestHeaders: reqHeaders,
		Response: Response{
			Status:  status,
			Headers: respHeaders,
			Body:    bodyBytes,
			Stream:  lines,
		},
		Expect: deriveExpect(body),
	}, nil
}

// deriveExpect states the facts [Exchange.Expect] carries, read out of the
// assembled body rather than asked of the caller a second time — the same
// facts [validateExpect] holds a hand-authored fixture to.
func deriveExpect(body completion) Expect {
	if len(body.Choices) == 0 {
		return Expect{}
	}
	c := body.Choices[0]
	var names []string
	var route string
	if c.Message != nil {
		for _, tc := range c.Message.ToolCalls {
			names = append(names, tc.Function.Name)
		}
		if len(names) > 0 {
			route = routeNative
		}
	}
	var usage Usage
	if body.Usage != nil {
		usage = Usage{
			Prompt:     body.Usage.PromptTokens,
			Completion: body.Usage.CompletionTokens,
			Total:      body.Usage.TotalTokens,
		}
	}
	return Expect{
		FinishReason: c.finishReason(),
		Route:        route,
		Tools:        names,
		Usage:        usage,
		ServedBy:     body.Provider,
	}
}

// Fixture assembles everything recorded so far into a [Fixture] named name,
// with description as its human-readable summary. It does not call
// [Validate] — a caller does that explicitly, the same way it would for any
// other fixture — because a recording in progress is allowed to be
// incomplete and Fixture is how a caller inspects it, not only how a caller
// finishes it.
//
// It fails if any exchange along the way could not be assembled, joining
// every recording error together rather than surfacing only the first, so a
// multi-turn recording session that went wrong twice is reported twice.
func (r *Recorder) Fixture(name, description string) (Fixture, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if len(r.errs) > 0 {
		return Fixture{}, fmt.Errorf("fixture: recorder: %w", errors.Join(r.errs...))
	}

	exchanges := make([]Exchange, len(r.exchanges))
	copy(exchanges, r.exchanges)

	return Fixture{
		FormatVersion: Version,
		Origin:        OriginRecorded,
		Name:          name,
		Description:   description,
		ModelID:       r.modelID,
		Pin:           r.pin,
		Exchanges:     exchanges,
	}, nil
}

// Write marshals f as indented JSON and writes it to <dir>/<f.Name>.json,
// returning the path written. It is the recorder's half of what [Load]
// reads: the same container, the same name-matches-filename contract [Load]
// checks.
func Write(dir string, f Fixture) (string, error) {
	if strings.TrimSpace(f.Name) == "" {
		return "", fmt.Errorf("fixture: recorder: writing a fixture with no name")
	}
	data, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return "", fmt.Errorf("fixture: recorder: encoding %q: %w", f.Name, err)
	}
	path := filepath.Join(dir, f.Name+".json")
	if err := os.WriteFile(path, append(data, '\n'), 0o600); err != nil {
		return "", fmt.Errorf("fixture: recorder: writing %q: %w", path, err)
	}
	return path, nil
}

// drainAndRestore reads req's body whole and replaces it with a fresh reader
// over the same bytes, so the request the recorder inspects is still the
// exact request that reaches the network.
func drainAndRestore(req *http.Request) ([]byte, error) {
	if req.Body == nil {
		return nil, nil
	}
	data, err := io.ReadAll(req.Body)
	closeErr := req.Body.Close()
	if err != nil {
		return nil, err
	}
	if closeErr != nil {
		return nil, closeErr
	}
	req.Body = io.NopCloser(bytes.NewReader(data))
	return data, nil
}

// scrubHeaders copies only the headers named in allow, lower-casing the
// name. Every header not named is gone before this function returns, not
// masked and not logged — [Recorder]'s whole claim rests on that being true
// of Authorization specifically, so nothing here special-cases it: it is
// absent from every allowlist this package defines, and that is the entire
// mechanism.
func scrubHeaders(h http.Header, allow map[string]bool) map[string]string {
	var out map[string]string
	for name, values := range h {
		key := strings.ToLower(name)
		if !allow[key] || len(values) == 0 {
			continue
		}
		if out == nil {
			out = make(map[string]string)
		}
		out[key] = values[0]
	}
	return out
}

// isEventStreamContentType reports whether a Content-Type names SSE. A local
// copy of internal/provider's isEventStream: this package imports nothing of
// the client's, the same restraint wire.go documents for the response
// shapes.
func isEventStreamContentType(contentType string) bool {
	if contentType == "" {
		return false
	}
	mediaType, _, err := mime.ParseMediaType(contentType)
	if err != nil {
		return false
	}
	return mediaType == "text/event-stream"
}

// splitSSELines inverts the write side of the wire: every recorded line was
// written as line+"\n", so raw ends in exactly one trailing newline beyond
// the blank separator line SSE already puts after a real event. Trimming
// that one newline before splitting recovers the same []string a
// hand-authored fixture's Response.Stream holds, blank separators and all.
func splitSSELines(raw []byte) []string {
	s := strings.TrimSuffix(string(raw), "\n")
	if s == "" {
		return nil
	}
	return strings.Split(s, "\n")
}

// capturingBody wraps a response body, buffering every byte read from it and
// calling finish exactly once with the whole thing — on EOF, or on Close if
// EOF was never reached because the caller stopped reading early.
type capturingBody struct {
	underlying io.ReadCloser
	buf        bytes.Buffer
	once       sync.Once
	finish     func(raw []byte)
}

func (b *capturingBody) Read(p []byte) (int, error) {
	n, err := b.underlying.Read(p)
	if n > 0 {
		b.buf.Write(p[:n])
	}
	if err == io.EOF {
		b.done()
	}
	return n, err
}

func (b *capturingBody) Close() error {
	b.done()
	return b.underlying.Close()
}

func (b *capturingBody) done() {
	b.once.Do(func() { b.finish(b.buf.Bytes()) })
}
