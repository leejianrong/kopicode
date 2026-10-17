package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/leejianrong/kopicode/internal/harness"
	"github.com/leejianrong/kopicode/internal/journal"
	"github.com/leejianrong/kopicode/internal/procgroup"
)

// User hooks (ADR-0036). A hook is a command the person declared in their own
// config that the engine runs at a fixed point. This file is the one place a
// hook command is started, so the three points share one subprocess path and one
// answer to the subprocess rule (Dir named, Env built here, own process group).
//
// The contract with the command, from the ADR: the event goes in as JSON on
// stdin; exit 0 passes; exit 2 is a deliberate refusal with stderr as the
// reason; anything else (another exit code, a signal, a timeout, a command that
// cannot start) is a failure, and a failed gate is not a pass.
//
// Only the REPL fills Config.Selection.Config.Hooks, from the user's file, so
// everything here is inert under run --print, serve, mcp and kopibench.

// Hook points, as they appear in the journal and on the wire to the command.
const (
	hookPreTool  = "pre_tool"
	hookPostTool = "post_tool"
	hookStop     = "stop"
)

// Hook outcomes, journal.HookRan.Outcome.
const (
	hookPassed   = "passed"
	hookDenied   = "denied"
	hookFailed   = "failed"
	hookTimedOut = "timed_out"
)

const (
	// DefaultHookTimeout is how long a hook gets when Config.HookTimeout is unset.
	DefaultHookTimeout = 30 * time.Second
	// MaxHookTimeout is the longest a hook may be allowed to run.
	MaxHookTimeout = 5 * time.Minute
	// hookGrace is how long a signalled process group has to exit before it is
	// killed, and how long Wait waits on a pipe a grandchild still holds.
	hookGrace = 2 * time.Second
)

// HookClock is where a hook's duration and timeout come from. Nil in
// [Config.HookClock] means the real one.
type HookClock interface {
	Now() time.Time
	// NewTimer matches procgroup.Clock, so the grace period is measured by the
	// same clock as the timeout.
	NewTimer(d time.Duration) (<-chan time.Time, func())
}

type realHookClock struct{}

func (realHookClock) Now() time.Time { return time.Now() }

func (realHookClock) NewTimer(d time.Duration) (<-chan time.Time, func()) {
	t := time.NewTimer(d)
	return t.C, func() { t.Stop() }
}

// hookEvent is what a hook command reads on stdin. It carries the point, the
// call and where in the session it happened, and nothing credential-shaped:
// there is no field a provider key could be in.
type hookEvent struct {
	Hook      string          `json:"hook"`
	SessionID string          `json:"session_id"`
	Turn      int             `json:"turn"`
	CallID    string          `json:"call_id,omitempty"`
	Tool      string          `json:"tool,omitempty"`
	Arguments json.RawMessage `json:"arguments,omitempty"`
}

// hookResult is how one run went, in the terms the journal and the caller need.
type hookResult struct {
	outcome   string
	detail    string
	exitCode  int
	stdout    string
	stderr    string
	cancelled bool
}

// hookPassEnv names what a hook command inherits, and nothing else.
//
// **An allowlist, as ADR-0036 decision 7 says and for the reason AGENTS.md gives
// for secrets:** a hook is a command the person wrote, but its output is
// journaled and shown to the model, and the cost of letting the provider key or a
// git-redirecting variable through is unbounded. So a name is here or it is gone.
// The list is what a formatter, a linter or `make` needs to find its toolchain
// (PATH, a home directory, the Go and locale variables) and what Windows needs to
// start a process at all. A hook that needs another variable sets it itself in
// its own command, for example `["env", "FOO=1", "make", "lint"]`.
//
// GIT_DIR and the other GIT_* overrides are absent by construction, which is the
// reason `Dir` alone would not have been enough.
var hookPassEnv = []string{
	"PATH", "HOME", "USER", "LOGNAME", "SHELL", "TERM", "TZ",
	"LANG", "LC_ALL", "LC_CTYPE", "LC_MESSAGES",
	"TMPDIR", "TEMP", "TMP",
	"XDG_CACHE_HOME", "XDG_CONFIG_HOME", "XDG_DATA_HOME",
	"GOROOT", "GOPATH", "GOCACHE", "GOMODCACHE",
	"SSL_CERT_FILE", "SSL_CERT_DIR",
	// Windows needs these to start a process at all.
	"SystemRoot", "USERPROFILE", "LOCALAPPDATA", "APPDATA", "ComSpec", "PATHEXT",
}

// hookEnv builds a hook command's environment from [hookPassEnv].
func hookEnv() []string {
	env := make([]string, 0, len(hookPassEnv))
	for _, name := range hookPassEnv {
		if value, ok := os.LookupEnv(name); ok {
			env = append(env, name+"="+value)
		}
	}
	return env
}

func (e *Engine) hooks() harness.Hooks { return e.cfg.Selection.Config.Hooks }

func (e *Engine) hookClock() HookClock {
	if e.cfg.HookClock == nil {
		return realHookClock{}
	}
	return e.cfg.HookClock
}

// hookTimeout is the bound a hook runs under: the default when unset, never more
// than [MaxHookTimeout].
func (e *Engine) hookTimeout() time.Duration {
	switch d := e.cfg.HookTimeout; {
	case d <= 0:
		return DefaultHookTimeout
	case d > MaxHookTimeout:
		return MaxHookTimeout
	default:
		return d
	}
}

// hookAppliesTo reports whether a tool hook is declared for tool: a command is
// set and hook_tools is either empty or names it.
func hookAppliesTo(h harness.Hooks, argv []string, tool string) bool {
	if len(argv) == 0 {
		return false
	}
	if len(h.Tools) == 0 {
		return true
	}
	for _, t := range h.Tools {
		if t == tool {
			return true
		}
	}
	return false
}

// runHook starts one hook command, waits for it under the timeout, journals a
// HookRan whatever happened, and reports how it went. The error is fatal and only
// the journal's.
//
// It is the one place in this package that starts a hook, so the Dir and Env
// decisions below are the whole of the answer to the subprocess rule, and
// internal/arch/subprocess_test.go covers the call site with no waiver.
func (e *Engine) runHook(ctx context.Context, turn int, point, callID, tool string, argv []string, args json.RawMessage) (hookResult, error) {
	stdin, err := json.Marshal(hookEvent{
		Hook: point, SessionID: e.cfg.SessionID, Turn: turn, CallID: callID, Tool: tool, Arguments: args,
	})
	if err != nil {
		// Unreachable for the field types above, and a hook that cannot be told
		// what happened must not be skipped as if it had passed.
		res := hookResult{outcome: hookFailed, detail: fmt.Sprintf("could not encode the event: %v", err), exitCode: -1}
		return res, e.journalHook(ctx, turn, point, callID, tool, argv, res, 0)
	}

	clock := e.hookClock()
	start := clock.Now()
	res := e.execHook(ctx, clock, argv, stdin)
	if point == hookPostTool && res.outcome == hookDenied {
		// Exit 2 is a refusal, and a post_tool hook has nothing to refuse: the
		// call already ran. It is recorded as the failure it is.
		res.outcome = hookFailed
	}
	return res, e.journalHook(ctx, turn, point, callID, tool, argv, res, clock.Now().Sub(start))
}

func (e *Engine) execHook(ctx context.Context, clock HookClock, argv []string, stdin []byte) hookResult {
	var stdout, stderr bytes.Buffer

	cmd := exec.Command(argv[0], argv[1:]...) //nolint:gosec // the argv is the person's own, from their user config, never written by the model
	cmd.Dir = e.cfg.Tools.Root.Path()
	cmd.Env = hookEnv()
	cmd.Stdin = bytes.NewReader(stdin)
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	cmd.WaitDelay = hookGrace
	procgroup.Isolate(cmd)

	if err := cmd.Start(); err != nil {
		return hookResult{outcome: hookFailed, exitCode: -1, detail: fmt.Sprintf("%s could not be started: %v", argv[0], err)}
	}

	waited := make(chan error, 1)
	go func() { waited <- cmd.Wait() }()

	timer, stop := clock.NewTimer(e.hookTimeout())
	defer stop()

	res := hookResult{exitCode: -1}
	var endedBy string
	select {
	case <-waited:
	case <-timer:
		res.outcome = hookTimedOut
		endedBy, _ = procgroup.Stop(cmd, waited, clock, hookGrace)
		res.detail = fmt.Sprintf("timed out after %s", e.hookTimeout())
	case <-ctx.Done():
		res.outcome = hookFailed
		res.cancelled = true
		endedBy, _ = procgroup.Stop(cmd, waited, clock, hookGrace)
		res.detail = "cancelled"
	}
	if endedBy != "" {
		res.detail += "; stopped with " + endedBy
	}

	// cmd.Wait has returned on every path above, which is what makes reading the
	// buffers safe.
	res.stdout, res.stderr = stdout.String(), stderr.String()
	if res.outcome != "" {
		return res
	}

	code, signal := -1, ""
	if ps := cmd.ProcessState; ps != nil {
		code, signal = procgroup.ExitStatus(ps)
	}
	switch {
	case signal != "":
		res.outcome, res.detail = hookFailed, "killed by "+signal
	case code == 0:
		res.outcome, res.exitCode = hookPassed, 0
	case code == 2:
		res.outcome, res.exitCode, res.detail = hookDenied, 2, "exited with status 2"
	default:
		res.outcome, res.exitCode, res.detail = hookFailed, code, fmt.Sprintf("exited with status %d", code)
	}
	return res
}

func (e *Engine) journalHook(ctx context.Context, turn int, point, callID, tool string, argv []string, res hookResult, took time.Duration) error {
	_, err := e.append(ctx, turn, journal.HookRan{
		Point:      point,
		CallID:     callID,
		Tool:       tool,
		Command:    argv,
		ExitCode:   res.exitCode,
		DurationMS: took.Milliseconds(),
		Outcome:    res.outcome,
		Detail:     res.detail,
		Stdout:     journal.InlineText(res.stdout),
		Stderr:     journal.InlineText(res.stderr),
	})
	return err
}

// preToolBlock runs the pre_tool hook for a call that decoded, and says whether
// the call must not run and what to tell the model. A hook can only deny: a pass
// changes nothing, and the consent gate still runs after it (ADR-0036 decision
// 4). The hook goes first so a person is not asked to approve a call a hook has
// already refused.
func (e *Engine) preToolBlock(ctx context.Context, turn int, callID string, tool string, args json.RawMessage) (string, bool, error) {
	h := e.hooks()
	if !hookAppliesTo(h, h.PreTool, tool) {
		return "", false, nil
	}
	res, err := e.runHook(ctx, turn, hookPreTool, callID, tool, h.PreTool, args)
	if err != nil {
		return "", false, err
	}
	if res.outcome == hookPassed {
		return "", false, nil
	}
	return e.hookRefusal(hookPreTool, res), true, nil
}

// hookRefusal is what the model is told when a hook stopped a call. The reason is
// the hook's own stderr for a deliberate no, and its whole output for a failure,
// so the model and the person see what the command said. Capped for the model by
// ADR-0033's rule; the journal has it whole.
func (e *Engine) hookRefusal(point string, res hookResult) string {
	var msg string
	switch res.outcome {
	case hookDenied:
		reason := strings.TrimSpace(res.stderr)
		if reason == "" {
			reason = "the hook gave no reason"
		}
		msg = fmt.Sprintf("This call was not run: the %s hook denied it. Reason: %s", point, reason)
	default:
		msg = fmt.Sprintf("This call was not run: the %s hook failed (%s), and a failing hook blocks the call.", point, res.detail)
		if out := strings.TrimSpace(res.stderr + res.stdout); out != "" {
			msg += " Its output: " + out
		}
	}
	return capText(msg, e.cfg.Selection.Config.OutputCap, "")
}

// postToolNote runs the post_tool hook for a call that ran and may have changed
// the tree, and returns the note to show the model after the call's result, or ""
// when there is nothing to say.
//
// The hook cannot undo the call, so its failure is not a block. It is the
// opposite of quiet: a hook that exits non-zero, times out or cannot start
// produces a note that says so, because a check that fails unseen is the gate
// that is open when it is broken (ADR-0036 decision 5). A hook that passes with
// no output adds nothing.
func (e *Engine) postToolNote(ctx context.Context, turn int, callID, tool string, args json.RawMessage) (string, error) {
	h := e.hooks()
	if !hookAppliesTo(h, h.PostTool, tool) {
		return "", nil
	}
	res, err := e.runHook(ctx, turn, hookPostTool, callID, tool, h.PostTool, args)
	if err != nil {
		return "", err
	}
	return capText(postToolNote(res.outcome, res.detail, res.stdout, res.stderr), e.cfg.Selection.Config.OutputCap, ""), nil
}

// postToolNote is the text the model is shown for one post_tool run. It is a pure
// function of what HookRan records, so resume can rebuild exactly what the model
// saw from the journal without running the hook again.
func postToolNote(outcome, detail, stdout, stderr string) string {
	out := strings.TrimSpace(stdout + stderr)
	if outcome == hookPassed {
		if out == "" {
			return ""
		}
		return "Note from the post_tool hook:\n" + out
	}
	note := fmt.Sprintf("The post_tool hook failed (%s). The call above already ran and was not undone.", detail)
	if out != "" {
		note += " Its output:\n" + out
	}
	return note
}
