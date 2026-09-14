package main

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"

	"github.com/leejianrong/kopicode/internal/engine"
)

// `kopicode serve` — the resident session surface,
// docs/adr/0013-agent-controlled-resident-session-surface.md decisions 1, 2, 4
// and 5.
//
// # What it is
//
// A fourth rendering of the one engine (repl, run --print, serve), for a
// consumer neither of the others fits: another agentic harness — cuttlefish,
// hermes, openclaw, or a Claude Code session — that spawns kopicode as a child
// process and drives it over stdio, holding one process open across a sequence
// of tasks rather than paying process-startup cost per task and rather than
// restarting to get the next turn. The orchestrator owns the child's
// stdin/stdout, so the OS's own process-ownership model is the access control
// and there is no connection to authenticate (ADR-0013 decision 1); a listening
// socket, which nothing here needs, would have to design that from scratch.
//
// # The wire: newline-delimited JSON-RPC 2.0 (decision 2)
//
// One JSON message per line. A request carries `id`/`method`/`params` and gets a
// response carrying the same `id` and either `result` or `error`; a server →
// client notification carries no `id` and expects no reply. This reuses
// `run --print`'s NDJSON discipline rather than adding LSP's Content-Length
// framing — the same binary should not carry two incompatible conventions.
//
//	--> {"jsonrpc":"2.0","id":1,"method":"session.start","params":{"session":"s1","dir":"/repo","prompt":"fix the test"}}
//	<-- {"jsonrpc":"2.0","method":"session.event","params":{"session":"s1","event":{"kind":"session_started",...}}}
//	<-- {"jsonrpc":"2.0","method":"session.event","params":{"session":"s1","event":{...}}}
//	<-- {"jsonrpc":"2.0","id":1,"result":{"session":"s1","record":"/repo/.kopicode/sessions/s1","stop":"completed","exit_code":0,"turns":1}}
//
// # The three methods (decision 3's lifecycle; its concurrency is KAN-1030)
//
//   - session.start — params {session, dir, prompt, model?, harness?}. Opens a
//     session on the given working tree with engine.Open, keyed by the
//     caller-supplied `session` id, and runs the first turn with `prompt`. The
//     id is the caller's so it can cancel a turn whose start has not yet
//     returned.
//   - session.submit — params {session, prompt}. Runs the next turn on an
//     already-open session.
//   - session.cancel — params {session}. Cancels that session's in-flight turn's
//     context, the identical mechanism the REPL's Ctrl-C drives; it does not
//     end the session, which stays open for further submits.
//
// A session lives from its start until the serve process shuts down (stdin
// closes); there is no session.close in ADR-0013's enumerated set, so shutdown
// is where every open session's SessionEnded is written. Adding an explicit
// close is a later decision, not this card's.
//
// # Concurrency here is deliberately the simple one (KAN-1030 refines it)
//
// This card serializes every turn through one global lock: at most one turn runs
// at a time across all sessions, so two turns can never race
// internal/engine/context.go's Assembler, which is documented as belonging to
// one loop. session.cancel is handled on the read-loop goroutine and never waits
// on a turn, so a turn is always cancellable while it runs. KAN-1030 replaces
// the one global lock with a per-session queue — same-session turns still
// serialize, different-session turns run truly concurrently (the model
// internal/bench already proves at scale) — and turns the internal/lock
// one-session-per-working-tree collision into a wire error rather than the
// generic open failure it is here.
//
// # Credentials: unchanged (decision 4)
//
// OPENROUTER_API_KEY is read once from the serve process's environment, by
// engine.Open exactly as every other invocation reads it. The wire protocol
// carries no credential and no per-session override: a value that varied per
// request would reopen ADR-0007 decision 6's hash-preimage discipline and the
// API-key redaction discipline for a brand-new path, with no caller needing it.
//
// # Permissions and ask: ADR-0011's flags, reused (decision 5)
//
// `--policy-file` works exactly as under `run --print` — the same
// AllowlistFile grammar and fail-closed default — and `--ask-policy-file`
// (KAN-1028) supplies the declared ask answer. Both are process-level and apply
// to every session this process opens; with neither passed, serve refuses shell
// and writes and dead-ends ask, the same unattended default headless already
// uses.

// jsonrpcVersion is the only "jsonrpc" value this surface accepts or emits.
const jsonrpcVersion = "2.0"

// The methods a client calls, and the one notification the server sends back.
const (
	methodSessionStart  = "session.start"
	methodSessionSubmit = "session.submit"
	methodSessionCancel = "session.cancel"
	methodSessionEvent  = "session.event" // server → client notification
)

// JSON-RPC error codes. The first four are the spec's reserved values; the
// server-defined ones sit in the reserved -32000..-32099 range and name the
// failures particular to this surface, so a client can branch on the code
// rather than parse the message.
const (
	codeParseError     = -32700
	codeInvalidRequest = -32600
	codeMethodNotFound = -32601
	codeInvalidParams  = -32602

	codeUnknownSession = -32000 // session.submit/cancel named an id with no open session
	codeSessionExists  = -32001 // session.start named an id already open in this process
	codeOpenFailed     = -32002 // engine.Open refused: a bad model, a missing credential, a locked tree
	codeUsageError     = -32003 // the arm could not be resolved (an unknown model or harness)
	codeSessionBusy    = -32004 // session.submit while that session already has a turn in flight
)

// rpcRequest is one line from the client. Params stays raw so each method
// decodes its own shape and a params error is that method's to report.
type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

// rpcResponse answers one request. Exactly one of Result and Error is set; the
// id is echoed verbatim from the request so the client can match it.
type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// rpcNotification is a server → client message with no id and no reply.
type rpcNotification struct {
	JSONRPC string `json:"jsonrpc"`
	Method  string `json:"method"`
	Params  any    `json:"params"`
}

// startParams, submitParams and cancelParams are the three methods' inputs.
type startParams struct {
	Session string `json:"session"`
	Dir     string `json:"dir"`
	Prompt  string `json:"prompt"`
	Model   string `json:"model"`
	Harness string `json:"harness"`
}

type submitParams struct {
	Session string `json:"session"`
	Prompt  string `json:"prompt"`
}

type cancelParams struct {
	Session string `json:"session"`
}

// turnResult is what session.start and session.submit return: the stop the turn
// settled on, projected the same way `run --print`'s last line is, so a serve
// client and a --print consumer read an outcome identically. Record is set only
// by start, the one call that opened the journal.
type turnResult struct {
	Session  string `json:"session"`
	Record   string `json:"record,omitempty"`
	Stop     string `json:"stop"`
	ExitCode int    `json:"exit_code"`
	Turns    int    `json:"turns"`
}

// cancelResult acknowledges a cancel. The cancelled turn reports its own
// StopCancelled through start/submit's own response; this only says the signal
// was delivered.
type cancelResult struct {
	Session   string `json:"session"`
	Cancelled bool   `json:"cancelled"`
}

// eventParams wraps one journal-derived event as a notification's params,
// tagged with the session it belongs to (ADR-0013 decision 3: Options.Events
// tees as notifications carrying the session id). The event itself is the exact
// `record` projection `run --print` emits, so the two surfaces speak one event
// vocabulary.
type eventParams struct {
	Session string `json:"session"`
	Event   record `json:"event"`
}

// serveCmd is `kopicode serve`. It parses the process-level flags, builds the
// base options every session inherits, and hands the run loop the process's own
// stdio.
func serveCmd(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("kopicode serve", flag.ContinueOnError)
	fs.SetOutput(stderr)
	debug := fs.Bool("debug", false, "engine diagnostics on stderr")
	// Reused verbatim from `run --print` (ADR-0011 decision 5 / ADR-0013
	// decision 5): a declared allowlist answering shell/write consent, or the
	// refuse-everything default when unset.
	policyFile := fs.String("policy-file", "", "load a declared-allowlist policy file (ADR-0011) governing "+
		"shell/write consent for every session; unset means refuse everything")
	// ADR-0013 decision 6 / KAN-1028: the orchestrator's standing answer for an
	// ask call nobody can answer, or the fixed headless refusal when unset.
	askPolicyFile := fs.String("ask-policy-file", "", "load an ask-policy file (ADR-0013) whose note answers "+
		"the model's ask calls for every session; unset means the fixed 'no human is present' refusal")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	setupLogging(*debug, stderr)

	if fs.NArg() != 0 {
		say(stderr, "kopicode: `serve` takes no positional arguments; a task arrives over the wire as "+
			"session.start's prompt, not on the command line\n")
		return exitUsage
	}

	var base engine.Options
	// Loaded up front, the same ordering ADR-0007 decision 4 holds every other
	// surface to: a malformed policy file is the caller's own mistake, refused
	// before any session is opened or any request is read.
	if *policyFile != "" {
		pf, err := engine.LoadPolicyFile(*policyFile)
		if err != nil {
			say(stderr, "kopicode: %v\n", err)
			return exitUsage
		}
		base.Policy = &pf
	}
	if *askPolicyFile != "" {
		ap, err := engine.LoadAskPolicyFile(*askPolicyFile)
		if err != nil {
			say(stderr, "kopicode: %v\n", err)
			return exitUsage
		}
		base.AskPolicy = &ap
	}

	return serve(context.Background(), os.Stdin, stdout, stderr, base)
}

// serve is the run loop, taking its streams and base options in the open so a
// test can drive scripted JSON-RPC lines through an in-memory reader and point
// base.ProviderBaseURL at an httptest server — the same seam print_test.go uses
// for `run --print`.
//
// base carries what every session inherits (Policy, AskPolicy, ProviderBaseURL);
// Dir and Selection are per-session, resolved from each session.start.
func serve(ctx context.Context, stdin io.Reader, stdout, stderr io.Writer, base engine.Options) int {
	s := &server{
		ctx:      ctx,
		base:     base,
		stderr:   stderr,
		sessions: map[string]*servedSession{},
	}
	s.enc = json.NewEncoder(stdout)
	// Off, for the reason journal.Marshal and print.go's emitter both give: the
	// default rewrites <, > and & as \uXXXX, so a diff or tool output would read
	// as different bytes on this stream than in the record it came from.
	s.enc.SetEscapeHTML(false)
	return s.run(stdin)
}

// server holds the resident state: the open sessions and the locks that keep
// the wire and the turn loop honest.
type server struct {
	ctx    context.Context
	base   engine.Options
	stderr io.Writer

	// enc and encMu serialize every write to stdout. Notifications come off a
	// turn's goroutine and responses come off both a turn's goroutine and the
	// read loop, so the encoder is shared and must be guarded.
	enc   *json.Encoder
	encMu sync.Mutex

	// turnMu serializes turn execution across all sessions — this card's whole
	// concurrency model, and the one lock KAN-1030 replaces with a per-session
	// queue. Held across a turn (and, for start, across the Open that precedes
	// it) so no two turns touch an Assembler at once.
	turnMu sync.Mutex

	// mu guards sessions and each session's current-turn cancel handle. wg
	// tracks in-flight turn goroutines so shutdown waits for them before closing
	// anything.
	mu       sync.Mutex
	sessions map[string]*servedSession
	wg       sync.WaitGroup
}

// servedSession is one open engine session plus the handle that cancels its
// current in-flight turn. cancel is replaced under server.mu at the start of
// every turn, so a session.cancel always targets the turn running now. running
// is true while a turn is in flight: this card runs at most one turn per session
// and rejects a second as busy, deferring the queue that would hold it to
// KAN-1030.
type servedSession struct {
	id      string
	sess    *engine.Session
	cancel  context.CancelFunc
	running bool
}

// run reads one JSON message per line until stdin closes, dispatching each, then
// shuts every open session down. A line that will not parse gets a parse-error
// response and the loop continues — one bad message does not end the process.
func (s *server) run(stdin io.Reader) int {
	br := bufio.NewReader(stdin)
	for {
		line, err := br.ReadString('\n')
		if trimmed := strings.TrimSpace(line); trimmed != "" {
			s.handleLine(trimmed)
		}
		if err != nil {
			if err != io.EOF {
				say(s.stderr, "kopicode: reading the serve input: %v\n", err)
			}
			break
		}
	}
	s.shutdown()
	return exitSuccess
}

// handleLine parses and dispatches one message. A request with an id gets
// exactly one response; a parse failure or an unknown method is reported against
// whatever id could be recovered (null when even that failed).
func (s *server) handleLine(line string) {
	var req rpcRequest
	if err := json.Unmarshal([]byte(line), &req); err != nil {
		s.writeError(nil, codeParseError, fmt.Sprintf("not valid JSON: %v", err))
		return
	}
	if req.Method == "" {
		s.writeError(req.ID, codeInvalidRequest, "request has no method")
		return
	}

	switch req.Method {
	case methodSessionStart:
		s.dispatchStart(req)
	case methodSessionSubmit:
		s.dispatchSubmit(req)
	case methodSessionCancel:
		s.handleCancel(req)
	default:
		s.writeError(req.ID, codeMethodNotFound, fmt.Sprintf("unknown method %q; this surface has "+
			"session.start, session.submit and session.cancel", req.Method))
	}
}

// dispatchStart validates start's params on the read-loop goroutine, registers
// the session with its first turn's cancel handle, then runs the turn on its own
// goroutine so the loop stays free to receive a cancel for the turn now
// starting. The session is registered before the turn runs, so a cancel naming
// the caller-supplied id — even one that arrives before this response does —
// finds it and aborts the Open or the turn.
func (s *server) dispatchStart(req rpcRequest) {
	var p startParams
	if err := json.Unmarshal(req.Params, &p); err != nil {
		s.writeError(req.ID, codeInvalidParams, fmt.Sprintf("session.start params: %v", err))
		return
	}
	switch {
	case p.Session == "":
		s.writeError(req.ID, codeInvalidParams, "session.start needs a session id to key the session by")
		return
	case p.Dir == "":
		s.writeError(req.ID, codeInvalidParams, "session.start needs a dir: the working tree to run in")
		return
	case p.Prompt == "":
		s.writeError(req.ID, codeInvalidParams, "session.start needs a prompt: the first turn's task")
		return
	}

	selection, err := engine.ResolveSelection(p.Dir, engine.SelectionOverrides{Model: p.Model, Harness: p.Harness})
	if err != nil {
		code := codeOpenFailed
		if engine.IsSelectionUsageError(err) {
			code = codeUsageError
		}
		s.writeError(req.ID, code, err.Error())
		return
	}

	turnCtx, cancel := context.WithCancel(s.ctx)
	// Registered running: start's own first turn is that turn, so a submit or a
	// second start that races it is rejected (busy / already-open) rather than
	// overwriting its cancel handle.
	if !s.register(&servedSession{id: p.Session, cancel: cancel, running: true}) {
		cancel()
		s.writeError(req.ID, codeSessionExists, fmt.Sprintf("session %q is already open in this process", p.Session))
		return
	}

	opts := s.base
	opts.Dir = p.Dir
	opts.SessionID = p.Session
	opts.Selection = selection
	opts.Events = s.notifier(p.Session)
	s.applyUnattendedAnswerers(&opts)

	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		defer cancel()

		s.turnMu.Lock()
		defer s.turnMu.Unlock()

		sess, err := engine.Open(turnCtx, opts)
		if err != nil {
			s.unregister(p.Session)
			s.writeError(req.ID, codeOpenFailed, err.Error())
			return
		}
		s.setSession(p.Session, sess)

		res, _ := sess.Run(turnCtx, p.Prompt)
		s.endTurn(p.Session)
		s.writeResult(req.ID, turnResult{
			Session:  p.Session,
			Record:   sess.Path(),
			Stop:     res.Stop.String(),
			ExitCode: res.Stop.ExitCode(),
			Turns:    res.Turns,
		})
	}()
}

// dispatchSubmit runs the next turn on an already-open session, off the read
// loop for the same reason start does. It installs a fresh cancel handle for
// this turn before spawning the goroutine, so a session.cancel that arrives
// while the turn is still queued on turnMu targets this turn, not the last one.
func (s *server) dispatchSubmit(req rpcRequest) {
	var p submitParams
	if err := json.Unmarshal(req.Params, &p); err != nil {
		s.writeError(req.ID, codeInvalidParams, fmt.Sprintf("session.submit params: %v", err))
		return
	}
	if p.Session == "" {
		s.writeError(req.ID, codeInvalidParams, "session.submit needs a session id")
		return
	}
	if p.Prompt == "" {
		s.writeError(req.ID, codeInvalidParams, "session.submit needs a prompt: the next turn's task")
		return
	}

	turnCtx, cancel := context.WithCancel(s.ctx)
	ss, reason := s.beginTurn(p.Session, cancel)
	if ss == nil {
		cancel()
		switch reason {
		case turnBusy:
			s.writeError(req.ID, codeSessionBusy, fmt.Sprintf("session %q already has a turn in flight; "+
				"wait for its result before submitting the next (queuing is KAN-1030)", p.Session))
		default:
			s.writeError(req.ID, codeUnknownSession, fmt.Sprintf("no open session %q; start one with "+
				"session.start first", p.Session))
		}
		return
	}

	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		defer cancel()

		s.turnMu.Lock()
		defer s.turnMu.Unlock()

		res, _ := ss.sess.Run(turnCtx, p.Prompt)
		s.endTurn(p.Session)
		s.writeResult(req.ID, turnResult{
			Session:  p.Session,
			Stop:     res.Stop.String(),
			ExitCode: res.Stop.ExitCode(),
			Turns:    res.Turns,
		})
	}()
}

// handleCancel cancels a session's in-flight turn. It runs inline on the read
// loop — the whole point of running turns on their own goroutines — so a cancel
// is delivered while the turn it targets is still running, never queued behind
// it. A session still opening (its Open in flight) is cancellable too: its
// cancel handle is registered before Open runs.
func (s *server) handleCancel(req rpcRequest) {
	var p cancelParams
	if err := json.Unmarshal(req.Params, &p); err != nil {
		s.writeError(req.ID, codeInvalidParams, fmt.Sprintf("session.cancel params: %v", err))
		return
	}
	if p.Session == "" {
		s.writeError(req.ID, codeInvalidParams, "session.cancel needs a session id")
		return
	}

	cancel := s.cancelOf(p.Session)
	if cancel == nil {
		s.writeError(req.ID, codeUnknownSession, fmt.Sprintf("no open session %q to cancel", p.Session))
		return
	}
	cancel()
	s.writeResult(req.ID, cancelResult{Session: p.Session, Cancelled: true})
}

// --- session registry -------------------------------------------------------

// register adds a session if its id is free, reporting whether it took. It holds
// the id and the first turn's cancel handle so a cancel can find the session
// before its engine session finishes opening.
func (s *server) register(ss *servedSession) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.sessions[ss.id]; ok {
		return false
	}
	s.sessions[ss.id] = ss
	return true
}

func (s *server) unregister(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.sessions, id)
}

func (s *server) setSession(id string, sess *engine.Session) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if ss, ok := s.sessions[id]; ok {
		ss.sess = sess
	}
}

// beginReason says why beginTurn refused, so submit can pick the right code.
type beginReason uint8

const (
	turnUnknown beginReason = iota // no such open session
	turnBusy                       // a turn is already in flight for it
)

// beginTurn claims the given session for a new turn: it installs cancel as the
// session's current-turn cancel and marks it running, or returns nil with the
// reason it could not — the session is not open (turnUnknown) or already has a
// turn in flight (turnBusy). Installing cancel here, under the same lock that
// checks running, is what makes a session.cancel target this turn and not a
// stale one.
func (s *server) beginTurn(id string, cancel context.CancelFunc) (*servedSession, beginReason) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ss := s.sessions[id]
	if ss == nil || ss.sess == nil {
		return nil, turnUnknown
	}
	if ss.running {
		return nil, turnBusy
	}
	ss.running = true
	ss.cancel = cancel
	return ss, turnUnknown
}

// endTurn marks a session's turn finished, so the next submit is accepted.
func (s *server) endTurn(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if ss := s.sessions[id]; ss != nil {
		ss.running = false
	}
}

// cancelOf returns the current-turn cancel of any registered session — including
// one still opening — or nil if none. handleCancel uses it inline.
func (s *server) cancelOf(id string) context.CancelFunc {
	s.mu.Lock()
	defer s.mu.Unlock()
	if ss := s.sessions[id]; ss != nil {
		return ss.cancel
	}
	return nil
}

// --- unattended answerers ---------------------------------------------------

// applyUnattendedAnswerers wires the consent and ask answerers for a surface
// with nobody at a terminal, exactly as headless does. Policy/AskPolicy from the
// process flags win where set (they are already on opts via s.base); otherwise
// the fail-closed defaults apply: refuse shell/writes, dead-end ask.
func (s *server) applyUnattendedAnswerers(opts *engine.Options) {
	// Consent: nil Consenter + ConsentUnattended is engine.Open's headless case
	// (permission.NewUnattendedDeny) when Policy is unset; when Policy is set it
	// governs instead. Either way no human is attributed a decision (KAN-885).
	opts.ConsentMode = engine.ConsentUnattended
	if opts.AskPolicy == nil {
		// No declared ask policy: dead-end ask the way headless does, attributed
		// to the policy since nobody was asked (ADR-0009 decision 4).
		opts.Ask = denyHeadlessAsk
		opts.AskMode = engine.AskUnattended
	}
	// When AskPolicy is set, Ask must stay nil — Open refuses both at once — and
	// mustAnswerer attributes the answer to the policy on its own (KAN-1028).
}

// --- writing the wire -------------------------------------------------------

// notifier is the engine.Observer a session announces its record through: every
// non-delta event becomes a session.event notification tagged with the session
// id. Deltas are dropped for print.go's reason — they are not in the record, and
// this stream carries only what the record holds.
func (s *server) notifier(session string) engine.Observer {
	return func(ev engine.Event) {
		if ev.Kind == engine.EventDelta {
			return
		}
		s.write(rpcNotification{
			JSONRPC: jsonrpcVersion,
			Method:  methodSessionEvent,
			Params:  eventParams{Session: session, Event: recordOf(ev)},
		})
	}
}

func (s *server) writeResult(id json.RawMessage, result any) {
	s.write(rpcResponse{JSONRPC: jsonrpcVersion, ID: idOrNull(id), Result: result})
}

func (s *server) writeError(id json.RawMessage, code int, message string) {
	s.write(rpcResponse{JSONRPC: jsonrpcVersion, ID: idOrNull(id), Error: &rpcError{Code: code, Message: message}})
}

// write encodes one value as a line under the shared lock. A write failure —
// stdout gone, a broken pipe — is reported once to stderr; there is nowhere else
// to put it, and a resident process whose client vanished has nothing left to
// say to it.
func (s *server) write(v any) {
	s.encMu.Lock()
	defer s.encMu.Unlock()
	if err := s.enc.Encode(v); err != nil {
		say(s.stderr, "kopicode: writing the serve output: %v\n", err)
	}
}

// idOrNull keeps a response's id a JSON null when the request had none, which is
// what JSON-RPC requires for an error that could not be tied to a request.
func idOrNull(id json.RawMessage) json.RawMessage {
	if len(id) == 0 {
		return json.RawMessage("null")
	}
	return id
}

// --- shutdown ---------------------------------------------------------------

// shutdown ends every open session once the client's stdin has closed. It
// cancels in-flight turns, waits for their goroutines, then closes each session
// so its SessionEnded is written — the record's other bookend, owed even to a
// session that was mid-turn when the client went away.
func (s *server) shutdown() {
	s.mu.Lock()
	for _, ss := range s.sessions {
		ss.cancel()
	}
	s.mu.Unlock()

	s.wg.Wait()

	s.mu.Lock()
	open := make([]*servedSession, 0, len(s.sessions))
	for _, ss := range s.sessions {
		open = append(open, ss)
	}
	s.sessions = map[string]*servedSession{}
	s.mu.Unlock()

	for _, ss := range open {
		if ss.sess == nil {
			continue
		}
		if err := ss.sess.Close(s.ctx); err != nil {
			say(s.stderr, "kopicode: closing session %q: %v\n", ss.id, err)
		}
	}
}
