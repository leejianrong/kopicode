package engine_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/leejianrong/kopicode/internal/engine"
)

// ADR-0027. A custom provider URL must never be sent the OpenRouter credential,
// must declare no routing, and a host that is not loopback needs its own key.

const openRouterSentinel = "sk-or-this-must-never-leave-the-machine-0123456789"

func TestACustomHostNeverReceivesTheOpenRouterKey(t *testing.T) {
	t.Setenv(engine.APIKeyEnv, openRouterSentinel)
	t.Setenv(engine.CustomAPIKeyEnv, "")

	var (
		mu   sync.Mutex
		auth string
		body []byte
		n    int
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		auth = r.Header.Get("Authorization")
		body, _ = io.ReadAll(r.Body)
		n++
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, `data: {"id":"x","model":"qwen2.5-coder:7b","choices":[{"index":0,"delta":{"role":"assistant","content":"done"},"finish_reason":"stop"}],"usage":{"prompt_tokens":5,"completion_tokens":1,"total_tokens":6}}`+"\n\n")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	dir := t.TempDir()
	sel, err := engine.ResolveSelection(dir, engine.SelectionOverrides{Model: "qwen2.5-coder:7b", ProviderURL: srv.URL})
	if err != nil {
		t.Fatalf("ResolveSelection: %v", err)
	}
	s, err := engine.Open(t.Context(), engine.Options{Dir: dir, Selection: sel, SessionID: "custom"})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close(context.Background()) })
	if _, err := s.Run(t.Context(), "say done"); err != nil {
		t.Fatalf("Run: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if n == 0 {
		t.Fatal("the custom endpoint was never called")
	}
	if auth != "" {
		t.Errorf("Authorization = %q; a loopback endpoint with no key must get no header, and never the OpenRouter key", auth)
	}
	if strings.Contains(string(body), openRouterSentinel) {
		t.Error("the OpenRouter key reached the request body")
	}
	var sent map[string]json.RawMessage
	if err := json.Unmarshal(body, &sent); err != nil {
		t.Fatalf("body is not JSON: %v", err)
	}
	if _, ok := sent["provider"]; ok {
		t.Errorf("the request carries a provider routing object %s; a custom endpoint is unpinned", sent["provider"])
	}
	if string(sent["model"]) != `"qwen2.5-coder:7b"` {
		t.Errorf("model = %s, want the name passed verbatim", sent["model"])
	}
}

func TestACustomHostThatIsNotLoopbackNeedsItsOwnKey(t *testing.T) {
	t.Setenv(engine.APIKeyEnv, openRouterSentinel)
	t.Setenv(engine.CustomAPIKeyEnv, "")

	dir := t.TempDir()
	sel, err := engine.ResolveSelection(dir, engine.SelectionOverrides{Model: "m", ProviderURL: "https://llm.example.com/v1"})
	if err != nil {
		t.Fatalf("ResolveSelection: %v", err)
	}
	_, err = engine.Open(t.Context(), engine.Options{Dir: dir, Selection: sel, SessionID: "nokey"})
	if !errors.Is(err, engine.ErrNoCustomAPIKey) {
		t.Fatalf("Open = %v, want it to wrap ErrNoCustomAPIKey: the OpenRouter key is set and must not stand in", err)
	}
}
