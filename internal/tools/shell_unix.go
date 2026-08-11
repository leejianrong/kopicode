//go:build unix

package tools

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"syscall"
)

// newShellCmd builds the command for one run_shell call, already in a process
// group of its own.
//
// Setpgid makes the shell a group leader, so its pid doubles as the group id
// and everything it spawns inherits that group. That is what lets one signal
// reach the whole tree. Without it the tree has no name, and the only thing
// that can be killed is the process we happen to hold a handle on.
//
// /bin/sh rather than $SHELL: POSIX puts it there, and a command recorded in
// the journal should mean the same thing when the session is replayed on
// another machine. A fish or a zsh with a user's rc file loaded would make the
// bench arm depend on whose laptop it ran on.
func newShellCmd(command string) *exec.Cmd {
	cmd := exec.Command("/bin/sh", "-c", command)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	return cmd
}

// terminateGroup asks the group to exit.
func terminateGroup(cmd *exec.Cmd) (string, error) {
	return signalGroup(cmd, syscall.SIGTERM)
}

// killGroup ends the group whether it agrees or not.
func killGroup(cmd *exec.Cmd) (string, error) {
	return signalGroup(cmd, syscall.SIGKILL)
}

// signalGroup sends sig to every process in the group cmd leads, and reports
// what it sent — or "" when the group had already gone.
//
// The negative pid is the mechanism the whole card rests on: kill(2) with a
// negative first argument signals every member of that process group, so a
// shell's backgrounded children are reached along with the shell.
func signalGroup(cmd *exec.Cmd, sig syscall.Signal) (string, error) {
	// A non-positive pid here would be catastrophic rather than merely wrong.
	// kill(0, sig) signals the caller's *own* group — the harness, and under
	// `go test` the test binary — and kill(-1, sig) signals every process the
	// user is permitted to signal. Neither is ever wanted, so the negation is
	// guarded rather than trusted.
	if cmd.Process == nil || cmd.Process.Pid <= 1 {
		return "", nil
	}

	err := syscall.Kill(-cmd.Process.Pid, sig)
	switch {
	case err == nil:
		return sigName(sig), nil
	case errors.Is(err, syscall.ESRCH):
		// Everything in the group exited between the decision to kill it and
		// the syscall. There is nothing to report and nothing to wait for.
		return "", nil
	default:
		return "", fmt.Errorf("signalling process group %d with %s: %w",
			cmd.Process.Pid, sigName(sig), err)
	}
}

// exitStatus reads a finished process's status.
//
// A process killed by a signal has no exit code, and reporting one anyway would
// erase the difference between a test suite that failed and a test suite that
// was killed — which is a distinction the bench classifier reads.
func exitStatus(ps *os.ProcessState) (int, string) {
	if ws, ok := ps.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
		return -1, sigName(ws.Signal())
	}
	return ps.ExitCode(), ""
}

// sigName renders a signal the way an operator writes it.
//
// syscall.Signal.String gives "terminated" and "killed", which read as prose
// and do not match what anyone greps for. The table is deliberately short —
// these are the signals a run_shell child plausibly dies of — and anything else
// falls through to a form that carries both the number and the description
// rather than pretending to a name it does not have.
func sigName(sig syscall.Signal) string {
	switch sig {
	case syscall.SIGHUP:
		return "SIGHUP"
	case syscall.SIGINT:
		return "SIGINT"
	case syscall.SIGQUIT:
		return "SIGQUIT"
	case syscall.SIGABRT:
		return "SIGABRT"
	case syscall.SIGKILL:
		return "SIGKILL"
	case syscall.SIGSEGV:
		return "SIGSEGV"
	case syscall.SIGPIPE:
		return "SIGPIPE"
	case syscall.SIGTERM:
		return "SIGTERM"
	default:
		return fmt.Sprintf("signal %d (%s)", int(sig), sig)
	}
}
