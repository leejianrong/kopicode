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
}

// Worktrees creates and reclaims the git worktrees a run checks its tasks out
// into.
//
// It is safe for concurrent use: the runner adds and releases from several
// workers at once, and the counts are the run's report.
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
func (w *Worktrees) Counts() Reclamation {
	w.mu.Lock()
	defer w.mu.Unlock()
	out := w.stats
	out.Failed = append([]string(nil), w.stats.Failed...)
	return out
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
// The count is git's own, taken from a dry run rather than inferred, so a
// report saying "reclaimed 3" is reporting three things git said it removed.
func (w *Worktrees) Reclaim(ctx context.Context) error {
	admin, err := w.pruneAdmin(ctx)
	if err != nil {
		return err
	}

	stale, err := w.registeredUnderBase(ctx)
	if err != nil {
		return err
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

// pruneAdmin runs `git worktree prune` and reports how many registrations it
// removed. The dry run is what supplies the count; git's real prune says
// nothing at all unless asked verbosely, and parsing a verbose real run would
// mean the number and the action came from two different commands anyway.
func (w *Worktrees) pruneAdmin(ctx context.Context) (int, error) {
	out, err := runGit(ctx, w.repo, "worktree", "prune", "--verbose", "--dry-run")
	if err != nil {
		return 0, fmt.Errorf("bench: listing prunable worktrees: %w", err)
	}
	n := len(gitLines(out))

	if _, err := runGit(ctx, w.repo, "worktree", "prune"); err != nil {
		return 0, fmt.Errorf("bench: pruning worktree registrations: %w", err)
	}
	return n, nil
}

// registeredUnderBase lists the worktrees git knows about whose path is inside
// [Base], sorted so a failure message is stable.
//
// The containment test is on cleaned absolute paths and is a path-component
// comparison, not a string prefix: a sibling directory named
// "bench/worktrees-old" must not be mistaken for something this package owns
// and deleted.
func (w *Worktrees) registeredUnderBase(ctx context.Context) ([]string, error) {
	out, err := runGit(ctx, w.repo, "worktree", "list", "--porcelain")
	if err != nil {
		return nil, fmt.Errorf("bench: listing worktrees: %w", err)
	}

	var found []string
	for _, line := range gitLines(out) {
		path, ok := strings.CutPrefix(line, "worktree ")
		if !ok {
			continue
		}
		if underDir(w.base, path) {
			found = append(found, path)
		}
	}
	sort.Strings(found)
	return found, nil
}

// Add creates a worktree named for the task, detached at commit, and returns
// its path.
//
// --detach is not an optimisation. `git worktree add` without it creates a
// *branch* named after the directory, in the shared ref store, and a benchmark
// run would then leave ten branches in the user's repository per arm — the ref
// half of exactly the accumulation this card is about, and a write to git state
// that is not kopicode's to make.
func (w *Worktrees) Add(ctx context.Context, name, commit string) (string, error) {
	if err := os.MkdirAll(w.base, 0o755); err != nil {
		return "", fmt.Errorf("bench: creating the worktree directory: %w", err)
	}
	path := filepath.Join(w.base, name)

	if _, err := runGit(ctx, w.repo,
		"worktree", "add", "--detach", "--quiet", path, commit); err != nil {
		return "", fmt.Errorf("bench: creating a worktree for %s: %w", name, err)
	}

	w.mu.Lock()
	w.stats.Created++
	w.mu.Unlock()
	return path, nil
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

	if err := w.remove(ctx, path); err != nil {
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

// remove takes one worktree away, by whichever of the two mechanisms works.
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
	if _, err := runGit(ctx, w.repo, "worktree", "prune"); err != nil {
		return errors.Join(gitErr, err)
	}
	if _, err := os.Stat(path); err == nil {
		return fmt.Errorf("%w: %s is still on disk", ErrReclaim, path)
	}
	return nil
}

func (w *Worktrees) gitRemove(ctx context.Context, path string) error {
	_, err := runGit(ctx, w.repo, "worktree", "remove", "--force", path)
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
