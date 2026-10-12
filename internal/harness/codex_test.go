package harness_test

import (
	"strings"
	"testing"

	"github.com/leejianrong/kopicode/internal/harness"
)

// ADR-0035. The Codex provider is a route nobody pinned: it names no pin, reads no
// registry row, and is its own arm in the hash. Only a person's own user file or
// flag may choose it.

func TestCodexSelectionIsUnpinnedAndNamedInTheHash(t *testing.T) {
	userHome(t, "")
	dir := repoWith(t, "")
	sel, err := harness.Resolve(dir, harness.Overrides{Provider: "codex", Model: "gpt-5.6-luna"})
	if err != nil {
		t.Fatal(err)
	}
	if sel.Provider != harness.ProviderCodex || sel.ProviderURL != "" {
		t.Errorf("provider %q url %q", sel.Provider, sel.ProviderURL)
	}
	if len(sel.Pin.Order) != 0 || sel.ContextWindow != 0 {
		t.Errorf("a subscription route has no pin or known context window: %+v window %d", sel.Pin, sel.ContextWindow)
	}
	if !strings.HasPrefix(sel.Config.Name, harness.CodexConfigNamePrefix) {
		t.Errorf("config name %q lacks the %q prefix", sel.Config.Name, harness.CodexConfigNamePrefix)
	}
	if sel.ModelID != "gpt-5.6-luna" {
		t.Errorf("model = %q: any id the backend serves is passed through", sel.ModelID)
	}

	// Its hash is its own: it can never pool with the registered arm or with a
	// custom endpoint.
	reg, err := harness.Resolve(dir, harness.Overrides{})
	if err != nil {
		t.Fatal(err)
	}
	custom, err := harness.Resolve(dir, harness.Overrides{Model: "m", ProviderURL: "http://localhost:1/v1"})
	if err != nil {
		t.Fatal(err)
	}
	if sel.HarnessConfigHash == reg.HarnessConfigHash || sel.HarnessConfigHash == custom.HarnessConfigHash {
		t.Error("a Codex run shares a hash with another arm")
	}
	if sel.HarnessConfigHash != sel.Config.Hash() {
		t.Error("the selection's hash is not its configuration's hash")
	}
}

func TestCodexNeedsAModelAndAKnownProviderName(t *testing.T) {
	userHome(t, "")
	dir := repoWith(t, "")
	if _, err := harness.Resolve(dir, harness.Overrides{Provider: "codex"}); err == nil || !strings.Contains(err.Error(), "needs a model") {
		t.Errorf("no model: err = %v, want a refusal that asks for --model", err)
	}
	if _, err := harness.Resolve(dir, harness.Overrides{Provider: "gemini", Model: "m"}); err == nil || !strings.Contains(err.Error(), `unknown provider "gemini"`) {
		t.Errorf("unknown provider: err = %v", err)
	}
}

func TestCodexAndAProviderURLAreTwoRoutes(t *testing.T) {
	userHome(t, "")
	dir := repoWith(t, "")
	_, err := harness.Resolve(dir, harness.Overrides{Provider: "codex", ProviderURL: "http://localhost:1/v1", Model: "m"})
	if err == nil || !strings.Contains(err.Error(), "two different routes") {
		t.Errorf("err = %v, want a refusal naming both", err)
	}
}

func TestOnlyTheUserFileMayChooseTheProvider(t *testing.T) {
	// A repository cannot choose whose account its visitors' prompts go to.
	userHome(t, "")
	dir := repoWith(t, "provider = \"codex\"\n")
	_, err := harness.Resolve(dir, harness.Overrides{})
	if err == nil || !strings.Contains(err.Error(), "belongs in the user config") {
		t.Fatalf("a repository's provider key: err = %v", err)
	}

	// The user file may, and only a front end that reads it sees it.
	userHome(t, "provider = \"codex\"\nmodel = \"gpt-5.6-luna\"\n")
	dir = repoWith(t, "")
	sel, err := harness.Resolve(dir, harness.Overrides{UserConfig: true})
	if err != nil {
		t.Fatal(err)
	}
	if sel.Provider != harness.ProviderCodex {
		t.Errorf("the user file's provider was not read: %q", sel.Provider)
	}
	headless, err := harness.Resolve(dir, harness.Overrides{})
	if err != nil {
		t.Fatal(err)
	}
	if headless.Provider != "" || strings.HasPrefix(headless.Config.Name, harness.CodexConfigNamePrefix) {
		t.Errorf("a front end that does not read the user file got provider %q (%s)", headless.Provider, headless.Config.Name)
	}
}

func TestTheFlagBeatsTheUserFilesProvider(t *testing.T) {
	userHome(t, "provider = \"codex\"\nmodel = \"gpt-5.6-luna\"\n")
	dir := repoWith(t, "")
	// An unknown name on the flag is refused even though the file is valid, so the
	// flag was the rung read.
	if _, err := harness.Resolve(dir, harness.Overrides{UserConfig: true, Provider: "other"}); err == nil {
		t.Error("the flag's value was ignored in favour of the user file's")
	}
}

func TestCodexRunsGetTheSameInteractiveDefaultsAsACustomEndpoint(t *testing.T) {
	userHome(t, "")
	dir := repoWith(t, "")
	sel, err := harness.Resolve(dir, harness.Overrides{Provider: "codex", Model: "gpt-5.6-luna", Interactive: true})
	if err != nil {
		t.Fatal(err)
	}
	if sel.Config.MaxTurns != harness.InteractiveMaxTurns || sel.Config.StallThreshold != harness.DefaultStallThreshold ||
		sel.Config.OutputCap != harness.DefaultOutputCap {
		t.Errorf("interactive defaults not applied: %+v", sel.Config)
	}
}
