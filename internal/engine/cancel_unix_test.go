//go:build unix

package engine_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/leejianrong/kopicode/internal/engine"
	"github.com/leejianrong/kopicode/internal/journal"
	"github.com/leejianrong/kopicode/internal/tools"
)

// spawnsAChild is the command the cancellation test runs.
//
// `sleep 300 &` starts a grandchild in the shell's own process group holding
// the same stdout pipe, and `wait` keeps the shell alive so there is a parent
// to kill. Kill the shell by pid and `sleep` is reparented to init and runs for
// five more minutes. Kill the *group* and both go. internal/tools proves this
// for run_shell called directly; what is proved here is that the engine's
// context reaches it, which is the card's third done-when clause.
const spawnsAChild = `sleep 300 & echo $! > child.pid; wait`

// TestCancellationMidTurnKillsTheShellProcessGroup is the card's third
// done-when clause, asserted on the process rather than on a return value.
//
// A test that only checked the error would pass just as happily while leaking a
// five-minute sleep per cancelled turn, and in a bench run that is a machine
// that slowly fills with orphans.
func TestCancellationMidTurnKillsTheShellProcessGroup(t *testing.T) {
	replies := []scriptedReply{{
		calls: []wireCall{nativeCall("call-shell", tools.ToolRunShell,
			`{"command":"`+spawnsAChild+`"}`)},
		usage: wireUsage{Prompt: 10, Completion: 5, Total: 15},
	}}
	h := scriptHarness(t, replies, oneAttemptPerTurn(1), nil, withMaxTurns(3))

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	type outcome struct {
		res engine.Result
		err error
	}
	done := make(chan outcome, 1)
	go func() {
		res, err := h.eng.Run(ctx, "run something long")
		done <- outcome{res, err}
	}()

	// The grandchild's pid is the only thing worth asserting on, so the test
	// waits for the command to have started one before cancelling.
	child := awaitChildPid(t, filepath.Join(h.root, "child.pid"))
	cancel()

	got := <-done
	if !errors.Is(got.err, context.Canceled) {
		t.Fatalf("Run = %v, want an error wrapping context.Canceled", got.err)
	}
	if got.res.Stop != engine.StopCancelled {
		t.Errorf("stop = %s, want %s", got.res.Stop, engine.StopCancelled)
	}

	assertGone(t, child, "the grandchild the cancelled command left behind")

	if err := h.eng.Close(ctx); err != nil {
		t.Fatalf("Close: %v", err)
	}

	evs := h.events()
	result := sole[journal.ToolResult](t, evs)
	if result.ErrorKind != tools.FaultCancelled.String() {
		t.Errorf("tool result error kind = %q, want %q — an internal error would book every "+
			"Ctrl-C as a harness failure, and a success would charge it to the model",
			result.ErrorKind, tools.FaultCancelled)
	}
	// The cancellation is on the record, which is what docs/SLICE-1.md's
	// "appends the cancellation to the journal" asks for on a turn that was
	// running a tool.
	if !strings.Contains(strings.ToLower(result.Output.Inline), "cancel") {
		t.Errorf("the tool result does not say the run was cancelled:\n%s", result.Output.Inline)
	}
	ended := sole[journal.SessionEnded](t, evs)
	if ended.Reason != "cancelled" {
		t.Errorf("session ended %q, want \"cancelled\"", ended.Reason)
	}
}

// awaitChildPid waits for the command to write its grandchild's pid.
func awaitChildPid(t *testing.T, path string) int {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		b, err := os.ReadFile(path)
		if err == nil {
			if pid, err := strconv.Atoi(strings.TrimSpace(string(b))); err == nil && pid > 0 {
				return pid
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("the command never wrote a pid to %s", path)
	return 0
}

// assertGone waits for a process to disappear, then fails if it has not.
//
// Signal 0 delivers nothing and reports whether the pid is still there, which
// is the only check that distinguishes a killed group from a returned function.
// A short poll rather than a single check, because the kill is asynchronous
// with respect to the caller's return.
func assertGone(t *testing.T, pid int, what string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if err := syscall.Kill(pid, 0); errors.Is(err, syscall.ESRCH) {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	// Do not leave it running for the rest of the suite, whatever the verdict.
	_ = syscall.Kill(-pid, syscall.SIGKILL)
	_ = syscall.Kill(pid, syscall.SIGKILL)
	t.Fatalf("%s (pid %d) is still running: the cancellation did not reach the process group", what, pid)
}
