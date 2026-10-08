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
	"time"

	sessioncore "github.com/leejianrong/kopicode/cmd/kopicode/session"
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
//   - session.close — params {session}. Ends that one session while the process
//     stays up (KAN-1795): the close queues behind any turns already accepted, so
//     they run to completion first (session.cancel first for an immediate end),
//     then the session's SessionEnded is written and announced, its working-tree
//     lock is released and its id is free to start again. New submits after a
//     close is accepted are refused. Without it a session lives until the serve
//     process shuts down (stdin closes) and SessionEnded — the only event whose
//     text carries the failure — arrives only then.
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
// # Permissions: a required per-session consent_mode (ADR-0016)
//
// Every session.start must declare consent_mode, one of three values, with no
// default that grants capability (mirroring ADR-0011 decision 3's own "an
// unconfigured invocation is exactly as safe as before" posture):
//
//   - "remote_interactive" — every permission-requiring decision bubbles live
//     to this client, per action, over a new server-to-client consent.request
//     (below). Nothing is declared ahead of time; the client answers each one
//     as it arrives.
//   - "unattended_policy" — ADR-0011's existing declared allowlist
//     (--policy-file) answers instead, exactly as before this ADR, but the
//     session.start call must also carry containment_provided: true — an
//     explicit acknowledgment that the caller is supplying real
//     process/container containment (ADR-0011 decision 4 is unchanged: this
//     package never does that itself). Omitting consent_mode entirely, or
//     requesting "unattended_policy" without the acknowledgment, is a startup
//     usage error, refused before engine.Open runs.
//   - "auto" (ADR-0017) — the harness answers itself, from a fixed rule: a shell
//     command whose directory is inside the session root, and which matches
//     nothing on a built-in never-allow list (sudo, rm outside the root, a
//     forced git push, a download piped into a shell, a redirection outside the
//     root), is allowed without a consent.request; a match is refused with a
//     reason, and so is every write outside the root. The list is not a sandbox
//     and cannot be shortened; session.start may add to it with never_allow.
//     Every decision is journalled as source "auto".
//
// --ask-policy-file (KAN-1028) is untouched by this and stays process-level:
// ADR-0016 only reopens the Consenter/permission half of consent, not the ask
// tool's.
//
// # consent.request: the one server-initiated request on this wire
//
// Every other message from server to client is a notification (session.event)
// or a response to something the client asked. consent.request is the
// exception: the server mints its own id, sends a request, and blocks the
// turn until a matching reply arrives or a bounded timeout elapses (ADR-0016
// decision 5), denying on either a timeout or a malformed/refused reply —
// never treated as though granted.
//
//	--> {"jsonrpc":"2.0","id":"c-1","method":"consent.request","params":{"session":"s1","kind":"run_shell","tool":"run_shell","detail":"uv run pytest -v","reason":"remote_interactive","resolved":""}}
//	<-- {"jsonrpc":"2.0","id":"c-1","result":{"answer":"allow"}}
//
// result.answer is one of "allow", "allow_session", "deny" —
// engine.ConsentAnswer's three values, spelled out the way every other enum on
// this wire is (decision, source, stop). Every resulting
// journal.PermissionDecided is stamped permission.SourceRemote, never
// SourceUser or SourcePolicy: kopicode cannot verify who or what answered on
// the far end of the channel.

// jsonrpcVersion is the only "jsonrpc" value this surface accepts or emits.
const jsonrpcVersion = "2.0"

// The methods a client calls, the one notification the server sends back, and
// the one request the server itself initiates (ADR-0016).
const (
	methodSessionStart   = "session.start"
	methodSessionSubmit  = "session.submit"
	methodSessionCancel  = "session.cancel"
	methodSessionClose   = "session.close"
	methodSessionUsage   = "session.usage"
	methodServerHello    = "server.hello"    // capabilities, see capabilities.go
	methodAskRequest     = "ask.request"     // server → client request (ADR-0020)
	methodSessionEvent   = "session.event"   // server → client notification
	methodConsentRequest = "consent.request" // server → client request
)

// The consent_mode values session.start accepts (ADR-0016 decision 3, ADR-0017).
// There is no "default" value: consent_mode is required, and a missing or
// unrecognised one is a usage error, not a fallback.
const (
	consentModeRemoteInteractive = sessioncore.ConsentRemoteInteractive
	consentModeUnattendedPolicy  = sessioncore.ConsentUnattendedPolicy
	consentModeAuto              = sessioncore.ConsentAuto
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
	codeInternalError  = -32603 // a session could not be closed cleanly

	codeUnknownSession = -32000 // session.submit/cancel named an id with no open session
	codeSessionExists  = -32001 // session.start named an id already open in this process
	codeOpenFailed     = -32002 // engine.Open refused: a bad model, a missing credential
	codeUsageError     = -32003 // the arm could not be resolved (an unknown model or harness)
	// -32004 is retired: it was codeSessionBusy, a submit while a turn was in
	// flight, which KAN-1030 replaced with a per-session queue that never rejects.
	codeSessionLocked = -32005 // session.start's dir is already held by another live session (internal/lock)
)

// rpcRequest is one client-to-server line. Params stays raw so each method
// decodes its own shape and a params error is that method's to report.
type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

// rpcLine is every field an inbound line might carry, decoded once so
// handleLine can tell which of two shapes it is before committing to either: a
// client-to-server request (Method set, Result/Error absent) or a client's
// reply to a server-initiated consent.request (Result or Error set, Method
// absent — ADR-0016). Every rpcRequest field name and tag matches this
// struct's, so a request line converts to one with a plain field copy.
type rpcLine struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
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

// rpcRequestOut is a server → client request: the mirror of rpcRequest, for
// the one shape a notification (no id, no reply expected) cannot carry.
// consent.request (ADR-0016) is the only user of this today. The id is minted
// by the server rather than echoed, which is why this is a distinct type
// rather than rpcRequest reused.
type rpcRequestOut struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  any             `json:"params"`
}

// startParams, submitParams and cancelParams are the three methods' inputs.
type startParams struct {
	Session string `json:"session"`
	Dir     string `json:"dir"`
	Prompt  string `json:"prompt"`
	Model   string `json:"model"`
	Harness string `json:"harness"`
	// HarnessConfig is a path to a declared harness-config file (ADR-0010), the
	// same axis Harness chooses; a relative path resolves against Dir. Naming
	// both is a usage error, decided in engine.ResolveSelection.
	HarnessConfig string `json:"harness_config"`

	// MaxTurns caps the turns one prompt may take, and TokenBudget the tokens
	// the whole session may spend (0 is unbounded); both override the harness
	// default for this session and move its config hash (ADR-0022).
	MaxTurns    *int `json:"max_turns"`
	TokenBudget *int `json:"token_budget"`

	// ConsentMode is ADR-0016 decision 3's required declaration:
	// consentModeRemoteInteractive, consentModeUnattendedPolicy or
	// consentModeAuto (ADR-0017). There is no
	// default — omitting it, or naming anything else, is a startup usage error
	// (codeUsageError), refused before engine.Open runs. See buildConsentOptions.
	ConsentMode string `json:"consent_mode"`

	// ContainmentProvided is required, and must be true, when ConsentMode is
	// consentModeUnattendedPolicy: the caller's explicit acknowledgment that it
	// is supplying real process/container containment for this session
	// (ADR-0011 decision 4 — this package never does that itself). Ignored
	// when ConsentMode is consentModeRemoteInteractive, which carries no
	// equivalent requirement.
	ContainmentProvided bool `json:"containment_provided"`

	// NeverAllow adds entries ("command [token ...]") to consentModeAuto's
	// built-in never-allow list; it cannot remove or narrow one. Supplying it
	// under any other consent_mode is a usage error: nothing would consult it,
	// and a caller who sent a never-allow list is relying on it.
	NeverAllow []string `json:"never_allow"`

	// ConsentTimeout overrides --consent-timeout for this session: a Go
	// duration string such as "5m", within the same bounds as the flag. Only
	// consent_mode "remote_interactive" waits on a live answer, so sending it
	// under any other mode is a usage error.
	ConsentTimeout string `json:"consent_timeout"`

	// ReadOnly refuses every file write for this session (ADR-0019). A usage
	// error under consent_mode "auto". Shell is not made read-only.
	ReadOnly bool `json:"read_only"`

	// AskMode is "" or "remote" (ADR-0020): "remote" puts the model's ask to the
	// client as an ask.request instead of the process ask policy or the fixed
	// refusal. Needs consent_mode "remote_interactive".
	AskMode string `json:"ask_mode"`
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
	// Usage is the session's usage as the turn settled (feature usage.context
	// and its siblings); see usageSummary.
	Usage *usageSummary `json:"usage,omitempty"`
}

// usageParams / usageResult are session.usage's shapes: a pull for a session's
// usage now, answerable while a turn is running.
type usageParams struct {
	Session string `json:"session"`
}

type usageResult struct {
	Session string        `json:"session"`
	Usage   *usageSummary `json:"usage"`
}

// cancelResult acknowledges a cancel. The cancelled turn reports its own
// StopCancelled through start/submit's own response; this only says the signal
// was delivered.
type cancelResult struct {
	Session   string `json:"session"`
	Cancelled bool   `json:"cancelled"`
}

// closeParams / closeResult are session.close's shapes. The result says the
// session is gone; why it ended is on its session_ended event, announced before
// this response.
type closeParams struct {
	Session string `json:"session"`
}

type closeResult struct {
	Session string `json:"session"`
	Closed  bool   `json:"closed"`
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
	base, timeout, code, ok := residentOptions("serve", "a task arrives over the wire as session.start's prompt, "+
		"not on the command line", args, stderr)
	if !ok {
		return code
	}
	return serveWith(context.Background(), os.Stdin, stdout, stderr, base, timeout)
}

// residentOptions parses the process-level flags the two resident front ends
// (`serve` and `mcp`) share and builds the base options every session inherits.
// ok is false when the process should exit with code instead of running.
func residentOptions(name, noArgsHint string, args []string, stderr io.Writer) (base engine.Options, consentTimeout time.Duration, code int, ok bool) {
	fs := flag.NewFlagSet("kopicode "+name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	debug := fs.Bool("debug", false, "engine diagnostics on stderr")
	// Reused verbatim from `run --print` (ADR-0011 decision 5 / ADR-0013
	// decision 5): a declared allowlist answering shell/write consent, or the
	// refuse-everything default when unset.
	policyFile := fs.String("policy-file", "", "load a declared-allowlist policy file (ADR-0011) governing "+
		"shell/write consent for every unattended_policy session; unset means refuse everything")
	// ADR-0013 decision 6 / KAN-1028: the orchestrator's standing answer for an
	// ask call nobody can answer, or the fixed headless refusal when unset.
	askPolicyFile := fs.String("ask-policy-file", "", "load an ask-policy file (ADR-0013) whose note answers "+
		"the model's ask calls for every session; unset means the fixed 'no human is present' refusal")
	// ADR-0027: every session this process opens talks to this endpoint instead
	// of OpenRouter. Each session.start must then name a model the endpoint serves.
	providerURL := fs.String("provider-url", "", "send every session's requests to this OpenAI-compatible endpoint "+
		"instead of OpenRouter (unpinned, never a benchmark arm; the key is KOPICODE_PROVIDER_API_KEY, optional on loopback)")
	timeoutFlag := fs.Duration("consent-timeout", remoteConsentTimeout, "how long a live consent request "+
		"(consent_mode remote_interactive) waits for the client's answer before it is denied, for example 5m; "+
		"between 1s and 24h")
	if err := fs.Parse(args); err != nil {
		// flag has already printed the error and the usage. A help request is not
		// an error: -h/--help exits 0, everything else is a usage error.
		if errors.Is(err, flag.ErrHelp) {
			return base, 0, exitSuccess, false
		}
		return base, 0, exitUsage, false
	}
	setupLogging(*debug, stderr)

	if fs.NArg() != 0 {
		say(stderr, "kopicode: `%s` takes no positional arguments; %s\n", name, noArgsHint)
		return base, 0, exitUsage, false
	}
	consentTimeout, err := parseConsentTimeout(*timeoutFlag)
	if err != nil {
		say(stderr, "kopicode: %v\n", err)
		return base, 0, exitUsage, false
	}

	if *providerURL != "" {
		u, err := engine.ValidateProviderURL(*providerURL)
		if err != nil {
			say(stderr, "kopicode: %v\n", err)
			return base, 0, exitUsage, false
		}
		base.ProviderURL = u
	}

	// Loaded up front, the same ordering ADR-0007 decision 4 holds every other
	// surface to: a malformed policy file is the caller's own mistake, refused
	// before any session is opened or any request is read.
	if *policyFile != "" {
		pf, err := engine.LoadPolicyFile(*policyFile)
		if err != nil {
			say(stderr, "kopicode: %v\n", err)
			return base, 0, exitUsage, false
		}
		base.Policy = &pf
	}
	if *askPolicyFile != "" {
		ap, err := engine.LoadAskPolicyFile(*askPolicyFile)
		if err != nil {
			say(stderr, "kopicode: %v\n", err)
			return base, 0, exitUsage, false
		}
		base.AskPolicy = &ap
	}
	return base, consentTimeout, exitSuccess, true
}

// serveWith is the run loop, taking its streams and base options in the open so a
// test can drive scripted JSON-RPC lines through an in-memory reader and point
// base.ProviderBaseURL at an httptest server — the same seam print_test.go uses
// for `run --print`.
//
// base carries what every session inherits (Policy, AskPolicy, ProviderBaseURL);
// Dir and Selection are per-session, resolved from each session.start.
// consentTimeout bounds every live consent request (`--consent-timeout`).
func serveWith(ctx context.Context, stdin io.Reader, stdout, stderr io.Writer, base engine.Options, consentTimeout time.Duration) int {
	s := &server{stderr: stderr, consentTimeout: consentTimeout}
	// The Manager is the session lifecycle both resident front ends share
	// (ADR-0015 decision 3); this file is only the JSON-RPC skin over it.
	s.mgr = sessioncore.New(ctx, base, stderr, func(id string, timeout time.Duration) engine.Consenter {
		return newRemoteConsenter(s, id, timeout).Ask
	})
	s.mgr.SetRemoteAsk(func(id string, timeout time.Duration) engine.Answerer {
		return newRemoteAsker(s, id, timeout).Ask
	})
	s.enc = json.NewEncoder(stdout)
	// Off, for the reason journal.Marshal and print.go's emitter both give: the
	// default rewrites <, > and & as \uXXXX, so a diff or tool output would read
	// as different bytes on this stream than in the record it came from.
	s.enc.SetEscapeHTML(false)
	return s.run(stdin)
}

// server is the JSON-RPC skin: it frames the wire and maps sessioncore.Manager's
// outcomes onto it. The sessions themselves live in the Manager.
type server struct {
	mgr    *sessioncore.Manager
	stderr io.Writer

	// consentTimeout bounds every live consent request (--consent-timeout).
	consentTimeout time.Duration

	// enc and encMu serialize every write to stdout. Notifications come off a
	// turn's goroutine and responses come off both a turn's goroutine and the
	// read loop, so the encoder is shared and must be guarded.
	enc   *json.Encoder
	encMu sync.Mutex

	// consentMu, consentSeq and pending back every remoteConsenter's blocking
	// round trip (ADR-0016, see serve_consent.go). Deliberately its own lock: a
	// consent wait has nothing to do with session bookkeeping, and a turn
	// blocked on a remote answer must not contend with the read loop
	// dispatching session.cancel for an unrelated session.
	consentMu  sync.Mutex
	consentSeq int64
	pending    map[string]chan consentReply
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
	s.mgr.Shutdown()
	return exitSuccess
}

// handleLine parses and dispatches one message. A request with an id gets
// exactly one response; a parse failure or an unknown method is reported against
// whatever id could be recovered (null when even that failed).
//
// A method-less line is one of two things, not one: the ordinary malformed
// case (nothing on this wire sends such a line deliberately), or a client's
// reply to a server-initiated consent.request (ADR-0016) — which carries a
// result or an error but never a method, the same shape JSON-RPC gives every
// response. deliverReply tries the second reading first; only a line that is
// neither a request nor a recognisable reply falls through to the original
// refusal.
func (s *server) handleLine(line string) {
	var raw rpcLine
	if err := json.Unmarshal([]byte(line), &raw); err != nil {
		s.writeError(nil, codeParseError, fmt.Sprintf("not valid JSON: %v", err))
		return
	}
	if raw.Method == "" {
		if raw.Result != nil || raw.Error != nil {
			// A well-formed reply, whether or not anything is still waiting
			// for it — a late answer after a timeout or a cancelled turn has
			// nothing to deliver to and is dropped silently rather than
			// reported as malformed, since the message itself was never
			// wrong.
			s.deliverConsentReply(raw)
			return
		}
		s.writeError(raw.ID, codeInvalidRequest, "request has no method")
		return
	}
	req := rpcRequest{JSONRPC: raw.JSONRPC, ID: raw.ID, Method: raw.Method, Params: raw.Params}

	switch req.Method {
	case methodSessionStart:
		s.dispatchStart(req)
	case methodSessionSubmit:
		s.dispatchSubmit(req)
	case methodSessionCancel:
		s.handleCancel(req)
	case methodSessionClose:
		s.dispatchClose(req)
	case methodSessionUsage:
		s.handleUsage(req)
	case methodServerHello:
		s.writeResult(req.ID, currentCapabilities())
	default:
		s.writeError(req.ID, codeMethodNotFound, fmt.Sprintf("unknown method %q; this surface has "+
			"session.start, session.submit, session.cancel, session.close, session.usage and server.hello", req.Method))
	}
}

// rpcErrorFor maps a sessioncore.Manager refusal onto this wire's error codes.
func rpcErrorFor(e *sessioncore.Error) (int, string) {
	switch e.Kind {
	case sessioncore.KindUsage:
		return codeUsageError, e.Message
	case sessioncore.KindUnknownSession:
		return codeUnknownSession, e.Message
	case sessioncore.KindSessionExists:
		return codeSessionExists, e.Message
	case sessioncore.KindSessionLocked:
		return codeSessionLocked, e.Message
	case sessioncore.KindInternal:
		return codeInternalError, e.Message
	default:
		return codeOpenFailed, e.Message
	}
}

func (s *server) writeSessionError(id json.RawMessage, e *sessioncore.Error) {
	code, msg := rpcErrorFor(e)
	s.writeError(id, code, msg)
}

func turnResultOf(r sessioncore.TurnResult) turnResult {
	return turnResult{Session: r.Session, Record: r.Record, Stop: r.Stop, ExitCode: r.ExitCode, Turns: r.Turns,
		Usage: usageSummaryOf(r.Usage)}
}

// dispatchStart opens a session and queues its first turn, on the read-loop
// goroutine. The response is written by the session's worker when the opening
// turn settles.
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

	var timeout time.Duration
	if p.ConsentTimeout != "" {
		d, err := time.ParseDuration(p.ConsentTimeout)
		if err == nil {
			d, err = parseConsentTimeout(d)
		}
		if err != nil {
			s.writeError(req.ID, codeUsageError, fmt.Sprintf("session.start consent_timeout %q: %v", p.ConsentTimeout, err))
			return
		}
		timeout = d
	}

	id := req.ID
	if err := s.mgr.Start(sessioncore.StartParams{
		ID: p.Session, Dir: p.Dir, Prompt: p.Prompt,
		Model: p.Model, Harness: p.Harness, HarnessConfig: p.HarnessConfig,
		MaxTurns: p.MaxTurns, TokenBudget: p.TokenBudget,
		ConsentMode: p.ConsentMode, ContainmentProvided: p.ContainmentProvided, NeverAllow: p.NeverAllow,
		ConsentTimeout: timeout, ReadOnly: p.ReadOnly, AskMode: p.AskMode,
	}, s.notifier(p.Session), sessioncore.Turn{Done: func(r sessioncore.TurnResult) {
		s.writeResult(id, turnResultOf(r))
	}}); err != nil {
		s.writeSessionError(req.ID, err)
	}
}

// dispatchSubmit queues the next turn on an already-open session. It never runs
// the turn itself and never rejects a busy session.
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
	id := req.ID
	if err := s.mgr.Submit(p.Session, p.Prompt, sessioncore.Turn{Done: func(r sessioncore.TurnResult) {
		s.writeResult(id, turnResultOf(r))
	}}); err != nil {
		s.writeSessionError(req.ID, err)
	}
}

// dispatchClose queues the end of a session behind the turns it has already
// accepted. It never waits, so a slow turn ahead of it cannot stall the loop
// that session.cancel needs.
func (s *server) dispatchClose(req rpcRequest) {
	var p closeParams
	if err := json.Unmarshal(req.Params, &p); err != nil {
		s.writeError(req.ID, codeInvalidParams, fmt.Sprintf("session.close params: %v", err))
		return
	}
	if p.Session == "" {
		s.writeError(req.ID, codeInvalidParams, "session.close needs a session id")
		return
	}
	id := req.ID
	if err := s.mgr.Close(p.Session, func(e *sessioncore.Error) {
		if e != nil {
			s.writeSessionError(id, e)
			return
		}
		s.writeResult(id, closeResult{Session: p.Session, Closed: true})
	}); err != nil {
		s.writeSessionError(req.ID, err)
	}
}

// handleUsage answers session.usage inline on the read loop, like cancel: it
// reads a snapshot and never queues behind the turn it is asking about.
func (s *server) handleUsage(req rpcRequest) {
	var p usageParams
	if err := json.Unmarshal(req.Params, &p); err != nil {
		s.writeError(req.ID, codeInvalidParams, fmt.Sprintf("session.usage params: %v", err))
		return
	}
	if p.Session == "" {
		s.writeError(req.ID, codeInvalidParams, "session.usage needs a session id")
		return
	}
	u, err := s.mgr.Usage(p.Session)
	if err != nil {
		s.writeSessionError(req.ID, err)
		return
	}
	s.writeResult(req.ID, usageResult{Session: p.Session, Usage: usageSummaryOf(u)})
}

// handleCancel cancels a session's in-flight turn. It runs inline on the read
// loop so a cancel reaches the turn it targets while that turn is still running,
// never queued behind it.
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
	if err := s.mgr.Cancel(p.Session); err != nil {
		s.writeSessionError(req.ID, err)
		return
	}
	s.writeResult(req.ID, cancelResult{Session: p.Session, Cancelled: true})
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
