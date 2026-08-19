//go:build integration

// This file drives the compiled kopicode binary's --resume flag itself,
// which frontends_integration_test.go's helpers (build, run, repoDir,
// sessionEnv) already exist for. It is network-free by construction: there
// is no --provider-base-url flag on the real binary (see resume_test.go's
// own doc comment on why not), so the one path testable here without a live
// endpoint is the refusal — Options.Resume set for a session id nothing on
// disk names — which fails before the provider is ever reached. The
// history-carrying success path is resume_test.go's, driven through
// session() with a fake server standing in for OpenRouter.
package main_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestResumeFlagOnTheRealBinaryRefusesANonexistentSession proves the CLI flag
// itself — argv parsing in main.go, not just the engine.Options it builds —
// reaches Open's refusal, end to end through a real process.
func TestResumeFlagOnTheRealBinaryRefusesANonexistentSession(t *testing.T) {
	bin := build(t, frontEnds[0]) // kopicode
	dir := repoDir(t)

	res := run(t, bin, dir, sessionEnv(), "--resume", "does-not-exist")

	if res.code != exitHarness {
		t.Fatalf("exit code = %d, want %d. stderr:\n%s", res.code, exitHarness, res.stderr)
	}
	for _, want := range []string{"does-not-exist", "Resume", "no prior session"} {
		if !strings.Contains(res.stderr, want) {
			t.Errorf("the refusal does not mention %q:\n%s", want, res.stderr)
		}
	}
	if res.stdout != "" {
		t.Errorf("a refused --resume wrote to stdout:\n%s", res.stdout)
	}

	// Nothing was opened, locked or written — the same property
	// TestModelSelectionOnBothFrontEnds holds an unknown model to, and for
	// the same reason: ADR-0007 decision 4's ordering applies to every
	// startup refusal, not only the one that named it first.
	if _, err := os.Stat(filepath.Join(dir, ".kopicode")); !os.IsNotExist(err) {
		t.Errorf("a refused --resume created %s", filepath.Join(dir, ".kopicode"))
	}
}
