package repo_test

import (
	"bytes"
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
// well under a second, and TestSnapshotLeavesUserGitStateUntouched is the one
// test that must run in the pre-push hook rather than only in CI: it guards the
// failure that cannot be apologised for.

// TestMain pins the git configuration the fixtures and the code under test both
// see.
//
// Without it these tests inherit whatever the developer has in ~/.gitconfig,
// and CI's own two jobs disagree: the `make test` job configures no git
// identity at all while the `make test-all` job sets one globally. A test that
// passes in one and fails in the other is worse than a test that fails, and the
// point of the fixed identity in snapshot.go is precisely that it does not
// depend on this.
func TestMain(m *testing.M) {
	if err := os.Setenv("GIT_CONFIG_GLOBAL", os.DevNull); err != nil {
		panic(err)
	}
	if err := os.Setenv("GIT_CONFIG_NOSYSTEM", "1"); err != nil {
		panic(err)
	}
	os.Exit(m.Run())
}

// fixtureEnv gives the fixture's own git commands an identity, since the global
// configuration is pinned away above. It is not the identity the code under
// test uses — that one is fixed in the package and is asserted on separately.
func fixtureEnv() []string {
	return append(os.Environ(),
		"GIT_AUTHOR_NAME=Fixture",
		"GIT_AUTHOR_EMAIL=fixture@example.invalid",
		"GIT_COMMITTER_NAME=Fixture",
		"GIT_COMMITTER_EMAIL=fixture@example.invalid",
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

func tryGit(t *testing.T, dir string, args ...string) (string, error) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = fixtureEnv()
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", &fixtureGitError{err: err, stderr: strings.TrimSpace(stderr.String())}
	}
	return strings.TrimRight(stdout.String(), "\n"), nil
}

type fixtureGitError struct {
	err    error
	stderr string
}

func (e *fixtureGitError) Error() string { return e.err.Error() + ": " + e.stderr }

// initRepo creates an empty repository at dir with no commits.
func initRepo(t *testing.T, dir string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	// -b main rather than relying on init.defaultBranch, which CI sets in one
	// job and not the other.
	git(t, dir, "init", "-q", "-b", "main")
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
