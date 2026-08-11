package tools_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/leejianrong/kopicode/internal/tools"
)

// Everything in this file is platform-independent: it either exercises a
// rejection that happens before anything is spawned, or it is the fake clock
// the unix tests drive. The runs themselves live in shell_unix_test.go, because
// `sh -c` and kill(2) do not exist on Windows and a test that pretends
// otherwise is a cross-compile failure waiting for someone else to hit.

func TestRunShellRejectsBadRequests(t *testing.T) {
	f := newFixture(t, map[string]string{"go.mod": "module x\n"})
	s := f.set(t)

	cases := []struct {
		name  string
		req   tools.ShellRequest
		fault tools.Fault
		want  string
	}{
		{
			name:  "no command",
			req:   tools.ShellRequest{Command: "   "},
			fault: tools.FaultTask,
			want:  "a command is required",
		},
		{
			name:  "negative timeout",
			req:   tools.ShellRequest{Command: "true", Timeout: -time.Second},
			fault: tools.FaultTask,
			want:  "is negative",
		},
		{
			name:  "working directory outside the root",
			req:   tools.ShellRequest{Command: "true", Dir: "../outside"},
			fault: tools.FaultTask,
			want:  "outside the repository root",
		},
		{
			name:  "working directory is a file",
			req:   tools.ShellRequest{Command: "true", Dir: "go.mod"},
			fault: tools.FaultTask,
			want:  "not a directory",
		},
		{
			name:  "working directory does not exist",
			req:   tools.ShellRequest{Command: "true", Dir: "nope"},
			fault: tools.FaultTask,
			want:  "no such file or directory",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res, err := s.RunShell(t.Context(), tc.req)
			wantFault(t, err, tc.fault)
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %q, want it to mention %q", err, tc.want)
			}
			// A refusal must not look like a command that exited 0.
			if res.Output != "" || res.ExitCode != 0 {
				t.Errorf("refused call returned a result: %+v", res)
			}
		})
	}
}

// TestRunShellOnAnAlreadyCancelledContext pins the half of KAN-808's convention
// that is specific to this tool: no process is started, and the result says
// that rather than describing a process group that never existed. The
// classification is tabled across all five tools in cancel_test.go.
func TestRunShellOnAnAlreadyCancelledContext(t *testing.T) {
	f := newFixture(t, nil)
	s := f.set(t)

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	res, err := s.RunShell(ctx, tools.ShellRequest{Command: "true"})
	if err == nil {
		t.Fatal("want an error for an already-cancelled context, got nil")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("error = %v, want it to wrap context.Canceled", err)
	}
	if res.Outcome != tools.OutcomeCancelled {
		t.Errorf("outcome = %s, want %s", res.Outcome, tools.OutcomeCancelled)
	}
	if !strings.Contains(res.Output, "cancelled before starting") {
		t.Errorf("output does not say nothing was run:\n%s", res.Output)
	}
}

func TestOutcomeStringIsTheWireForm(t *testing.T) {
	want := map[tools.Outcome]string{
		tools.OutcomeExited:    "exited",
		tools.OutcomeSignalled: "signalled",
		tools.OutcomeTimedOut:  "timed_out",
		tools.OutcomeCancelled: "cancelled",
	}
	for outcome, s := range want {
		if got := outcome.String(); got != s {
			t.Errorf("Outcome(%d).String() = %q, want %q", outcome, got, s)
		}
	}
	if got := tools.Outcome(99).String(); got != "unknown" {
		t.Errorf("unknown outcome = %q, want %q", got, "unknown")
	}
}

// fakeClock is a clock whose timers only fire when a test says so.
//
// It is what makes the timeout path assertable: the production timeout is 120
// seconds, and the choice without an injected clock is between a suite that
// waits that long and a test that shortens the bound until it proves something
// other than what ships. Here the test arms the run, waits for the timer to
// exist, fires it, and then asserts on what the harness did — no sleeping, and
// the reported Duration is exactly the timeout that was armed.
type fakeClock struct {
	mu     sync.Mutex
	now    time.Time
	armed  []*fakeTimer
	notify chan struct{}
}

type fakeTimer struct {
	d time.Duration
	c chan time.Time
}

func newFakeClock() *fakeClock {
	return &fakeClock{
		now: time.Date(2026, 8, 11, 9, 0, 0, 0, time.UTC),
		// Buffered well past the two timers one run can arm, so the
		// non-blocking send below never has to drop one.
		notify: make(chan struct{}, 8),
	}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) NewTimer(d time.Duration) (<-chan time.Time, func()) {
	t := &fakeTimer{d: d, c: make(chan time.Time, 1)}
	c.mu.Lock()
	c.armed = append(c.armed, t)
	c.mu.Unlock()
	select {
	case c.notify <- struct{}{}:
	default:
	}
	return t.c, func() {}
}

// fire advances to the oldest armed timer and fires it, blocking until one
// exists.
//
// The wait is a handshake and not a poll: NewTimer signals on notify, so fire
// wakes when the run arms its timeout — or when stopGroup arms the grace timer,
// which by construction cannot exist until the group has been signalled. The
// deadline is only there so a broken run fails as a test rather than as a hang.
func (c *fakeClock) fire(t *testing.T) time.Duration {
	t.Helper()
	for {
		c.mu.Lock()
		if len(c.armed) > 0 {
			timer := c.armed[0]
			c.armed = c.armed[1:]
			c.now = c.now.Add(timer.d)
			now := c.now
			c.mu.Unlock()
			timer.c <- now
			return timer.d
		}
		c.mu.Unlock()

		select {
		case <-c.notify:
		case <-time.After(10 * time.Second):
			t.Fatal("no timer was armed within 10s; the run never reached its timeout")
		}
	}
}
