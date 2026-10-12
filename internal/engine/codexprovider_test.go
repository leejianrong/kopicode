package engine_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/leejianrong/kopicode/internal/engine"
)

// ADR-0035. The Codex provider reads Codex CLI's login and sends it only to the
// Codex backend; the OpenRouter key is neither needed nor sent; the record names
// the route and never holds the token; and a login that cannot be used is refused
// before anything is opened.

const (
	codexFakeToken   = "tok-FAKE-engine-0000"
	codexFakeAccount = "acct-FAKE-engine-1111"
)

// codexHome points CODEX_HOME at a directory holding a login with mode.
func codexHome(t *testing.T, mode os.FileMode, body string) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("CODEX_HOME", home)
	if body != "" {
		path := filepath.Join(home, "auth.json")
		if err := os.WriteFile(path, []byte(body), mode); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(path, mode); err != nil {
			t.Fatal(err)
		}
	}
	return home
}

func fakeLogin() string {
	return fmt.Sprintf(`{"auth_mode":"chatgpt","tokens":{"access_token":%q,"account_id":%q}}`, codexFakeToken, codexFakeAccount)
}

func TestTheCodexRouteSendsOnlyTheCodexLoginAndRecordsNoCredential(t *testing.T) {
	t.Setenv(engine.APIKeyEnv, openRouterSentinel)
	codexHome(t, 0o600, fakeLogin())
	stream, err := os.ReadFile(filepath.Join("..", "provider", "testdata", "codex", "text_turn.sse"))
	if err != nil {
		t.Fatal(err)
	}

	var (
		mu                    sync.Mutex
		path, auth, acct, req string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		path, auth, acct = r.URL.Path, r.Header.Get("Authorization"), r.Header.Get("ChatGPT-Account-ID")
		b, _ := io.ReadAll(r.Body)
		req = string(b)
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write(stream)
	}))
	defer srv.Close()

	dir := t.TempDir()
	sel, err := engine.ResolveSelection(dir, engine.SelectionOverrides{Provider: engine.ProviderCodex, Model: "gpt-5.6-luna"})
	if err != nil {
		t.Fatal(err)
	}
	s, err := engine.Open(t.Context(), engine.Options{Dir: dir, Selection: sel, SessionID: "codex", ProviderBaseURL: srv.URL})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close(context.Background()) })
	if _, err := s.Run(t.Context(), "say ok"); err != nil {
		t.Fatalf("Run: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if path != "/responses" {
		t.Errorf("path = %q, want /responses", path)
	}
	if auth != "Bearer "+codexFakeToken || acct != codexFakeAccount {
		t.Error("the Codex login was not sent as the backend takes it")
	}
	if strings.Contains(auth, openRouterSentinel) || strings.Contains(req, openRouterSentinel) {
		t.Error("the OpenRouter key reached the Codex backend")
	}
	if strings.Contains(req, `"provider"`) || strings.Contains(req, "allow_fallbacks") {
		t.Errorf("a routing object went to the Codex backend: %s", req)
	}

	// The record: the route is named, the credential is nowhere in it.
	var record strings.Builder
	err = filepath.WalkDir(s.Path(), func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, rerr := os.ReadFile(p)
		record.Write(b)
		return rerr
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(record.String(), `"provider_host":"chatgpt.com"`) {
		t.Error("SessionStarted does not name the Codex host")
	}
	if record.Len() == 0 {
		t.Fatal("the session wrote no record at all")
	}
	for _, secret := range []string{codexFakeToken, codexFakeAccount, openRouterSentinel} {
		if strings.Contains(record.String(), secret) {
			t.Errorf("a credential reached the journal or its blobs: %s", secret)
		}
	}
}

func TestTheCodexRouteNeedsNoOpenRouterKey(t *testing.T) {
	t.Setenv(engine.APIKeyEnv, "")
	codexHome(t, 0o600, fakeLogin())
	dir := t.TempDir()
	sel, err := engine.ResolveSelection(dir, engine.SelectionOverrides{Provider: engine.ProviderCodex, Model: "gpt-5.6-luna"})
	if err != nil {
		t.Fatal(err)
	}
	s, err := engine.Open(t.Context(), engine.Options{Dir: dir, Selection: sel, SessionID: "nokey"})
	if err != nil {
		t.Fatalf("Open without OPENROUTER_API_KEY: %v", err)
	}
	_ = s.Close(context.Background())
}

func TestAnUnusableCodexLoginIsRefusedBeforeAnythingIsOpened(t *testing.T) {
	cases := map[string]func(t *testing.T){
		"no login": func(t *testing.T) { codexHome(t, 0o600, "") },
		"readable by others": func(t *testing.T) {
			codexHome(t, 0o644, fakeLogin())
		},
		"an API-key login, not a subscription": func(t *testing.T) {
			codexHome(t, 0o600, `{"auth_mode":"apikey","OPENAI_API_KEY":"sk-FAKE-1"}`)
		},
		"not the expected format": func(t *testing.T) { codexHome(t, 0o600, "nope") },
	}
	for name, setup := range cases {
		t.Run(name, func(t *testing.T) {
			setup(t)
			dir := t.TempDir()
			sel, err := engine.ResolveSelection(dir, engine.SelectionOverrides{Provider: engine.ProviderCodex, Model: "gpt-5.6-luna"})
			if err != nil {
				t.Fatal(err)
			}
			_, err = engine.Open(t.Context(), engine.Options{Dir: dir, Selection: sel, SessionID: "refused"})
			if !errors.Is(err, engine.ErrNoCodexLogin) {
				t.Fatalf("Open = %v, want it to wrap ErrNoCodexLogin", err)
			}
			for _, secret := range []string{codexFakeToken, codexFakeAccount, "sk-FAKE-1"} {
				if strings.Contains(err.Error(), secret) {
					t.Errorf("the error quotes the login: %v", err)
				}
			}
			// ADR-0007 decision 4: a refused Open leaves the tree as it found it.
			if entries, _ := os.ReadDir(dir); len(entries) != 0 {
				t.Errorf("a refused Open left %d entries in the working directory", len(entries))
			}
		})
	}
}
