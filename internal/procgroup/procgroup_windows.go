//go:build windows

package procgroup

// Windows is best-effort here, and the degradation is stated rather than
// implied — SLICE-1 risk 9 puts process groups, flock-style locking and shadow
// refs all in that bucket.
//
// What is the same: the command runs in a group of its own, a timeout and a
// cancellation both try to end the whole tree, and both report what they sent.
//
// What is weaker, and matters: unix's kill(-pgid) reaches every member of the
// group no matter who its parent is, because the group is a first-class kernel
// object. Windows has no equivalent signal. The closest thing is
// `taskkill /T`, which walks the *parent-child tree* — so a grandchild whose
// parent has already exited has lost the link taskkill follows and survives.
// The no-orphan guarantee holds on unix and is only a best effort here. A
// caller that can detect the shortfall reports it rather than hiding it:
// run_shell sets ShellResult.LeftRunning.
//
// A real fix is a Job Object with JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE, which
// kills by membership rather than by ancestry. That needs
// golang.org/x/sys/windows, and CLAUDE.md wants a reason in the PR before a
// dependency lands; until a Windows user asks for it the honest position is the
// one written here.

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"syscall"
)

// Isolate puts cmd in a new process group.
//
// An existing SysProcAttr is preserved rather than replaced: run_shell sets
// CmdLine on the same struct, and a helper that quietly dropped it would send
// cmd.exe a command line it never asked for.
func Isolate(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.CreationFlags |= syscall.CREATE_NEW_PROCESS_GROUP
}

// Terminate asks the tree to close.
//
// `taskkill /T` without /F is the closest thing to a SIGTERM available: it asks
// rather than forces, and a console process that ignores the request is dealt
// with by Kill once the grace period expires.
func Terminate(cmd *exec.Cmd) (string, error) {
	return taskkill(cmd, false)
}

// Kill ends the tree, falling back to the top process alone.
func Kill(cmd *exec.Cmd) (string, error) {
	label, err := taskkill(cmd, true)
	if err == nil {
		return label, nil
	}
	// taskkill is missing or refused. Killing the top process is all that is
	// left, and it is explicitly *not* the tree kill — the caller reports what
	// was sent, so this reads as the weaker thing it is.
	if cmd.Process == nil {
		return "", err
	}
	if kerr := cmd.Process.Kill(); kerr != nil && !errors.Is(kerr, os.ErrProcessDone) {
		return "", fmt.Errorf("killing process %d after taskkill failed (%v): %w",
			cmd.Process.Pid, err, kerr)
	}
	return "TerminateProcess, tree not reached", nil
}

// taskkill shells out to end the process tree rooted at cmd's process. Shelling
// out is the rule here, not a shortcut: the linking alternative is CGo or a new
// dependency, and CLAUDE.md rules out the first outright.
func taskkill(cmd *exec.Cmd, force bool) (string, error) {
	if cmd.Process == nil || cmd.Process.Pid <= 0 {
		return "", nil
	}

	args := []string{"/T", "/PID", strconv.Itoa(cmd.Process.Pid)}
	label := "taskkill /T"
	if force {
		args = append([]string{"/F"}, args...)
		label = "taskkill /T /F"
	}
	//kopicode:allow-nodir: taskkill is addressed to a pid, not to a tree. No directory
	// is the correct one, and the one it inherits changes nothing it does.
	//kopicode:allow-noenv: it has to be found on the ambient PATH — this is the kill
	// path, reached when a child has already outlived its deadline, and a built
	// environment that omitted PATH would turn a stuck process into an orphan.
	if err := exec.Command("taskkill", args...).Run(); err != nil {
		return "", fmt.Errorf("taskkill on process %d: %w", cmd.Process.Pid, err)
	}
	return label, nil
}

// ExitStatus reads a finished process's status. Windows has no signals, so a
// process only ever exits with a code and the signal half is always empty.
func ExitStatus(ps *os.ProcessState) (int, string) {
	return ps.ExitCode(), ""
}
