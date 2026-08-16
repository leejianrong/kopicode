package bench

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"slices"
	"time"

	"github.com/leejianrong/kopicode/internal/corpus"
	"github.com/leejianrong/kopicode/internal/procgroup"
)

// oracleGrace is how long a timed-out oracle's process group has to exit before
// it is killed. A `go test` given the chance to exit removes its own temporary
// files and reaps its own compile children; one that ignores the chance is not
// shutting down, so the grace period is bounded and the kill is not optional.
const oracleGrace = 5 * time.Second

// OracleResult is what the task's own test suite said.
//
// ADR-0005 decision 5 makes the oracle a unit-test suite rather than a judge,
// so the verdict is an exit status and nothing else. Everything beside Passed
// is here for the report and the post-mortem — and Output is never clipped,
// because clipping the diagnostic output that justifies a fix, exactly where a
// reviewer looks, is the specific failure this project designs out.
type OracleResult struct {
	// Argv is the command that ran, as the manifest declared it.
	Argv []string
	// Passed is the whole verdict: the oracle exited zero.
	Passed bool
	// ExitCode is the oracle's status, or -1 when it was signalled or never
	// started.
	ExitCode int
	// Signal names the signal that ended it, empty otherwise. A suite that was
	// killed is not a suite that failed, and a classifier reads the difference.
	Signal string
	// TimedOut reports that the oracle outlived its manifest timeout. It is
	// distinct from a failure for the same reason: nothing was measured.
	TimedOut bool
	// Duration is how long it ran.
	Duration time.Duration
	// Output is stdout and stderr interleaved, whole.
	Output string
	// Err is a failure to run the oracle at all — a missing executable, a
	// directory that is not there. Distinct from a non-zero exit, which is an
	// answer.
	Err error
}

// runOracle runs the task's oracle in dir and reports what it said.
//
// Three things are decided here and nowhere else. The **timeout** is the
// manifest's, enforced by the context rather than trusted to the command. The
// **process group** is the oracle's own, so a timeout reaches the compile and
// test children a `go test` spawns rather than leaving them running — in a
// bench run, orphans are how a machine fills up. And the **environment** is
// built by [oracleEnv] rather than inherited, because an ambient GOFLAGS,
// PYTHONPATH or GOPROXY changes what the oracle measures without changing
// anything visible in the command.
func runOracle(ctx context.Context, task corpus.Task, dir, home string, caches goCaches, now func() time.Time) OracleResult {
	res := OracleResult{Argv: append([]string(nil), task.Oracle.Argv...), ExitCode: -1}
	if len(task.Oracle.Argv) == 0 {
		res.Err = fmt.Errorf("bench: task %s has no oracle command", task.ID)
		return res
	}

	timeout := time.Duration(task.Oracle.TimeoutSeconds) * time.Second
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, task.Oracle.Argv[0], task.Oracle.Argv[1:]...)
	cmd.Dir = dir
	cmd.Env = oracleEnv(task, home, caches)

	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out

	procgroup.Isolate(cmd)
	cmd.Cancel = func() error { _, err := procgroup.Kill(cmd); return err }
	// A backstop for a descendant that left the group and still holds the
	// output pipe: without it a finished oracle becomes a hung run.
	cmd.WaitDelay = oracleGrace

	started := now()
	err := cmd.Run()
	res.Duration = now().Sub(started)
	res.Output = out.String()

	if cmd.ProcessState != nil {
		res.ExitCode, res.Signal = procgroup.ExitStatus(cmd.ProcessState)
	}

	switch {
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		res.TimedOut = true
		res.Err = fmt.Errorf("bench: the oracle for %s did not finish within %s", task.ID, timeout)
	case err == nil:
		res.Passed = true
	case isExitError(err):
		// A non-zero exit is the oracle's answer, not a failure to consult it.
	default:
		res.Err = fmt.Errorf("bench: running the oracle for %s: %w", task.ID, err)
	}
	return res
}

func isExitError(err error) bool {
	var exit *exec.ExitError
	return errors.As(err, &exit)
}

// oraclePassThrough are the variables a toolchain needs to find itself and its
// caches. Everything else is dropped.
//
// It is the allowlist internal/corpus's integration test uses, and it is here
// rather than shared with it because that one lives in a _test.go file behind
// the integration tag and is not importable — and because
// internal/arch/subprocess_test.go's value check funnels each package into a
// *named* builder of its own whose doc comment says what it admits. HOME is
// deliberately absent from the list: it is supplied per task by the caller, so
// an oracle writes its dotfiles into a temp directory rather than the
// developer's home.
var oraclePassThrough = []string{
	"PATH", "TMPDIR", "TEMP", "TMP",
	"GOROOT", "GOPATH", "GOCACHE", "GOMODCACHE",
	// Windows needs these to start a process at all.
	"SystemRoot", "USERPROFILE", "LOCALAPPDATA", "APPDATA", "ComSpec",
}

// oracleEnv is the environment one oracle runs under: the allowlist above, then
// the per-task temp HOME, then the manifest's own variables.
//
// The manifest goes last so a task's declared GOPROXY=off or
// PYTHONDONTWRITEBYTECODE=1 wins over anything inherited — those exist to
// remove variance, and an ambient value overriding them would put the variance
// back invisibly.
//
// # The temp HOME, and what it is honestly worth
//
// It stops the oracle writing to the developer's ~/.gitconfig, ~/.npmrc or
// ~/.cache. It is **not** a sandbox: the oracle is an argv from the corpus and
// the session before it ran model-authored shell in the same worktree, so this
// is containment of accidents, not of intent. Isolation is slice 3, and
// SLICE-1 §9 says so outright.
//
// GOCACHE and GOMODCACHE pass through from the ambient environment when they
// are set, and [resolveGoCaches] supplies them from `go env` when they are not.
// That is a deliberate exception to the temp HOME: a cold build cache per task
// would mean recompiling the standard library ten times, and the cache is
// content-addressed, so sharing it changes what the oracle costs rather than
// what it measures.
func oracleEnv(task corpus.Task, home string, caches goCaches) []string {
	env := make([]string, 0, len(oraclePassThrough)+len(task.Oracle.Env)+4)
	for _, name := range oraclePassThrough {
		if value, ok := os.LookupEnv(name); ok {
			env = append(env, name+"="+value)
		}
	}
	if home != "" {
		env = append(env, "HOME="+home, "USERPROFILE="+home)
	}
	// After HOME, so the resolved caches win over the ones the toolchain would
	// otherwise derive from a temp home, and before the manifest, so a task
	// that declares its own still does.
	if caches.Build != "" {
		env = append(env, "GOCACHE="+caches.Build)
	}
	if caches.Mod != "" {
		env = append(env, "GOMODCACHE="+caches.Mod)
	}
	// The manifest's variables live in a map, and Go randomises map iteration,
	// so they are sorted before they are appended. os/exec keeps the last
	// occurrence of a duplicate name, which makes the order load-bearing: an
	// environment that varied per process would put a difference into a run
	// that is supposed to be reproducible.
	names := make([]string, 0, len(task.Oracle.Env))
	for name := range task.Oracle.Env {
		names = append(names, name)
	}
	slices.Sort(names)
	for _, name := range names {
		env = append(env, name+"="+task.Oracle.Env[name])
	}
	return env
}
