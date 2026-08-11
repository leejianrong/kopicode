//go:build unix

package tools_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/leejianrong/kopicode/internal/tools"
)

// spawnsAChild is the command the orphan tests run.
//
// `sleep 300 &` starts a grandchild in the shell's process group holding the
// same stdout pipe, and `wait` keeps the shell alive so there is a parent to
// kill. Kill the shell by pid and `sleep` is reparented to init and keeps
// running for five minutes — the orphan KAN-782 exists to prevent. Kill the
// *group* and both go.
//
// The grandchild's pid is written to a file because that is the only way to
// check the thing being claimed. Asserting on the returned error, or even on
// the shell being gone, passes just as happily while leaking.
const spawnsAChild = `sleep 300 & echo $! > child.pid; wait`

// spawnsAChildIgnoringTERM is the same, with SIGTERM ignored. An ignored
// disposition survives fork and exec, so the grandchild ignores it too and only
// SIGKILL ends either of them.
const spawnsAChildIgnoringTERM = `trap '' TERM; ` + spawnsAChild

type shellRun struct {
	res tools.ShellResult
	err error
}

// startRun runs one command on another goroutine so the test can act while it
// is in flight — fire the clock, or cancel the context — and assert on the main
// goroutine where t.Fatal is legal.
func startRun(t *testing.T, ctx context.Context, s *tools.Set, req tools.ShellRequest) <-chan shellRun {
	t.Helper()
	done := make(chan shellRun, 1)
	go func() {
		res, err := s.RunShell(ctx, req)
		done <- shellRun{res, err}
	}()
	return done
}

// TestRunShellTimeoutKillsTheWholeProcessGroup is one half of the card's
// acceptance criterion.
func TestRunShellTimeoutKillsTheWholeProcessGroup(t *testing.T) {
	f := newFixture(t, nil)
	s := f.set(t)
	clk := newFakeClock()
	s.Clock = clk

	done := startRun(t, t.Context(), s, tools.ShellRequest{Command: spawnsAChild})

	child := awaitChildPid(t, filepath.Join(f.root, "child.pid"))
	armed := clk.fire(t)

	got := <-done
	if got.err != nil {
		t.Fatalf("RunShell: %v", got.err)
	}
	// A timeout is a result, not an error: the caller needs the partial output
	// and the exit information, and an error return invites dropping both.
	if got.res.Outcome != tools.OutcomeTimedOut {
		t.Errorf("outcome = %s, want %s", got.res.Outcome, tools.OutcomeTimedOut)
	}
	if got.res.StoppedBy != "SIGTERM" {
		t.Errorf("stopped by %q, want SIGTERM — the graceful signal comes first", got.res.StoppedBy)
	}
	if got.res.ExitCode != -1 {
		t.Errorf("exit code = %d, want -1: a killed process never produced one", got.res.ExitCode)
	}
	if armed != 120*time.Second {
		t.Errorf("timeout armed for %s, want the documented 120s default", armed)
	}
	if got.res.Timeout != 120*time.Second {
		t.Errorf("result timeout = %s, want 120s", got.res.Timeout)
	}
	// Asserted off the injected clock rather than waited for: the run really
	// did stop at its timeout, and the suite spent no wall-clock time on it.
	if got.res.Duration != armed {
		t.Errorf("duration = %s, want the armed %s", got.res.Duration, armed)
	}
	if !strings.Contains(got.res.Output, "timed out") {
		t.Errorf("output does not say it timed out:\n%s", got.res.Output)
	}

	assertGone(t, child, "the grandchild the timed-out command left behind")
}

// TestRunShellCancellationKillsTheWholeProcessGroup is the other half. The
// clock is real here: nothing is meant to time out, and the kill has to come
// from the context alone.
func TestRunShellCancellationKillsTheWholeProcessGroup(t *testing.T) {
	f := newFixture(t, nil)
	s := f.set(t)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := startRun(t, ctx, s, tools.ShellRequest{Command: spawnsAChild})

	child := awaitChildPid(t, filepath.Join(f.root, "child.pid"))
	cancel()

	got := <-done
	if got.err != nil {
		t.Fatalf("RunShell: %v", got.err)
	}
	// A cancellation is nobody's failure. Returning it as an internal fault
	// would put every Ctrl-C into ADR-0006's harness bucket.
	if got.res.Outcome != tools.OutcomeCancelled {
		t.Errorf("outcome = %s, want %s", got.res.Outcome, tools.OutcomeCancelled)
	}
	if got.res.StoppedBy != "SIGTERM" {
		t.Errorf("stopped by %q, want SIGTERM", got.res.StoppedBy)
	}

	assertGone(t, child, "the grandchild the cancelled command left behind")
}

// TestRunShellEscalatesToSIGKILL proves the second half of the two-phase stop.
// A graceful signal a process chooses to ignore is a hang, not a shutdown.
func TestRunShellEscalatesToSIGKILL(t *testing.T) {
	f := newFixture(t, nil)
	s := f.set(t)
	clk := newFakeClock()
	s.Clock = clk

	done := startRun(t, t.Context(), s, tools.ShellRequest{Command: spawnsAChildIgnoringTERM})

	child := awaitChildPid(t, filepath.Join(f.root, "child.pid"))
	timeout := clk.fire(t)
	// The grace timer cannot exist until the group has been signalled, so
	// reaching this at all is evidence SIGTERM was tried first.
	grace := clk.fire(t)

	got := <-done
	if got.err != nil {
		t.Fatalf("RunShell: %v", got.err)
	}
	if got.res.StoppedBy != "SIGKILL" {
		t.Errorf("stopped by %q, want SIGKILL after the grace period", got.res.StoppedBy)
	}
	if got.res.Signal != "SIGKILL" {
		t.Errorf("signal = %q, want SIGKILL", got.res.Signal)
	}
	if grace != 5*time.Second {
		t.Errorf("grace armed for %s, want the documented 5s", grace)
	}
	if want := timeout + grace; got.res.Duration != want {
		t.Errorf("duration = %s, want %s — the grace period was not waited out", got.res.Duration, want)
	}

	assertGone(t, child, "the grandchild that ignored SIGTERM")
}

func TestRunShellCapturesBothStreamsAndTheExitCode(t *testing.T) {
	f := newFixture(t, nil)
	s := f.set(t)

	res, err := s.RunShell(t.Context(), tools.ShellRequest{
		Command: `printf 'to stdout\n'; printf 'to stderr\n' >&2; exit 3`,
	})
	if err != nil {
		t.Fatalf("RunShell: %v", err)
	}
	// A command that fails is a result, not a harness fault. Getting this
	// backwards puts a failed build in ADR-0006's `internal` bucket.
	if tools.FaultOf(err) != tools.FaultNone {
		t.Errorf("fault = %s, want none", tools.FaultOf(err))
	}
	if res.Outcome != tools.OutcomeExited {
		t.Errorf("outcome = %s, want %s", res.Outcome, tools.OutcomeExited)
	}
	if res.ExitCode != 3 {
		t.Errorf("exit code = %d, want 3", res.ExitCode)
	}
	if res.Signal != "" || res.StoppedBy != "" {
		t.Errorf("signal = %q, stopped by = %q; want both empty for a clean exit", res.Signal, res.StoppedBy)
	}
	if res.Stdout != "to stdout\n" {
		t.Errorf("stdout = %q", res.Stdout)
	}
	if res.Stderr != "to stderr\n" {
		t.Errorf("stderr = %q", res.Stderr)
	}
	for _, want := range []string{"exited 3", "--- stdout (10 bytes) ---", "to stderr", "--- stderr (10 bytes) ---"} {
		if !strings.Contains(res.Output, want) {
			t.Errorf("output missing %q:\n%s", want, res.Output)
		}
	}
}

// TestRunShellDoesNotTruncateOutput is the CLAUDE.md rule in test form:
// clipping the diagnostic output that justifies a fix is the specific failure
// being designed out, so run_shell declares no bound on either stream.
func TestRunShellDoesNotTruncateOutput(t *testing.T) {
	f := newFixture(t, nil)
	s := f.set(t)

	const lines = 20000
	res, err := s.RunShell(t.Context(), tools.ShellRequest{
		Command: "yes 0123456789 | head -n " + strconv.Itoa(lines),
	})
	if err != nil {
		t.Fatalf("RunShell: %v", err)
	}
	if want := lines * len("0123456789\n"); len(res.Stdout) != want {
		t.Errorf("captured %d bytes of stdout, want %d — output was clipped", len(res.Stdout), want)
	}
}

func TestRunShellRunsInTheRequestedDirectoryWithoutTheAPIKey(t *testing.T) {
	f := newFixture(t, map[string]string{"sub/keep.txt": "x\n"})
	s := f.set(t)

	// A value that is obviously not a credential: the point is that it does not
	// reach the child, and a realistic-looking one would only upset gitleaks.
	t.Setenv("OPENROUTER_API_KEY", "not-a-real-key-for-a-test")

	res, err := s.RunShell(t.Context(), tools.ShellRequest{
		Dir:     "sub",
		Command: `pwd; echo "key=[$OPENROUTER_API_KEY]"; echo "path=[$PATH]"`,
	})
	if err != nil {
		t.Fatalf("RunShell: %v", err)
	}
	out := lines(res.Stdout)
	if len(out) != 3 {
		t.Fatalf("stdout = %q, want three lines", res.Stdout)
	}
	if want := filepath.Join(f.root, "sub"); out[0] != want {
		t.Errorf("working directory = %q, want %q", out[0], want)
	}
	if out[1] != "key=[]" {
		t.Errorf("the child saw the provider key: %q", out[1])
	}
	if strings.Contains(res.Output, "not-a-real-key") {
		t.Errorf("the key reached the tool output:\n%s", res.Output)
	}
	// The rest of the environment is inherited: a curated allowlist would break
	// real builds, and run_shell is not a sandbox.
	if out[2] == "path=[]" {
		t.Errorf("the child inherited no PATH: %q", out[2])
	}
	if res.Dir != "sub" {
		t.Errorf("result dir = %q, want %q", res.Dir, "sub")
	}
}

// TestRunShellReportsAKillFromOutside keeps "killed by a signal" distinct from
// "exited non-zero". A test suite that was OOM-killed did not fail its task.
func TestRunShellReportsAKillFromOutside(t *testing.T) {
	f := newFixture(t, nil)
	s := f.set(t)

	res, err := s.RunShell(t.Context(), tools.ShellRequest{Command: `kill -9 $$`})
	if err != nil {
		t.Fatalf("RunShell: %v", err)
	}
	if res.Outcome != tools.OutcomeSignalled {
		t.Errorf("outcome = %s, want %s", res.Outcome, tools.OutcomeSignalled)
	}
	if res.Signal != "SIGKILL" {
		t.Errorf("signal = %q, want SIGKILL", res.Signal)
	}
	if res.StoppedBy != "" {
		t.Errorf("stopped by = %q, want empty: this tool sent nothing", res.StoppedBy)
	}
	if res.ExitCode != -1 {
		t.Errorf("exit code = %d, want -1", res.ExitCode)
	}
	if !strings.Contains(res.Output, "killed by SIGKILL") {
		t.Errorf("output does not name the signal:\n%s", res.Output)
	}
}

// TestRunShellReportsADetachedSurvivorRatherThanHanging covers the one case a
// process group cannot close, honestly.
//
// The command exits straight away and leaves a background child holding its
// stdout pipe. Nothing here kills that child — it was not a timeout and not a
// cancellation, and group-signalling a pid that has already been reaped is how
// a tool kills a stranger. So the run returns, bounded by ShellGrace, and says
// what it left behind instead of waiting on a pipe forever.
func TestRunShellReportsADetachedSurvivorRatherThanHanging(t *testing.T) {
	f := newFixture(t, nil)
	s := f.set(t)
	s.Limits.ShellGrace = 200 * time.Millisecond

	res, err := s.RunShell(t.Context(), tools.ShellRequest{
		Command: `sleep 30 & echo $! > child.pid`,
	})
	if err != nil {
		t.Fatalf("RunShell: %v", err)
	}
	child := awaitChildPid(t, filepath.Join(f.root, "child.pid"))
	t.Cleanup(func() { _ = syscall.Kill(child, syscall.SIGKILL) })

	if res.ExitCode != 0 {
		t.Errorf("exit code = %d, want 0", res.ExitCode)
	}
	if !res.LeftRunning {
		t.Error("LeftRunning = false; the survivor was not reported")
	}
	if !strings.Contains(res.Output, "outlived it") {
		t.Errorf("output does not mention the survivor:\n%s", res.Output)
	}
}

// awaitChildPid waits for the command to have recorded its background child's
// pid.
//
// The file is written as the command's second statement, so this is a wait of
// milliseconds; the deadline is generous because the failure it guards against
// is the test killing the run before there is a grandchild to orphan, which
// would make every assertion below pass for the wrong reason. Polling is the
// honest instrument here — there is no channel to wait on, the writer is
// another process — and the alternative of sleeping a fixed interval is both
// slower and flakier.
func awaitChildPid(t *testing.T, path string) int {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		if b, err := os.ReadFile(path); err == nil {
			if pid, cerr := strconv.Atoi(strings.TrimSpace(string(b))); cerr == nil && pid > 1 {
				return pid
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("no child pid was recorded at %s within 10s", path)
		}
		time.Sleep(time.Millisecond)
	}
}

// assertGone waits for a process to disappear, and says what is still running
// when it does not.
//
// kill(pid, 0) asks "may I signal this process": a nil error means it is still
// there. Polling rather than waiting is not a shortcut — the process is not
// ours to wait on, because killing its parent is what reparented it to init in
// the first place. The deadline is generous because the claim being tested is
// "gone", not "gone within a millisecond".
func assertGone(t *testing.T, pid int, what string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if err := syscall.Kill(pid, 0); err != nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s (pid %d) is still running 5s after the kill — "+
				"the signal went to the process, not to the group\n%s", what, pid, psLine(pid))
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// psLine is the evidence in a failure message: whatever ps can still say about
// a process that was supposed to be gone.
func psLine(pid int) string {
	out, err := exec.Command("ps", "-o", "pid=,ppid=,stat=,args=", "-p", strconv.Itoa(pid)).Output()
	if err != nil || len(strings.TrimSpace(string(out))) == 0 {
		return "  (ps had nothing to say about it)"
	}
	return "  survivor: " + strings.TrimSpace(string(out))
}
