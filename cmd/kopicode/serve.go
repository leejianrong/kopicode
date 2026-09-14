package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
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
// # The three methods (decision 3)
//
//   - session.start — params {session, dir, prompt, model?, harness?}. Opens a
//     session on the given working tree with engine.Open, keyed by the
//     caller-supplied `session` id, and runs the first turn with `prompt`. The
//     id is the caller's so it can cancel a turn whose start has not yet
//     returned.
//   - session.submit — params {session, prompt}. Queues the next turn on an
//     already-open session; it never fails because a turn is already running —
//     the turn waits its place in the session's queue.
//   - session.cancel — params {session}. Cancels that session's in-flight turn's
//     context, the identical mechanism the REPL's Ctrl-C drives; it does not
//     end the session, which stays open for further submits.
//
// A session lives from its start until the serve process shuts down (stdin
// closes); there is no session.close in ADR-0013's enumerated set, so shutdown
// is where every open session's SessionEnded is written. Adding an explicit
// close is a later decision, not this card's.
//
// # Concurrency: a per-session queue, sessions run concurrently (KAN-1030)
//
// Each open session has one worker goroutine draining its own FIFO queue, so a
// session's turns serialize — two turns of one session can never race
// internal/engine/context.go's Assembler, which is documented as belonging to
// one loop — while different sessions' workers run truly concurrently, the model
// internal/bench already proves at scale across worktrees. engine.Open, by
// contrast, runs inline on the read loop: it is a fast local step (a
// non-blocking flock, a journal and a tool root) that touches no Assembler, so
// serialising the Opens costs nothing and the read loop stays free during the
// turns, which is what keeps a turn cancellable while it runs.
//
// session.cancel targets the turn running *now* and only that turn: the worker
// installs each turn's own cancel handle as it dequeues, so a cancel never lands
// on a stale one, and a turn still queued behind the running one is untouched —
// it runs in its place and returns its own result, cancellable once it reaches
// the front. The wire carries only a session id, no turn id, so "the in-flight
// turn" is the one coherent target, and it is the REPL's Ctrl-C exactly.
//
// A start whose working tree is already held by another live session is refused
// with engine.ErrSessionLocked (internal/lock's one-session-per-working-tree
// rule), surfaced as codeSessionLocked rather than the generic open failure.
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
	codeOpenFailed     = -32002 // engine.Open refused: a bad model, a missing credential
	codeUsageError     = -32003 // the arm could not be resolved (an unknown model or harness)
	// -32004 is retired: it was codeSessionBusy, a submit while a turn was in
	// flight, which KAN-1030 replaced with a per-session queue that never rejects.
	codeSessionLocked = -32005 // session.start's dir is already held by another live session (internal/lock)
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

	// mu guards the sessions map and every mutable field of a servedSession —
	// its queue, its current-turn cancel handle and its closed flag — and backs
	// each session's cond. wg tracks the per-session worker goroutines so
	// shutdown waits for them before closing anything.
	mu       sync.Mutex
	sessions map[string]*servedSession
	wg       sync.WaitGroup
}

// servedSession is one open engine session and its queue of turns to run. A
// single worker goroutine drains the queue in order, so a session's turns
// serialize; different sessions' workers run concurrently. Every field below the
// two immutable ones is guarded by server.mu.
type servedSession struct {
	id   string          // immutable
	sess *engine.Session // immutable after the worker is spawned

	// cond signals the worker when a turn is queued or when the session is
	// closing. Its L is &server.mu, so the worker waits and the read loop
	// enqueues under the one lock that also guards the fields below.
	cond   *sync.Cond
	queue  []turnJob          // turns waiting to run, oldest first
	cancel context.CancelFunc // the running turn's cancel, or nil between turns
	closed bool               // shutdown asked this worker to drain and exit
}

// turnJob is one queued turn: the request whose id its result answers, the
// prompt to run, and whether it is the session's opening turn (which alone
// carries the record path back, the way session.start's response does).
type turnJob struct {
	reqID   json.RawMessage
	prompt  string
	isStart bool
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

// dispatchStart opens a session and queues its first turn, all on the read-loop
// goroutine. engine.Open runs inline — it is a fast, Assembler-free local step —
// so the working-tree lock and its collision (codeSessionLocked) are decided
// here, before any worker exists. Only after Open succeeds is the session
// registered and its worker spawned, so a failed Open leaves nothing behind and
// its id stays free to retry.
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

	// The read loop is the only goroutine that registers, so this early check and
	// the insert below cannot race: a duplicate id is refused before a needless
	// Open, which the reused-id case (a live session, a second start) relies on.
	s.mu.Lock()
	_, exists := s.sessions[p.Session]
	s.mu.Unlock()
	if exists {
		s.writeError(req.ID, codeSessionExists, fmt.Sprintf("session %q is already open in this process", p.Session))
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

	opts := s.base
	opts.Dir = p.Dir
	opts.SessionID = p.Session
	opts.Selection = selection
	opts.Events = s.notifier(p.Session)
	s.applyUnattendedAnswerers(&opts)

	sess, err := engine.Open(s.ctx, opts)
	if err != nil {
		code := codeOpenFailed
		if errors.Is(err, engine.ErrSessionLocked) {
			code = codeSessionLocked
		}
		s.writeError(req.ID, code, err.Error())
		return
	}

	ss := &servedSession{id: p.Session, sess: sess}
	ss.cond = sync.NewCond(&s.mu)
	ss.queue = []turnJob{{reqID: req.ID, prompt: p.Prompt, isStart: true}}
	s.mu.Lock()
	s.sessions[p.Session] = ss
	s.mu.Unlock()

	s.wg.Add(1)
	go s.worker(ss)
}

// dispatchSubmit queues the next turn on an already-open session. It never runs
// the turn itself and never rejects a busy session: the session's worker runs
// queued turns one at a time, in order, so a submit while a turn is in flight
// waits its place rather than failing.
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

	s.mu.Lock()
	ss := s.sessions[p.Session]
	if ss == nil {
		s.mu.Unlock()
		s.writeError(req.ID, codeUnknownSession, fmt.Sprintf("no open session %q; start one with "+
			"session.start first", p.Session))
		return
	}
	ss.queue = append(ss.queue, turnJob{reqID: req.ID, prompt: p.Prompt})
	ss.cond.Signal()
	s.mu.Unlock()
}

// worker drains one session's queue in order until the session is closed. Each
// turn gets its own cancellable context, installed as the session's current-turn
// cancel before it runs and cleared after, so a session.cancel always finds the
// turn running now. Between turns the worker waits on the session's cond, woken
// by an enqueue or by shutdown.
func (s *server) worker(ss *servedSession) {
	defer s.wg.Done()
	for {
		s.mu.Lock()
		for len(ss.queue) == 0 && !ss.closed {
			ss.cond.Wait()
		}
		if ss.closed {
			// Shutdown: abandon any queued turns (their client's stdin has closed)
			// and exit. A turn already running finished above before we got here.
			ss.queue = nil
			s.mu.Unlock()
			return
		}
		job := ss.queue[0]
		ss.queue = ss.queue[1:]
		turnCtx, cancel := context.WithCancel(s.ctx)
		ss.cancel = cancel
		s.mu.Unlock()

		res, _ := ss.sess.Run(turnCtx, job.prompt)
		cancel()

		s.mu.Lock()
		ss.cancel = nil
		s.mu.Unlock()

		result := turnResult{
			Session:  ss.id,
			Stop:     res.Stop.String(),
			ExitCode: res.Stop.ExitCode(),
			Turns:    res.Turns,
		}
		if job.isStart {
			// Only the opening turn reports where the journal was opened, exactly
			// as session.start's response does and session.submit's does not.
			result.Record = ss.sess.Path()
		}
		s.writeResult(job.reqID, result)
	}
}

// handleCancel cancels a session's in-flight turn. It runs inline on the read
// loop — the whole point of running turns on their own worker goroutines — so a
// cancel reaches the turn it targets while that turn is still running, never
// queued behind it. It cancels the turn running now and only that one: a turn
// still queued keeps its place. A session with no turn in flight (idle between
// turns) has a nil cancel and the signal is a no-op, but it is still
// acknowledged — the ack says the signal was delivered to the session, the same
// as it did before queuing.
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

	s.mu.Lock()
	ss := s.sessions[p.Session]
	var cancel context.CancelFunc
	if ss != nil {
		cancel = ss.cancel
	}
	s.mu.Unlock()

	if ss == nil {
		s.writeError(req.ID, codeUnknownSession, fmt.Sprintf("no open session %q to cancel", p.Session))
		return
	}
	if cancel != nil {
		cancel()
	}
	s.writeResult(req.ID, cancelResult{Session: p.Session, Cancelled: true})
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

// shutdown ends every open session once the client's stdin has closed. It marks
// each session closed, cancels its in-flight turn and wakes its worker, waits
// for the workers to drain and exit, then closes each session so its
// SessionEnded is written — the record's other bookend, owed even to a session
// that was mid-turn when the client went away.
func (s *server) shutdown() {
	s.mu.Lock()
	for _, ss := range s.sessions {
		ss.closed = true
		if ss.cancel != nil {
			ss.cancel()
		}
		ss.cond.Signal()
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
