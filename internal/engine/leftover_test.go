package engine_test

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/leejianrong/kopicode/internal/anchor"
	"github.com/leejianrong/kopicode/internal/engine"
	"github.com/leejianrong/kopicode/internal/journal"
	"github.com/leejianrong/kopicode/internal/tools"
)

// ADR-0039 at the engine seam: two edits rename old_name away, a third place is
// left, verification passes, and the model is told.
const leftoverBody = "one old_name here\nfiller a\ntwo old_name there\nfiller b\nthree old_name left\nfiller c\n"

func leftoverRun(t *testing.T, on bool, edits ...int) (*recordingProvider, *harness) {
	t.Helper()
	a := anchor.Derive(anchor.Split([]byte(leftoverBody)))
	var calls []wireCall
	lines := strings.Split(strings.TrimSuffix(leftoverBody, "\n"), "\n")
	for i, line := range edits {
		args, err := json.Marshal(map[string]string{
			"path": "f.txt", "anchor_start": a[line], "anchor_end": a[line],
			"new_text": strings.ReplaceAll(lines[line], "old_name", "new_name") + "\n",
		})
		if err != nil {
			t.Fatal(err)
		}
		calls = append(calls, nativeCall("e"+string(rune('1'+i)), tools.ToolEditFile, string(args)))
	}
	replies := []scriptedReply{{calls: calls}, {text: "Done."}}
	prov := script(t, replies, oneAttemptPerTurn(len(replies)))
	rec := &recordingProvider{inner: prov}
	h := newHarness(t, prov, map[string]string{"f.txt": leftoverBody},
		withProvider(rec), withMaxTurns(3), withVerificationForced(true), withHelperVerification(t, "pass"),
		func(c *engine.Config) { c.Selection.Config.LeftoverNote = on })
	if _, err := h.eng.Run(t.Context(), "rename old_name to new_name"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	return rec, h
}

func TestARemovedNameStillInTheTreeIsNotedAfterAPassingVerification(t *testing.T) {
	rec, h := leftoverRun(t, true, 0, 2)

	noted := sole[journal.LeftoverNoted](t, h.events())
	if len(noted.Names) != 1 || noted.Names[0].Name != "old_name" || noted.Names[0].Edits != 2 ||
		!slices.Equal(noted.Names[0].Locations, []string{"f.txt:5"}) {
		t.Errorf("noted = %+v, want old_name removed by 2 edits, still at f.txt:5", noted.Names)
	}
	if got := told(rec, 1); !strings.Contains(got, "`old_name` was removed from lines by 2 of your edits and still appears at f.txt:5") {
		t.Errorf("the model was not told after verification:\n%s", got)
	}
}

func TestTheNoteIsOffByDefault(t *testing.T) {
	rec, h := leftoverRun(t, false, 0, 2)
	for _, ty := range h.types() {
		if ty == journal.TypeLeftoverNoted {
			t.Fatal("a LeftoverNoted was written with the note off")
		}
	}
	if strings.Contains(told(rec, 1), "was removed from lines") {
		t.Error("the model was told with the note off")
	}
}

func TestNoNoteWhenTheNameIsGone(t *testing.T) {
	_, h := leftoverRun(t, true, 0, 2, 4)
	for _, ty := range h.types() {
		if ty == journal.TypeLeftoverNoted {
			t.Fatal("noted a name that no longer appears anywhere")
		}
	}
}

func TestOneEditIsNotEnough(t *testing.T) {
	_, h := leftoverRun(t, true, 0)
	for _, ty := range h.types() {
		if ty == journal.TypeLeftoverNoted {
			t.Fatal("noted a name removed by a single edit; the threshold is two")
		}
	}
}
