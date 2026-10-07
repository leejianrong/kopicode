package main

import (
	"fmt"
	"strings"
	"testing"

	"github.com/leejianrong/kopicode/internal/engine"
)

// sseBodyWithUsage is sseBody with the extra fields OpenRouter puts in a usage
// block: the cache split and the provider-reported cost.
func sseBodyWithUsage(text string, prompt, completion, cached int, cost float64) string {
	var b strings.Builder
	fmt.Fprintf(&b, `data: {"id":"gen-1","object":"chat.completion.chunk","model":"qwen/qwen3-coder-next",`+
		`"provider":"Parasail","choices":[{"index":0,"delta":{"role":"assistant","content":%q},`+
		`"finish_reason":null}]}`+"\n\n", text)
	fmt.Fprintf(&b, `data: {"id":"gen-1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{},`+
		`"finish_reason":"stop"}],"usage":{"prompt_tokens":%d,"completion_tokens":%d,"total_tokens":%d,`+
		`"cost":%v,"prompt_tokens_details":{"cached_tokens":%d,"cache_write_tokens":0}}}`+"\n\n",
		prompt, completion, prompt+completion, cost, cached)
	b.WriteString("data: [DONE]\n\n")
	return b.String()
}

func nested(t *testing.T, m map[string]any, path ...string) any {
	t.Helper()
	var cur any = m
	for _, p := range path {
		obj, ok := cur.(map[string]any)
		if !ok {
			t.Fatalf("%v: %q is not an object in %v", path, p, m)
		}
		cur = obj[p]
	}
	return cur
}

// TestServeReportsUsageOnTheTurnResultAndTheEventStream. A client sees the
// split on each provider_response and a session summary on the turn result,
// without summing anything itself.
func TestServeReportsUsageOnTheTurnResultAndTheEventStream(t *testing.T) {
	t.Setenv(engine.APIKeyEnv, "kopicode-test-credential")
	srv := scriptedProvider(t, sseBodyWithUsage("one", 1000, 50, 800, 0.0005))
	h := startServe(t, engine.Options{ProviderBaseURL: srv.URL})

	h.send(map[string]any{"jsonrpc": "2.0", "id": 1, "method": methodSessionStart,
		"params": startPayload("s1", t.TempDir(), "first")})
	res := h.awaitResponse(1)["result"].(map[string]any)

	if !numEq(nested(t, res, "usage", "context_tokens"), 1000) {
		t.Errorf("usage.context_tokens = %v, want 1000", nested(t, res, "usage", "context_tokens"))
	}
	if !numEq(nested(t, res, "usage", "total"), 1050) || !numEq(nested(t, res, "usage", "cache_read"), 800) {
		t.Errorf("usage = %v, want total 1050 and cache_read 800", res["usage"])
	}
	if got := nested(t, res, "usage", "cost_usd"); got != 0.0005 {
		t.Errorf("usage.cost_usd = %v, want the provider's 0.0005", got)
	}
	if _, has := res["usage"].(map[string]any)["context_window"]; !has {
		// The test selection is the registered default, whose window is known.
		t.Errorf("usage has no context_window for a model the registry knows: %v", res["usage"])
	}

	var seen bool
	for _, n := range h.notificationsFor("s1") {
		ev := nested(t, n, "params", "event").(map[string]any)
		if ev["kind"] != "provider_response" {
			continue
		}
		seen = true
		if !numEq(nested(t, ev, "usage", "prompt"), 1000) || !numEq(nested(t, ev, "usage", "completion"), 50) ||
			!numEq(ev["size"], 1050) {
			t.Errorf("provider_response = %v, want the split under usage and the total still in size", ev)
		}
	}
	if !seen {
		t.Error("no provider_response notification reached the client")
	}
	h.close()
}

// TestServeSessionUsageIsAPull. session.usage answers for an open session and
// refuses an unknown one with the same code every other method uses.
func TestServeSessionUsageIsAPull(t *testing.T) {
	t.Setenv(engine.APIKeyEnv, "kopicode-test-credential")
	srv := scriptedProvider(t, sseBodyWithUsage("one", 400, 10, 0, 0.0001))
	h := startServe(t, engine.Options{ProviderBaseURL: srv.URL})

	h.send(map[string]any{"jsonrpc": "2.0", "id": 1, "method": methodSessionStart,
		"params": startPayload("s1", t.TempDir(), "first")})
	h.awaitResponse(1)

	h.send(map[string]any{"jsonrpc": "2.0", "id": 2, "method": methodSessionUsage,
		"params": map[string]any{"session": "s1"}})
	res := h.awaitResponse(2)["result"].(map[string]any)
	if res["session"] != "s1" || !numEq(nested(t, res, "usage", "context_tokens"), 400) {
		t.Errorf("session.usage = %v, want session s1 with context_tokens 400", res)
	}

	h.send(map[string]any{"jsonrpc": "2.0", "id": 3, "method": methodSessionUsage,
		"params": map[string]any{"session": "nope"}})
	errObj, _ := h.awaitResponse(3)["error"].(map[string]any)
	if errObj == nil || !numEq(errObj["code"], codeUnknownSession) {
		t.Errorf("session.usage for an unknown session = %v, want code %d", errObj, codeUnknownSession)
	}

	h.send(map[string]any{"jsonrpc": "2.0", "id": 4, "method": methodSessionUsage, "params": map[string]any{}})
	errObj, _ = h.awaitResponse(4)["error"].(map[string]any)
	if errObj == nil || !numEq(errObj["code"], codeInvalidParams) {
		t.Errorf("session.usage with no session = %v, want code %d", errObj, codeInvalidParams)
	}
	h.close()
}

// TestARouteThatReportsNoCostLeavesItOut. The default fixture body has no
// cost; the summary must omit it, not report zero.
func TestARouteThatReportsNoCostLeavesItOut(t *testing.T) {
	t.Setenv(engine.APIKeyEnv, "kopicode-test-credential")
	srv := scriptedProvider(t, sseBody("one"))
	h := startServe(t, engine.Options{ProviderBaseURL: srv.URL})

	h.send(map[string]any{"jsonrpc": "2.0", "id": 1, "method": methodSessionStart,
		"params": startPayload("s1", t.TempDir(), "first")})
	usage := h.awaitResponse(1)["result"].(map[string]any)["usage"].(map[string]any)
	if _, has := usage["cost_usd"]; has {
		t.Errorf("usage carries cost_usd %v for a route that reported none; unknown must be absent", usage["cost_usd"])
	}
	h.close()
}
