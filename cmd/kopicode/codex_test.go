package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ADR-0035 decision 4. The Codex provider uses one person's ChatGPT login and is for
// their own development use, so the resident front ends refuse it.

func TestServeAndMCPRefuseTheProviderFlag(t *testing.T) {
	for name, cmd := range map[string]func([]string, *bytes.Buffer, *bytes.Buffer) int{
		"serve": func(a []string, o, e *bytes.Buffer) int { return serveCmd(a, o, e) },
		"mcp":   func(a []string, o, e *bytes.Buffer) int { return mcpCmd(a, o, e) },
	} {
		var stdout, stderr bytes.Buffer
		if code := cmd([]string{"--provider", "codex"}, &stdout, &stderr); code != exitUsage {
			t.Errorf("%s: exit = %d, want %d (usage); stderr: %s", name, code, exitUsage, stderr.String())
		}
		if !strings.Contains(stderr.String(), "not available on `"+name+"`") || !strings.Contains(stderr.String(), "ADR-0035") {
			t.Errorf("%s: the refusal does not say why: %s", name, stderr.String())
		}
	}
}

// A Codex run with no usable login is refused as a harness failure, before a
// session exists, and says what is wrong without quoting the file.
func TestRunPrintWithTheCodexProviderNeedsALogin(t *testing.T) {
	t.Setenv("CODEX_HOME", t.TempDir()) // no auth.json in it
	dir := t.TempDir()
	var stdout, stderr bytes.Buffer
	code := runPrint([]string{"--print", "--provider", "codex", "--model", "gpt-5.6-luna", dir, "say ok"}, &stdout, &stderr)
	if code != exitHarness {
		t.Fatalf("exit = %d, want %d; stderr: %s", code, exitHarness, stderr.String())
	}
	if !strings.Contains(stderr.String(), "no Codex login found") {
		t.Errorf("the message does not say there is no login: %s", stderr.String())
	}
	if stdout.Len() != 0 {
		t.Errorf("a session that never opened wrote to stdout: %q", stdout.String())
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Errorf("a refused run left %d entries in the working directory", len(entries))
	}
}

func TestRunPrintWithTheCodexProviderNeedsAModel(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CODEX_HOME", home)
	if err := os.WriteFile(filepath.Join(home, "auth.json"), []byte(`{"auth_mode":"chatgpt","tokens":{"access_token":"t"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	code := runPrint([]string{"--print", "--provider", "codex", t.TempDir(), "say ok"}, &stdout, &stderr)
	if code != exitUsage || !strings.Contains(stderr.String(), "needs a model") {
		t.Errorf("exit = %d, stderr: %s", code, stderr.String())
	}
}
