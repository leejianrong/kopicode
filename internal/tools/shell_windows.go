//go:build windows

package tools

import (
	"os/exec"
	"syscall"

	"github.com/leejianrong/kopicode/internal/procgroup"
)

// newShellCmd builds the command for one run_shell call, in a new process
// group.
//
// The group, the tree kill and the honest account of how much weaker it is on
// Windows all live in internal/procgroup. What is decided here is only how the
// command line reaches cmd.exe.
//
// CmdLine is set rather than passing arguments, because Go quotes arguments for
// a program that parses them the way the C runtime does, and cmd.exe does not.
// A command line is handed to the shell exactly as written, which is the same
// promise the unix side makes with `sh -c`.
func newShellCmd(command string) *exec.Cmd {
	//kopicode:allow-nodir: this builds the command and does not run it. The working
	// directory is the permission-resolved workspace path, which only the caller has —
	// runShell in shell.go assigns it on the returned Cmd, before Start.
	//kopicode:allow-noenv: same seam. childEnv() is assembled in shell.go and assigned
	// there, because what a shell may see is a policy question and this file is not
	// where policy lives.
	cmd := exec.Command("cmd")
	cmd.SysProcAttr = &syscall.SysProcAttr{CmdLine: "cmd /c " + command}
	procgroup.Isolate(cmd)
	return cmd
}
