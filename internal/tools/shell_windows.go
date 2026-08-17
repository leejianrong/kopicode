//go:build windows

package tools

import (
	"os/exec"
	"strings"
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
	//kopicode:allow-noenv: same seam. childEnv is assembled in shell.go and assigned
	// there, because what a shell may see is a policy question and this file is not
	// where policy lives.
	cmd := exec.Command("cmd")
	cmd.SysProcAttr = &syscall.SysProcAttr{CmdLine: strings.Join(ShellArgv(command), " ")}
	procgroup.Isolate(cmd)
	return cmd
}

// ShellArgv is the argv run_shell will execute for command.
//
// It exists so that the engine's permission request and the process that
// actually runs describe the same thing. permission.Action.Command is argv and
// not a shell string on purpose — "what was consented to is unambiguous on
// replay" — and the engine cannot build that argv without knowing which shell
// this package picked. Spelling `cmd /c` a second time in the engine would make
// the consent record and the process disagree the first time either side
// changed, which is a consent for something that did not run.
//
// On Windows the argv is descriptive rather than executed as a vector: the
// command line is handed to cmd.exe as one string for the reason above, and
// this is that string split at the two points that are not part of the model's
// command.
func ShellArgv(command string) []string { return []string{"cmd", "/c", command} }
