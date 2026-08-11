package repo_test

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/leejianrong/kopicode/internal/repo"
)

func TestOpenResolvesFromASubdirectory(t *testing.T) {
	dir := newRepo(t)
	sub := filepath.Join(dir, "a", "b")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}

	r, err := repo.Open(context.Background(), sub)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if got, want := realPath(t, r.Root()), realPath(t, dir); got != want {
		t.Errorf("Root = %s, want %s", got, want)
	}
	if got, want := realPath(t, r.GitDir()), realPath(t, filepath.Join(dir, ".git")); got != want {
		t.Errorf("GitDir = %s, want %s", got, want)
	}
	// --git-common-dir comes back relative to the process's directory; the
	// resolution against it is what this asserts.
	if got, want := realPath(t, r.CommonDir()), realPath(t, filepath.Join(dir, ".git")); got != want {
		t.Errorf("CommonDir = %s, want %s", got, want)
	}
	if got, want := r.StatePath(), filepath.Join(r.Root(), repo.StateDir); got != want {
		t.Errorf("StatePath = %s, want %s", got, want)
	}
}

func TestOpenOutsideARepository(t *testing.T) {
	dir := t.TempDir()
	_, err := repo.Open(context.Background(), dir)
	if !errors.Is(err, repo.ErrNotRepository) {
		t.Fatalf("Open outside a repository = %v, want ErrNotRepository", err)
	}

	// The failure carries git's own diagnosis. An error that says only "git
	// failed" turns every git problem into guesswork.
	var ge *repo.GitError
	if !errors.As(err, &ge) {
		t.Fatalf("error does not unwrap to *GitError: %v", err)
	}
	if !strings.Contains(ge.Stderr, "not a git repository") {
		t.Errorf("GitError.Stderr = %q, want git's own message", ge.Stderr)
	}
	if ge.ExitCode != 128 {
		t.Errorf("GitError.ExitCode = %d, want 128", ge.ExitCode)
	}
	if !strings.Contains(err.Error(), "not a git repository") {
		t.Errorf("the formatted error hides git's stderr: %q", err.Error())
	}
}

func TestOpenBareRepository(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "bare.git")
	git(t, t.TempDir(), "init", "-q", "--bare", dir)

	_, err := repo.Open(context.Background(), dir)
	if !errors.Is(err, repo.ErrNoWorkTree) {
		t.Fatalf("Open on a bare repository = %v, want ErrNoWorkTree", err)
	}
	// Not mistaken for "not a repository": the two exit 128 alike and only the
	// prose separates them, which is why the classification asks git a second
	// question rather than matching on wording.
	if errors.Is(err, repo.ErrNotRepository) {
		t.Errorf("a bare repository was classified as not a repository: %v", err)
	}
}

func TestOpenHonoursCancellation(t *testing.T) {
	dir := newRepo(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := repo.Open(ctx, dir); !errors.Is(err, context.Canceled) {
		t.Errorf("Open with a cancelled context = %v, want context.Canceled", err)
	}
}

// TestOpenIgnoresAnInheritedGitDir — an inherited GIT_DIR from some unrelated
// parent process (a hook, a rebase, an editor plugin) would otherwise redirect
// a whole session's snapshots at another repository, silently.
func TestOpenIgnoresAnInheritedGitDir(t *testing.T) {
	base := t.TempDir()
	ours := initRepo(t, filepath.Join(base, "ours"))
	theirs := initRepo(t, filepath.Join(base, "theirs"))

	t.Setenv("GIT_DIR", filepath.Join(theirs, ".git"))
	t.Setenv("GIT_INDEX_FILE", filepath.Join(theirs, ".git", "index"))
	t.Setenv("GIT_WORK_TREE", theirs)

	r, err := repo.Open(context.Background(), ours)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if got, want := realPath(t, r.Root()), realPath(t, ours); got != want {
		t.Errorf("Root = %s, want %s: an inherited GIT_DIR redirected the session", got, want)
	}

	// And the inherited GIT_INDEX_FILE — pointing at another repository's real
	// index — does not survive into a snapshot either.
	s, err := repo.NewSnapshotter(context.Background(), r, "session-inherited", repo.WithClock(fixedClock()))
	if err != nil {
		t.Fatalf("NewSnapshotter: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	writeFile(t, ours, "a.txt", "a\n")
	if _, err := s.Snapshot(context.Background(), 1); err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if _, err := os.Stat(filepath.Join(theirs, ".git", "index")); !os.IsNotExist(err) {
		t.Errorf("the other repository's index exists (stat err = %v); the inherited GIT_INDEX_FILE was used", err)
	}
}

func TestExcludeStateDirIsIdempotent(t *testing.T) {
	dir := newRepo(t)
	r, err := repo.Open(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}

	for range 3 {
		if err := r.ExcludeStateDir(); err != nil {
			t.Fatalf("ExcludeStateDir: %v", err)
		}
	}

	path := filepath.Join(r.CommonDir(), "info", "exclude")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(string(data), "/"+repo.StateDir+"/"); n != 1 {
		t.Errorf("the pattern appears %d times in %s:\n%s", n, path, data)
	}

	// The mechanism, not merely the file content: git must actually ignore it.
	writeFile(t, dir, repo.StateDir+"/marker", "x\n")
	if out := git(t, dir, "--no-optional-locks", "status", "--porcelain"); strings.Contains(out, repo.StateDir) {
		t.Errorf("git still reports %s as untracked:\n%s", repo.StateDir, out)
	}
}

// TestExcludeStateDirDoesNotTouchGitignore — .gitignore is a tracked file.
// Writing to it would appear in the user's status, in their next commit, and in
// a diff they did not ask for. CLAUDE.md and ADR-0002 §3 both say info/exclude.
func TestExcludeStateDirDoesNotTouchGitignore(t *testing.T) {
	dir := newRepo(t)
	writeFile(t, dir, ".gitignore", "*.log\n")
	git(t, dir, "add", ".gitignore")
	git(t, dir, "commit", "-q", "-m", "add gitignore")
	head := git(t, dir, "rev-parse", "HEAD")

	r, err := repo.Open(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.ExcludeStateDir(); err != nil {
		t.Fatal(err)
	}

	got, err := os.ReadFile(filepath.Join(dir, ".gitignore"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "*.log\n" {
		t.Errorf(".gitignore = %q, want it untouched", got)
	}
	if out := git(t, dir, "--no-optional-locks", "status", "--porcelain"); out != "" {
		t.Errorf("a tracked file changed:\n%s", out)
	}
	if now := git(t, dir, "rev-parse", "HEAD"); now != head {
		t.Errorf("HEAD moved from %s to %s", head, now)
	}
}

// TestExcludeStateDirPreservesAnExistingFile — including one whose last line
// has no newline, where a naive append would splice the comment onto the user's
// last pattern and silently change what it matches.
func TestExcludeStateDirPreservesAnExistingFile(t *testing.T) {
	dir := newRepo(t)
	r, err := repo.Open(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(r.CommonDir(), "info", "exclude")
	if err := os.WriteFile(path, []byte("# theirs\n*.tmp\nbuild"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := r.ExcludeStateDir(); err != nil {
		t.Fatal(err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	if len(lines) < 4 || lines[0] != "# theirs" || lines[1] != "*.tmp" || lines[2] != "build" {
		t.Errorf("the existing patterns did not survive intact:\n%s", data)
	}
	if lines[len(lines)-1] != "/"+repo.StateDir+"/" {
		t.Errorf("last line = %q, want the kopicode pattern:\n%s", lines[len(lines)-1], data)
	}

	// "build" is still its own pattern, not "build# kopicode…".
	writeFile(t, dir, "build/artifact", "x\n")
	if out := git(t, dir, "--no-optional-locks", "status", "--porcelain"); strings.Contains(out, "build") {
		t.Errorf("the existing `build` pattern stopped working:\n%s", out)
	}
}

// TestGitBinaryIsPresent fails loudly rather than skipping. This package is a
// git layer; without git there is nothing to test, and a skipped suite looks
// exactly like a passing one in CI output.
func TestGitBinaryIsPresent(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Fatalf("git is not on PATH: %v", err)
	}
}
