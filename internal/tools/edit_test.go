package tools_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/leejianrong/kopicode/internal/anchor"
	"github.com/leejianrong/kopicode/internal/tools"
)

// derivedAnchors returns the file's current anchors, one per line, derived the same
// way read_file renders them. Every test that edits names its region this way
// rather than pasting a literal: an anchor literal in a test would pin the
// hash, so an anchor.Version bump would fail here as a wall of unrelated
// diffs instead of as the one thing it is.
func derivedAnchors(body string) []string {
	return anchor.Derive(anchor.Split([]byte(body)))
}

// onDisk reads a fixture file back as raw bytes. Rejection tests assert the
// file is *byte-identical*, not merely that an error came back: an edit that
// silently no-op'd and an edit that was refused are indistinguishable from the
// error alone, and the first is the defect ADR-0006 exists to prevent.
func onDisk(t *testing.T, f *fixture, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(f.root, filepath.FromSlash(name)))
	if err != nil {
		t.Fatalf("reading %s back: %v", name, err)
	}
	return string(b)
}

// editSample is the file the placement tests edit. Seven lines, no duplicates, so
// every line has a unique anchor and a rejection in these tests is never
// ambiguity wearing a different hat.
const editSample = `package main

import "fmt"

func main() {
	fmt.Println("hello")
}
`

// apply runs an edit that is expected to succeed.
func applyEdit(t *testing.T, s *tools.Set, req tools.EditRequest) tools.EditResult {
	t.Helper()
	res, err := s.EditFile(context.Background(), req)
	if err != nil {
		t.Fatalf("EditFile(%+v): %v", req, err)
	}
	if res.Rejection != nil {
		t.Fatalf("EditFile(%+v): rejected: %s", req, res.Rejection.Summary)
	}
	return res
}

// rejectEdit runs an edit that is expected to be refused, and holds the two
// halves of "fails closed" together: the refusal is structured, and the file
// did not move.
func rejectEdit(t *testing.T, f *fixture, s *tools.Set, name string, req tools.EditRequest) *tools.EditRejection {
	t.Helper()
	before := onDisk(t, f, name)

	res, err := s.EditFile(context.Background(), req)
	if err == nil {
		t.Fatalf("EditFile(%+v) was accepted; it must be refused", req)
	}
	if !errors.Is(err, tools.ErrEditRejected) {
		t.Fatalf("EditFile(%+v): error %v does not wrap ErrEditRejected", req, err)
	}
	wantFault(t, err, tools.FaultTask)

	if res.Rejection == nil {
		t.Fatalf("EditFile(%+v) refused with no structured rejection; the engine "+
			"cannot journal EditRejected without inventing one", req)
	}
	if after := onDisk(t, f, name); after != before {
		t.Fatalf("the file changed on a rejected edit:\nbefore %q\nafter  %q", before, after)
	}
	return res.Rejection
}

// --- placement --------------------------------------------------------------

// TestEditAppliesAtEveryPosition is the test plan's positional list. Off-by-one
// at a file boundary is the classic way an anchored editor corrupts a file
// while every happy-path test in the middle stays green, so the first line, the
// last line, one line and a span are all named cases.
func TestEditAppliesAtEveryPosition(t *testing.T) {
	tests := []struct {
		name       string
		start, end int // 1-based lines, inclusive
		newText    string
		want       string
		removed    int
		added      int
	}{
		{
			name:  "start of file, single line",
			start: 1, end: 1,
			newText: "package tools\n",
			want:    "package tools\n\nimport \"fmt\"\n\nfunc main() {\n\tfmt.Println(\"hello\")\n}\n",
			removed: 1, added: 1,
		},
		{
			name:  "end of file, single line",
			start: 7, end: 7,
			newText: "} // done\n",
			want:    "package main\n\nimport \"fmt\"\n\nfunc main() {\n\tfmt.Println(\"hello\")\n} // done\n",
			removed: 1, added: 1,
		},
		{
			name:  "a multi-line region",
			start: 5, end: 7,
			newText: "func main() {\n\tfmt.Println(\"goodbye\")\n\tfmt.Println(\"again\")\n}\n",
			want: "package main\n\nimport \"fmt\"\n\nfunc main() {\n\tfmt.Println(\"goodbye\")\n" +
				"\tfmt.Println(\"again\")\n}\n",
			removed: 3, added: 4,
		},
		{
			name:  "the whole file",
			start: 1, end: 7,
			newText: "package other\n",
			want:    "package other\n",
			removed: 7, added: 1,
		},
		{
			name:  "a blank line in the middle",
			start: 2, end: 2,
			newText: "// a comment\n",
			want:    "package main\n// a comment\nimport \"fmt\"\n\nfunc main() {\n\tfmt.Println(\"hello\")\n}\n",
			removed: 1, added: 1,
		},
		{
			name:  "deleting the first line",
			start: 1, end: 1,
			newText: "",
			want:    "\nimport \"fmt\"\n\nfunc main() {\n\tfmt.Println(\"hello\")\n}\n",
			removed: 1, added: 0,
		},
		{
			name:  "deleting the last line",
			start: 7, end: 7,
			newText: "",
			want:    "package main\n\nimport \"fmt\"\n\nfunc main() {\n\tfmt.Println(\"hello\")\n",
			removed: 1, added: 0,
		},
		{
			name:  "deleting a middle region",
			start: 2, end: 4,
			newText: "",
			want:    "package main\nfunc main() {\n\tfmt.Println(\"hello\")\n}\n",
			removed: 3, added: 0,
		},
		{
			name:  "deleting every line leaves an empty file",
			start: 1, end: 7,
			newText: "",
			want:    "",
			removed: 7, added: 0,
		},
		{
			name:  "a replacement without a trailing newline is still a line",
			start: 6, end: 6,
			newText: "\tfmt.Println(\"bye\")",
			want:    "package main\n\nimport \"fmt\"\n\nfunc main() {\n\tfmt.Println(\"bye\")\n}\n",
			removed: 1, added: 1,
		},
		{
			// "\n" is one blank line; "" is no lines, which is the deletion
			// above. Splitting NewText the way a file is split is what keeps
			// those two apart without a separate delete flag.
			name:  "replacing a line with a blank one is a newline, not an empty string",
			start: 6, end: 6,
			newText: "\n",
			want:    "package main\n\nimport \"fmt\"\n\nfunc main() {\n\n}\n",
			removed: 1, added: 1,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t, map[string]string{"main.go": editSample})
			s := f.set(t)
			a := derivedAnchors(editSample)

			res := applyEdit(t, s, tools.EditRequest{
				Path:        "main.go",
				AnchorStart: a[tc.start-1],
				AnchorEnd:   a[tc.end-1],
				NewText:     tc.newText,
			})

			if got := onDisk(t, f, "main.go"); got != tc.want {
				t.Errorf("file on disk =\n%q\nwant\n%q", got, tc.want)
			}
			if res.StartLine != tc.start || res.EndLine != tc.end {
				t.Errorf("region = %d-%d, want %d-%d", res.StartLine, res.EndLine, tc.start, tc.end)
			}
			if res.LinesRemoved != tc.removed || res.LinesAdded != tc.added {
				t.Errorf("removed/added = %d/%d, want %d/%d",
					res.LinesRemoved, res.LinesAdded, tc.removed, tc.added)
			}
			if res.Mode != tools.ModeAnchored {
				t.Errorf("mode = %q, want %q", res.Mode, tools.ModeAnchored)
			}
			if res.AnchorVersion != tools.AnchorVersion {
				t.Errorf("anchor version = %q, want %q", res.AnchorVersion, tools.AnchorVersion)
			}
			if res.BytesAfter != len(tc.want) {
				t.Errorf("BytesAfter = %d, want %d", res.BytesAfter, len(tc.want))
			}
		})
	}
}

// TestEditLeavesTheRestOfTheFileAlone is the byte-preservation promise, and it
// is why the file is spliced rather than rebuilt from its lines. Rebuilding
// would rewrite a CRLF checkout as LF — a whole-file change nobody asked for,
// invisible in a diff of the edited region, and enough to stale every anchor in
// the file at once.
func TestEditLeavesTheRestOfTheFileAlone(t *testing.T) {
	tests := []struct {
		name    string
		body    string
		line    int
		newText string
		want    string
	}{
		{
			name: "CRLF stays CRLF, including the edited line",
			body: "alpha\r\nbeta\r\ngamma\r\n",
			line: 2, newText: "BETA\n",
			want: "alpha\r\nBETA\r\ngamma\r\n",
		},
		{
			name: "CRLF at the first line",
			body: "alpha\r\nbeta\r\ngamma\r\n",
			line: 1, newText: "ALPHA\n",
			want: "ALPHA\r\nbeta\r\ngamma\r\n",
		},
		{
			name: "CRLF at the last line",
			body: "alpha\r\nbeta\r\ngamma\r\n",
			line: 3, newText: "GAMMA\n",
			want: "alpha\r\nbeta\r\nGAMMA\r\n",
		},
		{
			name: "a multi-line replacement takes the file's terminator",
			body: "alpha\r\nbeta\r\ngamma\r\n",
			line: 2, newText: "one\ntwo\n",
			want: "alpha\r\none\r\ntwo\r\ngamma\r\n",
		},
		{
			name: "no trailing newline stays that way",
			body: "alpha\nbeta\ngamma",
			line: 2, newText: "BETA\n",
			want: "alpha\nBETA\ngamma",
		},
		{
			name: "editing the last line of a file with no trailing newline",
			body: "alpha\nbeta\ngamma",
			line: 3, newText: "GAMMA",
			want: "alpha\nbeta\nGAMMA",
		},
		{
			name: "a one-line file",
			body: "alpha\n",
			line: 1, newText: "ALPHA\n",
			want: "ALPHA\n",
		},
		{
			name: "a one-line file with no trailing newline",
			body: "alpha",
			line: 1, newText: "ALPHA",
			want: "ALPHA",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t, map[string]string{"f.txt": tc.body})
			s := f.set(t)
			a := derivedAnchors(tc.body)

			applyEdit(t, s, tools.EditRequest{
				Path:        "f.txt",
				AnchorStart: a[tc.line-1],
				AnchorEnd:   a[tc.line-1],
				NewText:     tc.newText,
			})
			if got := onDisk(t, f, "f.txt"); got != tc.want {
				t.Errorf("file on disk = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestEditKeepsTheFileMode holds that editing a script does not strip its
// executable bit. os.Root.WriteFile applies the mode only when it creates, and
// this is the assertion that keeps it that way.
func TestEditKeepsTheFileMode(t *testing.T) {
	if os.Getenv("GOOS") == "windows" {
		t.Skip("file modes are not meaningful on Windows")
	}
	f := newFixture(t, map[string]string{"run.sh": "#!/bin/sh\necho hi\n"})
	path := filepath.Join(f.root, "run.sh")
	if err := os.Chmod(path, 0o755); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	before, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if before.Mode().Perm() != 0o755 {
		t.Skipf("the filesystem does not keep mode 0755 (got %s)", before.Mode())
	}

	s := f.set(t)
	a := derivedAnchors("#!/bin/sh\necho hi\n")
	applyEdit(t, s, tools.EditRequest{
		Path: "run.sh", AnchorStart: a[1], AnchorEnd: a[1], NewText: "echo bye\n",
	})

	after, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if after.Mode().Perm() != before.Mode().Perm() {
		t.Errorf("mode = %s, want %s", after.Mode().Perm(), before.Mode().Perm())
	}
}

// TestEditAnchorsStayValidOutsideTheChange is the property the whole scheme
// rests on: an anchor is a function of its line and its immediate neighbours,
// so an edit far away leaves it alone and the model can keep using anchors it
// read earlier. ADR-0006 §7 fixes the window at plus or minus one line for
// exactly this reason.
func TestEditAnchorsStayValidOutsideTheChange(t *testing.T) {
	body := "one\ntwo\nthree\nfour\nfive\nsix\nseven\neight\n"
	f := newFixture(t, map[string]string{"f.txt": body})
	s := f.set(t)
	before := derivedAnchors(body)

	applyEdit(t, s, tools.EditRequest{
		Path: "f.txt", AnchorStart: before[1], AnchorEnd: before[1], NewText: "TWO\n",
	})
	after := derivedAnchors(onDisk(t, f, "f.txt"))

	// Lines 1..3 are inside the blast radius of a change to line 2; 4 onwards
	// is not, and an anchor the model read moments ago must still work there.
	for i := 3; i < len(after); i++ {
		if after[i] != before[i] {
			t.Errorf("line %d's anchor changed (%s -> %s) although the edit was at line 2",
				i+1, before[i], after[i])
		}
	}

	// And the anchor read before the edit still names line 5 afterwards.
	res := applyEdit(t, s, tools.EditRequest{
		Path: "f.txt", AnchorStart: before[4], AnchorEnd: before[4], NewText: "FIVE\n",
	})
	if res.StartLine != 5 {
		t.Errorf("the pre-edit anchor for line 5 resolved to line %d", res.StartLine)
	}
}

// --- the diff ---------------------------------------------------------------

func TestEditReturnsTheDiffItApplied(t *testing.T) {
	f := newFixture(t, map[string]string{"main.go": editSample})
	s := f.set(t)
	a := derivedAnchors(editSample)

	res := applyEdit(t, s, tools.EditRequest{
		Path: "main.go", AnchorStart: a[5], AnchorEnd: a[5],
		NewText: "\tfmt.Println(\"goodbye\")\n",
	})

	want := "--- a/main.go\n" +
		"+++ b/main.go\n" +
		"@@ -3,5 +3,5 @@\n" +
		" import \"fmt\"\n" +
		" \n" +
		" func main() {\n" +
		"-\tfmt.Println(\"hello\")\n" +
		"+\tfmt.Println(\"goodbye\")\n" +
		" }\n"
	if res.Diff != want {
		t.Errorf("diff =\n%s\nwant\n%s", res.Diff, want)
	}
	if !strings.Contains(res.Output, res.Diff) {
		t.Errorf("the model-facing output does not carry the diff:\n%s", res.Output)
	}
}

func TestEditDiffHunkHeaders(t *testing.T) {
	tests := []struct {
		name       string
		body       string
		start, end int
		newText    string
		wantHeader string
	}{
		{
			name: "a single line either side omits the count, as diff(1) does",
			body: "one\n", start: 1, end: 1, newText: "ONE\n",
			wantHeader: "@@ -1 +1 @@",
		},
		{
			name: "an emptied file is numbered from the line before it",
			body: "one\ntwo\n", start: 1, end: 2, newText: "",
			wantHeader: "@@ -1,2 +0,0 @@",
		},
		{
			name: "context is bounded at three lines either side",
			body: "1\n2\n3\n4\n5\n6\n7\n8\n9\n", start: 5, end: 5, newText: "FIVE\n",
			wantHeader: "@@ -2,7 +2,7 @@",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t, map[string]string{"f.txt": tc.body})
			s := f.set(t)
			a := derivedAnchors(tc.body)

			res := applyEdit(t, s, tools.EditRequest{
				Path: "f.txt", AnchorStart: a[tc.start-1], AnchorEnd: a[tc.end-1], NewText: tc.newText,
			})
			if !strings.Contains(res.Diff, tc.wantHeader+"\n") {
				t.Errorf("diff does not contain %q:\n%s", tc.wantHeader, res.Diff)
			}
		})
	}
}

// --- rejection --------------------------------------------------------------

// TestEditRejectsADriftedAnchor is the slice's acceptance criterion: the file
// moved between the read and the edit, so the edit is refused, the file is
// unchanged, and the rejection carries the anchors as they are now.
func TestEditRejectsADriftedAnchor(t *testing.T) {
	f := newFixture(t, map[string]string{"main.go": editSample})
	s := f.set(t)
	stale := derivedAnchors(editSample)

	// Someone else changes the file after the model read it.
	drifted := strings.Replace(editSample, `fmt.Println("hello")`, `fmt.Println("hi")`, 1)
	f.write(t, "main.go", drifted)

	rej := rejectEdit(t, f, s, "main.go", tools.EditRequest{
		Path: "main.go", AnchorStart: stale[5], AnchorEnd: stale[5], NewText: "\tpanic(1)\n",
	})

	if rej.Reason != tools.RejectAnchorDrift {
		t.Errorf("reason = %q, want %q", rej.Reason, tools.RejectAnchorDrift)
	}
	if rej.Field != "anchor_start" {
		t.Errorf("field = %q, want %q", rej.Field, "anchor_start")
	}
	if rej.Anchor != stale[5] {
		t.Errorf("anchor = %q, want the argument %q", rej.Anchor, stale[5])
	}

	// The current anchors are the file's, as it is now, and they are the ones a
	// fresh read would show — that equality is what "retry against reality"
	// means, and it is asserted rather than eyeballed.
	current := derivedAnchors(drifted)
	rendered := anchor.Render(anchor.Split([]byte(drifted)))
	if len(rej.Current) != len(current) {
		t.Fatalf("got %d current anchors, want all %d lines of the drifted file",
			len(rej.Current), len(current))
	}
	for i, got := range rej.Current {
		if got.Line != i+1 || got.Anchor != current[i] || got.Rendered != rendered[i] {
			t.Errorf("current[%d] = %+v, want line %d anchor %s rendered %q",
				i, got, i+1, current[i], rendered[i])
		}
	}
	if !strings.Contains(rej.Detail, current[5]) {
		t.Errorf("the detail does not carry the region's current anchor:\n%s", rej.Detail)
	}
}

// TestEditRejectsAnInventedAnchor covers the two shapes an invented anchor
// takes, and they are deliberately different reasons. One is a plausible eight
// hex characters that names nothing, which is indistinguishable from drift and
// recovers the same way. The other is not an anchor at all, which no re-read
// will fix — the model has to be told the shape instead, and the rate of it is
// the direct measurement of SLICE-1 risk 2.
func TestEditRejectsAnInventedAnchor(t *testing.T) {
	tests := []struct {
		name   string
		anchor string
		want   tools.RejectReason
	}{
		{"well-formed but in no file", "deadbeef", tools.RejectAnchorDrift},
		{"a line number", "6", tools.RejectMalformedAnchor},
		{"uppercase hex", "DEADBEEF", tools.RejectMalformedAnchor},
		{"too short", "dead", tools.RejectMalformedAnchor},
		{"too long", "deadbeefcafe", tools.RejectMalformedAnchor},
		{"not hex at all", "zzzzzzzz", tools.RejectMalformedAnchor},
		{"empty", "", tools.RejectMalformedAnchor},
		{"the rendered line, copied whole", "a3f21b09   1| package main", tools.RejectMalformedAnchor},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t, map[string]string{"main.go": editSample})
			s := f.set(t)
			a := derivedAnchors(editSample)

			rej := rejectEdit(t, f, s, "main.go", tools.EditRequest{
				Path: "main.go", AnchorStart: tc.anchor, AnchorEnd: a[5], NewText: "x\n",
			})
			if rej.Reason != tc.want {
				t.Errorf("reason = %q, want %q", rej.Reason, tc.want)
			}
			if rej.Field != "anchor_start" {
				t.Errorf("field = %q, want %q", rej.Field, "anchor_start")
			}
		})
	}
}

// TestEditRejectsAnAmbiguousAnchor holds ADR-0006 §7's refusal rule. The file
// repeats a whole three-line window, so one anchor names two places; picking
// either would be a guess, and a guess that lands in the wrong one applies
// cleanly and is never seen again.
func TestEditRejectsAnAmbiguousAnchor(t *testing.T) {
	body := "a\nb\nc\nx\na\nb\nc\ny\n"
	f := newFixture(t, map[string]string{"f.txt": body})
	s := f.set(t)
	a := derivedAnchors(body)

	if a[1] != a[5] {
		t.Fatalf("the fixture is not ambiguous: line 2 is %s and line 6 is %s", a[1], a[5])
	}

	rej := rejectEdit(t, f, s, "f.txt", tools.EditRequest{
		Path: "f.txt", AnchorStart: a[1], AnchorEnd: a[1], NewText: "B\n",
	})
	if rej.Reason != tools.RejectAmbiguousAnchor {
		t.Errorf("reason = %q, want %q", rej.Reason, tools.RejectAmbiguousAnchor)
	}
	if len(rej.Candidates) != 2 || rej.Candidates[0] != 2 || rej.Candidates[1] != 6 {
		t.Errorf("candidates = %v, want [2 6]", rej.Candidates)
	}
	for _, want := range []string{"2", "6"} {
		if !strings.Contains(rej.Summary, want) {
			t.Errorf("the summary does not name candidate line %s: %s", want, rej.Summary)
		}
	}
}

// TestEditRejectsAnInvertedRegion is why anchor_order is its own reason. Both
// anchors resolve and the file has not moved, so "read the file again" is the
// wrong advice and would burn a turn; and taking the region in the other order
// would be exactly the fail-open reasoning ADR-0006 rejects.
func TestEditRejectsAnInvertedRegion(t *testing.T) {
	f := newFixture(t, map[string]string{"main.go": editSample})
	s := f.set(t)
	a := derivedAnchors(editSample)

	rej := rejectEdit(t, f, s, "main.go", tools.EditRequest{
		Path: "main.go", AnchorStart: a[5], AnchorEnd: a[2], NewText: "x\n",
	})
	if rej.Reason != tools.RejectAnchorOrder {
		t.Errorf("reason = %q, want %q", rej.Reason, tools.RejectAnchorOrder)
	}
	if len(rej.Current) == 0 {
		t.Error("an inverted region returned no current anchors, so the model cannot see the two lines")
	}
	if strings.Contains(rej.Detail, "read_file") {
		t.Errorf("the advice sends the model back to read_file although nothing drifted:\n%s", rej.Detail)
	}
}

// TestEditRejectionNamesTheFailingArgument holds the half of the rejection the
// model acts on: which of the two anchors was wrong. Reporting the pair without
// saying which one would send it to re-read for a stale anchor it already has.
func TestEditRejectionNamesTheFailingArgument(t *testing.T) {
	// Long enough that a narrowed window is visibly narrower than the file, and
	// with every line distinct so nothing here is ambiguity in disguise.
	var b strings.Builder
	for i := range 40 {
		fmt.Fprintf(&b, "line %d\n", i+1)
	}
	body := b.String()

	f := newFixture(t, map[string]string{"f.txt": body})
	s := f.set(t)
	a := derivedAnchors(body)

	rej := rejectEdit(t, f, s, "f.txt", tools.EditRequest{
		Path: "f.txt", AnchorStart: a[19], AnchorEnd: "deadbeef", NewText: "x\n",
	})
	if rej.Field != "anchor_end" {
		t.Errorf("field = %q, want %q", rej.Field, "anchor_end")
	}
	if rej.Anchor != "deadbeef" {
		t.Errorf("anchor = %q, want %q", rej.Anchor, "deadbeef")
	}

	// anchor_start resolved, so the window is the region it meant rather than
	// the whole file: the model is shown where it was aiming.
	if len(rej.Current) == 0 {
		t.Fatal("no current anchors returned")
	}
	if len(rej.Current) >= len(a) {
		t.Errorf("returned %d of %d lines; a resolved anchor should narrow the window",
			len(rej.Current), len(a))
	}
	found := false
	for _, l := range rej.Current {
		if l.Anchor == a[19] {
			found = true
		}
	}
	if !found {
		t.Error("the window does not include the line anchor_start resolved to")
	}
}

// TestEditRejectsEveryAnchorOnAnEmptyFile: an empty file has no lines and so no
// anchors, and the advice says so rather than sending the model to re-read
// nothing.
func TestEditRejectsEveryAnchorOnAnEmptyFile(t *testing.T) {
	f := newFixture(t, map[string]string{"empty.txt": ""})
	s := f.set(t)

	rej := rejectEdit(t, f, s, "empty.txt", tools.EditRequest{
		Path: "empty.txt", AnchorStart: "deadbeef", AnchorEnd: "deadbeef", NewText: "x\n",
	})
	if rej.Reason != tools.RejectAnchorDrift {
		t.Errorf("reason = %q, want %q", rej.Reason, tools.RejectAnchorDrift)
	}
	if len(rej.Current) != 0 {
		t.Errorf("an empty file returned %d current anchors", len(rej.Current))
	}
	if !strings.Contains(rej.Detail, "write_file") {
		t.Errorf("the advice does not point at write_file:\n%s", rej.Detail)
	}
}

// TestEditRejectionIsNeverAHarnessFault: a refusal is the tool working. ADR-0006
// §3 counts an internal tool error as `harness`, and SLICE-1's bar for the slice
// is zero of them — every anchored refusal landing there would make the bar
// unmeetable and, worse, meaningless.
func TestEditRejectionIsNeverAHarnessFault(t *testing.T) {
	f := newFixture(t, map[string]string{"main.go": editSample})
	s := f.set(t)

	_, err := s.EditFile(context.Background(), tools.EditRequest{
		Path: "main.go", AnchorStart: "deadbeef", AnchorEnd: "deadbeef", NewText: "x\n",
	})
	wantFault(t, err, tools.FaultTask)
	if tools.FaultOf(err).String() != "task" {
		t.Errorf("ErrorKind would be %q, want %q", tools.FaultOf(err).String(), "task")
	}
}

// TestEditRejectionDetailIsTheOutput keeps one string, not two. The engine
// journals EditRejected.Detail and hands the same text back to the model as the
// tool result; two renderings would drift the moment somebody edited one.
func TestEditRejectionDetailIsTheOutput(t *testing.T) {
	f := newFixture(t, map[string]string{"main.go": editSample})
	s := f.set(t)

	res, err := s.EditFile(context.Background(), tools.EditRequest{
		Path: "main.go", AnchorStart: "deadbeef", AnchorEnd: "deadbeef", NewText: "x\n",
	})
	if err == nil {
		t.Fatal("the edit was accepted")
	}
	if res.Rejection == nil {
		t.Fatal("no structured rejection")
	}
	if res.Output != res.Rejection.Detail {
		t.Errorf("Output and Rejection.Detail differ:\n%q\n%q", res.Output, res.Rejection.Detail)
	}
	if res.Diff != "" {
		t.Errorf("a rejected edit returned a diff: %q", res.Diff)
	}
}

// --- the failure kinds are distinguishable ----------------------------------

// TestRejectReasonsAreDistinct is the classifier's requirement. SLICE-1 §9
// charges different buckets to different causes, so two reasons sharing a wire
// form would make the distinction unrecoverable from the journal after the run.
func TestRejectReasonsAreDistinct(t *testing.T) {
	seen := map[tools.RejectReason]bool{}
	for _, r := range declaredRejectReasons(t) {
		if r == "" {
			t.Error("a reject reason has an empty wire form")
		}
		if seen[r] {
			t.Errorf("two reject reasons share the wire form %q", r)
		}
		seen[r] = true
	}
	// The values journal.EditRejected's doc names, so a rename here that left
	// that doc behind fails rather than drifting.
	for _, want := range []tools.RejectReason{
		tools.RejectMalformedAnchor, tools.RejectAnchorDrift,
		tools.RejectAmbiguousAnchor, tools.RejectAnchorOrder, tools.RejectBelowFloor,
	} {
		if !seen[want] {
			t.Errorf("%q is not among the package's declared reject reasons", want)
		}
	}
}

// TestEveryRejectReasonAdvisesADifferentMove is the test for whether these
// deserve to be separate values at all. If two refusals produced the same
// sentence back to the model, they would be one reason with two names, and the
// finer taxonomy would be cost without information.
//
// The unit is (mode, reason) rather than reason alone, because the fuzzy
// fallback reuses "ambiguous" — the *kind* of failure is the same and the
// recovery is not, and journal.EditRejected carries the mode beside the reason
// so a classifier can still tell them apart.
//
// It is also the completeness check: the set of reasons is parsed out of the
// package rather than listed here, so a sixth one added without a case fails
// this test instead of shipping with no evidence that it says anything new.
func TestEveryRejectReasonAdvisesADifferentMove(t *testing.T) {
	dupBody := "a\nb\nc\nx\na\nb\nc\ny\n"
	files := map[string]string{
		"main.go": editSample,
		"dup.txt": dupBody,
		"twin.go": "func a() {\n\tsame()\n}\n\nfunc b() {\n\tsame()\n}\n",
	}

	type refusal struct {
		mode string
		run  func(t *testing.T, f *fixture, s *tools.Set) *tools.EditRejection
	}
	cases := map[tools.RejectReason]refusal{
		tools.RejectMalformedAnchor: {tools.ModeAnchored,
			func(t *testing.T, f *fixture, s *tools.Set) *tools.EditRejection {
				return rejectEdit(t, f, s, "main.go", tools.EditRequest{Path: "main.go",
					AnchorStart: "line 6", AnchorEnd: derivedAnchors(editSample)[5], NewText: "x\n"})
			}},
		tools.RejectAnchorDrift: {tools.ModeAnchored,
			func(t *testing.T, f *fixture, s *tools.Set) *tools.EditRejection {
				return rejectEdit(t, f, s, "main.go", tools.EditRequest{Path: "main.go",
					AnchorStart: "deadbeef", AnchorEnd: "deadbeef", NewText: "x\n"})
			}},
		tools.RejectAmbiguousAnchor: {tools.ModeAnchored,
			func(t *testing.T, f *fixture, s *tools.Set) *tools.EditRejection {
				return rejectEdit(t, f, s, "dup.txt", tools.EditRequest{Path: "dup.txt",
					AnchorStart: derivedAnchors(dupBody)[1], AnchorEnd: derivedAnchors(dupBody)[1],
					NewText: "x\n"})
			}},
		tools.RejectAnchorOrder: {tools.ModeAnchored,
			func(t *testing.T, f *fixture, s *tools.Set) *tools.EditRejection {
				a := derivedAnchors(editSample)
				return rejectEdit(t, f, s, "main.go", tools.EditRequest{Path: "main.go",
					AnchorStart: a[5], AnchorEnd: a[2], NewText: "x\n"})
			}},
		tools.RejectBelowFloor: {tools.ModeFuzzy,
			func(t *testing.T, f *fixture, s *tools.Set) *tools.EditRejection {
				return rejectFuzzy(t, f, s, "main.go", tools.FuzzyEditRequest{Path: "main.go",
					Before: "func nothing(zzz complex128) {\n}\n", After: "x\n"})
			}},
	}
	// The fuzzy fallback's ambiguity refusal shares its reason with anchored
	// mode's and must not share its sentence.
	fuzzyAmbiguous := func(t *testing.T, f *fixture, s *tools.Set) *tools.EditRejection {
		return rejectFuzzy(t, f, s, "twin.go", tools.FuzzyEditRequest{Path: "twin.go",
			Before: "\tsame()\n", After: "x\n"})
	}

	declared := declaredRejectReasons(t)
	for _, r := range declared {
		if _, ok := cases[r]; !ok {
			t.Errorf("reject reason %q has no case here, so nothing checks that it says "+
				"anything a different reason does not", r)
		}
	}
	if len(declared) == 0 {
		t.Fatal("no reject reasons parsed out of the package; the completeness check is " +
			"asserting nothing")
	}

	type key struct{ mode, summary string }
	seen := map[key]tools.RejectReason{}
	record := func(t *testing.T, mode string, want tools.RejectReason, rej *tools.EditRejection) {
		t.Helper()
		if rej.Reason != want {
			t.Errorf("%s: reason = %q", want, rej.Reason)
			return
		}
		k := key{mode, rej.Summary}
		if other, dupe := seen[k]; dupe {
			t.Errorf("%q and %q produce the same sentence in %s mode, so they are one "+
				"reason with two names: %s", want, other, mode, rej.Summary)
		}
		seen[k] = want
		if strings.TrimSpace(rej.Detail) == strings.TrimSpace(rej.Summary) {
			t.Errorf("%q returns no advice beyond its summary, so the model is told what "+
				"went wrong and not what to do", want)
		}
	}

	for want, tc := range cases {
		f := newFixture(t, files)
		record(t, tc.mode, want, tc.run(t, f, f.set(t)))
	}

	f := newFixture(t, files)
	fuzzyAmb := fuzzyAmbiguous(t, f, f.set(t))
	record(t, tools.ModeFuzzy, tools.RejectAmbiguousAnchor, fuzzyAmb)
	for k, r := range seen {
		if k.mode == tools.ModeAnchored && r == tools.RejectAmbiguousAnchor &&
			k.summary == fuzzyAmb.Summary {
			t.Errorf("anchored and fuzzy ambiguity produce the same sentence, so the mode "+
				"carries no information: %s", k.summary)
		}
	}
}

// --- anchors come only from a read ------------------------------------------

// TestNoToolButReadFileEmitsAnchors is what makes "editing a region the model
// was never shown is impossible" structural rather than advisory.
//
// The argument has two halves. An anchor is eight hex characters of SHA-256
// over the anchor version and a length-prefixed (previous, this, next) window,
// so producing one requires the content it names — which a model cannot compute
// and cannot guess (a well-formed guess is refused by
// TestEditRejectsAnInventedAnchor). And read_file is the only tool that hands
// one over, which is the half a comment cannot hold: a tool added later could
// leak one by rendering a line the same way, so it is asserted here over every
// tool in the set.
func TestNoToolButReadFileEmitsAnchors(t *testing.T) {
	f := newFixture(t, map[string]string{"main.go": editSample, "third.go": editSample})
	s := f.set(t)
	ctx := context.Background()
	want := derivedAnchors(editSample)

	outputs := map[string]string{}

	list, err := s.ListDir(ctx, tools.ListRequest{Recursive: true})
	if err != nil {
		t.Fatalf("ListDir: %v", err)
	}
	outputs["list_dir"] = list

	// A pattern matching every line, so grep has the most chance to leak one.
	grep, err := s.Grep(ctx, tools.GrepRequest{Pattern: ".*"})
	if err != nil {
		t.Fatalf("Grep: %v", err)
	}
	outputs["grep"] = grep

	wrote, err := s.WriteFile(ctx, tools.WriteRequest{Path: "copy.go", Content: editSample})
	if err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	outputs["write_file"] = wrote.Output

	shell, err := s.RunShell(ctx, tools.ShellRequest{Command: "cat main.go"})
	if err != nil {
		t.Fatalf("RunShell: %v", err)
	}
	outputs["run_shell"] = shell.Output

	edit := applyEdit(t, s, tools.EditRequest{
		Path: "main.go", AnchorStart: want[5], AnchorEnd: want[5], NewText: "\tfmt.Println(\"bye\")\n",
	})
	// edit_file echoes the anchors it was *given*, which the model already had;
	// what it must not do is hand out anchors for lines the model has not read,
	// so the diff and the rendered result are what is checked.
	outputs["edit_file diff"] = edit.Diff

	// edit_file_fuzzy is the sharpest case in the set, and the reason this test
	// grew when KAN-785 landed. Its near-miss report quotes file content the
	// model may never have read — that is the whole point of it — so rendering
	// those lines the way read_file renders them would hand over anchors for a
	// region the model has not seen, and the structural claim above would stop
	// being structural.
	fuzzy := applyFuzzy(t, s, tools.FuzzyEditRequest{
		Path: "third.go", Before: "\tfmt.Println(\"hello\")\n", After: "\tfmt.Println(\"bye\")\n",
	})
	outputs["edit_file_fuzzy applied"] = fuzzy.Output

	missed, _ := s.EditFileFuzzy(ctx, tools.FuzzyEditRequest{
		Path: "third.go", Before: "func nothing(zzz complex128) {\n}\n", After: "x\n",
	})
	if missed.Rejection == nil || len(missed.Rejection.Matches) == 0 {
		t.Fatal("the fuzzy call was expected to be refused with near misses quoting the file")
	}
	outputs["edit_file_fuzzy near misses"] = missed.Rejection.Detail

	for tool, out := range outputs {
		for i, a := range want {
			if strings.Contains(out, a) {
				t.Errorf("%s output contains line %d's anchor %q; anchors must come only "+
					"from read_file, or an edit into a region the model never saw stops being "+
					"impossible (ADR-0006 decision 1)\n%s", tool, i+1, a, out)
			}
		}
	}

	// And the positive control: read_file does emit them, so a bug that made
	// every anchor unfindable would not pass this test silently.
	read, err := s.ReadFile(ctx, tools.ReadRequest{Path: "copy.go"})
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	for i, a := range want {
		if !strings.Contains(read, a) {
			t.Fatalf("read_file did not emit line %d's anchor %q; this test is asserting "+
				"nothing about the other tools", i+1, a)
		}
	}
}

// TestWellFormedAnchorAcceptsEveryDerivedAnchor keeps the shape check honest.
// It encodes an assumption about anchor.Derive's output — lowercase hex of a
// fixed width — and an assumption nobody checks is how a malformed-anchor
// rejection starts refusing real anchors.
func TestWellFormedAnchorAcceptsEveryDerivedAnchor(t *testing.T) {
	bodies := []string{editSample, "a\n", "\n\n\n", "a\r\nb\r\n", "x", strings.Repeat("dup\n", 40)}
	for _, body := range bodies {
		for i, a := range derivedAnchors(body) {
			if !tools.WellFormedAnchor(a) {
				t.Errorf("anchor %q (line %d of %q) is rejected as malformed", a, i+1, body)
			}
		}
	}
}

// --- the file-level refusals ------------------------------------------------

func TestEditRefusesWhatItCannotAnchor(t *testing.T) {
	f := newFixture(t, map[string]string{
		"main.go":  editSample,
		"bin.dat":  "ok\x00\x01binary\n",
		"sub/a.go": "package sub\n",
	})
	f.symlink(t, filepath.Join(f.outside, "secret.txt"), "escape.txt")
	s := f.set(t)

	tests := []struct {
		name string
		req  tools.EditRequest
		want error
	}{
		{"a missing file", tools.EditRequest{Path: "nope.go"}, os.ErrNotExist},
		{"a directory", tools.EditRequest{Path: "sub"}, tools.ErrNotRegular},
		{"the repository root", tools.EditRequest{Path: "."}, tools.ErrNotRegular},
		{"a binary file", tools.EditRequest{Path: "bin.dat"}, tools.ErrBinaryFile},
		{"a path outside the root", tools.EditRequest{Path: "../outside/secret.txt"}, tools.ErrOutsideRoot},
		{"a symlink out of the root", tools.EditRequest{Path: "escape.txt"}, tools.ErrOutsideRoot},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			req := tc.req
			req.AnchorStart, req.AnchorEnd = "deadbeef", "deadbeef"
			res, err := s.EditFile(context.Background(), req)
			if err == nil {
				t.Fatalf("EditFile(%+v) succeeded", req)
			}
			if !errors.Is(err, tc.want) {
				t.Errorf("error %v does not wrap %v", err, tc.want)
			}
			if errors.Is(err, tools.ErrEditRejected) {
				t.Error("a file-level refusal was reported as an anchored rejection; " +
					"EditRejected is about where an anchor landed, not about whether the " +
					"file can be edited at all")
			}
			if res.Rejection != nil {
				t.Errorf("a file-level refusal carried an anchored rejection: %+v", res.Rejection)
			}
			wantFault(t, err, tools.FaultTask)
		})
	}
}

func TestEditRefusesAFileOverTheLimit(t *testing.T) {
	body := strings.Repeat("line\n", 100)
	f := newFixture(t, map[string]string{"big.txt": body})
	s := f.set(t)
	s.Limits.MaxFileBytes = 10

	res, err := s.EditFile(context.Background(), tools.EditRequest{
		Path: "big.txt", AnchorStart: "deadbeef", AnchorEnd: "deadbeef",
	})
	if !errors.Is(err, tools.ErrTooLarge) {
		t.Fatalf("error %v does not wrap ErrTooLarge", err)
	}
	if res.Rejection != nil {
		t.Errorf("carried an anchored rejection: %+v", res.Rejection)
	}
	wantFault(t, err, tools.FaultTask)
	if got := onDisk(t, f, "big.txt"); got != body {
		t.Error("the file changed")
	}
}

// TestEditRejectionWindowIsBounded holds the one place a rejection can grow
// without limit: a drift that resolved nothing returns the file's own anchors,
// and on a large file that has to be bounded the way read_file bounds a read —
// with the bound *declared*, never silently.
func TestEditRejectionWindowIsBounded(t *testing.T) {
	body := strings.Repeat("a\nb\nc\nd\ne\nf\ng\nh\n", 20) // 160 distinct-enough lines
	f := newFixture(t, map[string]string{"big.txt": body})
	s := f.set(t)
	s.Limits.MaxLines = 12

	rej := rejectEdit(t, f, s, "big.txt", tools.EditRequest{
		Path: "big.txt", AnchorStart: "deadbeef", AnchorEnd: "deadbeef", NewText: "x\n",
	})
	if len(rej.Current) != 12 {
		t.Errorf("returned %d current anchors, want the max_lines bound of 12", len(rej.Current))
	}
	if !strings.Contains(rej.Detail, "max_lines=12") {
		t.Errorf("the bound was applied without being declared:\n%s", rej.Detail)
	}
	if !strings.Contains(rej.Detail, "offset=13") {
		t.Errorf("the detail does not say how to fetch the rest:\n%s", rej.Detail)
	}
}

// --- the line offsets and the anchored lines must agree ---------------------

// TestEditAgreesWithAnchorSplitOnEveryShape is the guard behind checkSpans,
// exercised through the tool. Two pieces of code decide what a line is — the
// anchor package decides what one *is*, the edit tool decides where one *sits*
// — and an off-by-one between them at a file boundary is how an anchored editor
// corrupts a file while every happy-path test stays green.
func TestEditAgreesWithAnchorSplitOnEveryShape(t *testing.T) {
	bodies := []string{
		"a\n",
		"a",
		"\n",
		"\n\n\n",
		"a\nb",
		"a\r\n",
		"a\r\nb\r\nc",
		"\r\n\r\n",
		"a\n\nb\n",
		"\ta\n  b\n",
	}

	for _, body := range bodies {
		t.Run(strings.ReplaceAll(strings.ReplaceAll(body, "\n", `\n`), "\r", `\r`), func(t *testing.T) {
			f := newFixture(t, map[string]string{"f.txt": body})
			s := f.set(t)
			a := derivedAnchors(body)
			if len(a) == 0 {
				t.Skip("no lines to edit")
			}

			// Replacing every line one at a time with itself must leave the
			// file byte-identical. Any disagreement between the two views of a
			// line shows up here as a shifted or duplicated byte.
			//
			// The newline on NewText is the contract, not a convenience: it is
			// read the way a file is read, so "x\n" is the one line "x" and ""
			// is no lines at all, which is a deletion.
			lines := anchor.Split([]byte(body))
			for i := range lines {
				fresh := derivedAnchors(onDisk(t, f, "f.txt"))
				res := applyEdit(t, s, tools.EditRequest{
					Path: "f.txt", AnchorStart: fresh[i], AnchorEnd: fresh[i], NewText: lines[i] + "\n",
				})
				if res.StartLine != i+1 {
					t.Fatalf("line %d resolved to line %d", i+1, res.StartLine)
				}
				if got := onDisk(t, f, "f.txt"); got != body {
					t.Fatalf("replacing line %d with itself changed the file:\n got %q\nwant %q",
						i+1, got, body)
				}
			}
		})
	}
}
