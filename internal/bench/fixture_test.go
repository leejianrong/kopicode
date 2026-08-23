package bench_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/leejianrong/kopicode/internal/corpus"
)

// These tests shell out to real git against real repositories in t.TempDir(),
// and the thing under test *is* git worktrees. A mock would prove only that
// this package can call a mock: every fact worth asserting — that a worktree is
// registered in the shared ref store, that removing one deregisters it, that a
// prune reclaims a crashed run's checkout — is a fact about git.
//
// They are not behind the integration tag, deliberately, and the whole file
// runs in a couple of seconds. The reason is internal/repo's: the guards
// against a fixture escaping into the developer's own repository have to run in
// the pre-push hook, which runs `make test` and not `make test-all`. The one
// test that is tagged is the full-corpus smoke run, which is slow because it
// compiles ten small projects.
//
// # Why this harness is defensive
//
// A test in this package runs inside a git repository — kopicode's own — and
// spends its time creating and destroying git worktrees. If one of them escapes
// its fixture it does not fail, it registers a worktree against, or deletes a
// worktree from, the repository the developer is working in. That has happened
// in this project once already (see .claude/skills/agent-ground-rules/SKILL.md),
// and a worktree shares .git/config, the object store and the ref store with its
// parent, so "we are in a worktree" protects nothing.
//
// Four guards, each turning a silent corruption into a failed test: the
// environment is built rather than inherited ([fixtureEnv]), a fixture command's
// target directory must be under the temp root ([tryGit]), a fixture repository
// must resolve to a git dir under the temp root ([assertIsolated]), and the whole
// suite is bracketed by a fingerprint of the ambient repository ([TestMain]).

// fixtureIdentityEmail is the author every fixture commit carries. It is what
// makes leaked objects identifiable after the fact, and what the ambient
// fingerprint looks for.
const fixtureIdentityEmail = "bench-fixture@example.invalid"

// gitEnvNames are the variables that redirect git at a repository, an index or
// an object store other than the one the command's working directory implies. A
// fixture must never inherit one.
var gitEnvNames = []string{
	"GIT_DIR", "GIT_WORK_TREE", "GIT_INDEX_FILE", "GIT_COMMON_DIR",
	"GIT_OBJECT_DIRECTORY", "GIT_ALTERNATE_OBJECT_DIRECTORIES", "GIT_NAMESPACE",
}

// tempRoot is the resolved directory every fixture repository must live under.
// Resolved through symlinks, because macOS reports /var/folders/... from
// os.TempDir() and /private/var/folders/... from git.
var tempRoot = func() string {
	resolved, err := filepath.EvalSymlinks(os.TempDir())
	if err != nil {
		return os.TempDir()
	}
	return resolved
}()

func underTempRoot(path string) bool {
	abs, err := filepath.Abs(path)
	if err != nil {
		return false
	}
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		abs = resolved
	}
	rel, err := filepath.Rel(tempRoot, abs)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// TestMain pins the git configuration the fixtures and the code under test both
// see, and brackets the run with a fingerprint of the ambient repository.
//
// The fingerprint is built from facts a leak changes and a colleague working in
// a parallel worktree does not: core.bare, the stash, refs authored by the
// fixture identity, and — the one this card most needs — the set of registered
// worktrees. A `git worktree add` that escaped, or a reclamation that reached a
// worktree it does not own, moves that last line and fails the suite.
func TestMain(m *testing.M) {
	if err := os.Setenv("GIT_CONFIG_GLOBAL", os.DevNull); err != nil {
		panic(err)
	}
	if err := os.Setenv("GIT_CONFIG_NOSYSTEM", "1"); err != nil {
		panic(err)
	}

	before, ok := ambientFingerprint()
	code := m.Run()

	if ok {
		if after, stillOK := ambientFingerprint(); stillOK && after != before {
			fmt.Fprintf(os.Stderr,
				"\nFATAL: this test run modified the git repository it is running inside.\n"+
					"A fixture escaped into the ambient repository — check for an inherited GIT_DIR,\n"+
					"a git command whose working directory is not under %s, or a reclamation that\n"+
					"reached a worktree this package does not own.\n\nbefore:\n%s\nafter:\n%s\n",
				tempRoot, before, after)
			code = 1
		}
	}
	os.Exit(code)
}

// ambientGit runs a read-only git command against whatever repository the test
// binary happens to be running inside. It is separate from [tryGit] so the
// temp-root guard cannot be bypassed by reaching for the wrong helper.
func ambientGit(args ...string) (string, bool) {
	//kopicode:allow-nodir: the ambient repository is the subject here, so the package
	// directory is the correct target. Read-only by construction — every caller passes a
	// query. The Env rule still applies and is honoured below.
	cmd := exec.Command("git", args...)
	cmd.Env = fixtureEnv()
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	if err := cmd.Run(); err != nil {
		return "", false
	}
	return strings.TrimRight(stdout.String(), "\n"), true
}

// ambientFingerprint summarises the ambient repository in the dimensions a
// leaking fixture moves. The second return is false when there is no ambient
// repository at all, in which case there is nothing to protect.
func ambientFingerprint() (string, bool) {
	if _, ok := ambientGit("rev-parse", "--git-dir"); !ok {
		return "", false
	}

	bare, _ := ambientGit("config", "--get", "core.bare")
	stash, _ := ambientGit("rev-parse", "--verify", "--quiet", "refs/stash")

	var fixtureRefs []string
	if refs, ok := ambientGit("for-each-ref", "--format=%(refname) %(authoremail)"); ok {
		for _, line := range splitLines(refs) {
			if strings.Contains(line, fixtureIdentityEmail) {
				fixtureRefs = append(fixtureRefs, line)
			}
		}
	}

	// Every registered worktree, not only the ones under /tmp. This package
	// creates and destroys worktrees for a living, so both directions matter:
	// one that appeared is a fixture that escaped, and one that vanished is a
	// reclamation that reached past its own base directory.
	var worktrees []string
	if list, ok := ambientGit("worktree", "list", "--porcelain"); ok {
		for _, line := range splitLines(list) {
			if path, found := strings.CutPrefix(line, "worktree "); found {
				worktrees = append(worktrees, path)
			}
		}
	}

	return fmt.Sprintf("core.bare=%s\nstash=%s\nfixture-refs=%v\nworktrees=%v",
		bare, stash, fixtureRefs, worktrees), true
}

func splitLines(s string) []string {
	s = strings.TrimRight(s, "\n")
	if s == "" {
		return nil
	}
	return strings.Split(s, "\n")
}

// fixtureEnv is the environment every fixture git command runs in: every
// inherited GIT_* variable dropped, then the configuration pins and the fixture
// identity added back explicitly.
func fixtureEnv() []string {
	env := make([]string, 0, len(os.Environ())+8)
	for _, kv := range os.Environ() {
		if name, _, ok := strings.Cut(kv, "="); !ok || !strings.HasPrefix(name, "GIT_") {
			env = append(env, kv)
		}
	}
	return append(env,
		"GIT_CONFIG_GLOBAL="+os.DevNull,
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_AUTHOR_NAME=Fixture",
		"GIT_AUTHOR_EMAIL="+fixtureIdentityEmail,
		"GIT_COMMITTER_NAME=Fixture",
		"GIT_COMMITTER_EMAIL="+fixtureIdentityEmail,
		"GIT_AUTHOR_DATE=1700000000 +0000",
		"GIT_COMMITTER_DATE=1700000000 +0000",
	)
}

// git runs a fixture git command, failing the test with git's stderr.
func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := tryGit(t, dir, args...)
	if err != nil {
		t.Fatalf("git %s in %s: %v", strings.Join(args, " "), dir, err)
	}
	return out
}

// tryGit runs a fixture git command, refusing outright to run one that could
// reach outside the temp root.
//
// An empty dir would inherit the test binary's working directory, which is the
// package directory, which is inside kopicode's own repository; a dir outside
// the temp root is a fixture that has lost track of where it lives. Neither has
// ever been legitimate here.
func tryGit(t *testing.T, dir string, args ...string) (string, error) {
	t.Helper()

	if dir == "" {
		t.Fatalf("git %s: no working directory, so it would run in the package directory — "+
			"inside the repository under test's own repository", strings.Join(args, " "))
	}
	if !underTempRoot(dir) {
		t.Fatalf("git %s: working directory %s is outside the temp root %s; a fixture may only "+
			"touch repositories it created", strings.Join(args, " "), dir, tempRoot)
	}

	env := fixtureEnv()
	for _, name := range gitEnvNames {
		if value, found := lookupEnv(env, name); found {
			t.Fatalf("git %s: %s=%q reached the fixture environment; it overrides the working "+
				"directory and would redirect this command at another repository",
				strings.Join(args, " "), name, value)
		}
	}

	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = env
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("%w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return strings.TrimRight(stdout.String(), "\n"), nil
}

func lookupEnv(env []string, name string) (string, bool) {
	value, found := "", false
	for _, kv := range env {
		if k, v, ok := strings.Cut(kv, "="); ok && k == name {
			value, found = v, true
		}
	}
	return value, found
}

// assertIsolated is the positive check: whatever git actually resolves from dir
// must be a repository under the temp root. The guards in tryGit are about the
// inputs; this one is about the answer, and it runs before a fixture is written
// to.
func assertIsolated(t *testing.T, dir string) {
	t.Helper()
	gitDir := git(t, dir, "rev-parse", "--absolute-git-dir")
	if !underTempRoot(gitDir) {
		t.Fatalf("the fixture at %s resolves to git dir %s, which is outside the temp root %s: "+
			"this test would have written to a real repository", dir, gitDir, tempRoot)
	}
}

// worktreePaths lists the worktrees a fixture repository has registered, which
// is the fact almost every test in this package is about.
func worktreePaths(t *testing.T, repoDir string) []string {
	t.Helper()
	var out []string
	for _, line := range splitLines(git(t, repoDir, "worktree", "list", "--porcelain")) {
		if path, ok := strings.CutPrefix(line, "worktree "); ok {
			out = append(out, path)
		}
	}
	return out
}

// fixture is a git repository under t.TempDir() holding a synthetic corpus at
// bench/tasks, committed.
type fixture struct {
	// Root is the repository root.
	Root string
	// CorpusDir is the corpus inside it.
	CorpusDir string
	// Commit is the commit the corpus was frozen at.
	Commit string
	// TaskCount is the number of tasks the corpus at CorpusDir actually holds.
	// The synthetic fixture always has exactly corpus.MinTasks; a copy of the
	// real corpus does not — the corpus has grown past that floor as the
	// hardening batch (KAN-990..994) lands tasks, and a test comparing against
	// corpus.MinTasks itself rather than this field would be asserting a
	// coincidence, not a contract. Zero for callers that never set it.
	TaskCount int
}

// taskSpec describes one synthetic task.
type taskSpec struct {
	// ID is the task id and directory name.
	ID string
	// Oracle is the body of the task's oracle.py, run by `python3 oracle.py`
	// in the task's repo directory.
	Oracle string
	// TimeoutSeconds bounds it. Zero means 30.
	TimeoutSeconds int
	// Traits are the corpus traits it declares.
	Traits []string
}

// oraclePassesWhenFixed is the default synthetic oracle: it fails on the
// starting tree and passes once a file named "fixed" exists, which is what an
// agent in these tests writes. That is the corpus's own contract — an oracle
// that already passes measures nothing — reduced to something that runs in
// milliseconds and needs no toolchain beyond python3.
const oraclePassesWhenFixed = `import os
import sys

sys.exit(0 if os.path.exists("fixed") else 1)
`

// oracleSleeps outlives any sane timeout, for the timeout test.
const oracleSleeps = `import time

time.sleep(120)
`

// newFixture builds a repository with a synthetic corpus of [corpus.MinTasks]
// tasks and commits it.
//
// A synthetic corpus rather than the real one at bench/tasks: the real oracles
// compile ten small Go projects, which belongs in the tagged smoke test and not
// in a suite the pre-push hook runs. These oracles are a two-line python
// script, so a Runner test measures the runner rather than a toolchain.
//
// extra replaces the generated spec for a task of the same id, which is how a
// test asks for a slow oracle or a task that cannot pass.
func newFixture(t *testing.T, extra ...taskSpec) *fixture {
	t.Helper()
	requirePython(t)

	root := filepath.Join(t.TempDir(), "repo")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	// -b main rather than relying on init.defaultBranch, which CI sets in one
	// job and not the other.
	git(t, root, "init", "-q", "-b", "main")
	assertIsolated(t, root)

	specs := defaultSpecs()
	for _, e := range extra {
		replaced := false
		for i := range specs {
			if specs[i].ID == e.ID {
				// The generated traits are kept unless the caller sets its
				// own: the corpus loader enforces composition floors over the
				// whole corpus, and a test replacing one task's oracle should
				// not have to know that.
				if e.Traits == nil {
					e.Traits = specs[i].Traits
				}
				specs[i] = e
				replaced = true
			}
		}
		if !replaced {
			t.Fatalf("taskSpec %q does not replace a generated task; ids are task-01..task-%02d",
				e.ID, corpus.MinTasks)
		}
	}

	corpusDir := filepath.Join(root, "bench", "tasks")
	ids := make([]string, 0, len(specs))
	for _, s := range specs {
		writeTask(t, corpusDir, s)
		ids = append(ids, s.ID)
	}
	writeCorpusManifest(t, corpusDir, ids)

	// Loading here rather than in every test: a fixture that does not satisfy
	// the corpus loader is a broken fixture, and finding that out inside the
	// runner would blame the runner.
	if _, err := corpus.Load(corpusDir); err != nil {
		t.Fatalf("the synthetic corpus is not valid: %v", err)
	}

	git(t, root, "add", "-A")
	git(t, root, "commit", "-q", "-m", "corpus")
	commit := git(t, root, "rev-parse", "HEAD")

	return &fixture{Root: root, CorpusDir: corpusDir, Commit: commit}
}

func defaultSpecs() []taskSpec {
	specs := make([]taskSpec, 0, corpus.MinTasks)
	for i := 1; i <= corpus.MinTasks; i++ {
		s := taskSpec{ID: fmt.Sprintf("task-%02d", i), Oracle: oraclePassesWhenFixed}
		switch i {
		case 1, 2:
			s.Traits = []string{corpus.TraitRequiresRead}
		case 3:
			s.Traits = []string{corpus.TraitMultiFile}
		}
		specs = append(specs, s)
	}
	return specs
}

func writeTask(t *testing.T, corpusDir string, s taskSpec) {
	t.Helper()
	dir := filepath.Join(corpusDir, s.ID)
	repoDir := filepath.Join(dir, corpus.RepoDirName)
	if err := os.MkdirAll(repoDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(repoDir, "oracle.py"), s.Oracle)

	timeout := s.TimeoutSeconds
	if timeout == 0 {
		timeout = 30
	}
	manifest := map[string]any{
		"schema_version": corpus.SchemaVersion,
		"id":             s.ID,
		"title":          "synthetic " + s.ID,
		"statement":      "Make the oracle for " + s.ID + " pass.",
		"language":       "python",
		"requires":       []string{"python3"},
		"oracle": map[string]any{
			"argv":            []string{"python3", "oracle.py"},
			"env":             map[string]string{"PYTHONDONTWRITEBYTECODE": "1"},
			"timeout_seconds": timeout,
		},
		"traits":    s.Traits,
		"max_turns": 4,
		"notes":     "Synthetic fixture task for the bench runner's own tests.",
	}
	writeJSON(t, filepath.Join(dir, corpus.TaskManifestName), manifest)
}

func writeCorpusManifest(t *testing.T, corpusDir string, ids []string) {
	t.Helper()
	// The digest excludes corpus.json itself, so it can be computed over the
	// tasks and then written into the manifest that records it.
	digest, err := corpus.Digest(corpusDir)
	if err != nil {
		t.Fatalf("digesting the synthetic corpus: %v", err)
	}
	writeJSON(t, filepath.Join(corpusDir, corpus.ManifestName), map[string]any{
		"schema_version": corpus.SchemaVersion,
		"corpus_version": "fixture-1.0.0",
		"digest":         digest,
		"description":    "Synthetic corpus for the bench runner's own tests.",
		"tasks":          ids,
	})
}

func writeJSON(t *testing.T, path string, v any) {
	t.Helper()
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, path, string(b)+"\n")
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// requirePython skips rather than fails when python3 is absent, and says so
// loudly: a missing toolchain is an environment fact, but a suite that silently
// skips its subject is worse than one that fails.
func requirePython(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skipf("python3 is not on PATH, so the synthetic corpus's oracles cannot run here")
	}
}
