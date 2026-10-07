package main

import (
	"context"
	"encoding/json"
	"io"
	"strconv"
	"testing"
	"time"

	"github.com/leejianrong/kopicode/internal/engine"
)

// This file unit-tests remoteConsenter.Ask directly, against a bare *server
// (just enough of one for write/registerConsentWaiter/deliverConsentReply to
// work) rather than the full JSON-RPC wire — serve_test.go's
// TestServeRemoteInteractiveAsksTheClientForConsent is the end-to-end proof
// that the wire itself carries a real consent.request/reply round trip; this
// file proves every branch of Ask's own select in isolation, including the
// two — timeout and an already-cancelled context — that a full turn cannot
// provoke deterministically without a real clock.

// fakeConsentClock lets a test fire remoteConsenter.Ask's timeout on demand,
// with no real sleep — internal/tools and internal/verify hold their own
// subprocess-timeout tests to the identical discipline.
type fakeConsentClock struct {
	fire chan time.Time
}

func newFakeConsentClock() *fakeConsentClock {
	return &fakeConsentClock{fire: make(chan time.Time, 1)}
}

func (c *fakeConsentClock) NewTimer(time.Duration) (<-chan time.Time, func()) {
	return c.fire, func() {}
}

// newConsentTestServer is just enough of a *server for remoteConsenter.Ask's
// write/registerConsentWaiter/deliverConsentReply calls: an encoder that
// discards its output (these tests read pending state directly, never the
// wire) and nothing else.
func newConsentTestServer() *server {
	return &server{enc: json.NewEncoder(io.Discard)}
}

// awaitPendingConsentID polls until Ask has registered exactly the one waiter
// a single-Ask test expects, and returns its id — the same polling idiom
// serve_test.go's awaitResponse already uses for the wire-level tests.
func awaitPendingConsentID(t *testing.T, srv *server) string {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		srv.consentMu.Lock()
		for id := range srv.pending {
			srv.consentMu.Unlock()
			return id
		}
		srv.consentMu.Unlock()
		time.Sleep(time.Millisecond)
	}
	t.Fatal("remoteConsenter never registered a pending consent request")
	return ""
}

type consentOutcome struct {
	answer engine.ConsentReply
	err    error
}

func runAsk(rc *remoteConsenter, ctx context.Context) <-chan consentOutcome {
	done := make(chan consentOutcome, 1)
	go func() {
		a, err := rc.Ask(ctx, engine.ConsentRequest{Kind: "run_shell", Tool: "run_shell", Detail: "echo hi"})
		done <- consentOutcome{a, err}
	}()
	return done
}

func awaitOutcome(t *testing.T, done <-chan consentOutcome) consentOutcome {
	t.Helper()
	select {
	case out := <-done:
		return out
	case <-time.After(2 * time.Second):
		t.Fatal("remoteConsenter.Ask never returned")
		return consentOutcome{}
	}
}

// TestRemoteConsenterDecodesEveryReplyShape covers decodeConsentReply's whole
// vocabulary through the real Ask/deliverConsentReply path: each of
// engine.ConsentAnswer's three wire spellings, an answer string this binary
// does not recognise, and an explicit JSON-RPC error reply — the last two
// both deny, the same "anything not affirmative is a refusal" rule
// cmd/kopicode/repl/consent.go's interpret holds a human's typed reply to.
func TestRemoteConsenterDecodesEveryReplyShape(t *testing.T) {
	tests := []struct {
		name   string
		result string
		errMsg string
		want   engine.ConsentReply
	}{
		{name: "allow", result: `{"answer":"allow"}`, want: engine.Reply(engine.ConsentAllow)},
		{name: "allow_session", result: `{"answer":"allow_session"}`, want: engine.Reply(engine.ConsentAllowSession)},
		{name: "deny", result: `{"answer":"deny"}`, want: engine.Deny("")},
		{name: "an unrecognised answer denies", result: `{"answer":"maybe"}`, want: engine.Deny("")},
		{name: "a missing answer denies", result: `{}`, want: engine.Deny("")},
		{name: "a deny with a note carries it", result: `{"answer":"deny","note":"use uv and a venv"}`, want: engine.Deny("use uv and a venv")},
		{name: "an unrecognised answer keeps its note", result: `{"answer":"maybe","note":"ask first"}`, want: engine.Deny("ask first")},
		{name: "a note beside an allow is dropped", result: `{"answer":"allow","note":"ignored"}`, want: engine.Reply(engine.ConsentAllow)},
		{name: "an error reply denies", errMsg: "the orchestrator refused to decide", want: engine.Deny("")},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv := newConsentTestServer()
			rc := &remoteConsenter{srv: srv, session: "s1", timeout: time.Minute, clock: newFakeConsentClock()}
			done := runAsk(rc, context.Background())

			id := awaitPendingConsentID(t, srv)
			line := rpcLine{ID: json.RawMessage(strconv.Quote(id))}
			if tc.errMsg != "" {
				line.Error = &rpcError{Code: 1, Message: tc.errMsg}
			} else {
				line.Result = json.RawMessage(tc.result)
			}
			srv.deliverConsentReply(line)

			out := awaitOutcome(t, done)
			if out.err != nil {
				t.Fatalf("Ask returned an error for a %s reply: %v", tc.name, out.err)
			}
			if out.answer != tc.want {
				t.Errorf("answer = %v, want %v", out.answer, tc.want)
			}
		})
	}
}

// TestRemoteConsenterTimesOutAsACleanDeny is ADR-0016 decision 5's own
// requirement, and the reason remoteConsenter.Ask's doc comment spends so many
// words on it: a timeout must be a plain (ConsentDeny, nil), not an error,
// because internal/permission.Gate.Check only journals a PermissionDecided
// (with this call's SourceRemote attribution) when Policy.Decide returns
// cleanly — an error here would make the timeout invisible on the record
// instead of a denied-and-attributed decision.
func TestRemoteConsenterTimesOutAsACleanDeny(t *testing.T) {
	srv := newConsentTestServer()
	clock := newFakeConsentClock()
	rc := &remoteConsenter{srv: srv, session: "s1", timeout: time.Minute, clock: clock}
	done := runAsk(rc, context.Background())

	awaitPendingConsentID(t, srv) // the request is registered and on the wire
	clock.fire <- time.Now()      // fire the timeout before any reply arrives

	out := awaitOutcome(t, done)
	if out.err != nil {
		t.Fatalf("a timeout must not be reported as an error (it would drop SourceRemote attribution "+
			"from the journal — see remoteConsenter.Ask's doc comment): %v", out.err)
	}
	if out.answer.Answer != engine.ConsentDeny {
		t.Errorf("answer = %v, want ConsentDeny", out.answer.Answer)
	}
}

// TestRemoteConsenterDeniesWithAnErrorOnCancellation is the one outcome that
// does return an error, mirroring cmd/kopicode/repl/consent.go's loop.Ask: the
// turn itself is ending, so there is no session left for a clean, attributed
// denial to matter to.
func TestRemoteConsenterDeniesWithAnErrorOnCancellation(t *testing.T) {
	srv := newConsentTestServer()
	ctx, cancel := context.WithCancel(context.Background())
	rc := &remoteConsenter{srv: srv, session: "s1", timeout: time.Minute, clock: newFakeConsentClock()}
	done := runAsk(rc, ctx)

	awaitPendingConsentID(t, srv)
	cancel()

	out := awaitOutcome(t, done)
	if out.err == nil {
		t.Fatal("Ask on a cancelled context returned no error")
	}
	if out.answer.Answer != engine.ConsentDeny {
		t.Errorf("answer = %v, want ConsentDeny", out.answer.Answer)
	}
}

// TestRemoteConsenterRefusesAnAlreadyCancelledContextWithoutAsking: a turn
// that was already cancelled before Ask was even called must not send a
// consent.request nobody will ever answer.
func TestRemoteConsenterRefusesAnAlreadyCancelledContextWithoutAsking(t *testing.T) {
	srv := newConsentTestServer()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	rc := &remoteConsenter{srv: srv, session: "s1", timeout: time.Minute, clock: newFakeConsentClock()}

	answer, err := rc.Ask(ctx, engine.ConsentRequest{Kind: "run_shell"})
	if err == nil {
		t.Fatal("Ask on an already-cancelled context returned no error")
	}
	if answer.Answer != engine.ConsentDeny {
		t.Errorf("answer = %v, want ConsentDeny", answer.Answer)
	}
	srv.consentMu.Lock()
	n := len(srv.pending)
	srv.consentMu.Unlock()
	if n != 0 {
		t.Errorf("an already-cancelled Ask registered %d waiter(s); it should never have asked at all", n)
	}
}

// TestRemoteConsenterNeverLeaksAWaiter proves the defer in Ask actually runs on
// the ordinary path: a long-lived serve process that leaked one map entry per
// permission check would grow without bound.
func TestRemoteConsenterNeverLeaksAWaiter(t *testing.T) {
	srv := newConsentTestServer()
	rc := &remoteConsenter{srv: srv, session: "s1", timeout: time.Minute, clock: newFakeConsentClock()}
	done := runAsk(rc, context.Background())

	id := awaitPendingConsentID(t, srv)
	srv.deliverConsentReply(rpcLine{ID: json.RawMessage(strconv.Quote(id)), Result: json.RawMessage(`{"answer":"allow"}`)})
	awaitOutcome(t, done)

	srv.consentMu.Lock()
	n := len(srv.pending)
	srv.consentMu.Unlock()
	if n != 0 {
		t.Errorf("%d waiter(s) still pending after Ask returned", n)
	}
}

// TestServerDeliverConsentReplyDropsAnUnmatchedID: a reply naming an id this
// process never minted, or one whose waiter has already been abandoned (a
// timeout that fired before a slow answer arrived), is dropped rather than
// causing any error — the message itself was well-formed, it simply arrived
// for a question that is no longer being asked.
func TestServerDeliverConsentReplyDropsAnUnmatchedID(t *testing.T) {
	srv := newConsentTestServer()
	// Must not panic, block, or register anything.
	srv.deliverConsentReply(rpcLine{ID: json.RawMessage(`"c-999"`), Result: json.RawMessage(`{"answer":"allow"}`)})
	srv.consentMu.Lock()
	n := len(srv.pending)
	srv.consentMu.Unlock()
	if n != 0 {
		t.Errorf("delivering an unmatched reply created %d pending entr(y/ies)", n)
	}
}
