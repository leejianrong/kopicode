// Package repo is kopicode's git layer. Today it writes turn snapshots to
// shadow refs (docs/adr/0002-no-durable-runtime-own-journal.md §3,
// docs/SLICE-1.md §2); worktrees and diff rendering land here later.
//
// # Why this package exists
//
// The journal records the agent's decisions. It does not record the repository.
// A session replayed from the journal alone would rewind the conversation over
// a working tree that never moved, which is the failure ADR-0002 chose an
// owned journal plus git — rather than a durable-execution runtime — to avoid.
// Git versions the tree; this package is that half.
//
// # The promise
//
// The user's branch, HEAD, index and stashes are off limits. That is not a
// quality goal, it is the reason the mechanism is shaped the way it is: every
// git command that can write an index runs with GIT_INDEX_FILE pointed at a
// throwaway file under .kopicode/, verified rather than assumed, and the
// commits are written with commit-tree and published to refs under
// refs/kopicode/ where nothing the user runs will look. Nothing here checks
// out, resets, stashes, commits or moves a ref the user owns. A coding agent
// that corrupts someone's staged work is not recoverable by apologising.
//
// Two structural guards hold that promise rather than a convention:
// [Repo.git] refuses to run a subcommand that can write an index at all, and
// the isolated path verifies the GIT_INDEX_FILE it is about to hand git
// (see [ErrIndexNotIsolated]). An inherited or empty GIT_INDEX_FILE silently
// means the real index, so "we set it" is not the same fact as "it is set".
//
// # Shell out, never link
//
// Git is a subprocess (ADR-0001, CLAUDE.md). Linking libgit2 would cost CGo,
// and CGo costs cross-compilation without a C toolchain and breaks `go install`
// for users without a compiler — which is the distribution promise ADR-0001
// rests on. Every path here therefore threads a [context.Context] explicitly
// and none of them stores one.
//
// # Write, and read back
//
// Snapshots are recorded by [Snapshotter.Snapshot] and materialized back to a
// directory by [Repo.Restore] (KAN-938, docs/SLICE-1.md affordance G1).
// Restore is the primitive only — read a tree back out safely, nothing more.
// Resuming a session's chain from a prior snapshot and forking a new session
// from one both decide *when* to restore and what happens to the chain
// afterward, which is a different question from *how*, and both are still
// slice 2.
package repo

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// StateDir is kopicode's per-repository state directory, relative to the work
// tree root.
//
// It duplicates journal.StateDir deliberately. The engine journals and this
// package snapshots; the import arrow between them points one way, and
// internal/repo importing internal/journal to share a five-character constant
// would invert it. A drift between the two is not silent: the throwaway index
// lives under this name, so a mismatch makes the index appear inside a snapshot
// tree, which TestSnapshotExcludesTheThrowawayIndex asserts against.
const StateDir = ".kopicode"

// RefPrefix is the namespace every shadow ref lives under. Refs here are
// invisible to `git branch`, `git log` and `git status`, which is the point:
// the snapshots are kopicode's record of the tree, not entries in the user's
// history.
const RefPrefix = "refs/kopicode/"

// excludePattern is what goes in the exclude file. Leading slash so it anchors
// at the work tree root and does not also hide a `.kopicode` directory
// somewhere inside a project that legitimately has one.
const excludePattern = "/" + StateDir + "/"

// excludeComment explains the line to whoever finds it in a file they did not
// write.
const excludeComment = "# kopicode session state (journal, throwaway index). Added by kopicode."

// Sentinel causes, for errors.Is.
var (
	// ErrNotRepository reports a directory that git does not consider part of
	// a repository.
	ErrNotRepository = errors.New("not a git repository")
	// ErrNoWorkTree reports a bare repository. Snapshots are of a working
	// tree; there is nothing here to snapshot.
	ErrNoWorkTree = errors.New("repository has no work tree")
	// ErrIndexNotIsolated reports that a git command that can write an index
	// was about to run without GIT_INDEX_FILE pointing at kopicode's throwaway
	// index. It is the guard on the promise this package exists to keep, and
	// it fails the operation rather than risking the user's staged work.
	ErrIndexNotIsolated = errors.New("git index is not isolated from the user's")
	// ErrInvalidSessionID reports a session id that cannot safely become a
	// path component and a git ref component.
	ErrInvalidSessionID = errors.New("invalid session id")
	// ErrSessionExists reports shadow refs already present for this session
	// id. Continuing would fork the snapshot chain in place; resuming a
	// session's chain is slice 2.
	ErrSessionExists = errors.New("session already has snapshots")
	// ErrTurnNotIncreasing reports a snapshot for a turn at or before the one
	// already snapshotted. The chain is ordered, and a repeated turn would
	// leave two commits claiming the same ref with the earlier one orphaned.
	ErrTurnNotIncreasing = errors.New("turn does not increase")
)

// A Repo is a resolved git repository with a work tree: the three paths every
// later operation needs, worked out once.
//
// The three are genuinely different in a linked worktree — the case the bench
// runner creates once per task, so it is a first-class case here and not an
// edge one. There, .git is a *file* containing a gitdir: pointer, GitDir is
// .git/worktrees/<name> inside the main repository, and CommonDir is the shared
// .git. Asking git for all three rather than parsing the .git file is what
// keeps that from mattering anywhere else.
type Repo struct {
	root      string
	gitDir    string
	commonDir string
}

// Open resolves the repository containing dir.
//
// dir may be any directory inside the work tree; git walks up. The resolution
// is one `git rev-parse`, which handles .git-as-a-file, submodules and
// GIT_DIR-less discovery correctly and costs one subprocess per session.
//
// The environment handed to git has the GIT_DIR / GIT_WORK_TREE family removed
// (see [baseEnv]), so an inherited variable from some unrelated parent process
// cannot silently redirect a session's snapshots at another repository. What is
// resolved is the repository containing dir, always.
func Open(ctx context.Context, dir string) (*Repo, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, fmt.Errorf("repo: resolving %s: %w", dir, err)
	}

	// One call, three answers, in the order the flags are given.
	// --show-toplevel and --absolute-git-dir are already absolute;
	// --git-common-dir is relative to the process's directory, which is why it
	// is joined against the directory the command ran in rather than trusted.
	out, err := runGit(ctx, abs, baseEnv(), "rev-parse",
		"--show-toplevel", "--absolute-git-dir", "--git-common-dir")
	if err != nil {
		return nil, classifyOpenError(ctx, abs, err)
	}

	lines := splitLines(out)
	if len(lines) != 3 {
		return nil, fmt.Errorf("repo: git rev-parse in %s returned %d lines, want 3: %q",
			abs, len(lines), out)
	}

	common := lines[2]
	if !filepath.IsAbs(common) {
		common = filepath.Join(abs, common)
	}

	return &Repo{
		root:      filepath.Clean(lines[0]),
		gitDir:    filepath.Clean(lines[1]),
		commonDir: filepath.Clean(common),
	}, nil
}

// classifyOpenError turns rev-parse's failure into a sentinel a caller can act
// on. A bare repository and a directory outside any repository both exit 128
// with only prose to tell them apart, so the distinction is drawn by asking git
// a second question rather than by matching on the wording of the first
// answer's stderr — which changes between git versions and locales.
func classifyOpenError(ctx context.Context, dir string, cause error) error {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return cause
	}
	if out, err := runGit(ctx, dir, baseEnv(), "rev-parse", "--is-bare-repository"); err == nil {
		if strings.TrimSpace(out) == "true" {
			return fmt.Errorf("repo: %s: %w: snapshots need a working tree: %w",
				dir, ErrNoWorkTree, cause)
		}
		// A repository with a work tree whose rev-parse still failed is
		// something else entirely; hand back what git said.
		return fmt.Errorf("repo: resolving the repository at %s: %w", dir, cause)
	}
	return fmt.Errorf("repo: %s: %w: %w", dir, ErrNotRepository, cause)
}

// Root is the work tree root: the directory snapshots are taken of, and the
// directory StateDir sits in.
func (r *Repo) Root() string { return r.root }

// WorkTreeRoot is the work tree root containing dir, or dir made absolute when
// dir is not inside a repository with a work tree.
//
// It exists for the session lock (docs/SLICE-1.md §8, internal/lock), which has
// to name one directory per *working tree* and cannot fail just because the
// session is not in a repository. Running kopicode outside git is supported —
// no snapshots, no head on the record — and so is running it in a subdirectory,
// where the whole tree is still what a snapshot covers and therefore what a
// second session would collide over. Both callers want the same answer for both
// cases, which is why the fallback is here rather than repeated at each.
//
// It never returns an error. A directory that git cannot resolve is a working
// tree of one directory as far as the lock is concerned, and a session there
// still excludes a second session in the same place.
func WorkTreeRoot(ctx context.Context, dir string) string {
	if r, err := Open(ctx, dir); err == nil {
		return r.Root()
	}
	if abs, err := filepath.Abs(dir); err == nil {
		return abs
	}
	return dir
}

// GitDir is this work tree's git directory. In a linked worktree that is
// .git/worktrees/<name>, not the shared .git.
func (r *Repo) GitDir() string { return r.gitDir }

// CommonDir is the shared git directory: where objects, refs and info/exclude
// actually live. In the main worktree it equals GitDir.
func (r *Repo) CommonDir() string { return r.commonDir }

// StatePath is the absolute path of kopicode's state directory for this work
// tree.
func (r *Repo) StatePath() string { return filepath.Join(r.root, StateDir) }

// Head is the commit HEAD points at, as a full sha.
//
// It is what journal.SessionStarted records as the tree the session ran
// against, and that is the only field tying a record to the code it was made
// over: the harness config hash deliberately excludes the repository, and the
// build identity describes the binary rather than the checkout.
//
// A repository with no commit yet has no head, and `rev-parse HEAD` fails
// there. Starting a session in one is an ordinary thing to do, so the error is
// returned plainly for the caller to read as "no head" rather than dressed up
// as a repository that could not be read.
//
// Read-only, and structurally so: rev-parse cannot write an index, and this
// goes through the guarded path that would refuse it if it could.
func (r *Repo) Head(ctx context.Context) (string, error) {
	out, err := r.git(ctx, "rev-parse", "HEAD")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(out), nil
}

// ExcludeStateDir makes git ignore .kopicode/ by appending a pattern to the
// repository's info/exclude, creating the file if it is absent.
//
// It is idempotent, and calling it is a precondition for snapshotting: the
// throwaway index and its .lock file live under .kopicode/, and `git add -A`
// would otherwise stage them into the very tree being snapshotted. That is not
// hypothetical — it is what the mechanism does when the exclude is missing.
//
// # info/exclude, not .gitignore
//
// .gitignore is a tracked file. Writing to it would show up in the user's
// `git status`, in their next commit, and in a diff they did not ask for.
// info/exclude is repository-local, untracked, and exactly the mechanism git
// documents for this. ADR-0002 §3 and CLAUDE.md both say so.
//
// # CommonDir, not GitDir
//
// In a linked worktree only $GIT_COMMON_DIR/info/exclude is read.
// A file at .git/worktrees/<name>/info/exclude is silently ignored — verified
// against git 2.34, where a pattern written there left the matching file still
// untracked. Writing to GitDir here would therefore appear to work, hold in the
// main worktree, and fail only under the bench runner, which creates one linked
// worktree per task.
func (r *Repo) ExcludeStateDir() error {
	dir := filepath.Join(r.commonDir, "info")
	path := filepath.Join(dir, "exclude")

	existing, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("repo: reading %s: %w", path, err)
	}
	for _, line := range splitLines(string(existing)) {
		if strings.TrimSpace(line) == excludePattern {
			return nil
		}
	}

	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("repo: creating %s: %w", dir, err)
	}

	var b strings.Builder
	b.Write(existing)
	// An exclude file that does not end in a newline would otherwise get the
	// comment spliced onto its last pattern, silently changing that pattern.
	if len(existing) > 0 && !strings.HasSuffix(string(existing), "\n") {
		b.WriteString("\n")
	}
	b.WriteString(excludeComment + "\n" + excludePattern + "\n")

	// Rewritten whole rather than appended to, so a failure part-way leaves the
	// original file rather than a half-written pattern that changes what git
	// ignores.
	if err := writeFileAtomic(path, []byte(b.String()), 0o644); err != nil {
		return fmt.Errorf("repo: adding %s to %s: %w", excludePattern, path, err)
	}
	return nil
}

// writeFileAtomic replaces path via a temporary file and a rename.
func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".kopicode-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer func() { _ = os.Remove(name) }() // no-op once the rename has happened

	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Chmod(perm); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(name, path)
}

// splitLines splits on newlines and drops the empty tail a trailing newline
// leaves behind, so callers do not each re-derive it.
func splitLines(s string) []string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.TrimSuffix(s, "\n")
	if s == "" {
		return nil
	}
	return strings.Split(s, "\n")
}
