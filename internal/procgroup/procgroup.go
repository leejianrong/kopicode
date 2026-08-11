// Package procgroup starts a subprocess in a process group of its own and ends
// the whole group when the caller has had enough of it.
//
// It exists so that kopicode has **one** answer to "how do we kill a
// subprocess", rather than one per package that happens to spawn something.
// run_shell (KAN-782) established the pattern and the post-edit syntax gate
// (KAN-786) is the second consumer; a second implementation would be two places
// to get orphan reaping wrong.
//
// **The process group is the point.** A command is started as a group leader,
// so its pid doubles as the group id and everything it spawns inherits the
// group. A timeout or a cancellation then signals the *negative* pid and the
// signal reaches the whole tree. Killing by pid instead leaves
// `sh -c 'sleep 300 & wait'` with `sleep` reparented to init and still running,
// and `go build` with its compile subprocesses still running — an orphan
// holding a pipe, and in a bench run a machine that slowly fills with them.
//
// Termination is graceful, then forceful: ask the group to exit, then end it
// after a grace period. A compiler given the chance to exit removes its own
// temporary files and reaps its own children; a graceful signal it chooses to
// ignore is not a shutdown, so the grace period is bounded and the forceful
// step is not optional.
//
// This package holds no policy. It does not decide when a run has gone on too
// long, it does not build environments, and it does not classify anything —
// callers own all of that.
package procgroup

import (
	"os/exec"
	"time"
)

// Clock is where the grace period comes from.
//
// It is an interface, and a minimal one, because the caller already has a clock
// injected for its own timeout and a second source of time would let the two
// disagree. Anything with this method satisfies it — tools.Clock and
// syntax.Clock both do, without either importing this package for the type.
type Clock interface {
	// NewTimer returns a channel that receives once d has elapsed, and a
	// function that releases the timer. The caller always calls it.
	NewTimer(d time.Duration) (<-chan time.Time, func())
}

// Stop ends the process group cmd leads and waits for the run to be reaped,
// returning what it took to stop it — "" when the group was already gone.
//
// waited must be a channel fed by a goroutine blocked in cmd.Wait; Stop
// receives from it exactly once on every path, so the caller must not also read
// it. That shape is deliberate: the leader has not been reaped while Wait is
// still blocked on it, so its pid cannot have been recycled and the negative
// pid reaches the group that was started here and no other. Signalling a group
// *after* the leader is reaped is the version of this that can hit a stranger,
// which is why nothing here signals on the ordinary exit path.
func Stop(cmd *exec.Cmd, waited <-chan error, clock Clock, grace time.Duration) (string, error) {
	label, err := Terminate(cmd)
	switch {
	case err != nil:
		// The graceful signal was not delivered at all. Waiting out a grace
		// period for it would be waiting for nothing, so escalate now.
	case label == "":
		// There was nothing left to signal: the group exited between the
		// decision to kill it and the syscall.
		return "", <-waited
	default:
		timer, stop := clock.NewTimer(grace)
		defer stop()
		select {
		case werr := <-waited:
			return label, werr
		case <-timer:
		}
	}

	label, _ = Kill(cmd)
	return label, <-waited
}
