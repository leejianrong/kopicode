package repo_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/leejianrong/kopicode/internal/repo"
)

// userGitState is everything a user would lose if this package got the
// isolation wrong: what they have staged, what they have stashed, where their
// branch is, and the index file itself, byte for byte.
//
// The working tree is deliberately not in here. The model is *supposed* to
// change files — that is the whole job — so a test that pinned `git status`
// would fail on its own fixture the moment a turn created something. What the
// model is not supposed to change is which of those files git considers staged,
// and that is what StagedDiff and the index capture.
type userGitState struct {
	Branch     string
	HeadCommit string
	StashList  string
	StashRef   string
	StagedDiff string
	// IndexEntries is the index in a form a failure message can be read in;
	// IndexBytes is the assertion.
	IndexEntries string
	IndexBytes   []byte
	IndexMode    os.FileMode
	IndexMtime   time.Time
}

// captureUserGitState reads the four things that are off limits.
//
// Every command here is either read-only or runs under --no-optional-locks,
// which is git's own switch for "report, but do not refresh and rewrite the
// index as a side effect". Without it, `git status` would rewrite the index
// while measuring it and the before/after comparison would be measuring the
// measurement.
func captureUserGitState(t *testing.T, dir string) userGitState {
	t.Helper()

	indexPath := filepath.Join(git(t, dir, "rev-parse", "--absolute-git-dir"), "index")
	info, err := os.Stat(indexPath)
	if err != nil {
		t.Fatalf("stat %s: %v", indexPath, err)
	}
	data, err := os.ReadFile(indexPath)
	if err != nil {
		t.Fatalf("reading %s: %v", indexPath, err)
	}

	stashRef, err := tryGit(t, dir, "rev-parse", "refs/stash")
	if err != nil {
		stashRef = "(no stash)"
	}

	return userGitState{
		Branch:       git(t, dir, "symbolic-ref", "HEAD"),
		HeadCommit:   git(t, dir, "rev-parse", "HEAD"),
		StashList:    git(t, dir, "stash", "list"),
		StashRef:     stashRef,
		StagedDiff:   git(t, dir, "--no-optional-locks", "diff", "--cached", "--name-status"),
		IndexEntries: git(t, dir, "--no-optional-locks", "ls-files", "--stage"),
		IndexBytes:   data,
		IndexMode:    info.Mode(),
		IndexMtime:   info.ModTime(),
	}
}

// TestSnapshotLeavesUserGitStateUntouched is the test this card exists to pass.
//
// The repository is set up the way a real one is when an agent is turned loose
// on it: a branch, a commit, something stashed, something staged, something
// modified but not staged, and something untracked. Two snapshots run over it.
// Afterwards the branch, HEAD, the stash and the index must be exactly what
// they were — the index compared as bytes, not as a summary, because a
// rewritten index with the same visible effect is still a rewritten index and
// the next thing that goes wrong will be harder to see.
//
// Proven red: removing the GIT_INDEX_FILE override in Snapshotter.env is caught
// by verifyIndexIsolated and every snapshot fails; removing the guard as well
// lets `git add -A` stage into the real index, and this test then reports a
// changed IndexBytes, a changed StagedDiff and a changed Status. Both failures
// are in the pull request.
func TestSnapshotLeavesUserGitStateUntouched(t *testing.T) {
	dir := newRepo(t)

	// Something stashed.
	writeFile(t, dir, "README.md", "# fixture, stashed edit\n")
	git(t, dir, "stash", "push", "-q", "-m", "work in progress")

	// Something staged, something modified but not staged, something
	// untracked. All three are states the user cares about and none of them
	// belongs to kopicode.
	writeFile(t, dir, "staged.txt", "staged content\n")
	git(t, dir, "add", "staged.txt")
	writeFile(t, dir, "README.md", "# fixture, unstaged edit\n")
	writeFile(t, dir, "untracked.txt", "untracked content\n")

	before := captureUserGitState(t, dir)

	ctx := context.Background()
	r, err := repo.Open(ctx, dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	s, err := repo.NewSnapshotter(ctx, r, "session-guard", repo.WithClock(fixedClock()))
	if err != nil {
		t.Fatalf("NewSnapshotter: %v", err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})

	first, err := s.Snapshot(ctx, 1)
	if err != nil {
		t.Fatalf("Snapshot(1): %v", err)
	}
	// A second turn, because a chain is where a naive implementation reaches
	// for HEAD or for `git commit` and takes the user's state with it.
	writeFile(t, dir, "model-made.txt", "written by the model\n")
	if _, err := s.Snapshot(ctx, 2); err != nil {
		t.Fatalf("Snapshot(2): %v", err)
	}

	after := captureUserGitState(t, dir)

	if diff := cmp.Diff(before, after); diff != "" {
		t.Errorf("the user's git state changed across two snapshots (-before +after):\n%s", diff)
	}

	// Nothing was checked out over the user's files either. A snapshot that
	// took a detour through `git stash` or `git checkout` would keep every
	// assertion above and still have overwritten what the user was editing.
	for name, want := range map[string]string{
		"README.md":     "# fixture, unstaged edit\n",
		"staged.txt":    "staged content\n",
		"untracked.txt": "untracked content\n",
	} {
		got, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatalf("reading %s after the snapshots: %v", name, err)
		}
		if string(got) != want {
			t.Errorf("%s = %q after the snapshots, want %q", name, got, want)
		}
	}

	// The snapshot must still have been real: a package that keeps the promise
	// by doing nothing keeps it for the wrong reason.
	if !contains(treePaths(t, dir, first.Commit), "untracked.txt") {
		t.Error("the snapshot did not capture untracked.txt, so the guard above proved nothing")
	}
}

// TestSnapshotDoesNotCreateAnIndexInARepoThatHasNone is the same promise in the
// case where "unchanged" is not enough to state it: a repository with no
// commits has no index file at all, and the strongest assertion available is
// that the snapshot did not bring one into existence.
func TestSnapshotDoesNotCreateAnIndexInARepoThatHasNone(t *testing.T) {
	dir := initRepo(t, filepath.Join(t.TempDir(), "work"))
	writeFile(t, dir, "new.txt", "created by the model\n")

	ctx := context.Background()
	r, err := repo.Open(ctx, dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	s, err := repo.NewSnapshotter(ctx, r, "session-no-index", repo.WithClock(fixedClock()))
	if err != nil {
		t.Fatalf("NewSnapshotter: %v", err)
	}
	if _, err := s.Snapshot(ctx, 1); err != nil {
		t.Fatalf("Snapshot: %v", err)
	}

	realIndex := filepath.Join(r.GitDir(), "index")
	if _, err := os.Stat(realIndex); !os.IsNotExist(err) {
		t.Errorf("the snapshot created the repository's real index at %s (stat err = %v); "+
			"GIT_INDEX_FILE was not in effect", realIndex, err)
	}
	if _, err := os.Stat(s.IndexPath()); err != nil {
		t.Errorf("the throwaway index at %s does not exist, so the staging went somewhere else: %v",
			s.IndexPath(), err)
	}
}

// TestSnapshotWritesOnlyUnderRefPrefix checks the other half of "off limits":
// no ref outside refs/kopicode/ moved, and no new one appeared.
func TestSnapshotWritesOnlyUnderRefPrefix(t *testing.T) {
	dir := newRepo(t)
	git(t, dir, "branch", "feature")
	git(t, dir, "tag", "v1")

	before := git(t, dir, "for-each-ref", "--format=%(objectname) %(refname)")

	ctx := context.Background()
	r, err := repo.Open(ctx, dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	s, err := repo.NewSnapshotter(ctx, r, "session-refs", repo.WithClock(fixedClock()))
	if err != nil {
		t.Fatalf("NewSnapshotter: %v", err)
	}
	writeFile(t, dir, "a.txt", "a\n")
	if _, err := s.Snapshot(ctx, 1); err != nil {
		t.Fatalf("Snapshot: %v", err)
	}

	after := git(t, dir, "for-each-ref", "--format=%(objectname) %(refname)",
		"refs/heads/", "refs/tags/", "refs/remotes/", "refs/stash")
	if before != after {
		t.Errorf("refs outside %s changed:\nbefore:\n%s\nafter:\n%s", repo.RefPrefix, before, after)
	}

	shadow := git(t, dir, "for-each-ref", "--format=%(refname)", repo.RefPrefix)
	if shadow != "refs/kopicode/session-refs/1" {
		t.Errorf("shadow refs = %q, want refs/kopicode/session-refs/1", shadow)
	}
}
