package engine_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/leejianrong/kopicode/internal/anchor"
	"github.com/leejianrong/kopicode/internal/journal"
	"github.com/leejianrong/kopicode/internal/tools"
)

// TestAnAnchorThisRepliesEditMadeStaleIsTranslatedAndJournaled holds ADR-0038 at
// the engine seam: two edits to neighbouring lines written against one read in
// one reply both apply and the second says it was translated; the same stale
// anchor in the next reply is refused, because the map is one reply's.
func TestAnAnchorThisRepliesEditMadeStaleIsTranslatedAndJournaled(t *testing.T) {
	a := anchor.Derive(anchor.Split([]byte(greetGo)))
	edit := func(id string, line int, text string) wireCall {
		args, err := json.Marshal(map[string]string{
			"path": "greet.go", "anchor_start": a[line], "anchor_end": a[line], "new_text": text,
		})
		if err != nil {
			t.Fatal(err)
		}
		return nativeCall(id, tools.ToolEditFile, string(args))
	}
	replies := []scriptedReply{
		// Lines five and six, whose windows overlap, from one read.
		{calls: []wireCall{
			edit("e1", 5, "\tcase \"\":\n"),
			edit("e2", 6, "\t\treturn \"stranger\"\n"),
		}},
		// Line four was re-anchored by the first edit, in the reply before this one.
		{calls: []wireCall{edit("e3", 4, "\tswitch strings.TrimSpace(name) {\n")}},
		{text: "Done."},
	}
	h := scriptHarness(t, replies, oneAttemptPerTurn(3), map[string]string{"greet.go": greetGo}, withMaxTurns(3))
	if _, err := h.eng.Run(t.Context(), "edit it"); err != nil {
		t.Fatalf("Run: %v", err)
	}

	evs := h.events()
	applied := payloadsOf[journal.EditApplied](t, evs)
	if len(applied) != 2 {
		t.Fatalf("got %d EditApplied, want 2 (e1 and e2)", len(applied))
	}
	if len(applied[0].Translated) != 0 {
		t.Errorf("the first edit translated %+v; there was nothing to translate", applied[0].Translated)
	}
	if got := applied[1].Translated; len(got) != 2 || got[0].From != a[6] || got[0].Line != 7 {
		t.Errorf("the second edit translated %+v, want start and end for line 7 from %s", got, a[6])
	}
	if applied[1].AnchorStart != a[6] {
		t.Errorf("AnchorStart = %q, want what the model sent, %q", applied[1].AnchorStart, a[6])
	}
	if applied[1].Mode != tools.ModeAnchored {
		t.Errorf("mode = %q, want anchored: a translation is not a fuzzy match", applied[1].Mode)
	}

	rej := payloadsOf[journal.EditRejected](t, evs)
	if len(rej) != 1 || rej[0].CallID != "e3" || rej[0].Reason != string(tools.RejectAnchorDrift) {
		t.Fatalf("rejected = %+v, want e3 as anchor_drift: the next reply has no map", rej)
	}

	got, err := os.ReadFile(filepath.Join(h.root, "greet.go"))
	if err != nil {
		t.Fatal(err)
	}
	if want := "\tcase \"\":\n\t\treturn \"stranger\"\n"; !strings.Contains(string(got), want) {
		t.Errorf("file does not hold both edits:\n%s", got)
	}
}
