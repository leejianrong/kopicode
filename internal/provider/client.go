package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"strings"
	"sync"
	"time"
)

// This file is the live OpenRouter client (docs/SLICE-1.md §Build Plan step 8).
//
// # What it is, in one paragraph
//
// One POST to /chat/completions with stream=true, the pin the caller handed it,
// and the credential in a header. A 429 or a 5xx is sent again after a capped
// exponential backoff with full jitter, drawn from an injected clock and RNG so
// the delays are assertable; anything else is reported as it arrived. On success
// the response body is handed to [NewSSEStream] — the same decoder the replay
// provider drives — and the caller pulls it.
//
// # Where the credential is, and is not
//
// It is on the client, in an [APIKey], and it reaches exactly one place: the
// Authorization header, set in newHTTPRequest. It is in no error message, no log
// record and no returned value, which key_test.go asserts by formatting every
// value this package hands back under every verb fmt consults a Stringer for.
//
// Three leak paths are realistic and each is closed deliberately rather than by
// luck. An error that wraps the *http.Request would carry the header map, so
// nothing here formats a request — net/http's own *url.Error carries the method
// and the URL and neither holds a credential. A debug log of the client's
// configuration would carry the key, so [APIKey] renders as [Redacted] to
// log/slog through LogValue. And a recorded fixture would carry the request
// headers, which is why fixtures hold none at all (internal/provider/fixture).
//
// # What it does not do yet, deliberately
//
// It does not honour a `Retry-After` header on a 429. The card this was built
// for asks for delays that are assertable from an injected clock and RNG, and a
// provider-supplied delay is neither — it would silently replace the policy
// under test with whatever the server said, so a test asserting the backoff
// would be asserting the fixture. Honouring it as a *floor* is the right end
// state and wants its own card; until then the policy is the whole story.
//
// # Retries are visible outside engine.Provider, not through it (KAN-851)
//
// engine.Provider stays one method on purpose (SLICE-1 §Build Plan step 8):
// widening it to report retries would let the loop start making policy out of
// them. So a [RetryObserver], not a return value, is how a retry storm inside
// one Complete call reaches the journal. It is injected the way the clock and
// the RNG are — a [ClientOption] set at construction — and called synchronously,
// in this goroutine, once per retry, with the same facts the diagnostic slog
// line already carries. A caller building the live client for a real session
// wires it straight to journal.Journal.Append; engine.Provider's caller never
// sees it and Complete's return value is exactly what it always was.
//
// journal.ProviderRequest.Attempt is a different count and stays one: it is the
// *engine's* own re-send, once per call to Complete (a turn, or a repair round
// trip), and it cannot see inside a single Complete call any more than
// engine.Provider can. A request the live client retried six times against 429s
// before succeeding still journals one ProviderRequest — the retries are
// [journal.ProviderRetried] events beside it, not a bigger Attempt.
//
// # What the client does not decide
//
// The pin. It arrives on [Request] and is written to the wire verbatim, because
// a client that carried its own copy would be a fourth place the project's pin
// is written down and the first one nobody would think to check against
// docs/provider-pin.md. ADR-0007 decision 5 puts the value in the registry that
// resolves a model id; this package takes it as an argument.

// DefaultBaseURL is OpenRouter's API root.
const DefaultBaseURL = "https://openrouter.ai/api/v1"

// completionsPath is the chat-completions endpoint under the base URL.
const completionsPath = "/chat/completions"

// DefaultHTTPTimeout bounds one attempt's *headers*, not the reply.
//
// It is deliberately not an http.Client.Timeout: that one covers reading the
// body too, so a long streamed answer would be cut off in the middle by a
// setting that was meant to catch a provider that never answers at all. The
// bound that belongs on a stream is the caller's context, and cancelling it
// stops the stream at its next increment.
const DefaultHTTPTimeout = 60 * time.Second

// Sentinel causes, for errors.Is.
var (
	// ErrRetriesExhausted reports a request that was retried up to the policy's
	// cap and failed every time. The last failure is wrapped beside it, so a
	// caller can still ask what the provider actually said.
	ErrRetriesExhausted = errors.New("provider: retries exhausted")

	// ErrUnpinned reports a request with no provider.order.
	//
	// It fails closed rather than sending the request unpinned, because an
	// unpinned result is indistinguishable from a pinned one after the fact —
	// it is served by whoever OpenRouter felt like, at whatever quantization,
	// and it lands in the same journal as the rest of the run
	// (docs/adr/0005-benchmark-and-ab-methodology.md §2).
	ErrUnpinned = errors.New("provider: request declares no provider pin")
)

// RetryEvent is one client-internal retry: an attempt that failed with a
// retryable cause, about to be followed by a backoff and another send.
//
// It carries exactly what the diagnostic slog line in Complete already
// computes, because the two exist to say the same thing to two different
// audiences — engine-internal diagnostics on stderr, and a durable record for
// whoever measures the arm afterward.
type RetryEvent struct {
	// Turn and Attempt echo [Request].Turn and [Request].Attempt verbatim, so a
	// retry can be correlated back to the ProviderRequest it belongs to. Zero
	// when the caller left them unstated, exactly as on Request.
	Turn    int
	Attempt int
	// Try is 1-based: which send within this Complete call just failed. 1 is
	// the first send, so a Try of 3 means two retries have already happened
	// for this request.
	Try int
	// OfTries is the retry policy's attempt budget for this request —
	// [Retry.MaxAttempts], resolved against [DefaultRetry].
	OfTries int
	// Delay is the backoff drawn before the next send.
	Delay time.Duration
	// Cause summarises the failure that triggered the retry: "http 429",
	// "http 503", "transport failure" — [cause]'s own classification.
	Cause string
}

// RetryObserver is notified once per client-internal retry, synchronously and
// in the goroutine running Complete — the same contract [Config.Stream] makes
// for a delta, and for the same reason: no goroutine and no buffering in an
// output path that a replayed journal has to reproduce byte for byte.
//
// It is injected at construction ([WithRetryObserver]), not reachable through
// [Client.Complete]'s signature, and that is deliberate: engine.Provider is one
// method, and an observer wired here is how a retry storm reaches a journal
// without widening it. Nil is the default and means nothing is notified —
// exactly the mock/replay path, which never retries because it never sends
// anything over a wire.
type RetryObserver func(ctx context.Context, ev RetryEvent)

// Client is the live OpenRouter provider.
//
// It satisfies engine.Provider structurally and does not import the engine: the
// interface is declared where it is consumed, and this package depends on
// nothing of the loop's.
//
// A Client is safe for concurrent use. The jitter source is not — a *rand.Rand
// is not — so the one call into it is serialised here rather than documented as
// the caller's problem.
type Client struct {
	key      APIKey
	baseURL  string
	http     *http.Client
	clock    Clock
	retry    Retry
	log      *slog.Logger
	referer  string
	title    string
	unpinned bool
	onRetry  RetryObserver

	mu   sync.Mutex
	rand Rand
}

// defaultTransport bounds the wait for response *headers* without bounding the
// reply.
//
// http.Client.Timeout would have been one line and is the wrong line: it covers
// reading the body, so a model that is still answering after a minute would have
// its stream cut by a setting meant to catch a provider that never answers at
// all.
func defaultTransport() http.RoundTripper {
	t, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		// Unreachable unless something replaced the package default. Falling
		// back to it keeps requests working without the header bound.
		return http.DefaultTransport
	}
	clone := t.Clone()
	clone.ResponseHeaderTimeout = DefaultHTTPTimeout
	return clone
}

// A ClientOption configures [NewClient].
type ClientOption func(*Client)

// WithBaseURL points the client at a different API root — an httptest server in
// this package's tests, and nothing else today.
func WithBaseURL(u string) ClientOption {
	return func(c *Client) {
		if u != "" {
			c.baseURL = strings.TrimSuffix(u, "/")
		}
	}
}

// WithHTTPClient replaces the http.Client. Nil is ignored.
func WithHTTPClient(h *http.Client) ClientOption {
	return func(c *Client) {
		if h != nil {
			c.http = h
		}
	}
}

// WithClock injects the clock the backoff delays are taken from.
func WithClock(clock Clock) ClientOption {
	return func(c *Client) {
		if clock != nil {
			c.clock = clock
		}
	}
}

// WithRand injects the jitter source. *math/rand/v2.Rand satisfies it.
func WithRand(r Rand) ClientOption {
	return func(c *Client) {
		if r != nil {
			c.rand = r
		}
	}
}

// WithRetry replaces the retry policy.
func WithRetry(r Retry) ClientOption {
	return func(c *Client) { c.retry = r }
}

// WithLogger sends engine-internal diagnostics — which attempt, which status,
// how long the backoff was — to lg.
//
// Diagnostics only. The journal owns session content and slog never duplicates
// it (CLAUDE.md, "Boundaries that must not be crossed"), so nothing here logs a
// message, a reply or a tool call. Nil is ignored and the default discards.
func WithLogger(lg *slog.Logger) ClientOption {
	return func(c *Client) {
		if lg != nil {
			c.log = lg
		}
	}
}

// WithRetryObserver injects the callback notified once per client-internal
// retry. Nil (the default) notifies nothing, which is the mock/replay path.
//
// It is the same shape of injection as [WithClock] and [WithRand]: a value
// supplied at construction rather than threaded through [Client.Complete],
// because Complete satisfies engine.Provider and that interface stays one
// method. See [RetryObserver] for what a caller does with it.
func WithRetryObserver(obs RetryObserver) ClientOption {
	return func(c *Client) { c.onRetry = obs }
}

// WithAttribution sets OpenRouter's optional HTTP-Referer and X-Title headers,
// which name the calling app on its dashboards. Both are omitted when empty.
func WithAttribution(referer, title string) ClientOption {
	return func(c *Client) { c.referer, c.title = referer, title }
}

// WithoutPin permits a request that declares no provider routing.
//
// It exists for the one honest case — poking at a provider by hand from a
// scratch program — and is off by default because the alternative default makes
// [ErrUnpinned] a lint nobody reads. Never set it on a benchmark run: ADR-0005
// §2 discards a result whose pin does not match the declared pin, and a result
// with no pin at all cannot match one.
func WithoutPin() ClientOption {
	return func(c *Client) { c.unpinned = true }
}

// NewClient builds a client for one credential.
//
// An empty key is refused here rather than at the first request. A client
// without a credential fails every call with a 401, which reads like a provider
// outage and is a configuration error — the same misreporting ADR-0007 decision
// 4 refuses for an unknown model id.
func NewClient(key APIKey, opts ...ClientOption) (*Client, error) {
	if key.IsZero() {
		return nil, ErrNoAPIKey
	}
	c := &Client{
		key:     key,
		baseURL: DefaultBaseURL,
		http:    &http.Client{Transport: defaultTransport()},
		clock:   RealClock{},
		retry:   DefaultRetry,
		log:     slog.New(slog.DiscardHandler),
		rand:    globalRand{},
	}
	for _, opt := range opts {
		opt(c)
	}
	return c, nil
}

// String renders the client's configuration without its credential.
//
// It is not decoration. [APIKey]'s own String is only consulted when the key is
// the operand fmt was handed, or a field it can reach — and fmt cannot reach an
// *unexported* field through reflection, so it falls back to printing the raw
// struct. `fmt.Sprintf("%v", client)` therefore printed the credential in full
// until this method existed, which key_test.go caught and now keeps caught.
//
// Declared on the pointer because that is how a Client is held; a copy of one
// would be a copied mutex, which go vet refuses.
func (c *Client) String() string {
	return fmt.Sprintf("provider.Client{base_url: %s, key: %s, retry: %d attempts, base %s, cap %s}",
		c.baseURL, Redacted, c.retry.maxAttempts(), c.retry.base(), c.retry.capped())
}

// GoString renders the client under %#v, which would otherwise reach past every
// unexported field including the key.
func (c *Client) GoString() string { return c.String() }

// Complete sends one request and returns the reply as a stream.
//
// It satisfies engine.Provider. An error here is a request that never produced a
// reply; a reply that arrived and then failed part way through is reported by
// the stream, because the deltas that did arrive are evidence.
//
// Retrying stops the moment a stream exists. A partially consumed reply has
// already been printed, already been counted by the provider, and re-sending
// over the top of it would bill the prompt twice and hand the loop two halves of
// two different answers.
func (c *Client) Complete(ctx context.Context, req Request) (*Stream, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	body, err := c.encode(req)
	if err != nil {
		return nil, err
	}

	attempts := c.retry.maxAttempts()
	var last error
	for attempt := range attempts {
		stream, err := c.send(ctx, body)
		if err == nil {
			return stream, nil
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			// Cancellation is not a provider failure and is never retried.
			return nil, fmt.Errorf("provider: request cancelled: %w", ctxErr)
		}
		if !retryable(err) {
			return nil, err
		}
		last = err

		if attempt == attempts-1 {
			break
		}
		delay := c.delay(attempt)
		failureCause := cause(err)
		c.log.LogAttrs(ctx, slog.LevelDebug, "provider request failed, retrying",
			slog.Int("attempt", attempt+1),
			slog.Int("of", attempts),
			slog.Duration("backoff", delay),
			slog.String("cause", failureCause),
		)
		if c.onRetry != nil {
			// Outside engine.Provider's one method by construction: this fires
			// from inside Complete, before it returns anything, and Complete's
			// signature carries nothing new because of it. See RetryObserver.
			c.onRetry(ctx, RetryEvent{
				Turn: req.Turn, Attempt: req.Attempt,
				Try: attempt + 1, OfTries: attempts,
				Delay: delay, Cause: failureCause,
			})
		}
		if err := wait(ctx, c.clock, delay); err != nil {
			return nil, err
		}
	}

	return nil, fmt.Errorf("%w after %d attempt(s): %w", ErrRetriesExhausted, attempts, last)
}

// delay draws one backoff, serialising access to the jitter source.
func (c *Client) delay(attempt int) time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.retry.Delay(attempt, c.rand)
}

// send makes one attempt, returning a stream or the reason there is none.
func (c *Client) send(ctx context.Context, body []byte) (*Stream, error) {
	httpReq, err := c.newHTTPRequest(ctx, body)
	if err != nil {
		return nil, err
	}

	resp, err := c.http.Do(httpReq) //nolint:bodyclose // closed on every path below: by drain on a failure, by Stream.Close on success.
	if err != nil {
		// net/http wraps this as *url.Error, which carries the method, the URL
		// and the cause — and no headers, so no credential. The request itself
		// is never formatted into an error anywhere in this package.
		return nil, &TransportError{Err: err}
	}

	if resp.StatusCode != http.StatusOK {
		return nil, drain(resp)
	}

	// A 200 that is not an event stream is OpenRouter answering without
	// streaming — an error object served with a 200, or a proxy that declined
	// the stream. Handing that to the SSE reader would report it as a malformed
	// stream, which is a true statement about the bytes and a useless one about
	// what happened, so it goes through the assembled-body path instead.
	if !isEventStream(resp.Header.Get("Content-Type")) {
		raw, err := readAll(resp)
		if err != nil {
			return nil, err
		}
		return NewBodyStream(ctx, raw), nil
	}

	return NewSSEStream(ctx, resp.Body, nil), nil
}

// newHTTPRequest builds one attempt's HTTP request.
//
// This is the only place the credential is read. Keep it that way: a second
// caller of APIKey.reveal is a second place to audit.
func (c *Client) newHTTPRequest(ctx context.Context, body []byte) (*http.Request, error) {
	url := c.baseURL + completionsPath
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("provider: building request for %s: %w", url, err)
	}
	req.Header.Set("Authorization", "Bearer "+c.key.reveal())
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	if c.referer != "" {
		req.Header.Set("HTTP-Referer", c.referer)
	}
	if c.title != "" {
		req.Header.Set("X-Title", c.title)
	}
	return req, nil
}

// TransportError is a request that never reached a provider response: DNS,
// connection, TLS, or a read that failed before the status line.
//
// It is a type rather than a wrapped sentinel because whether it is worth
// sending again is a property of the failure and not of the caller's mood, and
// [retryable] asks the error rather than string-matching it.
type TransportError struct{ Err error }

func (e *TransportError) Error() string { return "provider: transport: " + e.Err.Error() }
func (e *TransportError) Unwrap() error { return e.Err }

// cause summarises a failure for a log line, without the response body.
//
// The body belongs in the returned error, where the journal's append-time
// redaction covers it. It does not belong in a log line: nothing redacts those,
// and a provider that echoed the request's Authorization header into an error
// body — which is not hypothetical for a misconfigured gateway — would put the
// credential on a terminal by way of a diagnostic nobody thought of as an output
// path. Status and class are what a retry decision was made from anyway.
func cause(err error) string {
	var api *APIError
	if errors.As(err, &api) {
		return fmt.Sprintf("http %d", api.Status)
	}
	var transport *TransportError
	if errors.As(err, &transport) {
		return "transport failure"
	}
	return "request failed"
}

// retryable reports whether an attempt's failure is worth repeating.
//
// A transport failure is: nothing was answered, so nothing was billed and
// nothing was half-printed. A status is retryable when [retryableStatus] says
// so. Everything else — a body this build could not read, a request it could not
// build — is deterministic and would fail again identically.
func retryable(err error) bool {
	var transport *TransportError
	if errors.As(err, &transport) {
		return true
	}
	var api *APIError
	if errors.As(err, &api) {
		return retryableStatus(api.Status)
	}
	return false
}

// drain reads a failed response whole and turns it into an [APIError].
//
// Whole, and never truncated: the body of a failed provider call is the
// diagnostic — the rate-limit window, the moderation reason, the model id it did
// not recognise — and clipping it exactly where a reader looks is the specific
// failure this project designs out (CLAUDE.md, "Boundaries that must not be
// crossed").
func drain(resp *http.Response) error {
	defer resp.Body.Close() //nolint:errcheck // the response failed; a close error adds nothing to report.
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return &APIError{
			Status: resp.StatusCode,
			Body:   string(body) + fmt.Sprintf("\n[body truncated by a read error: %v]", err),
		}
	}
	return &APIError{Status: resp.StatusCode, Body: string(body)}
}

// readAll reads a successful non-streamed body whole.
func readAll(resp *http.Response) (json.RawMessage, error) {
	defer resp.Body.Close() //nolint:errcheck // the body has been read; a close error adds nothing to report.
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, &TransportError{Err: fmt.Errorf("reading response body: %w", err)}
	}
	return body, nil
}

// isEventStream reports whether a Content-Type names SSE. Parameters
// (`; charset=utf-8`) are stripped, and an unparsable value is treated as not a
// stream so the bytes are kept whole rather than fed to a decoder that will
// reject them.
func isEventStream(contentType string) bool {
	if contentType == "" {
		return false
	}
	mediaType, _, err := mime.ParseMediaType(contentType)
	if err != nil {
		return false
	}
	return mediaType == "text/event-stream"
}

// wireRequest is the chat-completions request body.
//
// Tools is the wire's tool-definition array (KAN-844), rendered from
// [Request.Tools] by [RenderTools] — the harness's parse.Schema catalogue
// translated into the JSON Schema shape a provider expects, at this
// wire-building boundary and nowhere upstream, the same discipline
// [Request.Tools]'s own doc comment states. Omitted from the wire when empty:
// see that comment for why "no key" and "empty array" are not offered as the
// same thing here.
type wireRequest struct {
	Model    string           `json:"model"`
	Messages []wireMessage    `json:"messages"`
	Stream   bool             `json:"stream"`
	Provider Pin              `json:"provider"`
	Tools    []ToolDefinition `json:"tools,omitempty"`

	// Temperature is always sent: 0 is a meaningful value (greedy decoding) and
	// omitting it would silently take the provider's default instead.
	Temperature float64 `json:"temperature"`
	// TopP and MaxTokens are omitted when zero, which is not a value either of
	// them can take — top_p must be positive, and max_tokens: 0 asks for no
	// answer.
	TopP      float64 `json:"top_p,omitempty"`
	MaxTokens int     `json:"max_tokens,omitempty"`
	Seed      *int64  `json:"seed,omitempty"`
}

// wireMessage is one conversation entry on the wire.
type wireMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
	// ToolCalls is the assistant's calls, echoed back so the provider sees the
	// conversation it produced.
	ToolCalls []wireCall `json:"tool_calls,omitempty"`
	// ToolCallID is set on a tool result and names the call it answers.
	ToolCallID string `json:"tool_call_id,omitempty"`
}

// encode renders a [Request] as the request body, refusing one that is not
// pinned.
func (c *Client) encode(req Request) ([]byte, error) {
	if req.ModelID == "" {
		return nil, errors.New("provider: request names no model")
	}
	if len(req.Pin.Order) == 0 && !c.unpinned {
		return nil, fmt.Errorf("%w: %s\n"+
			"every request carries provider.order, allow_fallbacks: false and a fixed quantization "+
			"(docs/adr/0005-benchmark-and-ab-methodology.md §2, docs/provider-pin.md); the value comes "+
			"from the model registry and not from this package",
			ErrUnpinned, req.Pin)
	}

	w := wireRequest{
		Model:       req.ModelID,
		Stream:      true,
		Provider:    req.Pin,
		Tools:       RenderTools(req.Tools),
		Temperature: req.Sampling.Temperature,
		TopP:        req.Sampling.TopP,
		MaxTokens:   req.Sampling.MaxTokens,
		Seed:        req.Sampling.Seed,
	}
	if w.Provider.Order == nil {
		w.Provider.Order = []string{}
	}
	if w.Provider.Quantizations == nil {
		w.Provider.Quantizations = []string{}
	}
	for _, m := range req.Messages {
		wm := wireMessage{Role: string(m.Role), Content: m.Content, ToolCallID: m.ToolCallID}
		for _, tc := range m.ToolCalls {
			wm.ToolCalls = append(wm.ToolCalls, wireCall{
				ID:       tc.ID,
				Type:     "function",
				Function: wireFunction{Name: tc.Name, Arguments: tc.Arguments},
			})
		}
		w.Messages = append(w.Messages, wm)
	}
	if w.Messages == nil {
		w.Messages = []wireMessage{}
	}

	// Escaping stays off for the same reason journal.Marshal turns it off: an
	// argument value carrying a `<` would go out as a unicode escape, which
	// decodes back to the same string and is not the same bytes — and these
	// bytes are what a recorded fixture would hold.
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(w); err != nil {
		return nil, fmt.Errorf("provider: encoding request: %w", err)
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}
