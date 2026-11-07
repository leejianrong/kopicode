package engine_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/leejianrong/kopicode/internal/anchor"
	"github.com/leejianrong/kopicode/internal/engine"
	"github.com/leejianrong/kopicode/internal/journal"
	"github.com/leejianrong/kopicode/internal/tools"
)

// ADR-0040 at the engine seam: an edit replaces only the `def` line with a whole
// new function, so the old body is left below the new return.
const aftercareBody = "def f(x):\n    return x\n"

func aftercareRun(t *testing.T, on bool, path string) (*recordingProvider, *harness) {
	t.Helper()
	a := anchor.Derive(anchor.Split([]byte(aftercareBody)))
	args, err := json.Marshal(map[string]string{
		"path": path, "anchor_start": a[0], "anchor_end": a[0],
		"new_text": "def f(x):\n    y = x\n    return y\n",
	})
	if err != nil {
		t.Fatal(err)
	}
	replies := []scriptedReply{{calls: []wireCall{nativeCall("e1", tools.ToolEditFile, string(args))}}, {text: "Done."}}
	prov := script(t, replies, oneAttemptPerTurn(len(replies)))
	rec := &recordingProvider{inner: prov}
	h := newHarness(t, prov, map[string]string{path: aftercareBody},
		withProvider(rec), withMaxTurns(3),
		func(c *engine.Config) { c.Selection.Config.EditAftercare = on })
	if _, err := h.eng.Run(t.Context(), "rewrite f"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	return rec, h
}

func applied(t *testing.T, h *harness) journal.EditApplied {
	t.Helper()
	return sole[journal.EditApplied](t, h.events())
}

func TestADeadLineAnEditLeftIsJournaledWhetherOrNotTheModelIsTold(t *testing.T) {
	for _, on := range []bool{false, true} {
		_, h := aftercareRun(t, on, "f.py")
		got := applied(t, h).Unreachable
		if len(got) != 1 || got[0].Line != 3 || got[0].Next != 4 {
			t.Errorf("on=%v: unreachable = %+v, want return at 3 and the dead line at 4", on, got)
		}
	}
}

func TestTheNoteIsShownOnlyWhenTheSettingIsOn(t *testing.T) {
	rec, _ := aftercareRun(t, true, "f.py")
	got := told(rec, 1)
	for _, want := range []string{"kopicode: after this edit, line 3 (`return y`)", "line 4 (`return x`)", "can never run", "your range ended short"} {
		if !strings.Contains(got, want) {
			t.Errorf("the model was not told %q:\n%s", want, got)
		}
	}
	rec, _ = aftercareRun(t, false, "f.py")
	if strings.Contains(told(rec, 1), "can never run") {
		t.Error("the model was told with the setting off")
	}
}

func TestNothingIsReadFromAFileThatIsNotPython(t *testing.T) {
	// The same bytes in a .txt file: no pair is looked for.
	rec, h := aftercareRun(t, true, "f.txt")
	if got := applied(t, h).Unreachable; len(got) != 0 {
		t.Errorf("unreachable = %+v for a .txt file", got)
	}
	if strings.Contains(told(rec, 1), "can never run") {
		t.Error("the model was told about a .txt file")
	}
}
