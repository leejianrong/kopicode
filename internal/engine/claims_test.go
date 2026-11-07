package engine_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/leejianrong/kopicode/internal/anchor"
	"github.com/leejianrong/kopicode/internal/engine"
	"github.com/leejianrong/kopicode/internal/journal"
	"github.com/leejianrong/kopicode/internal/repo"
	"github.com/leejianrong/kopicode/internal/tools"
)

// ADR-0041 at the engine seam: one edit to f.txt, then a final message that says
// more files were changed than the journal shows.

// steadyTree is a snapshotter whose tree never changes, so a shell turn is read as
// having written nothing.
type steadyTree struct{}

func (steadyTree) Tree(context.Context) (string, error) { return strings.Repeat("b", 40), nil }

func (steadyTree) Snapshot(_ context.Context, turn int) (repo.Snapshot, error) {
	return repo.Snapshot{Ref: "refs/kopicode/x/1", Commit: strings.Repeat("a", 40), Tree: strings.Repeat("b", 40)}, nil
}

func claimRun(t *testing.T, final string, shell bool, opts ...harnessOption) (*harness, engine.Result) {
	t.Helper()
	body := "alpha\nbeta\n"
	a := anchor.Derive(anchor.Split([]byte(body)))
	args, err := json.Marshal(map[string]string{"path": "f.txt", "anchor_start": a[0], "anchor_end": a[0], "new_text": "ALPHA\n"})
	if err != nil {
		t.Fatal(err)
	}
	replies := []scriptedReply{{calls: []wireCall{nativeCall("e1", tools.ToolEditFile, string(args))}}}
	if shell {
		replies = append(replies, scriptedReply{calls: []wireCall{nativeCall("s1", tools.ToolRunShell, `{"command":"echo hi"}`)}})
	}
	replies = append(replies, scriptedReply{text: final})
	h := scriptHarness(t, replies, oneAttemptPerTurn(len(replies)), map[string]string{"f.txt": body, "g.txt": "x\n"},
		append([]harnessOption{withMaxTurns(len(replies) + 1)}, opts...)...)
	res, err := h.eng.Run(t.Context(), "change things")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	return h, res
}

func finalSeq(t *testing.T, evs []journal.Event) uint64 {
	t.Helper()
	var seq uint64
	for _, e := range evs {
		if _, ok := e.Payload.(journal.AssistantMessage); ok {
			seq = e.Seq
		}
	}
	return seq
}

func TestAClaimedFileWithNoEditIsRecorded(t *testing.T) {
	h, _ := claimRun(t, "Updated `f.txt` and `g.txt` - Updated both.", false)
	evs := h.events()
	got := sole[journal.ClaimUnbacked](t, evs)
	if len(got.Files) != 1 || got.Files[0].Path != "g.txt" {
		t.Errorf("files = %+v, want only g.txt (f.txt was edited)", got.Files)
	}
	if got.Message != finalSeq(t, evs) {
		t.Errorf("message = %d, want the final AssistantMessage's seq %d", got.Message, finalSeq(t, evs))
	}
}

func TestABackedClaimWritesNothing(t *testing.T) {
	h, _ := claimRun(t, "`f.txt` - updated the first line.", false)
	for _, ty := range h.types() {
		if ty == journal.TypeClaimUnbacked || ty == journal.TypeClaimUnchecked {
			t.Fatalf("a claim the journal backs wrote %s", ty)
		}
	}
}

func TestAShellThatMayHaveWrittenMakesTheClaimUnchecked(t *testing.T) {
	// The stub snapshotter's tree changes every turn, so the shell turn reads as a write.
	h, _ := claimRun(t, "`g.txt` - updated.", true)
	got := sole[journal.ClaimUnchecked](t, h.events())
	if len(got.Files) != 1 || got.Files[0].Path != "g.txt" || got.Reason == "" {
		t.Errorf("unchecked = %+v", got)
	}
	for _, ty := range h.types() {
		if ty == journal.TypeClaimUnbacked {
			t.Error("a ClaimUnbacked was written beside the ClaimUnchecked")
		}
	}
}

func TestAShellThatLeftTheTreeAloneStillLetsTheClaimBeChecked(t *testing.T) {
	h, _ := claimRun(t, "`g.txt` - updated.", true, func(c *engine.Config) { c.Snapshots = steadyTree{} })
	// The first snapshot has nothing to compare with, so the edit turn sets the
	// tree and the shell turn that follows leaves it as it was.
	got := sole[journal.ClaimUnbacked](t, h.events())
	if len(got.Files) != 1 || got.Files[0].Path != "g.txt" {
		t.Errorf("unbacked = %+v", got)
	}
}

func TestTheRecordDoesNotChangeHowTheSessionEnds(t *testing.T) {
	_, res := claimRun(t, "`g.txt` - updated.", false)
	if res.Stop != engine.StopCompleted {
		t.Errorf("stop = %v, want completed", res.Stop)
	}
	if res.Turns != 2 {
		t.Errorf("turns = %d, want 2: the check adds no turn", res.Turns)
	}
}
