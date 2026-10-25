package tools_test

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"strings"
	"testing"

	"github.com/leejianrong/kopicode/internal/tools"
)

// ADR-0038. Every test here holds one half of the rule: an anchor this reply's
// own edit made stale is translated, and nothing else is.

const sevenLines = "one\ntwo\nthree\nfour\nfive\nsix\nseven\n"

func TestReplyTranslatesAnAnchorItsOwnEditMadeStale(t *testing.T) {
	f := newFixture(t, map[string]string{"f.txt": sevenLines})
	s := f.set(t)
	a := derivedAnchors(sevenLines)
	reply := tools.NewReplyAnchors()

	applyEdit(t, s, tools.EditRequest{Path: "f.txt", AnchorStart: a[2], AnchorEnd: a[2], NewText: "THREE\n", Reply: reply})
	// Written before the first result existed: line four's anchor from the read.
	res := applyEdit(t, s, tools.EditRequest{Path: "f.txt", AnchorStart: a[3], AnchorEnd: a[3], NewText: "FOUR\n", Reply: reply})

	if got, want := onDisk(t, f, "f.txt"), "one\ntwo\nTHREE\nFOUR\nfive\nsix\nseven\n"; got != want {
		t.Errorf("file = %q, want %q", got, want)
	}
	if len(res.Translated) != 2 || res.Translated[0].Which != "start" || res.Translated[1].Which != "end" ||
		res.Translated[0].From != a[3] || res.Translated[0].Line != 4 {
		t.Errorf("translated = %+v, want start and end for line 4 from %s", res.Translated, a[3])
	}
	if res.AnchorStart != a[3] {
		t.Errorf("AnchorStart = %q, want what the model sent, %q", res.AnchorStart, a[3])
	}
	if !strings.Contains(res.Output, "was this reply's earlier anchor for line 4") {
		t.Errorf("the result does not say a translation happened:\n%s", res.Output)
	}
}

// The guard that matters most: an anchor from an older read that is not this
// reply's own `Old` is refused with the file byte-identical, in a reply that did
// translate another one.
func TestReplyStillRefusesAnAnchorFromAnOlderRead(t *testing.T) {
	f := newFixture(t, map[string]string{"f.txt": sevenLines})
	s := f.set(t)
	a := derivedAnchors(sevenLines)

	// An earlier reply edits line six, so line seven is re-anchored under it.
	applyEdit(t, s, tools.EditRequest{Path: "f.txt", AnchorStart: a[5], AnchorEnd: a[5], NewText: "SIX\n", Reply: tools.NewReplyAnchors()})
	stale := derivedAnchors(onDisk(t, f, "f.txt"))

	reply := tools.NewReplyAnchors()
	applyEdit(t, s, tools.EditRequest{Path: "f.txt", AnchorStart: stale[2], AnchorEnd: stale[2], NewText: "THREE\n", Reply: reply})

	// a[6] is an older read's anchor for line seven; this reply never made it
	// stale.
	rej := rejectEdit(t, f, s, "f.txt", tools.EditRequest{
		Path: "f.txt", AnchorStart: a[6], AnchorEnd: a[6], NewText: "SEVEN\n", Reply: reply})
	if rej.Reason != tools.RejectAnchorDrift {
		t.Errorf("reason = %q, want %q", rej.Reason, tools.RejectAnchorDrift)
	}
}

func TestReplyMapDoesNotCarryToTheNextReply(t *testing.T) {
	f := newFixture(t, map[string]string{"f.txt": sevenLines})
	s := f.set(t)
	a := derivedAnchors(sevenLines)
	applyEdit(t, s, tools.EditRequest{Path: "f.txt", AnchorStart: a[2], AnchorEnd: a[2], NewText: "THREE\n", Reply: tools.NewReplyAnchors()})

	rej := rejectEdit(t, f, s, "f.txt", tools.EditRequest{
		Path: "f.txt", AnchorStart: a[3], AnchorEnd: a[3], NewText: "FOUR\n", Reply: tools.NewReplyAnchors()})
	if rej.Reason != tools.RejectAnchorDrift {
		t.Errorf("reason = %q, want drift", rej.Reason)
	}
}

func TestReplyMapIsPerFile(t *testing.T) {
	f := newFixture(t, map[string]string{"a.txt": sevenLines, "b.txt": sevenLines})
	s := f.set(t)
	a := derivedAnchors(sevenLines)
	// b.txt's line four was re-anchored by an edit in an earlier reply, so a[3]
	// is stale there and no entry of b's own names it.
	applyEdit(t, s, tools.EditRequest{Path: "b.txt", AnchorStart: a[2], AnchorEnd: a[2], NewText: "THREE\n", Reply: tools.NewReplyAnchors()})

	reply := tools.NewReplyAnchors()
	applyEdit(t, s, tools.EditRequest{Path: "a.txt", AnchorStart: a[2], AnchorEnd: a[2], NewText: "THREE\n", Reply: reply})
	rej := rejectEdit(t, f, s, "b.txt", tools.EditRequest{
		Path: "b.txt", AnchorStart: a[3], AnchorEnd: a[3], NewText: "FOUR\n", Reply: reply})
	if rej.Reason != tools.RejectAnchorDrift {
		t.Errorf("reason = %q, want drift", rej.Reason)
	}
}

func TestReplyDropsAnEntryWhoseLineALaterEditReplaced(t *testing.T) {
	f := newFixture(t, map[string]string{"f.txt": sevenLines})
	s := f.set(t)
	a := derivedAnchors(sevenLines)
	reply := tools.NewReplyAnchors()
	applyEdit(t, s, tools.EditRequest{Path: "f.txt", AnchorStart: a[2], AnchorEnd: a[2], NewText: "THREE\n", Reply: reply})
	// Line four is remembered; replace it with a call that translates, then
	// name it again: its old anchor no longer has a same line.
	applyEdit(t, s, tools.EditRequest{Path: "f.txt", AnchorStart: a[3], AnchorEnd: a[3], NewText: "FOUR\n", Reply: reply})

	rej := rejectEdit(t, f, s, "f.txt", tools.EditRequest{
		Path: "f.txt", AnchorStart: a[3], AnchorEnd: a[3], NewText: "again\n", Reply: reply})
	if rej.Reason != tools.RejectAnchorDrift {
		t.Errorf("reason = %q, want drift", rej.Reason)
	}
}

func TestReplyFollowsALineThroughASecondEdit(t *testing.T) {
	f := newFixture(t, map[string]string{"f.txt": sevenLines})
	s := f.set(t)
	a := derivedAnchors(sevenLines)
	reply := tools.NewReplyAnchors()
	// Line four is re-anchored by the edit above it, then again by the one below.
	applyEdit(t, s, tools.EditRequest{Path: "f.txt", AnchorStart: a[2], AnchorEnd: a[2], NewText: "THREE\nthree and a half\n", Reply: reply})
	applyEdit(t, s, tools.EditRequest{Path: "f.txt", AnchorStart: a[4], AnchorEnd: a[4], NewText: "FIVE\n", Reply: reply})
	applyEdit(t, s, tools.EditRequest{Path: "f.txt", AnchorStart: a[3], AnchorEnd: a[3], NewText: "FOUR\n", Reply: reply})

	if got, want := onDisk(t, f, "f.txt"), "one\ntwo\nTHREE\nthree and a half\nFOUR\nFIVE\nsix\nseven\n"; got != want {
		t.Errorf("file = %q, want %q", got, want)
	}
}

func TestReplyRefusesAnAnchorWhoseLineWasDeleted(t *testing.T) {
	f := newFixture(t, map[string]string{"f.txt": sevenLines})
	s := f.set(t)
	a := derivedAnchors(sevenLines)
	reply := tools.NewReplyAnchors()
	// Lines three to five go; line six is re-anchored. Line four has no entry.
	applyEdit(t, s, tools.EditRequest{Path: "f.txt", AnchorStart: a[2], AnchorEnd: a[4], NewText: "", Reply: reply})
	rej := rejectEdit(t, f, s, "f.txt", tools.EditRequest{
		Path: "f.txt", AnchorStart: a[3], AnchorEnd: a[3], NewText: "x\n", Reply: reply})
	if rej.Reason != tools.RejectAnchorDrift {
		t.Errorf("reason = %q, want drift", rej.Reason)
	}
}

func TestReplyRefusesWhenTheCurrentAnchorWouldBeAmbiguous(t *testing.T) {
	// The first A sits in the window (P, A, Q) and the second in (P, A, R).
	// Replacing Q with R gives the first A the second one's anchor, so the
	// translated anchor would name two lines.
	body := "P\nA\nQ\nm\nP\nA\nR\n"
	f := newFixture(t, map[string]string{"f.txt": body})
	s := f.set(t)
	a := derivedAnchors(body)
	reply := tools.NewReplyAnchors()
	applyEdit(t, s, tools.EditRequest{Path: "f.txt", AnchorStart: a[2], AnchorEnd: a[2], NewText: "R\n", Reply: reply})

	rej := rejectEdit(t, f, s, "f.txt", tools.EditRequest{
		Path: "f.txt", AnchorStart: a[1], AnchorEnd: a[1], NewText: "a\n", Reply: reply})
	if rej.Reason != tools.RejectAnchorDrift {
		t.Errorf("reason = %q, want the refusal the call would have had without the rule", rej.Reason)
	}
}

func TestReplyIsClearedByAnythingButAnEdit(t *testing.T) {
	f := newFixture(t, map[string]string{"f.txt": sevenLines})
	s := f.set(t)
	a := derivedAnchors(sevenLines)
	reply := tools.NewReplyAnchors()
	applyEdit(t, s, tools.EditRequest{Path: "f.txt", AnchorStart: a[2], AnchorEnd: a[2], NewText: "THREE\n", Reply: reply})

	reply.Reset() // what the engine does after a write_file, delete_file, shell or fuzzy edit
	rej := rejectEdit(t, f, s, "f.txt", tools.EditRequest{
		Path: "f.txt", AnchorStart: a[3], AnchorEnd: a[3], NewText: "FOUR\n", Reply: reply})
	if rej.Reason != tools.RejectAnchorDrift {
		t.Errorf("reason = %q, want drift", rej.Reason)
	}
}

func TestReplyIsVoidWhenTheFileChangedUnderIt(t *testing.T) {
	f := newFixture(t, map[string]string{"f.txt": sevenLines})
	s := f.set(t)
	a := derivedAnchors(sevenLines)
	reply := tools.NewReplyAnchors()
	applyEdit(t, s, tools.EditRequest{Path: "f.txt", AnchorStart: a[2], AnchorEnd: a[2], NewText: "THREE\n", Reply: reply})

	// Something other than an applied edit_file rewrites the file.
	if _, err := s.WriteFile(context.Background(), tools.WriteRequest{
		Path: "f.txt", Content: onDisk(t, f, "f.txt") + "eight\n"}); err != nil {
		t.Fatal(err)
	}
	rej := rejectEdit(t, f, s, "f.txt", tools.EditRequest{
		Path: "f.txt", AnchorStart: a[3], AnchorEnd: a[3], NewText: "FOUR\n", Reply: reply})
	if rej.Reason != tools.RejectAnchorDrift {
		t.Errorf("reason = %q, want drift", rej.Reason)
	}
}

func TestNilReplyIsTheRuleOff(t *testing.T) {
	f := newFixture(t, map[string]string{"f.txt": sevenLines})
	s := f.set(t)
	a := derivedAnchors(sevenLines)
	applyEdit(t, s, tools.EditRequest{Path: "f.txt", AnchorStart: a[2], AnchorEnd: a[2], NewText: "THREE\n"})
	rejectEdit(t, f, s, "f.txt", tools.EditRequest{Path: "f.txt", AnchorStart: a[3], AnchorEnd: a[3], NewText: "FOUR\n"})
}

// A live match is never translated: an anchor that is both an earlier anchor and
// a current one takes the line it is on now.
func TestReplyNeverTranslatesAnAnchorThatMatchesALine(t *testing.T) {
	f := newFixture(t, map[string]string{"f.txt": sevenLines})
	s := f.set(t)
	a := derivedAnchors(sevenLines)
	reply := tools.NewReplyAnchors()
	// Line one's anchor is unchanged by an edit to line four: it is live.
	applyEdit(t, s, tools.EditRequest{Path: "f.txt", AnchorStart: a[3], AnchorEnd: a[3], NewText: "FOUR\n", Reply: reply})
	res := applyEdit(t, s, tools.EditRequest{Path: "f.txt", AnchorStart: a[0], AnchorEnd: a[0], NewText: "ONE\n", Reply: reply})
	if len(res.Translated) != 0 {
		t.Errorf("translated = %+v, want none for a live anchor", res.Translated)
	}
}

// An anchor that is an earlier anchor of one line and the current anchor of
// another takes the line it is on now. (That is the ordinary rule, and the
// ambiguous-original hazard ADR-0038 records as not closed: the rule adds no
// second reading.)
func TestReplyLiveMatchBeatsATranslation(t *testing.T) {
	body := "P\nA\nQ\nm\nP\nA\nS\n"
	f := newFixture(t, map[string]string{"f.txt": body})
	s := f.set(t)
	a := derivedAnchors(body)
	reply := tools.NewReplyAnchors()
	// The first A is re-anchored from (P,A,Q) to (P,A,S); then the second A takes
	// the window (P,A,Q), so a[1] is live on line six and was line two's.
	applyEdit(t, s, tools.EditRequest{Path: "f.txt", AnchorStart: a[2], AnchorEnd: a[2], NewText: "S\n", Reply: reply})
	applyEdit(t, s, tools.EditRequest{Path: "f.txt", AnchorStart: a[6], AnchorEnd: a[6], NewText: "Q\n", Reply: reply})

	res := applyEdit(t, s, tools.EditRequest{Path: "f.txt", AnchorStart: a[1], AnchorEnd: a[1], NewText: "a\n", Reply: reply})
	if len(res.Translated) != 0 {
		t.Errorf("translated = %+v, want none: the anchor matches line six", res.Translated)
	}
	if got, want := onDisk(t, f, "f.txt"), "P\nA\nS\nm\nP\na\nQ\n"; got != want {
		t.Errorf("file = %q, want %q", got, want)
	}
}

// The property the rule rests on, over many shapes: edits written against one
// read are applied to exactly the lines they named, or refused with the file
// untouched. Lines and replacement text are all distinct, so no window recurs
// and "the line it named" is unambiguous; a translation that ever lands
// elsewhere, or a surviving line that is refused, fails.
func TestReplyAppliesToTheNamedLineOrRefusesAcrossShapes(t *testing.T) {
	for seed := int64(0); seed < 300; seed++ {
		t.Run(fmt.Sprint(seed), func(t *testing.T) {
			rng := rand.New(rand.NewSource(seed))
			n := 4 + rng.Intn(12)
			type line struct{ id, text string }
			var model []line
			var b strings.Builder
			for i := 0; i < n; i++ {
				l := line{fmt.Sprintf("o%d", i), fmt.Sprintf("orig line %d", i)}
				model = append(model, l)
				b.WriteString(l.text + "\n")
			}
			f := newFixture(t, map[string]string{"f.txt": b.String()})
			s := f.set(t)
			a0 := derivedAnchors(b.String())
			reply := tools.NewReplyAnchors()

			fresh := 0
			for call := 0; call < 2+rng.Intn(6); call++ {
				i := rng.Intn(n)
				j := i
				if rng.Intn(3) == 0 {
					j = i + rng.Intn(min(3, n-i))
				}
				var repl []line
				var text strings.Builder
				for k := rng.Intn(3); k > 0; k-- {
					fresh++
					l := line{fmt.Sprintf("n%d", fresh), fmt.Sprintf("new line %d", fresh)}
					repl = append(repl, l)
					text.WriteString(l.text + "\n")
				}

				pos := func(id string) int {
					for p, l := range model {
						if l.id == id {
							return p
						}
					}
					return -1
				}
				pi, pj := pos(fmt.Sprintf("o%d", i)), pos(fmt.Sprintf("o%d", j))
				survives := pi >= 0 && pj >= 0 && pi <= pj

				before := onDisk(t, f, "f.txt")
				_, err := s.EditFile(context.Background(), tools.EditRequest{
					Path: "f.txt", AnchorStart: a0[i], AnchorEnd: a0[j], NewText: text.String(), Reply: reply})

				switch {
				case survives && err != nil:
					t.Fatalf("call %d: lines %d..%d survive but the edit was refused: %v\nfile %q", call, i, j, err, before)
				case !survives && err == nil:
					t.Fatalf("call %d: lines %d..%d do not all survive in order but the edit applied\nbefore %q\nafter  %q",
						call, i, j, before, onDisk(t, f, "f.txt"))
				case !survives:
					if !errors.Is(err, tools.ErrEditRejected) {
						t.Fatalf("call %d: %v", call, err)
					}
					if after := onDisk(t, f, "f.txt"); after != before {
						t.Fatalf("call %d: a refused edit changed the file", call)
					}
					continue
				}
				model = append(append(append([]line{}, model[:pi]...), repl...), model[pj+1:]...)
				var want strings.Builder
				for _, l := range model {
					want.WriteString(l.text + "\n")
				}
				if got := onDisk(t, f, "f.txt"); got != want.String() {
					t.Fatalf("call %d: lines %d..%d\n got %q\nwant %q", call, i, j, got, want.String())
				}
			}
		})
	}
}
