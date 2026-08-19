package repo_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/leejianrong/kopicode/internal/repo"
)

// newSnapshotForRestore is the setup every restore test needs: a repository,
// a snapshotter over it, and cleanup registered. It mirrors newSnapshotter in
// snapshot_test.go; kept separate so this file reads standalone.
func newSnapshotForRestore(t *testing.T, dir, session string) (*repo.Repo, *repo.Snapshotter) {
	t.Helper()
	ctx := context.Background()
	r, err := repo.Open(ctx, dir)
	if err != nil {
		t.Fatalf("Open(%s): %v", dir, err)
	}
	s, err := repo.NewSnapshotter(ctx, r, session, repo.WithClock(fixedClock()))
	if err != nil {
		t.Fatalf("NewSnapshotter(%s): %v", session, err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	return r, s
}

// TestRestoreMaterializesTreeIntoAFreshDirectory is the primitive's ordinary
// case: a tree a caller only has the id of, read back out to a scratch
// directory for inspection.
func TestRestoreMaterializesTreeIntoAFreshDirectory(t *testing.T) {
	dir := newRepo(t)
	writeFile(t, dir, "brand-new.go", "package main\n")
	writeFile(t, dir, "nested/deep/also-new.txt", "new\n")

	r, s := newSnapshotForRestore(t, dir, "session-restore-fresh")
	snap, err := s.Snapshot(context.Background(), 1)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}

	dest := filepath.Join(t.TempDir(), "restored")
	if err := r.Restore(context.Background(), snap.Tree, dest); err != nil {
		t.Fatalf("Restore: %v", err)
	}

	for name, want := range map[string]string{
		"README.md":                "# fixture\n",
		"brand-new.go":             "package main\n",
		"nested/deep/also-new.txt": "new\n",
	} {
		got, err := os.ReadFile(filepath.Join(dest, name))
		if err != nil {
			t.Fatalf("reading restored %s: %v", name, err)
		}
		if string(got) != want {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
	}
}

// TestRestoreOverwritesMatchingPathsButLeavesExtrasAlone is the documented
// semantics: a path the tree contains is overwritten, a path in dest the
// tree does not mention is left exactly as it was. Restore never deletes.
func TestRestoreOverwritesMatchingPathsButLeavesExtrasAlone(t *testing.T) {
	dir := newRepo(t)
	r, s := newSnapshotForRestore(t, dir, "session-restore-overwrite")
	snap, err := s.Snapshot(context.Background(), 1)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}

	dest := t.TempDir()
	writeFile(t, dest, "README.md", "will be overwritten\n")
	writeFile(t, dest, "extra.txt", "not part of the tree\n")

	if err := r.Restore(context.Background(), snap.Tree, dest); err != nil {
		t.Fatalf("Restore: %v", err)
	}

	if got, err := os.ReadFile(filepath.Join(dest, "README.md")); err != nil || string(got) != "# fixture\n" {
		t.Errorf("README.md = %q, %v, want the tree's content", got, err)
	}
	if got, err := os.ReadFile(filepath.Join(dest, "extra.txt")); err != nil || string(got) != "not part of the tree\n" {
		t.Errorf("extra.txt = %q, %v; Restore must not delete a path the tree does not mention", got, err)
	}
}

// TestRestoreIntoTheLiveWorkingTreeRevertsFilesWithoutTouchingGitState is the
// other legitimate destination: the repository's own working tree. The
// promise this package exists to keep still has to hold when Restore, not
// Snapshot, is the thing running.
func TestRestoreIntoTheLiveWorkingTreeRevertsFilesWithoutTouchingGitState(t *testing.T) {
	dir := newRepo(t)
	r, s := newSnapshotForRestore(t, dir, "session-restore-live")
	ctx := context.Background()

	writeFile(t, dir, "work.txt", "turn one\n")
	first, err := s.Snapshot(ctx, 1)
	if err != nil {
		t.Fatalf("Snapshot(1): %v", err)
	}

	writeFile(t, dir, "work.txt", "turn two\n")
	writeFile(t, dir, "new-in-turn-two.txt", "only in turn two\n")
	if _, err := s.Snapshot(ctx, 2); err != nil {
		t.Fatalf("Snapshot(2): %v", err)
	}

	before := captureUserGitState(t, dir)

	if err := r.Restore(ctx, first.Tree, dir); err != nil {
		t.Fatalf("Restore into the working tree: %v", err)
	}

	after := captureUserGitState(t, dir)
	if diff := cmp.Diff(before, after); diff != "" {
		t.Errorf("restoring into the working tree changed the user's git state (-before +after):\n%s", diff)
	}

	if got, err := os.ReadFile(filepath.Join(dir, "work.txt")); err != nil || string(got) != "turn one\n" {
		t.Errorf("work.txt = %q, %v, want turn one's content restored", got, err)
	}
	// Restore never deletes: turn two's extra file is not part of turn one's
	// tree, but it must still be there.
	if _, err := os.Stat(filepath.Join(dir, "new-in-turn-two.txt")); err != nil {
		t.Errorf("new-in-turn-two.txt disappeared, but Restore is documented to never delete: %v", err)
	}
}

// TestRestoreOfAnEarlierTurnBringsBackADeletedFile exercises the exact
// motivating case: a file the model deleted in a later turn reappears when an
// earlier turn's tree is read back out.
func TestRestoreOfAnEarlierTurnBringsBackADeletedFile(t *testing.T) {
	dir := newRepo(t)
	r, s := newSnapshotForRestore(t, dir, "session-restore-undelete")
	ctx := context.Background()

	first, err := s.Snapshot(ctx, 1)
	if err != nil {
		t.Fatalf("Snapshot(1): %v", err)
	}
	if !contains(treePaths(t, dir, first.Commit), "README.md") {
		t.Fatal("README.md is not in the first snapshot; this test proves nothing")
	}

	if err := os.Remove(filepath.Join(dir, "README.md")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Snapshot(ctx, 2); err != nil {
		t.Fatalf("Snapshot(2): %v", err)
	}

	dest := t.TempDir()
	if err := r.Restore(ctx, first.Tree, dest); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dest, "README.md")); err != nil {
		t.Errorf("README.md missing from the restored earlier turn: %v", err)
	}
}

// TestRestoreRefusesDestInsideGitDir is the one destination Restore refuses
// on its own: writing a tree on top of git's own bookkeeping is never what
// either legitimate use of Restore means.
func TestRestoreRefusesDestInsideGitDir(t *testing.T) {
	dir := newRepo(t)
	r, s := newSnapshotForRestore(t, dir, "session-restore-gitdir")
	writeFile(t, dir, "a.txt", "a\n")
	snap, err := s.Snapshot(context.Background(), 1)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}

	for _, dest := range []string{r.GitDir(), filepath.Join(r.GitDir(), "objects"), r.CommonDir()} {
		if err := r.Restore(context.Background(), snap.Tree, dest); !errors.Is(err, repo.ErrRestoreDestInsideGitDir) {
			t.Errorf("Restore(dest=%s) = %v, want ErrRestoreDestInsideGitDir", dest, err)
		}
	}
}

// TestRestoreRefusesANonDirectoryDest — a destination that already exists as
// a plain file is not something Restore can extract a tree into.
func TestRestoreRefusesANonDirectoryDest(t *testing.T) {
	dir := newRepo(t)
	r, s := newSnapshotForRestore(t, dir, "session-restore-notdir")
	writeFile(t, dir, "a.txt", "a\n")
	snap, err := s.Snapshot(context.Background(), 1)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}

	dest := filepath.Join(t.TempDir(), "file.txt")
	if err := os.WriteFile(dest, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := r.Restore(context.Background(), snap.Tree, dest); err == nil {
		t.Error("Restore into a destination that is a regular file succeeded")
	}
}

// TestRestoreCreatesTheDestinationIfItDoesNotExist — the scratch-inspection
// case does not require the caller to have created the directory first.
func TestRestoreCreatesTheDestinationIfItDoesNotExist(t *testing.T) {
	dir := newRepo(t)
	r, s := newSnapshotForRestore(t, dir, "session-restore-mkdir")
	writeFile(t, dir, "a.txt", "a\n")
	snap, err := s.Snapshot(context.Background(), 1)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}

	dest := filepath.Join(t.TempDir(), "does", "not", "exist", "yet")
	if err := r.Restore(context.Background(), snap.Tree, dest); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if got, err := os.ReadFile(filepath.Join(dest, "a.txt")); err != nil || string(got) != "a\n" {
		t.Errorf("a.txt = %q, %v", got, err)
	}
}

// TestRestoreHonoursCancellation — the engine cancels a turn on Ctrl-C, and
// Restore has to respect that the same way Snapshot does.
func TestRestoreHonoursCancellation(t *testing.T) {
	dir := newRepo(t)
	r, s := newSnapshotForRestore(t, dir, "session-restore-cancel")
	writeFile(t, dir, "a.txt", "a\n")
	snap, err := s.Snapshot(context.Background(), 1)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := r.Restore(ctx, snap.Tree, t.TempDir()); !errors.Is(err, context.Canceled) {
		t.Errorf("Restore with a cancelled context = %v, want context.Canceled", err)
	}
}

// TestRestoreRejectsAnEmptyTree — a caller passing an unset field is a bug to
// surface immediately, not a call that shells out to git and lets it fail.
func TestRestoreRejectsAnEmptyTree(t *testing.T) {
	dir := newRepo(t)
	r, err := repo.Open(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, tree := range []string{"", "   "} {
		if err := r.Restore(context.Background(), tree, t.TempDir()); err == nil {
			t.Errorf("Restore(tree=%q) succeeded, want an error", tree)
		}
	}
}

// TestRestoreRejectsATreeThatDoesNotResolve — git's own diagnosis should
// surface rather than being swallowed.
func TestRestoreRejectsATreeThatDoesNotResolve(t *testing.T) {
	dir := newRepo(t)
	r, err := repo.Open(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	err = r.Restore(context.Background(), strings.Repeat("0", 40), t.TempDir())
	if err == nil {
		t.Fatal("Restore with a nonexistent tree object succeeded")
	}
	var ge *repo.GitError
	if !errors.As(err, &ge) {
		t.Errorf("error does not unwrap to *GitError: %v", err)
	}
}

// TestRestorePreservesSymlinksTrackedInTheTree — a symlink is a legitimate
// git object, and Restore's own escape guard must not catch an ordinary one.
func TestRestorePreservesSymlinksTrackedInTheTree(t *testing.T) {
	dir := newRepo(t)
	if err := os.WriteFile(filepath.Join(dir, "target.txt"), []byte("target content\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("target.txt", filepath.Join(dir, "link.txt")); err != nil {
		t.Skipf("symlinks are not supported here: %v", err)
	}
	git(t, dir, "add", "target.txt", "link.txt")
	git(t, dir, "commit", "-q", "-m", "add a tracked symlink")

	r, s := newSnapshotForRestore(t, dir, "session-restore-symlink")
	snap, err := s.Snapshot(context.Background(), 1)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}

	dest := t.TempDir()
	if err := r.Restore(context.Background(), snap.Tree, dest); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	got, err := os.Readlink(filepath.Join(dest, "link.txt"))
	if err != nil {
		t.Fatalf("reading restored symlink: %v", err)
	}
	if got != "target.txt" {
		t.Errorf("link.txt -> %q, want target.txt", got)
	}
}
