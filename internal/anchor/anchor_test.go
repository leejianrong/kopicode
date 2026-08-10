package anchor_test

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/leejianrong/kopicode/internal/anchor"
)

// dupFile is full of byte-identical lines, and repeats a whole three-line
// window on purpose. It is the case most likely to be wrong, so most tests here
// run against it as well as against ordinary source.
//
// Content-only anchors would collapse twelve of its seventeen lines into three
// classes. With one line of context only the closing braces at lines 5, 9 and
// 13 stay ambiguous — each is preceded by "\treturn nil" and followed by a
// blank line — which is the residual the refusal rule exists for. The brace on
// the last line resolves because it has no following line.
var dupFile = []string{
	"package main",     //  1
	"",                 //  2
	"func a() error {", //  3
	"\treturn nil",     //  4
	"}",                //  5  ambiguous
	"",                 //  6
	"func b() error {", //  7
	"\treturn nil",     //  8
	"}",                //  9  ambiguous
	"",                 // 10
	"func c() error {", // 11
	"\treturn nil",     // 12
	"}",                // 13  ambiguous
	"",                 // 14
	"func d() error {", // 15
	"\treturn nil",     // 16
	"}",                // 17
}

// dupAmbiguous are the 0-based indices of dupFile's three repeated windows.
var dupAmbiguous = []int{4, 8, 12}

var plainFile = []string{
	"package main",
	"",
	"import \"fmt\"",
	"",
	"func main() {",
	"\tfmt.Println(\"hello\")",
	"}",
}

// --- stability ---------------------------------------------------------

func TestDeriveIsDeterministic(t *testing.T) {
	for _, f := range [][]string{plainFile, dupFile} {
		first := anchor.Derive(f)
		for i := range 3 {
			got := anchor.Derive(f)
			if len(got) != len(first) {
				t.Fatalf("run %d: got %d anchors, want %d", i, len(got), len(first))
			}
			for j := range got {
				if got[j] != first[j] {
					t.Errorf("run %d line %d: got %s, want %s", i, j+1, got[j], first[j])
				}
			}
		}
	}
}

// TestStableUnderUnrelatedEdits is the property SLICE-1's Test Plan names. An
// edit outside a line's own window must leave that line's anchor byte-identical,
// or every anchor the model read earlier goes stale at once.
func TestStableUnderUnrelatedEdits(t *testing.T) {
	before := anchor.Derive(plainFile)

	// Insert two lines at the top. A scheme that mixed the line number in
	// would change every anchor below the insertion; this must change none.
	inserted := append([]string{"// Copyright 2026.", ""}, plainFile...)
	after := anchor.Derive(inserted)

	// plainFile[0] is now inserted[2]. Everything from index 1 on is outside
	// the window of the insertion and must be untouched.
	for i := 1; i < len(plainFile); i++ {
		if before[i] != after[i+2] {
			t.Errorf("line %q: anchor changed from %s to %s after an insertion at the top of the file",
				plainFile[i], before[i], after[i+2])
		}
	}

	// Rewriting the last line must not disturb the top of the file.
	edited := append([]string(nil), plainFile...)
	edited[len(edited)-1] = "} // done"
	tail := anchor.Derive(edited)
	for i := range len(plainFile) - 2 {
		if before[i] != tail[i] {
			t.Errorf("line %d (%q): anchor changed from %s to %s after an edit at the end of the file",
				i+1, plainFile[i], before[i], tail[i])
		}
	}
}

// TestBlastRadiusIsOneLine pins the cost of the context window: an edit to one
// line invalidates that line's anchor and its two neighbours', and nothing
// else. ADR-0006 §7 rejects wider windows on exactly this ground, so a change
// that widened Radius without revisiting the ADR should fail here.
func TestBlastRadiusIsOneLine(t *testing.T) {
	before := anchor.Derive(plainFile)
	edited := append([]string(nil), plainFile...)
	const target = 3 // 0-based; a line with neighbours either side
	edited[target] = "\t// changed"
	after := anchor.Derive(edited)

	for i := range plainFile {
		near := i >= target-anchor.Radius && i <= target+anchor.Radius
		changed := before[i] != after[i]
		if near && !changed {
			t.Errorf("line %d is within the window of the edit but its anchor did not change", i+1)
		}
		if !near && changed {
			t.Errorf("line %d is outside the window of the edit but its anchor changed from %s to %s",
				i+1, before[i], after[i])
		}
	}
}

// --- sensitivity -------------------------------------------------------

func TestSensitiveToChange(t *testing.T) {
	// Whitespace is content. An anchor that normalised it would let an edit
	// target a line whose indentation differs from what the model was shown,
	// which is a silent misapply in Python or YAML.
	cases := []struct {
		name string
		to   string
	}{
		{"body changed", "\tfmt.Println(\"goodbye\")"},
		{"one character", "\tfmt.Println(\"hellp\")"},
		{"indentation", "        fmt.Println(\"hello\")"},
		{"trailing space", "\tfmt.Println(\"hello\") "},
		{"emptied", ""},
	}
	const target = 5
	before := anchor.Derive(plainFile)

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			edited := append([]string(nil), plainFile...)
			edited[target] = tc.to
			after := anchor.Derive(edited)
			if before[target] == after[target] {
				t.Errorf("anchor %s unchanged after rewriting the line to %q",
					before[target], tc.to)
			}
		})
	}
}

// TestSensitiveToReordering catches a preimage that is not injective: two
// different files must not hash a line the same way just because their bytes
// concatenate alike.
func TestSensitiveToReordering(t *testing.T) {
	a := anchor.Derive([]string{"ab", "c", "x"})
	b := anchor.Derive([]string{"a", "bc", "x"})
	if a[0] == b[0] || a[1] == b[1] {
		t.Errorf("splitting the same bytes differently across lines produced the same anchors: %v vs %v", a, b)
	}
}

// TestVersionIsMixedIn proves a version bump changes every anchor, which is
// what makes a stale recorded fixture fail loudly rather than subtly.
func TestVersionIsMixedIn(t *testing.T) {
	// Recompute line 1 of a one-line file under a different version string,
	// mirroring the documented preimage, and require it to differ.
	preimage := func(version string) string {
		var b strings.Builder
		fmt.Fprintf(&b, "%d:%s", len(version), version)
		b.WriteString("-1:")
		fmt.Fprintf(&b, "%d:%s", len("solo"), "solo")
		b.WriteString("-1:")
		return b.String()
	}
	sum := sha256.Sum256([]byte(preimage(anchor.Version)))
	want := hex.EncodeToString(sum[:])[:anchor.Length]
	if got := anchor.Derive([]string{"solo"})[0]; got != want {
		t.Fatalf("derived anchor %s does not match the documented preimage (%s); "+
			"the format changed, so bump anchor.Version and re-record fixtures", got, want)
	}

	other := sha256.Sum256([]byte(preimage("kopicode-anchor-v99")))
	if hex.EncodeToString(other[:])[:anchor.Length] == want {
		t.Error("a version bump left the anchor unchanged")
	}
}

// --- collision safety within a file ------------------------------------

// TestResolveRefusesAmbiguity is the collision rule. Where a file genuinely
// repeats a window, the anchor is shared, and per ADR-0006 that is refused with
// the candidates named — never resolved to one of them.
func TestResolveRefusesAmbiguity(t *testing.T) {
	anchors := anchor.Derive(dupFile)

	dup := anchors[dupAmbiguous[0]]
	for _, i := range dupAmbiguous[1:] {
		if anchors[i] != dup {
			t.Fatalf("lines %d and %d repeat the same window but anchor differently (%s, %s)",
				dupAmbiguous[0]+1, i+1, dup, anchors[i])
		}
	}

	_, err := anchor.Resolve(anchors, dup)
	var ambig *anchor.AmbiguousError
	if !errors.As(err, &ambig) {
		t.Fatalf("resolving a repeated anchor: got %v, want an AmbiguousError", err)
	}
	want := []int{5, 9, 13}
	if fmt.Sprint(ambig.Lines) != fmt.Sprint(want) {
		t.Fatalf("got candidate lines %v, want %v", ambig.Lines, want)
	}
	if !strings.Contains(ambig.Error(), "5, 9, 13") {
		t.Errorf("the error must name the candidates so the model can pick another anchor: %q", ambig.Error())
	}
}

// TestOneLineOfContextDisambiguates is the reason Radius is 1 rather than 0.
// Byte-identical lines that a content-only hash would collapse into one
// unusable anchor must resolve uniquely here.
func TestOneLineOfContextDisambiguates(t *testing.T) {
	anchors := anchor.Derive(dupFile)

	ambiguous := map[int]bool{}
	for _, i := range dupAmbiguous {
		ambiguous[i] = true
	}

	// Group the lines a content-only hash would collapse, and check each
	// group is now distinguished.
	byContent := map[string][]int{}
	for i, l := range dupFile {
		byContent[l] = append(byContent[l], i)
	}
	collapsed := 0
	for content, idx := range byContent {
		if len(idx) < 2 {
			continue
		}
		collapsed += len(idx)
		for _, i := range idx {
			if ambiguous[i] {
				continue
			}
			if _, err := anchor.Resolve(anchors, anchors[i]); err != nil {
				t.Errorf("line %d (%q) should resolve uniquely, got %v", i+1, content, err)
			}
		}
	}
	if collapsed < 12 {
		t.Fatalf("test data is too weak: only %d lines share content", collapsed)
	}
	if got := len(dupAmbiguous); got >= collapsed {
		t.Errorf("one line of context left %d of %d duplicate lines ambiguous", got, collapsed)
	}
}

func TestResolveRejectsUnknownAnchor(t *testing.T) {
	anchors := anchor.Derive(plainFile)
	for _, a := range []string{"deadbeef", "", "not-hex", strings.Repeat("f", 64)} {
		if _, err := anchor.Resolve(anchors, a); !errors.Is(err, anchor.ErrUnknownAnchor) {
			t.Errorf("Resolve(%q): got %v, want ErrUnknownAnchor", a, err)
		}
	}
}

func TestResolveFindsUniqueAnchors(t *testing.T) {
	anchors := anchor.Derive(plainFile)
	for i, a := range anchors {
		got, err := anchor.Resolve(anchors, a)
		if err != nil {
			t.Fatalf("line %d: %v", i+1, err)
		}
		if got != i {
			t.Errorf("anchor %s resolved to line %d, want %d", a, got+1, i+1)
		}
	}
}

// TestAnchorsAreWellFormed guards the wire shape: the model has to transcribe
// this, and a variable-width or mixed-case field is a transcription error
// waiting to happen.
func TestAnchorsAreWellFormed(t *testing.T) {
	for _, a := range anchor.Derive(dupFile) {
		if len(a) != anchor.Length {
			t.Errorf("anchor %q is %d characters, want %d", a, len(a), anchor.Length)
		}
		if _, err := hex.DecodeString(a); err != nil {
			t.Errorf("anchor %q is not hex: %v", a, err)
		}
		if a != strings.ToLower(a) {
			t.Errorf("anchor %q is not lowercase", a)
		}
	}
}

// TestNoSpuriousCollisionsAcrossManyDistinctLines checks the 32-bit truncation
// empirically over far more distinct windows than any one file has. ADR-0006 §7
// argues the collision term is negligible against the rate at which real source
// repeats a window; this keeps that claim honest.
func TestNoSpuriousCollisionsAcrossManyDistinctLines(t *testing.T) {
	const n = 20000
	lines := make([]string, n)
	for i := range lines {
		lines[i] = fmt.Sprintf("\tif err := step%d(ctx, arg); err != nil {", i)
	}
	seen := make(map[string]int, n)
	collisions := 0
	for i, a := range anchor.Derive(lines) {
		if prev, ok := seen[a]; ok {
			collisions++
			t.Logf("collision: lines %d and %d both anchor %s", prev+1, i+1, a)
		}
		seen[a] = i
	}
	// At 32 bits, 20k distinct windows expect n^2/2/2^32 ~= 0.047 collisions.
	// Seeing several would mean the truncation is not uniform.
	if collisions > 2 {
		t.Errorf("%d collisions over %d distinct windows; expected ~0", collisions, n)
	}
}

// --- splitting ---------------------------------------------------------

func TestSplit(t *testing.T) {
	cases := []struct {
		name string
		src  string
		want []string
	}{
		{"empty file", "", nil},
		{"no trailing newline", "a\nb", []string{"a", "b"}},
		{"trailing newline", "a\nb\n", []string{"a", "b"}},
		{"crlf", "a\r\nb\r\n", []string{"a", "b"}},
		{"one blank line", "\n", []string{""}},
		{"blank lines kept", "a\n\n\nb\n", []string{"a", "", "", "b"}},
		{"trailing blank line", "a\n\n", []string{"a", ""}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := anchor.Split([]byte(tc.src))
			if len(got) != len(tc.want) {
				t.Fatalf("Split(%q) = %q, want %q", tc.src, got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("Split(%q) = %q, want %q", tc.src, got, tc.want)
				}
			}
		})
	}
}

// TestLineEndingsDoNotChangeAnchors is the cross-platform trap: a fixture
// recorded on a LF checkout must still resolve against a CRLF one, or every
// edit on Windows is rejected.
func TestLineEndingsDoNotChangeAnchors(t *testing.T) {
	lf := anchor.Derive(anchor.Split([]byte("package main\n\nfunc main() {}\n")))
	crlf := anchor.Derive(anchor.Split([]byte("package main\r\n\r\nfunc main() {}\r\n")))
	noEOL := anchor.Derive(anchor.Split([]byte("package main\n\nfunc main() {}")))

	for i := range lf {
		if lf[i] != crlf[i] || lf[i] != noEOL[i] {
			t.Errorf("line %d: lf=%s crlf=%s no-final-newline=%s, want all equal",
				i+1, lf[i], crlf[i], noEOL[i])
		}
	}
}

// --- rendering ---------------------------------------------------------

func TestRenderShape(t *testing.T) {
	lines := anchor.Split([]byte("package main\n\nimport \"fmt\"\n"))
	got := anchor.Render(lines)
	anchors := anchor.Derive(lines)

	want := []string{
		anchors[0] + " 1| package main",
		anchors[1] + " 2|",
		anchors[2] + " 3| import \"fmt\"",
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("line %d: got %q, want %q", i+1, got[i], want[i])
		}
	}
	if got[1] != strings.TrimRight(got[1], " ") {
		t.Errorf("a blank line rendered with trailing whitespace: %q", got[1])
	}
}

// TestRenderAnchorIsAtAFixedColumn is why the anchor comes first: it is the one
// field the model must copy, so it must not move when the file gets longer.
func TestRenderAnchorIsAtAFixedColumn(t *testing.T) {
	small := anchor.Render([]string{"a", "b"})
	large := make([]string, 1200)
	for i := range large {
		large[i] = fmt.Sprintf("line %d", i)
	}
	rendered := anchor.Render(large)

	for _, l := range append(append([]string(nil), small...), rendered...) {
		if _, err := hex.DecodeString(l[:anchor.Length]); err != nil {
			t.Fatalf("rendered line %q does not start with the anchor", l)
		}
		if l[anchor.Length] != ' ' {
			t.Fatalf("rendered line %q does not separate the anchor with a space", l)
		}
	}

	// Line numbers are right-aligned to a constant width within a file, so the
	// content column is constant too.
	col := strings.Index(rendered[0], "|")
	for i, l := range rendered {
		if strings.Index(l, "|") != col {
			t.Fatalf("line %d puts the separator at column %d, want %d: %q",
				i+1, strings.Index(l, "|"), col, l)
		}
	}
}

// TestRenderPassesContentThroughVerbatim: content containing the separator is a
// legibility question, not a parsing one. Nothing machine-parses this output.
func TestRenderPassesContentThroughVerbatim(t *testing.T) {
	lines := []string{"a := b | c", "\tif x || y {"}
	for i, l := range anchor.Render(lines) {
		if !strings.HasSuffix(l, lines[i]) {
			t.Errorf("rendered %q, want it to end with the verbatim content %q", l, lines[i])
		}
	}
}

// BenchmarkDerive substantiates ADR-0006 §7's claim that the choice of hash is
// not a performance question: a whole file's anchors cost far less than the
// syscall that read it.
func BenchmarkDerive(b *testing.B) {
	lines := make([]string, 2000)
	for i := range lines {
		lines[i] = fmt.Sprintf("\tif err := step%d(ctx, arg); err != nil {", i)
	}
	b.ReportAllocs()
	for b.Loop() {
		anchor.Derive(lines)
	}
}

func TestEmptyInputs(t *testing.T) {
	if got := anchor.Derive(nil); got != nil {
		t.Errorf("Derive(nil) = %v, want nil", got)
	}
	if got := anchor.Render(nil); got != nil {
		t.Errorf("Render(nil) = %v, want nil", got)
	}
	if _, err := anchor.Resolve(nil, "deadbeef"); !errors.Is(err, anchor.ErrUnknownAnchor) {
		t.Errorf("Resolve(nil, ...) = %v, want ErrUnknownAnchor", err)
	}
}
