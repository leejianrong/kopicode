package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/leejianrong/kopicode/cmd/kopicode/lineedit"
	"github.com/leejianrong/kopicode/internal/engine"
)

// This file drives KAN-941's --fork wiring the way resume_test.go drives
// --resume's: through session()/forkSession() directly rather than the
// compiled binary, over a real HTTP server standing in for OpenRouter —
// engine.Options.ProviderBaseURL is the seam that exists for exactly this,
// and there is no equivalent flag on the real binary to point the live
// client at a fake server (resume_test.go's own doc comment argues why).
//
// Unlike resume_test.go, this file needs a real git repository: [engine.Fork]
// refuses outright outside one (internal/engine/fork.go's openRepo), because
// it has nothing to restore the tree from or attach a snapshot chain to.
// internal/engine/fork_test.go already owns proving the git-level mechanics
// (ancestry, tree restoration, the source session left untouched) in
// isolation; what this file adds is that main.go's own --fork plumbing —
// forkSession, the shared runSession... — actually reaches engine.Fork with
// the options a real --fork invocation would build, wired to a real loop.

// forkFixtureEnv and forkFixtureGit mirror internal/engine/fork_test.go's own
// helpers of the same shape: every inherited GIT_* variable dropped, then the
// two isolation pins added back, and a refusal to run against anything
// outside a temp directory. Duplicated rather than shared because the two
// files are in different packages and ADR-0003's allowlist gives this one no
// way to import the other's test helpers even if they were exported.
func forkFixtureEnv() []string {
	env := make([]string, 0, len(os.Environ())+2)
	for _, kv := range os.Environ() {
		if name, _, ok := strings.Cut(kv, "="); !ok || !strings.HasPrefix(name, "GIT_") {
			env = append(env, kv)
		}
	}
	return append(env, "GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_CONFIG_NOSYSTEM=1")
}

func forkFixtureGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	tmp := os.TempDir()
	abs, err := filepath.Abs(dir)
	if err != nil {
		t.Fatalf("resolving fixture dir %s: %v", dir, err)
	}
	if dir == "" || (!strings.HasPrefix(abs, tmp) && !strings.HasPrefix(abs, t.TempDir())) {
		t.Fatalf("git %v: %s is not under a temp directory; refusing to run a fixture command "+
			"against a directory that might be this repository's own checkout", args, dir)
	}

	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = forkFixtureEnv()
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %s in %s: %v\n%s", strings.Join(args, " "), dir, err, out)
	}
}

func newForkFixtureRepo(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "work")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	forkFixtureGit(t, dir, "init", "-q", "-b", "main")
	return dir
}

// driveIn is [drive] (resume_test.go) generalised over which opener runs the
// session, so this file can exercise [session] (via [drive] itself) and
// [forkSession] through one shared shape.
func driveIn(t *testing.T, dir, typed string, opts engine.Options, run func(streams, engine.Options) int) (string, string, int) {
	t.Helper()
	var out, errOut strings.Builder
	code := run(streams{
		in:       strings.NewReader(typed),
		out:      &out,
		err:      &errOut,
		terminal: lineedit.NonInteractive(),
	}, opts)
	return out.String(), errOut.String(), code
}

// TestForkSessionRestoresHistoryAndRunsANewTurn is this file's headline
// proof: a source session runs one turn and closes; forkSession — the exact
// function main.go's --fork calls after confirmFork — branches a new
// session from it and runs a further turn against its own server, and both
// the exit code and the new session's own journal reflect it.
func TestForkSessionRestoresHistoryAndRunsANewTurn(t *testing.T) {
	t.Setenv(engine.APIKeyEnv, "kopicode-test-credential")
	dir := newForkFixtureRepo(t)

	selection, err := engine.ResolveSelection(dir, engine.SelectionOverrides{})
	if err != nil {
		t.Fatalf("resolving the default arm: %v", err)
	}

	sourceSrv, _ := providerServer(t, "source session reply")
	sourceOut, sourceErr, sourceCode := driveIn(t, dir, "help with the source session\n", engine.Options{
		Dir: dir, Selection: selection, SessionID: "fork-source", ProviderBaseURL: sourceSrv.URL,
	}, session)
	if sourceCode != exitSuccess {
		t.Fatalf("source session exit = %d, want %d. stderr:\n%s\nstdout:\n%s",
			sourceCode, exitSuccess, sourceErr, sourceOut)
	}

	forkSrv, forkBodies := bodyCapturingServer(t, "forked session reply")
	forkOut, forkErr, forkCode := driveIn(t, dir, "try something different\n", engine.Options{
		Dir: dir, Selection: selection, SessionID: "fork-new", ProviderBaseURL: forkSrv.URL,
		Fork: &engine.ForkSource{SessionID: "fork-source", Turn: 1},
	}, forkSession)
	if forkCode != exitSuccess {
		t.Fatalf("forked session exit = %d, want %d. stderr:\n%s\nstdout:\n%s",
			forkCode, exitSuccess, forkErr, forkOut)
	}
	if len(*forkBodies) == 0 {
		t.Fatal("the forked session never called its own provider")
	}

	// The forked session's own new request carries the source's turn — the
	// same wire-level check resume_test.go makes for --resume, held here for
	// --fork's own history-copying path (engine.Fork's recordFork).
	sent := (*forkBodies)[0]
	if !strings.Contains(sent, "help with the source session") {
		t.Errorf("the forked session's first request does not carry the source's turn:\n%s", sent)
	}

	// The forked session announced its own [fork] line — render.go's own
	// rendering of journal.SessionForked — naming the source it branched
	// from.
	if !strings.Contains(forkOut, "[fork]") || !strings.Contains(forkOut, "fork-source") {
		t.Errorf("the forked session's output does not announce the fork:\n%s", forkOut)
	}

	events := readSessionEvents(t, dir, "fork-new")
	if !strings.Contains(events, `"type":"SessionForked"`) {
		t.Errorf("the forked session's journal does not carry a SessionForked marker:\n%s", events)
	}
	if !strings.Contains(events, `"source_session_id":"fork-source"`) {
		t.Errorf("the SessionForked marker does not name its source:\n%s", events)
	}

	// The source session's own record is untouched — the same property
	// internal/engine/fork_test.go proves at the engine level, checked here
	// through the front end's own two invocations.
	sourceEvents := readSessionEvents(t, dir, "fork-source")
	if strings.Contains(sourceEvents, "fork-new") {
		t.Errorf("the source session's journal was touched by the fork:\n%s", sourceEvents)
	}
}

// readSessionEvents reads one named session's journal under dir. Unlike
// session_test.go's own findJournal — which globs for "the one session this
// directory has" — this file's dir holds two sessions (the source and the
// fork), so the id has to be named explicitly.
func readSessionEvents(t *testing.T, dir, id string) string {
	t.Helper()
	path := filepath.Join(dir, ".kopicode", "sessions", id, "events.jsonl")
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	return string(b)
}
