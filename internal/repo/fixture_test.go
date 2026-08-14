package repo_test

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// These tests shell out to real git against real repositories in t.TempDir().
// A mock would only prove that this package can call a mock: every fact worth
// asserting here — that untracked files land in the tree, that a linked
// worktree's exclude file is somewhere else, that the user's index is
// byte-identical afterwards — is a fact about git.
//
// They are not behind the integration tag, deliberately. The whole file runs in
// well under two seconds, and TestSnapshotLeavesUserGitStateUntouched is the one
// test that must run in the pre-push hook rather than only in CI: it guards the
// failure that cannot be apologised for.
//
// # Why this harness is defensive
//
// A test in this package runs *inside a git repository* — kopicode's own — and
// spends its time issuing git commands. If one of them escapes its fixture it
// does not fail, it silently rewrites the repository the developer is working
// in. That happened during this card: the suite was green in a shell and, run
// from the pre-push hook, committed fixture data onto the branch, created a
// stash whose diff deleted every file in the project, registered a worktree
// under /tmp, and set core.bare=true on the real repository.
//
// The escape was *not* a missing cmd.Dir — every fixture command set one. It
// was the environment. Git exports GIT_DIR into the environment of every hook
// it runs, the pre-push hook runs `make test`, and **GIT_DIR overrides the
// process working directory**: `git -C /tmp/fixture commit` with GIT_DIR set
// commits into GIT_DIR. `git init --bare /tmp/x` likewise honours GIT_DIR over
// its path argument, which is where core.bare=true came from.
//
// A git worktree also shares .git/config, the object store and the ref store
// with its parent, so "only run git against your own worktree" protects HEAD
// and the index and nothing else.
//
// So there are four guards below, and each one turns that silent corruption
// into a failed test: the environment is built rather than inherited
// ([fixtureEnv]), the target directory must be under the temp root
// ([tryGit]), a fixture repository must resolve to a git dir under the temp
// root ([assertIsolated]), and the whole suite is bracketed by a fingerprint of
// the ambient repository ([TestMain]).

// fixtureIdentityEmail is the author every fixture commit carries. It is what
// makes leaked objects identifiable after the fact, and it is what the ambient
// fingerprint looks for.
const fixtureIdentityEmail = "fixture@example.invalid"

// gitEnvNames are the variables that redirect git at a repository, an index or
// an object store other than the one the command's working directory implies.
// A fixture must never inherit one.
var gitEnvNames = []string{
	"GIT_DIR", "GIT_WORK_TREE", "GIT_INDEX_FILE", "GIT_COMMON_DIR",
	"GIT_OBJECT_DIRECTORY", "GIT_ALTERNATE_OBJECT_DIRECTORIES", "GIT_NAMESPACE",
}

// tempRoot is the resolved directory every fixture repository must live under.
// Resolved once, through symlinks, because macOS reports /var/folders/... from
// os.TempDir() and /private/var/folders/... from git.
var tempRoot = func() string {
	resolved, err := filepath.EvalSymlinks(os.TempDir())
	if err != nil {
		return os.TempDir()
	}
	return resolved
}()

// underTempRoot reports whether path is inside the temp root, which is the only
// place a fixture is allowed to operate.
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
// The pinning matters because these tests would otherwise inherit whatever the
// developer has in ~/.gitconfig, and CI's own two jobs disagree: the `make test`
// job configures no git identity at all while the `make test-all` job sets one
// globally. A test that passes in one and fails in the other is worse than a
// test that fails, and the fixed identity in snapshot.go exists precisely so
// nothing here depends on it.
//
// The fingerprint is the backstop. The other three guards each close a specific
// hole; this one does not care how an escape happened, only that the repository
// this suite runs inside came out the way it went in. It is deliberately built
// from facts a *leak* changes and a colleague working in a parallel worktree
// does not — refs authored by the fixture identity, worktrees registered under
// /tmp, core.bare, the stash — because the ref store is shared between a dozen
// agent worktrees and a fingerprint of "all refs" would fail on someone else's
// commit.
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
					"A fixture escaped into the ambient repository — check for an inherited GIT_DIR\n"+
					"or a git command whose working directory is not under %s.\n\nbefore:\n%s\nafter:\n%s\n",
				tempRoot, before, after)
			code = 1
		}
	}
	os.Exit(code)
}

// ambientGit runs a read-only git command against whatever repository the test
// binary happens to be running inside.
//
// It is the one helper that deliberately targets the ambient repository, and it
// is separate from [tryGit] so that the temp-root guard cannot be bypassed by
// reaching for the wrong helper by accident. Nothing here writes.
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

// ambientFingerprint summarises the ambient repository in the four dimensions a
// leaking fixture moves. The second return is false when there is no ambient
// repository at all, in which case there is nothing to protect.
func ambientFingerprint() (string, bool) {
	if _, ok := ambientGit("rev-parse", "--git-dir"); !ok {
		return "", false
	}

	bare, _ := ambientGit("config", "--get", "core.bare")
	stash, _ := ambientGit("rev-parse", "--verify", "--quiet", "refs/stash")

	// Refs pointing at anything the fixture identity authored. Zero is the
	// expected count; the set rather than the count, so pre-existing debris
	// from an earlier leak does not mask a new one.
	var fixtureRefs []string
	if refs, ok := ambientGit("for-each-ref", "--format=%(refname) %(authoremail)"); ok {
		for _, line := range splitFixtureLines(refs) {
			if strings.Contains(line, fixtureIdentityEmail) {
				fixtureRefs = append(fixtureRefs, line)
			}
		}
	}

	// Worktrees registered under the temp root: a fixture's `git worktree add`
	// escaping registers against the ambient repository, which is how this
	// suite left a prunable /tmp entry behind.
	var tempWorktrees []string
	if list, ok := ambientGit("worktree", "list", "--porcelain"); ok {
		for _, line := range splitFixtureLines(list) {
			if path, found := strings.CutPrefix(line, "worktree "); found && underTempRoot(path) {
				tempWorktrees = append(tempWorktrees, path)
			}
		}
	}

	return fmt.Sprintf("core.bare=%s\nstash=%s\nfixture-refs=%v\ntemp-worktrees=%v",
		bare, stash, fixtureRefs, tempWorktrees), true
}

func splitFixtureLines(s string) []string {
	s = strings.TrimRight(s, "\n")
	if s == "" {
		return nil
	}
	return strings.Split(s, "\n")
}

// fixtureEnv is the environment every fixture git command runs in: every
// inherited GIT_* variable dropped, then the configuration pins and the fixture
// identity added back explicitly.
//
// Building it rather than inheriting it is the fix for the escape described at
// the top of this file. Nothing about the caller's environment reaches git.
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

// git runs a fixture git command and returns its trimmed stdout, failing the
// test with git's stderr if it does not succeed.
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
// Both checks are the difference between a failed test and a rewritten
// repository. An empty dir would inherit the test binary's working directory,
// which is the package directory, which is inside kopicode's own repository;
// a dir outside the temp root is a fixture that has lost track of where it
// lives. Neither has ever been legitimate here.
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
		return "", &fixtureGitError{err: err, stderr: strings.TrimSpace(stderr.String())}
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
// must be a repository under the temp root.
//
// The guards in tryGit are about the inputs; this one is about the answer. If
// some mechanism nobody thought of still redirected git, this is what notices,
// and it runs before a fixture is ever written to.
func assertIsolated(t *testing.T, dir string) {
	t.Helper()
	gitDir := git(t, dir, "rev-parse", "--absolute-git-dir")
	if !underTempRoot(gitDir) {
		t.Fatalf("the fixture at %s resolves to git dir %s, which is outside the temp root %s: "+
			"this test would have written to a real repository", dir, gitDir, tempRoot)
	}
}

type fixtureGitError struct {
	err    error
	stderr string
}

func (e *fixtureGitError) Error() string { return e.err.Error() + ": " + e.stderr }

// initRepo creates an empty repository at dir with no commits, and refuses to
// return one that is not isolated.
func initRepo(t *testing.T, dir string) string {
	t.Helper()
	if !underTempRoot(dir) {
		t.Fatalf("fixture repository %s is not under the temp root %s", dir, tempRoot)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	// -b main rather than relying on init.defaultBranch, which CI sets in one
	// job and not the other.
	git(t, dir, "init", "-q", "-b", "main")
	assertIsolated(t, dir)
	return dir
}

// newRepo creates a repository with one commit — the ordinary case.
func newRepo(t *testing.T) string {
	t.Helper()
	dir := initRepo(t, filepath.Join(t.TempDir(), "work"))
	writeFile(t, dir, "README.md", "# fixture\n")
	git(t, dir, "add", "README.md")
	git(t, dir, "commit", "-q", "-m", "initial")
	return dir
}

func writeFile(t *testing.T, dir, name, content string) {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// treePaths lists every path in a commit's tree, which is the only honest way
// to ask "is this file in the snapshot".
func treePaths(t *testing.T, dir, commit string) []string {
	t.Helper()
	out := git(t, dir, "ls-tree", "-r", "--name-only", commit)
	if out == "" {
		return nil
	}
	return strings.Split(out, "\n")
}

func contains(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}

func containsPrefix(haystack []string, prefix string) bool {
	for _, s := range haystack {
		if strings.HasPrefix(s, prefix) {
			return true
		}
	}
	return false
}

// realPath resolves symlinks so a comparison against t.TempDir() holds on
// macOS, where /var is a symlink to /private/var and git reports the resolved
// form.
func realPath(t *testing.T, path string) string {
	t.Helper()
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		t.Fatalf("resolving %s: %v", path, err)
	}
	return resolved
}

// fixedClock is the injected clock: one value, so a commit is a pure function
// of its tree, its parent and its message.
func fixedClock() func() time.Time {
	return func() time.Time { return time.Unix(1700000000, 0).UTC() }
}
