package main

// `kopicode mcp` (ADR-0015): an MCP (Model Context Protocol) server over stdio,
// so an MCP-capable agent — Claude Code, or any other — adds kopicode to its
// server list and drives coding sessions with no bespoke client. It is a second
// protocol skin over the session lifecycle `kopicode serve` already uses
// (cmd/kopicode/session); `serve`'s own wire is unchanged.
//
// The transport is the one MCP's stdio binding specifies and `serve` already
// speaks: one JSON-RPC 2.0 message per line, the orchestrator owning the child's
// stdin/stdout. What differs from `serve` is the vocabulary on top:
//
//	initialize / notifications/initialized / ping
//	tools/list, tools/call           four tools, below
//	notifications/progress           a session's events, tied to the call
//	notifications/cancelled          cancels the turn behind a call
//	elicitation/create               server → client: remote_interactive consent
//
// # Tools
//
// kopicode_start, kopicode_submit, kopicode_cancel and kopicode_close mirror
// serve's four methods. start and submit block until the turn settles, as a
// tools/call must, and return the same outcome `run --print`'s last line gives
// (stop, exit_code, turns, and on start the record path). A task that did not
// complete is an isError result, not a protocol error: the call worked and the
// agent is told the outcome.
//
// # Events
//
// When a call carries _meta.progressToken, every journal event of the turn it is
// running is sent as a notifications/progress whose message is the schema-1
// `record` JSON `run --print` emits — no new event vocabulary and no second
// transcript. Without a token nothing is sent; the record on disk is the same
// either way.
//
// # Consent
//
// session start requires consent_mode exactly as on serve (ADR-0016/0017). The
// MCP wire carries no credential. remote_interactive is answered with MCP's
// elicitation primitive and so needs a client that declared the `elicitation`
// capability; start refuses it otherwise.

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	sessioncore "github.com/leejianrong/kopicode/cmd/kopicode/session"
	"github.com/leejianrong/kopicode/internal/build"
	"github.com/leejianrong/kopicode/internal/engine"
)

// mcpProtocolVersions are the MCP revisions this server speaks, newest first. A
// client naming one of them gets it back; anything else gets the newest.
var mcpProtocolVersions = []string{"2025-06-18", "2025-03-26", "2024-11-05"}

const (
	toolStart  = "kopicode_start"
	toolSubmit = "kopicode_submit"
	toolCancel = "kopicode_cancel"
	toolClose  = "kopicode_close"
)

// mcpInstructions is shown to the connecting agent in initialize's result.
const mcpInstructions = "kopicode runs coding tasks in a working tree. Call kopicode_start with a dir, a prompt " +
	"and an explicit consent_mode (\"auto\" to let it run shell inside the tree without asking, with a fixed " +
	"never-allow list; \"remote_interactive\" to be asked about each shell command or out-of-tree write; " +
	"\"unattended_policy\" with containment_provided for a declared allowlist). The call returns when the task " +
	"stops; use kopicode_submit for follow-up turns in the same session, and kopicode_close when finished."

// mcpCmd is `kopicode mcp`.
func mcpCmd(args []string, stdout, stderr io.Writer) int {
	base, timeout, code, ok := residentOptions("mcp", "a task arrives as a kopicode_start tool call, not on the "+
		"command line", args, stderr)
	if !ok {
		return code
	}
	return mcpServeWith(context.Background(), os.Stdin, stdout, stderr, base, timeout)
}

// mcpServe is the run loop, taking its streams and base options in the open so a
// test can drive scripted lines and point base.ProviderBaseURL at an httptest
// server, the same seam serve uses.
func mcpServe(ctx context.Context, stdin io.Reader, stdout, stderr io.Writer, base engine.Options) int {
	return mcpServeWith(ctx, stdin, stdout, stderr, base, remoteConsentTimeout)
}

// mcpServeWith is mcpServe with the live-consent timeout stated.
func mcpServeWith(ctx context.Context, stdin io.Reader, stdout, stderr io.Writer, base engine.Options, consentTimeout time.Duration) int {
	s := &mcpServer{
		consentTimeout: consentTimeout,
		stderr:         stderr,
		stop:           make(chan struct{}),
		calls:          map[string]*mcpCall{},
		targets:        map[string]*mcpCall{},
	}
	s.mgr = sessioncore.New(ctx, base, stderr, func(id string, _ time.Duration) engine.Consenter {
		return newElicitConsenter(s, id).Ask
	})
	s.enc = json.NewEncoder(stdout)
	s.enc.SetEscapeHTML(false)
	return s.run(stdin)
}

type mcpServer struct {
	mgr    *sessioncore.Manager
	stderr io.Writer

	// consentTimeout bounds every elicitation (--consent-timeout).
	consentTimeout time.Duration

	enc   *json.Encoder
	encMu sync.Mutex

	// stop is closed when the client goes away, releasing every call still
	// waiting on a turn.
	stop chan struct{}
	wg   sync.WaitGroup

	// mu guards the fields below.
	mu            sync.Mutex
	clientElicits bool                // the client declared the elicitation capability
	calls         map[string]*mcpCall // in-flight tools/call, by JSON-RPC request id
	targets       map[string]*mcpCall // session id -> the call whose turn is running
	sessionSeq    int64

	// consentMu, consentSeq and pending back every elicitConsenter's blocking
	// round trip (mcp_consent.go).
	consentMu  sync.Mutex
	consentSeq int64
	pending    map[string]chan elicitReply
}

// mcpCall is one in-flight tools/call.
type mcpCall struct {
	token     json.RawMessage // _meta.progressToken, or nil
	session   atomic.Value    // string: the session this call drives, once known
	cancelled atomic.Bool

	progMu   sync.Mutex
	progress float64
}

func (c *mcpCall) sessionID() string {
	v, _ := c.session.Load().(string)
	return v
}

// --- messages -----------------------------------------------------------------

// mcpLine is every field an inbound line might carry: a request (method + id), a
// notification (method, no id), or a client's reply to a server-initiated
// request (result or error, no method).
type mcpLine struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type toolContent struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// toolResult is tools/call's result. Content carries the outcome as text for any
// client; StructuredContent carries it as data for one that reads it.
type toolResult struct {
	Content           []toolContent `json:"content"`
	StructuredContent any           `json:"structuredContent,omitempty"`
	IsError           bool          `json:"isError,omitempty"`
}

// mcpTurnOutcome is a turn's outcome as structured content.
type mcpTurnOutcome struct {
	Session  string `json:"session"`
	Record   string `json:"record,omitempty"`
	Stop     string `json:"stop"`
	ExitCode int    `json:"exit_code"`
	Turns    int    `json:"turns"`
	// Usage is the session's usage as the turn settled; see usageSummary.
	Usage *usageSummary `json:"usage,omitempty"`
}

func textResult(text string, structured any, isError bool) toolResult {
	return toolResult{Content: []toolContent{{Type: "text", Text: text}}, StructuredContent: structured, IsError: isError}
}

func errorResult(format string, args ...any) toolResult {
	return textResult(fmt.Sprintf(format, args...), nil, true)
}

// structuredResult renders structured as both the text block and the data, which
// is what the MCP spec recommends for a client that reads only one of them.
func structuredResult(structured any, isError bool) toolResult {
	b, err := marshalNoEscape(structured)
	if err != nil {
		return errorResult("encoding the result: %v", err)
	}
	return textResult(strings.TrimSpace(string(b)), structured, isError)
}

// marshalNoEscape is json.Marshal with HTML escaping off, for the reason every
// emitter in this binary gives: <, > and & must read as themselves.
func marshalNoEscape(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// --- the loop -----------------------------------------------------------------

func (s *mcpServer) run(stdin io.Reader) int {
	br := bufio.NewReader(stdin)
	for {
		line, err := br.ReadString('\n')
		if trimmed := strings.TrimSpace(line); trimmed != "" {
			s.handleLine(trimmed)
		}
		if err != nil {
			if err != io.EOF {
				say(s.stderr, "kopicode: reading the mcp input: %v\n", err)
			}
			break
		}
	}
	// The client is gone. Release every call still waiting on a turn, then end
	// the sessions so each one's record gets its closing event.
	close(s.stop)
	s.wg.Wait()
	s.mgr.Shutdown()
	return exitSuccess
}

func (s *mcpServer) handleLine(line string) {
	var raw mcpLine
	if err := json.Unmarshal([]byte(line), &raw); err != nil {
		s.writeError(nil, codeParseError, fmt.Sprintf("not valid JSON: %v", err))
		return
	}

	if raw.Method == "" {
		if raw.Result != nil || raw.Error != nil {
			s.deliverElicitReply(raw)
			return
		}
		s.writeError(raw.ID, codeInvalidRequest, "message has no method")
		return
	}

	if len(raw.ID) == 0 { // a notification: no id, no response
		switch raw.Method {
		case "notifications/cancelled":
			s.handleCancelled(raw.Params)
		default: // notifications/initialized and anything unknown
		}
		return
	}

	switch raw.Method {
	case "initialize":
		s.handleInitialize(raw)
	case "ping":
		s.writeResult(raw.ID, struct{}{})
	case "tools/list":
		s.writeResult(raw.ID, struct {
			Tools []mcpTool `json:"tools"`
		}{Tools: mcpTools()})
	case "tools/call":
		s.dispatchCall(raw)
	default:
		s.writeError(raw.ID, codeMethodNotFound, fmt.Sprintf("unknown method %q", raw.Method))
	}
}

func (s *mcpServer) handleInitialize(raw mcpLine) {
	var p struct {
		ProtocolVersion string `json:"protocolVersion"`
		Capabilities    struct {
			Elicitation *json.RawMessage `json:"elicitation"`
		} `json:"capabilities"`
	}
	if len(raw.Params) > 0 {
		if err := json.Unmarshal(raw.Params, &p); err != nil {
			s.writeError(raw.ID, codeInvalidParams, fmt.Sprintf("initialize params: %v", err))
			return
		}
	}
	version := mcpProtocolVersions[0]
	for _, v := range mcpProtocolVersions {
		if v == p.ProtocolVersion {
			version = v
		}
	}
	s.mu.Lock()
	s.clientElicits = p.Capabilities.Elicitation != nil
	s.mu.Unlock()

	s.writeResult(raw.ID, map[string]any{
		"protocolVersion": version,
		"capabilities":    map[string]any{"tools": map[string]any{"listChanged": false}},
		"serverInfo":      map[string]any{"name": "kopicode", "version": build.Current().String()},
		"instructions":    mcpInstructions,
	})
}

// handleCancelled implements notifications/cancelled: stop the turn behind the
// named request. Per the spec the cancelled request gets no response.
func (s *mcpServer) handleCancelled(params json.RawMessage) {
	var p struct {
		RequestID json.RawMessage `json:"requestId"`
	}
	if json.Unmarshal(params, &p) != nil || len(p.RequestID) == 0 {
		return
	}
	s.mu.Lock()
	call := s.calls[string(p.RequestID)]
	s.mu.Unlock()
	if call == nil {
		return
	}
	call.cancelled.Store(true)
	if id := call.sessionID(); id != "" {
		_ = s.mgr.Cancel(id) // the session may already be gone; the flag still suppresses the reply
	}
}

// --- tools/call -----------------------------------------------------------------

func (s *mcpServer) dispatchCall(raw mcpLine) {
	var p struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
		Meta      struct {
			ProgressToken json.RawMessage `json:"progressToken"`
		} `json:"_meta"`
	}
	if err := json.Unmarshal(raw.Params, &p); err != nil {
		s.writeError(raw.ID, codeInvalidParams, fmt.Sprintf("tools/call params: %v", err))
		return
	}
	switch p.Name {
	case toolStart, toolSubmit, toolCancel, toolClose:
	default:
		s.writeError(raw.ID, codeInvalidParams, fmt.Sprintf("unknown tool %q; this server has %s, %s, %s and %s",
			p.Name, toolStart, toolSubmit, toolCancel, toolClose))
		return
	}

	call := &mcpCall{token: p.Meta.ProgressToken}
	key := string(raw.ID)
	s.mu.Lock()
	s.calls[key] = call
	s.mu.Unlock()

	// A call runs on its own goroutine: it blocks until its turn settles, and the
	// read loop must stay free to deliver a cancel or a consent reply meanwhile.
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		defer func() {
			s.mu.Lock()
			delete(s.calls, key)
			s.mu.Unlock()
		}()

		var res toolResult
		switch p.Name {
		case toolStart:
			res = s.toolStart(call, p.Arguments)
		case toolSubmit:
			res = s.toolSubmit(call, p.Arguments)
		case toolCancel:
			res = s.toolCancel(p.Arguments)
		case toolClose:
			res = s.toolClose(p.Arguments)
		}
		select {
		case <-s.stop:
			return
		default:
		}
		if call.cancelled.Load() {
			return // the client withdrew the request; the spec says to send no response
		}
		s.writeResult(raw.ID, res)
	}()
}

type startArgs struct {
	Session             string   `json:"session"`
	Dir                 string   `json:"dir"`
	Prompt              string   `json:"prompt"`
	Model               string   `json:"model"`
	Harness             string   `json:"harness"`
	HarnessConfig       string   `json:"harness_config"`
	MaxTurns            int      `json:"max_turns"`
	TokenBudget         *int     `json:"token_budget"`
	ConsentMode         string   `json:"consent_mode"`
	ContainmentProvided bool     `json:"containment_provided"`
	NeverAllow          []string `json:"never_allow"`
}

func (s *mcpServer) toolStart(call *mcpCall, raw json.RawMessage) toolResult {
	var a startArgs
	if err := decodeArgs(raw, &a); err != nil {
		return errorResult("%s arguments: %v", toolStart, err)
	}
	switch {
	case a.Dir == "":
		return errorResult("%s needs a dir: the working tree to run in", toolStart)
	case a.Prompt == "":
		return errorResult("%s needs a prompt: the first turn's task", toolStart)
	}
	s.mu.Lock()
	elicits := s.clientElicits
	s.mu.Unlock()
	if a.ConsentMode == sessioncore.ConsentRemoteInteractive && !elicits {
		return errorResult("consent_mode %q asks the client about each action using MCP elicitation, and "+
			"this client did not declare that capability; use %q or %q", sessioncore.ConsentRemoteInteractive,
			sessioncore.ConsentAuto, sessioncore.ConsentUnattendedPolicy)
	}
	if a.Session == "" {
		a.Session = s.newSessionID()
	}
	call.session.Store(a.Session)

	ch := make(chan sessioncore.TurnResult, 1)
	if err := s.mgr.Start(sessioncore.StartParams{
		ID: a.Session, Dir: a.Dir, Prompt: a.Prompt,
		Model: a.Model, Harness: a.Harness, HarnessConfig: a.HarnessConfig,
		MaxTurns: a.MaxTurns, TokenBudget: a.TokenBudget,
		ConsentMode: a.ConsentMode, ContainmentProvided: a.ContainmentProvided, NeverAllow: a.NeverAllow,
	}, s.observer(a.Session), s.turn(a.Session, call, ch)); err != nil {
		return errorResult("%s", err.Message)
	}
	if call.cancelled.Load() { // a cancel that arrived before the session existed
		_ = s.mgr.Cancel(a.Session)
	}
	return s.await(ch)
}

type submitArgs struct {
	Session string `json:"session"`
	Prompt  string `json:"prompt"`
}

func (s *mcpServer) toolSubmit(call *mcpCall, raw json.RawMessage) toolResult {
	var a submitArgs
	if err := decodeArgs(raw, &a); err != nil {
		return errorResult("%s arguments: %v", toolSubmit, err)
	}
	if a.Session == "" || a.Prompt == "" {
		return errorResult("%s needs a session id and a prompt: the next turn's task", toolSubmit)
	}
	call.session.Store(a.Session)

	ch := make(chan sessioncore.TurnResult, 1)
	if err := s.mgr.Submit(a.Session, a.Prompt, s.turn(a.Session, call, ch)); err != nil {
		return errorResult("%s", err.Message)
	}
	return s.await(ch)
}

type sessionArgs struct {
	Session string `json:"session"`
}

func (s *mcpServer) toolCancel(raw json.RawMessage) toolResult {
	var a sessionArgs
	if err := decodeArgs(raw, &a); err != nil {
		return errorResult("%s arguments: %v", toolCancel, err)
	}
	if a.Session == "" {
		return errorResult("%s needs a session id", toolCancel)
	}
	if err := s.mgr.Cancel(a.Session); err != nil {
		return errorResult("%s", err.Message)
	}
	return structuredResult(struct {
		Session   string `json:"session"`
		Cancelled bool   `json:"cancelled"`
	}{a.Session, true}, false)
}

func (s *mcpServer) toolClose(raw json.RawMessage) toolResult {
	var a sessionArgs
	if err := decodeArgs(raw, &a); err != nil {
		return errorResult("%s arguments: %v", toolClose, err)
	}
	if a.Session == "" {
		return errorResult("%s needs a session id", toolClose)
	}
	done := make(chan *sessioncore.Error, 1)
	if err := s.mgr.Close(a.Session, func(e *sessioncore.Error) { done <- e }); err != nil {
		return errorResult("%s", err.Message)
	}
	select {
	case e := <-done:
		if e != nil {
			return errorResult("%s", e.Message)
		}
		return structuredResult(struct {
			Session string `json:"session"`
			Closed  bool   `json:"closed"`
		}{a.Session, true}, false)
	case <-s.stop:
		return errorResult("the client disconnected")
	}
}

// turn builds the Turn callbacks for a call: while the turn runs, the session's
// events are tied to this call, and its outcome is delivered on ch.
func (s *mcpServer) turn(session string, call *mcpCall, ch chan<- sessioncore.TurnResult) sessioncore.Turn {
	return sessioncore.Turn{
		Begin: func() {
			s.mu.Lock()
			s.targets[session] = call
			s.mu.Unlock()
		},
		Done: func(r sessioncore.TurnResult) {
			s.mu.Lock()
			if s.targets[session] == call {
				delete(s.targets, session)
			}
			s.mu.Unlock()
			ch <- r
		},
	}
}

func (s *mcpServer) await(ch <-chan sessioncore.TurnResult) toolResult {
	select {
	case r := <-ch:
		return structuredResult(mcpTurnOutcome{
			Session: r.Session, Record: r.Record, Stop: r.Stop, ExitCode: r.ExitCode, Turns: r.Turns,
			Usage: usageSummaryOf(r.Usage),
		}, r.ExitCode != 0)
	case <-s.stop:
		return errorResult("the client disconnected")
	}
}

// observer is the engine.Observer a session announces its record through: while
// a call's turn is running, each non-delta event becomes a progress notification
// on that call's token, carrying the schema-1 record. Deltas are dropped for
// print.go's reason — they are not in the record.
func (s *mcpServer) observer(session string) engine.Observer {
	return func(ev engine.Event) {
		if ev.Kind == engine.EventDelta {
			return
		}
		s.mu.Lock()
		call := s.targets[session]
		s.mu.Unlock()
		if call == nil || len(call.token) == 0 {
			return
		}
		msg, err := marshalNoEscape(recordOf(ev))
		if err != nil {
			return
		}
		call.progMu.Lock()
		call.progress++
		n := call.progress
		call.progMu.Unlock()
		s.write(struct {
			JSONRPC string `json:"jsonrpc"`
			Method  string `json:"method"`
			Params  any    `json:"params"`
		}{jsonrpcVersion, "notifications/progress", map[string]any{
			"progressToken": call.token,
			"progress":      n,
			"message":       strings.TrimSpace(string(msg)),
		}})
	}
}

func (s *mcpServer) newSessionID() string {
	var b [6]byte
	if _, err := rand.Read(b[:]); err == nil {
		return "mcp-" + hex.EncodeToString(b[:])
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sessionSeq++
	return "mcp-" + strconv.FormatInt(time.Now().UnixNano(), 36) + "-" + strconv.FormatInt(s.sessionSeq, 10)
}

// decodeArgs decodes tool arguments, treating absent arguments as an empty
// object and refusing a field this tool does not take — a misspelt parameter is
// the caller's mistake and silently ignoring it would run the wrong thing.
func decodeArgs(raw json.RawMessage, into any) error {
	if len(raw) == 0 || string(raw) == "null" {
		raw = json.RawMessage("{}")
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	return dec.Decode(into)
}

// --- writing the wire -----------------------------------------------------------

func (s *mcpServer) writeResult(id json.RawMessage, result any) {
	s.write(rpcResponse{JSONRPC: jsonrpcVersion, ID: idOrNull(id), Result: result})
}

func (s *mcpServer) writeError(id json.RawMessage, code int, message string) {
	s.write(rpcResponse{JSONRPC: jsonrpcVersion, ID: idOrNull(id), Error: &rpcError{Code: code, Message: message}})
}

func (s *mcpServer) write(v any) {
	s.encMu.Lock()
	defer s.encMu.Unlock()
	if err := s.enc.Encode(v); err != nil {
		slog.Warn("writing the mcp output failed", "error", err)
		say(s.stderr, "kopicode: writing the mcp output: %v\n", err)
	}
}

// --- tool catalogue ---------------------------------------------------------------

type mcpTool struct {
	Name        string          `json:"name"`
	Title       string          `json:"title"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"inputSchema"`
}

func mcpTools() []mcpTool {
	return []mcpTool{
		{
			Name:  toolStart,
			Title: "Start a kopicode session",
			Description: "Open a coding session in a working tree and run its first task to completion. Returns " +
				"when the task stops: stop, exit_code, turns, and the path of the session record. A task that did " +
				"not complete is returned as an error result. consent_mode is required and is never defaulted.",
			InputSchema: json.RawMessage(`{
  "type": "object",
  "properties": {
    "dir": {"type": "string", "description": "The working tree to run in."},
    "prompt": {"type": "string", "description": "The first turn's task."},
    "consent_mode": {
      "type": "string",
      "enum": ["auto", "remote_interactive", "unattended_policy"],
      "description": "Who answers permission requests. auto: shell inside dir is allowed without asking and a fixed never-allow list (sudo, rm outside dir, forced git push, a download piped into a shell, writes outside dir) is refused; it is not a sandbox. remote_interactive: each shell command or out-of-tree write is put to you as an elicitation (needs a client that supports elicitation). unattended_policy: the server's --policy-file allowlist answers; needs containment_provided."
    },
    "containment_provided": {"type": "boolean", "description": "Required, and must be true, for unattended_policy: you are supplying real process or container containment."},
    "never_allow": {"type": "array", "items": {"type": "string"}, "description": "auto only: extra never-allow entries, each 'command [token ...]', for example 'terraform apply'. Adds to the built-in list; cannot remove from it."},
    "session": {"type": "string", "description": "An id for the session; generated if omitted."},
    "model": {"type": "string", "description": "Model id override."},
    "harness": {"type": "string", "description": "Built-in harness config name override."},
    "harness_config": {"type": "string", "description": "Path to a declared harness-config file; the same axis as harness."}
  },
  "required": ["dir", "prompt", "consent_mode"],
  "additionalProperties": false
}`),
		},
		{
			Name:  toolSubmit,
			Title: "Send a follow-up task",
			Description: "Queue the next turn on an open session and wait for it to settle. A submit while a turn " +
				"is running waits its place; it is never rejected as busy.",
			InputSchema: json.RawMessage(`{
  "type": "object",
  "properties": {
    "session": {"type": "string", "description": "The session id returned by kopicode_start."},
    "prompt": {"type": "string", "description": "The next turn's task."}
  },
  "required": ["session", "prompt"],
  "additionalProperties": false
}`),
		},
		{
			Name:  toolCancel,
			Title: "Cancel the running turn",
			Description: "Cancel the turn running now on a session, and only that one; queued turns keep their place. " +
				"The cancelled turn reports its own stop through the start or submit call that is waiting on it.",
			InputSchema: json.RawMessage(`{
  "type": "object",
  "properties": {"session": {"type": "string", "description": "The session id."}},
  "required": ["session"],
  "additionalProperties": false
}`),
		},
		{
			Name:  toolClose,
			Title: "Close a session",
			Description: "End a session after the turns it has already accepted finish, write its closing event, and " +
				"release its working tree so the id can be reused.",
			InputSchema: json.RawMessage(`{
  "type": "object",
  "properties": {"session": {"type": "string", "description": "The session id."}},
  "required": ["session"],
  "additionalProperties": false
}`),
		},
	}
}
