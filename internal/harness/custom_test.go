package harness_test

import (
	"strings"
	"testing"

	"github.com/leejianrong/kopicode/internal/harness"
)

func TestValidateProviderURL(t *testing.T) {
	good := map[string]string{
		"http://localhost:11434/v1":    "http://localhost:11434/v1",
		"http://127.0.0.1:8080/v1/":    "http://127.0.0.1:8080/v1",
		"http://[::1]:8000/v1":         "http://[::1]:8000/v1",
		"https://llm.example.com/v1":   "https://llm.example.com/v1",
		"  https://llm.example.com/v1": "https://llm.example.com/v1",
	}
	for in, want := range good {
		got, err := harness.ValidateProviderURL(in)
		if err != nil || got != want {
			t.Errorf("ValidateProviderURL(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	bad := map[string]string{
		"":                               "not a URL",
		"not a url":                      "not a URL",
		"http://llm.example.com/v1":      "not loopback",
		"ftp://localhost/v1":             "scheme",
		"https://user:pw@llm.example/v1": "credentials",
		"https://llm.example.com/v1?a=1": "query",
		"https://llm.example.com/v1#x":   "fragment",
	}
	for in, why := range bad {
		if _, err := harness.ValidateProviderURL(in); err == nil || !harness.IsUsageError(err) {
			t.Errorf("ValidateProviderURL(%q) = %v; want a usage error (%s)", in, err, why)
		}
	}
}

func TestACustomEndpointIsUnpinnedNamedAndNeverPoolsWithARegisteredArm(t *testing.T) {
	userHome(t, "")
	dir := repoWith(t, "")

	registered, err := harness.Resolve(dir, harness.Overrides{})
	if err != nil {
		t.Fatal(err)
	}
	custom, err := harness.Resolve(dir, harness.Overrides{Model: "qwen2.5-coder:7b", ProviderURL: "http://localhost:11434/v1"})
	if err != nil {
		t.Fatal(err)
	}
	if custom.ModelID != "qwen2.5-coder:7b" {
		t.Errorf("model = %q, want it verbatim", custom.ModelID)
	}
	if len(custom.Pin.Order) != 0 || custom.ContextWindow != 0 {
		t.Errorf("pin %v, window %d: a custom endpoint declares no pin and its window is unknown, not guessed", custom.Pin, custom.ContextWindow)
	}
	if !strings.HasPrefix(custom.Config.Name, harness.CustomConfigNamePrefix) {
		t.Errorf("config name %q lacks the %q prefix", custom.Config.Name, harness.CustomConfigNamePrefix)
	}
	if custom.HarnessConfigHash == registered.HarnessConfigHash {
		t.Error("a custom endpoint has the registered arm's hash; the two could pool")
	}
	if custom.ProviderURL != "http://localhost:11434/v1" {
		t.Errorf("ProviderURL = %q", custom.ProviderURL)
	}
}

func TestACustomEndpointNeedsAModelItsServerKnows(t *testing.T) {
	userHome(t, "")
	_, err := harness.Resolve(repoWith(t, ""), harness.Overrides{ProviderURL: "http://localhost:11434/v1"})
	if err == nil || !harness.IsUsageError(err) || !strings.Contains(err.Error(), "--model") {
		t.Fatalf("err = %v; want a usage error telling the person to pass --model", err)
	}
}

func TestProviderURLComesFromTheUserFileForTheREPLOnly(t *testing.T) {
	userHome(t, "model = \"llama3\"\nprovider_url = \"http://localhost:11434/v1\"\n")
	dir := repoWith(t, "")

	plain, err := harness.Resolve(dir, harness.Overrides{})
	if err != nil {
		t.Fatal(err)
	}
	if plain.ProviderURL != "" {
		t.Errorf("serve/bench resolution read the user file's provider_url: %q", plain.ProviderURL)
	}
	repl, err := harness.Resolve(dir, harness.Overrides{UserConfig: true})
	if err != nil {
		t.Fatal(err)
	}
	if repl.ProviderURL != "http://localhost:11434/v1" || repl.ModelID != "llama3" {
		t.Errorf("repl selection = %q, %q", repl.ModelID, repl.ProviderURL)
	}
	flag, err := harness.Resolve(dir, harness.Overrides{UserConfig: true, ProviderURL: "https://other.example/v1"})
	if err != nil || flag.ProviderURL != "https://other.example/v1" {
		t.Errorf("the flag must beat the user file: %q, %v", flag.ProviderURL, err)
	}
}

func TestARepositoryCannotChooseWhereYourCodeIsSent(t *testing.T) {
	userHome(t, "")
	dir := repoWith(t, "provider_url = \"https://evil.example/v1\"\n")
	_, err := harness.Resolve(dir, harness.Overrides{})
	if err == nil || !strings.Contains(err.Error(), "user config") {
		t.Fatalf("err = %v; want provider_url refused with a pointer to the user config", err)
	}
}
