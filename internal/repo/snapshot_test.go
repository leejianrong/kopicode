package repo_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/leejianrong/kopicode/internal/repo"
)

// newSnapshotter is the two-line setup every snapshot test needs.
func newSnapshotter(t *testing.T, dir, session string) (*repo.Repo, *repo.Snapshotter) {
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

// TestSnapshotCapturesUntrackedFiles is the other half of the card. The model's
// output is mostly new files; `git add -u` would stage none of them and the
// snapshot would describe a tree that never existed.
func TestSnapshotCapturesUntrackedFiles(t *testing.T) {
	dir := newRepo(t)
	writeFile(t, dir, "brand-new.go", "package main\n")
	writeFile(t, dir, "nested/deep/also-new.txt", "new\n")
	writeFile(t, dir, "README.md", "# modified\n")

	_, s := newSnapshotter(t, dir, "session-untracked")
	snap, err := s.Snapshot(context.Background(), 1)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}

	paths := treePaths(t, dir, snap.Commit)
	for _, want := range []string{"brand-new.go", "nested/deep/also-new.txt", "README.md"} {
		if !contains(paths, want) {
			t.Errorf("snapshot tree is missing %s; it has %v", want, paths)
		}
	}

	// And the content, not merely the name: a snapshot that recorded the
	// committed version of a modified file would pass a name check.
	if got := git(t, dir, "show", snap.Commit+":README.md"); got != "# modified" {
		t.Errorf("README.md in the snapshot = %q, want the modified working-tree content", got)
	}
}

// TestSnapshotDeletionsAreCaptured — the model deletes files too, and -A is
// what makes a deletion part of the tree rather than a silent no-op.
func TestSnapshotDeletionsAreCaptured(t *testing.T) {
	dir := newRepo(t)
	if err := os.Remove(filepath.Join(dir, "README.md")); err != nil {
		t.Fatal(err)
	}
	writeFile(t, dir, "replacement.md", "gone\n")

	_, s := newSnapshotter(t, dir, "session-delete")
	snap, err := s.Snapshot(context.Background(), 1)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if paths := treePaths(t, dir, snap.Commit); contains(paths, "README.md") {
		t.Errorf("the deleted README.md is still in the snapshot tree: %v", paths)
	}
}

// TestSnapshotExcludesTheThrowawayIndex is not hypothetical bookkeeping: with
// no exclude in place, `git add -A` stages kopicode's own index — and its
// transient .lock file — into the tree being snapshotted. That was reproduced
// against git 2.34 before the exclude was written.
func TestSnapshotExcludesTheThrowawayIndex(t *testing.T) {
	dir := newRepo(t)
	writeFile(t, dir, "work.txt", "work\n")

	_, s := newSnapshotter(t, dir, "session-exclude")
	snap, err := s.Snapshot(context.Background(), 1)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}

	paths := treePaths(t, dir, snap.Commit)
	if containsPrefix(paths, repo.StateDir+"/") {
		t.Errorf("kopicode's own state directory is inside the snapshot tree: %v", paths)
	}
	// The index really is under StateDir, so the assertion above is testing
	// the exclude rather than an accident of where the file happens to live.
	if !strings.HasPrefix(s.IndexPath(), filepath.Join(dir, repo.StateDir)+string(filepath.Separator)) {
		t.Errorf("throwaway index %s is not under %s", s.IndexPath(), filepath.Join(dir, repo.StateDir))
	}
}

// TestSnapshotChainsParents — the chain is what makes a sequence of snapshots a
// history rather than a pile of unrelated trees.
func TestSnapshotChainsParents(t *testing.T) {
	dir := newRepo(t)
	_, s := newSnapshotter(t, dir, "session-chain")
	ctx := context.Background()

	writeFile(t, dir, "one.txt", "1\n")
	first, err := s.Snapshot(ctx, 1)
	if err != nil {
		t.Fatalf("Snapshot(1): %v", err)
	}
	if first.Parent != "" {
		t.Errorf("first snapshot Parent = %q, want empty: the chain is kopicode's and does not root on the user's HEAD", first.Parent)
	}

	writeFile(t, dir, "two.txt", "2\n")
	second, err := s.Snapshot(ctx, 2)
	if err != nil {
		t.Fatalf("Snapshot(2): %v", err)
	}
	if second.Parent != first.Commit {
		t.Errorf("second snapshot Parent = %s, want the first commit %s", second.Parent, first.Commit)
	}

	// And git agrees, which the returned struct alone would not prove.
	if got := git(t, dir, "rev-parse", second.Commit+"^"); got != first.Commit {
		t.Errorf("git says %s's parent is %s, want %s", second.Commit, got, first.Commit)
	}
	if got := git(t, dir, "rev-parse", second.Ref); got != second.Commit {
		t.Errorf("%s points at %s, want %s", second.Ref, got, second.Commit)
	}
	if got := git(t, dir, "rev-parse", second.Commit+"^{tree}"); got != second.Tree {
		t.Errorf("commit tree = %s, want the reported %s", got, second.Tree)
	}
}

// TestSnapshotInRepoWithNoCommits — a repository whose HEAD is unborn. kopicode
// may well be the first thing to touch a freshly `git init`ed directory, and a
// snapshot that assumed a parent commit existed would fail at exactly that
// moment.
func TestSnapshotInRepoWithNoCommits(t *testing.T) {
	dir := initRepo(t, filepath.Join(t.TempDir(), "work"))
	writeFile(t, dir, "first.txt", "first\n")

	_, s := newSnapshotter(t, dir, "session-unborn")
	snap, err := s.Snapshot(context.Background(), 1)
	if err != nil {
		t.Fatalf("Snapshot in a repository with no commits: %v", err)
	}
	if snap.Parent != "" {
		t.Errorf("Parent = %q, want empty", snap.Parent)
	}
	if !contains(treePaths(t, dir, snap.Commit), "first.txt") {
		t.Error("first.txt is not in the snapshot")
	}
	// HEAD is still unborn: the snapshot did not helpfully create a commit on
	// the user's branch.
	if _, err := tryGit(t, dir, "rev-parse", "HEAD"); err == nil {
		t.Error("HEAD resolves after the snapshot, so a commit landed on the user's branch")
	}
}

// TestSnapshotInLinkedWorktree — a repository whose .git is a file, not a
// directory. The bench runner creates one linked worktree per task, so this is
// a first-class case; it is also where the exclude file moves, since only
// $GIT_COMMON_DIR/info/exclude is read from a linked worktree and a file
// written to the per-worktree git directory is silently ignored.
func TestSnapshotInLinkedWorktree(t *testing.T) {
	base := t.TempDir()
	main := initRepo(t, filepath.Join(base, "main"))
	writeFile(t, main, "README.md", "# main\n")
	git(t, main, "add", "README.md")
	git(t, main, "commit", "-q", "-m", "initial")

	linked := filepath.Join(base, "linked")
	git(t, main, "worktree", "add", "-q", linked, "-b", "task")

	// The premise: .git here is a file.
	info, err := os.Stat(filepath.Join(linked, ".git"))
	if err != nil {
		t.Fatal(err)
	}
	if info.IsDir() {
		t.Fatalf(".git in the linked worktree is a directory; this test is not testing what it claims")
	}

	r, s := newSnapshotter(t, linked, "session-worktree")

	if got, want := realPath(t, r.Root()), realPath(t, linked); got != want {
		t.Errorf("Root = %s, want %s", got, want)
	}
	if realPath(t, r.GitDir()) == realPath(t, r.CommonDir()) {
		t.Errorf("GitDir and CommonDir are both %s; in a linked worktree they differ", r.GitDir())
	}
	if got, want := realPath(t, r.CommonDir()), realPath(t, filepath.Join(main, ".git")); got != want {
		t.Errorf("CommonDir = %s, want the main repository's %s", got, want)
	}

	// The exclude has to be in the common directory to have any effect here.
	if _, err := os.Stat(filepath.Join(r.CommonDir(), "info", "exclude")); err != nil {
		t.Errorf("no exclude file in the common directory: %v", err)
	}

	writeFile(t, linked, "task.txt", "task output\n")
	snap, err := s.Snapshot(context.Background(), 1)
	if err != nil {
		t.Fatalf("Snapshot in a linked worktree: %v", err)
	}
	paths := treePaths(t, linked, snap.Commit)
	if !contains(paths, "task.txt") {
		t.Errorf("task.txt missing from the snapshot: %v", paths)
	}
	// The proof that the exclude landed somewhere git actually reads.
	if containsPrefix(paths, repo.StateDir+"/") {
		t.Errorf("kopicode's state directory leaked into the snapshot, so the exclude went to a path "+
			"a linked worktree does not read: %v", paths)
	}
}

// TestSnapshotIsDeterministicUnderAFixedClock is what the injected clock buys.
// Two repositories, the same content, the same clock: the same commit sha.
// Without the injection, commit-tree reads the wall clock and SLICE-1's
// byte-identical-replay criterion is unreachable.
func TestSnapshotIsDeterministicUnderAFixedClock(t *testing.T) {
	commitIn := func(dir string) repo.Snapshot {
		t.Helper()
		initRepo(t, dir)
		writeFile(t, dir, "same.txt", "identical content\n")
		_, s := newSnapshotter(t, dir, "session-determinism")
		snap, err := s.Snapshot(context.Background(), 1)
		if err != nil {
			t.Fatalf("Snapshot in %s: %v", dir, err)
		}
		return snap
	}

	base := t.TempDir()
	a := commitIn(filepath.Join(base, "a"))
	b := commitIn(filepath.Join(base, "b"))

	if a.Commit != b.Commit {
		t.Errorf("same tree, same clock, different commits: %s vs %s", a.Commit, b.Commit)
	}
	if a.Tree != b.Tree {
		t.Errorf("same content, different trees: %s vs %s", a.Tree, b.Tree)
	}
}

// TestSnapshotIdentityIsKopicodeNotTheUser — the commit is machine-made and
// says so. Attributing it to whoever happens to be configured would make an
// agent's snapshot indistinguishable from the user's own work in any tool that
// shows an author, and would make the sha depend on the machine.
func TestSnapshotIdentityIsKopicodeNotTheUser(t *testing.T) {
	dir := newRepo(t)
	// A configured identity the snapshot must not pick up.
	git(t, dir, "config", "user.name", "Real Person")
	git(t, dir, "config", "user.email", "real@example.invalid")

	_, s := newSnapshotter(t, dir, "session-identity")
	writeFile(t, dir, "a.txt", "a\n")
	snap, err := s.Snapshot(context.Background(), 1)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}

	// %at, the epoch seconds, rather than %aI. Both render the same instant,
	// but git spells a UTC offset "+00:00" up to 2.34 and "Z" after it, so an
	// ISO assertion pins the git version rather than the behaviour — which is
	// how this first went red on CI while passing locally.
	got := git(t, dir, "show", "-s", "--format=%an|%ae|%cn|%ce|%at|%ct", snap.Commit)
	want := "kopicode|kopicode@kopicode.invalid|kopicode|kopicode@kopicode.invalid|1700000000|1700000000"
	if got != want {
		t.Errorf("commit identity and date = %q, want %q", got, want)
	}
}

// TestSnapshotSucceedsWithNoGitIdentityConfigured — `git commit-tree` fails
// with "Author identity unknown" when none is configured, and a session dying
// at the end of its first modifying turn on a freshly installed machine is a
// bad first impression. The fixed identity is what removes the dependency;
// this test removes any chance of one being there.
func TestSnapshotSucceedsWithNoGitIdentityConfigured(t *testing.T) {
	dir := newRepo(t)
	// TestMain already pins the global and system configuration away. Nothing
	// local was ever set, so the repository has no identity at all — verify
	// that rather than assume it.
	if out, err := tryGit(t, dir, "config", "--get", "user.email"); err == nil {
		t.Fatalf("the fixture has user.email = %q configured; this test is not testing what it claims", out)
	}

	_, s := newSnapshotter(t, dir, "session-no-identity")
	writeFile(t, dir, "a.txt", "a\n")
	if _, err := s.Snapshot(context.Background(), 1); err != nil {
		t.Fatalf("Snapshot with no configured git identity: %v", err)
	}
}

// TestNewSnapshotterRejectsASessionThatAlreadyHasRefs — continuing would start
// a second chain from no parent under ref names the first already uses,
// orphaning commits and describing a sequence of turns that never happened.
func TestNewSnapshotterRejectsASessionThatAlreadyHasRefs(t *testing.T) {
	dir := newRepo(t)
	ctx := context.Background()
	_, s := newSnapshotter(t, dir, "session-twice")
	writeFile(t, dir, "a.txt", "a\n")
	if _, err := s.Snapshot(ctx, 1); err != nil {
		t.Fatalf("Snapshot: %v", err)
	}

	r, err := repo.Open(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	_, err = repo.NewSnapshotter(ctx, r, "session-twice", repo.WithClock(fixedClock()))
	if !errors.Is(err, repo.ErrSessionExists) {
		t.Errorf("NewSnapshotter on a session with refs = %v, want ErrSessionExists", err)
	}
}

// TestNewResumingSnapshotterAttachesToTheExistingChain is KAN-939: a second
// Snapshotter over the same session id, built with NewResumingSnapshotter
// rather than NewSnapshotter, picks up exactly where the first left off
// instead of refusing or starting a second chain.
func TestNewResumingSnapshotterAttachesToTheExistingChain(t *testing.T) {
	dir := newRepo(t)
	ctx := context.Background()

	_, s1 := newSnapshotter(t, dir, "session-resume")
	writeFile(t, dir, "one.txt", "1\n")
	first, err := s1.Snapshot(ctx, 1)
	if err != nil {
		t.Fatalf("Snapshot(1): %v", err)
	}
	writeFile(t, dir, "two.txt", "2\n")
	second, err := s1.Snapshot(ctx, 2)
	if err != nil {
		t.Fatalf("Snapshot(2): %v", err)
	}
	if second.Parent != first.Commit {
		t.Fatalf("setup: second.Parent = %s, want %s", second.Parent, first.Commit)
	}

	// A brand new Snapshotter value, as a resumed process would build: nothing
	// carried over in memory, only the session id and the refs already on disk.
	r, err := repo.Open(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	s2, err := repo.NewResumingSnapshotter(ctx, r, "session-resume", repo.WithClock(fixedClock()))
	if err != nil {
		t.Fatalf("NewResumingSnapshotter: %v", err)
	}
	t.Cleanup(func() {
		if err := s2.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})

	writeFile(t, dir, "three.txt", "3\n")
	third, err := s2.Snapshot(ctx, 3)
	if err != nil {
		t.Fatalf("Snapshot(3) after resuming: %v", err)
	}
	if third.Parent != second.Commit {
		t.Errorf("resumed snapshot's Parent = %s, want the pre-resume chain's last commit %s",
			third.Parent, second.Commit)
	}
	// And git agrees, which the returned struct alone would not prove.
	if got := git(t, dir, "rev-parse", third.Commit+"^"); got != second.Commit {
		t.Errorf("git says %s's parent is %s, want %s", third.Commit, got, second.Commit)
	}
	// A turn at or before the attached chain's last turn is still refused: the
	// attach seeded lastTurn from the real refs, not just Parent.
	if _, err := s2.Snapshot(ctx, 2); !errors.Is(err, repo.ErrTurnNotIncreasing) {
		t.Errorf("Snapshot(2) on a chain attached at turn 2 = %v, want ErrTurnNotIncreasing", err)
	}
}

// TestNewResumingSnapshotterWithNoExistingRefsStartsFresh — a session that
// never reached a mutating turn before it stopped has nothing to attach to,
// and resuming it must behave exactly like a fresh chain rather than erroring
// on an attach with nothing to find.
func TestNewResumingSnapshotterWithNoExistingRefsStartsFresh(t *testing.T) {
	dir := newRepo(t)
	ctx := context.Background()

	r, err := repo.Open(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	s, err := repo.NewResumingSnapshotter(ctx, r, "session-never-snapshotted", repo.WithClock(fixedClock()))
	if err != nil {
		t.Fatalf("NewResumingSnapshotter on a session with no refs: %v", err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})

	writeFile(t, dir, "one.txt", "1\n")
	first, err := s.Snapshot(ctx, 1)
	if err != nil {
		t.Fatalf("Snapshot(1): %v", err)
	}
	if first.Parent != "" {
		t.Errorf("first snapshot on an attach with nothing to find has Parent = %q, want empty", first.Parent)
	}
}

// TestNewSnapshotterStillRefusesACollisionEvenAfterTheAttachPathExists is a
// regression check on the point KAN-939 cares about most: adding
// NewResumingSnapshotter must not have loosened NewSnapshotter's own
// refusal. A genuinely new session that happens to collide with an old id is
// not the same fact as a caller asking to resume that id, and only the
// second is what NewResumingSnapshotter is for.
func TestNewSnapshotterStillRefusesACollisionEvenAfterTheAttachPathExists(t *testing.T) {
	dir := newRepo(t)
	ctx := context.Background()
	_, s := newSnapshotter(t, dir, "session-collides")
	writeFile(t, dir, "a.txt", "a\n")
	if _, err := s.Snapshot(ctx, 1); err != nil {
		t.Fatalf("Snapshot: %v", err)
	}

	r, err := repo.Open(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	_, err = repo.NewSnapshotter(ctx, r, "session-collides", repo.WithClock(fixedClock()))
	if !errors.Is(err, repo.ErrSessionExists) {
		t.Errorf("NewSnapshotter on a colliding id = %v, want ErrSessionExists", err)
	}
}

// TestSnapshotRejectsANonIncreasingTurn — the ref name is the turn, so a
// repeated turn would replace a published snapshot and orphan the commit it
// replaced.
func TestSnapshotRejectsANonIncreasingTurn(t *testing.T) {
	dir := newRepo(t)
	_, s := newSnapshotter(t, dir, "session-turns")
	ctx := context.Background()

	writeFile(t, dir, "a.txt", "a\n")
	if _, err := s.Snapshot(ctx, 3); err != nil {
		t.Fatalf("Snapshot(3): %v", err)
	}
	for _, turn := range []int{3, 2, 0} {
		if _, err := s.Snapshot(ctx, turn); !errors.Is(err, repo.ErrTurnNotIncreasing) {
			t.Errorf("Snapshot(%d) after turn 3 = %v, want ErrTurnNotIncreasing", turn, err)
		}
	}
	if _, err := s.Snapshot(ctx, -1); !errors.Is(err, repo.ErrTurnNotIncreasing) {
		t.Errorf("Snapshot(-1) = %v, want ErrTurnNotIncreasing", err)
	}
	if _, err := s.Snapshot(ctx, 4); err != nil {
		t.Errorf("Snapshot(4) = %v, want success", err)
	}
}

// TestNewSnapshotterRejectsUnsafeSessionIDs — the id becomes a path component
// and a git ref component. A slash is a directory traversal in one and a
// namespace change in the other.
func TestNewSnapshotterRejectsUnsafeSessionIDs(t *testing.T) {
	dir := newRepo(t)
	r, err := repo.Open(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}

	for _, id := range []string{
		"", "../escape", "a/b", ".hidden", "-dash", "with space", "a..b",
		"session.lock", "sess\x00ion", strings.Repeat("x", 129),
	} {
		if _, err := repo.NewSnapshotter(context.Background(), r, id, repo.WithClock(fixedClock())); !errors.Is(err, repo.ErrInvalidSessionID) {
			t.Errorf("NewSnapshotter(%q) = %v, want ErrInvalidSessionID", id, err)
		}
	}

	for _, id := range []string{"a", "01J8Z5X7", "sess-1_2.3", "A"} {
		s, err := repo.NewSnapshotter(context.Background(), r, id, repo.WithClock(fixedClock()))
		if err != nil {
			t.Errorf("NewSnapshotter(%q) = %v, want success", id, err)
			continue
		}
		if err := s.Close(); err != nil {
			t.Error(err)
		}
	}
}

// TestSnapshotHonoursCancellation — the engine cancels a turn on Ctrl-C, and a
// snapshot that ignored the context would keep a killed turn's work running.
func TestSnapshotHonoursCancellation(t *testing.T) {
	dir := newRepo(t)
	_, s := newSnapshotter(t, dir, "session-cancel")

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	writeFile(t, dir, "a.txt", "a\n")
	if _, err := s.Snapshot(ctx, 1); !errors.Is(err, context.Canceled) {
		t.Errorf("Snapshot with a cancelled context = %v, want context.Canceled", err)
	}
	// And nothing was published under a cancelled turn.
	if out := git(t, dir, "for-each-ref", "--format=%(refname)", repo.RefPrefix); out != "" {
		t.Errorf("a cancelled snapshot published %s", out)
	}
}

// TestCloseRemovesTheThrowawayIndex — the index names every path in the working
// tree, so a tidy session should not leave one behind.
func TestCloseRemovesTheThrowawayIndex(t *testing.T) {
	dir := newRepo(t)
	ctx := context.Background()
	r, err := repo.Open(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	s, err := repo.NewSnapshotter(ctx, r, "session-close", repo.WithClock(fixedClock()))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Snapshot(ctx, 1); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(s.IndexPath()); err != nil {
		t.Fatalf("no throwaway index after a snapshot: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := os.Stat(s.IndexPath()); !os.IsNotExist(err) {
		t.Errorf("Close left %s behind (stat err = %v)", s.IndexPath(), err)
	}
	// Idempotent: a deferred Close after an explicit one is normal.
	if err := s.Close(); err != nil {
		t.Errorf("second Close: %v", err)
	}
}

// TestSnapshotOfAnUnchangedTreeIsRecorded — whether a turn touched anything is
// the engine's judgement, not this package's. Recording a duplicate tree is
// honest; silently skipping would leave a turn with no snapshot and no
// explanation.
func TestSnapshotOfAnUnchangedTreeIsRecorded(t *testing.T) {
	dir := newRepo(t)
	_, s := newSnapshotter(t, dir, "session-nochange")
	ctx := context.Background()

	writeFile(t, dir, "a.txt", "a\n")
	first, err := s.Snapshot(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.Snapshot(ctx, 2)
	if err != nil {
		t.Fatal(err)
	}
	if first.Tree != second.Tree {
		t.Errorf("unchanged tree produced %s then %s", first.Tree, second.Tree)
	}
	if first.Commit == second.Commit {
		t.Error("the two snapshots share a commit, so turn 2 was not recorded")
	}
	if second.Parent != first.Commit {
		t.Errorf("Parent = %s, want %s", second.Parent, first.Commit)
	}
}

// TestSnapshotIsSafeForConcurrentUse — the engine loop is concurrent
// (streaming, tool dispatch, cancellation), so a Snapshotter reachable from two
// goroutines is a matter of when rather than whether. Under -race this checks
// the mutex; regardless of scheduling it checks that the chain stays a chain,
// since the turn ordering means exactly one of a racing pair may win.
func TestSnapshotIsSafeForConcurrentUse(t *testing.T) {
	dir := newRepo(t)
	_, s := newSnapshotter(t, dir, "session-concurrent")
	ctx := context.Background()
	writeFile(t, dir, "a.txt", "a\n")

	const n = 8
	results := make(chan repo.Snapshot, n)
	errs := make(chan error, n)
	var wg sync.WaitGroup
	for turn := 1; turn <= n; turn++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			snap, err := s.Snapshot(ctx, turn)
			if err != nil {
				errs <- err
				return
			}
			results <- snap
		}()
	}
	wg.Wait()
	close(results)
	close(errs)

	for err := range errs {
		// The only legal failure is losing the ordering race.
		if !errors.Is(err, repo.ErrTurnNotIncreasing) {
			t.Errorf("concurrent Snapshot = %v, want success or ErrTurnNotIncreasing", err)
		}
	}

	// Every snapshot that succeeded is on one chain, with no commit claimed
	// twice and no ref left pointing at an orphan.
	seen := map[string]bool{}
	for snap := range results {
		if seen[snap.Commit] {
			t.Errorf("commit %s was returned by two snapshots", snap.Commit)
		}
		seen[snap.Commit] = true
		if got := git(t, dir, "rev-parse", snap.Ref); got != snap.Commit {
			t.Errorf("%s points at %s, want %s", snap.Ref, got, snap.Commit)
		}
	}
	if len(seen) == 0 {
		t.Error("no snapshot succeeded at all")
	}
}

// clock advancing between calls, to prove the injected value is read per
// snapshot rather than captured once.
func TestSnapshotReadsTheClockPerSnapshot(t *testing.T) {
	dir := newRepo(t)
	ctx := context.Background()
	r, err := repo.Open(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	tick := time.Unix(1700000000, 0).UTC()
	s, err := repo.NewSnapshotter(ctx, r, "session-clock", repo.WithClock(func() time.Time {
		tick = tick.Add(time.Minute)
		return tick
	}))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })

	writeFile(t, dir, "a.txt", "a\n")
	first, err := s.Snapshot(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.Snapshot(ctx, 2)
	if err != nil {
		t.Fatal(err)
	}

	firstAt := git(t, dir, "show", "-s", "--format=%at", first.Commit)
	secondAt := git(t, dir, "show", "-s", "--format=%at", second.Commit)
	if firstAt == secondAt {
		t.Errorf("both snapshots are dated %s; the clock was read once and reused", firstAt)
	}
}
