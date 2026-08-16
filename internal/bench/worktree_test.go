package bench_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/leejianrong/kopicode/internal/bench"
)

// newTrees returns a manager over the fixture repository, with its base
// directory inside the fixture's own state directory.
func newTrees(t *testing.T, f *fixture, keep bool) *bench.Worktrees {
	t.Helper()
	return bench.NewWorktrees(f.Root, filepath.Join(f.Root, ".kopicode"), keep)
}

// TestWorktreeAddThenReleaseLeavesNothing is the base case, and it asserts the
// two halves separately: the checkout is gone from disk, and the registration is
// gone from the repository. A `git worktree remove` that left the administrative
// entry behind would pass a directory check and still accumulate.
func TestWorktreeAddThenReleaseLeavesNothing(t *testing.T) {
	f := newFixture(t)
	trees := newTrees(t, f, false)

	path, err := trees.Add(t.Context(), "task-01", f.Commit)
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	if !contains(worktreePaths(t, f.Root), path) {
		t.Fatalf("the worktree at %s is not registered:\n%v", path, worktreePaths(t, f.Root))
	}
	if _, err := os.Stat(filepath.Join(path, "bench", "tasks", "corpus.json")); err != nil {
		t.Fatalf("the worktree does not hold the corpus at the frozen commit: %v", err)
	}

	if err := trees.Release(t.Context(), path); err != nil {
		t.Fatalf("Release: %v", err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the worktree directory is still on disk: %v", err)
	}
	if contains(worktreePaths(t, f.Root), path) {
		t.Errorf("the worktree is still registered:\n%v", worktreePaths(t, f.Root))
	}

	got := trees.Counts()
	if got.Created != 1 || got.Removed != 1 || got.Kept != 0 || len(got.Failed) != 0 {
		t.Errorf("counts = %+v, want created 1, removed 1, kept 0, none failed", got)
	}
}

// TestWorktreeAddCreatesNoBranch holds the --detach half.
//
// `git worktree add` without --detach creates a branch named after the
// directory, in the ref store the whole repository shares. Ten tasks per arm and
// many arms would leave that many branches in the user's repository — the ref
// half of the accumulation this card is about, and a write to git state that is
// not kopicode's to make.
func TestWorktreeAddCreatesNoBranch(t *testing.T) {
	f := newFixture(t)
	before := git(t, f.Root, "for-each-ref", "--format=%(refname)")

	trees := newTrees(t, f, false)
	path, err := trees.Add(t.Context(), "task-01", f.Commit)
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	t.Cleanup(func() { _ = trees.Release(context.Background(), path) })

	if after := git(t, f.Root, "for-each-ref", "--format=%(refname)"); after != before {
		t.Errorf("creating a worktree changed the ref store:\nbefore:\n%s\nafter:\n%s", before, after)
	}
	if head := git(t, path, "rev-parse", "HEAD"); head != f.Commit {
		t.Errorf("the worktree is at %s, want the frozen commit %s", head, f.Commit)
	}
}

// TestKeepWorktreesKeepsAndCounts is the --keep-worktrees flag: the checkout
// stays, and the report says so. A kept worktree that was not counted is
// indistinguishable from a removed one, which is the sentence the card is built
// around.
func TestKeepWorktreesKeepsAndCounts(t *testing.T) {
	f := newFixture(t)
	trees := newTrees(t, f, true)

	path, err := trees.Add(t.Context(), "task-01", f.Commit)
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	if err := trees.Release(t.Context(), path); err != nil {
		t.Fatalf("Release: %v", err)
	}

	if _, err := os.Stat(path); err != nil {
		t.Errorf("--keep-worktrees removed the worktree anyway: %v", err)
	}
	got := trees.Counts()
	if got.Kept != 1 || got.Removed != 0 {
		t.Errorf("counts = %+v, want kept 1 and removed 0", got)
	}
}

// TestReclaimTakesBackACrashedRunsWorktrees is the run-start prune.
//
// A crash leaves both forms of orphan, and the two are reclaimed by different
// mechanisms: a checkout still on disk needs removing, and a registration whose
// directory has already gone needs pruning. Both are set up here, and both
// counts are asserted, because a Reclaim that only did one of them would look
// identical in a report that printed a single number.
func TestReclaimTakesBackACrashedRunsWorktrees(t *testing.T) {
	f := newFixture(t)
	crashed := newTrees(t, f, true) // keep: this stands in for a run that died

	stillOnDisk, err := crashed.Add(t.Context(), "task-01", f.Commit)
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	gone, err := crashed.Add(t.Context(), "task-02", f.Commit)
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	// The second orphan is the one whose directory a user, a CI cleanup or a
	// reboot removed without telling git.
	if err := os.RemoveAll(gone); err != nil {
		t.Fatal(err)
	}

	next := newTrees(t, f, false)
	if err := next.Reclaim(t.Context()); err != nil {
		t.Fatalf("Reclaim: %v", err)
	}

	got := next.Counts()
	if got.PrunedStale != 1 {
		t.Errorf("PrunedStale = %d, want 1 (the checkout left on disk)", got.PrunedStale)
	}
	if got.PrunedAdmin != 1 {
		t.Errorf("PrunedAdmin = %d, want 1 (the registration whose directory had gone)", got.PrunedAdmin)
	}
	if _, err := os.Stat(stillOnDisk); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the orphaned checkout is still on disk: %v", err)
	}
	if paths := worktreePaths(t, f.Root); len(paths) != 1 {
		t.Errorf("worktrees after reclaiming = %v, want only the main one", paths)
	}
}

// TestReclaimLeavesWorktreesItDoesNotOwn is the containment half, and it is the
// one that matters most.
//
// Reclamation removes checkouts under its own base directory and nothing else.
// A developer's worktree, another agent's lease, or a sibling directory whose
// name merely starts with the same characters must all survive — the failure
// this guards against is a benchmark run deleting the checkout somebody is
// working in, which is not recoverable by apologising.
func TestReclaimLeavesWorktreesItDoesNotOwn(t *testing.T) {
	f := newFixture(t)

	// One outside the base directory entirely, and one whose path is a string
	// prefix of the base but a different directory.
	elsewhere := filepath.Join(t.TempDir(), "someone-elses-work")
	git(t, f.Root, "worktree", "add", "--detach", "--quiet", elsewhere, f.Commit)

	base := filepath.Join(f.Root, ".kopicode", "bench", "worktrees")
	lookalike := base + "-old"
	git(t, f.Root, "worktree", "add", "--detach", "--quiet", lookalike, f.Commit)

	trees := newTrees(t, f, false)
	if err := trees.Reclaim(t.Context()); err != nil {
		t.Fatalf("Reclaim: %v", err)
	}

	for _, path := range []string{elsewhere, lookalike} {
		if _, err := os.Stat(path); err != nil {
			t.Errorf("reclamation removed %s, which this package does not own: %v", path, err)
		}
		if !contains(worktreePaths(t, f.Root), path) {
			t.Errorf("reclamation deregistered %s, which this package does not own", path)
		}
	}
	if got := trees.Counts(); got.PrunedStale != 0 {
		t.Errorf("PrunedStale = %d, want 0: nothing under the base directory existed", got.PrunedStale)
	}

	// Cleanup by hand, since the manager is deliberately unable to.
	git(t, f.Root, "worktree", "remove", "--force", elsewhere)
	git(t, f.Root, "worktree", "remove", "--force", lookalike)
}

// TestReleaseSurvivesAWorktreeGitCannotRemove holds the fallback.
//
// `git worktree remove` refuses a checkout it cannot fully delete. The disk is
// still this run's to reclaim, so the directory is removed directly and the
// registration pruned. Reported as failed only if both fail.
func TestReleaseSurvivesAWorktreeGitCannotRemove(t *testing.T) {
	f := newFixture(t)
	trees := newTrees(t, f, false)

	path, err := trees.Add(t.Context(), "task-01", f.Commit)
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	// A worktree git refuses to remove: locking is git's own documented way of
	// saying "not this one", and it is what a killed run can leave behind.
	git(t, f.Root, "worktree", "lock", path)

	if err := trees.Release(t.Context(), path); err != nil {
		t.Fatalf("Release: %v", err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the locked worktree is still on disk: %v", err)
	}
	if got := trees.Counts(); got.Removed != 1 || len(got.Failed) != 0 {
		t.Errorf("counts = %+v, want removed 1 and none failed", got)
	}
}

// TestReleaseReclaimsAfterCancellation is the cancellation clause, isolated.
//
// A cleanup that inherited the run's cancelled context would fail at its first
// subprocess and leave the checkout behind — an accumulation arriving through
// the very mechanism meant to prevent it. Release detaches, so a context that is
// already dead reclaims exactly as a live one does.
func TestReleaseReclaimsAfterCancellation(t *testing.T) {
	f := newFixture(t)
	trees := newTrees(t, f, false)

	path, err := trees.Add(t.Context(), "task-01", f.Commit)
	if err != nil {
		t.Fatalf("Add: %v", err)
	}

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := trees.Release(ctx, path); err != nil {
		t.Fatalf("Release with a cancelled context: %v", err)
	}

	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a cancelled run left its worktree on disk: %v", err)
	}
	if contains(worktreePaths(t, f.Root), path) {
		t.Errorf("a cancelled run left its worktree registered")
	}
}

// TestGitCommandsNameTheirDirectory is the negative case for the rule the whole
// package rests on. runGit refuses an empty working directory rather than
// letting os/exec fall back to the process's own, which for kopibench is
// wherever the user happened to be and for a test binary is inside kopicode's
// own repository.
func TestWorktreeManagerRefusesARepoWithNoWorkingDirectory(t *testing.T) {
	trees := bench.NewWorktrees("", t.TempDir(), false)
	err := trees.Reclaim(t.Context())
	if !errors.Is(err, bench.ErrNoWorkingDir) {
		t.Fatalf("Reclaim with no repository: %v, want ErrNoWorkingDir", err)
	}
	if !strings.Contains(err.Error(), "no working directory") {
		t.Errorf("the message does not say what is wrong: %v", err)
	}
}

func contains(list []string, want string) bool {
	for _, s := range list {
		if s == want || resolved(s) == resolved(want) {
			return true
		}
	}
	return false
}

func resolved(path string) string {
	if p, err := filepath.EvalSymlinks(path); err == nil {
		return p
	}
	return path
}
