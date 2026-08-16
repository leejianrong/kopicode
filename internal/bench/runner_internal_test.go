package bench

import (
	"errors"
	"strings"
	"testing"
)

// TestWorktreeAccountRefusesATaskThatRanNowhereForNoReason drives the catch-all
// half of [worktreeAccount] directly, because nothing in the runner can produce
// its input any more.
//
// That is exactly why it is worth a test. KAN-875's cause was never pinned to a
// single mechanism: what was seen was nine worktrees for ten tasks, symmetric
// counts and a clean exit. The first check in worktreeAccount catches that when
// the creation failed and said so, which is the only path that exists today.
// This one catches it however else it arrives — a task with no worktree and
// nothing to explain it — and a guard for an unknown cause has to be exercised
// with a fabricated input or it is asserting nothing at all.
func TestWorktreeAccountRefusesATaskThatRanNowhereForNoReason(t *testing.T) {
	r := &RunResult{
		Tasks: []TaskResult{
			{TaskID: "task-01", Worktree: "/somewhere/task-01"},
			{TaskID: "task-02"}, // no worktree, no session error, no panic
			{TaskID: "task-03", SessionErr: "cancelled"},
			{TaskID: "task-04", Panicked: true},
		},
	}

	err := worktreeAccount(r)
	if !errors.Is(err, ErrWorktreeCreate) {
		t.Fatalf("worktreeAccount = %v, want ErrWorktreeCreate", err)
	}
	if !strings.Contains(err.Error(), "task-02") {
		t.Errorf("the error does not name the unmeasured task: %v", err)
	}
	for _, id := range []string{"task-01", "task-03", "task-04"} {
		if strings.Contains(err.Error(), id) {
			t.Errorf("%s is accounted for and must not be reported: %v", id, err)
		}
	}
}

// TestWorktreeAccountAcceptsARunThatAddsUp is the negative case: every task ran
// somewhere or says why it did not, and nothing was left behind. A guard that
// fired on a healthy run would be worse than none, because a bench run's whole
// value is that its refusals mean something.
func TestWorktreeAccountAcceptsARunThatAddsUp(t *testing.T) {
	r := &RunResult{
		Tasks: []TaskResult{
			{TaskID: "task-01", Worktree: "/somewhere/task-01"},
			{TaskID: "task-02", SessionErr: "context canceled"},
		},
		Reclamation: Reclamation{Created: 1, Removed: 1},
	}
	if err := worktreeAccount(r); err != nil {
		t.Fatalf("worktreeAccount on a run that adds up: %v", err)
	}
}
