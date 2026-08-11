//go:build windows

package repo

import "os/exec"

// Windows has no process groups in the POSIX sense. Job objects are the
// equivalent and are not wired up here; cancellation kills the git process
// alone and a spawned filter can outlive it. That is the documented
// best-effort Windows posture (docs/SLICE-1.md §Risks 9), stated rather than
// papered over, and it is the reason this file exists instead of a build tag
// that would have silently excluded the whole runner.
func setProcessGroup(*exec.Cmd) {}

func killProcessGroup(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	return cmd.Process.Kill()
}
