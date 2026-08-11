package repo

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

// waitDelay bounds how long Wait blocks after a cancelled command's process
// group has been killed, waiting on a grandchild that inherited the output
// pipe. A clean filter that daemonises is the realistic case; without this the
// engine's cancellation would hang on it.
const waitDelay = 2 * time.Second

// GitError carries what a failed git subprocess actually said.
//
// An error that reports only "git failed" turns every git problem into
// guesswork, and git puts the whole diagnosis on stderr: the ref that could not
// be locked, the path it could not read, the identity it could not auto-detect.
// Err is the underlying *exec.ExitError or *exec.Error, so errors.Is reaches
// exec.ErrNotFound and errors.As reaches the exit status.
type GitError struct {
	// Args is the argv after "git".
	Args []string
	// Dir is the directory the command ran in.
	Dir string
	// ExitCode is git's status, or -1 if it never ran.
	ExitCode int
	// Stderr is git's diagnosis, trailing newlines trimmed.
	Stderr string
	// Err is the underlying os/exec failure.
	Err error
}

func (e *GitError) Error() string {
	msg := fmt.Sprintf("git %s (in %s)", strings.Join(e.Args, " "), e.Dir)
	if e.ExitCode >= 0 {
		msg += fmt.Sprintf(": exit %d", e.ExitCode)
	}
	if e.Stderr != "" {
		return msg + ": " + e.Stderr
	}
	return msg + ": " + e.Err.Error()
}

func (e *GitError) Unwrap() error { return e.Err }

// runGit runs one git command to completion and returns its stdout.
//
// ctx is the first parameter and is never stored: it is the engine's
// cancellation, and a context parked in a struct is a context that outlives the
// operation it was meant to bound. On cancellation the whole process *group*
// is killed rather than the pid, because git spawns helpers — clean and smudge
// filters, credential helpers, pagers — and killing only git leaves them
// holding the working tree.
//
// env is passed in rather than inherited so the caller is the one that decides
// whether this command may see a GIT_INDEX_FILE. See [baseEnv] and
// [Snapshotter.env].
func runGit(ctx context.Context, dir string, env []string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	cmd.Env = env

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	setProcessGroup(cmd)
	cmd.Cancel = func() error { return killProcessGroup(cmd) }
	cmd.WaitDelay = waitDelay

	if err := cmd.Run(); err != nil {
		// A cancelled context is the engine doing its job, not a git fault.
		// Report it as itself so a caller's errors.Is(err, context.Canceled)
		// does not have to know that a subprocess was involved.
		if ctxErr := ctx.Err(); ctxErr != nil {
			return "", fmt.Errorf("repo: git %s: %w", strings.Join(args, " "), ctxErr)
		}
		ge := &GitError{
			Args:     append([]string(nil), args...),
			Dir:      dir,
			ExitCode: -1,
			Stderr:   strings.TrimRight(stderr.String(), "\n"),
			Err:      err,
		}
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			ge.ExitCode = exit.ExitCode()
		}
		return "", ge
	}
	return stdout.String(), nil
}

// gitEnvOverrides are the variables removed from every inherited environment
// before git runs.
//
// Each one redirects git at a repository, an object store, an index or an
// identity other than the one resolved here, and each is set by ordinary tools
// — a git hook, a rebase, an editor plugin — in ways this process cannot see.
// GIT_INDEX_FILE leads the list because inheriting it is precisely the accident
// this package's central promise is about; the isolated path sets its own and
// verifies it afterwards.
//
// GIT_CONFIG_GLOBAL and GIT_CONFIG_NOSYSTEM are deliberately *not* here. The
// user's git configuration governs how their files are read — line endings,
// clean filters, core.symlinks — and a snapshot taken under different rules
// would not describe their working tree.
var gitEnvOverrides = []string{
	"GIT_INDEX_FILE",
	"GIT_DIR",
	"GIT_WORK_TREE",
	"GIT_COMMON_DIR",
	"GIT_OBJECT_DIRECTORY",
	"GIT_ALTERNATE_OBJECT_DIRECTORIES",
	"GIT_NAMESPACE",
	"GIT_CEILING_DIRECTORIES",
	"GIT_LITERAL_PATHSPECS",
	"GIT_AUTHOR_NAME",
	"GIT_AUTHOR_EMAIL",
	"GIT_AUTHOR_DATE",
	"GIT_COMMITTER_NAME",
	"GIT_COMMITTER_EMAIL",
	"GIT_COMMITTER_DATE",
}

// baseEnv is the process environment with [gitEnvOverrides] stripped: what a
// git command that must not touch any index gets.
func baseEnv() []string {
	src := os.Environ()
	out := make([]string, 0, len(src))
	for _, kv := range src {
		if !isOverridden(kv) {
			out = append(out, kv)
		}
	}
	return out
}

func isOverridden(kv string) bool {
	name, _, ok := strings.Cut(kv, "=")
	if !ok {
		return false
	}
	for _, n := range gitEnvOverrides {
		if name == n {
			return true
		}
	}
	return false
}

// envValue returns the effective value of name in env.
//
// The *last* occurrence wins, matching both execve and os/exec's own
// deduplication. The guard that calls this must see what git will see, not what
// the first matching entry happens to say — a duplicate appended later is
// exactly how an override sneaks past a check that stops at the first hit.
func envValue(env []string, name string) (string, bool) {
	value, found := "", false
	for _, kv := range env {
		if k, v, ok := strings.Cut(kv, "="); ok && k == name {
			value, found = v, true
		}
	}
	return value, found
}

// indexWriting names the git subcommands that can write an index.
//
// It backs a refusal, not a warning: [Repo.git] will not run these at all,
// because the environment it builds has no GIT_INDEX_FILE and git would
// therefore write the user's real index. `status` is on the list because it
// refreshes and rewrites the index as a side effect of reporting — the least
// obvious entry and the one most likely to be added innocently by a later card.
//
// The list is deliberately generous. A subcommand named here that turns out to
// be harmless costs one compile error and a moment's thought; one missing costs
// somebody's staged work.
var indexWriting = map[string]bool{
	"add": true, "am": true, "apply": true, "checkout": true, "cherry-pick": true,
	"clean": true, "commit": true, "merge": true, "mv": true, "pull": true,
	"read-tree": true, "rebase": true, "reset": true, "restore": true,
	"revert": true, "rm": true, "stash": true, "status": true, "switch": true,
	"update-index": true, "write-tree": true,
}

// git runs a read-only git command against this repository.
//
// It refuses any subcommand that can write an index. That refusal is the
// structural half of this package's promise: the environment here has no
// GIT_INDEX_FILE, so a `git add` on this path would stage into the user's real
// index, and the only reliable defence against a future card adding one is that
// it does not compile into a working call. Index-writing commands go through
// [Snapshotter], which sets and verifies an isolated index.
func (r *Repo) git(ctx context.Context, args ...string) (string, error) {
	if len(args) > 0 && indexWriting[args[0]] {
		return "", fmt.Errorf(
			"repo: refusing to run `git %s` without an isolated index: %w",
			args[0], ErrIndexNotIsolated)
	}
	return runGit(ctx, r.root, baseEnv(), args...)
}
