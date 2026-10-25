package tools_test

import (
	"context"
	"strings"
	"testing"

	"github.com/leejianrong/kopicode/internal/tools"
)

// KOP-225. "A" sits in the window (P, A, Q) twice, so its anchor names two lines
// when the file is read. Editing the first Q re-anchors the first A, leaving the
// old anchor on the second alone.
const repeatedWindow = "P\nA\nQ\nm\nP\nA\nQ\n"

func readFor(t *testing.T, s *tools.Set, path string) {
	t.Helper()
	if _, err := s.ReadFile(context.Background(), tools.ReadRequest{Path: path}); err != nil {
		t.Fatalf("ReadFile(%s): %v", path, err)
	}
}

func TestAnAnchorThatWasAmbiguousAtReadIsRefusedWhenAnEditMakesItUnique(t *testing.T) {
	f := newFixture(t, map[string]string{"f.txt": repeatedWindow})
	s := f.set(t)
	a := derivedAnchors(repeatedWindow)
	if a[1] != a[5] {
		t.Fatalf("setup: the two A lines should share an anchor, got %s and %s", a[1], a[5])
	}
	readFor(t, s, "f.txt")
	applyEdit(t, s, tools.EditRequest{Path: "f.txt", AnchorStart: a[2], AnchorEnd: a[2], NewText: "S\n"})

	// The anchor now names line six alone; it would have applied there.
	rej := rejectEdit(t, f, s, "f.txt", tools.EditRequest{
		Path: "f.txt", AnchorStart: a[1], AnchorEnd: a[1], NewText: "a\n"})
	if rej.Reason != tools.RejectAmbiguousAnchor {
		t.Errorf("reason = %q, want %q", rej.Reason, tools.RejectAmbiguousAnchor)
	}
	if !strings.Contains(rej.Summary, "matched more than one line when you read the file") {
		t.Errorf("summary does not say why: %s", rej.Summary)
	}
}

func TestAReadAfterTheEditClearsIt(t *testing.T) {
	f := newFixture(t, map[string]string{"f.txt": repeatedWindow})
	s := f.set(t)
	a := derivedAnchors(repeatedWindow)
	readFor(t, s, "f.txt")
	applyEdit(t, s, tools.EditRequest{Path: "f.txt", AnchorStart: a[2], AnchorEnd: a[2], NewText: "S\n"})

	readFor(t, s, "f.txt") // the file as it is: the anchor is unique, and the model can see it
	applyEdit(t, s, tools.EditRequest{Path: "f.txt", AnchorStart: a[1], AnchorEnd: a[1], NewText: "a\n"})
	if got, want := onDisk(t, f, "f.txt"), "P\nA\nS\nm\nP\na\nQ\n"; got != want {
		t.Errorf("file = %q, want %q", got, want)
	}
}

func TestAnAnchorThatWasUniqueAtReadIsNotAffected(t *testing.T) {
	body := "one\ntwo\nthree\nfour\nfive\n"
	f := newFixture(t, map[string]string{"f.txt": body})
	s := f.set(t)
	a := derivedAnchors(body)
	readFor(t, s, "f.txt")
	applyEdit(t, s, tools.EditRequest{Path: "f.txt", AnchorStart: a[0], AnchorEnd: a[0], NewText: "ONE\n"})
	applyEdit(t, s, tools.EditRequest{Path: "f.txt", AnchorStart: a[4], AnchorEnd: a[4], NewText: "FIVE\n"})
}

func TestNoReadNoLedger(t *testing.T) {
	// Without a read the ledger is empty, so behaviour is what it was: the
	// limit is stated on the type, and this holds it.
	f := newFixture(t, map[string]string{"f.txt": repeatedWindow})
	s := f.set(t)
	a := derivedAnchors(repeatedWindow)
	applyEdit(t, s, tools.EditRequest{Path: "f.txt", AnchorStart: a[2], AnchorEnd: a[2], NewText: "S\n"})
	applyEdit(t, s, tools.EditRequest{Path: "f.txt", AnchorStart: a[1], AnchorEnd: a[1], NewText: "a\n"})
}

func TestAWriteClearsTheLedger(t *testing.T) {
	f := newFixture(t, map[string]string{"f.txt": repeatedWindow})
	s := f.set(t)
	a := derivedAnchors(repeatedWindow)
	readFor(t, s, "f.txt")
	applyEdit(t, s, tools.EditRequest{Path: "f.txt", AnchorStart: a[2], AnchorEnd: a[2], NewText: "S\n"})
	if _, err := s.WriteFile(context.Background(), tools.WriteRequest{Path: "f.txt", Content: onDisk(t, f, "f.txt")}); err != nil {
		t.Fatal(err)
	}
	applyEdit(t, s, tools.EditRequest{Path: "f.txt", AnchorStart: a[1], AnchorEnd: a[1], NewText: "a\n"})
}
