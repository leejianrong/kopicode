package engine_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/leejianrong/kopicode/internal/engine"
	"github.com/leejianrong/kopicode/internal/journal"
	"github.com/leejianrong/kopicode/internal/repo"
	"github.com/leejianrong/kopicode/internal/tools"
)

// ADR-0042 at the engine seam. treeScript is a snapshotter whose tree for each
// call is chosen by the test: Tree is the baseline, and Snapshot returns the next
// of trees, so a turn reads as changed or unchanged as the test says.
type treeScript struct {
	baseline  string
	trees     []string
	treeCalls int
	snapshots int
}

func (s *treeScript) Tree(context.Context) (string, error) {
	s.treeCalls++
	return s.baseline, nil
}

func (s *treeScript) Snapshot(_ context.Context, turn int) (repo.Snapshot, error) {
	tree := s.trees[s.snapshots]
	s.snapshots++
	return repo.Snapshot{
		Ref:    fmt.Sprintf("refs/kopicode/x/%d", turn),
		Commit: fmt.Sprintf("%040d", turn),
		Tree:   tree,
	}, nil
}

func shellReply(id string) scriptedReply {
	return scriptedReply{calls: []wireCall{nativeCall(id, tools.ToolRunShell, `{"command":"echo hi"}`)}}
}

func verifyChangedRun(t *testing.T, on bool, snap *treeScript, mode string, replies ...scriptedReply) (*harness, engine.Result) {
	t.Helper()
	h := scriptHarness(t, replies, oneAttemptPerTurn(len(replies)), map[string]string{"f.txt": "x\n"},
		withMaxTurns(len(replies)+1), withVerificationForced(true), withHelperVerification(t, mode),
		func(c *engine.Config) {
			c.Snapshots = snap
			c.Selection.Config.VerifyChangedOnly = on
		})
	res, err := h.eng.Run(t.Context(), "do it")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	return h, res
}

func countOf(h *harness, ty journal.Type) int {
	n := 0
	for _, got := range h.types() {
		if got == ty {
			n++
		}
	}
	return n
}

func TestAShellTurnThatLeftTheTreeAloneIsNotVerified(t *testing.T) {
	snap := &treeScript{baseline: "T0", trees: []string{"T0"}}
	h, res := verifyChangedRun(t, true, snap, "fail", shellReply("s1"), scriptedReply{text: "ok"})

	if n := countOf(h, journal.TypeVerificationRun); n != 0 {
		t.Errorf("%d VerificationRun events after a turn that changed nothing, want none", n)
	}
	if got := sole[journal.TurnSnapshot](t, h.events()); !got.Unchanged {
		t.Errorf("TurnSnapshot.Unchanged = false, want true: a reader must see the gap was deliberate")
	}
	if res.Stop != engine.StopCompleted {
		t.Errorf("stop = %v, want completed: a failing suite the session never touched is not its failure", res.Stop)
	}
}

func TestTheSameTurnIsVerifiedWithTheSettingOff(t *testing.T) {
	snap := &treeScript{baseline: "T0", trees: []string{"T0"}}
	h, res := verifyChangedRun(t, false, snap, "fail", shellReply("s1"), scriptedReply{text: "ok"})

	if n := countOf(h, journal.TypeVerificationRun); n != 1 {
		t.Errorf("%d VerificationRun events with the setting off, want today's 1", n)
	}
	if got := sole[journal.TurnSnapshot](t, h.events()); got.Unchanged {
		t.Error("Unchanged was recorded with the setting off")
	}
	if snap.treeCalls != 0 {
		t.Errorf("Tree was called %d times with the setting off; no baseline should be read", snap.treeCalls)
	}
	if res.Stop != engine.StopVerificationFailed {
		t.Errorf("stop = %v, want verification_failed (today's behaviour)", res.Stop)
	}
}

func TestAShellTurnThatChangedTheTreeIsVerified(t *testing.T) {
	snap := &treeScript{baseline: "T0", trees: []string{"T1"}}
	h, _ := verifyChangedRun(t, true, snap, "pass", shellReply("s1"), scriptedReply{text: "ok"})

	if n := countOf(h, journal.TypeVerificationRun); n != 1 {
		t.Errorf("%d VerificationRun events after a turn that changed the tree, want 1", n)
	}
	if got := sole[journal.TurnSnapshot](t, h.events()); got.Unchanged {
		t.Error("Unchanged recorded for a turn that changed the tree")
	}
}

func TestTheBaselineIsReadOnceAndOnlyForAMutatingCall(t *testing.T) {
	readOnly := &treeScript{baseline: "T0"}
	verifyChangedRun(t, true, readOnly, "pass",
		scriptedReply{calls: []wireCall{nativeCall("r1", tools.ToolReadFile, `{"path":"f.txt"}`)}}, scriptedReply{text: "ok"})
	if readOnly.treeCalls != 0 {
		t.Errorf("a session with no mutating call read the baseline %d times, want 0", readOnly.treeCalls)
	}

	twice := &treeScript{baseline: "T0", trees: []string{"T1", "T2"}}
	verifyChangedRun(t, true, twice, "pass", shellReply("s1"), shellReply("s2"), scriptedReply{text: "ok"})
	if twice.treeCalls != 1 {
		t.Errorf("two mutating turns read the baseline %d times, want once; later turns compare with the last snapshot", twice.treeCalls)
	}
}

func TestAFailureStillOutstandingIsNotForgivenByAnUnchangedTurn(t *testing.T) {
	// Turn 1 writes (the tree changes, verification fails and stays outstanding);
	// turn 2 is a shell that changes nothing and is skipped; the model then stops.
	snap := &treeScript{baseline: "T0", trees: []string{"T1", "T1"}}
	h, res := verifyChangedRun(t, true, snap, "fail",
		scriptedReply{calls: []wireCall{nativeCall("w1", tools.ToolWriteFile, `{"path":"g.txt","content":"y\n"}`)}},
		shellReply("s1"), scriptedReply{text: "done"})

	if n := countOf(h, journal.TypeVerificationRun); n != 1 {
		t.Errorf("%d VerificationRun events, want 1 (the write's, not the unchanged shell's)", n)
	}
	if res.Stop != engine.StopVerificationFailed {
		t.Errorf("stop = %v, want verification_failed: the write's failure is still outstanding", res.Stop)
	}
}
