package bench

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/leejianrong/kopicode/internal/procgroup"
)

// gitWaitDelay bounds how long Wait blocks after a cancelled git command's
// process group has been killed, waiting on a grandchild that inherited the
// output pipe. The same reasoning as internal/repo's: a clean or smudge filter
// that daemonises is the realistic case, and without this a cancelled run hangs
// on it instead of reclaiming its worktrees.
const gitWaitDelay = 2 * time.Second

// gitEnvOverrides are the variables removed from every environment this package
// hands git.
//
// Every one of them redirects git at a repository, an object store, an index or
// an identity other than the one the command's working directory implies, and
// GIT_DIR does it while the command still reads perfectly. That is not
// hypothetical here: a `git worktree add` under an inherited GIT_DIR registers a
// worktree against a repository nobody chose, and this package's whole subject
// is worktrees.
//
// The list is internal/repo's, deliberately duplicated rather than shared. The
// two are separate builders because internal/arch/subprocess_test.go's value
// check funnels every subprocess into a *named* builder in its own package
// whose doc comment says what it strips — a single shared one would need a
// passthrough option, and a passthrough option is the hole this guard closes,
// with a name. Drift between the two lists is bounded by the fact that both are
// the same fixed set git documents.
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

// gitEnv is the process environment with [gitEnvOverrides] stripped: what every
// git command in this package runs under.
//
// The user's git *configuration* is deliberately left alone — a worktree
// checked out under different clean filters or line-ending rules would not be
// the tree the corpus was frozen as, and the corpus digest is verified inside
// the worktree afterwards, so a configuration that mangled it fails loudly
// rather than silently producing a different benchmark.
func gitEnv() []string {
	src := os.Environ()
	out := make([]string, 0, len(src))
	for _, kv := range src {
		if !gitOverridden(kv) {
			out = append(out, kv)
		}
	}
	return out
}

func gitOverridden(kv string) bool {
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

// GitError carries what a failed git subprocess actually said. A worktree that
// could not be created or removed is the failure this card exists to make
// visible, so the message has to name the ref, the path or the lock git
// complained about rather than reporting "git failed".
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
// dir is required. An empty one means os/exec uses the calling process's
// working directory, which for kopibench is wherever the user happened to be —
// and git then walks upward until it finds *a* repository. Every call here has
// a resolved directory available, so an empty one is a bug rather than a
// default.
func runGit(ctx context.Context, dir string, args ...string) (string, error) {
	if dir == "" {
		return "", fmt.Errorf("bench: git %s: no working directory: %w",
			strings.Join(args, " "), ErrNoWorkingDir)
	}

	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	cmd.Env = gitEnv()

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	// The group, not the pid: git spawns helpers — clean and smudge filters,
	// credential helpers — and killing only git leaves them holding the
	// worktree this package is about to remove.
	procgroup.Isolate(cmd)
	cmd.Cancel = func() error { _, err := procgroup.Kill(cmd); return err }
	cmd.WaitDelay = gitWaitDelay

	if err := cmd.Run(); err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return "", fmt.Errorf("bench: git %s: %w", strings.Join(args, " "), ctxErr)
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

// gitLines splits git output into non-empty lines.
func gitLines(out string) []string {
	out = strings.TrimRight(out, "\n")
	if out == "" {
		return nil
	}
	return strings.Split(out, "\n")
}
