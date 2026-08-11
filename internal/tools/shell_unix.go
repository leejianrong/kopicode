//go:build unix

package tools

import (
	"os/exec"

	"github.com/leejianrong/kopicode/internal/procgroup"
)

// newShellCmd builds the command for one run_shell call, already in a process
// group of its own.
//
// The group itself is internal/procgroup's business — see its package doc for
// why a group and not a pid, and why kopicode has exactly one implementation of
// it. What is decided here is only which shell runs the command.
//
// /bin/sh rather than $SHELL: POSIX puts it there, and a command recorded in
// the journal should mean the same thing when the session is replayed on
// another machine. A fish or a zsh with a user's rc file loaded would make the
// bench arm depend on whose laptop it ran on.
func newShellCmd(command string) *exec.Cmd {
	cmd := exec.Command("/bin/sh", "-c", command)
	procgroup.Isolate(cmd)
	return cmd
}
