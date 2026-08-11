//go:build !windows

package repo

import (
	"os/exec"
	"syscall"
)

// setProcessGroup puts the child in its own process group so the whole group
// can be signalled at once. Without it, killing a cancelled `git add` leaves
// any clean filter it spawned running against the working tree.
func setProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// killProcessGroup kills the group, not the pid. The negative pid is what makes
// the signal reach every descendant; os/exec's own cancellation would signal
// only the immediate child.
func killProcessGroup(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
}
