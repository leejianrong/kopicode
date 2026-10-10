package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strconv"
	"sync/atomic"
	"time"

	sessioncore "github.com/leejianrong/kopicode/cmd/kopicode/session"
	"github.com/leejianrong/kopicode/internal/engine"
)

// remoteConsentTimeout bounds every remoteConsenter.Ask call (ADR-0016
// decision 5). A remote peer that never answers — crashed, disconnected, or
// simply buggy — gives the running turn no equivalent of the REPL's "a human
// is assumed to eventually answer or hit Ctrl-C," so an unbounded wait would
// hang the turn silently. 60 seconds is long enough for an automated
// orchestrator to think and short enough that a dead peer does not hang a
// turn indefinitely. It is the default: `--consent-timeout` overrides it for
// a process (parseConsentTimeout), since a person answering through an
// orchestrator needs longer than a script does.
const remoteConsentTimeout = 60 * time.Second

// The bounds `--consent-timeout` accepts. Below the floor a timeout is
// indistinguishable from a broken peer; above the ceiling it is no longer a
// bound on anything, which is the failure ADR-0016 decision 5 exists to prevent.
const (
	minConsentTimeout = time.Second
	maxConsentTimeout = 24 * time.Hour
)

// parseConsentTimeout validates a `--consent-timeout` value. Zero is refused,
// not read as "no timeout": an unbounded wait is the one thing this flag may
// never express.
func parseConsentTimeout(d time.Duration) (time.Duration, error) {
	if d < minConsentTimeout || d > maxConsentTimeout {
		return 0, fmt.Errorf("--consent-timeout %s is outside %s to %s; a consent request must always be bounded "+
			"so a peer that never answers cannot hang a turn (ADR-0016 decision 5)", d, minConsentTimeout, maxConsentTimeout)
	}
	return d, nil
}

// consentRequestParams is consent.request's params: engine.ConsentRequest's
// fields, copied across field-for-field the same discipline session.event's
// eventParams already holds for journal payloads, plus the session id so a
// client juggling several concurrent sessions can tell whose question this is.
type consentRequestParams struct {
	Session  string `json:"session"`
	Kind     string `json:"kind"`
	Tool     string `json:"tool"`
	Detail   string `json:"detail"`
	Reason   string `json:"reason"`
	Resolved string `json:"resolved"`
	// Command is the exact line passed to `sh -c`, for a run_shell request whose
	// argv is `/bin/sh -c <line>`; absent otherwise. Detail carries the same
	// text behind a "/bin/sh -c " prefix and with the argv joined by spaces.
	Command string `json:"command,omitempty"`
	// Argv is the exact argv, for a shell request; absent for a write.
	Argv []string `json:"argv,omitempty"`
}

// shellCommand returns the line a `/bin/sh -c <line>` argv runs, or "" for any
// other shape — including a shell argv that is not exactly that, which a client
// should read from Argv rather than have guessed at.
func shellCommand(argv []string) string {
	if len(argv) == 3 && argv[0] == "/bin/sh" && argv[1] == "-c" {
		return argv[2]
	}
	return ""
}

// consentResult is consent.request's result shape: engine.ConsentAnswer's
// three values spelled out as a string, matching every other enum this wire
// renders as text (decision, source, stop) rather than a bare int a client
// would have to already know this binary's internal encoding to read.
type consentResult struct {
	Answer string `json:"answer"`
	// Text is ask.request's answer, a pointer so a reply with no text field (a
	// malformed one) is told apart from an empty answer, which is a person who
	// read the question and had nothing to add (ADR-0009).
	Text *string `json:"text"`
	// Note is what to do instead, on a refusal: "use uv and a venv". It is
	// shown to the model in the denial it reads and journaled on the decision.
	// Ignored with any approval. Optional; absent is a bare refusal.
	Note string `json:"note,omitempty"`
}

// consentReply is what handleLine hands a waiting remoteConsenter once a
// client's reply line decodes: at most one of Result and Error, mirroring
// rpcResponse's own "exactly one of these is set" shape. Both nil is a
// malformed reply (a result that would not decode into consentResult).
type consentReply struct {
	Result *consentResult
	Err    *rpcError
}

// clock is remoteConsenter's injectable timer seam, the same shape
// internal/tools and internal/verify each keep as a package-local interface
// for their own subprocess timeouts rather than a shared type across
// packages — see agent-ground-rules on why duplicating this small interface
// beats a shared one three ways.
type clock interface {
	NewTimer(d time.Duration) (<-chan time.Time, func())
}

// realClock is the production clock. Every constructor below defaults to it;
// a test substitutes a fake one that never actually sleeps.
type realClock struct{}

func (realClock) NewTimer(d time.Duration) (<-chan time.Time, func()) {
	t := time.NewTimer(d)
	return t.C, func() { t.Stop() }
}

// remoteConsenter is ADR-0016's live consenter: an engine.Consenter that asks
// whatever is driving this session over the same wire session.event
// notifications already travel, and blocks the calling turn until an answer
// arrives, the turn's own context ends, or remoteConsentTimeout elapses.
//
// It is built once per session (buildConsentOptions, serve.go) and its Ask
// method is wired as Options.Consent — a third implementation of the
// Consenter func value, alongside the REPL's loop.Ask, following that same
// shape rather than inventing a new engine-level mechanism.
type remoteConsenter struct {
	srv     *server
	session string
	timeout time.Duration
	clock   clock
}

// newRemoteConsenter builds one for session. A zero timeout means the
// process-wide --consent-timeout; a session.start consent_timeout overrides it
// for this session alone. The clock is the production one.
func newRemoteConsenter(srv *server, session string, timeout time.Duration) *remoteConsenter {
	if timeout == 0 {
		timeout = srv.consentTimeout
	}
	return &remoteConsenter{srv: srv, session: session, timeout: timeout, clock: realClock{}}
}

// Ask satisfies engine.Consenter.
//
// Every outcome but a genuine allow answer resolves to (engine.ConsentDeny,
// nil) rather than a non-nil error — deliberately, and it is the one detail
// easy to get backwards. internal/permission.Gate.Check only journals a
// PermissionDecided (with this call's attributed Source) when
// policy.Decide returns cleanly; an error from Decide (which
// internal/permission.AskPolicy.Decide produces whenever this method itself
// errors) discards the Decision entirely, so the whole consent round trip —
// including the SourceRemote attribution ADR-0016 decision 5 requires — would
// vanish from the record exactly when it matters most: a timeout or a
// malformed reply. So a real Go error is reserved for the one case where the
// turn is ending anyway and there is nothing left to attribute — the turn's
// own context ending — mirroring cmd/kopicode/repl/consent.go's loop.Ask,
// which returns a clean ConsentDeny for an unrecognised human answer and an
// error only for cancellation or a broken input stream.
func (r *remoteConsenter) Ask(ctx context.Context, req engine.ConsentRequest) (engine.ConsentReply, error) {
	if err := ctx.Err(); err != nil {
		return engine.Deny(""), err
	}

	id, waiter := r.srv.registerConsentWaiter()
	defer r.srv.abandonConsentWaiter(id)
	defer r.srv.mgr.Awaiting(r.session, sessioncore.StateAwaitingConsent, id)()

	r.srv.write(rpcRequestOut{
		JSONRPC: jsonrpcVersion,
		ID:      quoteConsentID(id),
		Method:  methodConsentRequest,
		Params: consentRequestParams{
			Session:  r.session,
			Kind:     req.Kind,
			Tool:     req.Tool,
			Detail:   req.Detail,
			Reason:   req.Reason,
			Resolved: req.Resolved,
			Command:  shellCommand(req.Argv),
			Argv:     req.Argv,
		},
	})

	timer, stop := r.clock.NewTimer(r.timeout)
	defer stop()

	select {
	case reply := <-waiter:
		return decodeConsentReply(reply), nil
	case <-timer:
		slog.WarnContext(ctx, "remote consent timed out", "session", r.session, "kind", req.Kind,
			"tool", req.Tool, "timeout", r.timeout)
		return engine.Deny(""), nil
	case <-ctx.Done():
		return engine.Deny(""), ctx.Err()
	}
}

// decodeConsentReply maps a reply onto an answer. A refusal, a missing
// result, or an answer string this binary does not recognise all deny — the
// same "anything not affirmative is a refusal" rule
// cmd/kopicode/repl/consent.go's interpret holds human input to, applied here
// to a remote peer's reply instead of a typed line.
func decodeConsentReply(reply consentReply) engine.ConsentReply {
	if reply.Result == nil {
		return engine.Deny("")
	}
	switch reply.Result.Answer {
	case "allow":
		return engine.Reply(engine.ConsentAllow)
	case "allow_session":
		return engine.Reply(engine.ConsentAllowSession)
	default:
		// The note rides a refusal only, and any answer this binary does not
		// recognise is one. A note beside an approval is ignored.
		return engine.Deny(reply.Result.Note)
	}
}

// quoteConsentID renders id (always a plain "c-<n>" string this package
// mints) as the JSON string literal an rpcRequestOut.ID needs. strconv.Quote
// rather than json.Marshal because id's alphabet (ASCII letters, digits, one
// hyphen) never needs anything json.Marshal would escape differently, and this
// avoids a second, unchecked error return for a value that cannot fail to
// encode.
func quoteConsentID(id string) json.RawMessage {
	return json.RawMessage(strconv.Quote(id))
}

// registerConsentWaiter mints a server-side id unrelated to any client id
// namespace (client ids arrive in whatever shape a client chooses; server ids
// are always this package's own "c-<n>", so the two can never collide even
// though JSON-RPC does not require that) and registers a buffered waiter for
// it. The buffer of 1 means deliverConsentReply never blocks on a goroutine
// that has already moved on past its select (timeout or cancellation).
func (s *server) registerConsentWaiter() (string, chan consentReply) {
	n := atomic.AddInt64(&s.consentSeq, 1)
	id := fmt.Sprintf("c-%d", n)
	waiter := make(chan consentReply, 1)

	s.consentMu.Lock()
	if s.pending == nil {
		s.pending = map[string]chan consentReply{}
	}
	s.pending[id] = waiter
	s.consentMu.Unlock()

	return id, waiter
}

// abandonConsentWaiter removes id's waiter once remoteConsenter.Ask has
// returned by any path, so a reply that arrives afterwards (a race between a
// timeout firing and an answer landing) finds nothing to deliver to and is
// dropped rather than sent to a goroutine nobody is receiving on any more.
func (s *server) abandonConsentWaiter(id string) {
	s.consentMu.Lock()
	delete(s.pending, id)
	s.consentMu.Unlock()
}

// deliverConsentReply routes a method-less inbound line recognised as a reply
// (handleLine, serve.go) to its waiter, if one is still registered. raw.ID is
// decoded as a plain JSON string because that is the only shape this package
// ever mints (quoteConsentID); an id in any other shape, or one with no
// registered waiter, simply matches nothing and the line is dropped —
// handleLine has already decided this line is not a request to answer with an
// error.
func (s *server) deliverConsentReply(raw rpcLine) {
	var id string
	if err := json.Unmarshal(raw.ID, &id); err != nil {
		return
	}

	s.consentMu.Lock()
	waiter, ok := s.pending[id]
	if ok {
		delete(s.pending, id)
	}
	s.consentMu.Unlock()
	if !ok {
		return
	}

	waiter <- consentReply{Result: decodeConsentResult(raw.Result), Err: raw.Error}
}

// decodeConsentResult decodes raw into a consentResult, or nil when raw is
// absent or does not decode — the latter is a malformed reply, which
// decodeConsentReply already treats identically to an explicit refusal.
func decodeConsentResult(raw json.RawMessage) *consentResult {
	if len(raw) == 0 {
		return nil
	}
	var res consentResult
	if err := json.Unmarshal(raw, &res); err != nil {
		return nil
	}
	return &res
}

// askRequestParams is ask.request's params: the model's question and the
// context it gave, plus the session id, shaped like consentRequestParams
// (ADR-0020). Both fields are model output and reach a person: untrusted.
type askRequestParams struct {
	Session  string `json:"session"`
	Question string `json:"question"`
	Context  string `json:"context"`
}

// remoteAsker is ADR-0020's live answerer for the ask tool. It rides the same
// waiter machinery as remoteConsenter, so a reply that lands after a timeout or
// a cancel is dropped the same way, and the wait is bounded by the same timeout.
type remoteAsker struct {
	srv     *server
	session string
	timeout time.Duration
	clock   clock
}

func newRemoteAsker(srv *server, session string, timeout time.Duration) *remoteAsker {
	if timeout == 0 {
		timeout = srv.consentTimeout
	}
	return &remoteAsker{srv: srv, session: session, timeout: timeout, clock: realClock{}}
}

// Ask satisfies engine.Answerer.
//
// Unlike a consent timeout, expiry is not a denial: an unanswered question has
// no safe default (ADR-0009), so it returns the same "nobody could answer"
// refusal the headless case gives, which the engine journals as refused and
// shows the model as the tool's output — never as an answer, and never as the
// end of the session. A reply that carries an error, or no text at all, is the
// same refusal. Only the turn's own context ending is a real error.
func (a *remoteAsker) Ask(ctx context.Context, req engine.AskRequest) (engine.AskAnswer, error) {
	if err := ctx.Err(); err != nil {
		return engine.AskAnswer{}, err
	}
	id, waiter := a.srv.registerConsentWaiter()
	defer a.srv.abandonConsentWaiter(id)
	defer a.srv.mgr.Awaiting(a.session, sessioncore.StateAwaitingAnswer, id)()

	a.srv.write(rpcRequestOut{
		JSONRPC: jsonrpcVersion,
		ID:      quoteConsentID(id),
		Method:  methodAskRequest,
		Params:  askRequestParams{Session: a.session, Question: req.Question, Context: req.Context},
	})

	timer, stop := a.clock.NewTimer(a.timeout)
	defer stop()

	select {
	case reply := <-waiter:
		if reply.Result != nil && reply.Result.Text != nil {
			return engine.AskAnswer{Text: *reply.Result.Text}, nil
		}
		return sessioncore.DenyAsk(ctx, req)
	case <-timer:
		slog.WarnContext(ctx, "remote ask timed out", "session", a.session, "timeout", a.timeout)
		return sessioncore.DenyAsk(ctx, req)
	case <-ctx.Done():
		return engine.AskAnswer{}, ctx.Err()
	}
}
