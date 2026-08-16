package bench

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// reclaimTimeout bounds one reclamation command. Removal runs on a context
// detached from the run's (see [Worktrees.Release]), so it needs a bound of its
// own or a cancelled run could hang in cleanup — which is the failure mode that
// leaves the disk full, arrived at from the other side.
const reclaimTimeout = 30 * time.Second

// WorktreeSubdir is the directory, relative to the repository's kopicode state
// directory, that this package's worktrees are created under.
//
// It is stable rather than per-run, and that is what makes reclamation at run
// start structural instead of a heuristic. A crashed run leaves its checkouts
// at exactly the paths the next run needs, so the next run *has* to reclaim
// them to proceed, and it can do so knowing they are its own: nothing outside
// this directory is ever touched, so a developer's worktrees and another
// agent's leases are out of reach by construction.
const WorktreeSubdir = "bench/worktrees"

// addAttempts and addBackoff bound the retry on [Worktrees.Add].
//
// Three attempts, and the delay grows, because the failure being retried is
// contention with a git process this package does not control — another
// kopibench, a developer's own `git worktree add`, a `gc` — and contention that
// has not cleared in ~150ms is not contention. The in-process half of the same
// race is fixed rather than retried; see [Worktrees.registry].
const (
	addAttempts = 3
	addBackoff  = 50 * time.Millisecond
)

// gitRunner is the seam every git subprocess in this file goes through.
//
// It exists so the retry and the serialisation can be *driven* rather than
// hoped for. The fault this guards against is intermittent by nature — it
// appeared once in fifteen runs — so a test that runs real git in a loop and
// waits to get lucky proves nothing. A test that forces the failure proves the
// path handles it.
type gitRunner func(ctx context.Context, dir string, args ...string) (string, error)

// Reclamation is the account of what a run did to the repository's worktrees.
//
// It is reported rather than kept internal because silent cleanup and silent
// accumulation look identical from outside: a run that removed ten worktrees
// and a run that quietly kept ten both print nothing. Every number here is
// counted at the point the git command succeeded, not predicted from the number
// of tasks.
type Reclamation struct {
	// PrunedAdmin is how many registrations `git worktree prune` reclaimed at
	// run start — entries whose directory had already gone.
	PrunedAdmin int
	// PrunedStale is how many leftover checkouts under [WorktreeSubdir] were
	// removed at run start. A non-zero value means a previous run did not
	// finish, or finished with --keep-worktrees.
	PrunedStale int
	// Created is how many worktrees this run added.
	Created int
	// Removed is how many it reclaimed.
	Removed int
	// Kept is how many it deliberately left behind for a post-mortem, which is
	// what --keep-worktrees asks for.
	Kept int
	// Failed lists the paths that could neither be removed by git nor deleted
	// from disk, so an accumulation has a name instead of a number.
	Failed []string
	// CreateFailed names the tasks whose worktree could not be created at all,
	// sorted.
	//
	// It is a list of names and not a count for the same reason Failed is: a
	// run that created nine worktrees for a ten-task corpus reports Created:9
	// and looks healthy from outside, and "9" does not say which task never
	// ran. Every entry here is a task the run measured nothing for, so
	// [Runner.Run] refuses to report success while this is non-empty.
	CreateFailed []string
}

// Worktrees creates and reclaims the git worktrees a run checks its tasks out
// into.
//
// It is safe for concurrent use: the runner adds and releases from several
// workers at once, and the counts are the run's report.
//
// # Git's worktree registry is not safe for concurrent writers, and that is this
// type's problem
//
// Two `git worktree add` processes against one repository race, and the race is
// git's rather than this package's: `add` enumerates the existing worktrees
// before it creates anything, and the enumeration dies with `fatal: failed to
// read .git/worktrees/<other>/commondir: No such file or directory` when it
// reads a sibling registration another `add` has mkdir'd but not yet filled in.
// Reproduced on git 2.34.1 at roughly one failure per hundred ten-way batches,
// which is the rate at which KAN-875's one-in-fifteen sighting arrives. `git
// worktree remove` races the same way ("is not a working tree"), and `git
// worktree prune` will delete a half-written registration outright, because an
// entry with no gitdir file yet is prunable without regard to any expiry.
//
// So every command in this file that reads or writes `.git/worktrees` is
// serialised through [Worktrees.registry]. The cost is nothing that matters —
// creating and removing a checkout of the corpus is milliseconds, and the
// parallelism a bench run exists for is the agent session and the oracle, both
// of which run outside the lock. What it buys is that the in-process half of
// the race cannot happen at all, rather than happening rarely.
//
// # Why every path is deferred
//
// Ten checkouts per arm, and an A/B series is many arms. Left alone that fills
// a disk quietly, and an agent harness that auto-creates worktrees is the
// textbook offender. So removal is deferred by the caller on every path —
// normal, error, panic and cancellation — and this type is written so that the
// last two actually work: [Worktrees.Release] runs its git commands on a
// context detached from the run's, because a cleanup that inherits a cancelled
// context is a cleanup that does nothing exactly when it is needed most.
type Worktrees struct {
	// repo is the repository the worktrees are linked to. Every git command
	// here names it as its working directory.
	repo string
	// base is the directory worktrees are created under, always inside repo's
	// state directory.
	base string
	// keep suppresses removal, for a post-mortem.
	keep bool

	// run is the git seam. Nil means [runGit]; only a test sets it, and it is
	// set before the first command runs.
	run gitRunner
	// sleep is the retry backoff. Nil means time.Sleep; only a test sets it, so
	// the attempts are driven rather than waited out.
	sleep func(time.Duration)

	// registry serialises every git command that reads or writes the
	// repository's worktree registry — add, remove, prune and list. See the
	// type's doc comment for the race it closes. It is never held while an
	// agent session or an oracle runs.
	registry sync.Mutex

	mu    sync.Mutex
	stats Reclamation
}

// NewWorktrees returns a manager for the worktrees of repoRoot, created under
// its state directory. keep leaves them behind on release.
func NewWorktrees(repoRoot, stateDir string, keep bool) *Worktrees {
	return &Worktrees{
		repo: repoRoot,
		base: filepath.Join(stateDir, filepath.FromSlash(WorktreeSubdir)),
		keep: keep,
	}
}

// Base is the directory worktrees are created under.
func (w *Worktrees) Base() string { return w.base }

// Counts reports the account so far.
//
// The two name lists are sorted rather than left in the order the workers
// happened to finish in, so two runs of the same corpus produce the same report.
func (w *Worktrees) Counts() Reclamation {
	w.mu.Lock()
	defer w.mu.Unlock()
	out := w.stats
	out.Failed = sortedCopy(w.stats.Failed)
	out.CreateFailed = sortedCopy(w.stats.CreateFailed)
	return out
}

func sortedCopy(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	out := append([]string(nil), in...)
	sort.Strings(out)
	return out
}

// gitLocked runs one registry command. The caller holds [Worktrees.registry];
// every path that reaches here has it, which is what makes the serialisation a
// property of the type rather than of each call site.
func (w *Worktrees) gitLocked(ctx context.Context, args ...string) (string, error) {
	run := w.run
	if run == nil {
		run = runGit
	}
	return run(ctx, w.repo, args...)
}

// Reclaim is the run-start prune. It reclaims what a previous crash orphaned,
// in the two forms an orphan takes.
//
// First `git worktree prune`, which removes the administrative entries under
// .git/worktrees whose checkout directory has already gone. Those are invisible
// otherwise: they hold locks on branches and they accumulate silently.
//
// Then the checkouts themselves. Anything still registered under [Base] is a
// previous run's — this directory is only ever written by this package — so it
// is removed rather than worked around. That is what makes a crashed run
// self-healing instead of a directory nobody ever looks in again, and it is why
// --keep-worktrees is documented as "inspect before you re-run": the next run
// reclaims what it kept.
//
// The admin count is *measured* rather than parsed: the registrations are
// listed either side of the prune and the difference is what git actually
// removed. An earlier version counted the lines of
// `worktree prune --verbose --dry-run`, which cost a red CI run — the count came
// back zero on the runner's git while the prune itself worked. A number that
// disagrees with what happened is worse than no number, because the whole point
// of reporting the reclamation is that it can be checked.
// The whole body holds [Worktrees.registry]: it lists, prunes and removes, and
// a concurrent Add between the two listings would be both miscounted and at
// risk of being pruned half-created.
func (w *Worktrees) Reclaim(ctx context.Context) error {
	w.registry.Lock()
	defer w.registry.Unlock()

	before, err := w.registered(ctx)
	if err != nil {
		return err
	}
	if _, err := w.gitLocked(ctx, "worktree", "prune"); err != nil {
		return fmt.Errorf("bench: pruning worktree registrations: %w", err)
	}
	after, err := w.registered(ctx)
	if err != nil {
		return err
	}
	admin := len(difference(before, after))

	// What is left registered under this package's own base directory is a
	// previous run's checkout, still on disk. Nothing outside the base is ever
	// considered: a developer's worktree and another agent's lease are out of
	// reach by construction.
	var stale []string
	for _, path := range after {
		if underDir(w.base, path) {
			stale = append(stale, path)
		}
	}

	var failed []string
	removed := 0
	for _, path := range stale {
		if err := w.remove(ctx, path); err != nil {
			failed = append(failed, path)
			continue
		}
		removed++
	}

	w.mu.Lock()
	w.stats.PrunedAdmin += admin
	w.stats.PrunedStale += removed
	w.stats.Failed = append(w.stats.Failed, failed...)
	w.mu.Unlock()

	if len(failed) > 0 {
		return fmt.Errorf("bench: %d orphaned worktree(s) under %s could not be reclaimed: %s: %w",
			len(failed), w.base, strings.Join(failed, ", "), ErrReclaim)
	}
	return nil
}

// registered lists the worktree paths git knows about, sorted so a failure
// message is stable across runs. The caller holds [Worktrees.registry].
func (w *Worktrees) registered(ctx context.Context) ([]string, error) {
	out, err := w.gitLocked(ctx, "worktree", "list", "--porcelain")
	if err != nil {
		return nil, fmt.Errorf("bench: listing worktrees: %w", err)
	}

	var found []string
	for _, line := range gitLines(out) {
		if path, ok := strings.CutPrefix(line, "worktree "); ok {
			found = append(found, path)
		}
	}
	sort.Strings(found)
	return found, nil
}

// difference returns the entries of a that are not in b.
func difference(a, b []string) []string {
	inB := make(map[string]bool, len(b))
	for _, s := range b {
		inB[s] = true
	}
	var only []string
	for _, s := range a {
		if !inB[s] {
			only = append(only, s)
		}
	}
	return only
}

// Add creates a worktree named for the task, detached at commit, and returns
// its path.
//
// --detach is not an optimisation. `git worktree add` without it creates a
// *branch* named after the directory, in the shared ref store, and a benchmark
// run would then leave ten branches in the user's repository per arm — the ref
// half of exactly the accumulation this card is about, and a write to git state
// that is not kopicode's to make.
//
// A failure is recorded under the task's name as well as returned, because the
// caller's error becomes one task's result and the run's account is what says
// the corpus was not fully measured. A run that created nine worktrees for ten
// tasks must not be able to report success, and [Runner.Run] reads
// [Reclamation.CreateFailed] to make sure it cannot.
func (w *Worktrees) Add(ctx context.Context, name, commit string) (string, error) {
	path := filepath.Join(w.base, name)
	if err := w.add(ctx, name, path, commit); err != nil {
		w.mu.Lock()
		w.stats.CreateFailed = append(w.stats.CreateFailed, name)
		w.mu.Unlock()
		return "", err
	}

	w.mu.Lock()
	w.stats.Created++
	w.mu.Unlock()
	return path, nil
}

// add is the creation itself: serialised against every other registry command,
// and retried a bounded number of times.
//
// The retry is for the half of the race this package cannot serialise away — a
// git process outside it, contending on the same `.git/worktrees` — and it is
// deliberately not conditioned on what git *said*. Git's wording for a losing
// race varies by version, and KAN-796 already paid for reading a version's
// output as if it were an API. The condition is structural instead: retry only
// when the failed attempt left no directory at the target path, which is the
// difference between "somebody else got in the way" and "there is already
// something here". The first error is the one reported, because it is the one
// that describes the original failure rather than its aftermath.
func (w *Worktrees) add(ctx context.Context, name, path, commit string) error {
	w.registry.Lock()
	defer w.registry.Unlock()

	var first error
	for attempt := 1; ; attempt++ {
		if err := os.MkdirAll(w.base, 0o755); err != nil {
			if first == nil {
				first = fmt.Errorf("creating the worktree directory: %w", err)
			}
			break
		}
		_, err := w.gitLocked(ctx, "worktree", "add", "--detach", "--quiet", path, commit)
		if err == nil {
			return nil
		}
		if first == nil {
			first = err
		}
		if ctx.Err() != nil || attempt >= addAttempts || !w.clearForRetry(ctx, path) {
			break
		}
		w.backoff(attempt)
	}
	return fmt.Errorf("bench: creating a worktree for %s: %w", name, first)
}

// clearForRetry reports whether another attempt at path is worth making.
//
// The prune first: a `worktree add` that died part way can leave a registration
// under .git/worktrees with no gitdir file in it, and git prunes exactly that
// without regard to any expiry. Without it a retry would be given a name with a
// numeric suffix, which is a second worktree rather than the one that was asked
// for. Its failure is ignored on purpose — it is a cleanup attempt inside an
// error path, and the error already in hand is the one worth reporting.
func (w *Worktrees) clearForRetry(ctx context.Context, path string) bool {
	_, _ = w.gitLocked(ctx, "worktree", "prune")
	_, err := os.Stat(path)
	return errors.Is(err, os.ErrNotExist)
}

func (w *Worktrees) backoff(attempt int) {
	sleep := w.sleep
	if sleep == nil {
		sleep = time.Sleep
	}
	sleep(time.Duration(attempt) * addBackoff)
}

// Release reclaims one worktree, or records that it was kept.
//
// It is what a caller defers, so it has to work on the two paths a defer exists
// for and a happy-path implementation would fail on:
//
//   - **Panic.** A deferred call runs while the panic unwinds, so nothing extra
//     is needed here — but the runner recovers per task rather than letting the
//     process die, so the other nine tasks' worktrees are reclaimed too.
//   - **Cancellation.** ctx is *deliberately detached* with
//     [context.WithoutCancel] before any git command runs. A cancelled run's
//     cleanup that inherited the cancelled context would fail at the first
//     subprocess and leave the checkout on disk, which is the accumulation this
//     card exists to prevent, arriving through the mechanism meant to prevent
//     it. The detached context still carries a deadline of its own, so a wedged
//     git cannot turn a cancelled run into a hung one.
//
// A removal git refuses is retried as a plain directory delete followed by a
// prune, because a worktree whose files are in the way is still disk this run
// created. Only a path that survives both is reported as failed.
func (w *Worktrees) Release(ctx context.Context, path string) error {
	if w.keep {
		w.mu.Lock()
		w.stats.Kept++
		w.mu.Unlock()
		return nil
	}

	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), reclaimTimeout)
	defer cancel()

	if err := w.removeSerialised(ctx, path); err != nil {
		w.mu.Lock()
		w.stats.Failed = append(w.stats.Failed, path)
		w.mu.Unlock()
		return fmt.Errorf("bench: reclaiming %s: %w", path, err)
	}

	w.mu.Lock()
	w.stats.Removed++
	w.mu.Unlock()
	return nil
}

// removeSerialised is [Worktrees.remove] with [Worktrees.registry] taken. It is
// the entry point for every caller that does not already hold the lock, which
// is everyone except [Worktrees.Reclaim].
//
// Removal is inside the lock rather than outside it because `git worktree
// remove` enumerates the registry exactly as `add` does, and its fallback runs
// a prune, which is the command that will delete a half-created registration
// belonging to a concurrent add.
func (w *Worktrees) removeSerialised(ctx context.Context, path string) error {
	w.registry.Lock()
	defer w.registry.Unlock()
	return w.remove(ctx, path)
}

// remove takes one worktree away, by whichever of the two mechanisms works. The
// caller holds [Worktrees.registry].
//
// `git worktree remove --force` is the right one: it deletes the checkout and
// the registration together. When it refuses — a file the model made read-only,
// a lock left by a killed git — the directory is deleted directly and the
// registration pruned, because the alternative is leaving disk behind and
// calling it an error. Both are attempted before anything is reported as
// failed, and success is verified by the directory being gone rather than by
// git's exit code alone.
func (w *Worktrees) remove(ctx context.Context, path string) error {
	gitErr := w.gitRemove(ctx, path)
	if gitErr == nil {
		return nil
	}

	if rmErr := os.RemoveAll(path); rmErr != nil {
		return errors.Join(gitErr, rmErr)
	}
	if _, err := w.gitLocked(ctx, "worktree", "prune"); err != nil {
		return errors.Join(gitErr, err)
	}
	if _, err := os.Stat(path); err == nil {
		return fmt.Errorf("%w: %s is still on disk", ErrReclaim, path)
	}
	return nil
}

func (w *Worktrees) gitRemove(ctx context.Context, path string) error {
	_, err := w.gitLocked(ctx, "worktree", "remove", "--force", path)
	return err
}

// underDir reports whether path is dir itself or inside it, comparing resolved
// absolute paths component by component.
//
// Symlinks are resolved where they can be, because macOS reports
// /var/folders/... from os.TempDir() and /private/var/folders/... from git, and
// a containment test that disagreed with git about the same directory would
// either skip a worktree this package owns or reach for one it does not.
func underDir(dir, path string) bool {
	dir, path = resolvePath(dir), resolvePath(path)
	rel, err := filepath.Rel(dir, path)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func resolvePath(path string) string {
	abs, err := filepath.Abs(path)
	if err != nil {
		return filepath.Clean(path)
	}
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		return resolved
	}
	return abs
}
