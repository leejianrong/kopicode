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

// elicitConsenter is ADR-0016's live consenter for `kopicode mcp`: an
// engine.Consenter that puts each permission request to the connected client as
// an MCP elicitation/create request — the server-initiated, blocking round trip
// ADR-0016 decision 4 requires of a channel — and blocks the turn until the
// client answers, the turn's context ends, or remoteConsentTimeout elapses.
//
// Every outcome but an explicit allow resolves to ConsentDeny with a nil error,
// for the reason remoteConsenter.Ask gives: an error from Decide would discard
// the decision and its SourceRemote attribution from the record exactly when it
// matters. A decline, a cancel, a malformed reply and a timeout all deny.
type elicitConsenter struct {
	srv     *mcpServer
	session string
	timeout time.Duration
	clock   clock
}

func newElicitConsenter(srv *mcpServer, session string) *elicitConsenter {
	return &elicitConsenter{srv: srv, session: session, timeout: remoteConsentTimeout, clock: realClock{}}
}

// elicitReply is a client's reply to an elicitation/create request.
type elicitReply struct {
	Result *elicitResult
	Err    *rpcError
}

type elicitResult struct {
	Action  string `json:"action"` // accept, decline or cancel
	Content struct {
		Decision string `json:"decision"`
	} `json:"content"`
}

// elicitSchema is the flat, primitive-only form MCP elicitation allows: one
// enumerated choice.
const elicitSchema = `{
  "type": "object",
  "properties": {
    "decision": {
      "type": "string",
      "title": "Decision",
      "description": "allow permits this one action; allow_session also permits the identical action again for this session; deny refuses it.",
      "enum": ["allow", "allow_session", "deny"]
    }
  },
  "required": ["decision"]
}`

// Ask satisfies engine.Consenter.
func (c *elicitConsenter) Ask(ctx context.Context, req engine.ConsentRequest) (engine.ConsentAnswer, error) {
	if err := ctx.Err(); err != nil {
		return engine.ConsentDeny, err
	}

	id, waiter := c.srv.registerElicit()
	defer c.srv.abandonElicit(id)

	message := fmt.Sprintf("kopicode (session %s) asks permission for %s:\n\n%s", c.session, req.Kind, req.Detail)
	if req.Resolved != "" && req.Resolved != req.Detail {
		message += "\n(resolves to " + req.Resolved + ")"
	}
	if req.Reason != "" {
		message += "\n\nWhy it asks: " + req.Reason
	}

	c.srv.write(struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      json.RawMessage `json:"id"`
		Method  string          `json:"method"`
		Params  any             `json:"params"`
	}{jsonrpcVersion, json.RawMessage(strconv.Quote(id)), "elicitation/create", map[string]any{
		"message":         message,
		"requestedSchema": json.RawMessage(elicitSchema),
	}})

	timer, stop := c.clock.NewTimer(c.timeout)
	defer stop()

	select {
	case reply := <-waiter:
		return decodeElicitReply(reply), nil
	case <-timer:
		slog.WarnContext(ctx, "mcp consent timed out", "session", c.session, "kind", req.Kind,
			"tool", req.Tool, "timeout", c.timeout)
		return engine.ConsentDeny, nil
	case <-ctx.Done():
		return engine.ConsentDeny, ctx.Err()
	}
}

// decodeElicitReply maps a reply onto an answer. Anything but an accepted
// allow / allow_session — a decline, a cancel, an error, a missing result, an
// unrecognised choice — is a refusal.
func decodeElicitReply(r elicitReply) engine.ConsentAnswer {
	if r.Result == nil || r.Result.Action != "accept" {
		return engine.ConsentDeny
	}
	switch r.Result.Content.Decision {
	case "allow":
		return engine.ConsentAllow
	case "allow_session":
		return engine.ConsentAllowSession
	default:
		return engine.ConsentDeny
	}
}

// registerElicit mints a server-side request id ("e-<n>", disjoint from any id a
// client chooses) and registers a buffered waiter for it, so a delivery never
// blocks on a goroutine that has already moved past its select.
func (s *mcpServer) registerElicit() (string, chan elicitReply) {
	n := atomic.AddInt64(&s.consentSeq, 1)
	id := fmt.Sprintf("e-%d", n)
	waiter := make(chan elicitReply, 1)

	s.consentMu.Lock()
	if s.pending == nil {
		s.pending = map[string]chan elicitReply{}
	}
	s.pending[id] = waiter
	s.consentMu.Unlock()
	return id, waiter
}

func (s *mcpServer) abandonElicit(id string) {
	s.consentMu.Lock()
	delete(s.pending, id)
	s.consentMu.Unlock()
}

// deliverElicitReply routes a method-less inbound line to its waiter. A reply
// with no waiter — a late answer after a timeout or a cancelled turn — is
// dropped: the message was never wrong, there is just nothing left to give it to.
func (s *mcpServer) deliverElicitReply(raw mcpLine) {
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

	var res *elicitResult
	if len(raw.Result) > 0 {
		var r elicitResult
		if json.Unmarshal(raw.Result, &r) == nil {
			res = &r
		}
	}
	waiter <- elicitReply{Result: res, Err: raw.Error}
}
