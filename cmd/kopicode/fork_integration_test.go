//go:build integration

// This file drives KAN-941's argv-level surface through the compiled
// kopicode binary — build, run and repoDir come from
// frontends_integration_test.go, in this same package. It is network-free by
// construction, the same way resume_integration_test.go's own doc comment
// argues: every case here is refused, or declined, before a provider is ever
// reached, so there is no need for a fake server standing in for OpenRouter.
package main_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestSessionsCommandOnAnUnusedDirectory holds `kopicode sessions` to the
// same "not a failure" treatment engine.ListSessions itself documents for a
// directory that has never run a session.
func TestSessionsCommandOnAnUnusedDirectory(t *testing.T) {
	bin := build(t, frontEnds[0])
	dir := repoDir(t)

	res := run(t, bin, dir, nil, "sessions")
	if res.code != 0 {
		t.Fatalf("exit code = %d, want 0. stderr:\n%s", res.code, res.stderr)
	}
	if !strings.Contains(res.stdout, "no sessions recorded") {
		t.Errorf("stdout does not say there are no sessions:\n%s", res.stdout)
	}

	jsonRes := run(t, bin, dir, nil, "sessions", "--json")
	if jsonRes.code != 0 {
		t.Fatalf("`sessions --json` exit code = %d, want 0. stderr:\n%s", jsonRes.code, jsonRes.stderr)
	}
	if strings.TrimSpace(jsonRes.stdout) != "" {
		t.Errorf("`sessions --json` on an unused directory wrote a line: %q", jsonRes.stdout)
	}
}

// TestForkFlagRefusedOnTheHeadlessSurface holds print.go's own decision:
// `run --print` refuses --fork outright rather than wiring it up, because
// forking wipes the working tree and there is nobody present on a headless
// invocation to type the confirmation the REPL demands.
func TestForkFlagRefusedOnTheHeadlessSurface(t *testing.T) {
	bin := build(t, frontEnds[0])
	dir := repoDir(t)
	before := treeOf(t, dir)

	res := run(t, bin, dir, nil, "run", "--print", "--fork", "some-session:1", "fix it")
	if res.code != exitUsage {
		t.Fatalf("exit code = %d, want %d (usage). stderr:\n%s", res.code, exitUsage, res.stderr)
	}
	for _, want := range []string{"not supported", "repl --fork"} {
		if !strings.Contains(res.stderr, want) {
			t.Errorf("the refusal does not mention %q:\n%s", want, res.stderr)
		}
	}
	if res.stdout != "" {
		t.Errorf("a refused --fork wrote to stdout, which --print owns:\n%s", res.stdout)
	}
	if !equal(before, treeOf(t, dir)) {
		t.Errorf("a refused --fork changed the working tree.\nbefore: %v\nafter:  %v",
			before, treeOf(t, dir))
	}
}

// TestForkAndResumeAreMutuallyExclusiveOnTheRealBinary is main.go's own
// early refusal, checked through argv rather than only through
// engine.Fork's Options.Resume/Options.Fork guard (internal/engine/fork_test.go's
// TestForkAndResumeAreMutuallyExclusive) — the CLI has to reach that
// refusal itself and not rely on the engine to have the last word.
func TestForkAndResumeAreMutuallyExclusiveOnTheRealBinary(t *testing.T) {
	bin := build(t, frontEnds[0])
	dir := repoDir(t)
	before := treeOf(t, dir)

	res := run(t, bin, dir, nil, "--resume", "some-id", "--fork", "other-id:1")
	if res.code != exitUsage {
		t.Fatalf("exit code = %d, want %d (usage). stderr:\n%s", res.code, exitUsage, res.stderr)
	}
	if !strings.Contains(res.stderr, "mutually exclusive") {
		t.Errorf("the refusal does not say the flags are mutually exclusive:\n%s", res.stderr)
	}
	if !equal(before, treeOf(t, dir)) {
		t.Errorf("a refused invocation changed the working tree.\nbefore: %v\nafter:  %v",
			before, treeOf(t, dir))
	}
}

// TestForkFlagMalformedArgumentIsAUsageErrorOnTheRealBinary holds
// parseForkSource's refusal to account through argv, before anything is
// opened.
func TestForkFlagMalformedArgumentIsAUsageErrorOnTheRealBinary(t *testing.T) {
	bin := build(t, frontEnds[0])
	dir := repoDir(t)

	res := run(t, bin, dir, nil, "--fork", "no-turn-here")
	if res.code != exitUsage {
		t.Fatalf("exit code = %d, want %d (usage). stderr:\n%s", res.code, exitUsage, res.stderr)
	}
	if !strings.Contains(res.stderr, "<session-id>:<turn>") {
		t.Errorf("the refusal does not explain the expected shape:\n%s", res.stderr)
	}
	if _, err := os.Stat(filepath.Join(dir, ".kopicode")); !os.IsNotExist(err) {
		t.Errorf("a malformed --fork created %s", filepath.Join(dir, ".kopicode"))
	}
}

// TestForkFlagWithoutConfirmationLeavesTheTreeUntouched drives the REPL's
// --fork with no stdin to answer the confirmation from — the real binary's
// stdin is the null device here, so repl.Confirm's ReadLine hits EOF
// immediately and confirmFork declines before engine.Fork, or anything
// else, is ever called. This is the property the whole confirmation exists
// for: an unaskable question is never answered yes by default.
func TestForkFlagWithoutConfirmationLeavesTheTreeUntouched(t *testing.T) {
	bin := build(t, frontEnds[0])
	dir := repoDir(t)
	before := treeOf(t, dir)

	res := run(t, bin, dir, nil, "--fork", "some-session:1")
	if res.code != exitUsage {
		t.Fatalf("exit code = %d, want %d (usage). stderr:\n%s", res.code, exitUsage, res.stderr)
	}
	if !strings.Contains(res.stderr, "fork cancelled") {
		t.Errorf("the refusal does not say the fork was cancelled:\n%s", res.stderr)
	}
	if !equal(before, treeOf(t, dir)) {
		t.Errorf("a cancelled --fork changed the working tree.\nbefore: %v\nafter:  %v",
			before, treeOf(t, dir))
	}
	if _, err := os.Stat(filepath.Join(dir, ".kopicode")); !os.IsNotExist(err) {
		t.Errorf("a cancelled --fork created %s", filepath.Join(dir, ".kopicode"))
	}
}
