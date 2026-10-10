package main

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/leejianrong/kopicode/internal/engine"
)

// This file is `kopicode mcp` (ADR-0015), driven end to end over its own stdio
// protocol by a scripted MCP client — the seam serve_test.go uses for serve,
// with the client standing in for Claude Code or any other MCP-capable agent.
// It reuses serveHarness: both are newline-framed JSON-RPC over pipes, and only
// the vocabulary differs.

func startMCP(t *testing.T, base engine.Options) *serveHarness {
	t.Helper()
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	h := &serveHarness{t: t, inW: inW, exit: make(chan int, 1), decoded: make(chan struct{})}

	go func() {
		defer close(h.decoded)
		sc := bufio.NewScanner(outR)
		sc.Buffer(make([]byte, 0, 1<<16), 1<<20)
		for sc.Scan() {
			var m map[string]any
			if err := json.Unmarshal(sc.Bytes(), &m); err == nil {
				h.mu.Lock()
				h.all = append(h.all, m)
				h.mu.Unlock()
			}
		}
	}()
	go func() {
		code := mcpServe(context.Background(), inR, outW, io.Discard, base)
		_ = outW.Close()
		h.exit <- code
	}()
	t.Cleanup(func() { _ = inW.Close() })
	return h
}

// initialize performs the handshake, optionally declaring elicitation support.
func initialize(h *serveHarness, id int, elicitation bool) map[string]any {
	caps := map[string]any{}
	if elicitation {
		caps["elicitation"] = map[string]any{}
	}
	h.send(map[string]any{"jsonrpc": "2.0", "id": id, "method": "initialize", "params": map[string]any{
		"protocolVersion": "2025-06-18", "capabilities": caps,
		"clientInfo": map[string]any{"name": "test-client", "version": "0"},
	}})
	resp := h.awaitResponse(id)
	h.send(map[string]any{"jsonrpc": "2.0", "method": "notifications/initialized"})
	return resp
}

// callTool sends a tools/call, with a progress token when token is non-nil.
func callTool(h *serveHarness, id int, name string, args map[string]any, token any) {
	params := map[string]any{"name": name, "arguments": args}
	if token != nil {
		params["_meta"] = map[string]any{"progressToken": token}
	}
	h.send(map[string]any{"jsonrpc": "2.0", "id": id, "method": "tools/call", "params": params})
}

func toolOutcome(t *testing.T, resp map[string]any) (structured map[string]any, isError bool) {
	t.Helper()
	if resp["error"] != nil {
		t.Fatalf("protocol error: %v", resp["error"])
	}
	result, _ := resp["result"].(map[string]any)
	structured, _ = result["structuredContent"].(map[string]any)
	isError, _ = result["isError"].(bool)
	return structured, isError
}

func toolText(resp map[string]any) string {
	result, _ := resp["result"].(map[string]any)
	content, _ := result["content"].([]any)
	if len(content) == 0 {
		return ""
	}
	c, _ := content[0].(map[string]any)
	s, _ := c["text"].(string)
	return s
}

// progressEvents returns the schema-1 records carried by progress notifications
// for token.
func progressEvents(t *testing.T, h *serveHarness, token any) []map[string]any {
	t.Helper()
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []map[string]any
	for _, m := range h.all {
		if m["method"] != "notifications/progress" {
			continue
		}
		params, _ := m["params"].(map[string]any)
		if params["progressToken"] != token {
			continue
		}
		msg, _ := params["message"].(string)
		var rec map[string]any
		if err := json.Unmarshal([]byte(msg), &rec); err != nil {
			t.Fatalf("progress message is not a record: %q: %v", msg, err)
		}
		out = append(out, rec)
	}
	return out
}

func TestMCPInitializeNegotiatesAndListsTheFiveTools(t *testing.T) {
	h := startMCP(t, engine.Options{})
	resp := initialize(h, 1, false)
	result, _ := resp["result"].(map[string]any)
	if result["protocolVersion"] != "2025-06-18" {
		t.Errorf("protocolVersion = %v, want the client's 2025-06-18", result["protocolVersion"])
	}
	info, _ := result["serverInfo"].(map[string]any)
	if info["name"] != "kopicode" {
		t.Errorf("serverInfo.name = %v, want kopicode", info["name"])
	}
	if caps, _ := result["capabilities"].(map[string]any); caps["tools"] == nil {
		t.Errorf("capabilities = %v, want tools", caps)
	}

	// An unknown revision gets the newest this server speaks, not an error.
	h.send(map[string]any{"jsonrpc": "2.0", "id": 2, "method": "initialize",
		"params": map[string]any{"protocolVersion": "1999-01-01", "capabilities": map[string]any{}}})
	if r, _ := h.awaitResponse(2)["result"].(map[string]any); r["protocolVersion"] != "2025-06-18" {
		t.Errorf("unknown client version got %v, want the newest", r["protocolVersion"])
	}

	h.send(map[string]any{"jsonrpc": "2.0", "id": 3, "method": "tools/list"})
	list, _ := h.awaitResponse(3)["result"].(map[string]any)
	tools, _ := list["tools"].([]any)
	var names []string
	for _, tl := range tools {
		m, _ := tl.(map[string]any)
		names = append(names, m["name"].(string))
		if m["inputSchema"] == nil || m["description"] == "" {
			t.Errorf("tool %v lacks a schema or description", m["name"])
		}
	}
	if strings.Join(names, ",") != "kopicode_start,kopicode_submit,kopicode_cancel,kopicode_handoff,kopicode_close" {
		t.Errorf("tools = %v", names)
	}

	h.send(map[string]any{"jsonrpc": "2.0", "id": 4, "method": "ping"})
	if h.awaitResponse(4)["error"] != nil {
		t.Error("ping errored")
	}
	if code := h.close(); code != exitSuccess {
		t.Errorf("exit = %d, want %d", code, exitSuccess)
	}
}

func TestMCPProtocolErrors(t *testing.T) {
	h := startMCP(t, engine.Options{})
	h.send(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "resources/list"})
	if e, _ := h.awaitResponse(1)["error"].(map[string]any); !numEq(e["code"], codeMethodNotFound) {
		t.Errorf("unknown method error = %v, want %d", e, codeMethodNotFound)
	}
	h.send(map[string]any{"jsonrpc": "2.0", "id": 2, "method": "tools/call",
		"params": map[string]any{"name": "rm_rf", "arguments": map[string]any{}}})
	if e, _ := h.awaitResponse(2)["error"].(map[string]any); !numEq(e["code"], codeInvalidParams) {
		t.Errorf("unknown tool error = %v, want %d", e, codeInvalidParams)
	}
	// Garbage does not end the process.
	if _, err := h.inW.Write([]byte("not json\n")); err != nil {
		t.Fatal(err)
	}
	h.send(map[string]any{"jsonrpc": "2.0", "id": 3, "method": "ping"})
	if h.awaitResponse(3)["error"] != nil {
		t.Error("a ping after garbage errored")
	}
	h.close()
}

// TestMCPStartRunsATaskAndStreamsItsRecordAsProgress is the adoption path end to
// end: an MCP client calls kopicode_start in auto mode, the model runs a shell
// command with no consent round trip, and the client gets the outcome plus the
// schema-1 event stream as progress notifications on its token.
func TestMCPStartRunsATaskAndStreamsItsRecordAsProgress(t *testing.T) {
	t.Setenv(engine.APIKeyEnv, "kopicode-test-credential")
	srv := scriptedProvider(t,
		sseToolCall("call-1", "run_shell", `{"command":"echo hi"}`),
		sseBody("done"),
	)
	h := startMCP(t, engine.Options{ProviderBaseURL: srv.URL})
	initialize(h, 1, false)

	callTool(h, 2, toolStart, map[string]any{
		"dir": t.TempDir(), "prompt": "run a command", "consent_mode": "auto", "session": "m1",
	}, "tok-1")
	resp := h.awaitResponse(2)
	out, isErr := toolOutcome(t, resp)
	if isErr {
		t.Fatalf("start failed: %s", toolText(resp))
	}
	if out["stop"] != "completed" || out["session"] != "m1" || out["record"] == "" {
		t.Errorf("outcome = %v, want completed on session m1 with a record path", out)
	}
	if !numEq(out["exit_code"], 0) {
		t.Errorf("exit_code = %v, want 0", out["exit_code"])
	}
	var parsed map[string]any
	if err := json.Unmarshal([]byte(toolText(resp)), &parsed); err != nil || parsed["stop"] != "completed" {
		t.Errorf("text content = %q, want the outcome as JSON for clients that read only text", toolText(resp))
	}

	events := progressEvents(t, h, "tok-1")
	var kinds []string
	for _, e := range events {
		kinds = append(kinds, e["kind"].(string))
	}
	if !hasKind(kinds, "tool_result") || !hasKind(kinds, "permission_decided") {
		t.Errorf("progress records = %v, want the shell's tool_result and its decision", kinds)
	}
	for _, e := range events {
		if e["kind"] == "permission_decided" && (e["source"] != "auto" || e["decision"] != "allow") {
			t.Errorf("decision = %v from %v, want allow from auto", e["decision"], e["source"])
		}
	}
	// Progress must increase, as the MCP spec requires.
	h.mu.Lock()
	last := 0.0
	for _, m := range h.all {
		if m["method"] == "notifications/progress" {
			p, _ := m["params"].(map[string]any)
			n, _ := p["progress"].(float64)
			if n <= last {
				t.Errorf("progress %v did not increase past %v", n, last)
			}
			last = n
		}
	}
	h.mu.Unlock()
	h.close()
}

func TestMCPWithoutAProgressTokenSendsNoProgress(t *testing.T) {
	t.Setenv(engine.APIKeyEnv, "kopicode-test-credential")
	srv := scriptedProvider(t, sseBody("done"))
	h := startMCP(t, engine.Options{ProviderBaseURL: srv.URL})
	initialize(h, 1, false)
	callTool(h, 2, toolStart, map[string]any{"dir": t.TempDir(), "prompt": "hi", "consent_mode": "auto"}, nil)
	out, isErr := toolOutcome(t, h.awaitResponse(2))
	if isErr || out["stop"] != "completed" {
		t.Fatalf("start = %v (error %v)", out, isErr)
	}
	if !strings.HasPrefix(out["session"].(string), "mcp-") {
		t.Errorf("generated session id = %v, want an mcp- id", out["session"])
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, m := range h.all {
		if m["method"] == "notifications/progress" {
			t.Errorf("progress sent without a token: %v", m)
		}
	}
	h.close()
}

// TestMCPStartRefusalsAreToolErrors: a caller's mistake comes back as an isError
// result the agent can read and correct — not a protocol error, and not a
// silently-defaulted session.
func TestMCPStartRefusalsAreToolErrors(t *testing.T) {
	h := startMCP(t, engine.Options{})
	initialize(h, 1, false)
	dir := t.TempDir()
	for i, tc := range []struct {
		name string
		args map[string]any
		want string
	}{
		{"no consent_mode", map[string]any{"dir": dir, "prompt": "x"}, "consent_mode"},
		{"unknown consent_mode", map[string]any{"dir": dir, "prompt": "x", "consent_mode": "yolo"}, "consent_mode"},
		{"unattended without containment", map[string]any{"dir": dir, "prompt": "x", "consent_mode": "unattended_policy"}, "containment_provided"},
		{"no dir", map[string]any{"prompt": "x", "consent_mode": "auto"}, "dir"},
		{"no prompt", map[string]any{"dir": dir, "consent_mode": "auto"}, "prompt"},
		{"never_allow beside another mode", map[string]any{"dir": dir, "prompt": "x", "consent_mode": "unattended_policy", "containment_provided": true, "never_allow": []string{"x"}}, "never_allow"},
		{"misspelt argument", map[string]any{"dir": dir, "prompt": "x", "consent_mode": "auto", "consent": "auto"}, "unknown field"},
		{"remote without elicitation", map[string]any{"dir": dir, "prompt": "x", "consent_mode": "remote_interactive"}, "elicitation"},
	} {
		id := 10 + i
		callTool(h, id, toolStart, tc.args, nil)
		resp := h.awaitResponse(id)
		if _, isErr := toolOutcome(t, resp); !isErr {
			t.Errorf("%s: not an error result: %v", tc.name, resp)
			continue
		}
		if !strings.Contains(toolText(resp), tc.want) {
			t.Errorf("%s: text = %q, want it to mention %q", tc.name, toolText(resp), tc.want)
		}
	}
	h.close()
}

// TestMCPRemoteInteractiveUsesElicitation: with a client that declared the
// capability, each permission request becomes an elicitation/create, the turn
// blocks on it, and the answer reaches the permission gate attributed remote.
func TestMCPRemoteInteractiveUsesElicitation(t *testing.T) {
	for _, tc := range []struct {
		name   string
		reply  map[string]any
		allow  bool
		source string
	}{
		{"accept allow", map[string]any{"action": "accept", "content": map[string]any{"decision": "allow"}}, true, "remote"},
		{"accept deny", map[string]any{"action": "accept", "content": map[string]any{"decision": "deny"}}, false, "remote"},
		{"decline", map[string]any{"action": "decline"}, false, "remote"},
		{"cancel", map[string]any{"action": "cancel"}, false, "remote"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(engine.APIKeyEnv, "kopicode-test-credential")
			srv := scriptedProvider(t,
				sseToolCall("call-1", "run_shell", `{"command":"echo hi"}`),
				sseBody("done"),
			)
			h := startMCP(t, engine.Options{ProviderBaseURL: srv.URL})
			initialize(h, 1, true)
			callTool(h, 2, toolStart, map[string]any{
				"dir": t.TempDir(), "prompt": "go", "consent_mode": "remote_interactive",
			}, "tok")

			req := h.awaitRequest("elicitation/create")
			params, _ := req["params"].(map[string]any)
			if msg, _ := params["message"].(string); !strings.Contains(msg, "echo hi") {
				t.Errorf("elicitation message = %q, want it to show the command", msg)
			}
			if params["requestedSchema"] == nil {
				t.Error("elicitation has no requestedSchema")
			}
			h.send(map[string]any{"jsonrpc": "2.0", "id": req["id"], "result": tc.reply})

			if _, isErr := toolOutcome(t, h.awaitResponse(2)); isErr {
				t.Fatal("the task did not complete")
			}
			var decision, source string
			ran := false
			for _, e := range progressEvents(t, h, "tok") {
				switch e["kind"] {
				case "permission_decided":
					decision, _ = e["decision"].(string)
					source, _ = e["source"].(string)
				case "tool_result":
					ran = true
				}
			}
			if source != tc.source {
				t.Errorf("source = %q, want %q", source, tc.source)
			}
			if tc.allow != (decision == "allow") {
				t.Errorf("decision = %q, want allow=%v", decision, tc.allow)
			}
			if !ran {
				t.Error("no tool_result: a refused command still reports back to the model")
			}
			h.close()
		})
	}
}

// TestMCPSubmitCloseAndReuse drives the session lifecycle: a follow-up turn on
// the same session, a close, and the id reusable on the same tree afterwards.
func TestMCPSubmitCloseAndReuse(t *testing.T) {
	t.Setenv(engine.APIKeyEnv, "kopicode-test-credential")
	srv := scriptedProvider(t, sseBody("one"), sseBody("two"), sseBody("three"))
	h := startMCP(t, engine.Options{ProviderBaseURL: srv.URL})
	initialize(h, 1, false)
	dir := t.TempDir()

	callTool(h, 2, toolStart, map[string]any{"dir": dir, "prompt": "a", "consent_mode": "auto", "session": "s"}, nil)
	if out, _ := toolOutcome(t, h.awaitResponse(2)); out["record"] == nil || out["record"] == "" {
		t.Errorf("start outcome = %v, want a record", out)
	}
	callTool(h, 3, toolSubmit, map[string]any{"session": "s", "prompt": "b"}, nil)
	out, isErr := toolOutcome(t, h.awaitResponse(3))
	if isErr || out["stop"] != "completed" {
		t.Errorf("submit outcome = %v (error %v)", out, isErr)
	}
	if out["record"] != nil {
		t.Errorf("submit reported a record path %v; only the opening turn does", out["record"])
	}

	callTool(h, 4, toolClose, map[string]any{"session": "s"}, nil)
	if c, isErr := toolOutcome(t, h.awaitResponse(4)); isErr || c["closed"] != true {
		t.Errorf("close = %v (error %v)", c, isErr)
	}

	callTool(h, 5, toolSubmit, map[string]any{"session": "s", "prompt": "c"}, nil)
	if _, isErr := toolOutcome(t, h.awaitResponse(5)); !isErr {
		t.Error("submit to a closed session succeeded")
	}
	callTool(h, 6, toolStart, map[string]any{"dir": dir, "prompt": "again", "consent_mode": "auto", "session": "s"}, nil)
	if _, isErr := toolOutcome(t, h.awaitResponse(6)); isErr {
		t.Error("the id was not reusable on the same tree after a close")
	}
	h.close()
}

// TestMCPCancelToolStopsTheRunningTurn: kopicode_cancel, called while a start is
// in flight, cancels that turn and the waiting call reports the cancelled stop.
func TestMCPCancelToolStopsTheRunningTurn(t *testing.T) {
	t.Setenv(engine.APIKeyEnv, "kopicode-test-credential")
	srv, reached := gatedProvider(t)
	h := startMCP(t, engine.Options{ProviderBaseURL: srv.URL})
	initialize(h, 1, false)

	callTool(h, 2, toolStart, map[string]any{"dir": t.TempDir(), "prompt": "x", "consent_mode": "auto", "session": "s"}, nil)
	<-reached
	callTool(h, 3, toolCancel, map[string]any{"session": "s"}, nil)
	if c, isErr := toolOutcome(t, h.awaitResponse(3)); isErr || c["cancelled"] != true {
		t.Errorf("cancel = %v (error %v)", c, isErr)
	}
	out, isErr := toolOutcome(t, h.awaitResponse(2))
	if out["stop"] != "cancelled" || !isErr {
		t.Errorf("start outcome = %v (error %v), want a cancelled stop reported as an error result", out, isErr)
	}
	h.close()
}

// TestMCPCancelledNotificationWithdrawsTheCall: notifications/cancelled stops the
// turn behind the named request and, per the MCP spec, that request gets no
// response — while the session stays open for the next call.
func TestMCPCancelledNotificationWithdrawsTheCall(t *testing.T) {
	t.Setenv(engine.APIKeyEnv, "kopicode-test-credential")
	srv, reached := gatedProvider(t)
	h := startMCP(t, engine.Options{ProviderBaseURL: srv.URL})
	initialize(h, 1, false)

	callTool(h, 2, toolStart, map[string]any{"dir": t.TempDir(), "prompt": "x", "consent_mode": "auto", "session": "s"}, nil)
	<-reached
	h.send(map[string]any{"jsonrpc": "2.0", "method": "notifications/cancelled",
		"params": map[string]any{"requestId": 2, "reason": "user stopped it"}})

	// The turn stops, so the session is idle again and answers a close.
	callTool(h, 3, toolClose, map[string]any{"session": "s"}, nil)
	if c, isErr := toolOutcome(t, h.awaitResponse(3)); isErr || c["closed"] != true {
		t.Fatalf("close after cancellation = %v (error %v)", c, isErr)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, m := range h.all {
		if m["method"] == nil && numEq(m["id"], 2) {
			t.Errorf("the cancelled request got a response: %v", m)
		}
	}
}

// TestMCPClientDisconnectEndsSessions: closing stdin mid-turn ends the process
// cleanly and the session's record still gets its closing event, as serve does.
// No call is left to carry that event as progress, so it is checked where it
// lives: in the journal on disk.
func TestMCPClientDisconnectEndsSessions(t *testing.T) {
	t.Setenv(engine.APIKeyEnv, "kopicode-test-credential")
	srv, reached := gatedProvider(t)
	h := startMCP(t, engine.Options{ProviderBaseURL: srv.URL})
	initialize(h, 1, false)
	dir := t.TempDir()
	callTool(h, 2, toolStart, map[string]any{"dir": dir, "prompt": "x", "consent_mode": "auto", "session": "s"}, nil)
	<-reached

	done := make(chan int, 1)
	go func() { done <- h.close() }()
	select {
	case code := <-done:
		if code != exitSuccess {
			t.Errorf("exit = %d, want %d", code, exitSuccess)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("mcp did not exit after its input closed mid-turn")
	}

	matches, err := filepath.Glob(filepath.Join(dir, ".kopicode", "sessions", "s", "*.jsonl"))
	if err != nil || len(matches) == 0 {
		t.Fatalf("no session record under %s: %v", dir, err)
	}
	var record strings.Builder
	for _, m := range matches {
		b, err := os.ReadFile(m)
		if err != nil {
			t.Fatal(err)
		}
		record.Write(b)
	}
	if !strings.Contains(record.String(), "SessionEnded") {
		t.Errorf("the record has no SessionEnded after a mid-turn disconnect")
	}
}
