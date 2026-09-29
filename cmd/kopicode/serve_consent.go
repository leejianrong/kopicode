package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/leejianrong/kopicode/internal/engine"
)

// remoteConsentTimeout bounds every remoteConsenter.Ask call (ADR-0016
// decision 5). A remote peer that never answers — crashed, disconnected, or
// simply buggy — gives the running turn no equivalent of the REPL's "a human
// is assumed to eventually answer or hit Ctrl-C," so an unbounded wait would
// hang the turn silently. 60 seconds is long enough for an automated
// orchestrator to think and short enough that a dead peer does not hang a
// turn indefinitely. Not yet configurable: no caller has asked for a
// different value, and a flag can follow additively if one does.
const remoteConsentTimeout = 60 * time.Second

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
}

// consentResult is consent.request's result shape: engine.ConsentAnswer's
// three values spelled out as a string, matching every other enum this wire
// renders as text (decision, source, stop) rather than a bare int a client
// would have to already know this binary's internal encoding to read.
type consentResult struct {
	Answer string `json:"answer"`
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

// newRemoteConsenter builds one for session, defaulting to the production
// timeout and clock.
func newRemoteConsenter(srv *server, session string) *remoteConsenter {
	return &remoteConsenter{srv: srv, session: session, timeout: remoteConsentTimeout, clock: realClock{}}
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
func (r *remoteConsenter) Ask(ctx context.Context, req engine.ConsentRequest) (engine.ConsentAnswer, error) {
	if err := ctx.Err(); err != nil {
		return engine.ConsentDeny, err
	}

	id, waiter := r.srv.registerConsentWaiter()
	defer r.srv.abandonConsentWaiter(id)

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
		return engine.ConsentDeny, nil
	case <-ctx.Done():
		return engine.ConsentDeny, ctx.Err()
	}
}

// decodeConsentReply maps a reply onto an answer. A refusal, a missing
// result, or an answer string this binary does not recognise all deny — the
// same "anything not affirmative is a refusal" rule
// cmd/kopicode/repl/consent.go's interpret holds human input to, applied here
// to a remote peer's reply instead of a typed line.
func decodeConsentReply(reply consentReply) engine.ConsentAnswer {
	if reply.Result == nil {
		return engine.ConsentDeny
	}
	switch reply.Result.Answer {
	case "allow":
		return engine.ConsentAllow
	case "allow_session":
		return engine.ConsentAllowSession
	default:
		return engine.ConsentDeny
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
