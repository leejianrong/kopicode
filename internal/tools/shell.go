package tools

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

// ShellRequest is one run_shell call.
type ShellRequest struct {
	// Command is a command line for the platform shell — `sh -c` on unix,
	// `cmd /c` on Windows. It is handed over whole and is never parsed here.
	Command string
	// Dir is the working directory the command starts in, relative to the
	// repository root or absolute. Empty means the root.
	Dir string
	// Timeout bounds the run. Zero means Limits.ShellTimeout.
	//
	// It is a request field and not a constant in the function because
	// SLICE-1's per-project config and a bench arm both want to vary it, and
	// two ways of expressing the same bound is how they drift.
	Timeout time.Duration
}

// Outcome says how a run ended.
//
// The four are distinct because the caller has to tell them apart and no two of
// them mean the same thing to a reader of the journal. A command that exited
// non-zero failed at its task. A command killed from outside was not given the
// chance to. A timeout is the harness deciding the command had had long enough,
// and a cancellation is the *user* deciding it — the same kill, a different
// reason, and only one of them says anything about the command.
type Outcome uint8

const (
	// OutcomeExited is a command that ran to completion. ExitCode says how it
	// went; non-zero is a result, not an error.
	OutcomeExited Outcome = iota

	// OutcomeSignalled is a command something else killed — the OOM killer, or
	// the command signalling itself. Nothing in this package decided it, which
	// is what separates it from the two below.
	OutcomeSignalled

	// OutcomeTimedOut is a command that outlived its timeout. The process group
	// was killed; StoppedBy says with what.
	OutcomeTimedOut

	// OutcomeCancelled is a command whose context was cancelled — Ctrl-C in the
	// REPL, or a bench task being abandoned. The process group was killed.
	OutcomeCancelled
)

var outcomeText = map[Outcome]string{
	OutcomeExited:    "exited",
	OutcomeSignalled: "signalled",
	OutcomeTimedOut:  "timed_out",
	OutcomeCancelled: "cancelled",
}

// String returns the wire form.
func (o Outcome) String() string {
	if s, ok := outcomeText[o]; ok {
		return s
	}
	return "unknown"
}

// ShellResult is what one run_shell call produced.
//
// It is a struct and not a rendered string because two of its fields are not
// prose: the engine puts ExitCode in journal.ToolResult.ExitCode, and Outcome
// is what tells a cancelled turn apart from a hung command. Output is the
// model-facing rendering of the rest.
type ShellResult struct {
	// Command is the command line as it was given.
	Command string
	// Dir is the working directory, as a slash path relative to the root.
	Dir string
	// Timeout is the bound that was in force, whether requested or default.
	Timeout time.Duration

	// Outcome says how the run ended.
	Outcome Outcome
	// ExitCode is the process's status, or -1 when it never produced one —
	// killed by a signal, timed out, or cancelled.
	ExitCode int
	// Signal names the signal that ended the process as the OS reported it,
	// "" when none did. On unix a timed-out run reports the signal this tool
	// sent; on Windows it is always "".
	Signal string
	// StoppedBy names what this tool sent to end the run, "" when it sent
	// nothing. It is how a reader tells a SIGTERM that was enough from a
	// SIGKILL that was needed, and it is separate from Signal because Windows
	// has a mechanism rather than a signal.
	StoppedBy string
	// LeftRunning reports that something the command started outlived it and
	// still held its output. It is the honest name for the case a process group
	// cannot cover: a descendant that detached from the group.
	LeftRunning bool

	// Stdout and Stderr are the captured streams, whole. Neither is truncated
	// and neither is bounded — CLAUDE.md's rule is that clipping the diagnostic
	// output which justifies a fix is the specific failure being designed out,
	// and the journal's blob spill is what handles size.
	Stdout string
	Stderr string
	// Duration is wall-clock time from start to reap, off the injected clock.
	Duration time.Duration

	// Output is the model-facing rendering of everything above.
	Output string
}

// apiKeyEnv is the one variable a shell command never inherits.
const apiKeyEnv = "OPENROUTER_API_KEY"

// RunShell runs one command line in its own process group and returns what it
// produced.
//
// **The process group is the point.** The command is started with its own group
// id, and a timeout or a cancellation signals the *negative* pid so the signal
// reaches everything the command spawned. Killing by pid instead leaves
// `sh -c 'sleep 300 & wait'` with `sleep` reparented to init and running, which
// is an orphan holding a pipe and, in a bench run, a machine that slowly fills
// with them.
//
// Termination is SIGTERM, then SIGKILL after Limits.ShellGrace. A compiler or a
// test runner given the chance to exit removes its own temporary files and
// reaps its own children; a graceful signal it chooses to ignore is not a
// shutdown, so the grace period is bounded and the forceful signal is not
// optional.
//
// **A command that fails is not an error.** A non-zero exit, a signal, and a
// timeout are all *results*: they are returned with a nil error, because the
// stderr of a failing command is exactly the diagnostic the model needs and an
// error return invites a caller to drop it. The error return is reserved for
// the two cases where there is no result — a request that was wrong
// ([FaultTask]) and a harness that could not run it ([FaultInternal]). That
// line is the one ADR-0006 §3 buckets on, so putting a failed build in the
// `internal` bucket would be a harness defect laundered into a model number.
//
// **A cancellation is a result *and* a [FaultCancelled] error.** The result
// carries the output the command managed to produce and the kill that stopped
// it, so nothing is dropped; the error is what the bench classifier reads.
// KAN-808 settled that shape across all five tools, in both directions:
// [FaultInternal] would inflate ADR-0006 §3's harness bucket with the one thing
// that is nobody's failure, and a nil error would look like a clean stop, which
// SLICE-1 §9's "everything else" arm charges to the *model*. A timeout is
// different and stays a plain result — that is this tool deciding the command
// had had long enough, which is a fact about the command.
//
// Dir is resolved through [Root.Resolve], but that is a check on where the
// command *starts* and not a containment guarantee: a shell can cd anywhere the
// user can. Containment is not what run_shell offers, which is exactly why
// SLICE-1 §M1 has the engine always ask before one runs. That gate is
// KAN-791's and lives in internal/permission; nothing here prompts.
//
// The child inherits the environment minus [apiKeyEnv]. `env` is a one-line
// command whose stdout goes into the journal verbatim, and the cheapest way to
// keep the key out of the journal is for the process that could print it not to
// have it. Stdin is the null device, so a command that reads it gets EOF rather
// than hanging until the timeout.
func (s *Set) RunShell(ctx context.Context, req ShellRequest) (ShellResult, error) {
	if err := ctx.Err(); err != nil {
		return cancelledShell(req), cancelledErr(ToolRunShell, req.Dir, err, "nothing was run")
	}
	if strings.TrimSpace(req.Command) == "" {
		return ShellResult{}, taskErr(ToolRunShell, req.Dir, nil, "a command is required")
	}
	if req.Timeout < 0 {
		return ShellResult{}, taskErr(ToolRunShell, req.Dir, nil,
			fmt.Sprintf("timeout %s is negative", req.Timeout))
	}

	p, err := s.Root.Resolve(ToolRunShell, req.Dir)
	if err != nil {
		return ShellResult{}, err
	}
	info, err := s.stat(ToolRunShell, p)
	if err != nil {
		return ShellResult{}, err
	}
	if !info.IsDir() {
		return ShellResult{}, taskErr(ToolRunShell, req.Dir, ErrNotRegular,
			"it is a file, not a directory; run_shell needs a directory to start in")
	}

	timeout := req.Timeout
	if timeout == 0 {
		timeout = s.Limits.ShellTimeout
	}

	var stdout, stderr bytes.Buffer
	cmd := newShellCmd(req.Command)
	cmd.Dir = p.Abs
	cmd.Env = childEnv()
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	// A backstop for the one case the group kill cannot reach: a descendant
	// that detached from the group and still holds the output pipe would
	// otherwise block Wait forever, turning a finished command into a hang.
	cmd.WaitDelay = s.Limits.ShellGrace

	clock := s.clock()
	started := clock.Now()
	if err := cmd.Start(); err != nil {
		return ShellResult{}, internalErr(ToolRunShell, req.Dir, err,
			fmt.Sprintf("the shell could not be started for %q", req.Command))
	}

	waited := make(chan error, 1)
	go func() { waited <- cmd.Wait() }()

	timer, stop := clock.NewTimer(timeout)
	defer stop()

	res := ShellResult{
		Command: req.Command,
		Dir:     p.Slash(),
		Timeout: timeout,
		Outcome: OutcomeExited,
		// Until a ProcessState says otherwise there is no exit code, and 0
		// would be the one wrong guess to make.
		ExitCode: -1,
	}

	var werr error
	select {
	case werr = <-waited:
	case <-timer:
		res.Outcome = OutcomeTimedOut
		res.StoppedBy, werr = s.stopGroup(cmd, waited)
	case <-ctx.Done():
		res.Outcome = OutcomeCancelled
		res.StoppedBy, werr = s.stopGroup(cmd, waited)
	}

	// cmd.Wait has returned on every path above, which is what makes reading
	// the buffers here safe: os/exec's copying goroutines are joined by Wait,
	// so touching them earlier would be a data race rather than merely a
	// partial read.
	res.Stdout, res.Stderr = stdout.String(), stderr.String()
	res.Duration = clock.Now().Sub(started)
	res.LeftRunning = errors.Is(werr, exec.ErrWaitDelay)

	if ps := cmd.ProcessState; ps != nil {
		res.ExitCode, res.Signal = exitStatus(ps)
		if res.Outcome == OutcomeExited && res.Signal != "" {
			res.Outcome = OutcomeSignalled
		}
	}
	res.Output = res.render()
	if res.Outcome == OutcomeCancelled {
		return res, cancelledErr(ToolRunShell, req.Dir, ctx.Err(),
			"the process group was stopped and the run abandoned")
	}
	return res, nil
}

// cancelledShell is the result of a call the context had already ended before
// anything was started. It is separate from [ShellResult.render] because that
// one describes a run — "the process group was killed" is the wrong sentence
// for a command that never had a process group.
func cancelledShell(req ShellRequest) ShellResult {
	return ShellResult{
		Command:  req.Command,
		Outcome:  OutcomeCancelled,
		ExitCode: -1,
		Output: fmt.Sprintf("run_shell `%s`: cancelled before starting; nothing was run\n",
			req.Command),
	}
}

// stopGroup ends the process group and waits for the run to be reaped,
// returning what it took to stop it.
//
// The leader has not been reaped when this runs — cmd.Wait is still blocked on
// it — so its pid cannot have been recycled, and the negative pid reaches the
// group that was started here and no other. Signalling a group *after* the
// leader is reaped is the version of this that can hit a stranger, and it is
// why nothing here signals on the ordinary exit path.
func (s *Set) stopGroup(cmd *exec.Cmd, waited <-chan error) (string, error) {
	label, err := terminateGroup(cmd)
	switch {
	case err != nil:
		// The graceful signal was not delivered at all. Waiting out a grace
		// period for it would be waiting for nothing, so escalate now.
	case label == "":
		// There was nothing left to signal: the group exited between the
		// decision to kill it and the syscall.
		return "", <-waited
	default:
		grace, stop := s.clock().NewTimer(s.Limits.ShellGrace)
		defer stop()
		select {
		case werr := <-waited:
			return label, werr
		case <-grace:
		}
	}

	label, _ = killGroup(cmd)
	return label, <-waited
}

// childEnv is the parent environment minus the provider credential.
//
// Everything else is inherited, because a coding agent's commands need PATH,
// HOME and whatever the project's toolchain reads, and an allowlist would break
// real builds for no gain — the command itself is arbitrary, so this is not a
// sandbox and pretending otherwise would be the dishonest version. The one
// removal is the one thing CLAUDE.md says must appear in no journal, no blob
// and no log.
func childEnv() []string {
	env := os.Environ()
	out := make([]string, 0, len(env))
	for _, kv := range env {
		if strings.HasPrefix(kv, apiKeyEnv+"=") {
			continue
		}
		out = append(out, kv)
	}
	return out
}

// render is the model-facing form:
//
//	run_shell `go test ./...` in internal/tools: exited 1 after 1.203s
//	--- stdout (28 bytes) ---
//	FAIL	kopicode/internal/tools
//	--- stderr (0 bytes) ---
//
// Both stream headers are always printed, including for an empty stream. A
// missing section would read as "you were not shown stderr", and the whole
// point of capturing both is that the model can tell the difference between a
// command that said nothing and a command whose complaint was withheld.
func (r ShellResult) render() string {
	var b strings.Builder
	fmt.Fprintf(&b, "run_shell `%s` in %s: %s\n", r.Command, r.Dir, r.status())
	if r.LeftRunning {
		b.WriteString("note: a process this command started outlived it and still held its output; " +
			"it had left the process group, so the kill did not reach it\n")
	}
	fmt.Fprintf(&b, "--- stdout (%d bytes) ---\n", len(r.Stdout))
	writeStream(&b, r.Stdout)
	fmt.Fprintf(&b, "--- stderr (%d bytes) ---\n", len(r.Stderr))
	writeStream(&b, r.Stderr)
	return b.String()
}

func (r ShellResult) status() string {
	took := r.Duration.Round(time.Millisecond)
	switch r.Outcome {
	case OutcomeSignalled:
		return fmt.Sprintf("killed by %s after %s", r.Signal, took)
	case OutcomeTimedOut:
		return fmt.Sprintf("timed out after %s (timeout %s); the process group was %s",
			took, r.Timeout, r.stopped())
	case OutcomeCancelled:
		return fmt.Sprintf("cancelled after %s; the process group was %s", took, r.stopped())
	default:
		return fmt.Sprintf("exited %d after %s", r.ExitCode, took)
	}
}

func (r ShellResult) stopped() string {
	if r.StoppedBy == "" {
		return "already gone"
	}
	return "killed with " + r.StoppedBy
}

// writeStream writes a captured stream whole, terminated by exactly one
// newline so the next header starts on its own line. Nothing is dropped: a
// stream that did not end in a newline gains one, and that is the only edit
// made to either stream anywhere in this file.
func writeStream(b *strings.Builder, s string) {
	if s == "" {
		return
	}
	b.WriteString(s)
	if !strings.HasSuffix(s, "\n") {
		b.WriteByte('\n')
	}
}
