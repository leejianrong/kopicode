package bench

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// These tests drive [Worktrees]'s git seam instead of git.
//
// KAN-875's fault is intermittent — one sighting in roughly fifteen runs — and
// the mechanism behind it is a race between git *subprocesses*, which -race
// cannot see and a loop over real git cannot be made to reproduce on demand. So
// the contention and the failure are injected here rather than waited for: what
// gets asserted is that concurrent creations do not overlap, that a failure
// leaving a clear path is retried, and that one which does not is reported under
// the task's name. worktree_test.go keeps the end-to-end half against real git.

// fakeGit stands in for the git subprocess and records what was asked of it.
//
// It measures its own concurrency, because that is the property under test: the
// serialisation is not observable from outside [Worktrees] in any other way, and
// asserting it through a real repository would be asserting a probability.
type fakeGit struct {
	mu sync.Mutex
	// calls is every argv, in the order the commands started.
	calls [][]string
	// active and peak track how many commands were in flight at once.
	active, peak int
	// hold is how long each command occupies the seam, which is what turns an
	// absent lock into an observed overlap rather than a lucky one.
	hold time.Duration
	// fail decides what a command does. Nil means every command succeeds.
	fail func(nth int, args []string) error
	// nth counts commands, so fail can vary by attempt.
	nth int
}

func (g *fakeGit) run(_ context.Context, _ string, args ...string) (string, error) {
	g.mu.Lock()
	g.nth++
	nth := g.nth
	g.calls = append(g.calls, append([]string(nil), args...))
	g.active++
	if g.active > g.peak {
		g.peak = g.active
	}
	g.mu.Unlock()

	if g.hold > 0 {
		time.Sleep(g.hold)
	}

	g.mu.Lock()
	g.active--
	g.mu.Unlock()

	if g.fail != nil {
		return "", g.fail(nth, args)
	}
	return "", nil
}

func (g *fakeGit) argvs() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	out := make([]string, 0, len(g.calls))
	for _, c := range g.calls {
		out = append(out, strings.Join(c, " "))
	}
	return out
}

func (g *fakeGit) count(prefix string) int {
	n := 0
	for _, argv := range g.argvs() {
		if strings.HasPrefix(argv, prefix) {
			n++
		}
	}
	return n
}

func (g *fakeGit) highWater() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.peak
}

// newFakeTrees is a manager whose git is the fake and whose backoff does not
// sleep. Both directories are real and under t.TempDir(), because Add and the
// retry decision both look at the filesystem.
func newFakeTrees(t *testing.T, g *fakeGit) *Worktrees {
	t.Helper()
	w := NewWorktrees(t.TempDir(), t.TempDir(), false)
	w.run = g.run
	w.sleep = func(time.Duration) {}
	return w
}

// gitFailure is what a losing race actually looks like coming back from git
// 2.34.1: a sibling registration that had been created but not yet filled in.
func gitFailure(other string) error {
	return &GitError{
		Args:     []string{"worktree", "add"},
		Dir:      "/repo",
		ExitCode: 128,
		Stderr: fmt.Sprintf("fatal: failed to read .git/worktrees/%s/commondir: "+
			"No such file or directory", other),
		Err: errors.New("exit status 128"),
	}
}

// TestConcurrentAddsDoNotOverlap is the fix, asserted as the property rather
// than as the mechanism.
//
// Two `git worktree add` processes against one repository race inside git: add
// enumerates the existing registrations before it creates anything, and dies
// reading a sibling that another add has mkdir'd and not yet written. It is
// git's race, not this package's, so the only cure available here is not to
// hold two of them open at once.
//
// Without the lock this fails on essentially every run: eight goroutines each
// occupying the seam for 20ms cannot avoid overlapping. It is not a probability
// dressed as a test — the sleep is what makes the interleaving certain rather
// than what makes it likely.
func TestConcurrentAddsDoNotOverlap(t *testing.T) {
	g := &fakeGit{hold: 20 * time.Millisecond}
	w := newFakeTrees(t, g)

	var wg sync.WaitGroup
	for i := 1; i <= 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			name := fmt.Sprintf("task-%02d", i)
			if _, err := w.Add(context.Background(), name, "sha"); err != nil {
				t.Errorf("Add(%s): %v", name, err)
			}
		}()
	}
	wg.Wait()

	if peak := g.highWater(); peak != 1 {
		t.Errorf("%d git worktree commands ran at once, want 1; concurrent `git worktree add` "+
			"against one repository races inside git and loses one of the worktrees", peak)
	}
	if got := w.Counts(); got.Created != 8 || len(got.CreateFailed) != 0 {
		t.Errorf("counts = %+v, want 8 created and none failed", got)
	}
}

// TestReleaseDoesNotOverlapAdd holds the other half of the same lock.
//
// A run releases one task's worktree while the next task's is being created,
// and `git worktree remove` enumerates the registry exactly as `add` does. Its
// fallback also prunes, and prune deletes a registration with no gitdir file in
// it — which is precisely what a half-finished concurrent add looks like. So
// removal is inside the lock too, and a run that only serialised creation would
// have kept the more destructive half of the race.
func TestReleaseDoesNotOverlapAdd(t *testing.T) {
	g := &fakeGit{hold: 10 * time.Millisecond}
	w := newFakeTrees(t, g)

	paths := make([]string, 0, 4)
	for i := 1; i <= 4; i++ {
		path, err := w.Add(context.Background(), fmt.Sprintf("existing-%02d", i), "sha")
		if err != nil {
			t.Fatalf("Add: %v", err)
		}
		paths = append(paths, path)
	}

	var wg sync.WaitGroup
	for i, path := range paths {
		wg.Add(2)
		go func() {
			defer wg.Done()
			if err := w.Release(context.Background(), path); err != nil {
				t.Errorf("Release(%s): %v", path, err)
			}
		}()
		go func() {
			defer wg.Done()
			if _, err := w.Add(context.Background(), fmt.Sprintf("new-%02d", i), "sha"); err != nil {
				t.Errorf("Add: %v", err)
			}
		}()
	}
	wg.Wait()

	if peak := g.highWater(); peak != 1 {
		t.Errorf("%d git worktree commands ran at once, want 1; a remove overlapping an add "+
			"races on the same registry, and remove's fallback prunes", peak)
	}
}

// TestAddRetriesWhenTheFailedAttemptLeftNothingBehind is the cross-process half.
//
// Serialising this package's own commands cannot reach a git process outside it
// — another kopibench, a developer's own `git worktree add`, a gc — so a losing
// attempt that left no directory at the target path is tried again. The prune
// between attempts matters: git names a retry `task-011` rather than `task-01`
// if the abandoned registration is still there, and that is a second worktree
// rather than the one that was asked for.
func TestAddRetriesWhenTheFailedAttemptLeftNothingBehind(t *testing.T) {
	g := &fakeGit{}
	g.fail = func(nth int, args []string) error {
		if nth == 1 && args[1] == "add" {
			return gitFailure("task-07")
		}
		return nil
	}
	w := newFakeTrees(t, g)

	path, err := w.Add(context.Background(), "task-01", "sha")
	if err != nil {
		t.Fatalf("Add: %v; a losing race against another git process is retried, not reported", err)
	}
	if want := filepath.Join(w.Base(), "task-01"); path != want {
		t.Errorf("path = %s, want %s", path, want)
	}
	if got := w.Counts(); got.Created != 1 || len(got.CreateFailed) != 0 {
		t.Errorf("counts = %+v, want created 1 and none failed", got)
	}

	want := []string{
		"worktree add --detach --quiet " + path + " sha",
		"worktree prune",
		"worktree add --detach --quiet " + path + " sha",
	}
	got := g.argvs()
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("git commands:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

// TestAddNamesTheTaskItCouldNotCreateAWorktreeFor is the outcome the card is
// really about: a creation that stays failed is loud, and it is loud under the
// task's own name.
//
// The count is not enough. A run reporting `created 9` for a ten-task corpus
// looks healthy — the numbers are symmetric, the reclamation is clean — and
// "9" does not say which task never ran. So the name goes on the account, and
// [Runner.Run] refuses to report success while the account holds one.
func TestAddNamesTheTaskItCouldNotCreateAWorktreeFor(t *testing.T) {
	g := &fakeGit{}
	g.fail = func(_ int, args []string) error {
		if args[1] == "add" {
			return gitFailure("task-07")
		}
		return nil
	}
	w := newFakeTrees(t, g)

	_, err := w.Add(context.Background(), "task-03", "sha")
	if err == nil {
		t.Fatal("Add: no error from a creation that never succeeded")
	}
	for _, want := range []string{"task-03", "commondir"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error does not mention %q, so neither the task nor git's own "+
				"diagnosis reaches the report: %v", want, err)
		}
	}

	got := w.Counts()
	if got.Created != 0 {
		t.Errorf("Created = %d, want 0", got.Created)
	}
	if len(got.CreateFailed) != 1 || got.CreateFailed[0] != "task-03" {
		t.Errorf("CreateFailed = %v, want [task-03]", got.CreateFailed)
	}
	if n := g.count("worktree add"); n != addAttempts {
		t.Errorf("%d attempts, want %d: the retry is bounded, and a run must not spend "+
			"a benchmark's time on a failure that is not going to clear", n, addAttempts)
	}
}

// TestAddDoesNotRetryWhenTheFailureLeftAPathBehind pins the retry *condition*,
// which is structural rather than a reading of git's message.
//
// Git's wording for a losing race varies by version and KAN-796 already paid for
// treating a version's output as an API. So the question asked is "did the
// failed attempt leave the ground clear", not "does this text look transient".
// A directory already at the target path is the answer no: something is there,
// and trying again would either fail identically or create a differently named
// worktree.
func TestAddDoesNotRetryWhenTheFailureLeftAPathBehind(t *testing.T) {
	g := &fakeGit{}
	g.fail = func(_ int, args []string) error {
		if args[1] == "add" {
			return gitFailure("task-07")
		}
		return nil
	}
	w := newFakeTrees(t, g)
	if err := os.MkdirAll(filepath.Join(w.Base(), "task-02"), 0o755); err != nil {
		t.Fatal(err)
	}

	if _, err := w.Add(context.Background(), "task-02", "sha"); err == nil {
		t.Fatal("Add: no error")
	}
	if n := g.count("worktree add"); n != 1 {
		t.Errorf("%d attempts, want 1: a path that is already occupied is not contention", n)
	}
}

// TestAddStopsRetryingOnCancellation keeps the retry from outliving the run it
// belongs to. A cancelled run must not spend three attempts and two backoffs per
// task discovering that git is not going to answer.
func TestAddStopsRetryingOnCancellation(t *testing.T) {
	g := &fakeGit{}
	g.fail = func(_ int, args []string) error {
		if args[1] == "add" {
			return gitFailure("task-07")
		}
		return nil
	}
	w := newFakeTrees(t, g)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := w.Add(ctx, "task-04", "sha"); err == nil {
		t.Fatal("Add: no error")
	}
	if n := g.count("worktree add"); n != 1 {
		t.Errorf("%d attempts after cancellation, want 1", n)
	}
}
